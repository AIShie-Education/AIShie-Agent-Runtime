package ocr

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/doctext"
)

// The lines of the scanned page the real test reads: a course's notice in
// traditional Chinese, in simplified, and in English.
var scannedLines = []string{
	"期中考試範圍：第一章到第五章",
	"期中考试范围：第一章到第五章",
	"Midterm: chapters 1 to 5",
}

// realEngine is an Engine on the programs installed here, or the test is
// skipped: only when tesseract (with chi_sim, chi_tra and eng),
// pdftoppm or prlimit is not installed.
func realEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	cfg.TempDir = t.TempDir()
	e, err := NewEngine(t.Context(), cfg)
	if errors.Is(err, ErrUnavailable) {
		if os.Getenv("OCR_REQUIRED") == "1" {
			t.Fatalf("OCR_REQUIRED is set, and OCR is not available: %v", err)
		}
		t.Skipf("the OCR programs are not installed: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestRecognizeScannedCJK runs the real programs on a scanned page drawn
// here: a PDF of the page and a blank one, which doctext finds no text in,
// rendered by pdftoppm and read by tesseract in Chinese and English; and
// the page as an image. The notice is read in both scripts and in English,
// the blank page is said to hold no text, each page is under its heading,
// progress is told page by page, and the private directory is gone after.
func TestRecognizeScannedCJK(t *testing.T) {
	e := realEngine(t, Config{})
	page := drawPage(t, 3, scannedLines...)
	blank := drawPage(t, 3)
	pdf := scannedPDF(t, page, blank)

	res, err := doctext.Extract(context.Background(), pdf, doctext.PDF, doctext.Limits{})
	if err != nil || res.Unreadable != doctext.UnreadableNoText {
		t.Fatalf("doctext's reading of the scan: %+v %v; want it judged no_text", res, err)
	}
	pages, err := doctext.PDFPages(context.Background(), pdf, doctext.Limits{})
	if err != nil || pages != 2 {
		t.Fatalf("pages %d %v", pages, err)
	}

	var mu sync.Mutex
	var told [][2]int
	start := time.Now()
	got, err := e.Recognize(t.Context(), pdf, PDF, pages, func(done, of int) {
		mu.Lock()
		told = append(told, [2]int{done, of})
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recognized 2 pages in %s:\n%s", time.Since(start).Round(time.Millisecond), got.Text)
	if got.Pages != 2 || got.Of != 2 || got.Empty != 1 || got.Failed != 0 || len(got.Sections) != 2 {
		t.Errorf("result %+v", got)
	}
	for i, s := range got.Sections {
		if s.N != i+1 || !strings.HasPrefix(got.Text[s.Offset:], "## Page ") {
			t.Errorf("section %d: %+v", i, s)
		}
	}
	first := got.Text[:got.Sections[1].Offset]
	wantSome(t, first, "the traditional line", "期中考試範圍", "第一章到第五章")
	// With both scripts' data, tesseract may give a simplified character
	// its traditional form (试 as 試): the simplified line is read as a
	// line of its own, 范 as it is.
	wantSome(t, first, "the simplified line", "范")
	if n := strings.Count(strings.Join(strings.Fields(first), ""), "第一章到第五章"); n != 2 {
		t.Errorf("the chapters are read %d times, want in both lines", n)
	}
	wantSome(t, first, "the English line", "Midterm", "chapters")
	if second := got.Text[got.Sections[1].Offset:]; !strings.Contains(second, noTextOnPage) {
		t.Errorf("the blank page: %q", second)
	}
	if len(told) != 2 || told[0] != [2]int{1, 2} || told[1] != [2]int{2, 2} {
		t.Errorf("progress told %v", told)
	}

	img, err := e.Recognize(t.Context(), pngOf(t, page), Image, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantSome(t, img.Text, "the image", "期中考試範圍", "Midterm")
	if img.Pages != 1 || len(img.Sections) != 0 {
		t.Errorf("the image's result %+v", img)
	}

	left, err := os.ReadDir(e.cfg.TempDir)
	if err != nil || len(left) != 0 {
		t.Errorf("left behind in the temporary directory: %v %v", left, err)
	}
}

// wantSome fails unless text holds every one of want, where OCR may have
// put spaces between characters.
func wantSome(t *testing.T, text, what string, want ...string) {
	t.Helper()
	squeezed := strings.Join(strings.Fields(text), "")
	for _, w := range want {
		if !strings.Contains(squeezed, strings.Join(strings.Fields(w), "")) {
			t.Errorf("%s: %q not recognized in %q", what, w, text)
		}
	}
}

// TestRecognizeRealLimits holds the real programs to their limits: a page
// that takes longer than its time is said so and the rest go on, and an
// image past MaxPixels is not read at all.
func TestRecognizeRealLimits(t *testing.T) {
	e := realEngine(t, Config{PageTimeout: time.Millisecond, MaxPixels: 1000})
	if _, err := e.Recognize(t.Context(), pngOf(t, drawPage(t, 3, "期中")), Image, 0, nil); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an image of 8.7 million pixels past 1000: %v", err)
	}
	pdf := scannedPDF(t, drawPage(t, 3, "期中"))
	if _, err := e.Recognize(t.Context(), pdf, PDF, 1, nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("a page that has 1 ms: %v", err)
	}
}
