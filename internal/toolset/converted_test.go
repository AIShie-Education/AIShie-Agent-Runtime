package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// stubOffice converts and cuts as a test says, and records what it is
// asked: a conversion gives out[to] unless convert says otherwise; a range
// is a PDF of those pages, each saying its number; picked pages are a PDF
// of their own.
type stubOffice struct {
	off     string
	noCuts  bool
	out     map[office.Target]*office.Output
	convert func(f office.Format, to office.Target) office.State

	mu        sync.Mutex
	converted []string
	ranges    [][2]int
	picks     [][]int
	badSum    bool
}

func (s *stubOffice) Available() (bool, string) { return s.off == "", s.off }

func (s *stubOffice) Convert(_ context.Context, sum string, f office.Format, to office.Target, data []byte) office.State {
	s.mu.Lock()
	s.converted = append(s.converted, f.Ext+">"+string(to))
	s.badSum = s.badSum || sum != checksum(data)
	s.mu.Unlock()
	if s.convert != nil {
		return s.convert(f, to)
	}
	return office.State{Status: office.StatusDone, Out: s.out[to]}
}

func (s *stubOffice) Cuts() bool { return !s.noCuts }

func (s *stubOffice) Range(_ context.Context, _ string, _ []byte, first, last int) ([]byte, error) {
	s.mu.Lock()
	s.ranges = append(s.ranges, [2]int{first, last})
	s.mu.Unlock()
	return numberedPDF(first, last), nil
}

func (s *stubOffice) Pick(_ context.Context, _ []byte, pages []int) ([]byte, error) {
	s.mu.Lock()
	s.picks = append(s.picks, pages)
	s.mu.Unlock()
	return []byte("%PDF-picked"), nil
}

func (s *stubOffice) record() (converted []string, ranges [][2]int, picks [][]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.converted...), append([][2]int(nil), s.ranges...), append([][]int(nil), s.picks...)
}

// numberedPDF is a PDF of pages first to last, each saying its number.
func numberedPDF(first, last int) []byte {
	var pages []doctexttest.PDFPage
	for i := first; i <= last; i++ {
		pages = append(pages, doctexttest.PDFPage{Lines: []string{fmt.Sprintf("page %d", i)}})
	}
	return doctexttest.PDF(pages...)
}

// pdfOf is what LibreOffice made: a PDF of n pages.
func pdfOf(n int) *office.Output {
	return &office.Output{Data: numberedPDF(1, n), Pages: n}
}

// lectureDeck is a deck of n slides, a picture on the first and the third,
// notes on the first and every third.
func lectureDeck(n int) []byte {
	var slides []doctexttest.Slide
	for i := 1; i <= n; i++ {
		s := doctexttest.Slide{Title: fmt.Sprintf("Slide %d", i), Body: []doctexttest.Bullet{{Text: fmt.Sprintf("point %d", i)}}}
		if i == 1 || i == 3 {
			s.Images = 1
		}
		if i == 1 || i%3 == 0 {
			s.Notes = fmt.Sprintf("Say %d.", i)
		}
		slides = append(slides, s)
	}
	return doctexttest.PPTX(slides...)
}

// getDoc calls document_get with args on r, whose Core serves the file at
// url, and returns the result's content and the file part, if any.
func getDoc(t *testing.T, r Runner, args string) (map[string]any, *llm.File) {
	t.Helper()
	parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", args)})
	if err != nil {
		t.Fatal(err)
	}
	if parts[0].IsError || len(parts[0].Content) > r.withDefaults().MaxResultBytes {
		t.Fatalf("result %+v", parts[0])
	}
	var file *llm.File
	if len(parts) == 2 {
		file = parts[1].File
	}
	return contentOf(t, parts[0]), file
}

func officeRunner(t *testing.T, data []byte, contentType string, o *stubOffice) Runner {
	srv, _ := deckServer(t, data)
	return Runner{Client: core.NewClient(versionedCore(srv.URL+"/file", contentType, len(data))), Files: NewHTTPFetcher(srv.Client()),
		Texts: NewTextCache(0), Office: o}
}

func noteOf(rec map[string]any) string { s, _ := rec["note"].(string); return s }

func wantNote(t *testing.T, rec map[string]any, says ...string) {
	t.Helper()
	for _, s := range says {
		if !strings.Contains(noteOf(rec), s) {
			t.Errorf("the note %q does not say %q", noteOf(rec), s)
		}
	}
}

