package doctext

import (
	"context"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
)

// holdSections holds a result's sections to what they promise: in order,
// each within the text, at the start of a line, and at a heading of its
// kind.
func holdSections(t testing.TB, res *Result) {
	t.Helper()
	heads := map[string]string{SectionSlide: "## Slide ", SectionPage: "## Page ", SectionSheet: "## Sheet "}
	prev := -1
	for i, s := range res.Sections {
		if s.Offset <= prev || s.Offset >= len(res.Text) {
			t.Fatalf("section %d at %d, after %d, in %d bytes of text", i, s.Offset, prev, len(res.Text))
		}
		prev = s.Offset
		if s.Offset > 0 && res.Text[s.Offset-1] != '\n' {
			t.Errorf("section %d does not begin a line", i)
		}
		if !strings.HasPrefix(res.Text[s.Offset:], heads[s.Kind]) {
			t.Errorf("section %d (%s %d) begins %q", i, s.Kind, s.N, res.Text[s.Offset:min(len(res.Text), s.Offset+20)])
		}
	}
}

// TestSections checks that each slide, page and sheet is recorded where
// its heading begins, by its number, and that nothing else is: a slide
// whose text reads like a heading is still one slide, and a document has
// none.
func TestSections(t *testing.T) {
	deck := doctexttest.PPTX(
		doctexttest.Slide{Title: "Sorting", Text: []string{"## Slide 9: not a slide"}},
		doctexttest.Slide{Title: "排序", Notes: "Say it twice."},
		doctexttest.Slide{Title: "Merge"},
	)
	pdf := doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"one"}}, doctexttest.PDFPage{Image: true}, doctexttest.PDFPage{Lines: []string{"three"}})
	book := doctexttest.XLSX(doctexttest.Sheet{Name: "Quiz", Rows: [][]any{{"a", 1}}}, doctexttest.Sheet{Name: "Lab", Rows: [][]any{{"b", 2}}})
	doc := doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "Lab 3", Heading: 1}, {Text: "Bring a laptop."}}})
	tests := []struct {
		name string
		data []byte
		f    Format
		kind string
		n    int
	}{
		{"a deck's slides", deck, PPTX, SectionSlide, 3},
		{"a PDF's pages, the one with no text among them", pdf, PDF, SectionPage, 3},
		{"a workbook's sheets", book, XLSX, SectionSheet, 2},
		{"a document has none", doc, DOCX, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Extract(context.Background(), tc.data, tc.f, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			holdSections(t, res)
			if len(res.Sections) != tc.n {
				t.Fatalf("%d sections, want %d: %+v", len(res.Sections), tc.n, res.Sections)
			}
			for i, s := range res.Sections {
				if s.Kind != tc.kind || s.N != i+1 {
					t.Errorf("section %d is %s %d", i, s.Kind, s.N)
				}
			}
		})
	}
}

// TestSectionsCut checks that a text cut at MaxText keeps the sections of
// what it gives, and none past the cut.
func TestSectionsCut(t *testing.T) {
	var slides []doctexttest.Slide
	for range 50 {
		slides = append(slides, doctexttest.Slide{Title: "Title", Text: []string{strings.Repeat("words ", 30)}})
	}
	res, err := Extract(context.Background(), doctexttest.PPTX(slides...), PPTX, Limits{MaxText: 2000})
	if err != nil {
		t.Fatal(err)
	}
	holdSections(t, res)
	if len(res.Sections) == 0 || len(res.Sections) >= 50 || !strings.Contains(res.Text, "[The rest of the file is not given") {
		t.Fatalf("%d sections of a cut text: %q", len(res.Sections), res.Text)
	}
}
