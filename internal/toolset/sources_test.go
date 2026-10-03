package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

// turn runs one turn's calls on r with set, as the loop does.
func turn(t *testing.T, set *Set, r Runner, calls ...llm.Part) []llm.Part {
	t.Helper()
	parts, err := set.Run(context.Background(), r, courseID, calls)
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

// sourcesText is a list of sources as the tests name them.
func sourcesText(list []core.Source) string {
	if list == nil {
		return "nil"
	}
	var out []string
	for _, s := range list {
		b := s.DocumentID + "@" + s.VersionID
		if s.FileID != "" {
			b += "/" + s.FileID
		}
		for _, n := range []struct {
			what string
			n    int
		}{{"page", s.Page}, {"slide", s.Slide}, {"part", s.Part}} {
			if n.n > 0 {
				b += fmt.Sprintf(" %s %d", n.what, n.n)
			}
		}
		out = append(out, b)
	}
	return "[" + strings.Join(out, ", ") + "]"
}

func wantSources(t *testing.T, s *Sources, want ...core.Source) {
	t.Helper()
	if want == nil {
		want = []core.Source{}
	}
	if got := s.List(); sourcesText(got) != sourcesText(want) {
		t.Errorf("the sources are %s;\nwant %s", sourcesText(got), sourcesText(want))
	}
}

func get(id, args string) llm.Part { return call(id, FilePartTool, args) }

// A material counts as relied on once the model's own document_get gave
// the model some of what it holds, in the order the calls were made,
// across turns: the syllabus by its own text, a PDF's file by its text,
// once however often it is read; a slide asked for alone by its number,
// and slides asked for together by none. A document only listed, and one
// Core does not give, are no source.
func TestSourcesAreWhatTheModelWasGiven(t *testing.T) {
	c := newSearchCore(t, course()...)
	r := searchRunner(c, memstore.New(), &SearchScope{}, false)
	r.Sources = &Sources{}
	set := searchSet(t)

	wantSources(t, r.Sources)
	turn(t, set, r, call("l", "document_list", `{}`),
		get("a", `{"document_id":"`+docSyllabus+`"}`),
		get("b", `{"document_id":"`+docReading+`"}`),
		get("c", `{"document_id":"0192f3c1-dead-7b4a-9c3d-2e1f0a9b8c7d"}`))
	syll := core.Source{DocumentID: docSyllabus, VersionID: verSyll}
	reading := core.Source{DocumentID: docReading, VersionID: verReading, FileID: sfileReadingPDF}
	wantSources(t, r.Sources, syll, reading)

	turn(t, set, r, get("d", `{"document_id":"`+docReading+`"}`),
		get("e", `{"document_id":"`+docSlides+`","file_id":"`+sfileSlides1+`","file_pages":"2"}`),
		get("f", `{"document_id":"`+docSlides+`","file_id":"`+sfileSlides1+`","file_pages":"1-2"}`))
	slide2 := core.Source{DocumentID: docSlides, VersionID: verSlides1, FileID: sfileSlides1, Slide: 2}
	slides := core.Source{DocumentID: docSlides, VersionID: verSlides1, FileID: sfileSlides1}
	wantSources(t, r.Sources, syll, reading, slide2, slides)
}

// A search's hits are no source, their excerpts cut, nor are the reads the
// search makes itself of its scope: an answer that only searched says
// nothing of its sources (nil), rather than that it relied on none, nor
// names what the search read. A hit read with the call it names is one,
// with the page or the slide of the hit where the call names none: a text
// version read by its part, where the hit is on page 2; a slide read by
// its number. Hits on two pages that one call reads leave the page out.
func TestSourcesOfASearch(t *testing.T) {
	c := newSearchCore(t, course()...)
	r := searchRunner(c, memstore.New(), &SearchScope{}, false)
	r.Sources = &Sources{}

	res, _ := searchFor(t, r, `{"query":"插入"}`)
	if got := hitsOf(res); !slices.Equal(got, []string{"Handout page 2"}) {
		t.Fatalf("the hits: %v", got)
	}
	h := res.Result.Hits[0]
	if h.Read.Arguments[FilePagesArg] != nil {
		t.Fatalf("the handout's hit is read by %v", h.Read.Arguments)
	}
	if got := r.Sources.List(); got != nil {
		t.Errorf("an answer that only searched relied on %s; want nil, which says nothing", sourcesText(got))
	}
	readHit(t, r, h.Read)
	handout := core.Source{DocumentID: docHandout, VersionID: verHandout, FileID: sfileHandout, Page: 2}
	wantSources(t, r.Sources, handout)

	res, _ = searchFor(t, r, `{"query":"合併排序的複雜度"}`)
	readHit(t, r, res.Result.Hits[0].Read)
	wantSources(t, r.Sources, handout, core.Source{DocumentID: docSlides, VersionID: verSlides1, FileID: sfileSlides1, Slide: 2})

	// The handout's two pages, one part of its text: one call reads both.
	c = newSearchCore(t, course()[2])
	r = searchRunner(c, memstore.New(), &SearchScope{}, false)
	r.Sources = &Sources{}
	res, _ = searchFor(t, r, `{"query":"排序"}`)
	if got := hitsOf(res); !slices.Equal(got, []string{"Handout page 1", "Handout page 2"}) && !slices.Equal(got, []string{"Handout page 2", "Handout page 1"}) {
		t.Fatalf("the hits: %v", got)
	}
	if a, b := res.Result.Hits[0].Read, res.Result.Hits[1].Read; fmt.Sprint(a.Arguments) != fmt.Sprint(b.Arguments) {
		t.Fatalf("the hits are read by %v and %v", a.Arguments, b.Arguments)
	}
	readHit(t, r, res.Result.Hits[0].Read)
	wantSources(t, r.Sources, core.Source{DocumentID: docHandout, VersionID: verHandout, FileID: sfileHandout})
}

// kinds are the documents kindCore gives, by their ids: one of each kind;
// a material whose version is purged, and one purged whole; one whose
// version is a scan the model is not given; and one whose version Core
// gives no id.
var kinds = map[string]string{
	"0192f3c1-e001-7b4a-9c3d-2e1f0a9b8c7d": "submission", "0192f3c1-e002-7b4a-9c3d-2e1f0a9b8c7d": "feedback",
	"0192f3c1-e003-7b4a-9c3d-2e1f0a9b8c7d": "purged", "0192f3c1-e004-7b4a-9c3d-2e1f0a9b8c7d": "empty",
	"0192f3c1-e005-7b4a-9c3d-2e1f0a9b8c7d": "rubric", "0192f3c1-e006-7b4a-9c3d-2e1f0a9b8c7d": "instructions",
	"0192f3c1-e007-7b4a-9c3d-2e1f0a9b8c7d": "material", "0192f3c1-e008-7b4a-9c3d-2e1f0a9b8c7d": "purged document",
	"0192f3c1-e009-7b4a-9c3d-2e1f0a9b8c7d": "unnamed",
}

// idOf is the id of kinds' document of kind.
func idOf(kind string) string {
	for id, k := range kinds {
		if k == kind {
			return id
		}
	}
	panic(kind)
}

// kindCore answers document_get with kinds' documents, each a version of
// its own text, its id the document's with a v: purged's version is
// purged, and purged document's document, each still giving its text, as
// no Core does, so that the purge alone keeps it from being a source;
// empty's has no text and a scan for its file; unnamed's version has no
// id.
func kindCore(fs *fileServer) func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
	return func(_ context.Context, _ string, args json.RawMessage) (*core.Envelope, error) {
		var a struct {
			DocumentID string `json:"document_id"`
		}
		_ = json.Unmarshal(args, &a)
		kind := kinds[a.DocumentID]
		version := `{"id":"v` + a.DocumentID + `","seq":1,"body_md":"What it says.","files":[],"published":true}`
		document := ""
		switch kind {
		case "purged":
			kind, version = "material", `{"id":"v`+a.DocumentID+`","seq":1,"body_md":"What it said.","files":[],"published":true,`+
				`"purged":{"at":"2026-09-30T00:00:00Z","by_actor_id":"a","reason":"r"}}`
		case "purged document":
			kind, document = "material", `"purged_at":"2026-09-30T00:00:00Z",`
		case "empty":
			kind, version = "material", `{"id":"v`+a.DocumentID+`","seq":1,"files":[{"id":"`+fileSlides+`","position":1,"filename":"scan.pdf",`+
				`"content_type":"application/pdf","byte_size":10,"download_url":"`+fs.url("/scanned.pdf")+`"}],"published":true}`
		case "unnamed":
			kind, version = "material", `{"seq":1,"body_md":"What it says.","files":[],"published":true}`
		}
		return executed(`{"id":"` + a.DocumentID + `","kind":"` + kind + `","title":"T",` + document + `"version":` + version + `}`), nil
	}
}