const firstPart = `{"document_id":"` + docID + `"}`

// TestConvertedDeckAsItsPDF: a deck, to a model that takes files, is
// LibreOffice's PDF of it, whole when it has at most a part's slides, its
// speaker notes beside it as file_text; file_part does not apply to it.
func TestConvertedDeckAsItsPDF(t *testing.T) {
	deck := lectureDeck(3)
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
	r := officeRunner(t, deck, doctexttest.PPTXType, o)
	r.FileInput = true
	c, file := getDoc(t, r, firstPart)
	rec := c["file"].(map[string]any)
	if rec["given_as"] != givenFile || rec["converted_to"] != "pdf" || rec["content_type"] != doctexttest.PPTXType || rec["part"] != nil {
		t.Errorf("record %v", rec)
	}
	wantNote(t, rec, "LibreOffice converted it to PDF, which is given: its 3 slides as they look, pictures, charts and drawings with them",
		"file_text holds the speaker notes of these slides, which the PDF does not show")
	if file == nil || file.MIME != "application/pdf" || file.Name != "Week 3.pdf" || string(file.Data) != string(pdfOf(3).Data) {
		t.Fatalf("file part %+v", file)
	}
	if c["file_text"] != "## Slide 1\nNotes: Say 1.\n\n## Slide 3\nNotes: Say 3." {
		t.Errorf("file_text %q", c["file_text"])
	}
	c, _ = getDoc(t, r, `{"document_id":"`+docID+`","file_part":2}`)
	wantNote(t, c["file"].(map[string]any), "file_part does not apply: the file itself is given, whole")
	converted, _, _ := o.record()
	if o.badSum || len(converted) != 2 || converted[0] != "pptx>pdf" {
		t.Errorf("converted %v, sums right %v", converted, !o.badSum)
	}
}

// TestConvertedDeckInParts: a deck of more slides than a part holds is
// given in parts of its slides, each a PDF of its own, cut from
// LibreOffice's; the first says how many there are and what each holds;
// next_part reads the next, each with the notes of its own slides, until
// the last; a part past the last is not given. The provider's own limit on
// pages makes the parts smaller.
func TestConvertedDeckInParts(t *testing.T) {
	deck := lectureDeck(25)
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(25)}}
	r := officeRunner(t, deck, doctexttest.PPTXType, o)
	r.FileInput = true
	args := firstPart
	var holds []string
	for n := 1; ; n++ {
		c, file := getDoc(t, r, args)
		rec := c["file"].(map[string]any)
		if rec["given_as"] != givenFile || rec["part"] != float64(n) || rec["parts"] != float64(3) || file == nil {
			t.Fatalf("part %d: %v", n, rec)
		}
		holds = append(holds, rec["part_holds"].(string))
		first := (n-1)*10 + 1
		if !strings.Contains(string(file.Data), fmt.Sprintf("page %d", first)) || file.Name != "Week 3 ("+holds[n-1]+").pdf" {
			t.Errorf("part %d's file %s", n, file.Name)
		}
		text, _ := c["file_text"].(string)
		if !strings.HasPrefix(text, fmt.Sprintf("## Slide %d\n", map[int]int{1: 1, 2: 12, 3: 21}[n])) || strings.Contains(text, "Say 1.") != (n == 1) {
			t.Errorf("part %d's notes %q", n, text)
		}
		if n == 1 {
			wantNote(t, rec, "it is given in 3 parts of 10 slides each", "this is part 1, slides 1–10", "of the others, 2 is slides 11–20, 3 is slides 21–25",
				"to read part 2, call document_get with next_part's arguments")
		}
		next, ok := rec["next_part"].(map[string]any)
		if !ok {
			wantNote(t, rec, "it is the last")
			break
		}
		raw, _ := json.Marshal(next["arguments"])
		args = string(raw)
	}
	if strings.Join(holds, "; ") != "slides 1–10; slides 11–20; slides 21–25" {
		t.Errorf("parts hold %v", holds)
	}
	_, ranges, _ := o.record()
	if fmt.Sprint(ranges) != "[[1 10] [11 20] [21 25]]" {
		t.Errorf("ranges cut %v", ranges)
	}
	c, file := getDoc(t, r, `{"document_id":"`+docID+`","file_part":4}`)
	if rec := c["file"].(map[string]any); rec["given_as"] != givenNot || file != nil {
		t.Errorf("part 4: %v", rec)
	} else {
		wantNote(t, rec, "there is no part 4; ask for file_part from 1 to 3")
	}

	r.PDFLimits = llm.FileLimits{PDFPages: 5}
	c, _ = getDoc(t, r, firstPart)
	if rec := c["file"].(map[string]any); rec["parts"] != float64(5) || rec["part_holds"] != "slides 1–5" {
		t.Errorf("within a provider's 5 pages: %v", rec)
	}
}

