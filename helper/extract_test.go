package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- fixtures

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644,
			Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// A real PDF, assembled here rather than checked in as a blob, so the xref
// offsets are correct and the parser is genuinely exercised.
func makePDF(text string) []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R " +
			"/Resources << /Font << /F1 5 0 R >> >> >>",
		"", // contents stream, filled below
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	stream := fmt.Sprintf("BT /F1 18 Tf 72 700 Td (%s) Tj ET", text)
	objs[3] = fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objs)+1, xref)
	return buf.Bytes()
}

func extractOne(t *testing.T, name string, data []byte) string {
	t.Helper()
	got, err := Extract(name, data)
	if err != nil {
		t.Fatalf("Extract(%s): %v", name, err)
	}
	if len(got) != 1 {
		t.Fatalf("Extract(%s) returned %d documents, want 1", name, len(got))
	}
	return got[0].Text
}

// ------------------------------------------------------------------ tests

func TestExtractPDF(t *testing.T) {
	text := extractOne(t, "notes.pdf", makePDF("the passphrase is purple pelican"))
	if !strings.Contains(text, "purple pelican") {
		t.Errorf("PDF text not recovered, got %q", text)
	}
}

func TestExtractPDFGivesAUsefulErrorForRubbish(t *testing.T) {
	// Must be an error, and must not panic — the library does on some inputs.
	if _, err := Extract("broken.pdf", []byte("%PDF-1.4 and then nonsense")); err == nil {
		t.Error("a corrupt PDF was accepted")
	}
}

func TestExtractDOCX(t *testing.T) {
	doc := `<?xml version="1.0"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>
<w:p><w:r><w:t>First paragraph about ejecting.</w:t></w:r></w:p>
<w:p><w:r><w:t>Second </w:t></w:r><w:r><w:t>paragraph split across runs.</w:t></w:r></w:p>
</w:body></w:document>`
	text := extractOne(t, "report.docx", zipOf(t, map[string]string{
		"word/document.xml":   doc,
		"[Content_Types].xml": `<Types/>`,
	}))
	if !strings.Contains(text, "First paragraph about ejecting.") {
		t.Errorf("missing first paragraph: %q", text)
	}
	// Word splits a sentence across runs whenever formatting or spellcheck
	// changes mid-line; joining them back is the whole job.
	if !strings.Contains(text, "Second paragraph split across runs.") {
		t.Errorf("runs not rejoined: %q", text)
	}
}

func TestExtractPPTXKeepsSlideOrder(t *testing.T) {
	slide := func(s string) string {
		return `<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">` +
			`<p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>` + s +
			`</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`
	}
	files := map[string]string{"[Content_Types].xml": "<Types/>"}
	for i, s := range []string{"alpha", "bravo", "charlie", "delta", "echo",
		"foxtrot", "golf", "hotel", "india", "juliet", "kilo"} {
		files[fmt.Sprintf("ppt/slides/slide%d.xml", i+1)] = slide(s)
	}
	text := extractOne(t, "deck.pptx", zipOf(t, files))
	// slide10 sorts before slide2 by name; a deck read out of order is worse
	// than useless when a citation points at "Slide 10".
	iKilo, iBravo := strings.Index(text, "kilo"), strings.Index(text, "bravo")
	if iKilo < 0 || iBravo < 0 || iBravo > iKilo {
		t.Errorf("slides out of order (bravo at %d, kilo at %d):\n%s", iBravo, iKilo, text)
	}
}

