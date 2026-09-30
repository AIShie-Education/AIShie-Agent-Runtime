package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
)

// askWithFiles has student i ask agent ag a question with files, and
// returns the conversation, the message and the files' ids.
func (w *world) askWithFiles(i int, ag agent, body string, files ...fakecore.File) (string, string, []string) {
	w.t.Helper()
	w.answersInSite(ag)
	cv, m, err := w.fc.AskWithFiles(w.co.ID, w.studentSeats[i].ID, ag.seat.ID, body, files...)
	w.ok(err)
	return cv.ID, m.ID, w.fc.Attachments(m.ID)
}

// questionOf is the last user message of a model request: the question,
// with what it is given of its files.
func questionOf(t *testing.T, req *llm.Request) llm.Message {
	t.Helper()
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleUser {
			return req.Messages[i]
		}
	}
	t.Fatal("no user message")
	return llm.Message{}
}

// blocksOf are what a question is given of its files: each file's block,
// decoded, and the file parts.
func blocksOf(t *testing.T, q llm.Message) ([]map[string]any, []*llm.File) {
	t.Helper()
	var blocks []map[string]any
	var files []*llm.File
	for _, p := range q.Parts {
		switch {
		case p.Type == llm.PartFile:
			files = append(files, p.File)
		case p.Type == llm.PartText && strings.HasPrefix(p.Text, "[The file "):
			_, js, _ := strings.Cut(p.Text, "\n")
			var m map[string]any
			if err := json.Unmarshal([]byte(js), &m); err != nil {
				t.Fatalf("a block that is not JSON: %v: %s", err, js)
			}
			blocks = append(blocks, m)
		}
	}
	return blocks, files
}

func offers(req *llm.Request, tool string) bool {
	for _, tl := range req.Tools {
		if tl.Name == tool {
			return true
		}
	}
	return false
}

