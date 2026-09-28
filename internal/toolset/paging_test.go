package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// longDeck is a lecture's deck of 38 slides, each with bullets, a table
// and speaker notes in two scripts: some 57 KB of text, as the deck that
// was found cut at its first 32 KB.
func longDeck() []byte {
	var slides []doctexttest.Slide
	for i := range 38 {
		slides = append(slides, doctexttest.Slide{
			Title: fmt.Sprintf("第%d講 Sorting, part %d", i+1, i+1),
			Body: []doctexttest.Bullet{
				{Text: fmt.Sprintf("Point %d: merge sort splits the list in two \"halves\" and merges them back", i+1)},
				{Text: "Each merge walks both halves once, comparing their heads", Level: 1},
				{Text: "Stable: equal keys keep their order — 穩定排序", Level: 1},
			},
			Table: [][]string{{"n", "comparisons"}, {"8", "24"}, {fmt.Sprint(i + 16), "64"}},
			Notes: strings.Repeat(fmt.Sprintf("Ask the class about slide %d. ", i+1), 40),
		})
	}
	return doctexttest.PPTX(slides...)
}

// deckServer serves one file, counting the fetches.
func deckServer(t *testing.T, data []byte) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// The versions of the document the tests read.
const (
	version1 = "0192f3c1-0001-7b4a-9c3d-2e1f0a9b8c7d"
	version2 = "0192f3c1-0002-7b4a-9c3d-2e1f0a9b8c7d"
)

// versionedCore is document_get's answer for one document, whose versions
// both hold the file served: the version named, and version2, the latest,
// when none is; it records the arguments of every call.
func versionedCore(url, contentType string, size int) *fakeCore {
	return &fakeCore{respond: func(_ context.Context, _ string, args json.RawMessage) (*core.Envelope, error) {
		var a struct {
			VersionID *string `json:"version_id"`
		}
		_ = json.Unmarshal(args, &a)
		res := documentResult(url+"?"+signature, "Week 3", contentType, size)
		res = strings.Replace(res, `"published":true`, `"published":true,"checksum":"sha256:abc"`, 1)
		version := version2
		if a.VersionID != nil {
			version = *a.VersionID
		}
		res = strings.Replace(res, `"id":"v1"`, `"id":"`+version+`"`, 1)
		return executed(res), nil
	}}
}

