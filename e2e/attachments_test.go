package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// attachedFile is a file a person attaches to a question.
type attachedFile struct {
	name, contentType string
	data              []byte
}

// askWithFiles has p open a conversation with the member respondent,
// asking body with files attached, as Core's front end does: an upload URL
// for each file (conversation.upload_url), its bytes PUT there, and the
// uploads named in conversation.open's attachments. It returns the
// conversation and the question.
func (w *world) askWithFiles(t *testing.T, p person, respondent, body string, files ...attachedFile) (conv, msg string) {
	t.Helper()
	w.answersInSite(t, respondent)
	var named []map[string]any
	for _, f := range files {
		up := result[struct {
			UploadURL   string            `json:"upload_url"`
			Headers     map[string]string `json:"headers"`
			UploadToken string            `json:"upload_token"`
		}](t, w.api, p.token, "GET", w.path("/conversations/upload-url?"+url.Values{"content_type": {f.contentType}}.Encode()), nil)
		ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, up.UploadURL, bytes.NewReader(f.data))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		for k, v := range up.Headers {
			req.Header.Set(k, v)
		}
		resp, err := w.api.hc.Do(req)
		cancel()
		if err != nil {
			t.Fatalf("the PUT of %s: %v", f.name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the PUT of %s: HTTP %d", f.name, resp.StatusCode)
		}
		named = append(named, map[string]any{"upload_token": up.UploadToken, "filename": f.name})
	}
	r := result[struct {
		ConversationID string `json:"conversation_id"`
		MessageID      string `json:"message_id"`
	}](t, w.api, p.token, "POST", w.path("/conversations"), map[string]any{"respondent_member_id": respondent, "body": body, "attachments": named})
	if r.ConversationID == "" || r.MessageID == "" {
		t.Fatalf("%s asked with files, and Core named no conversation or message", p.name)
	}
	return r.ConversationID, r.MessageID
}

// filesQuestion is what a student asks with their essay and slides.
const filesQuestion = "Is my essay right, and do my slides agree with it?"

// The student's files: an essay of two pages, and a deck of three slides
// whose second has speaker notes.
var (
	studentEssay = doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"My essay: merge sort is stable."}},
		doctexttest.PDFPage{CJK: []string{"合併排序是穩定的排序"}})
	studentSlides = doctexttest.PPTX(
		doctexttest.Slide{Title: "Merge sort", Body: []doctexttest.Bullet{{Text: "Split, sort, merge"}}},
		doctexttest.Slide{Title: "Stability", Body: []doctexttest.Bullet{{Text: "Equal keys keep their order"}}, Notes: "Say it slowly."},
		doctexttest.Slide{Title: "穩定性", Body: []doctexttest.Bullet{{Text: "相等的鍵保持次序"}}},
	)
)

// fileBlock is what the question's message says of one of its files, as
// the runtime gives it: the file, its record, its text.
type fileBlock struct {
	Attachment struct {
		ID       string `json:"attachment_id"`
		Filename string `json:"filename"`
	} `json:"attachment"`
	File struct {
		GivenAs       string `json:"given_as"`
		ConvertedTo   string `json:"converted_to"`
		ExtractedFrom string `json:"extracted_from"`
	} `json:"file"`
	FileText string `json:"file_text"`
}

