package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

// The conversation the tests answer, another the agent's token reads too,
// and the messages that carry the files.
const (
	thisConv  = "0192f3c1-c000-7b4a-9c3d-2e1f0a9b8c7d"
	otherConv = "0192f3c1-c001-7b4a-9c3d-2e1f0a9b8c7d"
	question  = "0192f3c1-d000-7b4a-9c3d-2e1f0a9b8c7d"
	followUp  = "0192f3c1-d001-7b4a-9c3d-2e1f0a9b8c7d"
)

// attached is a file a message carries, as a test's Core gives it.
type attached struct {
	id, conv, msg      string
	seq                int64
	name, ct, checksum string
	size               int
	url                string
	retracted          bool
}

// attachmentCore answers conversation_attachment for the files it holds,
// as Core does: not_found for one it holds not, not_found retracted for a
// retracted message's; and counts what it is asked.
type attachmentCore struct {
	mu    sync.Mutex
	files map[string]*attached
	asked map[string]int
}

func newAttachmentCore(files ...*attached) *attachmentCore {
	c := &attachmentCore{files: map[string]*attached{}, asked: map[string]int{}}
	for _, f := range files {
		c.files[f.id] = f
	}
	return c
}

func (c *attachmentCore) Call(_ context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
	var a struct {
		CourseID     string `json:"course_id"`
		AttachmentID string `json:"attachment_id"`
	}
	_ = json.Unmarshal(args, &a)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked[a.AttachmentID]++
	f := c.files[a.AttachmentID]
	switch {
	case tool != core.ToolAttachment || a.CourseID != courseID:
		return &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeInvalidArgument, Message: "not this"}}, nil
	case f == nil:
		return &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeNotFound, Message: "no such attachment in this course"}}, nil
	case f.retracted:
		return &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeNotFound, Message: "retracted",
			Details: map[string]any{"reason": core.ReasonRetracted}}}, nil
	}
	res := map[string]any{"id": f.id, "filename": f.name, "content_type": f.ct, "byte_size": f.size, "created_at": "2026-09-30T10:00:00Z",
		"conversation_id": f.conv, "message_id": f.msg, "message_seq": f.seq, "author_member_id": "m", "download_url": f.url,
		"expires_at": "2026-09-30T10:15:00Z"}
	if f.checksum != "" {
		res["checksum"] = f.checksum
	}
	b, _ := json.Marshal(res)
	return executed(string(b)), nil
}

func (c *attachmentCore) times(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked[id]
}

// fileAt is a file of the file server's, attached to the question as n.
func fileAt(fs *fileServer, n int, path, name, ct string, size int) *attached {
	return &attached{id: fmt.Sprintf("0192f3c1-f%03d-7b4a-9c3d-2e1f0a9b8c7d", n), conv: thisConv, msg: question, seq: 1, name: name, ct: ct,
		size: size, url: fs.url(path), checksum: fmt.Sprintf("sha256:%064d", n)}
}

