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

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

// signature stands for a presigned URL's credential: it must never reach
// the model, nor any error.
const signature = "X-Amz-Signature=5ecre7"

const markdown = "# Week 1\n\nRead chapter 2 <before> the lab & bring \"questions\".\n"

var pdf = append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0xE2, 0x00, 0x9F}, 100)...)

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
			http.NotFound(w, r)
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
		noFetcher   bool
		maxFile     int64
		givenAs     string
		note        string
		fetched     bool
		wantText    string
		wantFile    bool
		contentType string
	}{
		{name: "markdown is given as text", path: "/notes.md", title: "Week 1", ct: "text/markdown", size: len(markdown),
			givenAs: givenText, fetched: true, wantText: markdown, contentType: "text/markdown"},
		{name: "a PDF is a file part where the model takes files", path: "/syllabus.pdf", title: "Syllabus", ct: "application/pdf",
			size: len(pdf), fileInput: true, givenAs: givenFile, fetched: true, wantFile: true, contentType: "application/pdf"},
		{name: "a PDF is not given where the model takes no files", path: "/syllabus.pdf", title: "Syllabus", ct: "application/pdf",
			size: len(pdf), givenAs: givenNot, note: "this model does not take files", contentType: "application/pdf"},
		{name: "a type the runtime does not read is not fetched", path: "/notes.docx", title: "Notes",
			ct: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", size: 10, fileInput: true,
			givenAs: givenNot, note: "files are not read here"},
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
			r := Runner{Client: core.NewClient(f), FileInput: tc.fileInput, MaxFileBytes: tc.maxFile}
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
			note, _ := rec["note"].(string)
			if tc.note != "" && !strings.Contains(note, tc.note) || tc.note == "" && note != "" {
				t.Errorf("note %q, want one saying %q", note, tc.note)
			}
			text, _ := contentOf(t, parts[0])["file_text"].(string)
			if text != tc.wantText {
				t.Errorf("file_text %q, want %q", text, tc.wantText)
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
					fp.File.Name != "Syllabus.pdf" || !bytes.Equal(fp.File.Data, pdf) {
					t.Errorf("file part %+v", fp)
				}
				if rec["byte_size"] != float64(len(pdf)) {
					t.Errorf("byte_size %v, want %d", rec["byte_size"], len(pdf))
				}
			}
		})
	}
}

// TestRunFileTextBounded checks that a file's text is cut to the room the
// result leaves, and that the whole stays within MaxResultBytes.
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
			if n := len(parts[0].Content); n > max || n < max-40 {
				t.Errorf("%d bytes, want close to the limit of %d", n, max)
			}
			text, _ := contentOf(t, parts[0])["file_text"].(string)
			if !strings.HasPrefix(text, "line of text\n") || !strings.HasSuffix(text, "…[truncated, 130000 bytes]") {
				t.Errorf("file_text %q…", text[:min(len(text), 40)])
			}
			if rec := fileRecordOf(t, parts[0]); rec["given_as"] != givenText || rec["byte_size"] != float64(130000) {
				t.Errorf("file record %v", rec)
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
		"application/pdf": kindFile, "image/png": kindFile, "image/jpeg": kindFile, "image/webp": kindFile, "image/gif": kindFile,
		"image/svg+xml": kindOther, "application/zip": kindOther, "application/octet-stream": kindOther, "": kindOther,
	}
	for mt, want := range tests {
		if got := classify(mt); got != want {
			t.Errorf("classify(%q) = %v, want %v", mt, got, want)
		}
	}
	if mediaType("Text/Markdown; charset=UTF-8") != "text/markdown" || mediaType("not a type;;") != "" {
		t.Error("mediaType does not parse")
	}
}
