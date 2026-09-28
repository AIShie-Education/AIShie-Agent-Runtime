package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/fakellm"
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

// upload is p putting a file in the course as Core's front end does: an
// upload URL for it, the bytes PUT there, a material document made of the
// upload, and its version published. It returns the document's id.
func (w *world) upload(t *testing.T, p person, title, contentType string, data []byte) string {
	t.Helper()
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
	}](t, w.api, p.token, "POST", w.path("/documents"), map[string]any{"kind": "material", "title": title, "upload_token": up.UploadToken})
	w.api.call(t, http.StatusOK, p.token, "POST", w.path("/documents/"+doc.DocumentID+"/publish"), map[string]any{"version_id": doc.VersionID})
	return doc.DocumentID
}