// attachmentSet is a course tutor's set with AttachmentTool.
func attachmentSet(t testing.TB) *Set {
	t.Helper()
	s, err := snapshot(t).Build(tutorPerms, config.Tools{}, ReadOnly, toolschema.OpenAIStrict, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err = s.WithAttachments(config.Tools{}, toolschema.OpenAIStrict, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// readAttachment calls AttachmentTool with args, and returns the result's
// content and the file part, if any.
func readAttachment(t *testing.T, r Runner, args string) (llm.Part, map[string]any, *llm.File) {
	t.Helper()
	parts, err := attachmentSet(t).Run(context.Background(), r, courseID, []llm.Part{call("a", AttachmentTool, args)})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts[0].Content) > r.withDefaults().MaxResultBytes {
		t.Fatalf("a result of %d bytes", len(parts[0].Content))
	}
	var file *llm.File
	if len(parts) == 2 {
		file = parts[1].File
	}
	return parts[0], contentOf(t, parts[0]), file
}

func idArgs(id string, more ...string) string {
	return `{"attachment_id":"` + id + `"` + strings.Join(more, "") + `}`
}

// Each kind of file a question may carry, to a model that takes files and
// to one that takes none: a text file as its text to both; a PDF as a file
// part, or its text; a deck as LibreOffice's PDF with its speaker notes,
// or its text; an image as a file part, or what OCR reads of it, or a note
// that the model cannot see images; and anything else a note of what it
// is. The result names the file, and never its URL.
func TestAttachmentKinds(t *testing.T) {
	fs := newFileServer(t)
	img := ocr.State{Status: ocr.StatusDone, Text: &store.OCRText{Text: "看板：Notice", Pages: 1}}
	tests := []struct {
		name      string
		path, ct  string
		fileInput bool
		ocr       OCR
		office    Office
		givenAs   string
		mime      string // the file part's, "" for none
		text      string // in file_text
		note      string
	}{
		{name: "markdown, to a model that takes files", path: "/notes.md", ct: "text/markdown", fileInput: true, givenAs: givenText,
			text: "Read chapter 2 <before> the lab"},
		{name: "markdown, to a text-only model", path: "/notes.md", ct: "text/markdown", givenAs: givenText, text: "Read chapter 2"},
		{name: "code of no telling type", path: "/untyped", ct: "application/octet-stream", givenAs: givenText, text: "plain words"},
		{name: "a PDF, to a model that takes files", path: "/reading.pdf", ct: "application/pdf", fileInput: true, givenAs: givenFile,
			mime: "application/pdf"},
		{name: "a PDF, to a text-only model", path: "/reading.pdf", ct: "application/pdf", givenAs: givenText, text: "第三週：排序",
			note: "the runtime's text of its 2 pages"},
		{name: "a scan, to a text-only model, with OCR", path: "/scanned.pdf", ct: "application/pdf", givenAs: givenText, text: "Midterm",
			ocr: &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State {
				return ocr.State{Status: ocr.StatusDone, Text: &store.OCRText{Text: "## Page 1\nMidterm", Pages: 1, PagesOf: 2,
					Sections: []store.OCRSection{{N: 1}}}}
			}}, note: "the runtime's OCR of its 1 page"},
		{name: "a deck, to a model that takes files", path: "/deck.pptx", ct: pptxType, fileInput: true, givenAs: givenFile,
			mime: "application/pdf", text: "Notes: Draw the recursion tree.", note: "LibreOffice converted it to PDF",
			office: &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(2)}}},
		{name: "a deck, to a text-only model", path: "/deck.pptx", ct: pptxType, givenAs: givenText, text: "## Slide 1: Week 3: Sorting",
			office: &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(2)}}},
		{name: "an image, to a model that takes files", path: "/photo.png", ct: "image/png", fileInput: true, givenAs: givenFile,
			mime: "image/png"},
		{name: "an image, to a text-only model, with OCR", path: "/photo.png", ct: "image/png", givenAs: givenText, text: "看板：Notice",
			ocr:  &fakeOCR{respond: func(int, func(context.Context) ([]byte, error)) ocr.State { return img }},
			note: "the runtime's OCR of the image (this model cannot see images)"},
		{name: "an image, to a text-only model, without OCR", path: "/photo.png", ct: "image/png", givenAs: givenNot,
			ocr: &fakeOCR{off: "OCR is off"}, note: "this model cannot see images; the runtime has no OCR here"},
		{name: "an archive", path: "/archive.zip", ct: "application/zip", fileInput: true, givenAs: givenNot,
			note: "application/zip files are not read here"},
		{name: "a sound", path: "/talk.mp3", ct: "audio/mpeg", fileInput: true, givenAs: givenNot, note: "audio/mpeg files are not read here"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := fileAt(fs, i, tc.path, "file"+tc.path, tc.ct, 100)
			c := newAttachmentCore(f)
			r := Runner{Client: core.NewClient(c), Files: NewHTTPFetcher(fs.Client()), Texts: NewTextCache(0), FileInput: tc.fileInput,
				OCR: tc.ocr, Office: tc.office, Conversation: thisConv}
			res, content, file := readAttachment(t, r, idArgs(f.id))
			if res.IsError || content["status"] != "executed" {
				t.Fatalf("result %s", res.Content)
			}
			if strings.Contains(res.Content, "download_url") || strings.Contains(res.Content, signature) {
				t.Fatalf("the URL reached the model: %s", res.Content)
			}
			shown, _ := content["result"].(map[string]any)
			if shown["attachment_id"] != f.id || shown["filename"] != f.name || shown["content_type"] != tc.ct || fmt.Sprint(shown["message_seq"]) != "1" {
				t.Errorf("the file is shown as %v", shown)
			}
			rec := content["file"].(map[string]any)
			if rec["given_as"] != tc.givenAs {
				t.Errorf("given as %v, want %s: %v", rec["given_as"], tc.givenAs, rec)
			}
			switch {
			case tc.mime == "" && file != nil:
				t.Errorf("a file part %s", file.MIME)
			case tc.mime != "" && (file == nil || file.MIME != tc.mime):
				t.Errorf("the file part is %+v, want %s", file, tc.mime)
			}
			if text, _ := content["file_text"].(string); !strings.Contains(text, tc.text) {
				t.Errorf("file_text %q does not hold %q", text, tc.text)
			}
			wantNote(t, rec, tc.note)
		})
	}
}

