package doctext

import (
	"bytes"
	"compress/lzw"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
)

// course is a two-page PDF of what a course's handout holds: Latin text,
// traditional Chinese in a composite font with its ToUnicode map; a scan
// with its hidden recognized text, text in a form, kerned text and a
// ligature given as ActualText.
var course = []doctexttest.PDFPage{
	{Lines: []string{"CS101 Introduction to Computing", "Week 1: What is an algorithm?"},
		CJK: []string{"課程大綱：資料結構與演算法", "第一週 介紹課程目標與評分方式"}},
	{Form: []string{"Drawn in a form"}, Image: true, Hidden: []string{"Recognized from a scan"},
		Raw: "BT /F1 12 Tf 72 100 Td [(Hel) 20 (lo) -300 (there)] TJ ET\n" +
			"/Span << /ActualText <FEFF00660069> >> BDC BT /F1 12 Tf 72 80 Td (\x1e) Tj ET EMC BT /F1 12 Tf 80 80 Td (nal) Tj ET"},
}

const courseText = `## Page 1
CS101 Introduction to Computing
Week 1: What is an algorithm?
課程大綱：資料結構與演算法
第一週 介紹課程目標與評分方式

## Page 2
Recognized from a scan
Drawn in a form
Hello there
final`

// TestPDF reads the same document however it is written: uncompressed,
// compressed, with its objects in object streams, encrypted with RC4 or
// AES-256 for anyone to open, with its pages nested, and with its
// cross-reference damaged and rebuilt.
func TestPDF(t *testing.T) {
	for _, o := range []doctexttest.PDFOptions{{}, {Compress: true}, {ObjectStreams: true, Compress: true}, {Encrypt: "rc4"},
		{Encrypt: "aes256", ObjectStreams: true}, {NestedPages: true}} {
		for _, damaged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%+v damaged %v", o, damaged), func(t *testing.T) {
				data := doctexttest.PDFWith(o, course...)
				if damaged {
					i := bytes.LastIndex(data, []byte("startxref"))
					data = append(data[:i:i], []byte("startxref\n123\n%%EOF\n")...)
				}
				res := extract(t, data, PDF, DefaultLimits())
				wantText(t, res.Text, courseText)
				if res.Parts != 2 || res.Of != 2 || res.Images != 1 || res.Unreadable != "" || len(res.Notes) != 0 {
					t.Errorf("parts %d of %d, %d images, unreadable %q, notes %v", res.Parts, res.Of, res.Images, res.Unreadable, res.Notes)
				}
				n, err := PDFPages(context.Background(), data, DefaultLimits())
				if err != nil || n != 2 {
					t.Errorf("PDFPages: %d, %v", n, err)
				}
			})
		}
	}
}

// TestPDFPassword refuses a PDF that needs a password to open, as
// encrypted, however it is encrypted.
func TestPDFPassword(t *testing.T) {
	for _, enc := range []string{"rc4", "aes256"} {
		data := doctexttest.PDFWith(doctexttest.PDFOptions{Encrypt: enc, UserPassword: "secret"}, course...)
		if _, err := Extract(context.Background(), data, PDF, DefaultLimits()); !errors.Is(err, ErrEncrypted) {
			t.Errorf("%s: Extract: %v", enc, err)
		}
		if _, err := PDFPages(context.Background(), data, DefaultLimits()); !errors.Is(err, ErrEncrypted) {
			t.Errorf("%s: PDFPages: %v", enc, err)
		}
	}
}

