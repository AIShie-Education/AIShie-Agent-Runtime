package toolset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// signature stands for a presigned URL's credential: it must never reach
// the model, nor any error.
const signature = "X-Amz-Signature=5ecre7"

const markdown = "# Week 1\n\nRead chapter 2 <before> the lab & bring \"questions\".\n"

var pdf = append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0xE2, 0x00, 0x9F}, 100)...)

// Course documents as the model's reads find them, each at a path of the
// file server, with the content type it serves: slides, a handout, a
// workbook, PDFs that read and PDFs that do not.
var (
	deck = doctexttest.PPTX(
		doctexttest.Slide{Title: "Week 3: Sorting", Body: []doctexttest.Bullet{{Text: "Merge sort splits the list in two"}}, Images: 1},
		doctexttest.Slide{Title: "Complexity", Chart: "n log n", Notes: "Draw the recursion tree."},
	)
	handout = doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "Lab 3", Heading: 1}, {Text: "Bring a laptop.", List: "bullet"}}})
	grades  = doctexttest.XLSX(doctexttest.Sheet{Name: "Quiz", Rows: [][]any{{"Question", "Points"}, {"Q1", 5}}})
	reading = doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"Reading 3: sorting"}, CJK: []string{"第三週：排序"}},
		doctexttest.PDFPage{Lines: []string{"Merge sort is stable."}})
	scanned  = doctexttest.PDF(doctexttest.PDFPage{Image: true}, doctexttest.PDFPage{Image: true})
	garbled  = doctexttest.PDFWith(doctexttest.PDFOptions{BrokenToUnicode: true}, doctexttest.PDFPage{CJK: []string{"期中考範圍：第一章到第五章，含習題"}})
	locked   = doctexttest.PDFWith(doctexttest.PDFOptions{Encrypt: "aes256", UserPassword: "secret"}, doctexttest.PDFPage{Lines: []string{"x"}})
	cfb      = append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 504)...)
	password = append(append([]byte(nil), cfb...), []byte("E\x00n\x00c\x00r\x00y\x00p\x00t\x00e\x00d\x00P\x00a\x00c\x00k\x00a\x00g\x00e\x00")...)
	served   = map[string]struct {
		ct   string
		data []byte
	}{
		"/deck.pptx": {doctexttest.PPTXType, deck}, "/deck": {"", deck}, "/deck.bin": {"application/octet-stream", deck},
		"/handout.docx": {doctexttest.DOCXType, handout}, "/grades.xlsx": {doctexttest.XLSXType, grades},
		"/reading.pdf": {"application/pdf", reading}, "/scanned.pdf": {"application/pdf", scanned},
		"/garbled.pdf": {"application/pdf", garbled}, "/locked.pdf": {"application/pdf", locked},
		"/old.doc": {"application/msword", cfb}, "/old": {"", cfb}, "/secret.docx": {doctexttest.DOCXType, password},
		"/archive.zip": {"application/zip", doctexttest.Zip([2]string{"readme.txt", "hello"})},
		"/photo.png":   {"image/png", []byte("\x89PNG\r\n\x1a\n")},
	}
)