// pptxType is PowerPoint's media type.
const pptxType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"

// AttachmentTool reads the files of the conversation being answered and
// of no other: another conversation's file, which the agent's token reads,
// is refused as one that is none is, and nothing of it is fetched; a
// retracted message's file is gone, and what was kept of it goes too; and
// arguments that name no file, or both a part and pages, are refused
// before Core is asked.
func TestAttachmentRefusals(t *testing.T) {
	fs := newFileServer(t)
	mine := fileAt(fs, 1, "/notes.md", "notes.md", "text/markdown", len(markdown))
	theirs := fileAt(fs, 2, "/reading.pdf", "reading.pdf", "application/pdf", len(reading))
	theirs.conv, theirs.msg = otherConv, followUp
	c := newAttachmentCore(mine, theirs)
	texts := NewTextCache(0)
	r := Runner{Client: core.NewClient(c), Files: NewHTTPFetcher(fs.Client()), Texts: texts, Conversation: thisConv}

	res, _, _ := readAttachment(t, r, idArgs(theirs.id))
	if code, msg := errorOf(t, res); !res.IsError || code != core.CodeNotFound || !strings.Contains(msg, "no file of this conversation") {
		t.Errorf("another conversation's file: %s", res.Content)
	}
	if fs.count("/reading.pdf") != 0 {
		t.Error("another conversation's file was fetched")
	}
	res, _, _ = readAttachment(t, r, idArgs("0192f3c1-ffff-7b4a-9c3d-2e1f0a9b8c7d"))
	if code, _ := errorOf(t, res); code != core.CodeNotFound {
		t.Errorf("a file that is none: %s", res.Content)
	}
	if res, _, _ = readAttachment(t, Runner{Client: core.NewClient(c), Files: r.Files}, idArgs(mine.id)); !res.IsError {
		t.Errorf("a runner that answers no conversation read a file: %s", res.Content)
	}

	// Read, and kept; then its message is retracted.
	if res, content, _ := readAttachment(t, r, idArgs(mine.id)); res.IsError || content["file_text"] != markdown {
		t.Fatalf("my file: %s", res.Content)
	}
	if texts.Stats().Readings != 1 {
		t.Fatalf("kept %+v", texts.Stats())
	}
	mine.retracted = true
	res, _, _ = readAttachment(t, r, idArgs(mine.id))
	if code, msg := errorOf(t, res); code != core.CodeNotFound || !strings.Contains(msg, "retracted: its files are gone") {
		t.Errorf("a retracted message's file: %s", res.Content)
	}
	if texts.Stats().Readings != 0 {
		t.Errorf("what was read of a retracted message's file is still kept: %+v", texts.Stats())
	}

	before := c.times(mine.id) + c.times(theirs.id)
	for _, args := range []string{`{}`, `{"attachment_id":"essay.pdf"}`, idArgs(mine.id, `,"part":0`), idArgs(mine.id, `,"part":"2"`),
		idArgs(mine.id, `,"part":2,"file_pages":"1"`), idArgs(mine.id, `,"file_pages":"1-40"`), idArgs(mine.id, `,"conversation_id":"x"`), `[1]`} {
		res, _, _ := readAttachment(t, r, args)
		if code, _ := errorOf(t, res); code != core.CodeInvalidArgument {
			t.Errorf("%s: %s", args, res.Content)
		}
	}
	if c.times(mine.id)+c.times(theirs.id) != before {
		t.Error("Core was asked about arguments the runtime refuses")
	}
}

