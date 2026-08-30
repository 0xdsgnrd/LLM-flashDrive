package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------------------ fake server

// A stand-in for llama-server --embeddings. The real one cannot be a test
// dependency: it needs a model file, several hundred megabytes of it, on a
// drive that may not be plugged in.
type fakeEmbed struct {
	model     string
	dim       int
	nCtx      int  // reported context; longer inputs are refused outright
	refuseAll bool // refuse every input, however short
	nested    bool // reply with per-token vectors, as --pooling none does
	fail      int  // fail this many requests before answering
	calls     int
	batch     int // largest batch seen
	seen      []string
}

func (f *fakeEmbed) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id": f.model, "meta": map[string]int{"n_ctx": f.nCtx},
			}},
		})
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if f.fail > 0 {
			f.fail--
			http.Error(w, "loading model", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Input) > f.batch {
			f.batch = len(body.Input)
		}
		f.seen = append(f.seen, body.Input...)

		// llama-server refuses the WHOLE request when any one input exceeds the
		// context — it does not truncate and it does not answer the others.
		if f.nCtx > 0 || f.refuseAll {
			for _, in := range body.Input {
				if f.refuseAll || len([]rune(in)) > f.nCtx {
					http.Error(w, `{"error":{"message":"input is larger than the max context size"}}`,
						http.StatusBadRequest)
					return
				}
			}
		}

		data := []map[string]any{}
		for i, in := range body.Input {
			vec := toyVector(in, f.dim)
			var payload any = vec
			if f.nested {
				// Two identical token rows: mean-pooling them must give back
				// exactly the same vector.
				payload = [][]float32{vec, vec}
			}
			data = append(data, map[string]any{"index": i, "embedding": payload})
		}
		// Deliberately reversed. The response carries an index per item and
		// nothing promises order, so a client that trusts order is wrong.
		for i, j := 0, len(data)-1; i < j; i, j = i+1, j-1 {
			data[i], data[j] = data[j], data[i]
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// toyVector is a deterministic bag-of-words embedding: each distinct word is
// hashed to a dimension. Texts that share vocabulary come out similar and texts
// that do not come out close to orthogonal, which is the property of a real
// encoder these tests depend on.
//
// A letter histogram was tried first and is useless here: every English text
// has roughly the same letter distribution, so two unrelated passages score
// ~0.95 and nothing can be told apart.
func toyVector(s string, dim int) []float32 {
	v := make([]float32, dim)
	for _, w := range strings.Fields(strings.ToLower(s)) {
		h := uint32(2166136261)
		for _, r := range w {
			h = (h ^ uint32(r)) * 16777619
		}
		v[int(h%uint32(dim))]++
	}
	return normalize(v)
}

// ------------------------------------------------------------ embed client

func TestEmbedderProbeAndEmbed(t *testing.T) {
	f := &fakeEmbed{model: "nomic-embed-text-v1.5", dim: 8}
	e := NewEmbedder(f.server(t).URL, "auto", "auto")

	if err := e.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !e.Ready() || e.Dim() != 8 || e.Model() != "nomic-embed-text-v1.5" {
		t.Fatalf("after probe: ready=%v dim=%d model=%q", e.Ready(), e.Dim(), e.Model())
	}
	// The dimension is discovered from the server, never configured.
	if e.Dim() != f.dim {
		t.Errorf("dim %d, server serves %d", e.Dim(), f.dim)
	}

	vecs, err := e.EmbedDocs(context.Background(), []string{"one", "two", "three"})
	if err != nil {
		t.Fatalf("EmbedDocs: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors for 3 inputs", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 8 {
			t.Errorf("vector %d has %d dimensions", i, len(v))
		}
	}
	// Out-of-order responses must still land against the right input.
	want := toyVector("search_document: two", 8)
	if math.Abs(dot(vecs[1], want)-1) > 1e-5 {
		t.Errorf("vector 1 is not the embedding of input 1 — response order was trusted")
	}
}

// A model trained with an asymmetric instruction and asked without one is a
// silent quality loss, so the prefix is applied and it differs by direction.
func TestEmbedderAppliesAsymmetricPrefixes(t *testing.T) {
	f := &fakeEmbed{model: "multilingual-e5-small", dim: 8}
	e := NewEmbedder(f.server(t).URL, "auto", "auto")
	if err := e.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	f.seen = nil

	e.EmbedQuery(context.Background(), "how do I eject")
	e.EmbedDocs(context.Background(), []string{"eject the drive first"})

	if len(f.seen) != 2 {
		t.Fatalf("server saw %d inputs, want 2: %q", len(f.seen), f.seen)
	}
	if !strings.HasPrefix(f.seen[0], "query: ") {
		t.Errorf("query went in as %q, want the e5 query prefix", f.seen[0])
	}
	if !strings.HasPrefix(f.seen[1], "passage: ") {
		t.Errorf("passage went in as %q, want the e5 passage prefix", f.seen[1])
	}
}

func TestPrefixesForKnownEncoders(t *testing.T) {
	cases := []struct{ model, wantQ string }{
		{"embeddinggemma-300m-Q8_0", "task: search result | query: "},
		{"nomic-embed-text-v1.5.Q8_0", "search_query: "},
		{"multilingual-e5-small-f16", "query: "},
		{"bge-small-en-v1.5-q8_0", "Represent this sentence for searching relevant passages: "},
		{"some-unknown-encoder", ""},
	}
	for _, c := range cases {
		if q, _ := prefixesFor(c.model); q != c.wantQ {
			t.Errorf("prefixesFor(%q) query = %q, want %q", c.model, q, c.wantQ)
		}
	}
}

// An explicit flag must survive; only "auto" is replaced.
func TestExplicitPrefixOverridesAuto(t *testing.T) {
	f := &fakeEmbed{model: "multilingual-e5-small", dim: 8}
	e := NewEmbedder(f.server(t).URL, "ASK: ", "")
	if err := e.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	f.seen = nil
	e.EmbedQuery(context.Background(), "hello")
	e.EmbedDocs(context.Background(), []string{"world"})
	if f.seen[0] != "ASK: hello" {
		t.Errorf("query = %q, want the flag's prefix", f.seen[0])
	}
	if f.seen[1] != "world" {
		t.Errorf("passage = %q, want no prefix (empty is not auto)", f.seen[1])
	}
}

// --pooling none returns one vector per token. Taking the first would index the
// embedding of a single token and nothing would ever look wrong.
func TestEmbedderMeanPoolsTokenVectors(t *testing.T) {
	f := &fakeEmbed{model: "bge-small-en-v1.5", dim: 8, nested: true}
	e := NewEmbedder(f.server(t).URL, "", "")
	if err := e.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	vecs, err := e.EmbedDocs(context.Background(), []string{"drive"})
	if err != nil {
		t.Fatalf("EmbedDocs: %v", err)
	}
	if math.Abs(dot(vecs[0], toyVector("drive", 8))-1) > 1e-5 {
		t.Error("token vectors were not mean-pooled back to the passage vector")
	}
}

func TestEmbedderBatchesLargeInputs(t *testing.T) {
	f := &fakeEmbed{model: "e5", dim: 8}
	e := NewEmbedder(f.server(t).URL, "", "")
	if err := e.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	texts := make([]string, 30)
	for i := range texts {
		texts[i] = fmt.Sprintf("passage number %d", i)
	}
	vecs, err := e.EmbedDocs(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedDocs: %v", err)
	}
	if len(vecs) != 30 {
		t.Fatalf("got %d vectors for 30 inputs", len(vecs))
	}
	if f.batch > embedBatch {
		t.Errorf("sent a batch of %d, cap is %d", f.batch, embedBatch)
	}
}

func TestEmbedderReportsAnUnavailableServer(t *testing.T) {
	e := NewEmbedder("http://127.0.0.1:1", "", "")
	if err := e.Probe(context.Background()); err == nil {
		t.Fatal("probing a dead port should fail")
	}
	if e.Ready() {
		t.Error("embedder reports ready after a failed probe")
	}
	if e.LastErr() == "" {
		t.Error("no error recorded for the UI to show")
	}
}

func TestEmbedQueryRespectsItsDeadline(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
	}))
	defer slow.Close()

	e := NewEmbedder(slow.URL, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.EmbedQuery(ctx, "anything"); err == nil {
		t.Fatal("a query embedding past its deadline must fail rather than block the answer")
	}
}

// ------------------------------------------------------------ vector cache

func newVecs(t *testing.T) (*VecStore, string) {
	t.Helper()
	dir := t.TempDir()
	return NewVecStore(dir), dir
}

func TestVecStoreRoundTrip(t *testing.T) {
	vs, _ := newVecs(t)
	id := newID()
	in := map[chunkHash][]float32{
		hashChunk("alpha"): {0.5, 0.5, 0.5, 0.5},
		hashChunk("bravo"): {1, 0, 0, 0},
	}
	if err := vs.Save(id, "e5", 4, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out := vs.Load(id, "e5")
	if len(out) != 2 {
		t.Fatalf("loaded %d vectors, saved 2", len(out))
	}
	for h, want := range in {
		got, ok := out[h]
		if !ok {
			t.Fatalf("hash missing after round trip")
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("vector changed: %v vs %v", got, want)
			}
		}
	}
}

// Swapping the embedding model on the drive must not mix two vector spaces.
func TestVecStoreRejectsAnotherModel(t *testing.T) {
	vs, _ := newVecs(t)
	id := newID()
	vs.Save(id, "e5", 4, map[chunkHash][]float32{hashChunk("x"): {1, 0, 0, 0}})
	if got := vs.Load(id, "bge"); len(got) != 0 {
		t.Errorf("loaded %d vectors written by a different model", len(got))
	}
}

// The exact shape a yanked drive leaves behind.
func TestVecStoreDropsATruncatedFile(t *testing.T) {
	vs, dir := newVecs(t)
	id := newID()
	vs.Save(id, "e5", 4, map[chunkHash][]float32{
		hashChunk("x"): {1, 0, 0, 0},
		hashChunk("y"): {0, 1, 0, 0},
	})
	p := filepath.Join(dir, id+".vec")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw[:len(raw)-9], 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vs.Load(id, "e5"); len(got) != 0 {
		t.Errorf("a truncated cache loaded %d vectors instead of being discarded", len(got))
	}
}

func TestVecStoreDropsNaN(t *testing.T) {
	vs, _ := newVecs(t)
	id := newID()
	nan := float32(math.NaN())
	// Save refuses nothing here — the point is that Load must not hand a NaN
	// to the ranking, where it would sort as neither greater nor less.
	vs.Save(id, "e5", 4, map[chunkHash][]float32{hashChunk("x"): {nan, 0, 0, 0}})
	if got := vs.Load(id, "e5"); len(got) != 0 {
		t.Error("a NaN vector survived the load")
	}
}

func TestVecStoreWipeAndSweep(t *testing.T) {
	vs, dir := newVecs(t)
	keep, gone := newID(), newID()
	vs.Save(keep, "e5", 2, map[chunkHash][]float32{hashChunk("a"): {1, 0}})
	vs.Save(gone, "e5", 2, map[chunkHash][]float32{hashChunk("b"): {0, 1}})

	vs.Sweep(map[string]bool{keep: true})
	if _, err := os.Stat(filepath.Join(dir, gone+".vec")); !os.IsNotExist(err) {
		t.Error("sweep left a cache for a document that no longer exists")
	}
	if _, err := os.Stat(filepath.Join(dir, keep+".vec")); err != nil {
		t.Error("sweep removed a live document's cache")
	}

	if n := vs.Wipe(); n != 1 {
		t.Errorf("wipe removed %d files, want 1", n)
	}
	if got := vs.Load(keep, "e5"); len(got) != 0 {
		t.Error("vectors survived a wipe — erasing documents must erase them too")
	}
}

// ---------------------------------------------------------- hybrid ranking

// buildSemantic indexes some documents and attaches toy vectors for every
// passage, as the background worker would.
func buildSemantic(t *testing.T, texts map[string]string, dim int) *Index {
	t.Helper()
	d := newDocs(t)
	for name, body := range texts {
		if _, err := d.AddText(name, body); err != nil {
			t.Fatalf("AddText: %v", err)
		}
	}
	ix := buildIndex(t, d)

	vecs := map[chunkHash][]float32{}
	metas, all, _ := d.All()
	_ = metas
	for _, body := range all {
		for _, c := range chunk(body) {
			vecs[hashChunk(c)] = toyVector(c, dim)
		}
	}
	ix.AttachVectors("toy", dim, vecs)
	return ix
}

// The case the whole feature exists for: a question whose words appear nowhere
// in the document that answers it.
func TestSemanticFindsWhatLexicalCannot(t *testing.T) {
	docs := map[string]string{
		"vehicles.md": "An automobile is a wheeled motor vehicle used for transporting people.",
		"baking.md":   "Preheat the oven and whisk eggs with sugar until pale before folding flour.",
		"garden.md":   "Prune roses during late winter dormancy just above an outward facing bud.",
		"finance.md":  "Compound interest accrues on the principal and on previously accumulated interest.",
		"chess.md":    "The knight jumps in an L shape, two squares along one axis then one across.",
		"weather.md":  "A cold front arrives when colder air displaces warmer air, often bringing storms.",
		"coffee.md":   "Espresso forces pressurised hot water through finely ground beans at nine bars.",
		"sleep.md":    "Adults cycle through four stages of rest roughly every ninety minutes nightly.",
		"music.md":    "A major scale follows whole, whole, half, whole, whole, whole, half intervals.",
		"geology.md":  "Sedimentary strata form as mineral particles settle and compact over long ages.",
	}
	ix := buildSemantic(t, docs, 64)

	// "car" shares no term with "automobile", so BM25 has nothing.
	if hits := ix.Search("car", 4, nil); len(hits) != 0 {
		t.Fatalf("lexical search found %d hit(s) for a word not in the corpus", len(hits))
	}

	qvec := toyVector("An automobile is a wheeled motor vehicle", 64)
	hits := ix.Search("car", 4, qvec)
	if len(hits) == 0 {
		t.Fatal("semantic search found nothing")
	}
	if hits[0].DocName != "vehicles.md" {
		t.Errorf("top hit = %s, want vehicles.md", hits[0].DocName)
	}
	if hits[0].Via != "semantic" {
		t.Errorf("via = %q, want semantic — no term is shared", hits[0].Via)
	}
	if hits[0].Cosine <= 0 {
		t.Errorf("cosine = %v, want the similarity that produced the hit", hits[0].Cosine)
	}
}

// Adding a retriever must not remove the guarantee the other one gave. With no
// query vector the results are exactly the lexical ones.
func TestNilQueryVectorIsPurelyLexical(t *testing.T) {
	docs := map[string]string{
		"eject.md":  "Always eject the drive before pulling it out of the socket.",
		"baking.md": "Whisk the eggs and the sugar together until they are pale.",
	}
	ix := buildSemantic(t, docs, 64)

	with := ix.Search("eject the drive", 4, nil)
	if len(with) == 0 || with[0].DocName != "eject.md" {
		t.Fatalf("lexical ranking changed: %+v", with)
	}
	for _, h := range with {
		if h.Via != "lexical" {
			t.Errorf("via = %q with no query vector, want lexical", h.Via)
		}
		if h.Cosine != 0 {
			t.Errorf("cosine = %v with no query vector", h.Cosine)
		}
	}
}

// A passage both retrievers agree on should outrank one found by either alone.
// That is the entire argument for fusing instead of concatenating.
func TestFusionPrefersAgreement(t *testing.T) {
	docs := map[string]string{
		"both.md":    "eject the drive safely before removing it from the machine",
		"baking.md":  "Preheat the oven and whisk eggs with sugar until pale before folding flour.",
		"garden.md":  "Prune roses during late winter dormancy just above an outward facing bud.",
		"finance.md": "Compound interest accrues on the principal and on previously accumulated sums.",
		"chess.md":   "The knight jumps in an L shape, two squares along one axis then one across.",
		"weather.md": "A cold front arrives when colder air displaces warmer air, often bringing storms.",
		"coffee.md":  "Espresso forces pressurised hot water through finely ground beans at nine bars.",
		"sleep.md":   "Adults cycle through four stages of rest roughly every ninety minutes nightly.",
		"music.md":   "A major scale follows whole, whole, half, whole, whole, whole, half intervals.",
		// Shares the query's rarest term, so BM25 has a competitor to rank.
		"eject.md": "eject the cartridge from the console before switching the power off",
	}
	ix := buildSemantic(t, docs, 64)

	qvec := toyVector("eject the drive safely before removing it from the machine", 64)
	hits := ix.Search("eject drive safely removing machine", 4, qvec)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].DocName != "both.md" {
		t.Errorf("top hit = %s, want both.md", hits[0].DocName)
	}
	if hits[0].Via != "both" {
		t.Errorf("via = %q, want both", hits[0].Via)
	}
}