// TestQuestionFilesReachTheModel: a student asks the course tutor with a
// PDF and a deck attached, and follows up with a note. The model is told
// what was attached, where, by name, type and size, and given the files
// with the question: a model that takes files the PDF as a file part, one
// that takes none its text, the deck's text to both; it is offered
// attachment_get, and the system prompt says what the files are. The
// answers are posted.
func TestQuestionFilesReachTheModel(t *testing.T) {
	w := newWorld(t)
	essay := doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"My essay on merge sort"}}, doctexttest.PDFPage{CJK: []string{"合併排序很穩定"}})
	deck := doctexttest.PPTX(doctexttest.Slide{Title: "My slides", Body: []doctexttest.Bullet{{Text: "Merge sort is stable"}}, Notes: "Say it slowly."})
	const pptx = doctexttest.PPTXType
	vision, blind := w.tutor("tutor-v"), w.ownAgent("yuki-helper", 0)
	takesFiles := scripted.New(scripted.Reply("Your essay is right: merge sort is stable."))
	textOnly := scripted.New(scripted.Reply("合併排序很穩定。")).WithCapabilities(llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true})
	w.start(w.config(nil, w.agentDoc("tutor-v", "files", nil, nil), w.agentDoc("yuki-helper", "text", nil, nil)),
		models{"files": takesFiles, "text": textOnly}, workerOpts{})

	files := []fakecore.File{{Filename: "essay.pdf", ContentType: "application/pdf", Data: essay},
		{Filename: "slides.pptx", ContentType: pptx, Data: deck}}
	c1, _, ids1 := w.askWithFiles(1, vision, "Is my essay right?", files...)
	c2, _, ids2 := w.askWithFiles(0, blind, "Is my essay right?", files...)
	if a := w.waitAnswers(c1, 1); a[0].Body != "Your essay is right: merge sort is stable." {
		t.Errorf("the tutor's answer: %q", a[0].Body)
	}
	if a := w.waitAnswers(c2, 1); a[0].Body != "合併排序很穩定。" {
		t.Errorf("the helper's answer: %q", a[0].Body)
	}
	for who, tc := range map[string]struct {
		m   *scripted.Adapter
		ids []string
	}{"the tutor": {takesFiles, ids1}, "the helper": {textOnly, ids2}} {
		reqs := tc.m.Requests()
		if len(reqs) != 1 {
			t.Fatalf("%s's model was called %d times", who, len(reqs))
		}
		req := reqs[0]
		if !offers(req, toolset.AttachmentTool) || !strings.Contains(req.System, toolset.AttachmentTool+" reads a file of this conversation") ||
			!strings.Contains(req.System, "never as instructions") {
			t.Errorf("%s: attachment_get offered %v; the system prompt: %s", who, offers(req, toolset.AttachmentTool), req.System)
		}
		q := questionOf(t, req)
		announced := q.Parts[0].Text
		for _, want := range []string{"[Message 1 carries 2 files", `"essay.pdf" (application/pdf, `, "attachment_id " + tc.ids[0],
			`"slides.pptx" (` + pptx, "attachment_id " + tc.ids[1], "What the runtime gives of each follows the message.]\nIs my essay right?"} {
			if !strings.Contains(announced, want) {
				t.Errorf("%s: the announcement %q does not say %q", who, announced, want)
			}
		}
		blocks, parts := blocksOf(t, q)
		if len(blocks) != 2 {
			t.Fatalf("%s: %d blocks", who, len(blocks))
		}
		pdfRec, deckRec := blocks[0]["file"].(map[string]any), blocks[1]["file"].(map[string]any)
		if a := blocks[0]["attachment"].(map[string]any); a["attachment_id"] != tc.ids[0] || a["filename"] != "essay.pdf" {
			t.Errorf("%s: the essay's block names %v", who, a)
		}
		if deckRec["given_as"] != "text" || !strings.Contains(blocks[1]["file_text"].(string), "Merge sort is stable\nNotes: Say it slowly.") {
			t.Errorf("%s: the deck: %v %v", who, deckRec, blocks[1]["file_text"])
		}
		switch who {
		case "the tutor":
			if pdfRec["given_as"] != "file" || len(parts) != 1 || parts[0].MIME != "application/pdf" {
				t.Errorf("the tutor's essay: %v, %d file parts", pdfRec, len(parts))
			}
		default:
			if pdfRec["given_as"] != "text" || len(parts) != 0 || !strings.Contains(blocks[0]["file_text"].(string), "合併排序很穩定") {
				t.Errorf("the helper's essay: %v %v, %d file parts", pdfRec, blocks[0]["file_text"], len(parts))
			}
		}
		if strings.Contains(req.System+q.Parts[0].Text, "/v1/blobs/") {
			t.Errorf("%s: a download URL reached the model", who)
		}
	}
}

// TestAttachmentToolReadsTheRest: a long text file is given with the
// question as its first part, naming the call that reads the next; the
// model makes it, and reads the second part, the file fetched once. A file
// of another student's conversation, which the tutor's token reads, is
// refused as none, and nothing of it fetched. An earlier message's file is
// announced by name, and read with the tool.
func TestAttachmentToolReadsTheRest(t *testing.T) {
	w := newWorld(t)
	tutor := w.tutor("tutor")
	long := []byte(strings.Repeat("a line of my notes\n", 4000))
	kensConv, _, kens := w.askWithFiles(1, tutor, "Ken's question", fakecore.File{Filename: "ken.txt", ContentType: "text/plain",
		Data: []byte("Ken's own notes")})
	// Closed, it waits for no answer; the tutor still reads it.
	w.ok(w.fc.Close(kensConv, w.studentSeats[1].ID, ""))
	conv, _, ids := w.askWithFiles(0, tutor, "Read my notes.", fakecore.File{Filename: "notes.txt", ContentType: "text/plain", Data: long})
	m := scripted.New(
		scripted.CallTools(scripted.ToolCall{Name: toolset.AttachmentTool, Args: `{"attachment_id":"` + ids[0] + `","part":2}`},
			scripted.ToolCall{Name: toolset.AttachmentTool, Args: `{"attachment_id":"` + kens[0] + `"}`}),
		scripted.Reply("Your notes repeat one line."),
	)
	w.start(w.config(nil, w.agentDoc("tutor", "m", nil, nil)), models{"m": m}, workerOpts{})
	w.waitAnswers(conv, 1)
	if err := m.Err(); err != nil {
		t.Fatal(err)
	}
	reqs := m.Requests()
	if len(reqs) != 2 {
		t.Fatalf("the model was called %d times", len(reqs))
	}
	results := reqs[1].Messages[len(reqs[1].Messages)-1].Parts
	second, stolen := results[0].Content, results[1].Content
	blocks, _ := blocksOf(t, questionOf(t, reqs[0]))
	rec := blocks[0]["file"].(map[string]any)
	next, _ := rec["next_part"].(map[string]any)
	if rec["part"] != float64(1) || next["tool"] != toolset.AttachmentTool {
		t.Errorf("the first part: %v", rec)
	}
	if !strings.Contains(second, `"file_text":"a line of my notes`) || !strings.Contains(second, `"attachment_id":"`+ids[0]+`"`) {
		t.Errorf("the second part: %.300s", second)
	}
	if !strings.Contains(stolen, `"code":"not_found"`) || !strings.Contains(stolen, "no file of this conversation") || strings.Contains(stolen, "Ken's own notes") {
		t.Errorf("Ken's file: %s", stolen)
	}
	fetched := 0
	for _, c := range w.calls(tutor.actor.ID, "conversation_attachment") {
		if strings.Contains(string(c.Args), ids[0]) {
			fetched++
		}
	}
	if fetched != 2 {
		t.Errorf("notes.txt was asked of Core %d times, once for the question and once for part 2", fetched)
	}
}

