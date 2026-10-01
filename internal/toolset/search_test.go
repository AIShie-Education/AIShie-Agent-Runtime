package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

// The course the search tests read: its documents, versions and files, by
// ids as Core's (UUIDs, which document_get's schema holds the model's
// calls to).
const (
	docSlides   = "0192f3c1-d001-7b4a-9c3d-2e1f0a9b8c7d"
	docReading  = "0192f3c1-d002-7b4a-9c3d-2e1f0a9b8c7d"
	docHandout  = "0192f3c1-d003-7b4a-9c3d-2e1f0a9b8c7d"
	docSyllabus = "0192f3c1-d004-7b4a-9c3d-2e1f0a9b8c7d"
	docAnswers  = "0192f3c1-d005-7b4a-9c3d-2e1f0a9b8c7d"
	docWithheld = "0192f3c1-d006-7b4a-9c3d-2e1f0a9b8c7d"

	verSlides1 = "0192f3c1-a001-7b4a-9c3d-2e1f0a9b8c7d"
	verSlides2 = "0192f3c1-a002-7b4a-9c3d-2e1f0a9b8c7d"
	verReading = "0192f3c1-a003-7b4a-9c3d-2e1f0a9b8c7d"
	verHandout = "0192f3c1-a004-7b4a-9c3d-2e1f0a9b8c7d"
	verSyll    = "0192f3c1-a005-7b4a-9c3d-2e1f0a9b8c7d"
	verAnswers = "0192f3c1-a006-7b4a-9c3d-2e1f0a9b8c7d"
	verWithhel = "0192f3c1-a007-7b4a-9c3d-2e1f0a9b8c7d"

	sfileSlides1    = "0192f3c1-b001-7b4a-9c3d-2e1f0a9b8c7d"
	sfileSlides2    = "0192f3c1-b002-7b4a-9c3d-2e1f0a9b8c7d"
	sfileReadingPDF = "0192f3c1-b003-7b4a-9c3d-2e1f0a9b8c7d"
	sfileHandout    = "0192f3c1-b004-7b4a-9c3d-2e1f0a9b8c7d"
	sfileAnswers    = "0192f3c1-b005-7b4a-9c3d-2e1f0a9b8c7d"
	sfileWithhel    = "0192f3c1-b006-7b4a-9c3d-2e1f0a9b8c7d"
)

// sfile is a file of a version as the search tests' Core lists it: its
// id, name, type and bytes, its text version, nil for none, and the PDF
// Core made of it (its rendition, done), nil for none.
type sfile struct {
	id, name, ct string
	data         []byte
	text         *core.TextView
	pdf          []byte
}

// sversion is a version of a document: its files and its own text.
type sversion struct {
	id     string
	body   string
	files  []sfile
	purged bool
}

// sdoc is a document of the course: its published version, and, for a
// seat that reads drafts, a newer one; a draft has no published one.
// withheld is listed to students but not given them, as instructions
// whose assignment is withdrawn meanwhile.
type sdoc struct {
	id, title, kind string
	published       *sversion
	latest          *sversion
	withheld        bool
	purged          bool
	// sortOrder is its place in the course, as staff set it.
	sortOrder int
}

// searchCore is Core as the search reads it, for one seat: document_list
// lists the documents the seat may read, document_get gives each the
// version it reads (the published one, or for staff the latest), and the
// files are served from its own server, which counts what it is asked
// for, and the PDFs Core made of them. Every call is counted.
type searchCore struct {
	t     *testing.T
	staff bool
	srv   *httptest.Server

	mu    sync.Mutex
	docs  []*sdoc
	calls map[string]int
	hits  map[string]int
	files map[string]sfile
	// refuseList is the code document_list is refused with, "" for none.
	refuseList string
	// texts are the text versions document_text gives, by file, whose
	// document_get gives none whole; failText how many of its calls fail
	// first.
	texts    map[string]string
	failText int
}