// Dense retrieval always has an answer. Without a floor, an unrelated question
// would ground itself in whatever happened to be nearest.
func TestSemanticFloorRejectsAnIndifferentQuery(t *testing.T) {
	docs := map[string]string{}
	for i := 0; i < 12; i++ {
		docs[fmt.Sprintf("d%d.md", i)] = "aaaa bbbb cccc dddd eeee ffff gggg."
	}
	ix := buildSemantic(t, docs, 64)

	// Every passage identical means every similarity identical: the encoder
	// has no opinion, and no opinion must not become a citation.
	qvec := toyVector("completely different subject", 64)
	if hits := ix.Search("zzzz", 4, qvec); len(hits) != 0 {
		t.Errorf("a query the encoder cannot discriminate on returned %d hit(s)", len(hits))
	}
}

func TestSemanticRespectsThePerDocumentCap(t *testing.T) {
	body := strings.Repeat("ejecting the drive matters a great deal here. ", 200)
	docs := map[string]string{"long.md": body}
	for i := 0; i < 9; i++ {
		docs[fmt.Sprintf("s%d.md", i)] = fmt.Sprintf("ejecting the drive matters note %d here", i)
	}
	ix := buildSemantic(t, docs, 64)

	qvec := toyVector(body[:200], 64)
	hits := ix.Search("ejecting the drive", 8, qvec)
	per := map[string]int{}
	for _, h := range hits {
		per[h.DocName]++
	}
	if per["long.md"] > maxHitsPerDoc {
		t.Errorf("long.md took %d slots even through the dense path, cap is %d", per["long.md"], maxHitsPerDoc)
	}
}

