package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// withText is documentResult with the version's text version.
func withText(result, text string) string {
	return strings.TrimSuffix(result, `"published":true}}`) + `"published":true,"text":` + text + `}}`
}

// transcript is a text version's body of pages pages.
func transcript(pages int, filler string) string {
	var b strings.Builder
	for n := 1; n <= pages; n++ {
		if n > 1 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## 第 %d 頁\n\nPage %d says %s.", n, n, filler)
	}
	return b.String()
}

// textCore answers document_get with the document at url, its version's
// text version as text says, and document_text with the parts of body,
// each of at most per bytes, at the revision revs gives the call (from 1).
type textCore struct {
	url, text, body string
	per             int
	revs            func(n int) int

	mu    sync.Mutex
	reads []int
}

func (c *textCore) respond(_ context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
	switch tool {
	case "document_get":
		return executed(withText(documentResult(c.url, "Week 1", "application/pdf", len(reading)), c.text)), nil
	case "document_text":
		var a struct {
			Part int `json:"part"`
		}
		_ = json.Unmarshal(args, &a)
		c.mu.Lock()
		c.reads = append(c.reads, a.Part)
		n := len(c.reads)
		c.mu.Unlock()
		var parts []string
		for s := c.body; s != ""; {
			k := min(c.per, len(s))
			parts, s = append(parts, s[:k]), s[k:]
		}
		body, _ := json.Marshal(parts[a.Part-1])
		rev := 3
		if c.revs != nil {
			rev = c.revs(n)
		}
		return executed(fmt.Sprintf(`{"document_id":%q,"version_id":"v1","seq":1,"published":true,"part":%d,"parts":%d,`+
			`"text":{"status":"done","source":"ai","model":"Flash-Lite","pages":4,"revision":%d,"updated_at":"2026-09-01T00:00:00Z","bytes":%d,"body":%s}}`,
			docID, a.Part, len(parts), rev, len(c.body), body)), nil
	}
	return executed(`{}`), nil
}

func (c *textCore) partsRead() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.reads...)
}

