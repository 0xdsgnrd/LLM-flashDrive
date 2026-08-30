package main

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Retrieval is hybrid: BM25 over the words, cosine over the meanings, fused.
//
// BM25 alone was the drive's whole search for good reason — it needs no model,
// no second server and no entry in drive.lock, and for your own notes the words
// you ask with are usually the words you wrote. What it cannot do is match on
// meaning: ask about "cars" and a document that only ever says "automobile"
// never comes back, because the two share no term.
//
// Semantic search fixes exactly that and breaks the complementary case. An
// encoder is fuzzy about the things BM25 is exact about: a part number, an
// error code, a surname, a flag like GGML_OPENMP. Those are rare tokens with
// enormous IDF and almost no semantic neighbourhood, and a dense retriever will
// happily return a passage that is *about* the same topic instead of the one
// that contains the string you typed.
//
// So neither replaces the other and this file runs both. Semantic retrieval is
// additive in the strict sense: with no embedding model on the drive, or with
// the embedding server down, every dense path here is skipped and the results
// are byte-for-byte what BM25 alone produced.
const (
	bm25K1 = 1.2
	bm25B  = 0.75

	maxHitsPerDoc = 2 // one long document must not crowd out every other source

	// Reciprocal Rank Fusion. BM25 scores are unbounded and corpus-relative;
	// cosine lives in [-1,1] and, for most encoders, in a narrow band near the
	// top of it. There is no honest way to add them, and normalising each list
	// to [0,1] is worse than it looks — it stretches whatever noise is at the
	// bottom of a list up to meet the signal at the top of the other.
	//
	// RRF ignores the scores and uses only the positions, which is the part
	// both retrievers agree on the meaning of. A passage found by both rises
	// above one found by either.
	rrfK = 60

	// How deep each retriever is read before fusing. Past this, ranks are
	// noise and 1/(60+r) has flattened out anyway.
	fuseDepth = 50

	// Dense retrieval always returns something — every passage has a cosine
	// against every query — so an unrelated question would still ground an
	// answer in the nearest three documents on the drive. BM25 has the
	// opposite and better behaviour: no shared term, no result. Something has
	// to play that part for the dense half, and it cannot be a fixed cosine:
	// 0.5 is "unrelated" for one encoder and "closely related" for another.
	//
	// Nor can it be a fixed number of standard deviations above the mean
	// similarity for the query, which was the first thing tried here and does
	// not work. Measured on bge-small over a ten-document corpus:
	//
	//   "how do I make bread rise" → baking.md            z = +2.20  (right)
	//   "xylophone marsupial quantum" → music.md          z = +2.48  (noise)
	//
	// The noise scores HIGHER. When a query means nothing to the encoder the
	// similarities collapse into a narrow band, the standard deviation shrinks
	// with them, and whichever passage happens to lead is left standing several
	// deviations clear of a mean it is barely above. Dividing by a quantity
	// that collapses exactly when the answer is "nothing here" cannot work.
	//
	// So the yardstick is the corpus rather than the query: the similarity of
	// two unrelated passages ON THIS DRIVE, under THIS encoder, measured. Same
	// corpus, same encoder, same queries, scored against that baseline instead:
	//
	//   relevant   5.12  4.08  2.88  4.12
	//   noise      0.82  0.76 -0.14
	//
	// which separates with room to spare, and calibrates itself to whatever
	// encoder the drive happens to carry.
	defaultMinZ = 2.0

	// Passages sampled to measure that baseline. All pairs of 96 is 4,560 dot
	// products — a few milliseconds, recomputed whenever vectors change.
	baselineSample = 96

	// Fewer pairs than this is not a distribution, it is a handful of numbers.
	// Below it there is no floor at all: on a corpus that small, k and
	// maxHitsPerDoc are limit enough, and recall matters more.
	minBaselinePairs = 20

	// Chunks overlap by design, so neighbours in a single-document corpus are
	// similar for a reason that has nothing to do with the encoder. Skip them
	// when measuring what "unrelated" looks like.
	baselineNeighbourGap = 4
)

// Common words carry almost no signal and inflate every posting list.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "but": true, "not": true,
	"you": true, "all": true, "can": true, "her": true, "was": true, "one": true,
	"our": true, "out": true, "his": true, "has": true, "had": true, "how": true,
	"its": true, "who": true, "did": true, "yes": true, "she": true, "him": true,
	"they": true, "them": true, "this": true, "that": true, "with": true,
	"from": true, "have": true, "were": true, "what": true, "when": true,
	"your": true, "will": true, "would": true, "there": true, "their": true,
	"about": true, "which": true, "been": true, "into": true, "than": true,
	"then": true, "also": true, "does": true, "each": true, "such": true,
}

