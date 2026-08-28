package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/ledongthuc/pdf"
)

// Turning a file into searchable text.
//
// Everything here reduces a format to plain text and nothing else — no layout,
// no styling, no images. The index only ever sees words, so anything that is not
// a word is weight the drive does not need to carry.
//
// One file can yield several documents: an archive becomes one document per
// member, named "archive.zip → path/inside.md" so a citation still says where
// the text came from.
type Extracted struct {
	Name string
	Text string
}

const (
	maxArchiveMembers = 300
	maxExtractedTotal = 64 << 20 // guards against a decompression bomb
	maxArchiveDepth   = 1        // an archive inside an archive is not unpacked
)

// Formats that are refused by name rather than mangled. The legacy Office trio
// are OLE compound binaries with no usable Go extractor; rar and 7z would each
// mean shipping a whole decompressor. Saying which format it is, and what to do
// instead, is far more use than "unsupported file".
var refusedFormats = map[string]string{
	".ppt": "legacy PowerPoint (.ppt) — re-save it as .pptx and add that",
	".doc": "legacy Word (.doc) — re-save it as .docx and add that",
	".xls": "legacy Excel (.xls) — re-save it as .xlsx and add that",
	".rar": "RAR archives — extract it first, then add the files or a .zip",
	".7z":  "7-Zip archives — extract it first, then add the files or a .zip",
}

func Extract(name string, data []byte) ([]Extracted, error) {
	return extractAt(name, data, 0)
}

func extractAt(name string, data []byte, depth int) ([]Extracted, error) {
	lower := strings.ToLower(name)
	ext := filepath.Ext(lower)

	if why, bad := refusedFormats[ext]; bad {
		return nil, fmt.Errorf("%s", why)
	}

	one := func(text string, err error) ([]Extracted, error) {
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("no text could be read from it")
		}
		return []Extracted{{Name: name, Text: text}}, nil
	}

	switch {
	case strings.HasSuffix(lower, ".tar.gz") || ext == ".tgz" || ext == ".tar":
		return tarDocs(name, data, depth)
	case ext == ".zip":
		return zipDocs(name, data, depth)

	case ext == ".pdf":
		return one(pdfText(data))
	case ext == ".docx":
		return one(ooxmlText(data, "word/document.xml", "t", "p"))
	case ext == ".pptx":
		return one(pptxText(data))
	case ext == ".xlsx":
		return one(xlsxText(data))
	case ext == ".rtf":
		return one(stripRTF(data), nil)
	case ext == ".html" || ext == ".htm" || ext == ".xhtml":
		return one(stripHTML(data), nil)
	case ext == ".xml" || ext == ".svg":
		return one(xmlText(bytes.NewReader(data)))

	default:
		// Anything else is treated as plain text if it reads as text. That keeps
		// source files, notes and config working without an extension whitelist
		// that would need extending forever.
		if looksBinary(data) {
			return nil, ErrNotText
		}
		return one(string(data), nil)
	}
}

// ------------------------------------------------------------------ PDF

func pdfText(data []byte) (text string, err error) {
	// PDF parsers meet a lot of malformed files, and this one panics on some of
	// them. A bad PDF must cost the user an error message, not the server.
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("this PDF could not be read (it may be damaged or encrypted)")
		}
	}()

	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("this PDF could not be read (it may be encrypted): %w", err)
	}
	rc, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("no text could be read from this PDF: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(rc, maxDocBytes))
	if err != nil {
		return "", err
	}
	out := squeeze(string(b))
	if strings.TrimSpace(out) == "" {
		// The overwhelmingly common cause, and not obvious to a non-technical
		// person staring at a page full of visible words.
		return "", fmt.Errorf("this PDF has no text layer — it is probably a scan, which would need OCR")
	}
	return out, nil
}

// --------------------------------------------------------------- OOXML

func openZip(data []byte) (*zip.Reader, error) {
	return zip.NewReader(bytes.NewReader(data), int64(len(data)))
}

func zipEntry(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, maxDocBytes))
		}
	}
	return nil, fmt.Errorf("%s missing", name)
}