// A source is a course's material, instructions or rubric: never a
// student's work or a grader's feedback, which Core would refuse as none
// the seat may read; nor a version purged, or of a document purged,
// whatever the result still holds; nor one the model was given nothing
// of (a scan with no text, no OCR here, and no text of the version's
// own). An answer given none of them relied on none. A version Core
// gives no id of, its text given, is given and named by no source: an
// answer given it and nothing else says nothing of its sources.
func TestSourcesAreCourseMaterialsGiven(t *testing.T) {
	fs := newFileServer(t)
	r := Runner{Client: core.NewClient(&fakeCore{respond: kindCore(fs)}), Files: NewHTTPFetcher(fs.Client()), Sources: &Sources{}}
	set := delegateSet(t)
	doc := func(id, kind string) llm.Part { return get(id, `{"document_id":"`+idOf(kind)+`"}`) }
	turn(t, set, r, doc("a", "submission"), doc("b", "feedback"), doc("c", "purged"), doc("d", "purged document"), doc("e", "empty"))
	wantSources(t, r.Sources)
	turn(t, set, r, doc("f", "rubric"), doc("g", "instructions"), doc("h", "material"))
	source := func(kind string) core.Source { return core.Source{DocumentID: idOf(kind), VersionID: "v" + idOf(kind)} }
	wantSources(t, r.Sources, source("rubric"), source("instructions"), source("material"))

	r.Sources = &Sources{}
	turn(t, set, r, doc("i", "unnamed"))
	if got := r.Sources.List(); got != nil {
		t.Errorf("an answer given a version Core named no id of relied on %s; want nil, which says nothing", sourcesText(got))
	}
	turn(t, set, r, doc("j", "material"))
	wantSources(t, r.Sources, source("material"))
}

