package main

// The other half of retrieval: turning text into a vector.
//
// This talks to a SECOND llama-server, started by the launcher only when an
// embedding model is present in embed/ on the drive. It is deliberately not the
// router that serves the chat models. The router is started with --models-max N
// derived from the host's RAM, and on an 8GB machine N is 1 — so every search
// would evict the chat model and every answer would reload it. A dedicated
// process for a ~150MB model costs a fraction of a percent of the budget and
// removes that contention entirely.
//
// Everything here degrades to nothing. No embed/ model, a server that never
// comes up, a request that times out: each one leaves BM25 answering alone,
// which is exactly what this drive did before.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	// Inputs per request. llama-server processes each input as its own
	// sequence and the whole batch has to fit n_batch, so this is bounded by
	// the launcher's -b, not by anything here. Four ~1000-character chunks is
	// roughly 1000 tokens against a 2048-token batch.
	embedBatch = 4

	// Characters allowed per token when sizing an input against the encoder's
	// context. English prose runs about four; this is deliberately below that,
	// because the alternative to being conservative is a 400 from the server.
	//
	// An input that does not fit is NOT truncated by llama-server. It is
	// refused outright:
	//
	//   input (802 tokens) is larger than the max context size (512 tokens)
	//
	// and the whole batch fails with it. Retrieval encoders are small models
	// with small contexts — bge-small trains at 512 tokens — so a single
	// hard-split passage is enough to hit this.
	charsPerToken = 3

	// Used only until the probe reports the encoder's real context.
	fallbackMaxChars = 1536

	// A query embedding sits directly in front of the user's question, so it
	// gets a short leash. Missing the deadline costs semantic recall for that
	// one turn; blocking on it would cost the answer.
	queryTimeout = 3 * time.Second

	// Indexing runs in the background where latency is nobody's problem.
	indexTimeout = 120 * time.Second
)

// Embedder is a client for one llama-server running with --embeddings.
type Embedder struct {
	base   string
	client *http.Client

	mu       sync.RWMutex
	ready    bool
	model    string
	maxTok   int // the encoder's context, as it reports it
	dim      int
	lastErr  string
	queryPfx string
	docPfx   string
}

// Prefixes are not decoration. Every current retrieval encoder is trained with
// an asymmetric instruction — the query and the passage go in with different
// leading text — and dropping it measurably degrades ranking. They are also
// per-model, so they are picked from the model id the server reports and can be
// overridden by flag when a model is not one of these.
func prefixesFor(model string) (query, doc string) {
	m := strings.ToLower(modelName(model))
	switch {
	case strings.Contains(m, "embeddinggemma"), strings.Contains(m, "embedding-gemma"):
		return "task: search result | query: ", "title: none | text: "
	case strings.Contains(m, "nomic-embed"):
		return "search_query: ", "search_document: "
	case strings.Contains(m, "e5"): // multilingual-e5, e5-base/large
		return "query: ", "passage: "
	case strings.Contains(m, "bge"), strings.Contains(m, "mxbai"), strings.Contains(m, "gte"):
		return "Represent this sentence for searching relevant passages: ", ""
	case strings.Contains(m, "qwen3-embedding"):
		return "Instruct: Given a question, retrieve passages that answer it\nQuery: ", ""
	}
	return "", ""
}

// modelName reduces what the server reports to something stable.
//
// llama-server started with -m reports the model's ABSOLUTE PATH as its id:
//
//	/Volumes/Pocket-LLM/embed/bge-small-en-v1.5-q8_0.gguf
//
// That path is different on every machine the drive is plugged into — a
// different volume name on macOS, /media/<user>/... on Linux, a drive letter on
// Windows. Using it as the identity of the vector space would invalidate the
// entire embedding cache on arrival at each new machine and re-encode the whole
// corpus, which is the one cost this feature exists to avoid paying twice.
//
// The filename is the part that actually identifies the encoder, and it travels
// with the drive.
func modelName(id string) string {
	// Both separators, because the id comes from whichever platform wrote it.
	id = strings.ReplaceAll(id, "\\", "/")
	name := path.Base(id)
	// Only ".gguf", never "the extension". Encoder names carry version numbers
	// — nomic-embed-text-v1.5 — and a generic extension strip turns that into
	// nomic-embed-text-v1, which is a different key for the same model.
	if ext := name[max(0, len(name)-5):]; strings.EqualFold(ext, ".gguf") {
		name = name[:len(name)-5]
	}
	return name
}

func NewEmbedder(base, queryPfx, docPfx string) *Embedder {
	return &Embedder{
		base: strings.TrimSuffix(base, "/"),
		// No global timeout on the client: the two call sites have very
		// different deadlines and each passes its own context.
		client:   &http.Client{},
		queryPfx: queryPfx,
		docPfx:   docPfx,
	}
}