// Vectors are keyed by the text they came from, so a rebuild that reorders or
// re-adds documents can never pair a vector with the wrong passage.
func TestVectorsSurviveARebuildByContentHash(t *testing.T) {
	d := newDocs(t)
	d.AddText("a.md", "the first document about ejecting drives")
	d.AddText("b.md", "the second document about baking bread")

	ix := buildIndex(t, d)
	vecs := map[chunkHash][]float32{
		hashChunk("the first document about ejecting drives"): {1, 0},
		hashChunk("the second document about baking bread"):   {0, 1},
	}
	if n := ix.AttachVectors("toy", 2, vecs); n != 2 {
		t.Fatalf("attached %d vectors, want 2", n)
	}

	// A third document changes every chunk index in the corpus.
	d.AddText("c.md", "a third document appears")
	metas, texts, _ := d.All()
	ix.Build(metas, texts)
	if got, _ := ix.VecStats(); got != 0 {
		t.Fatalf("%d vectors survived a rebuild — they must be reattached, not carried", got)
	}
	if n := ix.AttachVectors("toy", 2, vecs); n != 2 {
		t.Fatalf("reattached %d vectors after the rebuild, want 2", n)
	}

	// Now ask for a vector that only "a.md" should match.
	hits := ix.Search("", 1, []float32{1, 0})
	if len(hits) != 1 || hits[0].DocName != "a.md" {
		t.Errorf("vector landed on the wrong passage after a rebuild: %+v", hits)
	}
}