// A version whose text version is done is given it, in place of the
// runtime's reading of the file, which is not fetched: marked as an AI
// transcription by its model, or as staff's; read in parts, at one
// revision, where Core does not give it whole; kept, and dropped as the
// worker drops it. One not done is given as before.
func TestRunTextVersion(t *testing.T) {
	fs := newFileServer(t)
	body := transcript(2, "hello")
	done := func(source, extra string) string {
		b, _ := json.Marshal(body)
		return fmt.Sprintf(`{"status":"done","source":%q,"model":"Flash-Lite","pages":2,"revision":3,"updated_at":"2026-09-01T00:00:00Z",`+
			`"bytes":%d%s,"body":%s}`, source, len(body), extra, b)
	}
	get := func(t *testing.T, r Runner, args string) llm.Part {
		t.Helper()
		parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", args)})
		if err != nil {
			t.Fatal(err)
		}
		return parts[0]
	}
	for _, c := range []struct {
		name, text, source string
		fileInput          bool
	}{
		{"an AI transcription", done("ai", ""), "AI transcription (Flash-Lite)", true},
		{"a staff's text", done("staff", `,"edited_by_name":"Sato","edited_at":"2026-09-02T00:00:00Z"`), "edited by staff", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := fs.count("/reading.pdf")
			tc := &textCore{url: fs.url("/reading.pdf"), text: c.text}
			f := &fakeCore{respond: tc.respond}
			r := Runner{Client: core.NewClient(f), Files: NewHTTPFetcher(fs.Client()), FileInput: c.fileInput, Texts: NewTextCache(0)}
			p := get(t, r, `{"document_id":"`+docID+`"}`)
			rec := fileRecordOf(t, p)
			text, _ := contentOf(t, p)["file_text"].(string)
			note, _ := rec["note"].(string)
			if rec["given_as"] != givenText || rec["text_source"] != c.source || text != body || rec["extracted_from"] != nil ||
				!strings.Contains(note, "file_text is the document's text version") || strings.Contains(note, FilePagesArg) != c.fileInput {
				t.Errorf("the text version: %s", p.Content)
			}
			if fs.count("/reading.pdf") != before {
				t.Error("the file was fetched")
			}
		})
	}

	// Pending: the file, as before.
	tc := &textCore{url: fs.url("/reading.pdf"), text: `{"status":"pending","revision":0,"updated_at":"2026-09-01T00:00:00Z","bytes":0}`}
	r := Runner{Client: core.NewClient(&fakeCore{respond: tc.respond}), Files: NewHTTPFetcher(fs.Client()), FileInput: true}
	p := get(t, r, `{"document_id":"`+docID+`"}`)
	if rec := fileRecordOf(t, p); rec["given_as"] != givenFile || rec["text_source"] != nil {
		t.Errorf("a text version pending: %s", p.Content)
	}

	// Long: read in parts, at one revision, a change of it read again;
	// then kept, until the worker drops it.
	long := transcript(4, strings.Repeat("x", 3000))
	tc = &textCore{url: fs.url("/reading.pdf"), body: long, per: 4000, revs: func(n int) int { return min(n+2, 4) },
		text: `{"status":"done","source":"ai","model":"Flash-Lite","pages":4,"revision":3,"updated_at":"2026-09-01T00:00:00Z","bytes":12114}`}
	cache := NewTextCache(0)
	r = Runner{Client: core.NewClient(&fakeCore{respond: tc.respond}), Files: NewHTTPFetcher(fs.Client()), Texts: cache}
	p = get(t, r, `{"document_id":"`+docID+`"}`)
	if text, _ := contentOf(t, p)["file_text"].(string); text != long {
		t.Fatalf("a long text version: %q", text)
	}
	if got := tc.partsRead(); fmt.Sprint(got) != "[1 2 1 2 3 4]" {
		t.Errorf("parts read %v, want them all again after the revision moved", got)
	}
	// Core now says the revision read.
	tc.text = strings.Replace(tc.text, `"revision":3`, `"revision":4`, 1)
	get(t, r, `{"document_id":"`+docID+`"}`)
	if got := tc.partsRead(); len(got) != 6 {
		t.Errorf("the text kept was read again: %v", got)
	}
	cache.DropText("v1")
	if st := cache.Stats(); st.Readings != 0 {
		t.Errorf("dropped, and kept: %+v", st)
	}

	// Too long for one result: in parts of its pages, file_part reading
	// the next.
	r.MaxResultBytes = 8192
	p = get(t, r, `{"document_id":"`+docID+`"}`)
	rec := fileRecordOf(t, p)
	if rec["parts"] != float64(2) || rec["part_holds"] != "pages 1–2" || rec["text_source"] != "AI transcription (Flash-Lite)" {
		t.Errorf("part 1: %s", p.Content)
	}
	p = get(t, r, `{"document_id":"`+docID+`","file_part":2}`)
	if rec = fileRecordOf(t, p); rec["part"] != float64(2) || rec["part_holds"] != "pages 3–4" {
		t.Errorf("part 2: %s", p.Content)
	}
	if text, _ := contentOf(t, p)["file_text"].(string); !strings.HasPrefix(text, "## 第 3 頁") {
		t.Errorf("part 2's text: %q", text)
	}
}