// TestPDFInParts: a PDF of the course's own, of more pages than a part
// holds, is given in parts of its pages too, where the runtime cuts PDFs,
// converting nothing; and in parts within its provider's limit on pages,
// where it would have been text before.
func TestPDFInParts(t *testing.T) {
	book := numberedPDF(1, 12)
	o := &stubOffice{off: "LibreOffice is not installed"}
	r := officeRunner(t, book, "application/pdf", o)
	r.FileInput = true
	c, file := getDoc(t, r, firstPart)
	rec := c["file"].(map[string]any)
	if rec["given_as"] != givenFile || rec["part_holds"] != "pages 1–10" || rec["parts"] != float64(2) || rec["converted_to"] != nil || file == nil {
		t.Errorf("record %v", rec)
	}
	wantNote(t, rec, "it is given in 2 parts of 10 pages each", "of the others, 2 is pages 11–12")
	c, _ = getDoc(t, r, `{"document_id":"`+docID+`","file_part":2}`)
	if rec := c["file"].(map[string]any); rec["part_holds"] != "pages 11–12" || rec["next_part"] != nil {
		t.Errorf("part 2: %v", rec)
	}
	r.PDFLimits = llm.FileLimits{PDFPages: 4}
	c, _ = getDoc(t, r, firstPart)
	if rec := c["file"].(map[string]any); rec["given_as"] != givenFile || rec["parts"] != float64(3) {
		t.Errorf("past its provider's pages: %v", rec)
	}
	o.noCuts = true
	c, _ = getDoc(t, r, firstPart)
	if rec := c["file"].(map[string]any); rec["given_as"] != givenText || rec["parts"] != nil {
		t.Errorf("past its provider's pages, with nothing to cut it: %v", rec)
	}
	r.PDFLimits = llm.FileLimits{}
	c, file = getDoc(t, r, firstPart)
	if rec := c["file"].(map[string]any); rec["given_as"] != givenFile || rec["part"] != nil || file == nil || len(file.Data) != len(book) {
		t.Errorf("nothing to cut it: %v", rec)
	}
	if converted, _, _ := o.record(); len(converted) != 0 {
		t.Errorf("converted %v", converted)
	}
}

// ocrOfSlides is what OCR read of the slides picked out: slide 1's
// screenshot, and nothing on slide 3.
func ocrOfSlides() ocr.State {
	return done("## Page 1\nfor i in range(n):\n    swap(a, i)\n\n## Page 2\n[no text found on this page]",
		store.OCRSection{N: 1}, store.OCRSection{N: 2, Offset: 45})
}