func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if len(f) < 2 || stopwords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

type Hit struct {
	DocID   string  `json:"docId"`
	DocName string  `json:"docName"`
	Chunk   int     `json:"chunk"`
	Text    string  `json:"text"`
	Score   float64 `json:"score"`
	// Which retriever found it. Not decoration: "semantic" on a passage that
	// shares no word with the question is the one case worth being able to see
	// from the outside, and it is invisible in the text of the answer.
	Via string `json:"via"`
	// Cosine against the query, when this passage had a vector at all. Zero
	// means lexical-only, which is not the same as "dissimilar".
	Cosine float64 `json:"cosine,omitempty"`
}

type posting struct {
	chunk int
	tf    int
}

type indexedChunk struct {
	docID, docName string
	no             int
	text           string
	length         int
	hash           chunkHash
	vec            []float32 // nil until embedded; nil forever without a model
}

type Index struct {
	mu       sync.RWMutex
	chunks   []indexedChunk
	postings map[string][]posting
	avgLen   float64
	docCount int

	// The vector space the attached vectors belong to. Two models produce
	// numbers of the same shape and no shared meaning, so this is checked on
	// every attach rather than assumed.
	vecModel string
	vecDim   int
	minZ     float64

	// What this encoder thinks two unrelated passages of this corpus look
	// like. Measured, never configured — it is the only thing that makes a
	// similarity threshold mean the same thing across models.
	bgMean, bgSD float64
	bgPairs      int
}

func NewIndex() *Index {
	return &Index{postings: map[string][]posting{}, minZ: defaultMinZ}
}

func (ix *Index) SetMinZ(z float64) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.minZ = z
}

// Baseline reports the measured reference, so a drive can be asked why it
// judged a passage relevant rather than only told that it did.
func (ix *Index) Baseline() (mean, sd float64, pairs int) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.bgMean, ix.bgSD, ix.bgPairs
}