// TestPDFReadability tells text that reads from text that does not: a
// ToUnicode map that gives every glyph the same character (《, as a real
// course's PDF gave), a composite font with no map at all, and a scan with
// no text; while CJK that maps, and a scan with its recognized text, read.
func TestPDFReadability(t *testing.T) {
	cjk := []string{"課程大綱：資料結構與演算法，第一週介紹課程目標與評分方式", "第二週：陣列、串列與堆疊"}
	cases := []struct {
		name string
		data []byte
		want string
		text string
	}{
		{"CJK with its map", doctexttest.PDF(doctexttest.PDFPage{CJK: cjk}), "", "課程大綱"},
		{"a map that gives every glyph 《", doctexttest.PDFWith(doctexttest.PDFOptions{BrokenToUnicode: true}, doctexttest.PDFPage{CJK: cjk}),
			UnreadableUnmapped, "《《《《《《"},
		{"a short text that maps every glyph to 《", doctexttest.PDFWith(doctexttest.PDFOptions{BrokenToUnicode: true},
			doctexttest.PDFPage{CJK: []string{"期中考範圍"}}), UnreadableUnmapped, "《《《《《"},
		{"a composite font with no map", doctexttest.PDFWith(doctexttest.PDFOptions{NoToUnicode: true}, doctexttest.PDFPage{CJK: cjk}),
			UnreadableUnmapped, "�"},
		{"a scan", doctexttest.PDF(doctexttest.PDFPage{Image: true}, doctexttest.PDFPage{Image: true}), UnreadableNoText, "[no text on this page]"},
		{"a scan with its recognized text", doctexttest.PDF(doctexttest.PDFPage{Image: true, Hidden: []string{"Lecture notes, week three"}}), "", "week three"},
		{"Latin text under a broken CJK heading", doctexttest.PDFWith(doctexttest.PDFOptions{BrokenToUnicode: true},
			doctexttest.PDFPage{CJK: []string{"標題"}, Lines: []string{strings.Repeat("Sorting takes n log n comparisons. ", 10)}}), "", "Sorting"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := extract(t, c.data, PDF, DefaultLimits())
			if res.Unreadable != c.want || !strings.Contains(res.Text, c.text) {
				t.Errorf("unreadable %q, text %q; want %q and %q", res.Unreadable, cut(res.Text, 200), c.want, c.text)
			}
		})
	}
}

// TestPDFSomePagesScanned reads the pages with text, and says how many
// have none.
func TestPDFSomePagesScanned(t *testing.T) {
	res := extract(t, doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"Typed page"}}, doctexttest.PDFPage{Image: true},
		doctexttest.PDFPage{Lines: []string{"Another typed page"}}), PDF, DefaultLimits())
	wantText(t, res.Text, "## Page 1\nTyped page\n\n## Page 2\n[no text on this page]\n\n## Page 3\nAnother typed page")
	if res.Unreadable != "" || len(res.Notes) != 1 || res.Notes[0] != "1 of its 3 pages have no text: they may be scanned, or pictures of text" {
		t.Errorf("unreadable %q, notes %v", res.Unreadable, res.Notes)
	}
}

func TestPDFPartsCut(t *testing.T) {
	var pages []doctexttest.PDFPage
	for i := range 5 {
		pages = append(pages, doctexttest.PDFPage{Lines: []string{fmt.Sprint("page ", i+1)}})
	}
	lim := DefaultLimits()
	lim.MaxParts = 3
	res := extract(t, doctexttest.PDFWith(doctexttest.PDFOptions{NestedPages: true}, pages...), PDF, lim)
	if res.Parts != 3 || res.Of != 5 || strings.Contains(res.Text, "page 4") || len(res.Notes) != 1 || res.Notes[0] != "only its first 3 pages of 5 are given" {
		t.Errorf("parts %d of %d, notes %v, text %q", res.Parts, res.Of, res.Notes, res.Text)
	}
}

