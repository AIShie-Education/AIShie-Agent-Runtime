package toolset

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

// realOffice is the worker's conversion on the LibreOffice and poppler
// installed here, or the test is skipped: only when they or prlimit are
// not installed, and never with OFFICE_PDF_REQUIRED=1, as the image's test
// runs it.
func realOffice(t *testing.T) *office.Service {
	t.Helper()
	cfg := office.Config{TempDir: t.TempDir()}
	c, err := office.NewConverter(t.Context(), cfg)
	var p *office.Pager
	if err == nil {
		p, err = office.NewPager(cfg)
	}
	if errors.Is(err, office.ErrUnavailable) {
		if os.Getenv("OFFICE_PDF_REQUIRED") == "1" {
			t.Fatalf("OFFICE_PDF_REQUIRED is set, and the conversion is not available: %v", err)
		}
		t.Skipf("LibreOffice or poppler is not installed: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := office.NewService(ctx, office.ServiceOptions{Converter: c, Pager: p, Config: cfg})
	t.Cleanup(func() { cancel(); s.Wait() })
	return s
}

// realOCR is the worker's OCR on the programs installed here, or the test
// is skipped, as the ocr package's own are: never with OCR_REQUIRED=1.
func realOCR(t *testing.T) *ocr.Service {
	t.Helper()
	cfg := ocr.Config{TempDir: t.TempDir(), Wait: 20 * time.Second}
	e, err := ocr.NewEngine(t.Context(), cfg)
	if errors.Is(err, ocr.ErrUnavailable) {
		if os.Getenv("OCR_REQUIRED") == "1" {
			t.Fatalf("OCR_REQUIRED is set, and OCR is not available: %v", err)
		}
		t.Skipf("the OCR programs are not installed: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := ocr.NewService(ctx, ocr.ServiceOptions{Recognizer: e, Config: cfg, Store: memstore.New(), Holder: "test"})
	t.Cleanup(func() { cancel(); s.Wait() })
	return s
}

// screenshot is a picture of lines of text, as a slide's screenshot of
// code is: the PDF of them rendered by pdftoppm, which OCR's own tests
// have shown there.
func screenshot(t *testing.T, lines ...string) []byte {
	t.Helper()
	pdftoppm, err := exec.LookPath("pdftoppm")
	if err != nil {
		t.Skip("pdftoppm is not installed")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, doctexttest.PDFLines(lines...), 0o600); err != nil {
		t.Fatal(err)
	}
	// The lines, at the top left of the page, cut as wide for their height
	// as the slide's frame for the picture is (doctexttest's), so that
	// they are drawn large and unstretched.
	if out, err := exec.Command(pdftoppm, "-r", "200", "-png", "-singlefile", "-x", "180", "-y", "0", "-W", "440", "-H", "357", in,
		filepath.Join(dir, "shot")).CombinedOutput(); err != nil {
		t.Fatalf("pdftoppm: %v %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "shot.png"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRealDeckToModels converts a deck for real: Chinese slides, one with
// a screenshot of code, speaker notes, twelve slides in all. A model that
// takes files is given LibreOffice's PDF in parts of ten slides and two,
// each a PDF whose Chinese reads, with the notes of its own slides beside
// it; a model that takes none is given the slides' text and notes, and
// what OCR read of the screenshot's slide, which the slide's own text
// does not hold.
func TestRealDeckToModels(t *testing.T) {
	conv := realOffice(t)
	shot := screenshot(t, "def merge(left, right):", "    return sorted(left + right)")
	slides := []doctexttest.Slide{
		{Title: "第三週：合併排序", Body: []doctexttest.Bullet{{Text: "把串列分成兩半"}}, Notes: "先問學生：什麼是穩定排序？", Picture: shot},
	}
	for i := 2; i <= 12; i++ {
		slides = append(slides, doctexttest.Slide{Title: "Step " + string(rune('A'+i-2)), Body: []doctexttest.Bullet{{Text: "排序的複雜度"}}})
	}
	deck := doctexttest.PPTX(slides...)
	r := officeRunner(t, deck, doctexttest.PPTXType, nil)
	r.Office, r.FileInput = conv, true
	c, file := getDoc(t, r, firstPart)
	rec := c["file"].(map[string]any)
	if file == nil || rec["given_as"] != givenFile || rec["converted_to"] != "pdf" || rec["parts"] != float64(2) || rec["part_holds"] != "slides 1–10" {
		t.Fatalf("to a model that takes files: %v", rec)
	}
	res, err := doctext.Extract(context.Background(), file.Data, doctext.PDF, doctext.Limits{})
	if err != nil || res.Parts != 10 || !strings.Contains(squeeze(res.Text), "第三週：合併排序") || res.Unreadable != "" {
		t.Errorf("part 1's PDF: %v, %d pages\n%s", err, res.Parts, res.Text)
	}
	if c["file_text"] != "## Slide 1\nNotes: 先問學生：什麼是穩定排序？" {
		t.Errorf("part 1's notes %q", c["file_text"])
	}
	c, file = getDoc(t, r, `{"document_id":"`+docID+`","file_part":2}`)
	if res, err := doctext.Extract(context.Background(), file.Data, doctext.PDF, doctext.Limits{}); err != nil || res.Parts != 2 ||
		!strings.Contains(res.Text, "Step K") || c["file_text"] != nil {
		t.Errorf("part 2: %v %v", c["file"], err)
	}

	r.FileInput, r.OCR = false, realOCR(t)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		c, _ = getDoc(t, r, firstPart)
		rec = c["file"].(map[string]any)
		if rec["ocr"] != OCRInProgress || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	text, _ := c["file_text"].(string)
	t.Logf("to a model that takes no files:\n%s", text)
	if rec["given_as"] != givenText || rec["ocr"] != nil || !strings.HasPrefix(text, "## Slide 1: 第三週：合併排序\n- 把串列分成兩半\n[image]\n"+ocrMark+"\n") {
		t.Fatalf("to a model that takes no files: %v", rec)
	}
	slide1 := text[:strings.Index(text, "## Slide 2")]
	if !strings.Contains(squeeze(slide1), "sorted(left+right)") || !strings.HasSuffix(strings.TrimSpace(slide1), "Notes: 先問學生：什麼是穩定排序？") {
		t.Errorf("slide 1 with what OCR read of it:\n%s", slide1)
	}
}

// TestRealOlderDocument converts a Word 97 document LibreOffice saved from
// a Word one, for a model that takes no files: the runtime's text of
// LibreOffice's PDF of it, its Chinese read.
func TestRealOlderDocument(t *testing.T) {
	conv := realOffice(t)
	doc := doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "期中考試範圍", Heading: 1}, {Text: "第一章到第五章"}}})
	pdf := conv.Convert(t.Context(), checksum(doc), office.Format{Ext: "docx", Family: office.Document, OOXML: true}, office.ToPDF, doc)
	if pdf.Status != office.StatusDone {
		t.Fatalf("the Word document's PDF: %+v", pdf)
	}
	r := officeRunner(t, doc, doctexttest.DOCXType, nil)
	r.Office, r.FileInput, r.PDFLimits = conv, true, llm.FileLimits{PDFBytes: 1 << 20, PDFPages: 100}
	c, file := getDoc(t, r, firstPart)
	if rec := c["file"].(map[string]any); file == nil || rec["given_as"] != givenFile || rec["part"] != nil {
		t.Errorf("a Word document to a model that takes files: %v", rec)
	}
	c, _ = getDoc(t, officeRunnerWith(t, doc, "", conv), firstPart)
	if rec := c["file"].(map[string]any); rec["given_as"] != givenText || rec["extracted_from"] != "docx" {
		t.Errorf("a Word document of no type, to one that takes none: %v", rec)
	}
}

// officeRunnerWith is officeRunner on a conversion of the test's.
func officeRunnerWith(t *testing.T, data []byte, contentType string, o Office) Runner {
	r := officeRunner(t, data, contentType, nil)
	r.Office = o
	return r
}

func squeeze(s string) string { return strings.Join(strings.Fields(s), "") }