// A page asked for alone (file_pages) is named where it was given alone:
// cut from the PDF as a PDF of its own, or as the text of it alone to a
// model that takes no files. Pages asked for together name none; nor does
// the whole PDF, given where the runtime cuts no PDF, though the model is
// told to see the page in it: it was given every page.
func TestSourcesOfPagesAskedFor(t *testing.T) {
	fs := newFileServer(t)
	result := versionResult(fs, "", vfile{id: fileSlides, name: "w1.pdf", ct: "application/pdf", path: "/reading.pdf", size: len(reading)})
	file := core.Source{DocumentID: docID, VersionID: "v1", FileID: fileSlides}
	page2 := file
	page2.Page = 2
	for _, c := range []struct {
		name         string
		files, cuts  bool
		pages        string
		givenAs, say string
		want         core.Source
	}{
		{"a page cut as a PDF of its own", true, true, "2", givenFile, "the file's page 2 are given as a PDF of their own", page2},
		{"pages cut together", true, true, "1-2", givenFile, "the file's pages 1–2 are given as a PDF of their own", file},
		{"the whole PDF, where none are cut", true, false, "2", givenFile, "the whole PDF is given: see page 2 in it", file},
		{"the text of a page alone", false, true, "2", givenText, "file_text is page 2 alone", page2},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := Runner{Client: core.NewClient(&fakeCore{respond: (&versionCore{result: func() string { return result }}).respond}),
				Files: NewHTTPFetcher(fs.Client()), Texts: NewTextCache(0), FileInput: c.files, Office: &stubOffice{noCuts: !c.cuts},
				Sources: &Sources{}}
			parts := turn(t, delegateSet(t), r, get("p", `{"document_id":"`+docID+`","file_pages":"`+c.pages+`"}`))
			if rec := fileRecordOf(t, parts[0]); rec["given_as"] != c.givenAs || !strings.Contains(noteOf(rec), c.say) {
				t.Fatalf("file_pages %s: %s", c.pages, parts[0].Content)
			}
			wantSources(t, r.Sources, c.want)
		})
	}
}

// A version of several files read whole names no file, having read
// several; read by its file_id, the file. A part of a long file the
// runtime cut (file_part) is no part of Core's: the file alone.
func TestSourcesOfAVersionOfSeveralFiles(t *testing.T) {
	fs := newFileServer(t)
	result := versionResult(fs, "", vfile{id: fileSlides, name: "notes.md", ct: "text/markdown", path: "/notes.md", size: len(markdown)},
		vfile{id: fileHandout, name: "long.txt", ct: "text/plain", path: "/long.txt", size: 130000})
	r := Runner{Client: core.NewClient(&fakeCore{respond: (&versionCore{result: func() string { return result }}).respond}),
		Files: NewHTTPFetcher(fs.Client()), Texts: NewTextCache(0), Sources: &Sources{}}
	set := delegateSet(t)
	turn(t, set, r, get("a", `{"document_id":"`+docID+`"}`))
	turn(t, set, r, get("b", `{"document_id":"`+docID+`","file_id":"`+fileSlides+`"}`))
	parts := turn(t, set, r, get("c", `{"document_id":"`+docID+`","file_id":"`+fileHandout+`","file_part":2}`))
	if rec := fileRecordOf(t, parts[0]); rec["part"] != float64(2) {
		t.Fatalf("the long file's part: %s", parts[0].Content)
	}
	wantSources(t, r.Sources, core.Source{DocumentID: docID, VersionID: "v1"}, core.Source{DocumentID: docID, VersionID: "v1", FileID: fileSlides},
		core.Source{DocumentID: docID, VersionID: "v1", FileID: fileHandout})
}

