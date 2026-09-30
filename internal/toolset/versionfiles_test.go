package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// The files of a version of several, as the tests' Cores list them.
const (
	fileSlides  = "0192f3c1-f001-7b4a-9c3d-2e1f0a9b8c7d"
	fileHandout = "0192f3c1-f002-7b4a-9c3d-2e1f0a9b8c7d"
	fileNotes   = "0192f3c1-f003-7b4a-9c3d-2e1f0a9b8c7d"
	fileFourth  = "0192f3c1-f004-7b4a-9c3d-2e1f0a9b8c7d"
)

// vfile is a file of a version as document_get lists it (version.files,
// AIShie-Core #49): its id, name, type, the path the file server serves
// it at, its size, and its text version, raw JSON, "" for none.
type vfile struct {
	id, name, ct, path string
	size               int
	text               string
}

// versionResult is document_get's result for version v1 of the document,
// its text body (none for "") and files, in order; the version's own
// deprecated fields are the first file's, as Core gives them.
func versionResult(fs *fileServer, body string, files ...vfile) string {
	var b strings.Builder
	for i, f := range files {
		if i > 0 {
			b.WriteString(",")
		}
		text := ""
		if f.text != "" {
			text = `,"text":` + f.text
		}
		fmt.Fprintf(&b, `{"id":%q,"position":%d,"filename":%q,"content_type":%q,"byte_size":%d,"checksum":"sha256:%x","download_url":%q%s}`,
			f.id, i+1, f.name, f.ct, f.size, i+1, fs.url(f.path), text)
	}
	first := ""
	if len(files) > 0 {
		f := files[0]
		first = fmt.Sprintf(`"download_url":%q,"content_type":%q,"byte_size":%d,`, fs.url(f.path), f.ct, f.size)
		if f.text != "" {
			first += `"text":` + f.text + `,`
		}
	}
	bodyMD := ""
	if body != "" {
		bodyMD = fmt.Sprintf(`"body_md":%q,`, body)
	}
	return fmt.Sprintf(`{"id":%q,"kind":"material","title":"Week 3","sort_order":0,"status":"active","created_at":"2026-09-01T00:00:00Z",`+
		`"version":{"id":"v1","seq":1,%s%s"files":[%s],"author_member_id":"m","created_at":"2026-09-01T00:00:00Z","published":true}}`,
		docID, bodyMD, first, b.String())
}

// versionCore answers document_get with result, document_text of a file
// with the parts of its body in bodies (of at most per bytes), and
// document_file with a fresh URL at fresh; it records every call.
type versionCore struct {
	result func() string
	bodies map[string]string
	per    int
	fresh  func(fileID string) string

	mu    sync.Mutex
	texts []string
	files []string
}

func (c *versionCore) respond(_ context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
	var a struct {
		FileID string `json:"file_id"`
		Part   int    `json:"part"`
	}
	_ = json.Unmarshal(args, &a)
	switch tool {
	case "document_get":
		return executed(c.result()), nil
	case "document_file":
		c.mu.Lock()
		c.files = append(c.files, a.FileID)
		c.mu.Unlock()
		return executed(fmt.Sprintf(`{"id":%q,"position":1,"filename":"x","content_type":"application/pdf","byte_size":1,"document_id":%q,`+
			`"version_id":"v1","seq":1,"published":true,"download_url":%q,"expires_at":"2026-09-01T00:15:00Z"}`, a.FileID, docID, c.fresh(a.FileID))), nil
	case "document_text":
		c.mu.Lock()
		c.texts = append(c.texts, fmt.Sprintf("%s:%d", a.FileID, a.Part))
		c.mu.Unlock()
		var parts []string
		for s := c.bodies[a.FileID]; s != ""; {
			k := min(c.per, len(s))
			parts, s = append(parts, s[:k]), s[k:]
		}
		body, _ := json.Marshal(parts[a.Part-1])
		return executed(fmt.Sprintf(`{"document_id":%q,"version_id":"v1","seq":1,"published":true,"file_id":%q,"position":2,"filename":"x",`+
			`"part":%d,"parts":%d,"text":{"status":"done","source":"staff","revision":2,"updated_at":"2026-09-01T00:00:00Z","bytes":%d,"body":%s}}`,
			docID, a.FileID, a.Part, len(parts), len(c.bodies[a.FileID]), body)), nil
	}
	return executed(`{}`), nil
}