// TestConvertedDeckTextWithOCR: a deck, to a model that takes no files, is
// the runtime's text of its slides and notes, and what OCR read of the
// slides that show pictures, after each one's own text and before its
// notes; OCR is asked once, for those slides alone, picked out of
// LibreOffice's PDF, by a checksum of the runtime's own; the parts after
// read what was kept. While OCR reads them, the slides are given without
// it, and the note says to ask again; with no OCR, the note says so.
func TestConvertedDeckTextWithOCR(t *testing.T) {
	deck := lectureDeck(3)
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
	r := officeRunner(t, deck, doctexttest.PPTXType, o)
	var picked []byte
	fo := &fakeOCR{respond: func(n int, data func(context.Context) ([]byte, error)) ocr.State {
		picked, _ = data(context.Background())
		return ocrOfSlides()
	}}
	r.OCR = fo
	c, file := getDoc(t, r, firstPart)
	rec := c["file"].(map[string]any)
	want := "## Slide 1: Slide 1\n- point 1\n[image]\n" + ocrMark + "\nfor i in range(n):\n    swap(a, i)\nNotes: Say 1.\n\n" +
		"## Slide 2: Slide 2\n- point 2\n\n## Slide 3: Slide 3\n- point 3\n[image]\nNotes: Say 3."
	if file != nil || rec["given_as"] != givenText || rec["extracted_from"] != "pptx" || c["file_text"] != want {
		t.Fatalf("record %v\nfile_text %q", rec, c["file_text"])
	}
	wantNote(t, rec, "its 2 images are named, as [image] and [chart]", "the text in the pictures and charts of slides 1 and 3 is what the runtime's OCR read")
	if fo.times() != 1 || fo.asked[0] != (ocrAsked{derivedSum("slides", checksum(deck), []int{1, 3}), ocr.PDF, 2}) {
		t.Errorf("OCR was asked %+v", fo.asked)
	}
	if converted, _, picks := o.record(); fmt.Sprint(converted, picks) != "[pptx>pdf] [[1 3]]" || string(picked) != "%PDF-picked" {
		t.Errorf("converted %v, picked %v, OCR given %q", converted, picks, picked)
	}
	c, _ = getDoc(t, r, firstPart)
	if c["file_text"] != want || fo.times() != 1 {
		t.Errorf("asked again: OCR asked %d times, file_text %q", fo.times(), c["file_text"])
	}

	pending := &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State {
		return ocr.State{Status: ocr.StatusPending}
	}}
	r = officeRunner(t, deck, doctexttest.PPTXType, o)
	r.OCR = pending
	c, _ = getDoc(t, r, firstPart)
	rec = c["file"].(map[string]any)
	if rec["given_as"] != givenText || rec["ocr"] != OCRInProgress || rec["ask_again"] == nil || strings.Contains(c["file_text"].(string), ocrMark) {
		t.Errorf("while OCR reads the pictures: %v", rec)
	}
	wantNote(t, rec, "the runtime is reading the text in the pictures and charts of slides 1 and 3 now (OCR)")

	r = officeRunner(t, deck, doctexttest.PPTXType, o)
	c, _ = getDoc(t, r, firstPart)
	rec = c["file"].(map[string]any)
	if rec["given_as"] != givenText || rec["ocr"] != nil {
		t.Errorf("with no OCR: %v", rec)
	}
	wantNote(t, rec, "only named, as [image] and [chart]", "the text in its pictures and charts is not read: the runtime has no OCR here")
}

// TestConvertedDeckPastItsProvider: a deck whose PDF is larger than the
// model's provider takes is its text, with OCR of its pictures, and the
// note says why.
func TestConvertedDeckPastItsProvider(t *testing.T) {
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
	r := officeRunner(t, lectureDeck(3), doctexttest.PPTXType, o)
	r.FileInput, r.PDFLimits = true, llm.FileLimits{PDFBytes: 100}
	r.OCR = &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State { return ocrOfSlides() }}
	c, file := getDoc(t, r, firstPart)
	rec := c["file"].(map[string]any)
	if file != nil || rec["given_as"] != givenText || !strings.Contains(c["file_text"].(string), ocrMark) {
		t.Errorf("record %v", rec)
	}
	wantNote(t, rec, "it is given as the runtime's text of it, since its PDF is", "more than the 100 bytes this model takes as a file")
}

// TestConversionNotDone: while a deck's or a Word document's PDF is made,
// a model that takes files is given its text, told to ask again to see its
// pages, and its pictures are not read by OCR, as the PDF will show them;
// where LibreOffice cannot convert it, the text, and why. Where the text
// needs the conversion too (an older Word file), nothing, and why.
func TestConversionNotDone(t *testing.T) {
	state := office.State{Status: office.StatusPending}
	o := &stubOffice{convert: func(office.Format, office.Target) office.State { return state }}
	fo := &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State { return ocrOfSlides() }}
	r := officeRunner(t, lectureDeck(3), doctexttest.PPTXType, o)
	r.FileInput, r.OCR = true, fo
	c, file := getDoc(t, r, firstPart)
	rec := c["file"].(map[string]any)
	if file != nil || rec["given_as"] != givenText || rec["conversion"] != ConversionInProgress || rec["ask_again"] == nil || fo.times() != 0 {
		t.Errorf("while its PDF is made: %v", rec)
	}
	wantNote(t, rec, "since the runtime is converting it to PDF now (LibreOffice)", "in a minute or so to see its pages as they look")

	state = office.State{Status: office.StatusFailed, Why: "LibreOffice could not open it: it is damaged"}
	handout := doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "Lab 3", Heading: 1}}})
	r = officeRunner(t, handout, doctexttest.DOCXType, o)
	r.FileInput = true
	c, _ = getDoc(t, r, firstPart)
	rec = c["file"].(map[string]any)
	if rec["given_as"] != givenText || rec["conversion"] != ConversionFailed || c["file_text"] != "# Lab 3" || rec["ask_again"] != nil {
		t.Errorf("where it cannot be converted: %v", rec)
	}
	wantNote(t, rec, "LibreOffice could not convert it to PDF: LibreOffice could not open it: it is damaged")

	state = office.State{Status: office.StatusPending}
	r = officeRunner(t, cfbOf("WordDocument"), "application/msword", o)
	c, _ = getDoc(t, r, firstPart)
	rec = c["file"].(map[string]any)
	if rec["given_as"] != givenNot || rec["conversion"] != ConversionInProgress || rec["ask_again"] == nil {
		t.Errorf("an older Word file while its PDF is made: %v", rec)
	}
	wantNote(t, rec, "the runtime is converting it to PDF now (LibreOffice); call document_get again with ask_again's arguments in a minute or so")
}