// TestReadDeckInParts reads a long deck as a model does: the first result
// says how many parts there are and how to ask for the next, and each part
// asked for with the call the one before names, until the last. The parts
// are the runtime's whole text of the deck, nothing lost and nothing twice,
// each cut where a slide begins; the file is fetched and read once; Core is
// asked for every part, for the version the first named, never with the
// runtime's own argument; and every result is within the limit. The first
// call names no version, and Core gives its latest: the parts are that
// version's.
func TestReadDeckInParts(t *testing.T) {
	deck := longDeck()
	want, err := doctext.Extract(context.Background(), deck, doctext.PPTX, doctext.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(want.Text); n < 50<<10 || n > 64<<10 {
		t.Fatalf("the deck's text is %d bytes, want some 57 KB", n)
	}
	srv, hits := deckServer(t, deck)
	f := versionedCore(srv.URL+"/deck", doctexttest.PPTXType, len(deck))
	r := Runner{Client: core.NewClient(f), Files: NewHTTPFetcher(srv.Client()), Texts: NewTextCache(0)}
	set := delegateSet(t)

	args := `{"document_id":"` + docID + `","version_id":null,"file_part":null}`
	var got strings.Builder
	var holds []string
	for n := 1; ; n++ {
		if n > 10 {
			t.Fatal("more parts than a 57 KB text makes")
		}
		parts, err := set.Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", args)})
		if err != nil {
			t.Fatal(err)
		}
		p := parts[0]
		if p.IsError || len(p.Content) > DefaultMaxResultBytes {
			t.Fatalf("part %d: is_error %v, %d bytes", n, p.IsError, len(p.Content))
		}
		rec := fileRecordOf(t, p)
		text, _ := contentOf(t, p)["file_text"].(string)
		if rec["given_as"] != givenText || rec["extracted_from"] != "pptx" || rec["part"] != float64(n) {
			t.Fatalf("part %d: file record %v", n, rec)
		}
		note, _ := rec["note"].(string)
		if n == 1 && (!strings.Contains(note, fmt.Sprintf("given in %v parts", rec["parts"])) || !strings.Contains(note, "38 slides")) {
			t.Errorf("the first part's note says %q", note)
		}
		if !strings.HasPrefix(text, "## Slide ") {
			t.Errorf("part %d begins %q, not where a slide does", n, text[:min(len(text), 20)])
		}
		holds = append(holds, rec["part_holds"].(string))
		got.WriteString(text)
		next, ok := rec["next_part"].(map[string]any)
		if !ok {
			if rec["part"] != rec["parts"] || !strings.Contains(note, "the last") {
				t.Errorf("part %d of %v gives no next part: %v", n, rec["parts"], rec)
			}
			break
		}
		if next["tool"] != "document_get" {
			t.Fatalf("next_part %v", next)
		}
		a := next["arguments"].(map[string]any)
		if a["document_id"] != docID || a["version_id"] != version2 || a["file_part"] != float64(n+1) {
			t.Fatalf("part %d's next_part %v", n, a)
		}
		raw, _ := json.Marshal(a)
		args = string(raw)
	}
	if got.String() != want.Text {
		t.Fatalf("the parts are not the deck's text:\n got %d bytes\nwant %d bytes", got.Len(), len(want.Text))
	}
	for i := range 38 {
		if c := strings.Count(got.String(), fmt.Sprintf("## Slide %d: ", i+1)); c != 1 {
			t.Errorf("slide %d is given %d times", i+1, c)
		}
	}
	if len(holds) < 3 || holds[0] != "slides 1–"+strings.TrimPrefix(holds[0], "slides 1–") || !strings.HasSuffix(holds[len(holds)-1], "–38") {
		t.Errorf("the parts hold %q", holds)
	}
	if h := hits.Load(); h != 1 {
		t.Errorf("the file was fetched %d times, want once", h)
	}
	calls := f.recorded()
	if len(calls) != len(holds) {
		t.Fatalf("Core was asked %d times for %d parts", len(calls), len(holds))
	}
	for i, c := range calls {
		if _, has := c.args[FilePartArg]; has {
			t.Errorf("call %d sent %s to Core: %v", i, FilePartArg, c.args)
		}
		if i > 0 && c.args["version_id"] != version2 {
			t.Errorf("call %d asked Core for version %v", i, c.args["version_id"])
		}
	}
}

// TestReadPartsOfOneLongSlide checks that a slide longer than a part is
// cut on its paragraphs and lines, and each part says which end of it it
// holds.
func TestReadPartsOfOneLongSlide(t *testing.T) {
	var lines []string
	for i := range 800 {
		lines = append(lines, fmt.Sprintf("line %d of the long slide's text box", i))
	}
	deck := doctexttest.PPTX(doctexttest.Slide{Title: "Short"}, doctexttest.Slide{Title: "Long", Text: lines}, doctexttest.Slide{Title: "After"})
	want, err := doctext.Extract(context.Background(), deck, doctext.PPTX, doctext.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	srv, hits := deckServer(t, deck)
	f := versionedCore(srv.URL+"/deck", doctexttest.PPTXType, len(deck))
	r := Runner{Client: core.NewClient(f), Files: NewHTTPFetcher(srv.Client()), Texts: NewTextCache(0)}
	var got strings.Builder
	var holds []string
	for n := 1; n < 10; n++ {
		parts, err := delegateSet(t).Run(context.Background(), r, courseID,
			[]llm.Part{call("d", "document_get", fmt.Sprintf(`{"document_id":%q,"version_id":%q,"file_part":%d}`, docID, version1, n))})
		if err != nil {
			t.Fatal(err)
		}
		rec := fileRecordOf(t, parts[0])
		text, _ := contentOf(t, parts[0])["file_text"].(string)
		got.WriteString(text)
		holds = append(holds, rec["part_holds"].(string))
		if rec["next_part"] == nil {
			break
		}
	}
	if got.String() != want.Text {
		t.Fatalf("the parts are not the deck's text")
	}
	if len(holds) < 2 || holds[0] != "slide 1 to the start of slide 2" || holds[len(holds)-1] != "the end of slide 2 to slide 3" {
		t.Errorf("the parts hold %q", holds)
	}
	if hits.Load() != 1 {
		t.Errorf("the file was fetched %d times", hits.Load())
	}
}

// TestFilePartAsked checks what a part the model asks for gives beside the
// text read: a part past the last, a part of a text given whole, a part of
// a file given as itself, and a part that is no number, which reaches
// nobody.
func TestFilePartAsked(t *testing.T) {
	fs := newFileServer(t)
	deck := longDeck()
	srv, _ := deckServer(t, deck)
	tests := []struct {
		name, url, ct string
		size          int
		fileInput     bool
		args          string
		givenAs, note string
	}{
		{"past the last part", srv.URL + "/deck", doctexttest.PPTXType, len(deck), false, `"file_part":40`,
			givenNot, "parts: there is no part 40; ask for file_part from 1 to "},
		{"a text given whole", fs.URL + "/notes.md", "text/markdown", len(markdown), false, `"file_part":2`,
			givenNot, "the file's text is one part: there is no part 2"},
		{"the first part of a text given whole is all of it", fs.URL + "/notes.md", "text/markdown", len(markdown), false, `"file_part":1`,
			givenText, ""},
		{"a PDF given as a file", fs.URL + "/syllabus.pdf", "application/pdf", len(pdf), true, `"file_part":2`,
			givenFile, "file_part does not apply: the file itself is given, whole"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
				return executed(documentResult(tc.url+"?"+signature, "Doc", tc.ct, tc.size)), nil
			}}
			r := Runner{Client: core.NewClient(f), Files: NewHTTPFetcher(srv.Client()), FileInput: tc.fileInput}
			parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", `{"document_id":"`+docID+`",`+tc.args+`}`)})
			if err != nil {
				t.Fatal(err)
			}
			rec := fileRecordOf(t, parts[0])
			note, _ := rec["note"].(string)
			if rec["given_as"] != tc.givenAs || !strings.Contains(note, tc.note) || parts[0].IsError {
				t.Errorf("file record %v", rec)
			}
		})
	}
	for _, bad := range []string{`"two"`, `0`, `-1`, `1.5`, `[1]`, `1e9`} {
		f := &fakeCore{}
		parts, err := delegateSet(t).Run(context.Background(), runner(f), courseID,
			[]llm.Part{call("d", "document_get", `{"document_id":"`+docID+`","file_part":`+bad+`}`)})
		if err != nil {
			t.Fatal(err)
		}
		if code, msg := errorOf(t, parts[0]); code != core.CodeInvalidArgument || !strings.Contains(msg, "file_part") || len(f.recorded()) != 0 {
			t.Errorf("file_part %s: %s %q, %d calls", bad, code, msg, len(f.recorded()))
		}
	}
}