// A long file is read in parts through AttachmentTool: the first says how
// many there are and names the call that reads the next, which reads it,
// the file fetched once; a PDF of many pages, to a model that takes files,
// in parts of its pages, and pages of it asked for as a PDF of their own.
func TestAttachmentInParts(t *testing.T) {
	fs := newFileServer(t)
	long := fileAt(fs, 1, "/long.txt", "long.txt", "text/plain", 130000)
	c := newAttachmentCore(long)
	r := Runner{Client: core.NewClient(c), Files: NewHTTPFetcher(fs.Client()), Texts: NewTextCache(0), Conversation: thisConv}
	var whole strings.Builder
	args := idArgs(long.id)
	for n := 1; ; n++ {
		res, content, _ := readAttachment(t, r, args)
		rec := content["file"].(map[string]any)
		if res.IsError || fmt.Sprint(rec["part"]) != fmt.Sprint(n) {
			t.Fatalf("part %d: %s", n, res.Content)
		}
		whole.WriteString(content["file_text"].(string))
		next, _ := rec["next_part"].(map[string]any)
		if next == nil {
			break
		}
		if next["tool"] != AttachmentTool {
			t.Fatalf("next_part %v", next)
		}
		b, _ := json.Marshal(next["arguments"])
		args = string(b)
		if n > 10 {
			t.Fatal("no end to the parts")
		}
	}
	if whole.String() != strings.Repeat("line of text\n", 10000) {
		t.Errorf("the parts are not the text: %d bytes", whole.Len())
	}
	if fs.count("/long.txt") != 1 {
		t.Errorf("the file was fetched %d times", fs.count("/long.txt"))
	}

	// A deck of 25 slides, to a model that takes files: parts of its
	// slides, and slides asked for.
	deck := lectureDeck(25)
	srv, hits := deckServer(t, deck)
	d := &attached{id: "0192f3c1-f100-7b4a-9c3d-2e1f0a9b8c7d", conv: thisConv, msg: question, seq: 1, name: "lecture.pptx", ct: pptxType,
		size: len(deck), url: srv.URL + "/file"}
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(25)}}
	rd := Runner{Client: core.NewClient(newAttachmentCore(d)), Files: NewHTTPFetcher(srv.Client()), Texts: NewTextCache(0), Office: o,
		FileInput: true, Conversation: thisConv}
	_, content, file := readAttachment(t, rd, idArgs(d.id))
	rec := content["file"].(map[string]any)
	next, _ := rec["next_part"].(map[string]any)
	if file == nil || fmt.Sprint(rec["parts"]) != "3" || rec["part_holds"] != "slides 1–10" || next["tool"] != AttachmentTool {
		t.Fatalf("the deck's first part: %v", rec)
	}
	_, content, file = readAttachment(t, rd, idArgs(d.id, `,"part":3`))
	if rec := content["file"].(map[string]any); file == nil || rec["part_holds"] != "slides 21–25" || rec["next_part"] != nil {
		t.Errorf("the deck's last part: %v", rec)
	}
	_, content, file = readAttachment(t, rd, idArgs(d.id, `,"file_pages":"7"`))
	if rec := content["file"].(map[string]any); file == nil || rec["part_holds"] != "slide 7" {
		t.Errorf("slide 7: %v", rec)
	}
	if hits.Load() != 3 {
		t.Errorf("the deck was fetched %d times, once a call", hits.Load())
	}
}

// What was read of a file is kept by the checksum Core worked out from its
// bytes: the same file attached again, to another message, is not read
// again; and what was kept goes when either message is retracted (Drop).
// A file of no checksum Core computed is kept by its id alone.
func TestAttachmentKept(t *testing.T) {
	fs := newFileServer(t)
	a := fileAt(fs, 1, "/reading.pdf", "reading.pdf", "application/pdf", len(reading))
	b := fileAt(fs, 1, "/reading.pdf", "reading (1).pdf", "application/pdf", len(reading))
	b.id, b.msg, b.seq = "0192f3c1-f009-7b4a-9c3d-2e1f0a9b8c7d", followUp, 3
	etag := fileAt(fs, 2, "/notes.md", "notes.md", "text/markdown", len(markdown))
	etag.checksum = `etag:"abc"`
	texts := NewTextCache(0)
	r := Runner{Client: core.NewClient(newAttachmentCore(a, b, etag)), Files: NewHTTPFetcher(fs.Client()), Texts: texts, Conversation: thisConv}
	for _, id := range []string{a.id, b.id, a.id} {
		if res, content, _ := readAttachment(t, r, idArgs(id)); res.IsError || !strings.Contains(content["file_text"].(string), "Reading 3") {
			t.Fatalf("%s: %s", id, res.Content)
		}
	}
	if fs.count("/reading.pdf") != 1 {
		t.Errorf("one file of two messages was fetched %d times", fs.count("/reading.pdf"))
	}
	readAttachment(t, r, idArgs(etag.id))
	if key := r.attachmentKey(attachmentFile(&core.AttachmentFile{Attachment: core.Attachment{ID: etag.id, Checksum: &etag.checksum,
		ContentType: etag.ct}}, courseID)); !strings.Contains(key, etag.id) || texts.get(key) == nil {
		t.Errorf("a file of an etag is not kept by its id: %q", key)
	}
	texts.Drop(followUp)
	readAttachment(t, r, idArgs(a.id))
	if fs.count("/reading.pdf") != 2 {
		t.Errorf("what was kept of the retracted message's file was used again: fetched %d times", fs.count("/reading.pdf"))
	}
}

