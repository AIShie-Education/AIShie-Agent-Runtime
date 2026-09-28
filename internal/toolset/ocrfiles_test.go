package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// fakeOCR answers as a test scripts, and records what it is asked.
type fakeOCR struct {
	off     string
	mu      sync.Mutex
	asked   []ocrAsked
	respond func(n int, data func(context.Context) ([]byte, error)) ocr.State
}

type ocrAsked struct {
	sum   string
	kind  ocr.Kind
	pages int
}

func (f *fakeOCR) Available() (bool, string) { return f.off == "", f.off }

func (f *fakeOCR) Text(ctx context.Context, sum string, kind ocr.Kind, pages int, data func(context.Context) ([]byte, error)) ocr.State {
	f.mu.Lock()
	f.asked = append(f.asked, ocrAsked{sum, kind, pages})
	n := len(f.asked)
	f.mu.Unlock()
	return f.respond(n, data)
}

func (f *fakeOCR) times() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

// done is a text OCR recognized.
func done(text string, sections ...store.OCRSection) ocr.State {
	return ocr.State{Status: ocr.StatusDone, Text: &store.OCRText{Status: store.OCRDone, Text: text, Sections: sections,
		Pages: max(len(sections), 1), PagesOf: max(len(sections), 1)}}
}

// scannedPages is what OCR recognizes of a scan of n pages: each under its
// heading, and long enough that the whole is read in parts.
func scannedPages(n int) (string, []store.OCRSection) {
	var b strings.Builder
	var secs []store.OCRSection
	for i := range n {
		secs = append(secs, store.OCRSection{N: i + 1, Offset: b.Len()})
		fmt.Fprintf(&b, "## Page %d\n", i+1)
		for j := range 30 {
			fmt.Fprintf(&b, "第%d頁第%d行：期中考試範圍 chapter %d \"review\"\n", i+1, j+1, j)
		}
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n\n"), secs
}

// swapServer serves one file, which a test may change, counting the
// fetches.
func swapServer(t *testing.T, data []byte) (*httptest.Server, *atomic.Pointer[[]byte], *atomic.Int32) {
	var served atomic.Pointer[[]byte]
	served.Store(&data)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(*served.Load())
	}))
	t.Cleanup(srv.Close)
	return srv, &served, &hits
}

// readAll reads a document's text as a model does, from the call given,
// part after part as next_part says, and returns the records and the text.
func readAll(t *testing.T, r Runner, args string) ([]map[string]any, string) {
	t.Helper()
	var recs []map[string]any
	var text strings.Builder
	for n := 1; ; n++ {
		if n > 20 {
			t.Fatal("more parts than the text makes")
		}
		parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", args)})
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) != 1 || parts[0].IsError || len(parts[0].Content) > DefaultMaxResultBytes {
			t.Fatalf("part %d: %+v", n, parts)
		}
		rec := fileRecordOf(t, parts[0])
		recs = append(recs, rec)
		s, _ := contentOf(t, parts[0])["file_text"].(string)
		text.WriteString(s)
		next, ok := rec["next_part"].(map[string]any)
		if !ok {
			return recs, text.String()
		}
		raw, _ := json.Marshal(next["arguments"])
		args = string(raw)
	}
}