// ooxmlText walks one OOXML part and keeps the character data inside `want`
// elements, breaking a line whenever a `br` element closes. Element names are
// matched on their local part: the namespace prefix differs between Word (w:t)
// and PowerPoint (a:t) for what is the same idea.
func ooxmlText(data []byte, part, want, br string) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", fmt.Errorf("not a readable Office file: %w", err)
	}
	b, err := zipEntry(zr, part)
	if err != nil {
		return "", fmt.Errorf("not a readable Office file: %w", err)
	}
	return ooxmlPartText(b, want, br)
}

func ooxmlPartText(b []byte, want, br string) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	var out strings.Builder
	keep := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break // salvage whatever was read rather than losing the document
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case want:
				keep++
			case "tab":
				out.WriteByte('\t')
			case "br":
				out.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case want:
				if keep > 0 {
					keep--
				}
			case br:
				out.WriteByte('\n')
			}
		case xml.CharData:
			if keep > 0 {
				out.Write(t)
			}
		}
	}
	return squeeze(out.String()), nil
}

var slideNo = regexp.MustCompile(`slide(\d+)\.xml$`)

func pptxText(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", fmt.Errorf("not a readable PowerPoint file: %w", err)
	}
	type slide struct {
		n    int
		name string
	}
	var slides []slide
	for _, f := range zr.File {
		if m := slideNo.FindStringSubmatch(f.Name); m != nil &&
			strings.HasPrefix(f.Name, "ppt/slides/") {
			n, _ := strconv.Atoi(m[1])
			slides = append(slides, slide{n, f.Name})
		}
	}
	if len(slides) == 0 {
		return "", fmt.Errorf("no slides found in this file")
	}
	// slide2 sorts before slide10 by name; deck order is what a reader expects.
	sort.Slice(slides, func(i, j int) bool { return slides[i].n < slides[j].n })

	var out strings.Builder
	for _, s := range slides {
		b, err := zipEntry(zr, s.name)
		if err != nil {
			continue
		}
		text, _ := ooxmlPartText(b, "t", "p")
		if strings.TrimSpace(text) == "" {
			continue
		}
		fmt.Fprintf(&out, "Slide %d\n%s\n\n", s.n, text)
	}
	return out.String(), nil
}

var sheetNo = regexp.MustCompile(`sheet(\d+)\.xml$`)

func xlsxText(data []byte) (string, error) {
	zr, err := openZip(data)
	if err != nil {
		return "", fmt.Errorf("not a readable Excel file: %w", err)
	}
	// Most cell text lives once in sharedStrings and is referenced by index;
	// without it a sheet reads as a grid of integers.
	shared := []string{}
	if b, err := zipEntry(zr, "xl/sharedStrings.xml"); err == nil {
		dec := xml.NewDecoder(bytes.NewReader(b))
		dec.Strict = false
		var cur strings.Builder
		in := false
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "si" {
					cur.Reset()
				} else if t.Name.Local == "t" {
					in = true
				}
			case xml.EndElement:
				if t.Name.Local == "si" {
					shared = append(shared, cur.String())
				} else if t.Name.Local == "t" {
					in = false
				}
			case xml.CharData:
				if in {
					cur.Write(t)
				}
			}
		}
	}

	var sheets []string
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "xl/worksheets/") && sheetNo.MatchString(f.Name) {
			sheets = append(sheets, f.Name)
		}
	}
	sort.Slice(sheets, func(i, j int) bool {
		ni, _ := strconv.Atoi(sheetNo.FindStringSubmatch(sheets[i])[1])
		nj, _ := strconv.Atoi(sheetNo.FindStringSubmatch(sheets[j])[1])
		return ni < nj
	})
	if len(sheets) == 0 {
		return "", fmt.Errorf("no worksheets found in this file")
	}

	var out strings.Builder
	for si, name := range sheets {
		b, err := zipEntry(zr, name)
		if err != nil {
			continue
		}
		fmt.Fprintf(&out, "Sheet %d\n", si+1)
		out.WriteString(sheetText(b, shared))
		out.WriteString("\n")
	}
	return out.String(), nil
}

