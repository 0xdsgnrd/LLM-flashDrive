package main

import (
	"os"
	"strings"
	"testing"
)

func TestDocRoundTrip(t *testing.T) {
	d := newDocs(t)
	m, err := d.AddText("notes.md", "# Notes\n\nThe drive is exFAT.")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, text, err := d.Get(m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "notes.md" || !strings.Contains(text, "exFAT") {
		t.Errorf("round trip lost data: %+v %q", got, text)
	}
}

// Format handling lives in Extract (see extract_test.go); by the time text
// reaches the store the only questions left are "is there any" and "is it sane".
func TestAddTextRejectsEmptyAndOversized(t *testing.T) {
	d := newDocs(t)
	if _, err := d.AddText("empty.txt", "   \n "); err == nil {
		t.Error("empty document accepted")
	}
	if _, err := d.AddText("huge.txt", strings.Repeat("a", maxDocBytes+1)); err == nil {
		t.Error("oversized document accepted")
	}
}

func TestRealTextWithAccentsIsAccepted(t *testing.T) {
	d := newDocs(t)
	if _, err := d.AddText("café.md", "Café notes — em dashes, ümlauts, 日本語, emoji 🎧.\ttabs too."); err != nil {
		t.Errorf("legitimate unicode text rejected: %v", err)
	}
}

func TestDocNameCannotEscapeTheDirectory(t *testing.T) {
	d := newDocs(t)
	m, err := d.AddText("../../../etc/passwd", "root:x:0:0")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if strings.ContainsAny(m.Name, "/\\") {
		t.Errorf("path survived in stored name: %q", m.Name)
	}
	if _, err := d.path(m.ID); err != nil {
		t.Errorf("stored id is not usable: %v", err)
	}
}

func TestDeleteAndWipe(t *testing.T) {
	d := newDocs(t)
	a, _ := d.AddText("a.md", "alpha content here")
	d.AddText("b.md", "bravo content here")

	if err := d.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	metas, _, _ := d.All()
	if len(metas) != 1 {
		t.Fatalf("after delete, %d docs remain", len(metas))
	}
	n, err := d.Wipe()
	if err != nil || n != 1 {
		t.Fatalf("Wipe: n=%d err=%v", n, err)
	}
	metas, _, _ = d.All()
	if len(metas) != 0 {
		t.Errorf("wipe left %d docs", len(metas))
	}
}

// Deleting a document must remove it from search too, not just from the list.
func TestDeletedDocumentLeavesTheIndex(t *testing.T) {
	d := newDocs(t)
	m, _ := d.AddText("secret.md", "the passphrase is hunter2 pomegranate")
	ix := buildIndex(t, d)
	if len(ix.Search("pomegranate", 3, nil)) == 0 {
		t.Fatal("document not searchable after add")
	}
	d.Delete(m.ID)
	ix = buildIndex(t, d)
	if hits := ix.Search("pomegranate", 3, nil); len(hits) != 0 {
		t.Errorf("deleted document still searchable: %+v", hits)
	}
}

func TestUnreadableDocumentDoesNotBreakTheCorpus(t *testing.T) {
	d := newDocs(t)
	d.AddText("good.md", "searchable content about drives")
	// A file with no header line — a truncated or hand-edited document.
	if err := writeRaw(d.dir+"/20260101-000000-zzzz.doc", "no header at all"); err != nil {
		t.Fatal(err)
	}
	metas, texts, err := d.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(metas) != 1 || len(texts) != 1 {
		t.Errorf("bad file broke enumeration: %d metas", len(metas))
	}
}

func writeRaw(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