// TestOCRScanReadInParts: a scanned PDF, to a model that takes no files,
// is given as what OCR recognized of it, marked as OCR's and said to hold
// recognition errors, in parts cut where its pages begin; OCR is asked
// once, with the file's checksum, its kind and its pages as doctext
// counted them, and the file is fetched once.
func TestOCRScanReadInParts(t *testing.T) {
	want, secs := scannedPages(30)
	if len(want) < 40<<10 {
		t.Fatalf("the scan's text is %d bytes, too short for parts", len(want))
	}
	srv, _, hits := swapServer(t, scanned)
	o := &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State { return done(want, secs...) }}
	r := Runner{Client: core.NewClient(versionedCore(srv.URL+"/scan", "application/pdf", len(scanned))),
		Files: NewHTTPFetcher(srv.Client()), Texts: NewTextCache(0), OCR: o}
	recs, got := readAll(t, r, `{"document_id":"`+docID+`"}`)
	if got != want {
		t.Fatalf("the parts are not OCR's text: got %d bytes, want %d", len(got), len(want))
	}
	if len(recs) < 2 {
		t.Fatalf("%d parts", len(recs))
	}
	for i, rec := range recs {
		if rec["given_as"] != givenText || rec["extracted_from"] != "ocr" || rec["part"] != float64(i+1) || rec["ocr"] != nil {
			t.Errorf("part %d: %v", i+1, rec)
		}
		if !strings.HasPrefix(rec["part_holds"].(string), "page") {
			t.Errorf("part %d holds %v", i+1, rec["part_holds"])
		}
	}
	note := recs[0]["note"].(string)
	for _, s := range []string{"the runtime's OCR of its 30 pages", "(this model does not take files, and the PDF has no text to read",
		"may hold recognition errors", "given in "} {
		if !strings.Contains(note, s) {
			t.Errorf("the first part's note %q does not say %q", note, s)
		}
	}
	if o.times() != 1 || o.asked[0] != (ocrAsked{checksum(scanned), ocr.PDF, 2}) {
		t.Errorf("OCR was asked %+v, want once for the scan's 2 pages", o.asked)
	}
	if hits.Load() != 1 {
		t.Errorf("the scan was fetched %d times, want once", hits.Load())
	}
}

// TestOCRPendingThenDone: the first question about a scan is told its
// text is being recognized, how far it has come, and the call to ask
// again with, naming the version; OCR has the file's bytes. Asked again,
// the text is given, the file not fetched again.
func TestOCRPendingThenDone(t *testing.T) {
	srv, _, hits := swapServer(t, scanned)
	var gotBytes []byte
	o := &fakeOCR{respond: func(n int, data func(context.Context) ([]byte, error)) ocr.State {
		if n == 1 {
			gotBytes, _ = data(context.Background())
			return ocr.State{Status: ocr.StatusPending, Done: 1, Of: 2}
		}
		return done("## Page 1\n期中考試範圍：第一章到第五章\n\n## Page 2\nMidterm", store.OCRSection{N: 1}, store.OCRSection{N: 2, Offset: 51})
	}}
	r := Runner{Client: core.NewClient(versionedCore(srv.URL+"/scan", "application/pdf", len(scanned))),
		Files: NewHTTPFetcher(srv.Client()), Texts: NewTextCache(0), OCR: o}
	parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", `{"document_id":"`+docID+`"}`)})
	if err != nil {
		t.Fatal(err)
	}
	rec := fileRecordOf(t, parts[0])
	note, _ := rec["note"].(string)
	if rec["given_as"] != givenNot || rec["ocr"] != OCRInProgress || contentOf(t, parts[0])["file_text"] != nil ||
		!strings.Contains(note, "recognizing its text now (OCR), 1 of 2 pages done") || !strings.Contains(note, "ask_again") {
		t.Fatalf("the first question: %v", rec)
	}
	again, _ := rec["ask_again"].(map[string]any)
	a, _ := again["arguments"].(map[string]any)
	if again["tool"] != "document_get" || a["document_id"] != docID || a["version_id"] != version2 || len(a) != 2 {
		t.Fatalf("ask_again %v", again)
	}
	if string(gotBytes) != string(scanned) {
		t.Errorf("OCR was given %d bytes, not the scan", len(gotBytes))
	}

	raw, _ := json.Marshal(a)
	recs, text := readAll(t, r, string(raw))
	if len(recs) != 1 || recs[0]["given_as"] != givenText || recs[0]["extracted_from"] != "ocr" || recs[0]["ocr"] != nil ||
		!strings.Contains(text, "第一章到第五章") {
		t.Errorf("asked again: %v %q", recs, text)
	}
	if hits.Load() != 1 {
		t.Errorf("the scan was fetched %d times, want once", hits.Load())
	}
}