// fontPDF is a one-page PDF showing each of shows in the font dict, with
// more objects after it (numbered from 6).
func fontPDF(font string, shows []string, more ...string) []byte {
	var c strings.Builder
	c.WriteString("BT /F 12 Tf 72 700 Td")
	for i, s := range shows {
		if i > 0 {
			c.WriteString(" 0 -20 Td")
		}
		c.WriteString(" " + s + " Tj")
	}
	c.WriteString(" ET")
	return rawPDF("", append([]string{catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /Resources << /Font << /F 5 0 R >> >> /Contents 4 0 R >>", stream("", c.String()), font}, more...)...)
}

// TestPDFEncodings holds the fonts' encodings to what they mean: glyph
// names, the standard encodings, Symbol and dingbats; Adobe's predefined
// Unicode and national CMaps; an embedded encoding CMap; ToUnicode maps of
// ranges, arrays and surrogate pairs.
func TestPDFEncodings(t *testing.T) {
	type0 := func(enc string, more string) string {
		return "<< /Type /Font /Subtype /Type0 /BaseFont /X /Encoding " + enc + " /DescendantFonts [<< /Type /Font /Subtype /CIDFontType0 /BaseFont /X >>]" + more + " >>"
	}
	cases := []struct {
		name  string
		data  []byte
		want  string
		unmap bool
	}{
		{"glyph names in Differences", fontPDF("<< /Type /Font /Subtype /Type1 /BaseFont /CMR10 /Encoding << /BaseEncoding /WinAnsiEncoding "+
			"/Differences [1 /fi /quoteright /uni8AB2 /Aacute /alpha /Scommaaccent /f_f_l /u1F600 /a.sc] >> >>",
			[]string{`<010203040506070809> `}), "ﬁ’課ÁαȘffl😀a", false},
		{"StandardEncoding's quotes and ligatures", fontPDF("<< /Type /Font /Subtype /Type1 /BaseFont /Times-Roman >>",
			[]string{`(It\047s \256ne) `}), "It’s ﬁne", false},
		{"WinAnsi and MacRoman", fontPDF("<< /Type /Font /Subtype /TrueType /BaseFont /Arial /Encoding /WinAnsiEncoding >>",
			[]string{`(caf\351 \200) `}), "café €", false},
		{"Symbol's Greek", fontPDF("<< /Type /Font /Subtype /Type1 /BaseFont /Symbol >>", []string{`(abg \264 \326) `}), "αβγ × √", false},
		{"Wingdings' bullets", fontPDF("<< /Type /Font /Subtype /TrueType /BaseFont /ABCDEF+Wingdings >>", []string{`(l) `}), "•", false},
		{"UniGB-UCS2-H", fontPDF(type0("/UniGB-UCS2-H", ""), []string{`<4E2D6587> `}), "中文", false},
		{"UniJIS-UTF16-H with a surrogate pair", fontPDF(type0("/UniJIS-UTF16-H", ""), []string{`<65E5D83DDE00> `}), "日😀", false},
		{"GBK-EUC-H", fontPDF(type0("/GBK-EUC-H", ""), []string{`<D6D0CEC4> `, `(ab)`}), "中文\nab", false},
		{"Big Five", fontPDF(type0("/ETen-B5-H", ""), []string{`<A4A4A4E5> `}), "中文", false},
		{"Shift-JIS", fontPDF(type0("/90ms-RKSJ-H", ""), []string{`<93FA967B8CEA> `}), "日本語", false},
		{"EUC-KR", fontPDF(type0("/KSC-EUC-H", ""), []string{`<C7D1B1B9> `}), "한국", false},
		{"Identity-H with no map", fontPDF(type0("/Identity-H", ""), []string{`<00410042> `}), "��", true},
		{"an embedded encoding CMap of one byte, with a ToUnicode", fontPDF(type0("6 0 R", " /ToUnicode 7 0 R"), []string{`(\001\002) `},
			stream("", "/CIDInit /ProcSet findresource begin 1 begincodespacerange <00> <FF> endcodespacerange 1 begincidrange <00> <FF> 0 endcidrange"),
			stream("", "1 begincodespacerange <00> <FF> endcodespacerange 2 beginbfchar <01> <0048> <02> <0069> endbfchar")), "Hi", false},
		{"ToUnicode ranges and arrays", fontPDF(type0("/Identity-H", " /ToUnicode 6 0 R"), []string{`<0010001100120020002100220030> `},
			stream("", "1 begincodespacerange <0000> <FFFF> endcodespacerange 1 beginbfrange <0010> <0012> <0061> endbfrange "+
				"1 beginbfrange <0020> <0022> [<0078> <00660069> <D83DDE00>] endbfrange 1 beginbfchar <0030> /Omega endbfchar")), "abcxfi😀Ω", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := extract(t, c.data, PDF, DefaultLimits())
			if got := strings.TrimPrefix(res.Text, "## Page 1\n"); got != c.want {
				t.Errorf("text %q, want %q", got, c.want)
			}
			if (res.Unreadable != "") != c.unmap {
				t.Errorf("unreadable %q", res.Unreadable)
			}
		})
	}
}

// TestJudge holds the readability heuristics to text of each kind.
func TestJudge(t *testing.T) {
	garbled := func(from, to rune) string {
		var sb strings.Builder
		for r := from; r < to; r += 7 {
			sb.WriteRune(r)
			if r%5 == 0 {
				sb.WriteByte(' ')
			}
		}
		return sb.String()
	}
	cases := []struct {
		name  string
		pages []string
		want  string
	}{
		{"English", []string{"Sorting algorithms order a list. Merge sort takes n log n time."}, ""},
		{"traditional Chinese", []string{"資料結構是電腦科學的基礎課程，本週介紹陣列與串列。"}, ""},
		{"Japanese, of three scripts", []string{"データ構造の授業では、配列とリストを学びます。ソートも扱います。"}, ""},
		{"Russian", []string{"Алгоритмы сортировки упорядочивают список элементов по возрастанию."}, ""},
		{"a table of numbers", []string{"Week Score\n1 90.0\n2 85.5\n3 77.0\n4 100.0\n5 60.5\n6 88.0"}, ""},
		{"no text at all", []string{"", ""}, UnreadableNoText},
		{"one glyph over and over", []string{strings.Repeat("《", 60)}, UnreadableUnmapped},
		{"one glyph, in runs between spaces", []string{strings.Repeat("《《《《《《 ", 20)}, UnreadableUnmapped},
		{"unmapped glyphs", []string{strings.Repeat("�", 30) + " a few words"}, UnreadableUnmapped},
		{"private use", []string{strings.Repeat(" ", 20)}, UnreadableUnmapped},
		{"glyph numbers taken for characters", []string{garbled(0x0400, 0x0900)}, UnreadableUnmapped},
		{"punctuation where the words were", []string{strings.Repeat("!#$%&*+-/<=>?@^_~ ", 5)}, UnreadableUnmapped},
		{"mostly scanned", append(make([]string, 30), "a cover page"), UnreadableNoText},
	}
	for _, c := range cases {
		q := newQuality()
		for _, p := range c.pages {
			q.add(p)
		}
		if got := q.judge(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestLZW decodes the example of ISO 32000-1 §7.4.4.2, with the early
// change PDF's LZW has by default; and what Go's encoder, which has
// none, writes.
func TestLZW(t *testing.T) {
	got, err := unLZW([]byte{0x80, 0x0B, 0x60, 0x50, 0x22, 0x0C, 0x0C, 0x85, 0x01}, true, 1<<20)
	if err != nil || string(got) != "-----A---B" {
		t.Errorf("%q, %v", got, err)
	}
	text := strings.Repeat("the quick brown fox jumps over the lazy dog; ", 400)
	var b bytes.Buffer
	w := lzw.NewWriter(&b, lzw.MSB, 8)
	_, _ = w.Write([]byte(text))
	_ = w.Close()
	got, err = unLZW(b.Bytes(), false, 1<<20)
	if err != nil || string(got) != text {
		t.Errorf("%d bytes, %v", len(got), err)
	}
	if _, err := unLZW(b.Bytes(), false, 100); !errors.Is(err, ErrLimit) {
		t.Errorf("past the limit: %v", err)
	}
}

func TestFilters(t *testing.T) {
	if got := unASCIIHex([]byte("48 65 6C6C6f 7>")); string(got) != "Hellop" {
		t.Errorf("ASCIIHex %q", got)
	}
	if got, err := unASCII85([]byte("<~87cURD]i,\"Ebo80~>")); err != nil || string(got) != "Hello World!" {
		t.Errorf("ASCII85 %q, %v", got, err)
	}
	if got, err := unRunLength([]byte{2, 'a', 'b', 'c', 254, 'x', 128, 'z'}, 100); err != nil || string(got) != "abcxxx" {
		t.Errorf("RunLength %q, %v", got, err)
	}
	// TIFF's predictor 2: each byte the difference from the one before.
	got, err := predict([]byte{1, 1, 1, 5, 250, 10}, pdfDict{"Predictor": 2, "Columns": 3}, 100)
	if err != nil || !bytes.Equal(got, []byte{1, 2, 3, 5, 255, 9}) {
		t.Errorf("TIFF predictor %v, %v", got, err)
	}
	// PNG's Sub, Up, Average and Paeth rows.
	got, err = predict([]byte{1, 1, 2, 2, 1, 1, 1, 3, 2, 2, 2, 4, 0, 0, 0}, pdfDict{"Predictor": 15, "Columns": 2, "Colors": 1}, 100)
	if err != nil || len(got) != 10 {
		t.Errorf("PNG predictor %v, %v", got, err)
	}
	if _, err := predict([]byte{0}, pdfDict{"Predictor": 12, "Columns": -1}, 100); !errors.Is(err, ErrMalformed) {
		t.Errorf("a bad predictor: %v", err)
	}
}

func TestGlyphText(t *testing.T) {
	for name, want := range map[string]string{
		"A": "A", "space": " ", "quoteright": "’", "fi": "ﬁ", "f_f_i": "ffi", "uni8AB28A08": "課計", "u1F600": "😀", "Eacute": "É",
		"zcaron": "ž", "Gcommaaccent": "Ģ", "a.sc": "a", "g123": "", ".notdef": "", "uniD800": "", "": "", "alpha": "α",
	} {
		if got := glyphText(name); got != want {
			t.Errorf("glyphText(%q) = %q, want %q", name, got, want)
		}
	}
}
