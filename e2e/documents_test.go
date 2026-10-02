package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// weekThreeSlides is a deck of slides as a course's lecture slides are:
// titles, bullets, Chinese, and speaker notes.
var weekThreeSlides = doctexttest.PPTX(
	doctexttest.Slide{Title: "Week 3: Sorting", Body: []doctexttest.Bullet{{Text: "Merge sort splits the list in two"}}, Images: 1},
	doctexttest.Slide{Title: "排序的複雜度", Body: []doctexttest.Bullet{{Text: "合併排序：O(n log n)"}, {Text: "最壞情況也是 O(n log n)", Level: 1}},
		Notes: "Ask who has seen quicksort."},
)

// slideQuestion asks about a slide of a document by its title: the
// documents responder reads the document to answer it.
const slideQuestion = "What does slide 2 of Week 3 slides say?"

// documentsResponder is a model that reads a document to answer a question
// about one of its slides (slideQuestion): it lists the course's
// documents, reads the one of the title asked for, and answers with that
// slide's text as the runtime gave it; any other question it answers as
// DefaultResponder does.
func documentsResponder(req fakellm.ChatRequest) fakellm.ChatResponse {
	q := question(req)
	rest, ok := strings.CutPrefix(q, "What does slide ")
	if !ok {
		return fakellm.DefaultResponder(req)
	}
	n, title, _ := strings.Cut(strings.TrimSuffix(rest, " say?"), " of ")
	var results []string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			results = append(results, m.Text())
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
			if d.Title == title {
				return fakellm.CallTools(fakellm.FunctionCall{Name: "document_get", Arguments: fmt.Sprintf(`{"document_id":%q}`, d.ID)})
			}
		}
		return fakellm.Reply("There is no document titled " + title + ".")
	}
	var got struct {
		File struct {
			ExtractedFrom string `json:"extracted_from"`
		} `json:"file"`
		FileText string `json:"file_text"`
	}
	_ = json.Unmarshal([]byte(results[len(results)-1]), &got)
	for _, part := range strings.Split(got.FileText, "\n\n") {
		if strings.HasPrefix(part, "## Slide "+n+":") {
			return fakellm.Reply("From the " + got.File.ExtractedFrom + ": " + part)
		}
	}
	return fakellm.Reply("I could not read slide " + n + ".")
}

// uploadExt is the extension of the file upload names after its title,
// as Core named a file attached with no name of its own.
var uploadExt = map[string]string{"application/pdf": ".pdf", doctexttest.PPTXType: ".pptx"}