func TestAttachVectorsDiscardsAnotherModelsSpace(t *testing.T) {
	d := newDocs(t)
	d.AddText("a.md", "one document")
	ix := buildIndex(t, d)

	ix.AttachVectors("e5", 2, map[chunkHash][]float32{hashChunk("one document"): {1, 0}})
	if n, _ := ix.VecStats(); n != 1 {
		t.Fatalf("embedded = %d, want 1", n)
	}
	// A different model is a different space; the old vectors are meaningless
	// in it and must not be ranked alongside the new ones.
	ix.AttachVectors("bge", 3, map[chunkHash][]float32{})
	if n, _ := ix.VecStats(); n != 0 {
		t.Errorf("%d vector(s) from the previous model survived a model change", n)
	}
}

func TestPendingForListsOnlyUnembeddedPassages(t *testing.T) {
	d := newDocs(t)
	m, _ := d.AddText("a.md", "first paragraph here\n\n"+strings.Repeat("second paragraph ", 80))
	ix := buildIndex(t, d)

	_, texts := ix.PendingFor(m.ID)
	if len(texts) == 0 {
		t.Fatal("nothing pending on a freshly built index")
	}
	total := len(texts)

	ix.AttachVectors("toy", 2, map[chunkHash][]float32{hashChunk(texts[0]): {1, 0}})
	_, after := ix.PendingFor(m.ID)
	if len(after) != total-1 {
		t.Errorf("pending = %d after embedding one of %d", len(after), total)
	}
}