func (c *versionCore) calls() (texts, files []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.texts...), append([]string(nil), c.files...)
}

// getVersion calls document_get with args, and returns the result's
// content, the files given beside it, and the result as the model reads
// it.
func getVersion(t *testing.T, r Runner, args string) (map[string]any, []*llm.File, string) {
	t.Helper()
	parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", args)})
	if err != nil {
		t.Fatal(err)
	}
	if parts[0].IsError {
		t.Fatalf("result %+v", parts[0])
	}
	if strings.Contains(parts[0].Content, signature) || strings.Contains(parts[0].Content, "download_url") {
		t.Fatalf("a URL reached the model: %s", parts[0].Content)
	}
	var files []*llm.File
	for _, p := range parts[1:] {
		files = append(files, p.File)
	}
	return contentOf(t, parts[0]), files, parts[0].Content
}

// entriesOf are the files a result gives, by position.
func entriesOf(t *testing.T, c map[string]any) []map[string]any {
	t.Helper()
	raw, _ := c["files"].([]any)
	var out []map[string]any
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

// lecture is a lecture's version of three files, as a course puts them
// up: the reading as a PDF, the handout as a .docx, and notes as
// Markdown.
func lecture(fs *fileServer) func() string {
	return func() string {
		return versionResult(fs, "Read the PDF first.",
			vfile{id: fileSlides, name: "reading.pdf", ct: "application/pdf", path: "/reading.pdf", size: len(reading)},
			vfile{id: fileHandout, name: "handout.docx", ct: doctexttest.DOCXType, path: "/handout.docx", size: len(handout)},
			vfile{id: fileNotes, name: "notes.md", ct: "text/markdown", path: "/notes.md", size: len(markdown)})
	}
}

// A version of three files is given file by file, in order, each under its
// name and id: to a model that takes files, the PDF as a file part that
// follows the result under its name, the Word file as the runtime's text
// of it, the notes as their text; to one that takes none, the PDF as its
// text. The version's own text is in the result, no URL is.
func TestVersionOfSeveralFiles(t *testing.T) {
	fs := newFileServer(t)
	vc := &versionCore{result: lecture(fs)}
	for _, fileInput := range []bool{true, false} {
		t.Run(fmt.Sprintf("file_input=%v", fileInput), func(t *testing.T) {
			r := Runner{Client: core.NewClient(&fakeCore{respond: vc.respond}), Files: NewHTTPFetcher(fs.Client()), FileInput: fileInput,
				Texts: NewTextCache(0)}
			c, files, raw := getVersion(t, r, `{"document_id":"`+docID+`"}`)
			if c["file"] != nil || c["file_text"] != nil || !strings.Contains(c["files_note"].(string), "the version holds 3 files") ||
				!strings.Contains(raw, `"body_md":"Read the PDF first."`) {
				t.Fatalf("the result: %s", raw)
			}
			es := entriesOf(t, c)
			if len(es) != 3 {
				t.Fatalf("%d files: %s", len(es), raw)
			}
			for i, want := range []struct{ id, name string }{{fileSlides, "reading.pdf"}, {fileHandout, "handout.docx"}, {fileNotes, "notes.md"}} {
				if es[i]["file_id"] != want.id || es[i]["name"] != want.name || es[i]["position"] != float64(i+1) {
					t.Errorf("file %d: %v", i+1, es[i])
				}
			}
			if fileInput {
				if es[0]["given_as"] != givenFile || es[0]["file_text"] != nil || len(files) != 1 || files[0].Name != "reading.pdf" ||
					files[0].MIME != "application/pdf" {
					t.Errorf("the PDF to a model that takes files: %v, %d files", es[0], len(files))
				}
			} else if text, _ := es[0]["file_text"].(string); es[0]["given_as"] != givenText || !strings.Contains(text, "Merge sort is stable.") ||
				len(files) != 0 {
				t.Errorf("the PDF to a model that takes none: %v", es[0])
			}
			if text, _ := es[1]["file_text"].(string); es[1]["given_as"] != givenText || es[1]["extracted_from"] != "docx" || !strings.Contains(text, "Bring a laptop.") {
				t.Errorf("the handout: %v", es[1])
			}
			if es[2]["given_as"] != givenText || es[2]["file_text"] != markdown {
				t.Errorf("the notes: %v", es[2])
			}
			if len(raw) > versionResults*r.withDefaults().MaxResultBytes {
				t.Errorf("the result is %d bytes", len(raw))
			}
		})
	}
}

// Each file's text version is given first where it is done, under the
// file's name and saying whose it is: from its body in document_get, which
// the result then leaves out, or read part by part with document_text
// naming the file; the file itself is not fetched. A file whose text is
// not done is fetched and given as before.
func TestVersionTextsFirst(t *testing.T) {
	fs := newFileServer(t)
	short := transcript(1, "short")
	long := transcript(3, strings.Repeat("y", 900))
	shortJSON, _ := json.Marshal(short)
	vc := &versionCore{per: 1000, bodies: map[string]string{fileHandout: long}, result: func() string {
		return versionResult(fs, "",
			vfile{id: fileSlides, name: "reading.pdf", ct: "application/pdf", path: "/reading.pdf", size: len(reading),
				text: fmt.Sprintf(`{"status":"done","source":"ai","model":"Flash-Lite","pages":1,"revision":2,"updated_at":"2026-09-01T00:00:00Z","bytes":%d,"body":%s}`,
					len(short), shortJSON)},
			vfile{id: fileHandout, name: "handout.docx", ct: doctexttest.DOCXType, path: "/handout.docx", size: len(handout),
				text: fmt.Sprintf(`{"status":"done","source":"staff","revision":2,"updated_at":"2026-09-01T00:00:00Z","bytes":%d}`, len(long))},
			vfile{id: fileNotes, name: "notes.md", ct: "text/markdown", path: "/notes.md", size: len(markdown),
				text: `{"status":"pending","revision":1,"updated_at":"2026-09-01T00:00:00Z","bytes":0}`})
	}}
	cache := NewTextCache(0)
	r := Runner{Client: core.NewClient(&fakeCore{respond: vc.respond}), Files: NewHTTPFetcher(fs.Client()), FileInput: true, Texts: cache}
	c, files, raw := getVersion(t, r, `{"document_id":"`+docID+`"}`)
	es := entriesOf(t, c)
	if len(es) != 3 || len(files) != 0 {
		t.Fatalf("%d files, %d parts: %s", len(es), len(files), raw)
	}
	if es[0]["given_as"] != givenText || es[0]["file_text"] != short || es[0]["text_source"] != "AI transcription (Flash-Lite)" {
		t.Errorf("the PDF's transcription: %v", es[0])
	}
	if es[1]["given_as"] != givenText || es[1]["file_text"] != long || es[1]["text_source"] != "edited by staff" {
		t.Errorf("the handout's staff text: %v", es[1])
	}
	if es[2]["given_as"] != givenText || es[2]["file_text"] != markdown || es[2]["text_source"] != nil {
		t.Errorf("the notes, whose text is pending: %v", es[2])
	}
	if strings.Count(raw, "short.") != 1 {
		t.Errorf("the body given twice, Core's and the runtime's: %s", raw)
	}
	if fs.count("/reading.pdf") != 0 || fs.count("/handout.docx") != 0 || fs.count("/notes.md") != 1 {
		t.Errorf("fetched %d, %d, %d", fs.count("/reading.pdf"), fs.count("/handout.docx"), fs.count("/notes.md"))
	}
	if texts, _ := vc.calls(); strings.Join(texts, " ") != fileHandout+":1 "+fileHandout+":2 "+fileHandout+":3" {
		t.Errorf("document_text read %v", texts)
	}
	// Kept by the file: read again, not asked for again; dropped by the
	// file's id, it is, and the other file's is kept.
	getVersion(t, r, `{"document_id":"`+docID+`"}`)
	if texts, _ := vc.calls(); len(texts) != 3 {
		t.Errorf("the text kept was read again: %v", texts)
	}
	before := cache.Stats().Readings
	cache.DropText(fileHandout)
	if got := cache.Stats().Readings; got != before-1 {
		t.Errorf("dropping the handout's text: %d readings of %d", got, before)
	}
	getVersion(t, r, `{"document_id":"`+docID+`"}`)
	if texts, _ := vc.calls(); len(texts) != 6 {
		t.Errorf("the handout's text was not read again: %v", texts)
	}
}

// What one call gives of a version is bounded: each file at most its
// first part, with next_part naming the file; all of them at most
// versionResults results' worth of text, and one PDF part's pages of file
// parts. A file past that is named with the call that reads it, unfetched
// where it would be a file part and the pages are spent.
func TestVersionCaps(t *testing.T) {
	fs := newFileServer(t)
	// The version's own text, beside its files, takes a third of the room.
	body := strings.Repeat("Read the files in order. ", 120)
	vc := &versionCore{result: func() string {
		return versionResult(fs, body,
			vfile{id: fileSlides, name: "long.txt", ct: "text/plain", path: "/long.txt", size: 130000},
			vfile{id: fileHandout, name: "reading.pdf", ct: "application/pdf", path: "/reading.pdf", size: len(reading)},
			vfile{id: fileNotes, name: "syllabus.pdf", ct: "application/pdf", path: "/syllabus.pdf", size: len(pdf)},
			vfile{id: fileFourth, name: "more.txt", ct: "text/plain", path: "/long.txt", size: 130000})
	}}
	r := Runner{Client: core.NewClient(&fakeCore{respond: vc.respond}), Files: NewHTTPFetcher(fs.Client()), FileInput: true,
		Texts: NewTextCache(0), MaxResultBytes: 8192, PartPages: 2, Office: &stubOffice{}}
	c, files, raw := getVersion(t, r, `{"document_id":"`+docID+`"}`)
	es := entriesOf(t, c)
	if len(es) != 4 || len(raw) > versionResults*8192 {
		t.Fatalf("%d files in %d bytes: %s", len(es), len(raw), raw)
	}
	np, _ := es[0]["next_part"].(map[string]any)
	args, _ := np["arguments"].(map[string]any)
	if es[0]["given_as"] != givenText || es[0]["part"] != float64(1) || args[FileIDArg] != fileSlides || args[FilePartArg] != float64(2) ||
		args["version_id"] != "v1" || len(es[0]["file_text"].(string)) > r.withDefaults().partBudget() {
		t.Errorf("a long file's first part: %v", es[0])
	}
	if es[1]["given_as"] != givenFile || len(files) != 1 || files[0].Name != "reading.pdf" {
		t.Errorf("the PDF of two pages, the part's: %v, %d files", es[1], len(files))
	}
	for i, id := range map[int]string{2: fileNotes, 3: fileFourth} {
		np, _ := es[i]["next_part"].(map[string]any)
		args, _ := np["arguments"].(map[string]any)
		if es[i]["given_as"] != givenNot || args[FileIDArg] != id || args[FilePartArg] != nil || !strings.Contains(es[i]["note"].(string), roomTaken) {
			t.Errorf("file %d, past the room: %v", i+1, es[i])
		}
	}
	if fs.count("/syllabus.pdf") != 0 {
		t.Error("a PDF past the pages was fetched")
	}
}

// file_id reads one file of the version alone, as a version of one file is
// read: in parts, and its pages, each call naming the file again; an id of
// no file of the version is said so; file_part or file_pages without it,
// of a version of several, give the version's files and say to name one;
// an id that is none is refused before Core.
func TestVersionPagingByFile(t *testing.T) {
	fs := newFileServer(t)
	vc := &versionCore{result: func() string {
		return versionResult(fs, "",
			vfile{id: fileSlides, name: "notes.md", ct: "text/markdown", path: "/notes.md", size: len(markdown)},
			vfile{id: fileHandout, name: "long.txt", ct: "text/plain", path: "/long.txt", size: 130000})
	}}
	r := Runner{Client: core.NewClient(&fakeCore{respond: vc.respond}), Files: NewHTTPFetcher(fs.Client()), Texts: NewTextCache(0)}
	c, _, raw := getVersion(t, r, `{"document_id":"`+docID+`","file_id":"`+fileHandout+`","file_part":2}`)
	rec, _ := c["file"].(map[string]any)
	np, _ := rec["next_part"].(map[string]any)
	args, _ := np["arguments"].(map[string]any)
	if c["files"] != nil || rec["file_id"] != fileHandout || rec["name"] != "long.txt" || rec["part"] != float64(2) ||
		args[FileIDArg] != fileHandout || args[FilePartArg] != float64(3) || !strings.Contains(rec["note"].(string), "which name this file of this version") {
		t.Errorf("part 2 of the long file: %s", raw[:min(len(raw), 800)])
	}
	if !strings.HasPrefix(c["file_text"].(string), "line of text") {
		t.Errorf("part 2's text: %.80q", c["file_text"])
	}

	c, _, raw = getVersion(t, r, `{"document_id":"`+docID+`","file_id":"`+fileNotes+`"}`)
	if c["file"] != nil || c["files"] != nil || !strings.Contains(c["files_note"].(string), "no file of this version has file_id "+fileNotes) {
		t.Errorf("a file of no such id: %s", raw)
	}
	c, _, raw = getVersion(t, r, `{"document_id":"`+docID+`","file_part":2}`)
	if len(entriesOf(t, c)) != 2 || !strings.Contains(c["files_note"].(string), "file_part and file_pages are of one file: name it with file_id") {
		t.Errorf("file_part naming no file: %s", raw[:min(len(raw), 800)])
	}
	parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", `{"document_id":"`+docID+`","file_id":"notes.md"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if code, msg := errorOf(t, parts[0]); code != core.CodeInvalidArgument || !strings.Contains(msg, "file_id is the id of one of the version's files") {
		t.Errorf("a file_id that is none: %s", parts[0].Content)
	}
}

