package main

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Lexical retrieval (BM25), not embeddings.
//
// The deliberate trade: BM25 needs no embedding model on the drive, no second
// llama-server instance, no extra RAM, and no entry in drive.lock — and for
// searching your own notes it is a strong baseline, because the words you
// search with are usually the words you wrote. What it cannot do is match on
// meaning alone: ask about "cars" and a document that only ever says
// "automobile" will not come back. That is the upgrade path, not a defect here.
const (
	bm25K1 = 1.2
	bm25B  = 0.75

	maxHitsPerDoc = 2 // one long document must not crowd out every other source
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
}

type Index struct {
	mu       sync.RWMutex
	chunks   []indexedChunk
	postings map[string][]posting
	avgLen   float64
	docCount int
}

func NewIndex() *Index { return &Index{postings: map[string][]posting{}} }

// Build replaces the whole index. Rebuilding beats incremental updates here:
// the corpus is small, and an index that can only ever be wrong by being stale
// is much easier to reason about than one maintained by a diff.
func (ix *Index) Build(metas []DocMeta, texts []string) {
	chunks := []indexedChunk{}
	for i, m := range metas {
		for n, c := range chunk(texts[i]) {
			chunks = append(chunks, indexedChunk{
				docID: m.ID, docName: m.Name, no: n, text: c, length: len(tokenize(c)),
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
}

func (ix *Index) Stats() (docs, chunks int) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.docCount, len(ix.chunks)
}

func (ix *Index) Search(query string, k int) []Hit {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	if len(ix.chunks) == 0 || k <= 0 {
		return []Hit{}
	}
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
	if len(scores) == 0 {
		return []Hit{}
	}

	ranked := make([]Hit, 0, len(scores))
	for ci, s := range scores {
		c := ix.chunks[ci]
		ranked = append(ranked, Hit{DocID: c.docID, DocName: c.docName, Chunk: c.no,
			Text: c.text, Score: s})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		// Deterministic order for equal scores, so repeating a search does not
		// silently change which passage the model is shown.
		if ranked[i].DocID != ranked[j].DocID {
			return ranked[i].DocID < ranked[j].DocID
		}
		return ranked[i].Chunk < ranked[j].Chunk
	})

	out := make([]Hit, 0, k)
	perDoc := map[string]int{}
	for _, h := range ranked {
		if perDoc[h.DocID] >= maxHitsPerDoc {
			continue
		}
		perDoc[h.DocID]++
		out = append(out, h)
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
