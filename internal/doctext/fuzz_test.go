package doctext

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/doctext/doctexttest"
)

// The fuzz targets hold the readers to what they promise of any file,
// however made: no panic (recovery is off while they run, so that a
// mistake shows), no more time than the context gives, no more text than
// MaxText, text that is valid UTF-8, and an error only of the kinds
// Extract names. Their seeds are the fixtures of the other tests and the
// hostile files of hostile_test.go; `go test -fuzz=FuzzX` runs one.

// fuzzLimits are small limits, so that a fuzzed file meets them soon.
func fuzzLimits() Limits {
	return Limits{MaxInflated: 8 << 20, MaxEntry: 4 << 20, MaxEntries: 500, MaxDepth: 64, MaxTokens: 300_000,
		MaxText: 64 << 10, MaxRows: 50, MaxCols: 20, MaxParts: 40, MaxObjects: 20000}
}

// noRecover turns panic recovery off for the length of the test.
func noRecover(t testing.TB) {
	recoverPanics = false
	t.Cleanup(func() { recoverPanics = true })
}

// hold runs Extract on data and holds its result to the promises above.
func hold(t *testing.T, data []byte, f Format) {
	t.Helper()
	lim := fuzzLimits()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	res, err := Extract(ctx, data, f, lim)
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("reading took %s, past its context", d)
	}
	if err != nil {
		for _, known := range []error{ErrLimit, ErrMalformed, ErrEncrypted, ErrOldFormat, context.DeadlineExceeded} {
			if errors.Is(err, known) {
				return
			}
		}
		t.Fatalf("an error of no known kind: %v", err)
	}
	if res == nil {
		t.Fatal("no result and no error")
	}
	if len(res.Text) > lim.MaxText+200 {
		t.Errorf("%d bytes of text, past MaxText (%d)", len(res.Text), lim.MaxText)
	}
	if !utf8.ValidString(res.Text) {
		t.Error("the text is not valid UTF-8")
	}
	if res.Parts > res.Of && f != DOCX {
		t.Errorf("%d parts read of %d", res.Parts, res.Of)
	}
	if f == PDF {
		if _, err := PDFPages(ctx, data, lim); err != nil && !errors.Is(err, ErrLimit) && !errors.Is(err, ErrMalformed) &&
			!errors.Is(err, ErrEncrypted) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("PDFPages: an error of no known kind: %v", err)
		}
	}
}

// seedDeck and the others are the fixtures the fuzzers start from.
func seedDeck() []byte {
	return doctexttest.PPTXDeck(doctexttest.Deck{NamedBackwards: true, External: true, Slides: []doctexttest.Slide{
		{Title: "Introduction", Text: []string{"Week 1"}, Images: 2, SlideNumber: "1", Footer: "CS101"},
		{Title: "演算法", Body: []doctexttest.Bullet{{Text: "步驟", Level: 0}, {Text: "sub", Level: 1}}, Table: [][]string{{"a", "b"}, {"1", "2"}},
			Chart: "Growth", Notes: "Say hello.\nThen ask."},
		{Title: "Hidden", Hidden: true},
	}})
}

func seedDoc() []byte {
	return doctexttest.DOCX(doctexttest.Doc{Header: "Header", Footer: "Footer", Sections: 2, Blocks: []doctexttest.Block{
		{Text: "Title", Heading: 1}, {Text: "Body", Footnote: "A note."}, {Text: "Item", List: "bullet"},
		{Text: "One", List: "number"}, {Text: "Two", List: "number", Level: 1}, {Table: [][]string{{"h1", "h2"}, {"c", "d|e"}}},
		{Text: "Pic", Image: true, TextBox: "Box", Deleted: "old"},
	}})
}

func seedBook() []byte {
	return doctexttest.XLSX(doctexttest.Sheet{Name: "Grades", Rows: [][]any{{"Name", "Score"}, {"Yuki", 90.5}, {"Ken", doctexttest.Formula{F: "A1", Cached: 3}}, {nil, true}}},
		doctexttest.Sheet{Name: "Inline", Inline: true, Hidden: true, Rows: [][]any{{"x", "y"}}})
}