// TestOCRRefetchesTheSameFile: OCR asked for the bytes of a file whose
// reading was kept fetches it again, and refuses a file that is not the
// one read.
func TestOCRRefetchesTheSameFile(t *testing.T) {
	srv, served, hits := swapServer(t, scanned)
	var got [][]byte
	var errs []error
	o := &fakeOCR{respond: func(n int, data func(context.Context) ([]byte, error)) ocr.State {
		if n == 1 {
			return ocr.State{Status: ocr.StatusBusy, Why: "the runtime is recognizing other files just now"}
		}
		b, err := data(context.Background())
		got, errs = append(got, b), append(errs, err)
		return ocr.State{Status: ocr.StatusPending}
	}}
	r := Runner{Client: core.NewClient(versionedCore(srv.URL+"/scan", "application/pdf", len(scanned))),
		Files: NewHTTPFetcher(srv.Client()), Texts: NewTextCache(0), OCR: o}
	ask := func() map[string]any {
		parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", `{"document_id":"`+docID+`"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return fileRecordOf(t, parts[0])
	}
	if rec := ask(); rec["ocr"] != OCRBusy || rec["ask_again"] == nil || !strings.Contains(rec["note"].(string), "other files just now") {
		t.Errorf("busy: %v", rec)
	}
	ask()
	other := append([]byte(nil), garbled...)
	served.Store(&other)
	ask()
	if hits.Load() != 3 || len(got) != 2 || string(got[0]) != string(scanned) || errs[0] != nil || !errors.Is(errs[1], errNotTheFile) {
		t.Errorf("fetched %d times; OCR got %d files, errors %v", hits.Load(), len(got), errs)
	}
}