// Probe asks the server what it is serving and embeds one short string to learn
// the dimension. The dimension is discovered rather than configured because it
// is a property of the model file on the drive, and a number typed into a flag
// is a number that can be wrong.
func (e *Embedder) Probe(ctx context.Context) error {
	model, nCtx, err := e.serverModel(ctx)
	if err != nil {
		e.fail(err)
		return err
	}

	e.mu.Lock()
	e.model, e.maxTok = model, nCtx
	qp, dp := e.queryPfx, e.docPfx
	e.mu.Unlock()

	// "auto" is the default so that a drive works with whatever encoder is on
	// it; an explicit empty string from the flag stays empty.
	if qp == "auto" || dp == "auto" {
		aq, ad := prefixesFor(model)
		e.mu.Lock()
		if e.queryPfx == "auto" {
			e.queryPfx = aq
		}
		if e.docPfx == "auto" {
			e.docPfx = ad
		}
		e.mu.Unlock()
	}

	vecs, err := e.post(ctx, []string{"probe"})
	if err != nil {
		e.fail(err)
		return err
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		err := errors.New("embedding server returned no vector")
		e.fail(err)
		return err
	}

	e.mu.Lock()
	e.ready, e.dim, e.lastErr = true, len(vecs[0]), ""
	e.mu.Unlock()
	return nil
}

func (e *Embedder) fail(err error) {
	e.mu.Lock()
	e.ready, e.lastErr = false, err.Error()
	e.mu.Unlock()
}

func (e *Embedder) Ready() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.ready
}

// Model is the name the vector cache is keyed by, so it must be the stable one.
func (e *Embedder) Model() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return modelName(e.model)
}

// maxChars is the largest input this encoder will accept, derived from the
// context it reports rather than guessed at.
func (e *Embedder) maxChars() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.maxTok <= 0 {
		return fallbackMaxChars
	}
	return e.maxTok * charsPerToken
}

func (e *Embedder) Dim() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dim
}

func (e *Embedder) LastErr() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastErr
}