// cfbOf is an older Office file whose container names the stream.
func cfbOf(stream string) []byte {
	b := append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 504)...)
	for i := 0; i < len(stream); i++ {
		b = append(b, stream[i], 0)
	}
	return b
}

// TestConvertedOlderFormats: a PowerPoint 97 deck is read in LibreOffice's
// PowerPoint form of it, and its PDF given with that form's notes; a Word
// 97 document is read in LibreOffice's PDF of it, page by page, or, where
// that has no text, as OCR recognizes it, by a checksum of the runtime's
// own; an Excel 97 workbook in LibreOffice's Excel form, to any model; and
// each is known by what it holds when Core gives it no type.
func TestConvertedOlderFormats(t *testing.T) {
	deck := lectureDeck(3)
	o := &stubOffice{out: map[office.Target]*office.Output{
		office.ToPDF:  pdfOf(3),
		office.ToPPTX: {Data: deck},
		office.ToXLSX: {Data: doctexttest.XLSX(doctexttest.Sheet{Name: "Quiz", Rows: [][]any{{"Q1", 5}}})},
	}}
	ppt := cfbOf("PowerPoint Document")
	for _, ct := range []string{"application/vnd.ms-powerpoint", ""} {
		r := officeRunner(t, ppt, ct, o)
		c, _ := getDoc(t, r, firstPart)
		rec := c["file"].(map[string]any)
		if rec["given_as"] != givenText || rec["converted_to"] != "pptx" || rec["content_type"] != "application/vnd.ms-powerpoint" ||
			!strings.HasPrefix(c["file_text"].(string), "## Slide 1: Slide 1") {
			t.Errorf("a PowerPoint 97 deck (%q) to a model that takes no files: %v", ct, rec)
		}
		wantNote(t, rec, "LibreOffice converted it to PowerPoint's format: the runtime's text of its 3 slides")
		r.FileInput = true
		c, file := getDoc(t, r, firstPart)
		if file == nil || c["file_text"] != "## Slide 1\nNotes: Say 1.\n\n## Slide 3\nNotes: Say 3." {
			t.Errorf("a PowerPoint 97 deck to one that does: %v %q", c["file"], c["file_text"])
		}
	}

	r := officeRunner(t, cfbOf("WordDocument"), "", o)
	c, _ := getDoc(t, r, firstPart)
	rec := c["file"].(map[string]any)
	if rec["given_as"] != givenText || rec["converted_to"] != "pdf" || rec["extracted_from"] != "pdf" || c["file_text"] != "## Page 1\npage 1\n\n## Page 2\npage 2\n\n## Page 3\npage 3" {
		t.Errorf("a Word 97 document: %v %q", rec, c["file_text"])
	}
	wantNote(t, rec, "LibreOffice converted it to PDF: the runtime's text of its 3 pages")

	scan := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: {Data: scanned, Pages: 2}}}
	var gotPDF []byte
	fo := &fakeOCR{respond: func(_ int, data func(context.Context) ([]byte, error)) ocr.State {
		gotPDF, _ = data(context.Background())
		return done("## Page 1\n期中考試範圍", store.OCRSection{N: 1})
	}}
	odt := officeRunner(t, cfbOf("WordDocument"), "application/msword", scan)
	odt.OCR = fo
	c, _ = getDoc(t, odt, firstPart)
	rec = c["file"].(map[string]any)
	if rec["given_as"] != givenText || rec["extracted_from"] != ExtractedOCR || c["file_text"] != "## Page 1\n期中考試範圍" ||
		fo.asked[0].sum != derivedSum("pdf", checksum(cfbOf("WordDocument")), nil) || string(gotPDF) != string(scanned) {
		t.Errorf("a Word 97 scan: %v, OCR asked %+v", rec, fo.asked)
	}

	for _, fileInput := range []bool{false, true} {
		r := officeRunner(t, cfbOf("Workbook"), "application/vnd.ms-excel", o)
		r.FileInput = fileInput
		c, file := getDoc(t, r, firstPart)
		rec := c["file"].(map[string]any)
		if file != nil || rec["given_as"] != givenText || rec["converted_to"] != "xlsx" || c["file_text"] != "## Sheet 1: Quiz\nQ1,5" {
			t.Errorf("an Excel 97 workbook: %v %q", rec, c["file_text"])
		}
		wantNote(t, rec, "LibreOffice converted it to Excel's format: the runtime's text of its 1 sheet")
	}
	if o.badSum {
		t.Error("a conversion was asked with a checksum not of the file")
	}
}