// filesResponder is a model asked filesQuestion: it answers with what it
// was given of the question's files, as the runtime gave them: for each,
// how (a file, or text), what LibreOffice made of it, the pages of a PDF
// it had as a file, and the text beside it or instead of it; and whether it
// was told of them before the question and offered attachment_get.
func filesResponder(req fakellm.ChatRequest) fakellm.ChatResponse {
	var q *fakellm.ChatMessage
	for i := range req.Messages {
		if m := &req.Messages[i]; m.Role == "user" && strings.Contains(m.Text(), filesQuestion) {
			q = m
		}
	}
	if q == nil {
		return fakellm.DefaultResponder(req)
	}
	var pages []int
	parts, _ := q.Content.([]any)
	for _, p := range parts {
		part, _ := p.(map[string]any)
		file, _ := part["file"].(map[string]any)
		data, _ := file["file_data"].(string)
		if raw, ok := strings.CutPrefix(data, "data:application/pdf;base64,"); ok {
			if pdf, err := base64.StdEncoding.DecodeString(raw); err == nil {
				n, _ := doctext.PDFPages(context.Background(), pdf, doctext.Limits{})
				pages = append(pages, n)
			}
		}
	}
	text := q.Text()
	var said []string
	if i := strings.Index(text, "[Message 1 carries 2 files"); i < 0 || i > strings.Index(text, filesQuestion) {
		said = append(said, "not told of the files before the question")
	}
	if !offered(req, "attachment_get") {
		said = append(said, "not offered attachment_get")
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "[The file ") || i+1 == len(lines) {
			continue
		}
		var b fileBlock
		if err := json.Unmarshal([]byte(lines[i+1]), &b); err != nil {
			said = append(said, "a block that does not read: "+err.Error())
			continue
		}
		s := b.Attachment.Filename + " given as " + b.File.GivenAs
		if b.File.ConvertedTo != "" {
			s += ", converted to " + b.File.ConvertedTo
		}
		if b.File.ExtractedFrom != "" {
			s += ", extracted from " + b.File.ExtractedFrom
		}
		if b.File.GivenAs == "file" && len(pages) > 0 {
			s += fmt.Sprintf(", %d pages", pages[0])
			pages = pages[1:]
		}
		if b.FileText != "" {
			s += ": " + b.FileText
		}
		said = append(said, s)
	}
	return fakellm.Reply(strings.Join(said, " | "))
}

// offered reports whether the request offers the tool name.
func offered(req fakellm.ChatRequest, name string) bool {
	for _, tl := range req.Tools {
		if tl.Function.Name == name {
			return true
		}
	}
	return false
}

// filesWithTheQuestion: Ken asks Sato's course tutor, whose model takes
// files, with his essay (a PDF) and his slides (a .pptx) attached; Yuki
// asks her own agent, whose model takes none, with the same. The runtime
// fetches the files through Core (conversation.attachment, with each
// agent's own token), and gives each model what it takes, with the
// question: the tutor's model the essay as a PDF of two pages and the deck
// as LibreOffice's PDF of it, its speaker notes beside it; Yuki's agent's
// model the text of both. Each is told of the files before the question,
// and offered attachment_get; each answer is posted in Core.
func filesWithTheQuestion(t *testing.T, w *world) {
	conv := realOffice(t)
	m := newModel(t, filesResponder)
	takesFiles := map[string]any{"model": map[string]any{"capabilities": map[string]any{"file_input": true}},
		"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 120}}}
	textOnly := map[string]any{"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 120}}}
	rt := w.startRuntime(t, m, runtimeConf{office: conv, agents: []agentConf{
		{id: "tutor", seat: w.tutor, over: takesFiles}, {id: "yuki-helper", seat: w.own, over: textOnly}}})
	rt.waitPolling("tutor")
	rt.waitPolling("yuki-helper")
	files := []attachedFile{{"essay.pdf", "application/pdf", studentEssay}, {"slides.pptx", doctexttest.PPTXType, studentSlides}}

	kens, kenQ := w.askWithFiles(t, w.ken, w.tutor.member, filesQuestion, files...)
	yukis, yukiQ := w.askWithFiles(t, w.yuki, w.own.member, filesQuestion, files...)

	wantTutor := "essay.pdf given as file, 2 pages | slides.pptx given as file, converted to pdf, 3 pages: ## Slide 2\nNotes: Say it slowly."
	if a := w.waitAnswer(t, w.ken, kens, w.tutor.member); a.text() != wantTutor || a.replyTo() != kenQ {
		t.Errorf("the tutor's answer is %q in reply to %s; want %q in reply to %s", a.text(), a.replyTo(), wantTutor, kenQ)
	}
	wantHelper := "essay.pdf given as text, extracted from pdf: ## Page 1\nMy essay: merge sort is stable.\n\n## Page 2\n合併排序是穩定的排序 | " +
		"slides.pptx given as text, extracted from pptx: ## Slide 1: Merge sort\n- Split, sort, merge\n\n" +
		"## Slide 2: Stability\n- Equal keys keep their order\nNotes: Say it slowly.\n\n## Slide 3: 穩定性\n- 相等的鍵保持次序"
	if a := w.waitAnswer(t, w.yuki, yukis, w.own.member); a.text() != wantHelper || a.replyTo() != yukiQ {
		t.Errorf("Yuki's agent's answer is %q in reply to %s; want %q in reply to %s", a.text(), a.replyTo(), wantHelper, yukiQ)
	}
	for _, req := range m.Requests() {
		if strings.Contains(string(req.Raw), "/v1/blobs/") {
			t.Error("a download URL reached the model")
		}
	}
}