func seedPDFs() [][]byte {
	page := doctexttest.PDFPage{Lines: []string{"Hello (world)"}, CJK: []string{"課程大綱"}, Form: []string{"In a form"},
		Hidden: []string{"hidden"}, Image: true, Raw: "BT /F1 12 Tf [(A) -300 (B)] TJ ET /Span << /ActualText (fi) >> BDC BT (x) Tj ET EMC"}
	var out [][]byte
	for _, o := range []doctexttest.PDFOptions{{}, {Compress: true}, {ObjectStreams: true, Compress: true}, {Encrypt: "rc4"},
		{Encrypt: "aes256", ObjectStreams: true}, {BrokenToUnicode: true, NestedPages: true}, {NoToUnicode: true}} {
		out = append(out, doctexttest.PDFWith(o, page, doctexttest.PDFPage{Image: true}))
	}
	return append(out, hostilePDFs()...)
}

func FuzzPPTX(f *testing.F) {
	f.Add(seedDeck())
	f.Add(doctexttest.PPTX(doctexttest.Slide{Title: "one"}))
	f.Fuzz(func(t *testing.T, data []byte) {
		noRecover(t)
		hold(t, data, PPTX)
	})
}

func FuzzDOCX(f *testing.F) {
	f.Add(seedDoc())
	f.Fuzz(func(t *testing.T, data []byte) {
		noRecover(t)
		hold(t, data, DOCX)
	})
}

func FuzzXLSX(f *testing.F) {
	f.Add(seedBook())
	f.Fuzz(func(t *testing.T, data []byte) {
		noRecover(t)
		hold(t, data, XLSX)
	})
}

// officeParts are the parts FuzzOfficePart replaces, by the number it is
// given: the package, the part, and the format it is read as.
var officeParts = []struct {
	pkg  func() []byte
	part string
	f    Format
}{
	{seedDeck, "ppt/slides/slide3.xml", PPTX},
	{seedDeck, "ppt/presentation.xml", PPTX},
	{seedDeck, "ppt/slides/_rels/slide2.xml.rels", PPTX},
	{seedDeck, "ppt/notesSlides/notesSlide2.xml", PPTX},
	{seedDeck, "ppt/charts/chart2.xml", PPTX},
	{seedDoc, "word/document.xml", DOCX},
	{seedDoc, "word/styles.xml", DOCX},
	{seedDoc, "word/numbering.xml", DOCX},
	{seedDoc, "word/footnotes.xml", DOCX},
	{seedDoc, "word/_rels/document.xml.rels", DOCX},
	{seedBook, "xl/worksheets/sheet1.xml", XLSX},
	{seedBook, "xl/sharedStrings.xml", XLSX},
	{seedBook, "xl/workbook.xml", XLSX},
	{seedBook, "[Content_Types].xml", XLSX},
	{seedBook, "_rels/.rels", XLSX},
}

// withPart is pkg with its part named name replaced by data.
func withPart(t testing.TB, pkg []byte, name string, data []byte) []byte {
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	var parts [][2]string
	for _, f := range zr.File {
		if f.Name == name {
			parts = append(parts, [2]string{name, string(data)})
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, [2]string{f.Name, string(b)})
	}
	return doctexttest.Zip(parts...)
}

// FuzzOfficePart fuzzes one XML part of a well-formed package, which a
// fuzzed archive seldom reaches.
func FuzzOfficePart(f *testing.F) {
	for i, p := range officeParts {
		zr, err := zip.NewReader(bytes.NewReader(p.pkg()), int64(len(p.pkg())))
		if err != nil {
			f.Fatal(err)
		}
		for _, zf := range zr.File {
			if zf.Name == p.part {
				rc, _ := zf.Open()
				b, _ := io.ReadAll(rc)
				_ = rc.Close()
				f.Add(uint8(i), b)
			}
		}
	}
	f.Add(uint8(0), []byte(`<p:sld xmlns:p="p"><p:cSld><p:spTree><p:grpSp><p:grpSp><p:sp><p:txBody><a:p><a:r><a:t>x</a:t></a:r></a:p></p:txBody></p:sp></p:grpSp></p:grpSp></p:spTree></p:cSld></p:sld>`))
	f.Add(uint8(5), []byte(`<!DOCTYPE d [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;&a;">]><w:document><w:body><w:p><w:r><w:t>&b;</w:t></w:r></w:p></w:body></w:document>`))
	f.Fuzz(func(t *testing.T, which uint8, part []byte) {
		noRecover(t)
		p := officeParts[int(which)%len(officeParts)]
		hold(t, withPart(t, p.pkg(), p.part, part), p.f)
	})
}

