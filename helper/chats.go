package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// One conversation is one append-only .jsonl file: a header line, then one
// line per message.
//
// Append-only is not a style choice. exFAT has no journal and this drive gets
// unplugged by people, not by software. Rewriting the whole transcript on every
// turn puts the entire conversation at risk on each message; appending a single
// line risks only that line, and a torn final line is detectable and skipped.
type header struct {
	V       int    `json:"v"`
	ID      string `json:"id"`
	Model   string `json:"model,omitempty"`
	Created string `json:"created"`
}

type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	TS      string `json:"ts,omitempty"`
	// Documents the answer was grounded in, when retrieval was used. omitempty
	// keeps every transcript written before this existed readable unchanged.
	Sources []string `json:"sources,omitempty"`
}

type Meta struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Model   string `json:"model,omitempty"`
	Created string `json:"created"`
	Updated string `json:"updated"`
	Count   int    `json:"count"`
}

type Chat struct {
	Meta
	Messages []Msg `json:"messages"`
}

// Ids are generated here and only ever come back from us, so a strict shape is
// free. It is also the whole path-traversal defence for /api/chats/{id}.
var idRe = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[a-z0-9]{4}$`)

type Store struct {
	dir string
	mu  sync.Mutex
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// MkdirAll succeeds on an existing directory even when the volume is
	// read-only, so prove writability rather than assuming it. Better to know
	// at startup than to lose someone's first conversation.
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return nil, fmt.Errorf("not writable: %w", err)
	}
	os.Remove(probe)
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", errors.New("bad conversation id")
	}
	return filepath.Join(s.dir, id+".jsonl"), nil
}

func newID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	for i := range b {
		b[i] = alphabet[rand.Intn(len(alphabet))]
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + string(b)
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func (s *Store) Create(model string) (Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := header{V: 1, ID: newID(), Model: model, Created: nowRFC3339()}
	p, err := s.path(h.ID)
	if err != nil {
		return Meta{}, err
	}
	line, _ := json.Marshal(h)
	if err := os.WriteFile(p, append(line, '\n'), 0o644); err != nil {
		return Meta{}, err
	}
	return Meta{ID: h.ID, Title: "New conversation", Model: model,
		Created: h.Created, Updated: h.Created}, nil
}

func (s *Store) Append(id string, m Msg) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.path(id)
	if err != nil {
		return err
	}
	if m.TS == "" {
		m.TS = nowRFC3339()
	}
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	// Flush to the stick now. The window between "assistant finished" and "user
	// pulls the drive out" is exactly where an unflushed write disappears.
	return f.Sync()
}

// read parses one file. A final line without a trailing newline is a write that
// did not complete — the drive was pulled, or the machine died mid-append — so
// it is dropped rather than half-parsed. Malformed lines anywhere else are
// skipped for the same reason: one bad line must not cost the conversation.
func (s *Store) read(p string) (header, []Msg, time.Time, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return header{}, nil, time.Time{}, err
	}
	var mod time.Time
	if fi, err := os.Stat(p); err == nil {
		mod = fi.ModTime()
	}
	if n := len(raw); n > 0 && raw[n-1] != '\n' {
		if i := bytes.LastIndexByte(raw, '\n'); i >= 0 {
			raw = raw[:i+1]
		} else {
			raw = nil
		}
	}

	var h header
	var msgs []Msg
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // a long answer is one line
	first := true
	for sc.Scan() {
		b := sc.Bytes()
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		if first {
			first = false
			if err := json.Unmarshal(b, &h); err == nil && h.V > 0 {
				continue // it was a header; otherwise fall through and treat as a message
			}
		}
		var m Msg
		if err := json.Unmarshal(b, &m); err == nil && m.Role != "" {
			msgs = append(msgs, m)
		}
	}
	return h, msgs, mod, nil
}

// A conversation is named by what was asked, not by a box the user has to fill
// in. Derived on read so the header stays append-only.
func titleFrom(msgs []Msg) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		t := strings.Join(strings.Fields(m.Content), " ")
		if r := []rune(t); len(r) > 60 {
			t = strings.TrimSpace(string(r[:60])) + "…"
		}
		if t != "" {
			return t
		}
	}
	return "New conversation"
}

func (s *Store) Get(id string) (*Chat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	h, msgs, mod, err := s.read(p)
	if err != nil {
		return nil, err
	}
	return &Chat{
		Meta: Meta{ID: id, Title: titleFrom(msgs), Model: h.Model, Created: h.Created,
			Updated: mod.UTC().Format(time.RFC3339), Count: len(msgs)},
		Messages: msgs,
	}, nil
}

func (s *Store) List() ([]Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []Meta{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if !idRe.MatchString(id) {
			continue
		}
		h, msgs, mod, err := s.read(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, Meta{ID: id, Title: titleFrom(msgs), Model: h.Model,
			Created: h.Created, Updated: mod.UTC().Format(time.RFC3339), Count: len(msgs)})
	}
	// Newest first. Updated has one-second resolution, so two conversations
	// touched in the same second would otherwise come back in whatever order
	// sort.Slice happened to leave them — and "open the most recent" on startup
	// would pick arbitrarily. Ids begin with their creation timestamp, so they
	// break the tie deterministically.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated != out[j].Updated {
			return out[i].Updated > out[j].Updated
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// DropLast removes the final message from a transcript, which is what
// "regenerate" needs: the answer being replaced has to stop existing, or a
// reload would show both attempts stacked on top of each other.
//
// Truncating at the last newline is the whole operation — the append-only
// format means the last line IS the last message, and everything before it is
// untouched. The header line is never removed.
func (s *Store) DropLast(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.path(id)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	end := len(raw)
	if end > 0 && raw[end-1] == '\n' {
		end-- // ignore the terminator of the line being dropped
	}
	cut := bytes.LastIndexByte(raw[:end], '\n')
	header := bytes.IndexByte(raw, '\n')
	if cut < 0 || header < 0 || cut < header {
		return errors.New("no message to drop")
	}
	return os.WriteFile(p, raw[:cut+1], 0o644)
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.path(id)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// Wipe removes conversations only. settings.json survives on purpose: it holds
// a model preference, not anything about what was said.
func (s *Store) Wipe() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err == nil {
			n++
		}
	}
	return n, nil
}

// Settings are the last thing that would otherwise sit in localStorage. Keeping
// them here is what lets the UI drop browser storage entirely, which is the
// point: the laptop ends the session as clean as it started.
func (s *Store) Settings() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(filepath.Join(s.dir, "settings.json"))
	if err != nil {
		return map[string]any{}, nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}, nil
	}
	return m, nil
}

func (s *Store) SaveSettings(m map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "settings.json"), b, 0o644)
}
