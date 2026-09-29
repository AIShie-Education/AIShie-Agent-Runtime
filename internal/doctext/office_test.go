package doctext

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
)

func extract(t *testing.T, data []byte, f Format, lim Limits) *Result {
	t.Helper()
	res, err := Extract(context.Background(), data, f, lim)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Format != f {
		t.Errorf("format %s, want %s", res.Format, f)
	}
	return res
}

func wantText(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("text:\n%s\n--- want:\n%s", got, want)
	}
}

// TestPPTX is a deck read as the model gets it: the slides in the order
// the presentation lists them, not the order of their parts' names; each
// slide's title, its body bulleted by level, a text box, a table, its
// pictures and chart named, its notes; the slide number and footer left
// out; a hidden slide marked. The links to a web page and to a file
// outside the package are never followed.
func TestPPTX(t *testing.T) {
	deck := doctexttest.PPTXDeck(doctexttest.Deck{NamedBackwards: true, External: true, Slides: []doctexttest.Slide{
		{Title: "Introduction to Computing", Text: []string{"Week 1", "Prof. Sato"}, Images: 1, SlideNumber: "1", Footer: "CS101 · Fall"},
		{Title: "What is an algorithm?", Body: []doctexttest.Bullet{{Text: "A finite sequence of steps"}, {Text: "Each step is precise", Level: 1},
			{Text: "Two lines\nin one paragraph"}}, Table: [][]string{{"Input", "Output"}, {"3", "9"}, {"a|b", ""}}, Images: 2,
			Chart: "Growth of n²", Notes: "Ask for everyday algorithms.\nThen recipes."},
		{Title: "Backup", Hidden: true, Body: []doctexttest.Bullet{{Text: "Spare examples"}}},
	}})
	res := extract(t, deck, PPTX, DefaultLimits())
	wantText(t, res.Text, `## Slide 1: Introduction to Computing
Week 1
Prof. Sato
[image]

## Slide 2: What is an algorithm?
- A finite sequence of steps
  - Each step is precise
- Two lines
  in one paragraph
| Input | Output |
| --- | --- |
| 3 | 9 |
| a\|b |  |
[image]
[image]
[chart: Growth of n²]
Notes: Ask for everyday algorithms.
Then recipes.

## Slide 3 (hidden): Backup
- Spare examples`)
	if res.Parts != 3 || res.Of != 3 || res.Images != 3 || res.Charts != 1 || len(res.Notes) != 0 {
		t.Errorf("parts %d of %d, %d images, %d charts, notes %v", res.Parts, res.Of, res.Images, res.Charts, res.Notes)
	}
}

// TestPPTXPartsCut gives the first MaxParts slides, and says so.
func TestPPTXPartsCut(t *testing.T) {
	var slides []doctexttest.Slide
	for _, title := range []string{"One", "Two", "Three"} {
		slides = append(slides, doctexttest.Slide{Title: title})
	}
	lim := DefaultLimits()
	lim.MaxParts = 2
	res := extract(t, doctexttest.PPTX(slides...), PPTX, lim)
	wantText(t, res.Text, "## Slide 1: One\n\n## Slide 2: Two")
	if res.Parts != 2 || res.Of != 3 || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "first 2 slides of 3") {
		t.Errorf("parts %d of %d, notes %v", res.Parts, res.Of, res.Notes)
	}
}

// TestPPTXDamagedSlide reads the slides it can.
func TestPPTXDamagedSlide(t *testing.T) {
	deck := doctexttest.PPTX(doctexttest.Slide{Title: "Fine"}, doctexttest.Slide{Title: "Broken"})
	deck = withPart(t, deck, "ppt/slides/slide2.xml", []byte("<p:sld><unclosed>"))
	res := extract(t, deck, PPTX, DefaultLimits())
	wantText(t, res.Text, "## Slide 1: Fine\n\n## Slide 2\n[this slide could not be read]")
}

