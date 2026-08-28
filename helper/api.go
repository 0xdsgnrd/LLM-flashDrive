package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type API struct {
	Store *Store
	Docs  *DocStore
	Index *Index
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
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "history": a.Store != nil, "search": a.Docs != nil,
			"docs": docs, "chunks": chunks,
		})
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
		var body struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected {name, content}"})
			return
		}
		m, err := a.Docs.Add(body.Name, body.Content)
		if err != nil {
			// A rejected file is the user's mistake to correct, not a server
			// fault — say which file and why, so the UI can name it.
			code := http.StatusBadRequest
			msg := err.Error()
			if errors.Is(err, ErrNotText) {
				msg = "only text files can be indexed (PDF and Word are not supported yet)"
			}
			writeJSON(w, code, map[string]string{"error": msg, "name": body.Name})
			return
		}
		a.reindex()
		writeJSON(w, http.StatusOK, DocInfo{DocMeta: m, Chunks: a.Index.ChunksFor(m.ID)})

	case http.MethodDelete:
		n, err := a.Docs.Wipe()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
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
	writeJSON(w, http.StatusOK, a.Index.Search(q, k))
}
