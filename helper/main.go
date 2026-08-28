// pocketd — the drive's front door.
//
// llama-server already serves the UI and the model API from one origin, which
// is why this project has never needed CORS. The one thing it cannot do is
// write a file, and chat history has to land on the drive rather than in the
// browser: plugging into a borrowed laptop must leave nothing behind.
//
// So pocketd takes over the front door and keeps the single-origin property
// intact. It serves ui/, forwards the model API to llama-server on a private
// port, and owns /api/* itself. scripts/devserver.mjs has done exactly this in
// development all along — this is that file, compiled and portable.
//
// Stdlib only. It ships next to the model binaries on an exFAT stick and has
// to start on a machine with nothing installed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Paths owned by llama-server. Everything else is either /api/* (ours) or a
// static file from ui/. Kept as an explicit list, mirroring devserver.mjs, so
// that adding an endpoint is a deliberate act rather than a surprise.
var upstreamPrefixes = []string{
	"/v1/", "/health", "/props", "/slots", "/completion", "/completions",
	"/tokenize", "/detokenize", "/embedding", "/embeddings", "/infill", "/metrics",
}

// Stamped at build time so drive.lock can pin the helper the way it pins the
// llama.cpp tag, and verify-drive.sh can tell you what is actually staged.
var version = "dev"

func main() {
	var (
		port     = flag.Int("port", 8080, "port to listen on")
		uiDir    = flag.String("ui", "ui", "directory of static UI files")
		chatsDir = flag.String("chats", "chats", "directory for saved conversations")
		docsDir  = flag.String("docs", "docs", "directory for indexed documents")
		upstream = flag.String("upstream", "127.0.0.1:8081", "llama-server host:port")
		showVer  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return
	}

	absUI, err := filepath.Abs(*uiDir)
	if err != nil {
		log.Fatalf("pocketd: ui dir: %v", err)
	}
	store, err := NewStore(*chatsDir)
	if err != nil {
		// A read-only or full drive must not take the whole app down: chat
		// saving degrades, inference still works. The UI asks /api/health and
		// tells the user plainly rather than silently dropping their history.
		log.Printf("pocketd: chat storage unavailable (%v) — history disabled", err)
		store = nil
	}

	docs, err := NewDocStore(*docsDir)
	if err != nil {
		log.Printf("pocketd: document storage unavailable (%v) — search disabled", err)
		docs = nil
	}
	index := NewIndex()
	if docs != nil {
		// Built at startup from the documents themselves. There is no index
		// file to load, so there is nothing to be stale or half-written.
		if metas, texts, err := docs.All(); err == nil {
			index.Build(metas, texts)
			if d, c := index.Stats(); d > 0 {
				log.Printf("pocketd: indexed %d document(s), %d chunk(s)", d, c)
			}
		}
	}

	target, err := url.Parse("http://" + *upstream)
	if err != nil {
		log.Fatalf("pocketd: bad -upstream %q: %v", *upstream, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Token streaming is the entire feel of the app. Without an immediate
	// flush the SSE body buffers and the UI looks frozen until generation
	// finishes, which reads as a hang rather than as latency.
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("llama-server at %s unreachable: %v", *upstream, err),
		})
	}

	api := &API{Store: store, Docs: docs, Index: index}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			api.ServeHTTP(w, r)
		case matchesUpstream(r.URL.Path):
			proxy.ServeHTTP(w, r)
		default:
			serveStatic(w, r, absUI)
		}
	})

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("pocketd %s: ui %s · chats %s · docs %s · upstream %s · listening on %s",
		version, absUI, *chatsDir, *docsDir, *upstream, addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("pocketd: %v", err)
	}
}

func matchesUpstream(path string) bool {
	for _, p := range upstreamPrefixes {
		if path == strings.TrimSuffix(p, "/") || strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// serveStatic reads one file out of ui/. Path traversal is blocked by cleaning
// the request path and then confirming the result is still inside the root —
// checking the string before resolution is not enough.
func serveStatic(w http.ResponseWriter, r *http.Request, root string) {
	rel := r.URL.Path
	if rel == "/" {
		rel = "/index.html"
	}
	full := filepath.Join(root, filepath.FromSlash(filepath.Clean("/"+strings.TrimPrefix(rel, "/"))))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body, err := os.ReadFile(full)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ct := mime.TypeByExtension(filepath.Ext(full))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	// The UI is edited in place on the drive; a cached copy would hide edits.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
