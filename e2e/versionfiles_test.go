package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// uploadFiles is p putting a version of several files in the course, as
// Core's front end does since AIShie-Core #49: an upload URL for each file,
// named, its bytes PUT there, one material document made of the uploads,
// in order, with body as its text, and its version published. It returns
// the document's id and its files', in order.
func (w *world) uploadFiles(t *testing.T, p person, title, body string, files ...attachedFile) (string, []string) {
	t.Helper()
	doc, version, ids := w.uploadDraft(t, p, title, body, files...)
	w.api.call(t, http.StatusOK, p.token, "POST", w.path("/documents/"+doc+"/publish"), map[string]any{"version_id": version})
	return doc, ids
}

// uploadDraft is uploadFiles but for the publishing: the document is a
// draft, which only members who read drafts see. It returns the
// document's id, its version's and its files', in order.
func (w *world) uploadDraft(t *testing.T, p person, title, body string, files ...attachedFile) (string, string, []string) {
	t.Helper()
	var named []map[string]any
	for _, f := range files {
		q := url.Values{"kind": {"material"}, "content_type": {f.contentType}, "filename": {f.name}}
		up := result[struct {
			UploadURL   string            `json:"upload_url"`
			Headers     map[string]string `json:"headers"`
			UploadToken string            `json:"upload_token"`
		}](t, w.api, p.token, "GET", w.path("/upload-url?"+q.Encode()), nil)
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
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			t.Fatalf("the PUT of %s: HTTP %d", f.name, resp.StatusCode)
		}
		// Named at the upload, and here too: the name here is the one kept.
		named = append(named, map[string]any{"upload_token": up.UploadToken, "filename": f.name})
	}
	doc := result[struct {
		DocumentID string   `json:"document_id"`
		VersionID  string   `json:"version_id"`
		FileIDs    []string `json:"file_ids"`
	}](t, w.api, p.token, "POST", w.path("/documents"), map[string]any{"kind": "material", "title": title, "body_md": body, "files": named})
	if len(doc.FileIDs) != len(files) {
		t.Fatalf("Core made %d files of %d", len(doc.FileIDs), len(files))
	}
	return doc.DocumentID, doc.VersionID, doc.FileIDs
}

// hasFiles reports whether the Core under test holds several files to a
// version (AIShie-Core #49): its catalogue has document.file. A Core before
// it skips the scenarios of several files, but in CI.
func hasFiles(t *testing.T, w *world) bool {
	t.Helper()
	cat, err := core.FetchCatalogue(t.Context(), w.api.hc, w.api.base)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Tool(core.ToolDocumentFile); ok {
		return true
	}
	if ci, _ := strconv.ParseBool(os.Getenv("CI")); ci {
		t.Fatal("the Core under test has no document.file: it holds one file to a version, as before AIShie-Core #49")
	}
	t.Skip("the Core under test holds one file to a version (before AIShie-Core #49)")
	return false
}

// The lecture of three files Sato puts in the course: its reading, a PDF
// of two pages; its handout, a Word file; and a program, as text.
const lectureWeek7 = "Week 7 lecture"

var (
	week7Reading = doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"Reading 7: hashing"}}, doctexttest.PDFPage{Lines: []string{"Open addressing"}})
	week7Handout = doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "Lab 7", Heading: 1}, {Text: "Bring a laptop.", List: "bullet"}}})
	week7Program = []byte("for i in range(3):\n    print(i)\n")
	week7Files   = []attachedFile{{"reading.pdf", "application/pdf", week7Reading}, {"handout.docx", doctexttest.DOCXType, week7Handout},
		{"loops.txt", "text/plain", week7Program}}
)

// versionQuestion asks what the files of a document hold: "What is in
// the files of <title>?".
const versionQuestionPrefix = "What is in the files of "

// versionEntry is what a document_get result says of one of its version's
// files.
type versionEntry struct {
	FileID        string `json:"file_id"`
	Position      int    `json:"position"`
	Name          string `json:"name"`
	GivenAs       string `json:"given_as"`
	ConvertedTo   string `json:"converted_to"`
	ExtractedFrom string `json:"extracted_from"`
	TextSource    string `json:"text_source"`
	FileText      string `json:"file_text"`
}