func newSearchCore(t *testing.T, docs ...*sdoc) *searchCore {
	c := &searchCore{t: t, docs: docs, calls: map[string]int{}, hits: map[string]int{}, files: map[string]sfile{}}
	for _, d := range docs {
		for _, v := range []*sversion{d.published, d.latest} {
			if v == nil {
				continue
			}
			for _, f := range v.files {
				c.files[f.id] = f
			}
		}
	}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/")
		c.mu.Lock()
		c.hits[id]++
		f, ok := c.files[strings.TrimSuffix(id, ".pdf")]
		c.mu.Unlock()
		switch {
		case !ok || strings.HasSuffix(id, ".pdf") && f.pdf == nil:
			http.NotFound(w, r)
		case strings.HasSuffix(id, ".pdf"):
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(f.pdf)
		default:
			w.Header().Set("Content-Type", f.ct)
			_, _ = w.Write(f.data)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// as is a copy of the Core for a seat that reads drafts, or not, sharing
// its documents, files and counts.
func (c *searchCore) as(staff bool) *searchCore {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &searchCore{t: c.t, staff: staff, srv: c.srv, docs: c.docs, calls: c.calls, hits: c.hits, files: c.files}
}

func (c *searchCore) count(tool string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[tool]
}

func (c *searchCore) fetched(fileID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits[fileID]
}

// readable is the version of d the seat reads, nil for none.
func (c *searchCore) readable(d *sdoc) *sversion {
	if c.staff && d.latest != nil {
		return d.latest
	}
	return d.published
}

func (c *searchCore) respond(_ context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
	var a struct {
		DocumentID string `json:"document_id"`
		VersionID  string `json:"version_id"`
	}
	_ = json.Unmarshal(args, &a)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[tool]++
	switch tool {
	case "document_list":
		if c.refuseList != "" {
			return &core.Envelope{Status: core.StatusDenied, Error: &core.Error{Code: c.refuseList, Message: "no"}}, nil
		}
		var docs []string
		for _, d := range c.docs {
			if c.readable(d) == nil && !d.purged {
				continue
			}
			purged := "null"
			if d.purged {
				purged = `"2026-09-30T00:00:00Z"`
			}
			docs = append(docs, fmt.Sprintf(`{"id":%q,"kind":%q,"title":%q,"sort_order":%d,"status":"active","created_at":"2026-09-01T00:00:00Z","purged_at":%s}`,
				d.id, d.kind, d.title, d.sortOrder, purged))
		}
		return executed(`{"documents":[` + strings.Join(docs, ",") + `]}`), nil
	case "document_get":
		for _, d := range c.docs {
			v := c.readable(d)
			if d.id != a.DocumentID || v == nil || d.withheld && !c.staff {
				continue
			}
			if a.VersionID != "" {
				switch {
				case d.published != nil && a.VersionID == d.published.id:
					v = d.published
				case c.staff && d.latest != nil && a.VersionID == d.latest.id:
				default:
					continue
				}
			}
			return executed(c.documentResult(d, v)), nil
		}
		return &core.Envelope{Status: core.StatusFailed, Error: &core.Error{Code: core.CodeNotFound, Message: "no such document"}}, nil
	case core.ToolText:
		var ta struct {
			FileID string `json:"file_id"`
		}
		_ = json.Unmarshal(args, &ta)
		body, ok := c.texts[ta.FileID]
		if c.failText > 0 || !ok {
			c.failText--
			return &core.Envelope{Status: core.StatusFailed, Error: &core.Error{Code: "internal", Message: "try again"}}, nil
		}
		f := c.files[ta.FileID]
		view := *f.text
		view.Body = &body
		raw, _ := json.Marshal(core.TextPart{DocumentID: a.DocumentID, VersionID: a.VersionID, FileID: ta.FileID, Text: view, Part: 1, Parts: 1})
		return executed(string(raw)), nil
	}
	return executed(`{}`), nil
}

// documentResult is document_get's result of v, a version of d, as Core
// gives it: its files with their URLs, their text versions, bodies
// whole, and their renditions.
func (c *searchCore) documentResult(d *sdoc, v *sversion) string {
	var files []string
	for i, f := range v.files {
		text := ""
		if f.text != nil {
			raw, _ := json.Marshal(f.text)
			text = `,"text":` + string(raw)
		}
		if f.pdf != nil {
			text += fmt.Sprintf(`,"rendition":{"state":"done","byte_size":%d,"download_url":%q}`, len(f.pdf), c.srv.URL+"/"+f.id+".pdf")
		}
		files = append(files, fmt.Sprintf(`{"id":%q,"position":%d,"filename":%q,"content_type":%q,"byte_size":%d,"checksum":"sha256:%x","download_url":%q%s}`,
			f.id, i+1, f.name, f.ct, len(f.data), len(f.data)+i, c.srv.URL+"/"+f.id, text))
	}
	if v.purged {
		files = nil
	}
	body, purged := "", ""
	if v.body != "" {
		raw, _ := json.Marshal(v.body)
		body = `"body_md":` + string(raw) + `,`
	}
	if v.purged {
		purged = `"purged":{"at":"2026-09-30T00:00:00Z","by_actor_id":"0192f3c1-0000-7b4a-9c3d-2e1f0a9b8c7d","reason":"uploaded by mistake"},`
	}
	return fmt.Sprintf(`{"id":%q,"kind":%q,"title":%q,"sort_order":0,"status":"active","created_at":"2026-09-01T00:00:00Z",`+
		`"version":{"id":%q,"seq":1,%s%s"files":[%s],"author_member_id":"m","created_at":"2026-09-01T00:00:00Z","published":true}}`,
		d.id, d.kind, d.title, v.id, body, purged, strings.Join(files, ","))
}

// textDone is a text version, done, of body, at revision.
func textDone(body string, revision int, source string) *core.TextView {
	return &core.TextView{Status: core.TextDone, Source: source, Model: "gpt-test", Revision: revision, Bytes: len(body), Body: &body,
		UpdatedAt: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)}
}

// course is the search tests' course: slides in two versions (the second
// a draft for staff, which alone says 二分搜尋), a reading as a PDF, a
// handout whose text version is done (its file a scan), the syllabus of
// its own text, a draft of the exam's answers, and instructions withheld
// from students.
func course() []*sdoc {
	deck := func(extra string) []byte {
		slides := []doctexttest.Slide{
			{Title: "Week 3: Sorting", Body: []doctexttest.Bullet{{Text: "Merge sort splits the list in two"}, {Text: "then merges the halves", Level: 1}}},
			{Title: "排序的複雜度", Body: []doctexttest.Bullet{{Text: "合併排序：O(n log n)"}}, Notes: "Ask who has seen quicksort."},
		}
		if extra != "" {
			slides = append(slides, doctexttest.Slide{Title: "Next week", Body: []doctexttest.Bullet{{Text: extra}}})
		}
		return doctexttest.PPTX(slides...)
	}
	handout := "## 第 1 頁\n穩定排序保留相等元素的次序。\n\n## 第 2 頁\n[圖：插入排序的步驟]\n插入排序是穩定的。"
	return []*sdoc{
		{id: docSlides, title: "Week 3 slides", kind: "material",
			published: &sversion{id: verSlides1, files: []sfile{{id: sfileSlides1, name: "week3.pptx", ct: doctexttest.PPTXType, data: deck("")}}},
			latest:    &sversion{id: verSlides2, files: []sfile{{id: sfileSlides2, name: "week3.pptx", ct: doctexttest.PPTXType, data: deck("二分搜尋 binary search")}}}},
		{id: docReading, title: "Reading 3", kind: "material",
			published: &sversion{id: verReading, files: []sfile{{id: sfileReadingPDF, name: "reading3.pdf", ct: "application/pdf",
				data: doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"Reading 3: stable sorting"}}, doctexttest.PDFPage{Lines: []string{"Quicksort is not stable."}})}}}},
		{id: docHandout, title: "Handout", kind: "material",
			published: &sversion{id: verHandout, files: []sfile{{id: sfileHandout, name: "handout.pdf", ct: "application/pdf",
				data: doctexttest.PDF(doctexttest.PDFPage{Image: true}), text: textDone(handout, 2, core.SourceAI)}}}},
		{id: docSyllabus, title: "Syllabus", kind: "material",
			published: &sversion{id: verSyll, body: "# Syllabus\n\nThe final exam covers chapters 1 to 5."}},
		{id: docAnswers, title: "Exam answers", kind: "material",
			latest: &sversion{id: verAnswers, files: []sfile{{id: sfileAnswers, name: "answers.md", ct: "text/markdown",
				data: []byte("# 期末考答案\n\n1. merge sort answer key: O(n log n)")}}}},
		{id: docWithheld, title: "HW2 instructions", kind: "instructions", withheld: true,
			published: &sversion{id: verWithhel, files: []sfile{{id: sfileWithhel, name: "hw2.md", ct: "text/markdown",
				data: []byte("HW2: implement merge sort, and say whether it is stable.")}}}},
	}
}