// upload is p putting a file in the course as Core's front end does: an
// upload URL for it, the bytes PUT there, a material document made of the
// upload, its one file named after the title, and its version published. It
// returns the document's id.
func (w *world) upload(t *testing.T, p person, title, contentType string, data []byte) string {
	t.Helper()
	ext, ok := uploadExt[contentType]
	if !ok {
		t.Fatalf("no extension for %s", contentType)
	}
	q := url.Values{"kind": {"material"}, "content_type": {contentType}}
	up := result[struct {
		UploadURL   string            `json:"upload_url"`
		Headers     map[string]string `json:"headers"`
		UploadToken string            `json:"upload_token"`
	}](t, w.api, p.token, "GET", w.path("/upload-url?"+q.Encode()), nil)
	ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, up.UploadURL, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	resp, err := w.api.hc.Do(req)
	if err != nil {
		t.Fatalf("the upload's PUT: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Fatalf("the upload's PUT: HTTP %d", resp.StatusCode)
	}
	doc := result[struct {
		DocumentID string `json:"document_id"`
		VersionID  string `json:"version_id"`
	}](t, w.api, p.token, "POST", w.path("/documents"), map[string]any{"kind": "material", "title": title,
		"files": []map[string]any{{"upload_token": up.UploadToken, "filename": title + ext}}})
	w.api.call(t, http.StatusOK, p.token, "POST", w.path("/documents/"+doc.DocumentID+"/publish"), map[string]any{"version_id": doc.VersionID})
	return doc.DocumentID
}

// realOffice is the worker's conversion of Office files on the LibreOffice
// and poppler installed here, or the test is skipped, as the office
// package's own tests are: never with OFFICE_PDF_REQUIRED=1.
func realOffice(t *testing.T) *office.Service {
	t.Helper()
	cfg := office.Config{TempDir: t.TempDir()}
	c, err := office.NewConverter(t.Context(), cfg)
	var p *office.Pager
	if err == nil {
		p, err = office.NewPager(cfg)
	}
	if errors.Is(err, office.ErrUnavailable) {
		if os.Getenv("OFFICE_PDF_REQUIRED") == "1" {
			t.Fatalf("OFFICE_PDF_REQUIRED is set, and the conversion is not available: %v", err)
		}
		t.Skipf("LibreOffice or poppler is not installed: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := office.NewService(ctx, office.ServiceOptions{Converter: c, Pager: p, Config: cfg})
	t.Cleanup(func() { cancel(); s.Wait() })
	return s
}

// lectureQuestion asks what a lecture's slides show; lectureTitle is the
// lecture's.
const (
	lectureTitle    = "Week 3 lecture"
	lectureQuestion = "What do the slides of " + lectureTitle + " show?"
)

// lectureSlides is a lecture of twelve slides, notes on the first.
func lectureSlides() []byte {
	slides := []doctexttest.Slide{{Title: "第三週：合併排序", Body: []doctexttest.Bullet{{Text: "把串列分成兩半"}}, Notes: "Ask who has seen quicksort."}}
	for i := 2; i <= 12; i++ {
		slides = append(slides, doctexttest.Slide{Title: fmt.Sprintf("Step %d", i), Body: []doctexttest.Bullet{{Text: "排序的複雜度"}}})
	}
	return doctexttest.PPTX(slides...)
}

// pdfResponder is a model that takes files and reads the lecture to say
// what its slides show: it lists the course's documents, reads the
// lecture's, and answers with what it was given of it: the file's record,
// the pages of the PDF it had as a file, and the text beside it.
func pdfResponder(req fakellm.ChatRequest) fakellm.ChatResponse {
	// The file comes in a message of the user's own after the results,
	// so the question is any message of the user's, not the last.
	asked := false
	for _, m := range req.Messages {
		asked = asked || m.Role == "user" && m.Text() == lectureQuestion
	}
	if !asked {
		return fakellm.DefaultResponder(req)
	}
	var results []string
	pdfPages := 0
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
					pdfPages, _ = doctext.PDFPages(context.Background(), pdf, doctext.Limits{})
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
			if d.Title == lectureTitle {
				return fakellm.CallTools(fakellm.FunctionCall{Name: "document_get", Arguments: fmt.Sprintf(`{"document_id":%q}`, d.ID)})
			}
		}
		return fakellm.Reply("There is no " + lectureTitle + ".")
	}
	var got struct {
		File struct {
			GivenAs     string `json:"given_as"`
			ConvertedTo string `json:"converted_to"`
			Part        int    `json:"part"`
			Parts       int    `json:"parts"`
			PartHolds   string `json:"part_holds"`
		} `json:"file"`
		FileText string `json:"file_text"`
	}
	_ = json.Unmarshal([]byte(results[len(results)-1]), &got)
	f := got.File
	return fakellm.Reply(fmt.Sprintf("Given as a %s of its %s, part %d of %d, %s, %d pages; beside it: %s",
		f.GivenAs, f.ConvertedTo, f.Part, f.Parts, f.PartHolds, pdfPages, got.FileText))
}

// slidesAsTheirPDF: Sato uploads a lecture of twelve slides as a .pptx, and
// Yuki's own agent, whose model takes files, reads it through Core for her:
// the runtime converts it with LibreOffice and gives the model its first
// ten slides as a PDF, with their speaker notes beside it, and says there
// is a second part.
func slidesAsTheirPDF(t *testing.T, w *world) {
	conv := realOffice(t)
	m := newModel(t, pdfResponder)
	takesFiles := map[string]any{"model": map[string]any{"capabilities": map[string]any{"file_input": true}}}
	rt := w.startRuntime(t, m, runtimeConf{office: conv, agents: []agentConf{{id: "yuki-helper", seat: w.own, over: takesFiles}}})
	rt.waitPolling("yuki-helper")
	w.upload(t, w.sato, lectureTitle, doctexttest.PPTXType, lectureSlides())
	asked, msg := w.ask(t, w.yuki, w.own.member, lectureQuestion)
	answer := w.waitAnswer(t, w.yuki, asked, w.own.member)
	want := "Given as a file of its pdf, part 1 of 2, slides 1–10, 10 pages; beside it: ## Slide 1\nNotes: Ask who has seen quicksort."
	if answer.text() != want || answer.replyTo() != msg {
		t.Errorf("the answer is %q in reply to %s; want %q in reply to %s", answer.text(), answer.replyTo(), want, msg)
	}
}