func TestNormalizeMakesCosineADotProduct(t *testing.T) {
	v := normalize([]float32{3, 4})
	if math.Abs(dot(v, v)-1) > 1e-6 {
		t.Errorf("normalized vector has length %v, want 1", math.Sqrt(dot(v, v)))
	}
	if got := normalize([]float32{0, 0}); dot(got, got) != 0 {
		t.Error("normalizing a zero vector should leave it alone, not divide by zero")
	}
}

// ------------------------------------------------------- oversized inputs

// The identity of the vector space must travel with the drive. llama-server
// reports the model's absolute path, which is different on every machine.
func TestModelNameIsStableAcrossMachines(t *testing.T) {
	same := []string{
		"/Volumes/Pocket-LLM/embed/nomic-embed-text-v1.5.Q8_0.gguf",
		"/media/kris/POCKET/embed/nomic-embed-text-v1.5.Q8_0.gguf",
		`E:\embed\nomic-embed-text-v1.5.Q8_0.gguf`,
	}
	want := modelName(same[0])
	if want != "nomic-embed-text-v1.5.Q8_0" {
		t.Fatalf("modelName = %q, want the filename with only .gguf removed", want)
	}
	for _, id := range same[1:] {
		if got := modelName(id); got != want {
			t.Errorf("modelName(%q) = %q, want %q — the cache would be rebuilt on that machine", id, got, want)
		}
	}
}

