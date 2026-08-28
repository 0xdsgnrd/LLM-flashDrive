package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// A document is one file in docs/: a JSON header line, then the raw text. Same
// shape as a conversation, for the same reason — the drive should describe
// itself without a sidecar database that can drift from what is actually there.
//
// There is deliberately NO persisted search index. The index is rebuilt in
// memory from these files at startup and after every change. A corpus of a few
// megabytes rebuilds in well under a second, and in exchange there is no index
// format to version, no half-written index to detect, and nothing that can
// disagree with the documents themselves. Delete the index and it comes back.
type DocMeta struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
	Added string `json:"added"`
}

type DocInfo struct {
	DocMeta
	Chunks int `json:"chunks"`
}

// Text only, and checked rather than trusted: a PDF or an image dropped in here
// would index as line noise and quietly poison every search.
var ErrNotText = errors.New("not a text document")

const maxDocBytes = 16 << 20 // 16MB — a book of plain text is ~1MB

type DocStore struct {
	dir string
	mu  sync.Mutex
}

func NewDocStore(dir string) (*DocStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	probe := filepath.Join(dir, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return nil, fmt.Errorf("not writable: %w", err)
	}
	os.Remove(probe)
	return &DocStore{dir: dir}, nil
}

func (d *DocStore) path(id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", errors.New("bad document id")
	}
	return filepath.Join(d.dir, id+".doc"), nil
}

// looksBinary rejects anything with a NUL byte or a high proportion of control
// characters. It is a cheap check that catches the realistic mistake — dragging
// in a PDF, a .docx, an image — without pretending to sniff file types.
func looksBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	ctrl := 0
	for _, c := range b[:n] {
		if c == 0 {
			return true
		}
		if c < 9 || (c > 13 && c < 32) {
			ctrl++
		}
	}
	return n > 0 && ctrl*100/n > 5
}

func (d *DocStore) Add(name, content string) (DocMeta, error) {
	if len(content) > maxDocBytes {
		return DocMeta{}, fmt.Errorf("document is larger than %dMB", maxDocBytes>>20)
	}
	if strings.TrimSpace(content) == "" {
		return DocMeta{}, errors.New("document is empty")
	}
	if looksBinary([]byte(content)) {
		return DocMeta{}, ErrNotText
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	name = strings.TrimSpace(filepath.Base(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "untitled.txt"
	}
	m := DocMeta{ID: newID(), Name: name, Bytes: len(content), Added: nowRFC3339()}
	p, err := d.path(m.ID)
	if err != nil {
		return DocMeta{}, err
	}
	head, _ := json.Marshal(m)
	body := append(head, '\n')
	body = append(body, content...)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		return DocMeta{}, err
	}
	return m, nil
}

func (d *DocStore) read(p string) (DocMeta, string, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return DocMeta{}, "", err
	}
	i := bytes.IndexByte(raw, '\n')
	if i < 0 {
		return DocMeta{}, "", errors.New("document has no header")
	}
	var m DocMeta
	if err := json.Unmarshal(raw[:i], &m); err != nil {
		return DocMeta{}, "", err
	}
	return m, string(raw[i+1:]), nil
}

func (d *DocStore) Get(id string) (DocMeta, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, err := d.path(id)
	if err != nil {
		return DocMeta{}, "", err
	}
	return d.read(p)
}

// All returns every document with its text, for an index rebuild.
func (d *DocStore) All() ([]DocMeta, []string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return nil, nil, err
	}
	var metas []DocMeta
	var texts []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".doc") {
			continue
		}
		if !idRe.MatchString(strings.TrimSuffix(e.Name(), ".doc")) {
			continue
		}
		m, text, err := d.read(filepath.Join(d.dir, e.Name()))
		if err != nil {
			continue // one unreadable document must not break the whole corpus
		}
		metas = append(metas, m)
		texts = append(texts, text)
	}
	return metas, texts, nil
}

func (d *DocStore) Delete(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, err := d.path(id)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

func (d *DocStore) Wipe() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".doc") {
			continue
		}
		if err := os.Remove(filepath.Join(d.dir, e.Name())); err == nil {
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- chunking

// Chunks are paragraph-aligned rather than cut every N characters: a retrieved
// fragment that starts mid-sentence reads as noise to the model and to the
// person checking the citation. Paragraphs longer than the target are split on
// their own, and consecutive chunks overlap so a fact spanning a boundary is
// still findable from either side.
const (
	chunkTarget  = 1000
	chunkOverlap = 150
)

func chunk(text string) []string {
	var out []string
	var cur strings.Builder

	flush := func() {
		s := strings.TrimSpace(cur.String())
		if s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}

	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var para strings.Builder
	paras := []string{}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			if para.Len() > 0 {
				paras = append(paras, para.String())
				para.Reset()
			}
			continue
		}
		if para.Len() > 0 {
			para.WriteByte('\n')
		}
		para.WriteString(line)
	}
	if para.Len() > 0 {
		paras = append(paras, para.String())
	}

	for _, p := range paras {
		// A single paragraph bigger than the target gets hard-split; nothing
		// else can be done without losing it entirely.
		for len(p) > chunkTarget*2 {
			flush()
			out = append(out, strings.TrimSpace(p[:chunkTarget]))
			cut := chunkTarget - chunkOverlap
			if cut < 1 {
				cut = chunkTarget
			}
			p = p[cut:]
		}
		if cur.Len()+len(p) > chunkTarget && cur.Len() > 0 {
			prev := cur.String()
			flush()
			// Carry the tail of the previous chunk into the next one.
			if len(prev) > chunkOverlap {
				cur.WriteString(prev[len(prev)-chunkOverlap:])
				cur.WriteString("\n")
			}
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(p)
	}
	flush()
	return out
}