// Build replaces the whole index. Rebuilding beats incremental updates here:
// the corpus is small, and an index that can only ever be wrong by being stale
// is much easier to reason about than one maintained by a diff.
//
// Vectors are NOT rebuilt — they cannot be, there is no model in this process.
// They are dropped here and attached again by hash, which is why the hash is
// computed now.
func (ix *Index) Build(metas []DocMeta, texts []string) {
	chunks := []indexedChunk{}
	for i, m := range metas {
		for n, c := range chunk(texts[i]) {
			chunks = append(chunks, indexedChunk{
				docID: m.ID, docName: m.Name, no: n, text: c,
				length: len(tokenize(c)), hash: hashChunk(c),
			})
		}
	}
	postings := make(map[string][]posting, len(chunks)*8)
	total := 0
	for ci, c := range chunks {
		total += c.length
		tf := map[string]int{}
		for _, t := range tokenize(c.text) {
			tf[t]++
		}
		for t, n := range tf {
			postings[t] = append(postings[t], posting{chunk: ci, tf: n})
		}
	}
	avg := 0.0
	if len(chunks) > 0 {
		avg = float64(total) / float64(len(chunks))
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.chunks, ix.postings, ix.avgLen, ix.docCount = chunks, postings, avg, len(metas)
	// Every vector was just dropped, so the baseline measured from them
	// describes a corpus that no longer exists.
	ix.bgMean, ix.bgSD, ix.bgPairs = 0, 0, 0
}

// AttachVectors fills in the vectors for whatever passages it has them for,
// matched by content hash rather than by position. That is what makes it safe
// to call at any time, in any order, against an index that has been rebuilt in
// between: a hash that still exists is the same passage, and one that does not
// is simply dropped.
//
// A different model means a different space. Mixing two would not error, it
// would just rank badly, so everything already attached is discarded instead.
func (ix *Index) AttachVectors(model string, dim int, vecs map[chunkHash][]float32) int {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	if model == "" || dim <= 0 {
		return 0
	}
	if ix.vecModel != model || ix.vecDim != dim {
		for i := range ix.chunks {
			ix.chunks[i].vec = nil
		}
		ix.vecModel, ix.vecDim = model, dim
	}
	n := 0
	for i := range ix.chunks {
		if v, ok := vecs[ix.chunks[i].hash]; ok && len(v) == dim {
			ix.chunks[i].vec = v
			n++
		}
	}
	ix.calibrate()
	return n
}

// calibrate measures the similarity of unrelated passages under this encoder,
// which is the reference every semantic match is then judged against. Called
// with the write lock held.
//
// Two passages of the SAME document are not evidence of what unrelated looks
// like — consecutive chunks deliberately overlap — so they are excluded
// wherever the corpus is large enough to have an alternative.
func (ix *Index) calibrate() {
	ix.bgMean, ix.bgSD, ix.bgPairs = 0, 0, 0

	var withVec []int
	docs := map[string]bool{}
	for i, c := range ix.chunks {
		if c.vec != nil {
			withVec = append(withVec, i)
			docs[c.docID] = true
		}
	}
	if len(withVec) < 2 {
		return
	}

	// Strided rather than the first N, so the sample describes the whole
	// corpus rather than whichever document sorted first.
	step := 1
	if len(withVec) > baselineSample {
		step = len(withVec) / baselineSample
	}
	var sample []int
	for i := 0; i < len(withVec) && len(sample) < baselineSample; i += step {
		sample = append(sample, withVec[i])
	}

	multiDoc := len(docs) > 1
	var sum, sumSq float64
	pairs := 0
	for a := 0; a < len(sample); a++ {
		for b := a + 1; b < len(sample); b++ {
			ca, cb := ix.chunks[sample[a]], ix.chunks[sample[b]]
			if multiDoc {
				if ca.docID == cb.docID {
					continue
				}
			} else if b-a < baselineNeighbourGap {
				continue
			}
			s := dot(ca.vec, cb.vec)
			sum += s
			sumSq += s * s
			pairs++
		}
	}
	if pairs < minBaselinePairs {
		return
	}
	mean := sum / float64(pairs)
	variance := sumSq/float64(pairs) - mean*mean
	if variance < 0 {
		variance = 0
	}
	ix.bgMean, ix.bgSD, ix.bgPairs = mean, math.Sqrt(variance), pairs
}

// PendingFor lists the passages of one document that still have no vector,
// newest work first. Returned as parallel slices because the caller feeds the
// texts straight to the embedder and needs the hashes back to store them.
func (ix *Index) PendingFor(docID string) (hashes []chunkHash, texts []string) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	seen := map[chunkHash]bool{}
	for _, c := range ix.chunks {
		if c.docID != docID || c.vec != nil || seen[c.hash] {
			continue
		}
		seen[c.hash] = true
		hashes = append(hashes, c.hash)
		texts = append(texts, c.text)
	}
	return hashes, texts
}

// VectorsFor returns every vector currently held for one document, which is
// what gets written to its .vec file.
func (ix *Index) VectorsFor(docID string) map[chunkHash][]float32 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := map[chunkHash][]float32{}
	for _, c := range ix.chunks {
		if c.docID == docID && c.vec != nil {
			out[c.hash] = c.vec
		}
	}
	return out
}

func (ix *Index) DocIDs() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	seen := map[string]bool{}
	out := []string{}
	for _, c := range ix.chunks {
		if !seen[c.docID] {
			seen[c.docID] = true
			out = append(out, c.docID)
		}
	}
	return out
}

func (ix *Index) Stats() (docs, chunks int) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.docCount, len(ix.chunks)
}

// VecStats reports how much of the corpus is embedded, so the UI can say
// "indexing 200 of 900 passages" instead of quietly ranking on half a corpus.
func (ix *Index) VecStats() (embedded, total int) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	for _, c := range ix.chunks {
		if c.vec != nil {
			embedded++
		}
	}
	return embedded, len(ix.chunks)
}

type scored struct {
	chunk int
	score float64
}

// lexical is BM25, unchanged. It returns every chunk with a non-zero score,
// ordered, and an empty list when the query shares no term with the corpus —
// which stays a meaningful answer rather than a reason to fall back to dense.
func (ix *Index) lexical(query string) []scored {
	n := float64(len(ix.chunks))
	scores := map[int]float64{}

	for _, term := range tokenize(query) {
		pl := ix.postings[term]
		if len(pl) == 0 {
			continue
		}
		df := float64(len(pl))
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for _, p := range pl {
			tf := float64(p.tf)
			norm := tf + bm25K1*(1-bm25B+bm25B*float64(ix.chunks[p.chunk].length)/ix.avgLen)
			scores[p.chunk] += idf * tf * (bm25K1 + 1) / norm
		}
	}
	out := make([]scored, 0, len(scores))
	for ci, s := range scores {
		out = append(out, scored{ci, s})
	}
	ix.sortScored(out)
	return out
}