func TestExtractXLSXResolvesSharedStrings(t *testing.T) {
	shared := `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="2">
<si><t>Region</t></si><si><t>Pacific Northwest</t></si></sst>`
	sheet := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1"><v>2026</v></c></row>
<row r="2"><c r="A2" t="s"><v>1</v></c><c r="B2"><v>41</v></c></row>
</sheetData></worksheet>`
	text := extractOne(t, "book.xlsx", zipOf(t, map[string]string{
		"xl/sharedStrings.xml":     shared,
		"xl/worksheets/sheet1.xml": sheet,
		"[Content_Types].xml":      "<Types/>",
	}))
	// Without sharedStrings a sheet reads as a grid of meaningless integers.
	if !strings.Contains(text, "Pacific Northwest") || !strings.Contains(text, "Region") {
		t.Errorf("shared strings not resolved: %q", text)
	}
	if !strings.Contains(text, "2026") {
		t.Errorf("numeric cell lost: %q", text)
	}
}

func TestExtractHTMLDropsScriptsAndDecodesEntities(t *testing.T) {
	text := extractOne(t, "page.html", []byte(`<html><head>
<style>body{color:#fff}</style>
<script>var secret = "do not index me";</script></head>
<body><h1>Title</h1><p>Caf&eacute; &amp; cr&egrave;me</p><p>Second para</p></body></html>`))
	if strings.Contains(text, "do not index me") || strings.Contains(text, "color") {
		t.Errorf("script or style leaked into the index: %q", text)
	}
	if !strings.Contains(text, "Café & crème") {
		t.Errorf("entities not decoded: %q", text)
	}
	if !strings.Contains(text, "Title") || !strings.Contains(text, "Second para") {
		t.Errorf("body text lost: %q", text)
	}
}

func TestExtractXML(t *testing.T) {
	text := extractOne(t, "feed.xml", []byte(
		`<rss><channel><title>Drive notes</title><item><desc>eject cleanly</desc></item></channel></rss>`))
	if !strings.Contains(text, "Drive notes") || !strings.Contains(text, "eject cleanly") {
		t.Errorf("xml text lost: %q", text)
	}
	if strings.Contains(text, "<channel>") {
		t.Errorf("tags survived: %q", text)
	}
}

func TestExtractRTF(t *testing.T) {
	rtf := `{\rtf1\ansi\deff0{\fonttbl{\f0\froman Times;}{\f1\fswiss Arial;}}` +
		`{\colortbl;\red0\green0\blue0;}` +
		`{\info{\author Somebody}}` +
		`\f0\fs24 Hello \b world\b0 .\par Second line with a tab\tab here.\par` +
		`Unicode: \u233 e and a byte: \'e9\par}`
	text := extractOne(t, "note.rtf", []byte(rtf))
	if !strings.Contains(text, "Hello world.") {
		t.Errorf("body text lost: %q", text)
	}
	// Font and colour tables are markup; indexing them adds junk tokens.
	for _, junk := range []string{"Times", "Arial", "froman", "colortbl", "Somebody"} {
		if strings.Contains(text, junk) {
			t.Errorf("control data %q leaked into text: %q", junk, text)
		}
	}
	if !strings.Contains(text, "Second line") {
		t.Errorf("paragraph break lost: %q", text)
	}
	if !strings.Contains(text, "é") {
		t.Errorf("unicode escape not decoded: %q", text)
	}
}

func TestExtractZipYieldsOneDocumentPerMember(t *testing.T) {
	got, err := Extract("bundle.zip", zipOf(t, map[string]string{
		"notes/a.md":      "alpha content about drives",
		"notes/b.txt":     "bravo content about ejecting",
		"__MACOSX/._a.md": "junk",
		".DS_Store":       "junk",
		"images/logo.bin": "\x00\x01\x02\x03\x00\x00binary",
	}))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d documents, want 2: %+v", len(got), names(got))
	}
	// The citation has to say which file inside the archive it came from.
	for _, g := range got {
		if !strings.HasPrefix(g.Name, "bundle.zip → notes/") {
			t.Errorf("member not attributed to its archive: %q", g.Name)
		}
	}
}

func TestExtractTarGz(t *testing.T) {
	got, err := Extract("bundle.tar.gz", tarGzOf(t, map[string]string{
		"docs/one.md": "first document text",
		"docs/two.md": "second document text",
	}))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d documents, want 2: %v", len(got), names(got))
	}
}

// An archive inside an archive is where a decompression bomb hides.
func TestNestedArchivesAreNotUnpacked(t *testing.T) {
	inner := zipOf(t, map[string]string{"deep.md": "buried text"})
	got, err := Extract("outer.zip", zipOf(t, map[string]string{
		"inner.zip": string(inner),
		"plain.md":  "surface text",
	}))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Text, "surface text") {
		t.Errorf("nested archive was unpacked or surface file lost: %v", names(got))
	}
}

func TestRefusedFormatsSayWhichFormat(t *testing.T) {
	for ext, want := range map[string]string{
		".ppt": "pptx", ".doc": "docx", ".xls": "xlsx",
		".rar": "extract it first", ".7z": "extract it first",
	} {
		_, err := Extract("thing"+ext, []byte("whatever"))
		if err == nil {
			t.Errorf("%s was accepted", ext)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s error does not say what to do: %q", ext, err)
		}
	}
}

func TestUnknownExtensionFallsBackToText(t *testing.T) {
	if text := extractOne(t, "notes.conf", []byte("key = value\nother = thing")); !strings.Contains(text, "key = value") {
		t.Errorf("plain text with an odd extension was lost: %q", text)
	}
	if _, err := Extract("mystery.bin", []byte("\x00\x01\x02\x00\x00\x03binary junk")); err == nil {
		t.Error("binary with an unknown extension was accepted")
	}
}

func TestEmptyExtractionIsAnError(t *testing.T) {
	if _, err := Extract("blank.md", []byte("   \n\n  ")); err == nil {
		t.Error("a whitespace-only file was accepted")
	}
}

func names(e []Extracted) []string {
	out := make([]string, len(e))
	for i, x := range e {
		out[i] = x.Name
	}
	return out
}

// macOS textutil marks paragraphs with a backslash before a real line break
// rather than with \par. Missing it collapsed whole documents into a single
// run-on paragraph, which then defeated the paragraph-aligned chunker.
func TestExtractRTFBackslashLineBreaks(t *testing.T) {
	rtf := "{\\rtf1\\ansi\\ansicpg1252\\cocoartf2870\n" +
		"{\\fonttbl\\f0\\fswiss\\fcharset0 Helvetica;}\n" +
		"{\\colortbl;\\red255\\green255\\blue255;}\n" +
		"\\pard\\pardirnatural\\partightenfactor0\n\n" +
		"\\f0\\fs24 \\cf0 First paragraph.\\\n" +
		"\\\n" +
		"Second paragraph.\\\n}"
	text := extractOne(t, "textutil.rtf", []byte(rtf))
	if strings.Contains(text, "First paragraph.Second") {
		t.Errorf("paragraphs ran together: %q", text)
	}
	if !strings.Contains(text, "First paragraph.") || !strings.Contains(text, "Second paragraph.") {
		t.Errorf("text lost: %q", text)
	}
	if len(chunk(text)) == 0 {
		t.Error("no chunks produced")
	}
}