// searchSet is a tutor's reads with the search, declared for OpenAI: made
// once, and shared, since a Set does not change and the tests make
// hundreds of calls through it.
func searchSet(t testing.TB) *Set {
	t.Helper()
	builtSearchSet.Lock()
	defer builtSearchSet.Unlock()
	if builtSearchSet.s != nil {
		return builtSearchSet.s
	}
	s, err := snapshot(t).Build(tutorPerms, config.Tools{}, ReadOnly, toolschema.OpenAI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s, err = s.WithSearch(config.Tools{}, toolschema.OpenAI, nil); err != nil {
		t.Fatal(err)
	}
	builtSearchSet.s = s
	return s
}

var builtSearchSet struct {
	sync.Mutex
	s *Set
}

// searchRunner is a runner of c, keeping its search in index, for one
// answer (scope), a model that takes files or not.
func searchRunner(c *searchCore, index store.SearchIndex, scope *SearchScope, fileInput bool) Runner {
	return Runner{Client: core.NewClient(&fakeCore{respond: c.respond}), Files: NewHTTPFetcher(c.srv.Client()), Texts: NewTextCache(0),
		Index: index, Search: scope, FileInput: fileInput}
}

// searchResultOf is a search's result, as the model reads it.
type searchResultOf struct {
	Status string `json:"status"`
	Note   string `json:"note"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Result struct {
		Query string `json:"query"`
		Hits  []struct {
			DocumentID string    `json:"document_id"`
			Title      string    `json:"title"`
			Kind       string    `json:"kind"`
			VersionID  string    `json:"version_id"`
			FileID     string    `json:"file_id"`
			File       string    `json:"file"`
			Where      string    `json:"where"`
			TextSource string    `json:"text_source"`
			Excerpt    string    `json:"excerpt"`
			Read       *nextPart `json:"read"`
			ReadNote   string    `json:"read_note"`
		} `json:"hits"`
		Page     int       `json:"page"`
		More     bool      `json:"more"`
		Next     *nextPart `json:"next"`
		Searched struct {
			Documents   int `json:"documents"`
			Files       int `json:"files"`
			WithoutText int `json:"without_text"`
			NotYet      int `json:"not_yet_read"`
		} `json:"searched"`
	} `json:"result"`
}

// searchFor runs one call of SearchTool with args, and returns its result.
func searchFor(t *testing.T, r Runner, args string) (searchResultOf, llm.Part) {
	t.Helper()
	parts, err := searchSet(t).Run(context.Background(), r, courseID, []llm.Part{call("s", SearchTool, args)})
	if err != nil {
		t.Fatal(err)
	}
	var out searchResultOf
	if err := json.Unmarshal([]byte(parts[0].Content), &out); err != nil {
		t.Fatalf("the result is not JSON: %v\n%s", err, parts[0].Content)
	}
	if strings.Contains(parts[0].Content, "download_url") || strings.Contains(parts[0].Content, "http://") {
		t.Fatalf("a URL reached the model: %s", parts[0].Content)
	}
	return out, parts[0]
}

// readHit makes a hit's read call on r, as the model would, and returns
// its result.
func readHit(t *testing.T, r Runner, read *nextPart) map[string]any {
	t.Helper()
	if read == nil {
		t.Fatal("the hit names no call that reads it")
	}
	args, _ := json.Marshal(read.Arguments)
	parts, err := searchSet(t).Run(context.Background(), r, courseID, []llm.Part{call("g", read.Tool, string(args))})
	if err != nil || parts[0].IsError {
		t.Fatalf("the hit's read: %v %+v", err, parts)
	}
	return contentOf(t, parts[0])
}

// hitsOf are a result's hits, each "document version where".
func hitsOf(res searchResultOf) []string {
	var out []string
	for _, h := range res.Result.Hits {
		out = append(out, strings.TrimSpace(h.Title+" "+h.Where))
	}
	return out
}

// TestSearchFindsPassagesInChineseAndEnglish: a student's search finds
// the passage of a Traditional Chinese question in a deck's slide, of an
// English one in a PDF's page and in a deck, of a text version's page by
// its own words, and of the syllabus's own text; each hit says where it
// is, whose the text is, and gives an excerpt and the call that reads it.
func TestSearchFindsPassagesInChineseAndEnglish(t *testing.T) {
	c := newSearchCore(t, course()...)
	r := searchRunner(c, memstore.New(), &SearchScope{}, false)

	res, part := searchFor(t, r, `{"query":"合併排序的複雜度"}`)
	if part.IsError || res.Status != "executed" || len(res.Result.Hits) == 0 {
		t.Fatalf("合併排序的複雜度: %s", part.Content)
	}
	h := res.Result.Hits[0]
	if h.DocumentID != docSlides || h.VersionID != verSlides1 || h.FileID != sfileSlides1 || h.File != "week3.pptx" || h.Where != "slide 2" ||
		!strings.Contains(h.Excerpt, "排序的複雜度") || h.TextSource != "the runtime's text of the file" {
		t.Errorf("the first hit: %+v", h)
	}
	if want := map[string]any{"document_id": docSlides, "version_id": verSlides1, FileIDArg: sfileSlides1, FilePagesArg: "2"}; h.Read == nil ||
		h.Read.Tool != FilePartTool || fmt.Sprint(h.Read.Arguments) != fmt.Sprint(want) || h.ReadNote != "" {
		t.Errorf("the call that reads it: %+v", h.Read)
	}
	// Made as it is, the call gives that slide's text alone.
	if got := readHit(t, r, h.Read); got["file_text"] != "## Slide 2: 排序的複雜度\n- 合併排序：O(n log n)\nNotes: Ask who has seen quicksort." ||
		got["file"].(map[string]any)["part_holds"] != "slide 2" {
		t.Errorf("the hit's read gives %v", got)
	}
	if got := res.Result.Searched; got.Documents != 4 || got.Files != 4 || got.WithoutText != 0 || got.NotYet != 0 {
		t.Errorf("searched %+v: a student reads four documents (not the draft, nor the instructions withheld), of four texts", got)
	}

	for q, want := range map[string]string{
		"merge sort":           "Week 3 slides slide 1",
		"Is quicksort stable?": "Reading 3 page 2",
		"插入排序":                 "Handout page 2",
		"final exam":           "Syllabus",
	} {
		res, part := searchFor(t, r, `{"query":`+string(must(json.Marshal(q)))+`}`)
		if part.IsError || len(res.Result.Hits) == 0 || hitsOf(res)[0] != want {
			t.Errorf("%s: %v, want %s first\n%s", q, hitsOf(res), want, part.Content)
		}
	}
	res, _ = searchFor(t, r, `{"query":"插入排序"}`)
	if h := res.Result.Hits[0]; h.TextSource != "AI transcription (gpt-test)" || !strings.Contains(h.Excerpt, "插入排序是穩定的") {
		t.Errorf("a text version's hit: %+v", h)
	}
	res, _ = searchFor(t, r, `{"query":"final exam"}`)
	if h := res.Result.Hits[0]; h.TextSource != "the version's own text (body_md)" || h.FileID != "" ||
		fmt.Sprint(h.Read.Arguments) != fmt.Sprint(map[string]any{"document_id": docSyllabus, "version_id": verSyll}) {
		t.Errorf("the syllabus's hit: %+v", h)
	}
	res, part = searchFor(t, r, `{"query":"photosynthesis"}`)
	if part.IsError || len(res.Result.Hits) != 0 || !strings.Contains(res.Note, "no passage of the documents searched holds these words") {
		t.Errorf("a word in no document: %s", part.Content)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestSearchKeepsToWhatTheSeatMayRead: the index is the course's, shared
// by every seat, and holds the staff's drafts once staff search; a
// student's search still finds nothing of a draft, of a version newer
// than the published one, or of a document Core lists but no longer
// gives it, which it says it could not read.
func TestSearchKeepsToWhatTheSeatMayRead(t *testing.T) {
	c := newSearchCore(t, course()...)
	index := memstore.New()
	staff := searchRunner(c.as(true), index, &SearchScope{}, false)
	for q, want := range map[string]string{"期末考答案": "Exam answers", "二分搜尋": "Week 3 slides slide 3", "implement merge sort": "HW2 instructions"} {
		res, part := searchFor(t, staff, `{"query":`+string(must(json.Marshal(q)))+`}`)
		if len(res.Result.Hits) == 0 || hitsOf(res)[0] != want {
			t.Fatalf("staff's %s: %v\n%s", q, hitsOf(res), part.Content)
		}
	}
	// The index now holds the draft, the slides' newer version and the
	// withheld instructions.
	have, err := index.UseSearchFiles(context.Background(), courseID, []store.SearchFileKey{{VersionID: verAnswers, Key: sfileAnswers},
		{VersionID: verSlides2, Key: sfileSlides2}, {VersionID: verWithhel, Key: sfileWithhel}}, time.Time{})
	if err != nil || len(have) != 3 {
		t.Fatalf("the index after staff searched: %v %v", have, err)
	}

	student := searchRunner(c.as(false), index, &SearchScope{}, false)
	for _, q := range []string{"期末考答案", "answer key", "二分搜尋", "binary search", "implement merge sort"} {
		res, part := searchFor(t, student, `{"query":`+string(must(json.Marshal(q)))+`}`)
		for _, h := range res.Result.Hits {
			if h.DocumentID == docAnswers || h.VersionID == verSlides2 || h.DocumentID == docWithheld ||
				strings.Contains(h.Excerpt, "二分") || strings.Contains(h.Excerpt, "answer key") || strings.Contains(h.Excerpt, "HW2") {
				t.Errorf("a student's %s found what it may not read: %+v", q, h)
			}
		}
		if !strings.Contains(res.Note, "1 document listed could not be read just now") {
			t.Errorf("a student's %s does not say the withheld document was not read: %s", q, part.Content)
		}
	}
	res, _ := searchFor(t, student, `{"query":"merge sort"}`)
	for _, h := range res.Result.Hits {
		if h.DocumentID == docSlides && h.VersionID != verSlides1 {
			t.Errorf("the slides' hit is of version %s, not the published one", h.VersionID)
		}
	}
	if len(res.Result.Hits) == 0 || res.Result.Hits[0].VersionID != verSlides1 {
		t.Errorf("merge sort, for a student: %+v", res.Result.Hits)
	}
}

// TestSearchBuildsItsIndexOnce: the first search reads each file of the
// course once, Core's text version in place of a scanned file's own,
// within the answer, and keeps it; a later answer's search reads its seat's
// documents from Core again but no file; one answer's searches read the
// seat's documents once. A text version edited since is read again, and
// what it said before is found no more.
func TestSearchBuildsItsIndexOnce(t *testing.T) {
	docs := course()
	c := newSearchCore(t, docs...)
	index := memstore.New()
	scope := &SearchScope{}
	r := searchRunner(c, index, scope, false)
	var seen []SearchStats
	r.Searched = func(s SearchStats) { seen = append(seen, s) }

	searchFor(t, r, `{"query":"merge sort"}`)
	for _, f := range []string{sfileSlides1, sfileReadingPDF} {
		if n := c.fetched(f); n != 1 {
			t.Errorf("file %s fetched %d times by the first search", f, n)
		}
	}
	if n := c.fetched(sfileHandout); n != 0 {
		t.Errorf("the handout, whose text version is done, was fetched %d times", n)
	}
	if lists, gets := c.count("document_list"), c.count("document_get"); lists != 1 || gets != 5 {
		t.Errorf("the first search called document_list %d times and document_get %d, want 1 and 5", lists, gets)
	}
	if s := seen[0]; s.Outcome != "hits" || !s.ScopeRead || s.Indexed != 4 || s.Empty != 0 || s.Documents != 4 || s.Files != 4 || s.Hits == 0 {
		t.Errorf("the first search's stats: %+v", s)
	}

	// The same answer: no call to Core, no file read.
	searchFor(t, r, `{"query":"排序"}`)
	if lists, gets := c.count("document_list"), c.count("document_get"); lists != 1 || gets != 5 {
		t.Errorf("the answer's second search called document_list %d times and document_get %d in all", lists, gets)
	}
	if s := seen[1]; s.ScopeRead || s.Indexed != 0 {
		t.Errorf("the second search's stats: %+v", s)
	}

	// Another answer: the seat's documents read again, no file.
	r2 := searchRunner(c, index, &SearchScope{}, false)
	searchFor(t, r2, `{"query":"排序"}`)
	if lists, gets := c.count("document_list"), c.count("document_get"); lists != 2 || gets != 10 {
		t.Errorf("another answer's search called document_list %d times and document_get %d in all", lists, gets)
	}
	for _, f := range []string{sfileSlides1, sfileReadingPDF} {
		if n := c.fetched(f); n != 1 {
			t.Errorf("file %s fetched %d times in all", f, n)
		}
	}

	// Staff edit the handout's text: the next answer reads it again, and
	// finds the new words and not the old.
	edited := "## 第 1 頁\n希爾排序以間隔分組。"
	c.mu.Lock()
	docs[2].published.files[0].text = textDone(edited, 3, core.SourceStaff)
	c.mu.Unlock()
	r3 := searchRunner(c, index, &SearchScope{}, false)
	res, _ := searchFor(t, r3, `{"query":"希爾"}`)
	if len(res.Result.Hits) != 1 || res.Result.Hits[0].DocumentID != docHandout || res.Result.Hits[0].TextSource != "edited by staff" {
		t.Errorf("the edited text: %+v", res.Result.Hits)
	}
	if res, _ = searchFor(t, r3, `{"query":"插入"}`); len(res.Result.Hits) != 0 {
		t.Errorf("the text before the edit is still found: %+v", res.Result.Hits)
	}
}

// TestSearchSaysWhatItCannotRead: a scanned PDF with no text version is
// kept with no text, and said to be so, at this search and the next,
// never read again; the time for reading files spent, the files left are
// said not to be read yet, and a later search reads them.
func TestSearchSaysWhatItCannotRead(t *testing.T) {
	docs := course()
	docs[2].published.files[0].text = &core.TextView{Status: "pending", UpdatedAt: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)}
	c := newSearchCore(t, docs...)
	index := memstore.New()
	r := searchRunner(c, index, nil, false)
	res, part := searchFor(t, r, `{"query":"插入排序"}`)
	if res.Result.Searched.WithoutText != 1 || !strings.Contains(res.Note, "1 file has no text the search can read") {
		t.Errorf("a scan with no text version: %s", part.Content)
	}
	res, _ = searchFor(t, r, `{"query":"插入排序"}`)
	if res.Result.Searched.WithoutText != 1 || c.fetched(sfileHandout) != 1 {
		t.Errorf("the next search: %+v, the scan fetched %d times", res.Result.Searched, c.fetched(sfileHandout))
	}

	// No time left to read files: none is read, each is said not to be.
	fresh := memstore.New()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	short := searchRunner(c, fresh, nil, false)
	view, _, err := short.scope(ctx, courseID)
	if err != nil {
		t.Fatal(err)
	}
	var files []*scopeFile
	for _, d := range view.docs {
		files = append(files, d.files...)
	}
	// A deadline already past: cancelled at once, whatever the load, not
	// when a timer fires.
	spent, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer stop()
	b := short.index(spent, courseID, files)
	if b.notYet != len(files) || len(b.done) != 0 {
		t.Errorf("with no time left: %+v", b)
	}
	res, _ = searchFor(t, short, `{"query":"merge sort"}`)
	if len(res.Result.Hits) == 0 || res.Result.Searched.NotYet != 0 {
		t.Errorf("the search after: %+v", res.Result)
	}
}

// TestSearchDropsWhatCoreSaysIsPurged: a version Core gives the seat as
// purged (its tombstone) is dropped from the index as the search reads it,
// and found no more; so is a document listed as purged, which today's
// Core does not list (a purged document is archived, and lists to no
// search: the worker's events, or retention, drop it).
func TestSearchDropsWhatCoreSaysIsPurged(t *testing.T) {
	docs := course()
	c := newSearchCore(t, docs...)
	index := memstore.New()
	searchFor(t, searchRunner(c, index, nil, false), `{"query":"stable"}`)
	keys := []store.SearchFileKey{{VersionID: verReading, Key: sfileReadingPDF}, {VersionID: verSyll, Key: store.SearchBody}}
	if have, err := index.UseSearchFiles(context.Background(), courseID, keys, time.Time{}); err != nil || len(have) != 2 {
		t.Fatalf("indexed: %v %v", have, err)
	}
	c.mu.Lock()
	docs[1].published.purged = true
	docs[3].purged = true
	c.mu.Unlock()
	res, part := searchFor(t, searchRunner(c, index, nil, false), `{"query":"stable"}`)
	for _, h := range res.Result.Hits {
		if h.DocumentID == docReading || h.DocumentID == docSyllabus {
			t.Errorf("a purged document found: %+v", h)
		}
	}
	if have, err := index.UseSearchFiles(context.Background(), courseID, keys, time.Time{}); err != nil || len(have) != 0 {
		t.Errorf("the index after the purges: %v %v\n%s", have, err, part.Content)
	}
}

// TestSearchPagesAndPoints: hits come a page at a time, next the call
// that gives the next page; a long text version's hit names the part of
// it document_get gives the passage in, which that call gives; a PDF's,
// to a model that takes files, its page.
func TestSearchPagesAndPoints(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "## 第 %d 頁\n第 %d 週的作業練習。%s\n\n", i, i, strings.Repeat("這一頁說明演算法的細節與例子。", 50))
	}
	long := strings.TrimSpace(b.String())
	docs := course()
	docs[2].published.files[0].text = textDone(long, 5, core.SourceAI)
	c := newSearchCore(t, docs...)
	r := searchRunner(c, memstore.New(), &SearchScope{}, false)

	var all []string
	next := `{"query":"作業練習","limit":4}`
	for page := 1; ; page++ {
		res, part := searchFor(t, r, next)
		if res.Result.Page != page || len(res.Result.Hits) == 0 || len(res.Result.Hits) > 4 {
			t.Fatalf("page %d: %s", page, part.Content)
		}
		all = append(all, hitsOf(res)...)
		if !res.Result.More {
			if res.Result.Next != nil {
				t.Errorf("the last page names a next: %+v", res.Result.Next)
			}
			break
		}
		if res.Result.Next == nil || res.Result.Next.Tool != SearchTool || res.Result.Next.Arguments["page"] != float64(page+1) ||
			res.Result.Next.Arguments["limit"] != float64(4) || res.Result.Next.Arguments["query"] != "作業練習" {
			t.Fatalf("page %d's next: %+v", page, res.Result.Next)
		}
		args, _ := json.Marshal(res.Result.Next.Arguments)
		next = string(args)
	}
	if len(all) != 40 || len(slices.Compact(slices.Sorted(slices.Values(all)))) != 40 {
		t.Errorf("40 pages hold the words, each once: %d hits, %v", len(all), all)
	}
	res, part := searchFor(t, r, `{"query":"作業練習","limit":4,"page":11}`)
	if part.IsError || len(res.Result.Hits) != 0 || !strings.Contains(res.Note, "there are 40 hits: no page 11 of them") {
		t.Errorf("a page past the last: %s", part.Content)
	}

	// The 33rd page's hit names the part that holds it, which gives it.
	res, _ = searchFor(t, r, `{"query":"第 33 週的作業練習","limit":1}`)
	h := res.Result.Hits[0]
	if h.Where != "page 33" || h.Read == nil || h.Read.Arguments[FilePartArg] == nil {
		t.Fatalf("page 33's hit: %+v", h)
	}
	args, _ := json.Marshal(h.Read.Arguments)
	parts, err := searchSet(t).Run(context.Background(), r, courseID, []llm.Part{call("g", h.Read.Tool, string(args))})
	if err != nil || parts[0].IsError {
		t.Fatalf("the hit's read: %v %+v", err, parts)
	}
	got := contentOf(t, parts[0])
	text, _ := got["file_text"].(string)
	if !strings.Contains(text, "## 第 33 頁\n第 33 週的作業練習") {
		t.Errorf("the part read does not hold page 33: %v", got["file"])
	}

	// A PDF, to a model that takes files: its page.
	rf := searchRunner(c, memstore.New(), &SearchScope{}, true)
	res, _ = searchFor(t, rf, `{"query":"quicksort stable"}`)
	if h := res.Result.Hits[0]; h.DocumentID != docReading || h.Read.Arguments[FilePagesArg] != "2" || h.Read.Arguments[FilePartArg] != nil {
		t.Errorf("a PDF's hit, to a model that takes files: %+v", h)
	}
}

// oneFile is a course of one document, published, of one file.
func oneFile(name, ct string, data []byte, text *core.TextView) []*sdoc {
	return []*sdoc{{id: docReading, title: "Reading", kind: "material",
		published: &sversion{id: verReading, files: []sfile{{id: sfileReadingPDF, name: name, ct: ct, data: data, text: text}}}}}
}

// withoutArg is read without its argument arg.
func withoutArg(read *nextPart, arg string) *nextPart {
	args := map[string]any{}
	for k, v := range read.Arguments {
		if k != arg {
			args[k] = v
		}
	}
	return &nextPart{Tool: read.Tool, Arguments: args}
}

// TestSearchHitsReadTheirPassage: in a long text of no pages, whose
// passages and parts are cut apart (a text file, and a text version with
// no page headings), every word found near where a part begins, one at the
// end of each paragraph, is in the part its hit's read gives: a passage
// never runs from one part into the next.
func TestSearchHitsReadTheirPassage(t *testing.T) {
	const paragraphs = 200
	var b strings.Builder
	var starts []int
	for i := range paragraphs {
		starts = append(starts, b.Len())
		fmt.Fprintf(&b, "Paragraph %d. %s zq%03dx\n\n", i, strings.Repeat("Sorting puts things in order, step by step. ", 8), i)
	}
	long := strings.TrimSpace(b.String())
	// The paragraphs around where each part but the first begins.
	cuts := splitText(long, nil, Runner{}.withDefaults().partBudget())
	if len(cuts) < 3 {
		t.Fatalf("the text is %d parts; the test wants several", len(cuts))
	}
	var near []int
	for _, p := range cuts[1:] {
		i := sort.SearchInts(starts, p.start+1) - 1
		for k := max(i-4, 0); k <= min(i+4, paragraphs-1); k++ {
			near = append(near, k)
		}
	}
	for name, docs := range map[string][]*sdoc{
		"a text file":    oneFile("notes.md", "text/markdown", []byte(long), nil),
		"a text version": oneFile("scan.pdf", "application/pdf", doctexttest.PDF(doctexttest.PDFPage{Image: true}), textDone(long, 1, core.SourceStaff)),
	} {
		c := newSearchCore(t, docs...)
		r := searchRunner(c, memstore.New(), &SearchScope{}, false)
		for _, i := range near {
			word := fmt.Sprintf("zq%03dx", i)
			res, part := searchFor(t, r, `{"query":"`+word+`","limit":1}`)
			if len(res.Result.Hits) != 1 {
				t.Fatalf("%s, %s: %s", name, word, part.Content)
			}
			got := readHit(t, r, res.Result.Hits[0].Read)
			if text, _ := got["file_text"].(string); !strings.Contains(text, word) {
				t.Errorf("%s, %s: read %v gives a part without it (%v)", name, word, res.Result.Hits[0].Read.Arguments, got["file"])
			}
		}
	}
}

// TestSearchPointsAtTheSlideTheModelReads: a deck of pictures, to a model
// that takes no files on a runtime with LibreOffice and OCR, is given with
// what OCR read of its slides after each, which the index, reading no
// pictures, does not hold: the model's parts are not the index's. A hit is
// read by its slide, which gives that slide's text, OCR's with it; to a
// model that takes files, its slide as LibreOffice draws it.
func TestSearchPointsAtTheSlideTheModelReads(t *testing.T) {
	var slides []doctexttest.Slide
	for i := 1; i <= 60; i++ {
		body := fmt.Sprintf("point %d", i)
		if i == 33 {
			body = "the zebra crossing theorem"
		}
		slides = append(slides, doctexttest.Slide{Title: fmt.Sprintf("Slide %d", i), Body: []doctexttest.Bullet{{Text: body}}, Images: 1})
	}
	var ocrText strings.Builder
	var secs []store.OCRSection
	for i := 1; i <= maxPictured; i++ {
		secs = append(secs, store.OCRSection{N: i, Offset: ocrText.Len()})
		fmt.Fprintf(&ocrText, "## Page %d\n%s\n\n", i, strings.Repeat("What the picture on this slide shows, in words. ", 35))
	}
	read := done(strings.TrimSpace(ocrText.String()), secs...)
	c := newSearchCore(t, oneFile("week5.pptx", doctexttest.PPTXType, doctexttest.PPTX(slides...), nil)...)
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(60)}}
	r := searchRunner(c, memstore.New(), &SearchScope{}, false)
	r.Office, r.OCR = o, &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State { return read }}

	res, part := searchFor(t, r, `{"query":"zebra crossing"}`)
	if len(res.Result.Hits) != 1 {
		t.Fatalf("zebra crossing: %s", part.Content)
	}
	h := res.Result.Hits[0]
	if h.Where != "slide 33" || h.Read.Arguments[FilePagesArg] != "33" || h.Read.Arguments[FilePartArg] != nil || h.ReadNote != "" {
		t.Fatalf("the hit: %+v %+v", h, h.Read)
	}
	got := readHit(t, r, h.Read)
	rec := got["file"].(map[string]any)
	if text, _ := got["file_text"].(string); !strings.HasPrefix(text, "## Slide 33: Slide 33\n- the zebra crossing theorem\n[image]\n"+ocrMark) ||
		strings.Contains(text, "Slide 34") || rec["part_holds"] != "slide 33" || rec["given_as"] != givenText {
		t.Errorf("the hit's read gives %v\n%q", rec, got["file_text"])
	}
	// Read whole, the deck is in parts that are not the index's: its first
	// does not hold slide 33.
	whole := readHit(t, r, withoutArg(h.Read, FilePagesArg))
	if n, _ := whole["file"].(map[string]any)["parts"].(float64); n < 3 || strings.Contains(whole["file_text"].(string), "zebra") {
		t.Errorf("the deck read from its start: %v", whole["file"])
	}

	// To a model that takes files: slide 33 as LibreOffice draws it.
	rf := searchRunner(c, memstore.New(), &SearchScope{}, true)
	rf.Office = o
	res, _ = searchFor(t, rf, `{"query":"zebra crossing"}`)
	if h := res.Result.Hits[0]; h.Read.Arguments[FilePagesArg] != "33" {
		t.Fatalf("to a model that takes files: %+v", h.Read)
	}
	args, _ := json.Marshal(res.Result.Hits[0].Read.Arguments)
	parts, err := searchSet(t).Run(context.Background(), rf, courseID, []llm.Part{call("g", FilePartTool, string(args))})
	if err != nil || len(parts) != 2 || parts[1].File == nil || fileRecordOf(t, parts[0])["part_holds"] != "slide 33" {
		t.Errorf("its read, to a model that takes files: %v %+v", err, parts)
	}
}

// TestSearchSaysWhenThePageIsNotKnown: a Word file, to a model that takes
// files on a runtime with LibreOffice, is given as its PDF's pages, which
// the index's reading of its text does not know: the hit says so, and its
// read gives the file from its first pages. To a model that takes no
// files, read as the index reads it, the hit's part holds the passage.
func TestSearchSaysWhenThePageIsNotKnown(t *testing.T) {
	var blocks []doctexttest.Block
	for i := range 400 {
		text := fmt.Sprintf("Paragraph %d of the notes, which say a little about sorting and searching, at some length.", i)
		if i == 380 {
			text = "The zebra crossing theorem is proved here."
		}
		blocks = append(blocks, doctexttest.Block{Text: text})
	}
	c := newSearchCore(t, oneFile("notes.docx", doctexttest.DOCXType, doctexttest.DOCX(doctexttest.Doc{Blocks: blocks}), nil)...)
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(60)}}

	rf := searchRunner(c, memstore.New(), &SearchScope{}, true)
	rf.Office = o
	res, part := searchFor(t, rf, `{"query":"zebra crossing"}`)
	if len(res.Result.Hits) != 1 {
		t.Fatalf("zebra crossing: %s", part.Content)
	}
	h := res.Result.Hits[0]
	if h.Where != "" || h.Read.Arguments[FilePagesArg] != nil || h.Read.Arguments[FilePartArg] != nil ||
		!strings.Contains(h.ReadNote, "the page this passage is on is not known") {
		t.Errorf("to a model that takes files: %+v %+v", h, h.Read)
	}
	if !strings.Contains(res.Note, "unless its read_note says otherwise") {
		t.Errorf("the note: %s", res.Note)
	}

	r := searchRunner(c, memstore.New(), &SearchScope{}, false)
	r.Office = o
	res, _ = searchFor(t, r, `{"query":"zebra crossing"}`)
	h = res.Result.Hits[0]
	if h.Read.Arguments[FilePartArg] != float64(2) || h.ReadNote != "" {
		t.Fatalf("to a model that takes no files: %+v %+v", h, h.Read)
	}
	if got := readHit(t, r, h.Read); !strings.Contains(got["file_text"].(string), "The zebra crossing theorem") {
		t.Errorf("its read gives %v", got["file"])
	}
}

// TestSearchPointsIntoCoresPDF: where LibreOffice does not convert here, a
// deck and a Word file whose PDF Core made are given to a model that takes
// files as that PDF's pages (rendered), and their hits point as for
// LibreOffice's: the deck's by its slide, which gives that page of Core's
// PDF, and the Word file's, whose page the index does not know, by saying
// so, its read giving Core's PDF from its first page. A Word file whose PDF
// Core has not made is given as its text, and its hit read by its part.
func TestSearchPointsIntoCoresPDF(t *testing.T) {
	var slides []doctexttest.Slide
	for i := 1; i <= 3; i++ {
		body := fmt.Sprintf("point %d", i)
		if i == 2 {
			body = "the zebra crossing theorem"
		}
		slides = append(slides, doctexttest.Slide{Title: fmt.Sprintf("Slide %d", i), Body: []doctexttest.Bullet{{Text: body}}})
	}
	deck := oneFile("week5.pptx", doctexttest.PPTXType, doctexttest.PPTX(slides...), nil)
	deck[0].published.files[0].pdf = corePDF(3)
	c := newSearchCore(t, deck...)
	o := &stubOffice{off: "LibreOffice is not installed"}
	r := searchRunner(c, memstore.New(), &SearchScope{}, true)
	r.Office = o
	res, part := searchFor(t, r, `{"query":"zebra crossing"}`)
	if len(res.Result.Hits) != 1 {
		t.Fatalf("zebra crossing, in a deck: %s", part.Content)
	}
	h := res.Result.Hits[0]
	if h.Where != "slide 2" || h.Read.Arguments[FilePagesArg] != "2" || h.ReadNote != "" {
		t.Fatalf("the deck's hit: %+v %+v", h, h.Read)
	}
	args, _ := json.Marshal(h.Read.Arguments)
	parts, err := searchSet(t).Run(context.Background(), r, courseID, []llm.Part{call("g", FilePartTool, string(args))})
	if err != nil || len(parts) != 2 || parts[1].File == nil || fileRecordOf(t, parts[0])["part_holds"] != "slide 2" {
		t.Fatalf("the deck's hit read: %v %+v", err, parts)
	}
	if _, ranges, _ := o.record(); fmt.Sprint(ranges) != "[[2 2]]" || !strings.HasSuffix(fmt.Sprint(o.rangeNames()), "/rendition]") {
		t.Errorf("cut %v from %v, not slide 2 of Core's PDF", ranges, o.rangeNames())
	}

	var blocks []doctexttest.Block
	for i := range 400 {
		text := fmt.Sprintf("Paragraph %d of the notes, which say a little about sorting and searching, at some length.", i)
		if i == 380 {
			text = "The zebra crossing theorem is proved here."
		}
		blocks = append(blocks, doctexttest.Block{Text: text})
	}
	notes := doctexttest.DOCX(doctexttest.Doc{Blocks: blocks})
	for _, pdf := range [][]byte{corePDF(3), nil} {
		doc := oneFile("notes.docx", doctexttest.DOCXType, notes, nil)
		doc[0].published.files[0].pdf = pdf
		r := searchRunner(newSearchCore(t, doc...), memstore.New(), &SearchScope{}, true)
		r.Office = o
		res, part := searchFor(t, r, `{"query":"zebra crossing"}`)
		if len(res.Result.Hits) != 1 {
			t.Fatalf("zebra crossing, in a Word file: %s", part.Content)
		}
		h := res.Result.Hits[0]
		if pdf == nil {
			if h.Read.Arguments[FilePartArg] != float64(2) || h.ReadNote != "" {
				t.Errorf("a Word file whose PDF Core has not made: %+v %+v", h, h.Read)
			}
			if got := readHit(t, r, h.Read); !strings.Contains(got["file_text"].(string), "The zebra crossing theorem") {
				t.Errorf("its read gives %v", got["file"])
			}
			continue
		}
		if h.Read.Arguments[FilePagesArg] != nil || h.Read.Arguments[FilePartArg] != nil ||
			!strings.Contains(h.ReadNote, "the page this passage is on is not known") {
			t.Errorf("a Word file whose PDF Core made: %+v %+v", h, h.Read)
		}
		args, _ := json.Marshal(h.Read.Arguments)
		parts, err := searchSet(t).Run(context.Background(), r, courseID, []llm.Part{call("g", FilePartTool, string(args))})
		if err != nil || len(parts) != 2 || parts[1].File == nil || string(parts[1].File.Data) != string(pdf) {
			t.Errorf("its read: %v %+v", err, parts)
		}
	}
	if converted, _, _ := o.record(); len(converted) != 0 {
		t.Errorf("LibreOffice was asked %v", converted)
	}
}

// TestSearchKeepsNoTextVersionItCouldNotRead: a file whose text version is
// done, which Core could not give just now, is not kept under that text's
// revision with what was read in its place (a scan's nothing), which would
// hide its text until it changed: it is said not to be read yet, and the
// next search reads it and finds its words.
func TestSearchKeepsNoTextVersionItCouldNotRead(t *testing.T) {
	handout := "## 第 1 頁\n穩定排序保留相等元素的次序。\n\n## 第 2 頁\n插入排序是穩定的。"
	text := textDone(handout, 2, core.SourceAI)
	text.Body = nil
	docs := oneFile("handout.pdf", "application/pdf", doctexttest.PDF(doctexttest.PDFPage{Image: true}), text)
	c := newSearchCore(t, docs...)
	c.texts = map[string]string{sfileReadingPDF: handout}
	c.failText = 1
	index := memstore.New()

	res, part := searchFor(t, searchRunner(c, index, nil, false), `{"query":"插入排序"}`)
	if len(res.Result.Hits) != 0 || res.Result.Searched.NotYet != 1 || res.Result.Searched.WithoutText != 0 {
		t.Errorf("with its text not given: %s", part.Content)
	}
	key := store.SearchFileKey{VersionID: verReading, Key: sfileReadingPDF}
	if have, err := index.UseSearchFiles(context.Background(), courseID, []store.SearchFileKey{key}, time.Time{}); err != nil || len(have) != 0 {
		t.Errorf("kept in its text's place: %v %v", have, err)
	}
	res, part = searchFor(t, searchRunner(c, index, nil, false), `{"query":"插入排序"}`)
	if len(res.Result.Hits) == 0 || res.Result.Hits[0].Where != "page 2" || res.Result.Searched.NotYet != 0 {
		t.Errorf("the next search: %s", part.Content)
	}
}

// TestSearchTiesInTheCoursesOrder: passages that match alike are given in
// the course's order, as staff set it (sort_order), before the order Core
// lists the documents in, the oldest first.
func TestSearchTiesInTheCoursesOrder(t *testing.T) {
	body := "# Week notes\n\nMerge sort splits the list in two."
	docs := []*sdoc{
		{id: docSlides, title: "Older", kind: "material", sortOrder: 2, published: &sversion{id: verSlides1, body: body}},
		{id: docReading, title: "Newer", kind: "material", sortOrder: 1, published: &sversion{id: verReading, body: body}},
		{id: docHandout, title: "Newest", kind: "material", sortOrder: 2, published: &sversion{id: verHandout, body: body}},
	}
	res, part := searchFor(t, searchRunner(newSearchCore(t, docs...), memstore.New(), nil, false), `{"query":"merge sort"}`)
	if got := strings.Join(hitsOf(res), ", "); got != "Newer, Older, Newest" {
		t.Errorf("ties in the order %s\n%s", got, part.Content)
	}
}

// TestSearchArguments: a call that does not say what to look for, or asks
// for too much, is refused before anything is read, saying what to put
// right; one with nothing to search by too.
func TestSearchArguments(t *testing.T) {
	c := newSearchCore(t, course()...)
	r := searchRunner(c, memstore.New(), nil, false)
	for args, want := range map[string]string{
		`{}`:              "query is required",
		`{"query":"   "}`: "query is required",
		`{"query":"` + strings.Repeat("字", 201) + `"}`: "query is at most 200 characters",
		`{"query":"sort","limit":0}`:                   "limit is how many hits to give, a whole number from 1 to 10",
		`{"query":"sort","limit":11}`:                  "limit is how many hits",
		`{"query":"sort","page":0}`:                    "page is which page of hits",
		`{"query":"sort","kind":"rubric"}`:             `it takes query, limit and page, and not "kind"`,
		`{"query":"?!…"}`:                              "query has no word or character to search by",
		`["sort"]`:                                     "the arguments are not a JSON object",
	} {
		_, part := searchFor(t, r, args)
		if code, msg := errorOf(t, part); !part.IsError || code != core.CodeInvalidArgument || !strings.Contains(msg, want) {
			t.Errorf("%s: %s %q", args, code, msg)
		}
	}
	if n := c.count("document_list"); n != 0 {
		t.Errorf("refused calls listed the documents %d times", n)
	}
}

// TestSearchUnavailable: with no index, the search says so; Core refusing
// the list, its code; neither reads a file.
func TestSearchUnavailable(t *testing.T) {
	c := newSearchCore(t, course()...)
	_, part := searchFor(t, searchRunner(c, nil, nil, false), `{"query":"sort"}`)
	if code, msg := errorOf(t, part); !part.IsError || code != codeUnavailable || !strings.Contains(msg, "not available here") {
		t.Errorf("no index: %s %q", code, msg)
	}
	c.refuseList = core.CodeForbidden
	_, part = searchFor(t, searchRunner(c, memstore.New(), nil, false), `{"query":"sort"}`)
	if code, msg := errorOf(t, part); !part.IsError || code != core.CodeForbidden || !strings.Contains(msg, "Core refused to list") {
		t.Errorf("the list refused: %s %q", code, msg)
	}
	if n := c.count("document_get"); n != 0 {
		t.Errorf("document_get called %d times", n)
	}
}

// TestWithSearch: the search is offered with what it reads through,
// document_list and document_get, and never without; deny takes it away
// by name or by a * entry; it is declared in every dialect; and Core
// offering a tool of its name fails the catalogue's check.
func TestWithSearch(t *testing.T) {
	cat := snapshot(t)
	for _, d := range toolschema.Dialects {
		base, err := cat.Build(tutorPerms, config.Tools{}, ReadOnly, d, nil)
		if err != nil {
			t.Fatal(err)
		}
		s, err := base.WithSearch(config.Tools{}, d, nil)
		if err != nil || !s.Has(SearchTool) || base.Has(SearchTool) || slices.Contains(s.Reads(), SearchTool) {
			t.Fatalf("%s: offered %v (%v); the set it was made from %v", d, s.Names(), err, base.Names())
		}
	}
	for name, tc := range map[string]struct {
		perms map[string]string
		cfg   config.Tools
	}{
		"no read of documents":      {map[string]string{"grade_read": "autonomous"}, config.Tools{}},
		"document_get denied":       {tutorPerms, config.Tools{Deny: []string{"document_get"}}},
		"document_list not allowed": {tutorPerms, config.Tools{Allow: []string{"document_get", "course_get"}}},
		"denied by name":            {tutorPerms, config.Tools{Deny: []string{SearchTool}}},
		"denied by a * entry":       {tutorPerms, config.Tools{Deny: []string{"course_*"}}},
		"mode none":                 {tutorPerms, config.Tools{Mode: "none"}},
	} {
		base, err := cat.Build(tc.perms, tc.cfg, ReadOnly, toolschema.OpenAI, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s, err := base.WithSearch(tc.cfg, toolschema.OpenAI, nil); err != nil || s.Has(SearchTool) {
			t.Errorf("%s: offered %v (%v)", name, s.Names(), err)
		}
	}
	taken := *cat
	taken.Tools = cloneTools(cat.Tools)
	taken.Tools[SearchTool] = CatalogueTool{Name: SearchTool, Kind: KindRead, InputSchema: json.RawMessage(`{"type":"object"}`)}
	if err := taken.Check(); err == nil || !strings.Contains(err.Error(), "the name of the runtime's own tool") {
		t.Errorf("Check of a catalogue that offers %s: %v", SearchTool, err)
	}
}

func cloneTools(m map[string]CatalogueTool) map[string]CatalogueTool {
	out := make(map[string]CatalogueTool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