// dense is cosine against every embedded passage, then the relative floor.
// Scoring the whole corpus exhaustively is the right call at this scale: a
// thousand passages at 768 dimensions is under a million multiply-adds, well
// under a millisecond, and it buys exact results with no ANN index to build,
// tune, persist or invalidate.
func (ix *Index) dense(qvec []float32) []scored {
	if len(qvec) == 0 || ix.vecDim != len(qvec) {
		return nil
	}
	sims := make([]scored, 0, len(ix.chunks))
	var sum, sumSq float64
	for i, c := range ix.chunks {
		if c.vec == nil {
			continue
		}
		s := dot(qvec, c.vec)
		sims = append(sims, scored{i, s})
		sum += s
		sumSq += s * s
	}
	if len(sims) == 0 {
		return nil
	}

	// Every passage equally similar is not a corpus where everything matches,
	// it is a query the encoder has no opinion about. Handing back the whole
	// corpus on the strength of a tie is worse than handing back nothing.
	mean := sum / float64(len(sims))
	if variance := sumSq/float64(len(sims)) - mean*mean; variance < 1e-12 {
		return nil
	}

	// No baseline means the corpus is too small to have measured one. Rather
	// than invent a threshold, let everything through: k and maxHitsPerDoc
	// still apply, and on a handful of passages recall is the better error.
	if ix.bgPairs >= minBaselinePairs && ix.bgSD > 1e-6 {
		floor := ix.bgMean + ix.minZ*ix.bgSD
		kept := sims[:0]
		for _, s := range sims {
			if s.score >= floor {
				kept = append(kept, s)
			}
		}
		sims = kept
	}
	ix.sortScored(sims)
	return sims
}

// sortScored orders by score and then, for equal scores, by document and
// passage. Without the tie-break, repeating a search could silently show the
// model a different passage — Go's map iteration and sort are both free to
// reorder equals.
func (ix *Index) sortScored(s []scored) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].score != s[j].score {
			return s[i].score > s[j].score
		}
		a, b := ix.chunks[s[i].chunk], ix.chunks[s[j].chunk]
		if a.docID != b.docID {
			return a.docID < b.docID
		}
		return a.no < b.no
	})
}

// Search fuses the two retrievers. Pass a nil qvec — no embedding model, server
// down, query embedding timed out — and this is BM25 exactly as it was.
func (ix *Index) Search(query string, k int, qvec []float32) []Hit {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	if len(ix.chunks) == 0 || k <= 0 {
		return []Hit{}
	}

	lex := ix.lexical(query)
	den := ix.dense(qvec)
	if len(lex) == 0 && len(den) == 0 {
		return []Hit{}
	}

	fused := map[int]float64{}
	inLex := map[int]bool{}
	inDen := map[int]float64{}

	for r, s := range lex {
		if r >= fuseDepth {
			break
		}
		fused[s.chunk] += 1 / float64(rrfK+r+1)
		inLex[s.chunk] = true
	}
	for r, s := range den {
		if r >= fuseDepth {
			break
		}
		fused[s.chunk] += 1 / float64(rrfK+r+1)
		inDen[s.chunk] = s.score
	}

	ranked := make([]scored, 0, len(fused))
	for ci, s := range fused {
		ranked = append(ranked, scored{ci, s})
	}
	ix.sortScored(ranked)

	out := make([]Hit, 0, k)
	perDoc := map[string]int{}
	for _, s := range ranked {
		c := ix.chunks[s.chunk]
		if perDoc[c.docID] >= maxHitsPerDoc {
			continue
		}
		perDoc[c.docID]++

		via := "semantic"
		if inLex[s.chunk] {
			via = "lexical"
			if _, ok := inDen[s.chunk]; ok {
				via = "both"
			}
		}
		out = append(out, Hit{
			DocID: c.docID, DocName: c.docName, Chunk: c.no, Text: c.text,
			Score: s.score, Via: via, Cosine: inDen[s.chunk],
		})
		if len(out) == k {
			break
		}
	}
	return out
}

// ChunksFor reports how many chunks a document contributed, so the UI can show
// what indexing actually did with a file rather than just its byte count.
func (ix *Index) ChunksFor(docID string) int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n := 0
	for _, c := range ix.chunks {
		if c.docID == docID {
			n++
		}
	}
	return n
}