// An oversized passage is refused outright, and it takes its whole batch down
// with it. Losing three good passages to one bad one would be a bug that only
// shows up on documents with one long paragraph.
func TestOneRefusedPassageDoesNotLoseItsBatch(t *testing.T) {
	f := &fakeEmbed{model: "bge-small-en-v1.5", dim: 8, nCtx: 64}
	e := NewEmbedder(f.server(t).URL, "", "")
	if err := e.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	texts := []string{"short one", "short two", strings.Repeat("x", 5000), "short four"}
	vecs, err := e.EmbedDocs(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedDocs returned an error instead of skipping: %v", err)
	}
	if len(vecs) != 4 {
		t.Fatalf("got %d results for 4 inputs", len(vecs))
	}
	for _, i := range []int{0, 1, 3} {
		if vecs[i] == nil {
			t.Errorf("passage %d was lost to a different passage being too long", i)
		}
	}
}

// Cutting an input down is the right first response; giving up on it is the
// right last one. Neither may stall the pass.
func TestAnUnencodablePassageIsSkippedNotFatal(t *testing.T) {
	// Refused at every length, so halving never rescues it and the give-up
	// path is the one under test.
	f := &fakeEmbed{model: "toy", dim: 8, refuseAll: true}
	e := NewEmbedder(f.server(t).URL, "", "")
	e.mu.Lock()
	e.model, e.maxTok, e.dim, e.ready = "toy", 512, 8, true
	e.mu.Unlock()

	vecs, err := e.EmbedDocs(context.Background(), []string{"a passage that will never fit"})
	if err != nil {
		t.Fatalf("an unencodable passage must not be an error: %v", err)
	}
	if len(vecs) != 1 || vecs[0] != nil {
		t.Errorf("want one nil result, got %v", vecs)
	}
}

// A passage inside the limit must not be truncated on the way in.
func TestNormalPassagesAreNotTruncated(t *testing.T) {
	f := &fakeEmbed{model: "toy", dim: 8, nCtx: 512}
	e := NewEmbedder(f.server(t).URL, "", "")
	if err := e.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	f.seen = nil

	body := strings.Repeat("passage ", 120) // ~960 chars, a normal chunk
	if _, err := e.EmbedDocs(context.Background(), []string{body}); err != nil {
		t.Fatalf("EmbedDocs: %v", err)
	}
	if f.seen[0] != body {
		t.Errorf("a %d-char passage was altered before sending (%d chars arrived)",
			len(body), len(f.seen[0]))
	}
}

