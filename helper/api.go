package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type API struct {
	Store *Store
	Docs  *DocStore
	Index *Index
	// nil when the drive carries no embedding model. Every method on it is
	// nil-safe, so the lexical path needs no branch of its own.
	Vectors *Vectorizer
}

// Rebuild the whole index after any change to the corpus. Cheap at this scale,
// and it removes an entire class of bug: the index cannot drift from the files.
func (a *API) reindex() {
	if a.Docs == nil {
		return
	}
	metas, texts, err := a.Docs.All()
	if err != nil {
		return
	}
	a.Index.Build(metas, texts)
	// Rebuilding drops every vector — the index has no model to recompute them
	// with. They come back by content hash, from the drive's cache for passages
	// that have not changed and from the embedding server for the ones that
	// have, all of it behind this call rather than inside it.
	a.Vectors.Wake()
}

// Everything the browser can persist goes through here. It is deliberately the
// only writer: llama-server serves files read-only, so if pocketd is not
// running the UI can tell the difference and say so, rather than quietly
// falling back to browser storage and leaving a trail on the host machine.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api")

	if path == "/health" {
		docs, chunks := 0, 0
		if a.Index != nil {
			docs, chunks = a.Index.Stats()
		}
		body := map[string]any{
			"ok": true, "history": a.Store != nil, "search": a.Docs != nil,
			"docs": docs, "chunks": chunks,
		}
		// Reported separately from "search" because they fail separately: the
		// lexical half can be working perfectly while the embedding server is
		// still loading its model, and the UI should say which.
		ready, working, model, note, embedded, total := a.Vectors.Status()
		body["semantic"] = ready
		body["semanticModel"] = model
		body["semanticWorking"] = working
		body["semanticNote"] = note
		body["embedded"] = embedded
		body["embeddable"] = total
		writeJSON(w, http.StatusOK, body)
		return
	}

	// Documents and search do not depend on chat storage, so they are routed
	// before the history check below.
	switch {
	case path == "/docs":
		a.docs(w, r)
		return
	case strings.HasPrefix(path, "/docs/"):
		a.doc(w, r, strings.TrimPrefix(path, "/docs/"))
		return
	case path == "/search":
		a.search(w, r)
		return
	}

	if a.Store == nil {
		// 503 rather than 404: the route exists, the drive just cannot be
		// written to. The UI shows "history off" on exactly this.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "chat storage unavailable on this drive",
		})
		return
	}

	switch {
	case path == "/settings":
		a.settings(w, r)
	case path == "/chats":
		a.chats(w, r)
	case strings.HasSuffix(path, "/last") && strings.HasPrefix(path, "/chats/"):
		a.chatLast(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/chats/"), "/last"))
	case strings.HasPrefix(path, "/chats/"):
		a.chat(w, r, strings.TrimPrefix(path, "/chats/"))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such endpoint"})
	}
}

func (a *API) chats(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.Store.List()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)

	case http.MethodPost:
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		meta, err := a.Store.Create(body.Model)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, meta)

	case http.MethodDelete:
		n, err := a.Store.Wipe()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"deleted": n})

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) chat(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		c, err := a.Store.Get(id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, c)

	case http.MethodPost:
		var m Msg
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil || m.Role == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected {role, content}"})
			return
		}
		if err := a.Store.Append(id, m); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case http.MethodDelete:
		if err := a.Store.Delete(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) settings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		m, _ := a.Store.Settings()
		writeJSON(w, http.StatusOK, m)

	case http.MethodPut:
		m := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected an object"})
			return
		}
		if err := a.Store.SaveSettings(m); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) requireDocs(w http.ResponseWriter) bool {
	if a.Docs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "document storage unavailable on this drive",
		})
		return false
	}
	return true
}

func (a *API) docs(w http.ResponseWriter, r *http.Request) {
	if !a.requireDocs(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		metas, _, err := a.Docs.All()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out := []DocInfo{}
		for _, m := range metas {
			out = append(out, DocInfo{DocMeta: m, Chunks: a.Index.ChunksFor(m.ID)})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Added > out[j].Added })
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		// The body is the file itself, raw. Base64 in JSON would inflate every
		// upload by a third for no gain, and the browser can stream a File
		// straight into fetch().
		name := r.URL.Query().Get("name")
		if strings.TrimSpace(name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing ?name="})
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUploadBytes))
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": fmt.Sprintf("file is larger than %dMB", maxUploadBytes>>20), "name": name,
			})
			return
		}

		// One file can become several documents: an archive yields one per
		// member. Partial success is the useful outcome — a zip with two
		// readable files and one dud should add the two.
		parts, err := Extract(name, data)
		if err != nil {
			msg := err.Error()
			if errors.Is(err, ErrNotText) {
				msg = "this file is not text and its format is not supported"
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "name": name})
			return
		}

		added := []DocInfo{}
		var firstErr error
		for _, p := range parts {
			m, err := a.Docs.AddText(p.Name, p.Text)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			added = append(added, DocInfo{DocMeta: m})
		}
		if len(added) == 0 {
			msg := "no text could be read from it"
			if firstErr != nil {
				msg = firstErr.Error()
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "name": name})
			return
		}
		a.reindex()
		for i := range added {
			added[i].Chunks = a.Index.ChunksFor(added[i].ID)
		}
		writeJSON(w, http.StatusOK, added)

	case http.MethodDelete:
		n, err := a.Docs.Wipe()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		a.Vectors.ForgetAll()
		a.reindex()
		writeJSON(w, http.StatusOK, map[string]int{"deleted": n})

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) doc(w http.ResponseWriter, r *http.Request, id string) {
	if !a.requireDocs(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		m, text, err := a.Docs.Get(id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"meta": m, "content": text})

	case http.MethodDelete:
		if err := a.Docs.Delete(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		// A vector is a lossy but real representation of the passage it came
		// from, so deleting the document has to delete its cache too, or part
		// of it stays on the drive after the user believes it is gone.
		a.Vectors.Forget(id)
		a.reindex()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	if !a.requireDocs(w) {
		return
	}
	q := r.URL.Query().Get("q")
	k := 4
	if v := r.URL.Query().Get("k"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 20 {
			k = n
		}
	}
	// The one synchronous embedding call in the program, on a short deadline.
	// It returns nil for every failure — no model, server down, too slow — and
	// a nil query vector is not an error condition, it is BM25.
	qvec := a.Vectors.EmbedQuery(r.Context(), q)
	writeJSON(w, http.StatusOK, a.Index.Search(q, k, qvec))
}

// DELETE /api/chats/{id}/last — drop the final message. Used by regenerate,
// which replaces an answer rather than appending a second one.
func (a *API) chatLast(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := a.Store.DropLast(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
