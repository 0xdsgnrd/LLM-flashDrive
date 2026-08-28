package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	s := newTestStore(t)
	meta, err := s.Create("Qwen3-4B")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Append(meta.ID, Msg{Role: "user", Content: "how do I eject a drive?"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(meta.ID, Msg{Role: "assistant", Content: "diskutil eject"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	c, err := s.Get(meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(c.Messages))
	}
	if c.Model != "Qwen3-4B" {
		t.Errorf("model not preserved: %q", c.Model)
	}
	if c.Title != "how do I eject a drive?" {
		t.Errorf("title = %q", c.Title)
	}
}

// The whole reason for append-only JSONL: a drive pulled mid-write leaves a
// final line with no newline. That message is lost, but the conversation is not.
func TestTornFinalLineIsDropped(t *testing.T) {
	s := newTestStore(t)
	meta, _ := s.Create("m")
	s.Append(meta.ID, Msg{Role: "user", Content: "intact"})

	p := filepath.Join(s.dir, meta.ID+".jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"role":"assistant","content":"half a th`) // no newline
	f.Close()

	c, err := s.Get(meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Messages) != 1 || c.Messages[0].Content != "intact" {
		t.Fatalf("torn line not dropped cleanly: %+v", c.Messages)
	}
}

func TestGarbageLineSkippedNotFatal(t *testing.T) {
	s := newTestStore(t)
	meta, _ := s.Create("m")
	s.Append(meta.ID, Msg{Role: "user", Content: "first"})

	p := filepath.Join(s.dir, meta.ID+".jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("this is not json at all\n")
	f.Close()
	s.Append(meta.ID, Msg{Role: "assistant", Content: "second"})

	c, _ := s.Get(meta.ID)
	if len(c.Messages) != 2 {
		t.Fatalf("want 2 surviving messages, got %d: %+v", len(c.Messages), c.Messages)
	}
}

// Ids are ours and always match idRe, so this is the only path-traversal guard
// /api/chats/{id} needs. It has to actually hold.
func TestIDValidationBlocksTraversal(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []string{
		"../../../etc/passwd", "..", "", "settings", "20260828-140312-ab",
		"20260828-140312-ABCD", "20260828-140312-a1b2/../x", "a/b",
	} {
		if _, err := s.path(bad); err == nil {
			t.Errorf("accepted bad id %q", bad)
		}
		if _, err := s.Get(bad); err == nil {
			t.Errorf("Get accepted bad id %q", bad)
		}
	}
}

func TestListSortsNewestFirstAndCounts(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create("m")
	s.Append(a.ID, Msg{Role: "user", Content: "older"})
	b, _ := s.Create("m")
	s.Append(b.ID, Msg{Role: "user", Content: "newer"})
	// Same-second ids would tie on mtime; force b to be strictly newer.
	later := time.Now().Add(time.Minute)
	os.Chtimes(filepath.Join(s.dir, b.ID+".jsonl"), later, later)

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}
	if list[0].ID != b.ID {
		t.Errorf("newest not first: %s then %s", list[0].ID, list[1].ID)
	}
	if list[0].Count != 1 {
		t.Errorf("count = %d, want 1", list[0].Count)
	}
}

func TestWipeRemovesChatsButKeepsSettings(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create("m")
	s.Append(a.ID, Msg{Role: "user", Content: "secret"})
	s.Create("m")
	if err := s.SaveSettings(map[string]any{"model": "Qwen3-4B"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	n, err := s.Wipe()
	if err != nil || n != 2 {
		t.Fatalf("Wipe: n=%d err=%v", n, err)
	}
	list, _ := s.List()
	if len(list) != 0 {
		t.Errorf("conversations survived wipe: %+v", list)
	}
	got, _ := s.Settings()
	if got["model"] != "Qwen3-4B" {
		t.Errorf("settings lost in wipe: %+v", got)
	}
	// Nothing readable should be left behind for the next person.
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			t.Errorf("leftover transcript %s", e.Name())
		}
	}
}

func TestLongAnswerSurvivesScannerBuffer(t *testing.T) {
	s := newTestStore(t)
	meta, _ := s.Create("m")
	big := strings.Repeat("token ", 200000) // ~1.2MB on one line
	if err := s.Append(meta.ID, Msg{Role: "assistant", Content: big}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	c, err := s.Get(meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(c.Messages) != 1 || len(c.Messages[0].Content) != len(big) {
		t.Fatalf("long message truncated or dropped")
	}
}

// Two conversations written inside the same second must still come back in a
// stable, newest-first order — startup opens list[0] and it cannot be a coin flip.
func TestListOrderIsDeterministicWithinOneSecond(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.Create("m")
	b, _ := s.Create("m")
	same := time.Now()
	for _, id := range []string{a.ID, b.ID} {
		os.Chtimes(filepath.Join(s.dir, id+".jsonl"), same, same)
	}
	first, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, _ := s.List()
		if again[0].ID != first[0].ID {
			t.Fatalf("order flipped between calls: %s then %s", first[0].ID, again[0].ID)
		}
	}
	if first[0].ID < first[1].ID {
		t.Errorf("tie not broken newest-first: %s before %s", first[0].ID, first[1].ID)
	}
}