// GiveAttachments gives the question's files in order as AttachmentTool
// would, while they fit what the first turn holds for files (two results
// of text, a PDF part's pages): a PDF and an image as file parts, a long
// text as its first part, naming the call that reads the next; a file past
// the room named with the call that reads it; a retracted message's file
// said to be gone; and none of another conversation.
func TestGiveAttachments(t *testing.T) {
	fs := newFileServer(t)
	pdfFile := fileAt(fs, 1, "/reading.pdf", "reading.pdf", "application/pdf", len(reading))
	photo := fileAt(fs, 2, "/photo.png", "photo.png", "image/png", 8)
	long1 := fileAt(fs, 3, "/long.txt", "long.txt", "text/plain", 130000)
	long2 := fileAt(fs, 4, "/long.txt", "long2.txt", "text/plain", 130000)
	long3 := fileAt(fs, 8, "/long.txt", "long3.txt", "text/plain", 130000)
	later := fileAt(fs, 5, "/notes.md", "notes.md", "text/markdown", len(markdown))
	later.msg, later.seq = followUp, 3
	gone := fileAt(fs, 6, "/notes.md", "gone.md", "text/markdown", len(markdown))
	gone.msg, gone.seq, gone.retracted = followUp, 3, true
	stray := fileAt(fs, 7, "/notes.md", "stray.md", "text/markdown", len(markdown))
	stray.conv, stray.msg, stray.seq = otherConv, followUp, 3
	c := newAttachmentCore(pdfFile, photo, long1, long2, long3, later, gone, stray)
	r := Runner{Client: core.NewClient(c), Files: NewHTTPFetcher(fs.Client()), Texts: NewTextCache(0), FileInput: true, Conversation: thisConv}
	of := func(a *attached) MessageFile {
		return MessageFile{Attachment: core.Attachment{ID: a.id, Filename: a.name, ContentType: a.ct, ByteSize: int64(a.size)},
			MessageID: a.msg, MessageSeq: a.seq}
	}
	given := r.GiveAttachments(context.Background(), courseID, []MessageFile{of(pdfFile), of(photo), of(long1), of(long2), of(long3), of(later),
		of(gone), of(stray)}, false)
	q, f := given[question], given[followUp]
	// The question: the PDF and its part, the photo and its part, the
	// first two long texts' first parts; the third is past the room.
	if len(q) != 7 || q[1].File == nil || q[1].File.MIME != "application/pdf" || q[3].File == nil || q[3].File.MIME != "image/png" {
		t.Fatalf("the question's parts: %d", len(q))
	}
	block := func(p llm.Part) map[string]any {
		t.Helper()
		heading, js, ok := strings.Cut(p.Text, "\n")
		if !ok || !strings.HasPrefix(heading, "[The file ") {
			t.Fatalf("a block %q", p.Text)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(js), &m); err != nil {
			t.Fatalf("a block: %v: %s", err, js)
		}
		if _, ok := m["status"]; ok || m["attachment"] == nil {
			t.Errorf("a block of the question has a status, or names no file: %s", js)
		}
		return m
	}
	first := block(q[4])
	rec := first["file"].(map[string]any)
	next, _ := rec["next_part"].(map[string]any)
	if rec["part"] == nil || next["tool"] != AttachmentTool || next["arguments"].(map[string]any)["part"] != float64(2) ||
		!strings.HasPrefix(first["file_text"].(string), "line of text") {
		t.Errorf("the first long text: %v", rec)
	}
	if rec := block(q[5])["file"].(map[string]any); rec["given_as"] != givenText {
		t.Errorf("the second long text: %v", rec)
	}
	third := block(q[6])["file"].(map[string]any)
	next, _ = third["next_part"].(map[string]any)
	if third["given_as"] != givenNot || !strings.Contains(noteOf(third), roomSpent) || next["arguments"].(map[string]any)["attachment_id"] != long3.id {
		t.Errorf("the third long text: %v", third)
	}
	// The follow-up: the notes (text is not out of room: the third long
	// text did not take any), the retracted one gone, the stray refused.
	if len(f) != 3 {
		t.Fatalf("the follow-up's parts: %d", len(f))
	}
	if m := block(f[0]); m["file_text"] != markdown {
		t.Errorf("the notes: %v", m)
	}
	if rec := block(f[1])["file"].(map[string]any); !strings.Contains(noteOf(rec), "retracted: its files are gone") || rec["next_part"] != nil {
		t.Errorf("the retracted file: %v", rec)
	}
	if rec := block(f[2])["file"].(map[string]any); !strings.Contains(noteOf(rec), "not a file of this conversation") {
		t.Errorf("the stray file: %v", rec)
	}
	if fs.count("/long.txt") != 3 {
		t.Errorf("the long texts were fetched %d times", fs.count("/long.txt"))
	}

	// With no tool to read more, nothing names one.
	r.Texts = NewTextCache(0)
	given = r.GiveAttachments(context.Background(), courseID, []MessageFile{of(long1), of(long2)}, true)
	for _, p := range given[question] {
		if strings.Contains(p.Text, AttachmentTool) || strings.Contains(p.Text, "next_part") {
			t.Errorf("a block with no tool names one: %s", p.Text[:min(len(p.Text), 300)])
		}
	}
	if !strings.Contains(given[question][0].Text, "part 2 cannot be read here") {
		t.Errorf("a long text with no tool: %s", given[question][0].Text[:300])
	}
}