// versionResponder is a model asked versionQuestion: it lists the course's
// documents, reads the one of the title asked for, and answers with what
// it was given of each of its version's files, in order: its name, how it
// was given (a file, or text), what LibreOffice made of it, what the text
// was read from, the pages of a PDF it had as a file, whose text it was,
// and the text's first line; and whether the result said the version holds
// three files. Any other question it answers as DefaultResponder does.
func versionResponder(req fakellm.ChatRequest) fakellm.ChatResponse {
	asked := ""
	for _, m := range req.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Text(), versionQuestionPrefix) {
			asked = strings.TrimSuffix(strings.TrimPrefix(m.Text(), versionQuestionPrefix), "?")
		}
	}
	if asked == "" {
		return fakellm.DefaultResponder(req)
	}
	var results []string
	var pages []int
	for _, m := range req.Messages {
		if m.Role == "tool" {
			results = append(results, m.Text())
		}
		parts, _ := m.Content.([]any)
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
	}
	switch len(results) {
	case 0:
		return fakellm.CallTools(fakellm.FunctionCall{Name: "document_list", Arguments: `{}`})
	case 1:
		var env struct {
			Result struct {
				Documents []struct {
					ID    string `json:"id"`
					Title string `json:"title"`
				} `json:"documents"`
			} `json:"result"`
		}
		_ = json.Unmarshal([]byte(results[0]), &env)
		for _, d := range env.Result.Documents {
			if d.Title == asked {
				return fakellm.CallTools(fakellm.FunctionCall{Name: "document_get", Arguments: fmt.Sprintf(`{"document_id":%q}`, d.ID)})
			}
		}
		return fakellm.Reply("There is no document titled " + asked + ".")
	}
	var got struct {
		FilesNote string         `json:"files_note"`
		Files     []versionEntry `json:"files"`
	}
	_ = json.Unmarshal([]byte(results[len(results)-1]), &got)
	var said []string
	if !strings.Contains(got.FilesNote, "the version holds 3 files") {
		said = append(said, "not told of the three files")
	}
	for _, e := range got.Files {
		s := fmt.Sprintf("%d %s given as %s", e.Position, e.Name, e.GivenAs)
		if e.ConvertedTo != "" {
			s += ", converted to " + e.ConvertedTo
		}
		if e.ExtractedFrom != "" {
			s += ", extracted from " + e.ExtractedFrom
		}
		if e.TextSource != "" {
			s += ", " + e.TextSource
		}
		if e.GivenAs == "file" && len(pages) > 0 {
			s += fmt.Sprintf(", %d pages", pages[0])
			pages = pages[1:]
		}
		if first, _, _ := strings.Cut(e.FileText, "\n"); first != "" {
			s += ": " + first
		}
		said = append(said, s)
	}
	return fakellm.Reply(strings.Join(said, " | "))
}

// filesOfAVersion: Sato puts up a lecture of three files, a PDF, a Word
// file and a program as text, with a line of text, in one version. Ken
// asks Sato's course tutor, whose model takes files, and Yuki her own
// agent, whose model takes none, what its files hold. Each reads the
// version through Core and is given every file, in order, each under its
// name: the tutor's model the PDF as a file of its two pages, the Word
// file as LibreOffice's PDF of it, the program as its text; Yuki's agent's
// model the text of each. No URL reaches either model.
func filesOfAVersion(t *testing.T, w *world) {
	if !hasFiles(t, w) {
		return
	}
	conv := realOffice(t)
	m := newModel(t, versionResponder)
	takesFiles := map[string]any{"model": map[string]any{"capabilities": map[string]any{"file_input": true}},
		"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 120}}}
	textOnly := map[string]any{"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 120}}}
	rt := w.startRuntime(t, m, runtimeConf{office: conv, agents: []agentConf{
		{id: "tutor", seat: w.tutor, over: takesFiles}, {id: "yuki-helper", seat: w.own, over: textOnly}}})
	rt.waitPolling("tutor")
	rt.waitPolling("yuki-helper")
	w.uploadFiles(t, w.sato, lectureWeek7, "Read the PDF first, then run the program.", week7Files...)

	kens, kenQ := w.ask(t, w.ken, w.tutor.member, versionQuestionPrefix+lectureWeek7+"?")
	yukis, yukiQ := w.ask(t, w.yuki, w.own.member, versionQuestionPrefix+lectureWeek7+"?")
	wantTutor := "1 reading.pdf given as file, 2 pages | 2 handout.docx given as file, converted to pdf, 1 pages | 3 loops.txt given as text: for i in range(3):"
	if a := w.waitAnswer(t, w.ken, kens, w.tutor.member); a.text() != wantTutor || a.replyTo() != kenQ {
		t.Errorf("the tutor's answer is %q in reply to %s; want %q in reply to %s", a.text(), a.replyTo(), wantTutor, kenQ)
	}
	wantHelper := "1 reading.pdf given as text, extracted from pdf: ## Page 1 | 2 handout.docx given as text, extracted from docx: # Lab 7 | " +
		"3 loops.txt given as text: for i in range(3):"
	if a := w.waitAnswer(t, w.yuki, yukis, w.own.member); a.text() != wantHelper || a.replyTo() != yukiQ {
		t.Errorf("Yuki's agent's answer is %q in reply to %s; want %q in reply to %s", a.text(), a.replyTo(), wantHelper, yukiQ)
	}
	for _, req := range m.Requests() {
		if strings.Contains(string(req.Raw), "/v1/blobs/") {
			t.Error("a download URL reached the model")
		}
	}
}