// TestPPTXTextCut stops at MaxText, and says the rest is not given.
func TestPPTXTextCut(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxText = 1 << 10
	res := extract(t, doctexttest.PPTX(doctexttest.Slide{Title: "Long", Text: []string{strings.Repeat("字", 1000)}},
		doctexttest.Slide{Title: "Never read"}), PPTX, lim)
	if !strings.HasSuffix(res.Text, "[The rest of the file is not given: its text passed the 1 KiB the runtime reads.]") ||
		strings.Contains(res.Text, "Never read") || res.Parts != 1 {
		t.Errorf("text %q…, %d parts", cut(res.Text, 80), res.Parts)
	}
}

// TestDOCX is a Word document as the model gets it: headings as #,
// bulleted and numbered lists by level, a table in Markdown, a picture
// named, a text box's text once (not again from its fallback), deleted
// text left out; its footnote after the body; its header and footer once,
// however many sections repeat them.
func TestDOCX(t *testing.T) {
	doc := doctexttest.DOCX(doctexttest.Doc{Header: "CS101 Syllabus", Footer: "Fall term", Sections: 3, Blocks: []doctexttest.Block{
		{Text: "Course syllabus", Heading: 1},
		{Text: "This course covers algorithms.", Footnote: "See chapter 1."},
		{Text: "Goals", Heading: 2},
		{Text: "Learn to think", List: "bullet"},
		{Text: "In steps", List: "bullet", Level: 1},
		{Text: "First", List: "number"},
		{Text: "Detail", List: "number", Level: 1},
		{Text: "Second", List: "number"},
		{Table: [][]string{{"Week", "Topic"}, {"1", "Intro | basics"}, {"2"}}},
		{Text: "A figure:", Image: true, TextBox: "Boxed text", Deleted: "withdrawn words"},
		{Text: "課程目標：理解演算法"},
	}})
	res := extract(t, doc, DOCX, DefaultLimits())
	wantText(t, res.Text, `# Course syllabus

This course covers algorithms.[^1]

## Goals

- Learn to think
  - In steps
1. First
  1. Detail
2. Second

| Week | Topic |
| --- | --- |
| 1 | Intro \| basics |
| 2 |  |

A figure: [image]
Boxed text
課程目標：理解演算法

[^1]: See chapter 1.

Header: CS101 Syllabus

Footer: Fall term`)
	if res.Images != 1 || res.Parts != 1 {
		t.Errorf("%d images, %d parts", res.Images, res.Parts)
	}
}

// TestXLSX is a workbook as the model gets it: each sheet by name, its
// rows as CSV, shared and inline strings resolved, numbers as stored, a
// formula's cached value, booleans; empty rows left out; a hidden sheet
// marked.
func TestXLSX(t *testing.T) {
	book := doctexttest.XLSX(
		doctexttest.Sheet{Name: "Grades", Rows: [][]any{{"Name", "Score", "Passed"}, {"Yuki", 91.5, true},
			{"Ken, Jr.", doctexttest.Formula{F: "AVERAGE(B2:B2)", Cached: 77}, false}, nil, {nil, "只有第二欄"}, {"say \"hi\"", 1e-7}}},
		doctexttest.Sheet{Name: "Notes", Inline: true, Hidden: true, Rows: [][]any{{"inline", "strings"}}},
	)
	res := extract(t, book, XLSX, DefaultLimits())
	wantText(t, res.Text, `## Sheet 1: Grades
Name,Score,Passed
Yuki,91.5,TRUE
"Ken, Jr.",77,FALSE
,只有第二欄
"say ""hi""",1e-07

## Sheet 2: Notes (hidden)
inline,strings`)
	if res.Parts != 2 || res.Of != 2 {
		t.Errorf("parts %d of %d", res.Parts, res.Of)
	}
}

