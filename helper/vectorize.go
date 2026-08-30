package main

// The background half of semantic retrieval.
//
// Embedding a corpus is the one slow thing this program does, so it never
// happens on a path anybody is waiting on. Dropping a 300-page PDF onto the
// window returns as soon as the text is extracted and the lexical index is
// rebuilt — the document is searchable by word immediately — and the vectors
// arrive behind it, passage by passage, while the drive is being used.
//
// That ordering is also what makes the feature safe to ship. Every stage past
// "BM25 works" is optional: the embedding server may never come up, may die
// halfway, may be serving a model whose vectors do not match the cache on the
// drive. In each case the worker logs it once, stops, and leaves a working
// lexical search behind.

import (
	"context"
	"log"
	"sync"
	"time"
)

const (
	// The embedding server is started at the same moment as everything else and
	// has a model to load first, so the first probes are expected to fail.
	probeBackoffMin = 1 * time.Second
	probeBackoffMax = 30 * time.Second

	// Vectors are written per document rather than per batch. A document is the
	// unit the cache is keyed by, and rewriting a small file a few times is far
	// cheaper on exFAT than the alternative of holding everything until the end
	// and losing all of it when the drive is pulled.
	saveEvery = 64
)

type Vectorizer struct {
	idx  *Index
	emb  *Embedder
	vecs *VecStore

	wake chan struct{}

	mu      sync.RWMutex
	note    string
	working bool
	logged  bool // the "reused N cached embeddings" line has been said once
	// Passages the encoder refused even after being cut down. Remembered so a
	// pass does not offer them again on every wake and never finish. Kept in
	// memory only: retrying once after a relaunch costs one request.
	skipped map[chunkHash]bool
}

func NewVectorizer(idx *Index, emb *Embedder, vecs *VecStore) *Vectorizer {
	return &Vectorizer{idx: idx, emb: emb, vecs: vecs,
		wake: make(chan struct{}, 1), skipped: map[chunkHash]bool{}}
}

// Wake asks for a pass. The channel has room for exactly one pending request,
// so twenty uploads in a row queue one pass rather than twenty.
func (v *Vectorizer) Wake() {
	if v == nil {
		return
	}
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

func (v *Vectorizer) setNote(s string) {
	v.mu.Lock()
	v.note = s
	v.mu.Unlock()
}

// Status is what /api/health reports. "Working" matters to the UI: a corpus
// that is half embedded ranks on half a corpus, and saying so is better than
// letting the results quietly improve while somebody wonders why.
func (v *Vectorizer) Status() (ready, working bool, model, note string, embedded, total int) {
	if v == nil {
		return false, false, "", "", 0, 0
	}
	v.mu.RLock()
	working, note = v.working, v.note
	v.mu.RUnlock()
	embedded, total = v.idx.VecStats()
	return v.emb.Ready(), working, v.emb.Model(), note, embedded, total
}

func (v *Vectorizer) Run(ctx context.Context) {
	backoff := probeBackoffMin
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-v.wake:
		case <-timer.C:
		}

		if !v.emb.Ready() {
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := v.emb.Probe(pctx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Loud once, then quiet. A drive with no embedding server is a
				// supported configuration, not an incident, and repeating the
				// message every second would bury the log that matters.
				if backoff == probeBackoffMin {
					log.Printf("pocketd: embedding server not ready (%v) — retrying, search stays lexical", err)
				}
				v.setNote("connecting to the embedding server")
				resetTimer(timer, backoff)
				backoff = min(backoff*2, probeBackoffMax)
				continue
			}
			log.Printf("pocketd: embeddings ready — model %s, %d dimensions",
				v.emb.Model(), v.emb.Dim())
			backoff = probeBackoffMin
			v.mu.Lock()
			v.logged = false
			v.mu.Unlock()
		}

		v.loadCache()
		if !v.pass(ctx) && ctx.Err() == nil {
			// The pass stopped on an error rather than finishing. Try again on
			// the backoff rather than spinning.
			resetTimer(timer, backoff)
			backoff = min(backoff*2, probeBackoffMax)
			continue
		}
		backoff = probeBackoffMin
		stopTimer(timer)
	}
}

// loadCache attaches whatever the drive already holds for the current model.
// This is the difference between a drive that is instantly semantic on the
// machine you plug it into and one that spends ten minutes re-encoding a corpus
// it encoded last week.
//
// It runs on EVERY pass, not once at startup. Adding or deleting a document
// rebuilds the index, and a rebuild drops every vector — the index has no model
// to recompute them with. If the cache were read only once, adding a single
// note to a drive holding a thousand embedded passages would silently re-encode
// all thousand. Reading a few small files each time costs nothing next to that.
func (v *Vectorizer) loadCache() {
	model, dim := v.emb.Model(), v.emb.Dim()
	ids := v.idx.DocIDs()
	live := make(map[string]bool, len(ids))
	n := 0
	for _, id := range ids {
		live[id] = true
		if cached := v.vecs.Load(id, model); len(cached) > 0 {
			n += v.idx.AttachVectors(model, dim, cached)
		}
	}
	// A .vec whose document is gone is dead weight and, being a lossy copy of
	// the text, is also data the erase scripts are supposed to have removed.
	v.vecs.Sweep(live)

	v.mu.Lock()
	first := !v.logged
	v.logged = true
	v.mu.Unlock()
	if n > 0 && first {
		log.Printf("pocketd: reused %d cached embedding(s) from the drive", n)
	}
}