// TestConversionOff: where the runtime converts nothing, a deck is its text
// as before, an older Office file is not given, nor an OpenDocument one,
// and RTF is its own text.
func TestConversionOff(t *testing.T) {
	o := &stubOffice{off: "LibreOffice is not installed", noCuts: true}
	r := officeRunner(t, lectureDeck(3), doctexttest.PPTXType, o)
	r.FileInput = true
	c, file := getDoc(t, r, firstPart)
	if rec := c["file"].(map[string]any); file != nil || rec["given_as"] != givenText || rec["converted_to"] != nil {
		t.Errorf("a deck: %v", rec)
	}
	c, _ = getDoc(t, officeRunner(t, cfbOf("PowerPoint Document"), "", o), firstPart)
	wantNote(t, c["file"].(map[string]any), "an older Office format")
	c, _ = getDoc(t, officeRunner(t, []byte("PK\x03\x04odt"), "application/vnd.oasis.opendocument.text", o), firstPart)
	wantNote(t, c["file"].(map[string]any), "application/vnd.oasis.opendocument.text files are not read here")
	c, _ = getDoc(t, officeRunner(t, []byte(`{\rtf1 Hello}`), "text/rtf", o), firstPart)
	if c["file_text"] != `{\rtf1 Hello}` {
		t.Errorf("RTF: %v", c)
	}
	if converted, _, _ := o.record(); len(converted) != 0 {
		t.Errorf("converted %v", converted)
	}
}

// TestConvertedPasswordProtected: a Word file encrypted with a password is
// given to no model, and not converted.
func TestConvertedPasswordProtected(t *testing.T) {
	o := &stubOffice{}
	r := officeRunner(t, password, doctexttest.DOCXType, o)
	r.FileInput = true
	c, file := getDoc(t, r, firstPart)
	if rec := c["file"].(map[string]any); file != nil || rec["given_as"] != givenNot {
		t.Errorf("record %v", rec)
	} else {
		wantNote(t, rec, "it is password-protected; ask for a copy without a password")
	}
	if converted, _, _ := o.record(); len(converted) != 0 {
		t.Errorf("converted %v", converted)
	}
}

// TestConvertedTextKept: a long deck read in parts by a model that takes
// no files is fetched and read once, converting nothing when no slide shows
// a picture.
func TestConvertedTextKept(t *testing.T) {
	deck := longDeck()
	o := &stubOffice{}
	srv, hits := deckServer(t, deck)
	r := Runner{Client: core.NewClient(versionedCore(srv.URL+"/deck", doctexttest.PPTXType, len(deck))), Files: NewHTTPFetcher(srv.Client()),
		Texts: NewTextCache(0), Office: o}
	recs, text := readAll(t, r, firstPart)
	res, err := doctext.Extract(context.Background(), deck, doctext.PPTX, doctext.Limits{})
	if err != nil || text != res.Text || len(recs) < 2 {
		t.Fatalf("%d parts, the whole text %v", len(recs), text == res.Text)
	}
	if hits.Load() != 1 {
		t.Errorf("fetched %d times", hits.Load())
	}
	if converted, _, _ := o.record(); len(converted) != 0 {
		t.Errorf("converted %v", converted)
	}
}