// fileServer serves the files a document_get's download_url points at, and
// counts what it is asked for.
type fileServer struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func newFileServer(t *testing.T) *fileServer {
	fs := &fileServer{hits: map[string]int{}}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.hits[r.URL.Path]++
		fs.mu.Unlock()
		if r.URL.RawQuery != signature {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/notes.md":
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = w.Write([]byte(markdown))
		case "/long.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(strings.Repeat("line of text\n", 10000)))
		case "/latin1.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("caf\xe9"))
		case "/syllabus.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(pdf)
		case "/untyped":
			_, _ = w.Write([]byte("plain words, no type given"))
		case "/big.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(bytes.Repeat([]byte("x"), 5000))
		case "/streamed.pdf":
			// No Content-Length: the body runs past the cap as it is read.
			w.Header().Set("Content-Type", "application/pdf")
			for range 10 {
				_, _ = w.Write(bytes.Repeat([]byte("y"), 1000))
				w.(http.Flusher).Flush()
			}
		case "/expired.pdf":
			http.Error(w, "expired", http.StatusForbidden)
		default:
			f, ok := served[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if f.ct != "" {
				w.Header().Set("Content-Type", f.ct)
			}
			_, _ = w.Write(f.data)
		}
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fileServer) url(path string) string { return fs.URL + path + "?" + signature }

func (fs *fileServer) count(path string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.hits[path]
}

// documentResult is document_get's result for a version with a file.
func documentResult(url, title, contentType string, size int) string {
	ct := "null"
	if contentType != "" {
		ct = `"` + contentType + `"`
	}
	return fmt.Sprintf(`{"id":%q,"kind":"material","title":%q,"sort_order":0,"status":"active","created_at":"2026-09-01T00:00:00Z",`+
		`"version":{"id":"v1","seq":1,"download_url":%q,"content_type":%s,"byte_size":%d,"author_member_id":"m","created_at":"2026-09-01T00:00:00Z","published":true}}`,
		docID, title, url, ct, size)
}

func fileRecordOf(t *testing.T, p llm.Part) map[string]any {
	t.Helper()
	rec, ok := contentOf(t, p)["file"].(map[string]any)
	if !ok {
		t.Fatalf("no file record in %s", p.Content)
	}
	return rec
}

func TestRunFiles(t *testing.T) {
	fs := newFileServer(t)
	tests := []struct {
		name        string
		path        string
		title, ct   string
		size        int
		fileInput   bool
		pdfLimits   llm.FileLimits
		docLimits   doctext.Limits
		noFetcher   bool
		maxFile     int64
		givenAs     string
		note        string
		fetched     bool
		wantText    string
		textHas     []string
		wantFile    bool
		contentType string
		extracted   string
		// convert has the runner convert Office files (a stub of
		// LibreOffice's, which makes PDFs of two pages); off, as the
		// other cases are, the files are given as before.
		convert bool
	}{
		{name: "markdown is given as text", path: "/notes.md", title: "Week 1", ct: "text/markdown", size: len(markdown),
			givenAs: givenText, fetched: true, wantText: markdown, contentType: "text/markdown"},
		{name: "a PDF is a file part where the model takes files", path: "/syllabus.pdf", title: "Syllabus", ct: "application/pdf",
			size: len(pdf), fileInput: true, givenAs: givenFile, fetched: true, wantFile: true, contentType: "application/pdf"},
		{name: "a PDF within its provider's limits is a file part", path: "/reading.pdf", title: "Reading", ct: "application/pdf",
			size: len(reading), fileInput: true, pdfLimits: llm.FileLimits{PDFBytes: 1 << 20, PDFPages: 2}, givenAs: givenFile, fetched: true,
			wantFile: true, contentType: "application/pdf"},
		{name: "a PDF's text, where the model takes no files", path: "/reading.pdf", title: "Reading", ct: "application/pdf",
			size: len(reading), givenAs: givenText, fetched: true, extracted: "pdf", note: "the runtime's text of its 2 pages",
			wantText: "## Page 1\nReading 3: sorting\n第三週：排序\n\n## Page 2\nMerge sort is stable.", contentType: "application/pdf"},
		{name: "a PDF of more pages than its provider takes is given as its text", path: "/reading.pdf", title: "Reading", ct: "application/pdf",
			size: len(reading), fileInput: true, pdfLimits: llm.FileLimits{PDFPages: 1}, givenAs: givenText, fetched: true, extracted: "pdf",
			note: "since it has 2 pages, more than the 1 this model takes in a file", textHas: []string{"第三週：排序", "## Page 2"}},
		{name: "a PDF larger than its provider takes is given as its text", path: "/reading.pdf", title: "Reading", ct: "application/pdf",
			size: len(reading), fileInput: true, pdfLimits: llm.FileLimits{PDFBytes: 100}, givenAs: givenText, fetched: true, extracted: "pdf",
			note: "more than the 100 bytes this model takes as a file", textHas: []string{"Merge sort is stable."}},
		{name: "a scanned PDF is a file part where the model takes files", path: "/scanned.pdf", title: "Scan", ct: "application/pdf",
			size: len(scanned), fileInput: true, pdfLimits: llm.FileLimits{PDFPages: 10}, givenAs: givenFile, fetched: true, wantFile: true},
		{name: "a scanned PDF is not given where the model takes no files", path: "/scanned.pdf", title: "Scan", ct: "application/pdf",
			size: len(scanned), givenAs: givenNot, fetched: true,
			note: "this model does not take files, and the PDF has no text to read: it looks scanned, or like pictures of text; " +
				"the runtime has no OCR here to recognize its text; ask for a version with selectable text"},
		{name: "a scanned PDF of more pages than its provider takes is not given", path: "/scanned.pdf", title: "Scan", ct: "application/pdf",
			size: len(scanned), fileInput: true, pdfLimits: llm.FileLimits{PDFPages: 1}, givenAs: givenNot, fetched: true,
			note: "it has 2 pages, more than the 1 this model takes in a file, and the PDF has no text to read"},
		{name: "a PDF whose fonts do not map is a file part where the model takes files", path: "/garbled.pdf", title: "Midterm", ct: "application/pdf",
			size: len(garbled), fileInput: true, pdfLimits: llm.FileLimits{PDFPages: 10}, givenAs: givenFile, fetched: true, wantFile: true},
		{name: "a PDF whose fonts do not map is not given where the model takes no files", path: "/garbled.pdf", title: "Midterm", ct: "application/pdf",
			size: len(garbled), givenAs: givenNot, fetched: true,
			note: "this model does not take files, and the PDF's text cannot be read: its fonts do not map to text; " +
				"the runtime has no OCR here to recognize its text; ask for a version with selectable text"},
		{name: "a PDF that needs a password is given to no model", path: "/locked.pdf", title: "Answers", ct: "application/pdf",
			size: len(locked), fileInput: true, pdfLimits: llm.FileLimits{PDFPages: 10}, givenAs: givenNot, fetched: true, note: "password-protected"},
		{name: "a PDF that does not read is not given where the model takes no files", path: "/syllabus.pdf", title: "Syllabus", ct: "application/pdf",
			size: len(pdf), givenAs: givenNot, fetched: true, note: "it could not be read", contentType: "application/pdf"},
		{name: "a PowerPoint deck is given as the runtime's text of it", path: "/deck.pptx", title: "Week 3", ct: doctexttest.PPTXType,
			size: len(deck), givenAs: givenText, fetched: true, extracted: "pptx",
			note:     "the runtime's text of its 2 slides, with their speaker notes; its 1 image and 1 chart are only named, as [image] and [chart]",
			wantText: "## Slide 1: Week 3: Sorting\n- Merge sort splits the list in two\n[image]\n\n## Slide 2: Complexity\n[chart: n log n]\nNotes: Draw the recursion tree."},
		{name: "a deck to a model that takes files is text all the same", path: "/deck.pptx", title: "Week 3", ct: doctexttest.PPTXType,
			size: len(deck), fileInput: true, givenAs: givenText, fetched: true, extracted: "pptx", note: "2 slides", textHas: []string{"Merge sort"}},
		{name: "a Word document", path: "/handout.docx", title: "Lab 3", ct: doctexttest.DOCXType, size: len(handout), givenAs: givenText,
			fetched: true, extracted: "docx", note: "the runtime's text of the document", wantText: "# Lab 3\n\n- Bring a laptop."},
		{name: "an Excel workbook", path: "/grades.xlsx", title: "Quiz", ct: doctexttest.XLSXType, size: len(grades), givenAs: givenText,
			fetched: true, extracted: "xlsx", note: "the runtime's text of its 1 sheet", wantText: "## Sheet 1: Quiz\nQuestion,Points\nQ1,5"},
		{name: "a deck of no recorded type is known by what it holds", path: "/deck", title: "Week 3", size: len(deck), givenAs: givenText,
			fetched: true, extracted: "pptx", note: "2 slides", textHas: []string{"Merge sort"}, contentType: doctexttest.PPTXType},
		{name: "a deck recorded as an octet stream is known by what it holds", path: "/deck.bin", title: "Week 3", ct: "application/octet-stream",
			size: len(deck), givenAs: givenText, fetched: true, extracted: "pptx", note: "2 slides", textHas: []string{"Complexity"},
			contentType: doctexttest.PPTXType},
		{name: "a zip that is no Office file is not given", path: "/archive.zip", title: "Files", ct: "application/zip", size: 100,
			givenAs: givenNot, fetched: true, note: "application/zip files are not read here", contentType: "application/zip"},
		{name: "an older Word file is not fetched", path: "/old.doc", title: "Old notes", ct: "application/msword", size: len(cfb),
			givenAs: givenNot, note: "an older Office format (.ppt, .doc or .xls) that the runtime cannot read; ask for it as .pptx, .docx or .xlsx, or as a PDF"},
		{name: "an older Office file of no recorded type", path: "/old", title: "Old notes", size: len(cfb), givenAs: givenNot, fetched: true,
			note: "an older Office format"},
		{name: "a Word document encrypted with a password", path: "/secret.docx", title: "Key", ct: doctexttest.DOCXType, size: len(password),
			givenAs: givenNot, fetched: true, note: "it is password-protected; ask for a copy without a password"},
		{name: "a document past what the runtime reads of one", path: "/deck.pptx", title: "Week 3", ct: doctexttest.PPTXType, size: len(deck),
			docLimits: doctext.Limits{MaxTokens: 50}, givenAs: givenNot, fetched: true, note: "larger or more complex than the runtime reads"},
		{name: "an image is not fetched for a model that takes no files", path: "/photo.png", title: "Photo", ct: "image/png", size: 8,
			givenAs: givenNot, note: "this model does not take files"},
		{name: "a type the runtime does not read is not fetched", path: "/talk.mp3", title: "Talk", ct: "audio/mpeg", size: 10,
			fileInput: true, givenAs: givenNot, note: "audio/mpeg files are not read here"},
		{name: "a file Core says is too large is not fetched", path: "/big.pdf", title: "Big", ct: "application/pdf",
			size: 5000, fileInput: true, maxFile: 4096, givenAs: givenNot, note: "larger than the 4096 bytes the runtime reads"},
		{name: "a file larger than Core said is refused as it is read", path: "/big.pdf", title: "Big", ct: "application/pdf",
			size: 10, fileInput: true, maxFile: 4096, givenAs: givenNot, note: "larger than", fetched: true},
		{name: "a streamed file past the cap is refused", path: "/streamed.pdf", title: "Stream", ct: "application/pdf",
			size: 10, fileInput: true, maxFile: 4096, givenAs: givenNot, note: "larger than", fetched: true},
		{name: "an expired URL", path: "/expired.pdf", title: "Old", ct: "application/pdf", size: 10, fileInput: true,
			givenAs: givenNot, note: "could not be fetched", fetched: true},
		{name: "a file of no recorded type is fetched and sniffed", path: "/untyped", title: "Readme", size: 26,
			givenAs: givenText, fetched: true, wantText: "plain words, no type given", contentType: "text/plain"},
		{name: "text that is not UTF-8 is made so", path: "/latin1.txt", title: "Café", ct: "text/plain", size: 4,
			givenAs: givenText, fetched: true, wantText: "caf�", contentType: "text/plain"},
		{name: "no fetcher, no file", path: "/notes.md", title: "Week 1", ct: "text/markdown", size: 10, noFetcher: true,
			givenAs: givenNot, note: "files are not fetched here"},
		{name: "converting, a deck to a model that takes files is LibreOffice's PDF of it", convert: true, path: "/deck.pptx", title: "Week 3",
			ct: doctexttest.PPTXType, size: len(deck), fileInput: true, givenAs: givenFile, fetched: true, wantFile: true,
			note: "LibreOffice converted it to PDF, which is given: its 2 slides as they look", wantText: "## Slide 2\nNotes: Draw the recursion tree."},
		{name: "converting, a deck to a model that takes none is its text", convert: true, path: "/deck.pptx", title: "Week 3", ct: doctexttest.PPTXType,
			size: len(deck), givenAs: givenText, fetched: true, extracted: "pptx",
			note:    "its 1 image and 1 chart are only named, as [image] and [chart]; the text in its pictures and charts is not read: the runtime has no OCR here",
			textHas: []string{"## Slide 1: Week 3: Sorting", "Notes: Draw the recursion tree."}},
		{name: "converting, a Word document to a model that takes files is LibreOffice's PDF of it", convert: true, path: "/handout.docx", title: "Lab 3",
			ct: doctexttest.DOCXType, size: len(handout), fileInput: true, givenAs: givenFile, fetched: true, wantFile: true,
			note: "LibreOffice converted it to PDF, which is given: its 2 pages as they look"},
		{name: "converting, a Word document to a model that takes none is its text as before", convert: true, path: "/handout.docx", title: "Lab 3",
			ct: doctexttest.DOCXType, size: len(handout), givenAs: givenText, fetched: true, extracted: "docx", note: "the runtime's text of the document",
			wantText: "# Lab 3\n\n- Bring a laptop."},
		{name: "converting, a workbook is its text as before", convert: true, path: "/grades.xlsx", title: "Quiz", ct: doctexttest.XLSXType,
			size: len(grades), fileInput: true, givenAs: givenText, fetched: true, extracted: "xlsx", wantText: "## Sheet 1: Quiz\nQuestion,Points\nQ1,5",
			note: "the runtime's text of its 1 sheet"},
		{name: "converting, an older Word file is read in LibreOffice's PDF of it", convert: true, path: "/old.doc", title: "Old notes",
			ct: "application/msword", size: len(cfb), givenAs: givenText, fetched: true, extracted: "pdf",
			note: "LibreOffice converted it to PDF: the runtime's text of its 2 pages", wantText: "## Page 1\npage 1\n\n## Page 2\npage 2"},
		{name: "converting, an older Office file that says nothing of what it holds is not given", convert: true, path: "/old", title: "Old notes",
			size: len(cfb), givenAs: givenNot, fetched: true, note: "an older Office format"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := fs.count(tc.path)
			f := &fakeCore{respond: func(_ context.Context, tool string, _ json.RawMessage) (*core.Envelope, error) {
				if tool == "document_get" {
					return executed(documentResult(fs.url(tc.path), tc.title, tc.ct, tc.size)), nil
				}
				return executed(`{"id":"c"}`), nil
			}}
			r := Runner{Client: core.NewClient(f), FileInput: tc.fileInput, PDFLimits: tc.pdfLimits, MaxFileBytes: tc.maxFile, DocLimits: tc.docLimits}
			if tc.convert {
				r.Office = &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(2)}}
			}
			if !tc.noFetcher {
				r.Files = NewHTTPFetcher(fs.Client())
			}
			parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{
				call("d", "document_get", `{"document_id":"`+docID+`"}`),
				call("c", "course_get", `{}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range parts {
				if strings.Contains(p.Content, "Signature") || strings.Contains(p.Content, fs.URL) || strings.Contains(p.Content, "download_url") {
					t.Fatalf("the URL reached the model: %s", p.Content)
				}
			}
			if fetched := fs.count(tc.path) > before; fetched != tc.fetched {
				t.Errorf("fetched %v, want %v", fetched, tc.fetched)
			}
			if parts[0].IsError || parts[1].IsError {
				t.Errorf("a file problem made the call an error: %+v", parts[0])
			}
			rec := fileRecordOf(t, parts[0])
			if rec["given_as"] != tc.givenAs || rec["name"] != tc.title {
				t.Errorf("file record %v", rec)
			}
			if tc.contentType != "" && rec["content_type"] != tc.contentType {
				t.Errorf("content_type %v, want %s", rec["content_type"], tc.contentType)
			}
			if got, _ := rec["extracted_from"].(string); got != tc.extracted {
				t.Errorf("extracted_from %q, want %q", got, tc.extracted)
			}
			note, _ := rec["note"].(string)
			if tc.note != "" && !strings.Contains(note, tc.note) || tc.note == "" && note != "" {
				t.Errorf("note %q, want one saying %q", note, tc.note)
			}
			text, _ := contentOf(t, parts[0])["file_text"].(string)
			if tc.wantText != "" || tc.textHas == nil {
				if text != tc.wantText {
					t.Errorf("file_text %q, want %q", text, tc.wantText)
				}
			}
			for _, want := range tc.textHas {
				if !strings.Contains(text, want) {
					t.Errorf("file_text %q, want it to hold %q", text, want)
				}
			}
			// The results come first, in call order; then the files.
			wantParts := 2
			if tc.wantFile {
				wantParts = 3
			}
			if len(parts) != wantParts || parts[0].CallID != "d" || parts[1].CallID != "c" {
				t.Fatalf("parts %+v", parts)
			}
			if tc.wantFile {
				fp := parts[2]
				if fp.Type != llm.PartFile || fp.File == nil || fp.File.MIME != "application/pdf" ||
					fp.File.Name != tc.title+".pdf" || !tc.convert && int(rec["byte_size"].(float64)) != len(fp.File.Data) {
					t.Errorf("file part %+v", fp)
				}
				if tc.convert && (rec["converted_to"] != "pdf" || string(fp.File.Data) != string(pdfOf(2).Data)) {
					t.Errorf("not LibreOffice's PDF: %v", rec)
				}
			}
		})
	}
}

// TestRunFileTextBounded checks that a text file too long for one result
// is given in parts that each fit it, whatever the limit, and that the
// first says how many there are.
func TestRunFileTextBounded(t *testing.T) {
	fs := newFileServer(t)
	for _, limit := range []int{0, 4000} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
				return executed(documentResult(fs.url("/long.txt"), "Long", "text/plain", 130000)), nil
			}}
			r := Runner{Client: core.NewClient(f), Files: NewHTTPFetcher(fs.Client()), MaxResultBytes: limit}
			parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", `{"document_id":"`+docID+`"}`)})
			if err != nil {
				t.Fatal(err)
			}
			max := r.withDefaults().MaxResultBytes
			if n := len(parts[0].Content); n > max {
				t.Errorf("%d bytes, over the limit of %d", n, max)
			}
			text, _ := contentOf(t, parts[0])["file_text"].(string)
			if !strings.HasPrefix(text, "line of text\n") || strings.Contains(text, "truncated") || !strings.HasSuffix(text, "line of text\n") {
				t.Errorf("file_text %q…", text[:min(len(text), 40)])
			}
			rec := fileRecordOf(t, parts[0])
			wantParts := (130000 + len(text) - 1) / len(text)
			if rec["given_as"] != givenText || rec["byte_size"] != float64(130000) || rec["part"] != float64(1) || int(rec["parts"].(float64)) != wantParts {
				t.Errorf("file record %v, want part 1 of %d", rec, wantParts)
			}
			if note, _ := rec["note"].(string); !strings.Contains(note, fmt.Sprintf("given in %d parts", wantParts)) {
				t.Errorf("note %q", note)
			}
		})
	}
}

// TestRunFileTextNoRoom checks that a result that fills the limit on its own
// leaves the file out, and says so.
func TestRunFileTextNoRoom(t *testing.T) {
	fs := newFileServer(t)
	body, _ := json.Marshal(strings.Repeat("x", 5000))
	f := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
		res := documentResult(fs.url("/notes.md"), "Week 1", "text/markdown", len(markdown))
		return executed(strings.Replace(res, `"kind":"material"`, `"kind":"material","description":`+string(body), 1)), nil
	}}
	r := Runner{Client: core.NewClient(f), Files: NewHTTPFetcher(fs.Client()), MaxResultBytes: 2000}
	parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", `{"document_id":"`+docID+`"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts[0].Content) > 2000 {
		t.Fatalf("%d bytes, over the limit", len(parts[0].Content))
	}
	m := contentOf(t, parts[0])
	if _, has := m["result_truncated"]; !has {
		t.Errorf("not truncated: %s", parts[0].Content)
	}
	rec := m["file"].(map[string]any)
	if rec["given_as"] != givenNot || !strings.Contains(rec["note"].(string), "no room") {
		t.Errorf("file record %v", rec)
	}
	if strings.Contains(parts[0].Content, "Signature") {
		t.Error("the URL reached the model")
	}
}

func TestHTTPFetcher(t *testing.T) {
	fs := newFileServer(t)
	ctx := context.Background()
	fetcher := NewHTTPFetcher(fs.Client())

	f, err := fetcher.Fetch(ctx, fs.url("/syllabus.pdf"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if f.ContentType != "application/pdf" || !bytes.Equal(f.Data, pdf) {
		t.Errorf("fetched %q, %d bytes", f.ContentType, len(f.Data))
	}
	if _, err := fetcher.Fetch(ctx, fs.url("/big.pdf"), 4096); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a Content-Length over the cap: err %v", err)
	}
	if _, err := fetcher.Fetch(ctx, fs.url("/streamed.pdf"), 4096); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a body over the cap: err %v", err)
	}
	if f, err := fetcher.Fetch(ctx, fs.url("/big.pdf"), 5000); err != nil || len(f.Data) != 5000 {
		t.Errorf("a file of exactly the cap: %v", err)
	}

	var fe *FetchError
	_, err = fetcher.Fetch(ctx, fs.url("/expired.pdf"), 4096)
	if !errors.As(err, &fe) || fe.Status != http.StatusForbidden {
		t.Errorf("a 403: err %v", err)
	}
	checkNoURL(t, err)

	for _, bad := range []string{"ftp://files.example/x?" + signature, "file:///etc/passwd", "/relative?" + signature, "https://?" + signature, "%zz"} {
		_, err := fetcher.Fetch(ctx, bad, 4096)
		if err == nil {
			t.Errorf("%s: fetched", bad)
		}
		checkNoURL(t, err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, err = fetcher.Fetch(ctx, closed.URL+"/x?"+signature, 4096)
	if err == nil {
		t.Error("a closed server answered")
	}
	checkNoURL(t, err)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := fetcher.Fetch(cancelled, fs.url("/notes.md"), 4096); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled fetch: err %v", err)
	}
}

func checkNoURL(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "5ecre7") {
		t.Errorf("the URL's credential is in the error: %v", err)
	}
}

func TestFileName(t *testing.T) {
	tests := []struct{ title, mt, want string }{
		{"Syllabus", "application/pdf", "Syllabus.pdf"},
		{"syllabus.PDF", "application/pdf", "syllabus.PDF"},
		{"Diagram", "image/jpeg", "Diagram.jpg"},
		{"photo.jpeg", "image/jpeg", "photo.jpeg"},
		{"", "image/png", "document.png"},
		{"Notes", "text/plain", "Notes"},
	}
	for _, tc := range tests {
		if got := fileName(tc.title, tc.mt); got != tc.want {
			t.Errorf("fileName(%q, %q) = %q, want %q", tc.title, tc.mt, got, tc.want)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := map[string]fileKind{
		"text/plain": kindText, "text/markdown": kindText, "text/csv": kindText, "application/json": kindText,
		"application/ld+json": kindText, "application/x-markdown": kindText,
		"application/pdf": kindPDF, "image/png": kindImage, "image/jpeg": kindImage, "image/webp": kindImage, "image/gif": kindImage,
		doctexttest.PPTXType: kindOffice, doctexttest.DOCXType: kindOffice, doctexttest.XLSXType: kindOffice,
		"application/vnd.ms-powerpoint.presentation.macroenabled.12": kindOffice,
		"application/msword": kindOldOffice, "application/vnd.ms-excel": kindOldOffice, "application/vnd.ms-powerpoint": kindOldOffice,
		"application/zip": kindUnknown, "application/octet-stream": kindUnknown, "": kindUnknown,
		"image/svg+xml": kindOther, "audio/mpeg": kindOther, "application/vnd.oasis.opendocument.text": kindOther,
	}
	for mt, want := range tests {
		if got := classify(mt); got != want {
			t.Errorf("classify(%q) = %v, want %v", mt, got, want)
		}
	}
	// Where the runtime converts Office files, presentations, documents
	// and the workbooks it reads no other way are converted; an older
	// Office file of no telling type is fetched to know which it is.
	converting := Runner{Office: &stubOffice{}}
	for mt, want := range map[string]fileKind{
		doctexttest.PPTXType: kindConvert, doctexttest.DOCXType: kindConvert, doctexttest.XLSXType: kindOffice,
		"application/msword": kindConvert, "application/vnd.ms-excel": kindConvert, "application/vnd.oasis.opendocument.text": kindConvert,
		"text/rtf": kindConvert, "application/x-ole-storage": kindUnknown, "application/pdf": kindPDF, "text/plain": kindText,
	} {
		if got := converting.kindOf(mt); got != want {
			t.Errorf("converting, kindOf(%q) = %v, want %v", mt, got, want)
		}
	}
	if mediaType("Text/Markdown; charset=UTF-8") != "text/markdown" || mediaType("not a type;;") != "" {
		t.Error("mediaType does not parse")
	}
}

func TestSizeOf(t *testing.T) {
	for n, want := range map[int64]string{4096: "4096 bytes", 10 << 20: "10 MiB", 3<<20 + 1<<19: "3.5 MiB"} {
		if got := sizeOf(n); got != want {
			t.Errorf("sizeOf(%d) = %q, want %q", n, got, want)
		}
	}
}