// TestOCRFiles: which files OCR is asked for, and what the model is told
// of each outcome.
func TestOCRFiles(t *testing.T) {
	fs := newFileServer(t)
	img := done("看板：Notice")
	img.Text.Kind = store.OCRImage
	tests := []struct {
		name      string
		path, ct  string
		fileInput bool
		pdfLimits llm.FileLimits
		ocr       *fakeOCR
		asked     ocr.Kind
		givenAs   string
		file      bool
		state     string
		notes     []string
		text      string
	}{
		{name: "an image, to a model that takes no files", path: "/photo.png", ct: "image/png", asked: ocr.Image, givenAs: givenText,
			notes: []string{"the runtime's OCR of the image (this model does not take files)", "recognition errors"}, text: "看板：Notice",
			ocr: &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State { return img }}},
		{name: "an image, to a model that takes files", path: "/photo.png", ct: "image/png", fileInput: true, givenAs: givenFile, file: true,
			ocr: &fakeOCR{}},
		{name: "a scan, to a model that takes files", path: "/scanned.pdf", ct: "application/pdf", fileInput: true,
			pdfLimits: llm.FileLimits{PDFPages: 10}, givenAs: givenFile, file: true, ocr: &fakeOCR{}},
		{name: "a PDF whose fonts do not map, to a model that takes files", path: "/garbled.pdf", ct: "application/pdf", fileInput: true,
			givenAs: givenFile, file: true, ocr: &fakeOCR{}},
		{name: "a PDF whose fonts do not map, to a model that takes no files", path: "/garbled.pdf", ct: "application/pdf", asked: ocr.PDF,
			givenAs: givenText, text: "期中考範圍", notes: []string{"(this model does not take files, and the PDF's text cannot be read"},
			ocr: &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State {
				return done("## Page 1\n期中考範圍", store.OCRSection{N: 1})
			}}},
		{name: "a scan past what its provider takes", path: "/scanned.pdf", ct: "application/pdf", fileInput: true,
			pdfLimits: llm.FileLimits{PDFPages: 1}, asked: ocr.PDF, givenAs: givenText, text: "Midterm",
			notes: []string{"the runtime's OCR of its 1 page (it has 2 pages, more than the 1 this model takes in a file, and the PDF has no text"},
			ocr: &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State {
				return done("## Page 1\nMidterm", store.OCRSection{N: 1})
			}}},
		{name: "a PDF with text is not OCR's", path: "/reading.pdf", ct: "application/pdf", givenAs: givenText, text: "Reading 3",
			ocr: &fakeOCR{}},
		{name: "a scan OCR could not read", path: "/scanned.pdf", ct: "application/pdf", asked: ocr.PDF, givenAs: givenNot, state: OCRFailed,
			notes: []string{"nor could the runtime's OCR recognize its text: recognizing it took longer than the runtime allows; ask for a version with selectable text"},
			ocr: &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State {
				return ocr.State{Status: ocr.StatusFailed, Why: "recognizing it took longer than the runtime allows"}
			}}},
		{name: "a scan OCR found no text in", path: "/scanned.pdf", ct: "application/pdf", asked: ocr.PDF, givenAs: givenNot,
			notes: []string{"it looks scanned, or like pictures of text; the runtime's OCR found no text in it; ask for a version with selectable text"},
			ocr:   &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State { return done(" \n") }}},
		{name: "a scan where there is no OCR", path: "/scanned.pdf", ct: "application/pdf", givenAs: givenNot, state: OCRUnavailable,
			notes: []string{"the runtime has no OCR here to recognize its text (tesseract is not installed); ask for a version"},
			ocr:   &fakeOCR{off: "tesseract is not installed"}},
		{name: "an image where there is no OCR is not fetched", path: "/photo.png", ct: "image/png", givenAs: givenNot,
			notes: []string{"this model does not take files; the runtime has no OCR here to recognize its text (OCR is off)"},
			ocr:   &fakeOCR{off: "OCR is off"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := fs.count(tc.path)
			f := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
				return executed(documentResult(fs.url(tc.path), "Notice", tc.ct, 100)), nil
			}}
			r := Runner{Client: core.NewClient(f), Files: NewHTTPFetcher(fs.Client()), FileInput: tc.fileInput, PDFLimits: tc.pdfLimits,
				Texts: NewTextCache(0), OCR: tc.ocr}
			parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", `{"document_id":"`+docID+`"}`)})
			if err != nil {
				t.Fatal(err)
			}
			rec := fileRecordOf(t, parts[0])
			if rec["given_as"] != tc.givenAs || (len(parts) == 2) != tc.file {
				t.Errorf("given as %v, %d parts: %v", rec["given_as"], len(parts), rec)
			}
			if got, _ := rec["ocr"].(string); got != tc.state {
				t.Errorf("ocr %q, want %q", got, tc.state)
			}
			note, _ := rec["note"].(string)
			for _, n := range tc.notes {
				if !strings.Contains(note, n) {
					t.Errorf("note %q, want one saying %q", note, n)
				}
			}
			if text, _ := contentOf(t, parts[0])["file_text"].(string); !strings.Contains(text, tc.text) {
				t.Errorf("file_text %q, want %q", text, tc.text)
			}
			if tc.asked == "" && tc.ocr.times() != 0 || tc.asked != "" && (tc.ocr.times() != 1 || tc.ocr.asked[0].kind != tc.asked) {
				t.Errorf("OCR asked %+v, want %q", tc.ocr.asked, tc.asked)
			}
			if tc.state == OCRUnavailable || tc.givenAs == givenText && tc.asked != "" {
				if (rec["extracted_from"] == "ocr") != (tc.givenAs == givenText) {
					t.Errorf("extracted_from %v", rec["extracted_from"])
				}
			}
			if fetched := fs.count(tc.path) > before; fetched == (tc.path == "/photo.png" && tc.ocr.off != "") {
				t.Errorf("fetched %v", fetched)
			}
		})
	}
}