// WithAttachments offers AttachmentTool beside the seat's tools, neither a
// read nor a write, with a schema each dialect takes; not where the
// configuration offers no tools or denies it.
func TestWithAttachments(t *testing.T) {
	base, err := snapshot(t).Build(tutorPerms, config.Tools{}, ReadOnly, toolschema.OpenAIStrict, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range toolschema.Dialects {
		s, err := base.WithAttachments(config.Tools{}, d, nil)
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if !s.Has(AttachmentTool) || base.Has(AttachmentTool) || s.Len() != base.Len()+1 {
			t.Fatalf("%s: %v", d, s.Names())
		}
		if strings.Join(s.Reads(), ",") != strings.Join(base.Reads(), ",") || len(s.Writes()) != 0 {
			t.Errorf("%s: reads %v, writes %v", d, s.Reads(), s.Writes())
		}
		var decl llm.Tool
		for _, tl := range s.Declarations() {
			if tl.Name == AttachmentTool {
				decl = tl
			}
		}
		if !json.Valid(decl.Schema) || !strings.Contains(string(decl.Schema), "attachment_id") || decl.Description == "" {
			t.Errorf("%s: %s", d, decl.Schema)
		}
	}
	for _, cfg := range []config.Tools{{Mode: "none"}, {Deny: []string{AttachmentTool}}, {Deny: []string{"attachment_*"}}} {
		s, err := base.WithAttachments(cfg, toolschema.OpenAIStrict, nil)
		if err != nil || s.Has(AttachmentTool) {
			t.Errorf("%+v: %v %v", cfg, s.Names(), err)
		}
	}
}

// A retraction drops what was kept for the message, and for each of its
// files, and nothing else: readings of documents, and of other messages'
// files, stay.
func TestTextCacheDrop(t *testing.T) {
	c := NewTextCache(0)
	rd := func(text string) *fileReading {
		return &fileReading{mt: "text/plain", res: &doctext.Result{Text: text}}
	}
	c.put("doc", rd("a document"))
	c.put("f1", rd("one"), "file1", question)
	c.put("f2", rd("two"), "file2", followUp)
	c.get("f1", "file3", followUp) // the same bytes, attached again
	c.Drop(followUp)
	if c.get("doc") == nil || c.get("f1") != nil || c.get("f2") != nil {
		t.Errorf("after dropping the follow-up: %+v", c.Stats())
	}
	c.put("f1", rd("one"), "file1", question)
	c.Drop("file1")
	if c.get("f1") != nil || c.Stats().Readings != 1 {
		t.Errorf("after dropping a file: %+v", c.Stats())
	}
	// A reading let go past the bound leaves no tag behind.
	small := NewTextCache(300)
	small.put("a", rd("x"), "t")
	small.put("b", rd(strings.Repeat("y", 40)), "u")
	if len(small.byTag) > small.Stats().Readings {
		t.Errorf("tags %v of %d readings", small.byTag, small.Stats().Readings)
	}
}