// TestXLSXCut gives a sheet's first MaxRows rows and MaxCols columns, and
// says what it cut, in the text and the notes.
func TestXLSXCut(t *testing.T) {
	var rows [][]any
	for i := range 12 {
		rows = append(rows, []any{i, "b", "c", "d", "e"})
	}
	lim := DefaultLimits()
	lim.MaxRows, lim.MaxCols = 10, 3
	res := extract(t, doctexttest.XLSX(doctexttest.Sheet{Name: "Big", Rows: rows}), XLSX, lim)
	head, _, _ := strings.Cut(res.Text, "\n")
	if head != "## Sheet 1: Big (its first 10 rows of 12 are given; its first 3 columns of 5 are given)" {
		t.Errorf("heading %q", head)
	}
	if !strings.Contains(res.Text, "\n9,b,c") || strings.Contains(res.Text, "\n10,b") || strings.Contains(res.Text, ",d") {
		t.Errorf("text %q", res.Text)
	}
	if len(res.Notes) != 1 || !strings.HasPrefix(res.Notes[0], `sheet "Big" is cut: its first 10 rows of 12`) {
		t.Errorf("notes %v", res.Notes)
	}
}

func TestColumn(t *testing.T) {
	for ref, want := range map[string]int{"A1": 0, "B3": 1, "Z9": 25, "AA1": 26, "XFD1048576": 16383, "a2": 0} {
		if got, ok := column(ref); !ok || got != want {
			t.Errorf("column(%q) = %d, %v; want %d", ref, got, ok, want)
		}
	}
	for _, bad := range []string{"", "1", "XFE1", "AAAA1"} {
		if _, ok := column(bad); ok {
			t.Errorf("column(%q) is a column", bad)
		}
	}
}

func TestSniff(t *testing.T) {
	cases := []struct {
		data []byte
		want Format
		err  error
	}{
		{doctexttest.PPTX(doctexttest.Slide{Title: "x"}), PPTX, nil},
		{doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "x"}}}), DOCX, nil},
		{doctexttest.XLSX(doctexttest.Sheet{Name: "x"}), XLSX, nil},
		{doctexttest.PDFLines("x"), PDF, nil},
		{append([]byte("junk before "), doctexttest.PDFLines("x")...), PDF, nil},
		{doctexttest.Zip([2]string{"readme.txt", "just a zip"}), "", nil},
		{[]byte("plain text"), "", nil},
		{append(append([]byte(nil), cfbMagic...), make([]byte, 100)...), "", ErrOldFormat},
		{append(append(append([]byte(nil), cfbMagic...), make([]byte, 100)...), utf16LE("EncryptedPackage")...), "", ErrEncrypted},
	}
	for i, c := range cases {
		got, err := Sniff(c.data)
		if got != c.want || !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
			t.Errorf("case %d: %q, %v; want %q, %v", i, got, err, c.want, c.err)
		}
	}
}

func TestFormatOf(t *testing.T) {
	for mt, want := range map[string]Format{
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": PPTX,
		"application/vnd.ms-powerpoint.presentation.macroenabled.12":                PPTX,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.template":   DOCX,
		"application/vnd.ms-excel.sheet.macroenabled.12":                            XLSX,
		"application/pdf": PDF,
	} {
		if got, ok := FormatOf(mt); !ok || got != want {
			t.Errorf("FormatOf(%q) = %q, %v", mt, got, ok)
		}
		if want.MediaType() == "" {
			t.Errorf("%s has no media type", want)
		}
	}
	if _, ok := FormatOf("application/msword"); ok || !OldOffice("application/msword") || !OldOffice("application/vnd.ms-powerpoint") ||
		OldOffice("application/pdf") {
		t.Error("the older formats are not told apart")
	}
}

func TestResolve(t *testing.T) {
	for _, c := range []struct{ dir, target, want string }{
		{"ppt/slides/", "../media/image1.png", "ppt/media/image1.png"},
		{"ppt/", "slides/slide1.xml", "ppt/slides/slide1.xml"},
		{"ppt/slides/", "/ppt/charts/chart1.xml", "ppt/charts/chart1.xml"},
		{"ppt/slides/", "media/image%201.png", "ppt/slides/media/image 1.png"},
		{"ppt/", "../../../etc/passwd", ""},
		{"", "../x.xml", ""},
		{"ppt/", "https://example.invalid/x", ""},
		{"ppt/", "file:///etc/passwd", ""},
		{"ppt/", `..\..\x`, ""},
		{"ppt/", "", ""},
	} {
		if got := resolve(c.dir, c.target); got != c.want {
			t.Errorf("resolve(%q, %q) = %q, want %q", c.dir, c.target, got, c.want)
		}
	}
}