// serverModel reads the id and the context size from /v1/models. A dedicated
// embedding server has exactly one model, which is the whole reason it is a
// dedicated server.
//
// n_ctx is the useful half. It is the encoder's real limit after llama-server
// has capped whatever -c the launcher asked for down to the model's training
// context ("the slot context (2048) exceeds the training context of the model
// (512) - capping"), so it is the only number that says what an input may
// actually be.
func (e *Embedder) serverModel(ctx context.Context) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.base+"/v1/models", nil)
	if err != nil {
		return "", 0, err
	}
	res, err := e.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("/v1/models: HTTP %d", res.StatusCode)
	}
	var body struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				NCtx int `json:"n_ctx"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", 0, err
	}
	if len(body.Data) == 0 {
		return "", 0, errors.New("embedding server lists no model")
	}
	return body.Data[0].ID, body.Data[0].Meta.NCtx, nil
}

// errBadInput marks a request the server refused because of the input itself
// rather than because of its own health. The distinction decides everything
// downstream: a bad input is skipped and indexing carries on, an unhealthy
// server means backing off and trying the whole thing again later.
var errBadInput = errors.New("input rejected by the embedding server")

// truncate cuts to a rune count, never mid-rune. Slicing a UTF-8 string by
// bytes would hand the server a broken final character.
func truncate(s string, runes int) string {
	if runes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= runes {
		return s
	}
	return string(r[:runes])
}

// EmbedQuery and EmbedDocs differ only in which prefix is applied, but they are
// separate calls so a caller cannot get that wrong: embedding a question with
// the passage prefix is a silent, unmeasurable quality loss.
func (e *Embedder) EmbedQuery(ctx context.Context, q string) ([]float32, error) {
	e.mu.RLock()
	pfx := e.queryPfx
	e.mu.RUnlock()
	if pfx == "auto" {
		pfx = ""
	}
	vecs, err := e.post(ctx, []string{pfx + truncate(q, e.maxChars()-len(pfx))})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("embedding server returned the wrong number of vectors")
	}
	return vecs[0], nil
}

// EmbedDocs returns one entry per input, in order. A nil entry is a passage the
// encoder would not take and that no retry is going to fix — it is skipped, not
// an error, because one unusual paragraph must not stop a document from being
// indexed. An actual error here means the server is unhealthy.
func (e *Embedder) EmbedDocs(ctx context.Context, texts []string) ([][]float32, error) {
	e.mu.RLock()
	pfx := e.docPfx
	e.mu.RUnlock()
	if pfx == "auto" {
		pfx = ""
	}
	limit := e.maxChars() - len(pfx)

	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += embedBatch {
		j := min(i+embedBatch, len(texts))
		batch := make([]string, 0, j-i)
		for _, t := range texts[i:j] {
			batch = append(batch, pfx+truncate(t, limit))
		}

		vecs, err := e.post(ctx, batch)
		if err == nil {
			if len(vecs) != len(batch) {
				return nil, fmt.Errorf("asked for %d embeddings, got %d", len(batch), len(vecs))
			}
			out = append(out, vecs...)
			continue
		}
		if !errors.Is(err, errBadInput) {
			return nil, err
		}
		// llama-server refuses a whole batch for one bad member, so the others
		// are collateral. Retry them individually: one refused passage should
		// cost one passage, not the three it happened to travel with.
		for _, in := range batch {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			out = append(out, e.embedOne(ctx, in))
		}
	}
	return out, nil
}

// embedOne retries a single input, halving it each time it is refused.
//
// The character cap is derived from the encoder's context at three characters
// per token, which is right for prose and wrong by a factor of three for CJK or
// for a passage that is mostly punctuation. Rather than pick a cap pessimistic
// enough for the worst script — and truncate every English passage to a third
// of what it could hold — the common case is sized generously and the rare
// refusal is halved until it fits. Two or three steps converge from anywhere.
func (e *Embedder) embedOne(ctx context.Context, in string) []float32 {
	for attempt := 0; attempt < 3; attempt++ {
		vecs, err := e.post(ctx, []string{in})
		if err == nil && len(vecs) == 1 {
			return vecs[0]
		}
		if !errors.Is(err, errBadInput) {
			return nil
		}
		in = truncate(in, len([]rune(in))/2)
		if in == "" {
			return nil
		}
	}
	return nil
}

func (e *Embedder) post(ctx context.Context, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"input": inputs})
	if err != nil {
		return nil, err
	}

	var last error
	// One retry only. The background worker re-queues on failure anyway, and a
	// query embedding that has already missed its deadline is worth nothing.
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		vecs, err := e.postOnce(ctx, body)
		if err == nil {
			return vecs, nil
		}
		last = err
		// Sending the same rejected input again would get the same answer.
		if errors.Is(err, errBadInput) || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, last
}

func (e *Embedder) postOnce(ctx context.Context, body []byte) ([][]float32, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.base+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		e := fmt.Errorf("/v1/embeddings: HTTP %d: %s",
			res.StatusCode, strings.TrimSpace(string(msg)))
		// 4xx is about what was sent — an oversized passage, most likely.
		// 5xx and a still-loading 503 are about the server, and are worth
		// waiting out.
		if res.StatusCode >= 400 && res.StatusCode < 500 {
			return nil, fmt.Errorf("%w: %s", errBadInput, e)
		}
		return nil, e
	}

	var parsed struct {
		Data []struct {
			Index     int             `json:"index"`
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return nil, err
	}

	out := make([][]float32, len(parsed.Data))
	for _, d := range parsed.Data {
		v, err := decodeEmbedding(d.Embedding)
		if err != nil {
			return nil, err
		}
		// The server reports the input index rather than guaranteeing order.
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embedding index %d out of range", d.Index)
		}
		out[d.Index] = normalize(v)
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("no embedding for input %d", i)
		}
	}
	return out, nil
}

// decodeEmbedding accepts both shapes llama-server can produce. With a pooling
// type set the field is a flat vector; with --pooling none it is one vector per
// token, which has to be mean-pooled here or the drive silently indexes the
// embedding of a single token.
func decodeEmbedding(raw json.RawMessage) ([]float32, error) {
	var flat []float32
	if err := json.Unmarshal(raw, &flat); err == nil {
		if len(flat) == 0 {
			return nil, errors.New("empty embedding")
		}
		return flat, nil
	}
	var rows [][]float32
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, errors.New("embedding is neither a vector nor a list of vectors")
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return nil, errors.New("empty embedding")
	}
	acc := make([]float32, len(rows[0]))
	for _, r := range rows {
		if len(r) != len(acc) {
			return nil, errors.New("ragged token embeddings")
		}
		for i, x := range r {
			acc[i] += x
		}
	}
	for i := range acc {
		acc[i] /= float32(len(rows))
	}
	return acc, nil
}

// normalize makes cosine similarity a plain dot product, so scoring the whole
// corpus is one multiply-add per dimension with no square roots in the loop.
// llama-server normalizes by default, but "by default" is not a guarantee and
// an unnormalized vector would quietly rank by passage length.
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}

func dot(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return float64(s)
}