// TestSplitText holds the cutting of any text to its promises: parts that
// together are the text, none empty, each within the budget once written
// as JSON, and a cut where a section begins wherever that fills half a
// part.
func TestSplitText(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	pieces := []string{"a", "bc ", "\n", "\n\n", "\"", "\\", "\t", "\x01", "é", "排序", "😀", " ", "line of words "}
	for trial := range 300 {
		var b strings.Builder
		var sections []doctext.Section
		for b.Len() < rng.IntN(20000) {
			if rng.IntN(40) == 0 {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				sections = append(sections, doctext.Section{Kind: doctext.SectionPage, N: len(sections) + 1, Offset: b.Len()})
				fmt.Fprintf(&b, "## Page %d\n", len(sections))
			}
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		text := b.String()
		budget := 16 + rng.IntN(4000)
		parts := splitText(text, sections, budget)
		var got strings.Builder
		for i, p := range parts {
			if p.end <= p.start || p.start != got.Len() {
				t.Fatalf("trial %d: part %d is %v after %d bytes", trial, i, p, got.Len())
			}
			s := text[p.start:p.end]
			if n := len(encodeJSON(s)) - 2; n > budget {
				t.Fatalf("trial %d: part %d is %d bytes as JSON, over %d", trial, i, n, budget)
			}
			if n := escapedLenOf(s); n != len(encodeJSON(s))-2 {
				t.Fatalf("trial %d: escapedLenOf says %d, JSON takes %d", trial, n, len(encodeJSON(s))-2)
			}
			got.WriteString(s)
		}
		if got.String() != text {
			t.Fatalf("trial %d: the parts are not the text", trial)
		}
	}
	// Slides of 100 bytes in parts of 1000: every part ends where a slide
	// begins.
	var b strings.Builder
	var sections []doctext.Section
	for i := range 50 {
		sections = append(sections, doctext.Section{Kind: doctext.SectionSlide, N: i + 1, Offset: b.Len()})
		b.WriteString(fmt.Sprintf("## Slide %d\n", i+1) + strings.Repeat("x", 86) + "\n\n")
	}
	for _, p := range splitText(b.String(), sections, 1000) {
		if !strings.HasPrefix(b.String()[p.start:], "## Slide ") {
			t.Errorf("a part begins at %d, where no slide does", p.start)
		}
	}
}

func TestPartHolds(t *testing.T) {
	secs := []doctext.Section{{Kind: "page", N: 1, Offset: 0}, {Kind: "page", N: 2, Offset: 100}, {Kind: "page", N: 3, Offset: 200}}
	tests := []struct {
		p    textPart
		want string
	}{
		{textPart{0, 300}, "pages 1–3"},
		{textPart{0, 100}, "page 1"},
		{textPart{100, 150}, "the start of page 2"},
		{textPart{150, 200}, "the end of page 2"},
		{textPart{120, 150}, "part of page 2"},
		{textPart{50, 250}, "the end of page 1 to the start of page 3"},
		{textPart{200, 300}, "page 3"},
	}
	for _, tc := range tests {
		if got := partHolds(tc.p, secs, 300); got != tc.want {
			t.Errorf("%v holds %q, want %q", tc.p, got, tc.want)
		}
	}
	if got := partHolds(textPart{0, 10}, nil, 300); got != "" {
		t.Errorf("a text of no sections: %q", got)
	}
}

// TestTextCache checks the cache's bound: the reading used least recently
// goes first, and one larger than the whole bound is not kept.
func TestTextCache(t *testing.T) {
	c := NewTextCache(3000)
	rd := func(n int) *fileReading {
		return &fileReading{mt: "text/plain", res: &doctext.Result{Text: strings.Repeat("x", n)}}
	}
	c.put("a", rd(700))
	c.put("b", rd(700))
	c.put("c", rd(700))
	if c.get("a") == nil {
		t.Fatal("a is not kept")
	}
	c.put("d", rd(700)) // past the bound: b, used least recently, goes
	if c.get("b") != nil || c.get("a") == nil || c.get("c") == nil || c.get("d") == nil {
		t.Errorf("kept %+v", c.Stats())
	}
	c.put("huge", rd(5000))
	if c.get("huge") != nil {
		t.Error("a reading past the whole bound is kept")
	}
	if s := c.Stats(); s.Bytes > 3000 || s.Readings != 3 {
		t.Errorf("stats %+v", s)
	}
	var nilCache *TextCache
	nilCache.put("a", rd(1))
	if nilCache.get("a") != nil {
		t.Error("a nil cache keeps")
	}
}

// TestTextCacheShared checks that another agent's runner on the same worker
// reads a version already read from the cache, and that a version Core
// names anew is read anew.
func TestTextCacheShared(t *testing.T) {
	deck := longDeck()
	srv, hits := deckServer(t, deck)
	cache := NewTextCache(0)
	for i, version := range []string{version1, version1, version2} {
		f := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
			res := documentResult(srv.URL+"/deck?"+signature, "Week 3", doctexttest.PPTXType, len(deck))
			return executed(strings.Replace(res, `"id":"v1"`, `"id":"`+version+`"`, 1)), nil
		}}
		r := Runner{Client: core.NewClient(f), Files: NewHTTPFetcher(srv.Client()), Texts: cache}
		if _, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", `{"document_id":"`+docID+`"}`)}); err != nil {
			t.Fatal(err)
		}
		if want := []int32{1, 1, 2}[i]; hits.Load() != want {
			t.Errorf("read %d (%s): %d fetches, want %d", i, version, hits.Load(), want)
		}
	}
}

// TestFilePartInCoreSchema checks that a Core whose document_get takes an
// argument of the runtime's own name is refused: the two would be one.
func TestFilePartInCoreSchema(t *testing.T) {
	cat := snapshot(t)
	tool := cat.Tools[FilePartTool]
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	schema["properties"].(map[string]any)[FilePartArg] = map[string]any{"type": "integer"}
	tool.InputSchema, _ = json.Marshal(schema)
	cat.Tools[FilePartTool] = tool
	if err := cat.Check(); err == nil || !strings.Contains(err.Error(), "takes file_part itself") {
		t.Errorf("Check: %v", err)
	}
	if _, err := cat.Build(delegatePerms, config.Tools{}, ReadOnly, toolschema.OpenAI, nil); err == nil {
		t.Error("Build offered it")
	}
}