// file_pages gives a model that takes files pages of the file itself, cut
// as a PDF of their own, in place of the text version; a model that takes
// none is given the text version, and told why; pages past the file's, too
// many, or asked for with file_part are refused or said so.
func TestRunFilePages(t *testing.T) {
	fs := newFileServer(t)
	body := transcript(2, "hello")
	b, _ := json.Marshal(body)
	tc := &textCore{url: fs.url("/reading.pdf"),
		text: fmt.Sprintf(`{"status":"done","source":"ai","model":"Flash-Lite","revision":1,"updated_at":"2026-09-01T00:00:00Z","bytes":%d,"body":%s}`, len(body), b)}
	so := &stubOffice{}
	r := Runner{Client: core.NewClient(&fakeCore{respond: tc.respond}), Files: NewHTTPFetcher(fs.Client()), FileInput: true, Office: so}
	run := func(args string) []llm.Part {
		t.Helper()
		parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", args)})
		if err != nil {
			t.Fatal(err)
		}
		return parts
	}
	parts := run(`{"document_id":"` + docID + `","file_pages":"2"}`)
	rec := fileRecordOf(t, parts[0])
	if len(parts) != 2 || parts[1].File == nil || parts[1].File.Name != "Week 1 (page 2).pdf" || rec["given_as"] != givenFile ||
		rec["part_holds"] != "page 2" || rec["text_source"] != nil || contentOf(t, parts[0])["file_text"] != nil {
		t.Fatalf("page 2: %s", parts[0].Content)
	}
	if _, ranges, _ := so.record(); fmt.Sprint(ranges) != "[[2 2]]" {
		t.Errorf("cut %v", ranges)
	}
	if rec = fileRecordOf(t, run(`{"document_id":"` + docID + `","file_pages":"5-6"}`)[0]); rec["given_as"] != givenNot ||
		!strings.Contains(rec["note"].(string), "the file has 2 pages: there is no page 5") {
		t.Errorf("pages past the last: %v", rec)
	}
	for _, args := range []string{`"file_pages":"0"`, `"file_pages":"3-1"`, `"file_pages":"1-20"`, `"file_pages":7`, `"file_pages":"2","file_part":2`} {
		p := run(`{"document_id":"` + docID + `",` + args + `}`)[0]
		if code, _ := errorOf(t, p); code != core.CodeInvalidArgument {
			t.Errorf("%s: %s", args, p.Content)
		}
	}
	// A model that takes no files: the text version, and why not the pages.
	r.FileInput = false
	p := run(`{"document_id":"` + docID + `","file_pages":"1-2"}`)[0]
	if rec = fileRecordOf(t, p); rec["given_as"] != givenText || rec["text_source"] == nil ||
		!strings.Contains(rec["note"].(string), "file_pages does not apply: this model does not take files") {
		t.Errorf("no files: %s", p.Content)
	}
	// Pages of a PDF where none are cut: the whole of it.
	r.FileInput, so.noCuts = true, true
	parts = run(`{"document_id":"` + docID + `","file_pages":"2"}`)
	if rec = fileRecordOf(t, parts[0]); len(parts) != 2 || rec["given_as"] != givenFile ||
		!strings.Contains(rec["note"].(string), "the whole PDF is given: see page 2 in it") {
		t.Errorf("no cuts: %s", parts[0].Content)
	}
}

// document_get offers the runtime's file_pages beside file_part.
func TestDocumentGetOffersFilePages(t *testing.T) {
	for _, d := range delegateSet(t).Declarations() {
		if d.Name == FilePartTool && !strings.Contains(string(d.Schema), `"`+FilePagesArg+`"`) {
			t.Errorf("%s's schema: %s", d.Name, d.Schema)
		}
	}
}

func TestPageSpan(t *testing.T) {
	for in, want := range map[string][3]int{"3": {3, 3, 1}, " 3-5 ": {3, 5, 1}, "3–5": {3, 5, 1}, "3 ~ 4": {3, 4, 1}, "0": {}, "5-3": {}, "x": {},
		"": {}, "1-": {}} {
		first, last, ok := pageSpan(in)
		if ok != (want[2] == 1) || ok && (first != want[0] || last != want[1]) {
			t.Errorf("pageSpan(%q) = %d, %d, %v", in, first, last, ok)
		}
	}
}

func TestHeadingSections(t *testing.T) {
	text := "## 第 1 頁\n\na\n\n## 第 2 頁\n\nb\n## 投影片 3\nc\n## 第 x 頁\n"
	got := headingSections(text)
	if len(got) != 3 || got[0].Offset != 0 || got[1].N != 2 || got[2].Kind != "slide" || got[2].N != 3 || text[got[2].Offset:got[2].Offset+3] != "## " {
		t.Errorf("%+v", got)
	}
}