// pagedText is a text version of four pages of 2,000 bytes each, as the
// transcriber heads them.
func pagedText() string {
	var b strings.Builder
	for n := 1; n <= 4; n++ {
		head := fmt.Sprintf("## 第 %d 頁\n", n)
		b.WriteString(head + strings.Repeat("x", 2000-len(head)-1) + "\n")
	}
	return b.String()
}

// The part a source names is Core's: where the runtime read the file's
// text version in Core's parts (document_text) and gave the model text
// of one of them alone. Core's parts here are of 3,000 bytes, and the
// runtime gives the text a page a part: its part 1 is in Core's part 1,
// its part 2 runs over Core's parts 1 and 2 and names none, its parts 3
// and 4 are in Core's 2 and 3. A page asked for alone is the page, and
// the part it is in. A text version Core gave whole beside the version
// names no part, however the runtime cut it.
func TestSourcesNameCoresPartAlone(t *testing.T) {
	fs := newFileServer(t)
	body := pagedText()
	if len(body) != 8000 {
		t.Fatalf("the text is %d bytes", len(body))
	}
	tv := func(whole bool) string {
		v := fmt.Sprintf(`{"status":"done","source":"ai","model":"Flash-Lite","pages":4,"revision":3,"updated_at":"2026-09-01T00:00:00Z","bytes":%d`, len(body))
		if whole {
			b, _ := json.Marshal(body)
			v += `,"body":` + string(b)
		}
		return v + "}"
	}
	for _, whole := range []bool{false, true} {
		t.Run(fmt.Sprint("whole=", whole), func(t *testing.T) {
			tc := &textCore{body: body, per: 3000}
			result := versionResult(fs, "", vfile{id: fileSlides, name: "w1.pdf", ct: "application/pdf", path: "/reading.pdf", size: len(reading), text: tv(whole)})
			respond := func(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
				if tool == FilePartTool {
					return executed(result), nil
				}
				return tc.respond(ctx, tool, args)
			}
			r := Runner{Client: core.NewClient(&fakeCore{respond: respond}), Files: NewHTTPFetcher(fs.Client()), Texts: NewTextCache(0),
				MaxResultBytes: 4096, Sources: &Sources{}}
			set := delegateSet(t)
			for k := 1; k <= 4; k++ {
				parts := turn(t, set, r, get("p", fmt.Sprintf(`{"document_id":%q,"file_part":%d}`, docID, k)))
				if rec := fileRecordOf(t, parts[0]); rec["part"] != float64(k) || rec["parts"] != float64(4) || rec["part_holds"] != fmt.Sprintf("page %d", k) {
					t.Fatalf("part %d: %s", k, parts[0].Content)
				}
			}
			turn(t, set, r, get("q", `{"document_id":"`+docID+`","file_pages":"3"}`))
			file := core.Source{DocumentID: docID, VersionID: "v1", FileID: fileSlides}
			part := func(s core.Source, n int) core.Source { s.Part = n; return s }
			page3 := file
			page3.Page = 3
			if whole {
				wantSources(t, r.Sources, file, page3)
				return
			}
			wantSources(t, r.Sources, part(file, 1), file, part(file, 2), part(file, 3), part(page3, 2))
		})
	}
}

// A page or a slide is of a file: a hit's where makes none of a source
// that names no file. What one answer names is at most 20, the first
// read, each once; and List gives a copy.
func TestSourcesAdd(t *testing.T) {
	s := &Sources{}
	k := newReadKey("D", "V", "", 0, 0, 0)
	s.add(k, &materialGiven{hits: []hitAt{{key: k, kind: doctext.SectionPage, n: 3}}})
	s.add(k, &materialGiven{source: &core.Source{DocumentID: "D", VersionID: "V"}})
	for i := range 25 {
		s.add(readKey{}, &materialGiven{source: &core.Source{DocumentID: fmt.Sprint("doc", i%22), VersionID: "v"}})
	}
	got := s.List()
	if len(got) != core.MaxSources || got[0] != (core.Source{DocumentID: "D", VersionID: "V"}) || got[19].DocumentID != "doc18" {
		t.Errorf("the sources: %s", sourcesText(got))
	}
	got[0].DocumentID = "changed"
	if s.List()[0].DocumentID != "D" {
		t.Error("List gives away its own slice")
	}
	var none *Sources
	if none.List() != nil {
		t.Error("a nil Sources says what an answer relied on")
	}
	none.add(k, &materialGiven{source: &core.Source{DocumentID: "D", VersionID: "V"}})
}
