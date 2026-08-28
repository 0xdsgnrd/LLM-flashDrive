package main

import (
	"strings"
	"testing"
)

func newDocs(t *testing.T) *DocStore {
	t.Helper()
	d, err := NewDocStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewDocStore: %v", err)
	}
	return d
}

func buildIndex(t *testing.T, d *DocStore) *Index {
	t.Helper()
	metas, texts, err := d.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	ix := NewIndex()
	ix.Build(metas, texts)
	return ix
}

func TestSearchRanksTheRelevantDocumentFirst(t *testing.T) {
	d := newDocs(t)
	d.Add("ejecting.md", "To remove the drive safely, always eject it first.\n\nexFAT has no journal, so an interrupted write can corrupt a file.")
	d.Add("baking.md", "Preheat the oven to 200 degrees. Whisk the eggs and the sugar together until pale.")
	d.Add("models.md", "Quantisation shrinks a model by storing weights at lower precision, trading a little quality for a lot of memory.")

	ix := buildIndex(t, d)
	hits := ix.Search("why should I eject the drive", 3)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].DocName != "ejecting.md" {
		t.Errorf("top hit = %s, want ejecting.md (scores: %+v)", hits[0].DocName, hits)
	}

	hits = ix.Search("quantisation memory tradeoff", 3)
	if len(hits) == 0 || hits[0].DocName != "models.md" {
		t.Errorf("quantisation query returned %+v", hits)
	}
}

func TestSearchReturnsNothingForUnrelatedQuery(t *testing.T) {
	d := newDocs(t)
	d.Add("a.md", "the drive must be ejected before removal")
	ix := buildIndex(t, d)
	if hits := ix.Search("xylophone marsupial", 3); len(hits) != 0 {
		t.Errorf("expected no hits, got %+v", hits)
	}
	if hits := ix.Search("", 3); len(hits) != 0 {
		t.Errorf("empty query should return nothing, got %d", len(hits))
	}
}

// One big document must not fill every retrieval slot and hide the others.
func TestSearchCapsHitsPerDocument(t *testing.T) {
	d := newDocs(t)
	var big strings.Builder
	for i := 0; i < 12; i++ {
		big.WriteString("The drive should be ejected cleanly every single time you finish. ")
		big.WriteString(strings.Repeat("filler sentence about ejecting drives. ", 25))
		big.WriteString("\n\n")
	}
	d.Add("long.md", big.String())
	d.Add("short.md", "Ejecting the drive flushes pending writes.")

	ix := buildIndex(t, d)
	hits := ix.Search("ejecting the drive", 6)
	perDoc := map[string]int{}
	for _, h := range hits {
		perDoc[h.DocName]++
	}
	if perDoc["long.md"] > maxHitsPerDoc {
		t.Errorf("long.md took %d slots, cap is %d", perDoc["long.md"], maxHitsPerDoc)
	}
	if perDoc["short.md"] == 0 {
		t.Error("short.md was crowded out entirely")
	}
}

func TestSearchIsDeterministic(t *testing.T) {
	d := newDocs(t)
	for _, s := range []string{"drive drive", "drive drive", "drive drive"} {
		d.Add("same.md", s)
	}
	ix := buildIndex(t, d)
	first := ix.Search("drive", 3)
	for i := 0; i < 25; i++ {
		again := ix.Search("drive", 3)
		for j := range first {
			if again[j].DocID != first[j].DocID || again[j].Chunk != first[j].Chunk {
				t.Fatalf("ranking changed between identical searches at %d", j)
			}
		}
	}
}

func TestTokenizeDropsStopwordsAndCase(t *testing.T) {
	got := tokenize("The Drive and THE files")
	want := map[string]bool{"drive": true, "files": true}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected token %q in %v", g, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("tokens = %v, want exactly drive and files", got)
	}
}

func TestChunkKeepsParagraphsWholeAndOverlaps(t *testing.T) {
	p1 := strings.Repeat("alpha ", 100) // ~600 chars
	p2 := strings.Repeat("bravo ", 100)
	cs := chunk(p1 + "\n\n" + p2)
	if len(cs) < 2 {
		t.Fatalf("expected a split, got %d chunk(s)", len(cs))
	}
	if !strings.Contains(cs[0], "alpha") {
		t.Error("first chunk lost its paragraph")
	}
	joined := strings.Join(cs, " ")
	if !strings.Contains(joined, "bravo") {
		t.Error("second paragraph missing entirely")
	}
	// The tail of chunk 1 should reappear at the head of chunk 2.
	if !strings.HasPrefix(strings.TrimSpace(cs[1]), "alpha") {
		t.Errorf("no overlap carried into the next chunk: %.60q", cs[1])
	}
}

func TestChunkSplitsAnOversizedParagraph(t *testing.T) {
	cs := chunk(strings.Repeat("x", 9000))
	if len(cs) < 3 {
		t.Fatalf("a 9000-char paragraph produced only %d chunk(s)", len(cs))
	}
	for i, c := range cs {
		if len(c) > chunkTarget*2 {
			t.Errorf("chunk %d is %d chars, too large", i, len(c))
		}
	}
}

func TestChunkOfEmptyText(t *testing.T) {
	if cs := chunk("   \n\n  \n"); len(cs) != 0 {
		t.Errorf("blank text produced %d chunk(s)", len(cs))
	}
}