func FuzzPDF(f *testing.F) {
	for _, s := range seedPDFs() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		noRecover(t)
		hold(t, data, PDF)
	})
}

// pdfWithContent is a one-page PDF whose page content is content, with the
// fixtures' fonts F1 (simple) and F2 (composite) and a form Fm1.
func pdfWithContent(content []byte) []byte {
	base := doctexttest.PDF(doctexttest.PDFPage{Form: []string{"form"}, CJK: []string{"字"}, Raw: "%CONTENT%"})
	i := bytes.Index(base, []byte("%CONTENT%"))
	// The page's stream is uncompressed: its /Length is written before it,
	// and the file's cross-reference is rebuilt when the offsets move.
	return append(append(bytes.Clone(base[:i]), content...), base[i+len("%CONTENT%"):]...)
}

// FuzzPDFContent fuzzes a page's content stream: the text operators, the
// graphics state, forms, marked content and inline images.
func FuzzPDFContent(f *testing.F) {
	for _, c := range []string{
		"BT /F1 12 Tf 72 700 Td (Hello) Tj T* (x) ' 1 2 (y) \" ET",
		"BT /F2 14 Tf <0001> Tj [<0001> -500 <0001>] TJ ET",
		"q 1 0 0 1 10 10 cm /Fm1 Do Q /Fm1 Do",
		"BI /W 2 /H 2 /CS /G /BPC 8 ID \x00\x01EI\x02\x03 EI Q BT (after) Tj ET",
		"/Span << /ActualText <FEFF0066006900660069> >> BDC BT /F1 9 Tf (fi) Tj ET EMC",
		"BT 3 Tr 100 Tz 2 Tc 3 Tw 14 TL 2 Ts 1 0 0 1 5 5 Tm (z) Tj ET",
		"[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[",
		"<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<",
	} {
		f.Add([]byte(c))
	}
	f.Fuzz(func(t *testing.T, content []byte) {
		noRecover(t)
		hold(t, pdfWithContent(content), PDF)
	})
}

// FuzzCMap fuzzes a CMap: read as a ToUnicode map, and as an encoding,
// then used to decode strings.
func FuzzCMap(f *testing.F) {
	f.Add([]byte("1 begincodespacerange <0000> <FFFF> endcodespacerange 2 beginbfchar <0001> <8AB2> <0002> <D83DDE00> endbfchar "+
		"1 beginbfrange <0010> <0020> <0041> endbfrange 1 beginbfrange <0030> <0032> [<0061> <0062> <0063>] endbfrange"), []byte("\x00\x01\x00\x02\x00\x15\x00\x31"))
	f.Add([]byte("/usecmap /GBK-EUC-H usecmap /WMode 1 def 1 begincodespacerange <00> <80> endcodespacerange"), []byte("abc\xb0\xa1"))
	f.Fuzz(func(t *testing.T, data, s []byte) {
		noRecover(t)
		b := newBudget(context.Background(), fuzzLimits())
		m := parseCMap(data, b)
		font := &pdfFont{composite: true, toU: m, encCMap: m, dw: 1000, fm0: 0.001}
		if p, ok := predefinedCMap(m.use); ok {
			font.pre = &p
		}
		for _, g := range font.decode(s) {
			if !utf8.ValidString(g.text) {
				t.Fatalf("a glyph's text is not valid UTF-8: %q", g.text)
			}
		}
		simple := &pdfFont{toU: m, simple: standardEncoding, fm0: 0.001}
		_ = simple.decode(s)
	})
}

// A fuzzed PDF must not make PDFPages run away either.
func TestFuzzSeedsAreHeld(t *testing.T) {
	noRecover(t)
	for i, s := range seedPDFs() {
		t.Run(fmt.Sprint(i), func(t *testing.T) { hold(t, s, PDF) })
	}
}