// pass embeds everything still missing, one document at a time. Returns false
// if it stopped early on an embedding failure.
func (v *Vectorizer) pass(ctx context.Context) bool {
	v.mu.Lock()
	v.working = true
	v.mu.Unlock()
	defer func() {
		v.mu.Lock()
		v.working = false
		v.mu.Unlock()
	}()

	model, dim := v.emb.Model(), v.emb.Dim()
	started := time.Now()
	total := 0

	for _, id := range v.idx.DocIDs() {
		if ctx.Err() != nil {
			return false
		}
		hashes, texts := v.pendingFor(id)
		if len(texts) == 0 {
			continue
		}
		v.setNote("indexing meanings")

		done := 0
		for done < len(texts) {
			end := min(done+saveEvery, len(texts))

			ectx, cancel := context.WithTimeout(ctx, indexTimeout)
			vecs, err := v.emb.EmbedDocs(ectx, texts[done:end])
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return false
				}
				log.Printf("pocketd: embedding failed (%v) — search stays lexical for the rest", err)
				v.setNote("the embedding server stopped responding")
				// Whatever was already encoded is still valid and still worth
				// keeping; only the remainder is lost.
				v.save(id, model, dim)
				return false
			}

			batch := map[chunkHash][]float32{}
			refused := 0
			for i, vec := range vecs {
				// nil is a passage the encoder would not take at any length.
				// Recording it is what keeps the next pass from offering it
				// again and looping on the same paragraph forever.
				if vec == nil {
					v.mu.Lock()
					v.skipped[hashes[done+i]] = true
					v.mu.Unlock()
					refused++
					continue
				}
				batch[hashes[done+i]] = vec
			}
			v.idx.AttachVectors(model, dim, batch)
			done = end
			total += len(batch)
			if refused > 0 {
				// Not an error: those passages stay findable by word, which is
				// how every passage on this drive was findable until now.
				log.Printf("pocketd: %d passage(s) could not be encoded — they remain lexical-only", refused)
			}

			// Save after each slice, not at the end of the document. This runs
			// on a stick that gets unplugged by hand.
			v.save(id, model, dim)
		}
	}

	v.setNote("")
	if total > 0 {
		log.Printf("pocketd: embedded %d passage(s) in %s", total, time.Since(started).Truncate(time.Millisecond))
	}
	return true
}

// pendingFor is PendingFor minus the passages already known to be unencodable.
func (v *Vectorizer) pendingFor(docID string) ([]chunkHash, []string) {
	hashes, texts := v.idx.PendingFor(docID)
	v.mu.RLock()
	defer v.mu.RUnlock()
	if len(v.skipped) == 0 {
		return hashes, texts
	}
	oh, ot := hashes[:0], texts[:0]
	for i, h := range hashes {
		if !v.skipped[h] {
			oh = append(oh, h)
			ot = append(ot, texts[i])
		}
	}
	return oh, ot
}

func (v *Vectorizer) save(docID, model string, dim int) {
	held := v.idx.VectorsFor(docID)
	if len(held) == 0 {
		return
	}
	if err := v.vecs.Save(docID, model, dim, held); err != nil {
		// A read-only drive is a real configuration — a locked-down laptop, a
		// write-protect switch. Semantic search still works for this session;
		// it just has to be earned again next time.
		v.setNote("embeddings cannot be saved to this drive")
		log.Printf("pocketd: could not cache embeddings (%v) — they will be recomputed next launch", err)
	}
}

// Forget drops the cache for one document. Called when it is deleted, because a
// vector is a lossy representation of the passage it came from and leaving it
// behind would leave part of the document on the drive.
func (v *Vectorizer) Forget(docID string) {
	if v == nil {
		return
	}
	v.vecs.Delete(docID)
}

func (v *Vectorizer) ForgetAll() {
	if v == nil {
		return
	}
	v.vecs.Wipe()
}

// EmbedQuery is the one synchronous use of the embedder. It is on the path of a
// question the user is waiting for, so it fails fast and fails silently: a nil
// vector makes Search fall back to BM25 for this turn and nothing else.
func (v *Vectorizer) EmbedQuery(ctx context.Context, q string) []float32 {
	if v == nil || !v.emb.Ready() {
		return nil
	}
	if embedded, _ := v.idx.VecStats(); embedded == 0 {
		return nil // nothing to compare it against yet
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	vec, err := v.emb.EmbedQuery(ctx, q)
	if err != nil {
		return nil
	}
	return vec
}

func resetTimer(t *time.Timer, d time.Duration) {
	stopTimer(t)
	t.Reset(d)
}

func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}