// A server that is still loading its model answers 503. That is worth waiting
// out, unlike a 400, which will say the same thing forever.
func TestTransientFailureIsRetriedButBadInputIsNot(t *testing.T) {
	f := &fakeEmbed{model: "toy", dim: 8, fail: 1}
	e := NewEmbedder(f.server(t).URL, "", "")
	if err := e.Probe(context.Background()); err != nil {
		t.Fatalf("Probe should ride out one 503: %v", err)
	}

	f2 := &fakeEmbed{model: "toy", dim: 8, nCtx: 2}
	e2 := NewEmbedder(f2.server(t).URL, "", "")
	e2.mu.Lock()
	e2.model, e2.maxTok, e2.ready = "toy", 2, true
	e2.mu.Unlock()
	f2.calls = 0
	e2.EmbedDocs(context.Background(), []string{"far too long for two tokens"})
	// One batch attempt, then three single attempts that halve. Retrying a
	// refusal at the same size would double every one of those.
	if f2.calls > 4 {
		t.Errorf("a refused input was retried %d times; it should not be retried at the same size", f2.calls)
	}
}

// The finding that shaped the floor. Measured on bge-small over a ten-document
// corpus, a nonsense query scored its best passage at z = +2.48 against the
// query's own mean while a real question scored its correct answer at +2.20 —
// the noise ranked higher. Judged against what unrelated passages of the same
// corpus actually look like, the two separate cleanly.
func TestNoiseQueryIsRejectedByTheCorpusBaseline(t *testing.T) {
	docs := map[string]string{
		"vehicles.md": "An automobile is a wheeled motor vehicle used for transporting people.",
		"baking.md":   "Preheat the oven and whisk eggs with sugar until pale before folding flour.",
		"garden.md":   "Prune roses during late winter dormancy just above an outward facing bud.",
		"finance.md":  "Compound interest accrues on the principal and on previously accumulated interest.",
		"chess.md":    "The knight jumps in an L shape, two squares along one axis then one across.",
		"weather.md":  "A cold front arrives when colder air displaces warmer air, often bringing storms.",
		"coffee.md":   "Espresso forces pressurised hot water through finely ground beans at nine bars.",
		"sleep.md":    "Adults cycle through four stages of rest roughly every ninety minutes nightly.",
		"music.md":    "A major scale follows whole, whole, half, whole, whole, whole, half intervals.",
		"geology.md":  "Sedimentary strata form as mineral particles settle and compact over long ages.",
	}
	ix := buildSemantic(t, docs, 64)

	mean, sd, pairs := ix.Baseline()
	if pairs < minBaselinePairs {
		t.Fatalf("baseline measured from %d pairs, want at least %d", pairs, minBaselinePairs)
	}
	if sd <= 0 {
		t.Fatalf("baseline sd = %v — nothing to calibrate against", sd)
	}
	t.Logf("baseline: mean=%.3f sd=%.3f over %d pairs", mean, sd, pairs)

	// Shares no vocabulary with anything on the drive.
	noise := toyVector("xylophone marsupial quantum kumquat", 64)
	if hits := ix.Search("xylophone marsupial quantum", 4, noise); len(hits) != 0 {
		t.Errorf("a nonsense query grounded itself in %d passage(s): %+v", len(hits), hits)
	}

	// The same corpus must still answer a question it does have an answer to.
	real := toyVector("whisk eggs with sugar in the oven", 64)
	hits := ix.Search("whisk eggs sugar oven", 4, real)
	if len(hits) == 0 || hits[0].DocName != "baking.md" {
		t.Errorf("the floor also rejected a real match: %+v", hits)
	}
}

// The baseline describes a corpus. Rebuilding replaces the corpus, so carrying
// the old one forward would judge new passages against a drive that is gone.
func TestBaselineIsResetOnRebuild(t *testing.T) {
	docs := map[string]string{}
	for i := 0; i < 10; i++ {
		docs[fmt.Sprintf("d%d.md", i)] = fmt.Sprintf("document %d about subject %d only", i, i)
	}
	ix := buildSemantic(t, docs, 64)
	if _, _, pairs := ix.Baseline(); pairs < minBaselinePairs {
		t.Fatalf("no baseline after attaching vectors (%d pairs)", pairs)
	}

	ix.Build(nil, nil)
	if _, sd, pairs := ix.Baseline(); pairs != 0 || sd != 0 {
		t.Errorf("baseline survived a rebuild: sd=%v pairs=%d", sd, pairs)
	}
}