func sheetText(b []byte, shared []string) string {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	var out strings.Builder
	var row []string
	var cell strings.Builder
	cellType, inValue := "", false

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = row[:0]
			case "c":
				cell.Reset()
				cellType = ""
				for _, a := range t.Attr {
					if a.Name.Local == "t" {
						cellType = a.Value
					}
				}
			case "v", "t":
				inValue = true
			}
		case xml.CharData:
			if inValue {
				cell.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t":
				inValue = false
			case "c":
				v := cell.String()
				if cellType == "s" { // an index into sharedStrings, not a number
					if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil &&
						i >= 0 && i < len(shared) {
						v = shared[i]
					}
				}
				if strings.TrimSpace(v) != "" {
					row = append(row, v)
				}
			case "row":
				if len(row) > 0 {
					out.WriteString(strings.Join(row, "\t"))
					out.WriteByte('\n')
				}
			}
		}
	}
	return out.String()
}

// ------------------------------------------------------------ HTML / XML

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</\s*(script|style)\s*>`)
	reComment     = regexp.MustCompile(`(?s)<!--.*?-->`)
	reBlockClose  = regexp.MustCompile(`(?i)<\s*/?\s*(p|div|br|li|tr|h[1-6]|section|article|blockquote|pre|td|th)\b[^>]*>`)
	reAnyTag      = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces      = regexp.MustCompile(`[ \t\x{00a0}]+`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
)

func stripHTML(data []byte) string {
	s := string(data)
	s = reComment.ReplaceAllString(s, " ")
	s = reScriptStyle.ReplaceAllString(s, " ") // never index minified JS
	s = reBlockClose.ReplaceAllString(s, "\n") // keep the paragraph shape
	s = reAnyTag.ReplaceAllString(s, "")
	return squeeze(html.UnescapeString(s))
}

func xmlText(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	var out strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if cd, ok := tok.(xml.CharData); ok {
			if s := strings.TrimSpace(string(cd)); s != "" {
				out.WriteString(s)
				out.WriteByte('\n')
			}
		}
	}
	return squeeze(out.String()), nil
}

// squeeze collapses the runs of whitespace these formats leave behind. Chunking
// is length-based, so indentation and blank lines would otherwise consume the
// budget that should be holding words.
func squeeze(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = reSpaces.ReplaceAllString(s, " ")
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString(strings.TrimSpace(line))
		b.WriteByte('\n')
	}
	return strings.TrimSpace(reBlankLines.ReplaceAllString(b.String(), "\n\n"))
}

// ------------------------------------------------------------------ RTF

// Destinations whose contents are markup, not prose: font tables, colour
// tables, embedded pictures, revision metadata. Left in, they add thousands of
// junk tokens to the index.
var rtfSkip = map[string]bool{
	"fonttbl": true, "colortbl": true, "stylesheet": true, "info": true,
	"pict": true, "themedata": true, "colorschememapping": true,
	"latentstyles": true, "datastore": true, "listtable": true,
	"listoverridetable": true, "rsidtbl": true, "generator": true,
	"filetbl": true, "xmlnstbl": true, "mmathPr": true, "objdata": true,
}

func stripRTF(data []byte) string {
	s := string(data)
	var out strings.Builder
	depth, skipAt := 0, -1
	skipChars := 0 // \ucN: how many literal chars follow a \uN as a fallback

	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '{':
			depth++
			i++
		case c == '}':
			if skipAt >= 0 && depth <= skipAt {
				skipAt = -1
			}
			depth--
			i++
		case c == '\\':
			i++
			if i >= len(s) {
				break
			}
			switch s[i] {
			case '\\', '{', '}':
				if skipAt < 0 {
					out.WriteByte(s[i])
				}
				i++
				continue
			case '\'': // \'hh — a raw byte in the document's codepage
				if i+2 < len(s) {
					if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
						if skipAt < 0 && skipChars == 0 {
							out.WriteRune(rune(v)) // latin-1 approximation
						}
						if skipChars > 0 {
							skipChars--
						}
					}
					i += 3
					continue
				}
				i++
				continue
			case '*':
				// \*\destination — an extension whose content is ignorable.
				i++
				if skipAt < 0 {
					skipAt = depth
				}
				continue
			case '\n', '\r':
				// A backslash before a real line break IS a paragraph mark.
				// macOS textutil writes every paragraph this way rather than
				// with \par, so missing it collapses a whole document into one
				// run-on paragraph — and chunking is paragraph-aligned.
				if skipAt < 0 {
					out.WriteByte('\n')
				}
				i++
				continue
			}

			start := i
			for i < len(s) && ((s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z')) {
				i++
			}
			word := s[start:i]
			numStart := i
			if i < len(s) && (s[i] == '-' || (s[i] >= '0' && s[i] <= '9')) {
				i++
				for i < len(s) && s[i] >= '0' && s[i] <= '9' {
					i++
				}
			}
			num, hasNum := 0, i > numStart
			if hasNum {
				num, _ = strconv.Atoi(s[numStart:i])
			}
			if i < len(s) && s[i] == ' ' { // a single space after a control word is a delimiter
				i++
			}

			if rtfSkip[word] {
				skipAt = depth
				continue
			}
			if skipAt >= 0 {
				continue
			}
			switch word {
			case "par", "line", "sect", "page":
				out.WriteByte('\n')
			case "tab", "cell":
				out.WriteByte('\t')
			case "row":
				out.WriteByte('\n')
			case "u": // \uN — a UTF-16 code unit
				if hasNum {
					u := num
					if u < 0 {
						u += 65536
					}
					if utf16.IsSurrogate(rune(u)) {
						out.WriteRune(0xFFFD)
					} else {
						out.WriteRune(rune(u))
					}
				}
				skipChars = 1
			case "uc":
				skipChars = 0
				if hasNum {
					_ = num // the count applies to the next \u, tracked as 1 above
				}
			}
		default:
			if skipAt < 0 {
				if skipChars > 0 && c != '\n' && c != '\r' {
					skipChars-- // the ASCII fallback for a \uN we already emitted
				} else if c != '\n' && c != '\r' {
					out.WriteByte(c)
				}
			}
			i++
		}
	}
	return squeeze(out.String())
}

// -------------------------------------------------------------- archives

func skipMember(name string) bool {
	base := path.Base(name)
	return strings.HasPrefix(name, "__MACOSX/") ||
		strings.HasPrefix(base, ".") ||
		strings.HasSuffix(name, "/")
}

func addMembers(out []Extracted, archive, member string, data []byte, depth int, total *int) []Extracted {
	sub, err := extractAt(member, data, depth+1)
	if err != nil {
		return out // a member we cannot read is skipped, not fatal for the archive
	}
	for _, s := range sub {
		if *total+len(s.Text) > maxExtractedTotal {
			return out
		}
		*total += len(s.Text)
		out = append(out, Extracted{Name: archive + " → " + s.Name, Text: s.Text})
	}
	return out
}

func zipDocs(name string, data []byte, depth int) ([]Extracted, error) {
	if depth >= maxArchiveDepth {
		return nil, fmt.Errorf("archives nested inside archives are not unpacked")
	}
	zr, err := openZip(data)
	if err != nil {
		return nil, fmt.Errorf("not a readable zip: %w", err)
	}
	out := []Extracted{}
	total := 0
	for _, f := range zr.File {
		if len(out) >= maxArchiveMembers {
			break
		}
		if f.FileInfo().IsDir() || skipMember(f.Name) || f.UncompressedSize64 > maxDocBytes {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxDocBytes))
		rc.Close()
		if err != nil {
			continue
		}
		out = addMembers(out, name, f.Name, b, depth, &total)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no readable text files inside this archive")
	}
	return out, nil
}

func tarDocs(name string, data []byte, depth int) ([]Extracted, error) {
	if depth >= maxArchiveDepth {
		return nil, fmt.Errorf("archives nested inside archives are not unpacked")
	}
	var r io.Reader = bytes.NewReader(data)
	if !strings.HasSuffix(strings.ToLower(name), ".tar") {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("not a readable .tar.gz: %w", err)
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	out := []Extracted{}
	total := 0
	for len(out) < maxArchiveMembers {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if h.Typeflag != tar.TypeReg || skipMember(h.Name) || h.Size > maxDocBytes {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxDocBytes))
		if err != nil {
			continue
		}
		out = addMembers(out, name, h.Name, b, depth, &total)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no readable text files inside this archive")
	}
	return out, nil
}
