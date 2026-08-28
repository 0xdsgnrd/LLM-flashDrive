package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

type API struct{ Store *Store }

// Everything the browser can persist goes through here. It is deliberately the
// only writer: llama-server serves files read-only, so if pocketd is not
// running the UI can tell the difference and say so, rather than quietly
// falling back to browser storage and leaving a trail on the host machine.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api")

	if path == "/health" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "history": a.Store != nil,
		})
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