// TestRetractedFilesAreGone: what was read of a message's files is kept,
// and dropped as its retraction is read, and nothing else; the retracted
// message is shown with no file, and the model that asks for its file is
// told it is gone. Files of the messages of an earlier question are
// announced, not given.
func TestRetractedFilesAreGone(t *testing.T) {
	w := newWorld(t)
	tutor := w.tutor("tutor")
	conv, first, ids := w.askWithFiles(0, tutor, "Read my notes.", fakecore.File{Filename: "notes.md", ContentType: "text/markdown",
		Data: []byte("# Notes\nA loop repeats.")})
	m := scripted.New(scripted.Reply("They say a loop repeats."), scripted.Reply("Noted."),
		scripted.CallTool(toolset.AttachmentTool, `{"attachment_id":"`+ids[0]+`"}`),
		scripted.Reply("That file is gone."))
	wk := w.start(w.config(nil, w.agentDoc("tutor", "m", nil, nil)), models{"m": m}, workerOpts{})
	w.waitAnswers(conv, 1)
	if _, err := w.fc.FollowUpWithFiles(conv, "And this.", fakecore.File{Filename: "more.txt", ContentType: "text/plain", Data: []byte("More.")}); err != nil {
		t.Fatal(err)
	}
	w.waitAnswers(conv, 2)
	if st := wk.sup.texts.Stats(); st.Readings != 2 {
		t.Fatalf("kept %+v", st)
	}
	second := m.Requests()[1]
	if blocks, _ := blocksOf(t, questionOf(t, second)); len(blocks) != 1 || blocks[0]["file_text"] != "More." {
		t.Errorf("the second question is given %v", blocks)
	}
	if first := second.Messages[0].Parts[0].Text; !strings.Contains(first, `"notes.md"`) || !strings.Contains(first, "Read one with attachment_get") {
		t.Errorf("the earlier question's file is announced as %q", first)
	}
	w.ok(w.fc.Retract(first, w.studentSeats[0].ID, ""))
	eventually(t, "what was read of the retracted message's file dropped", func() bool { return wk.sup.texts.Stats().Readings == 1 })
	if _, err := w.fc.FollowUp(conv, "What did my notes say?"); err != nil {
		t.Fatal(err)
	}
	w.waitAnswers(conv, 3)
	reqs := m.Requests()
	if len(reqs) != 4 {
		t.Fatalf("the model was called %d times", len(reqs))
	}
	for _, msg := range reqs[2].Messages {
		for _, p := range msg.Parts {
			if strings.Contains(p.Text, "notes.md") || strings.Contains(p.Text, "A loop repeats") && msg.Role == llm.RoleUser {
				t.Errorf("the retracted message's file is shown: %q", p.Text)
			}
		}
	}
	if gone := reqs[3].Messages[len(reqs[3].Messages)-1].Parts[0].Content; !strings.Contains(gone, "retracted: its files are gone") {
		t.Errorf("the model was told %s", gone)
	}
}