// A version of one file, as a Core of several files lists it, is given as
// it always was: the file's record and text, and a next_part that names
// the version alone; its record names the file, by its name and id.
func TestVersionOfOneFileUnchanged(t *testing.T) {
	fs := newFileServer(t)
	listed := &versionCore{result: func() string {
		return versionResult(fs, "", vfile{id: fileSlides, name: "long.txt", ct: "text/plain", path: "/long.txt", size: 130000})
	}}
	before := &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
		return executed(documentResult(fs.url("/long.txt"), "long.txt", "text/plain", 130000)), nil
	}}
	var got [2]map[string]any
	for i, f := range []*fakeCore{{respond: listed.respond}, before} {
		r := Runner{Client: core.NewClient(f), Files: NewHTTPFetcher(fs.Client()), Texts: NewTextCache(0)}
		c, _, raw := getVersion(t, r, `{"document_id":"`+docID+`","file_part":2}`)
		if c["files"] != nil || c["files_note"] != nil {
			t.Errorf("a version of one file given as one of several: %s", raw[:min(len(raw), 600)])
		}
		got[i] = c
	}
	a, b := got[0]["file"].(map[string]any), got[1]["file"].(map[string]any)
	if a["file_id"] != fileSlides || a["position"] != float64(1) || b["file_id"] != nil {
		t.Errorf("the records' files: %v and %v", a, b)
	}
	delete(a, "file_id")
	delete(a, "position")
	if encodeJSON(a) != encodeJSON(b) || got[0]["file_text"] != got[1]["file_text"] {
		t.Errorf("a version of one file listed, and one of before:\n%v\n%v", a, b)
	}
	if np, _ := a["next_part"].(map[string]any); np["arguments"].(map[string]any)[FileIDArg] != nil {
		t.Errorf("next_part names the one file: %v", np)
	}
}

// A file whose URL has lapsed is fetched again from a fresh one, which
// document_file gives with the caller's own token; nothing of either URL
// reaches the model.
func TestVersionFreshURL(t *testing.T) {
	fs := newFileServer(t)
	vc := &versionCore{fresh: func(string) string { return fs.url("/notes.md") }, result: func() string {
		return versionResult(fs, "",
			vfile{id: fileSlides, name: "notes.md", ct: "text/markdown", path: "/expired.pdf", size: len(markdown)},
			vfile{id: fileHandout, name: "more.md", ct: "text/markdown", path: "/notes.md", size: len(markdown)})
	}}
	r := Runner{Client: core.NewClient(&fakeCore{respond: vc.respond}), Files: NewHTTPFetcher(fs.Client()), Texts: NewTextCache(0)}
	c, _, _ := getVersion(t, r, `{"document_id":"`+docID+`"}`)
	es := entriesOf(t, c)
	if es[0]["given_as"] != givenText || es[0]["file_text"] != markdown {
		t.Errorf("the file at a lapsed URL: %v", es[0])
	}
	if _, files := vc.calls(); len(files) != 1 || files[0] != fileSlides {
		t.Errorf("document_file asked for %v", files)
	}
}
