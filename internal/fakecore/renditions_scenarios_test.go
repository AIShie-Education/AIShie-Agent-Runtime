package fakecore

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
)

// renditions is the site's agent runtime converting Office files to PDF
// (AIShie-Core's migration 0026): a material of a Word file and a text,
// and a message carrying a deck declared of no particular type and a text;
// the Office files queued, the others not; the service's claim, its file,
// its renewal, its upload URL and the PUT, and its completion, done and
// skipped, with every refusal of each (arguments out of range, another's
// lease, no such rendition, the upload's token, nothing uploaded, not a
// PDF, a PUT of another type or made twice, after the end); and what the
// file's readers are shown of the rendition as it goes (document_get,
// document_versions, document_file, conversation_attachment,
// conversation_messages), the PDF itself, and an agent at the service's
// route.
var renditions = scenario{name: "renditions", about: "Office files converted to PDF by the site's agent runtime: a version's file and a " +
	"message's queued, claimed, renewed, uploaded and completed done and skipped, every refusal of the service's calls, and what " +
	"the file's readers are shown of the rendition as it goes, and the PDF",
	run: func(t *testing.T, w world, s *steps) {
		ctx := context.Background()
		svc := w.runtimeService()
		const base = "/v1/services/agent_runtime/renditions"
		do := func(name string, r *restClient, method, path string, body any, key string) httpAnswer {
			t.Helper()
			a, err := r.do(ctx, method, path, body, key)
			if err != nil {
				t.Fatal(err)
			}
			s.rest(name, method, path, body, a)
			scrubURLs(s)
			return a
		}
		sato, yuki := w.as("sato"), w.as("yuki")
		// What other worlds of this Core queued is given back first, unrecorded.
		drainRenditions(t, svc)

		handout := []byte("PK\x03\x04 the week's handout, as Word keeps it")
		docID, fileIDs := w.officeMaterial("Week 4", namedFile{"第四週 handout.docx",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document", handout},
			namedFile{"notes.txt", "text/plain", []byte("Read chapter 4.\n")})
		readAs := func(c *mcpClient, name, tool string, args map[string]any) toolAnswer {
			t.Helper()
			a := callAs(t, c, s, name, tool, args)
			scrubURLs(s)
			if tool == "document_get" {
				// Where the material sorts among the course's is how each
				// world was made, not what Core does.
				dropResultField(s, "sort_order")
			}
			return a
		}
		doc := inCourseArgs(w, "document_id", docID)
		readAs(sato, "get_queued", "document_get", doc)
		readAs(sato, "versions_queued", "document_versions", doc)

		// The claim's refusals, and an agent at the service's route.
		do("claim_bad_max", svc, "POST", base+"/claim", map[string]any{"max": 11}, "")
		do("claim_bad_lease", svc, "POST", base+"/claim", map[string]any{"lease_s": 59}, "")
		do("claim_bad_wait", svc, "POST", base+"/claim", map[string]any{"wait_s": 26}, "")
		do("claim_as_an_agent", w.rest(), "POST", base+"/claim", map[string]any{}, "")

		claimed := do("claim", svc, "POST", base+"/claim", map[string]any{"max": 1, "lease_s": 600}, "")
		r := claimedOne(t, claimed)
		readAs(sato, "file_claimed", "document_file", inCourseArgs(w, "document_id", docID, "file_id", fileIDs[0]))
		lease := url.Values{"lease_id": {r.LeaseID}}.Encode()
		other := url.Values{"lease_id": {uuid.NewString()}}.Encode()
		nobody := uuid.NewString()
		do("file", svc, "GET", base+"/"+r.RenditionID+"/file?"+lease, nil, "")
		do("file_another_lease", svc, "GET", base+"/"+r.RenditionID+"/file?"+other, nil, "")
		do("file_no_rendition", svc, "GET", base+"/"+nobody+"/file?"+lease, nil, "")
		do("renew", svc, "POST", base+"/"+r.RenditionID+"/renew", map[string]any{"lease_id": r.LeaseID, "lease_s": 120}, "")
		do("renew_another_lease", svc, "POST", base+"/"+r.RenditionID+"/renew", map[string]any{"lease_id": uuid.NewString()}, "")
		do("renew_bad_lease", svc, "POST", base+"/"+r.RenditionID+"/renew", map[string]any{"lease_id": r.LeaseID, "lease_s": 30}, "")

		// The completion's refusals of its shape, and of an upload that is none.
		complete := base + "/" + r.RenditionID + "/complete"
		key := func(n string) string { return "rendition:" + r.RenditionID + ":" + n }
		do("complete_done_without_upload", svc, "POST", complete, map[string]any{"lease_id": r.LeaseID, "status": "done", "page_count": 1}, key("a"))
		do("complete_failed_without_reason", svc, "POST", complete, map[string]any{"lease_id": r.LeaseID, "status": "failed"}, key("b"))
		do("complete_skipped_unknown_reason", svc, "POST", complete, map[string]any{"lease_id": r.LeaseID, "status": "skipped",
			"reason": "it is too pretty"}, key("c"))
		do("complete_done_with_reason", svc, "POST", complete, map[string]any{"lease_id": r.LeaseID, "status": "done",
			"upload_token": "x", "page_count": 1, "reason": "timeout"}, key("d"))
		do("complete_bad_upload_token", svc, "POST", complete, map[string]any{"lease_id": r.LeaseID, "status": "done",
			"upload_token": "not-a-token", "page_count": 1}, key("e"))
		do("complete_no_rendition", svc, "POST", base+"/"+nobody+"/complete", map[string]any{"lease_id": r.LeaseID, "status": "failed",
			"reason": "timeout"}, key("f"))

		// An upload URL, and what its PUT and the completion take.
		do("upload_url_another_lease", svc, "GET", base+"/"+r.RenditionID+"/upload-url?"+other, nil, "")
		first := uploadOf(t, do("upload_url", svc, "GET", base+"/"+r.RenditionID+"/upload-url?"+lease, nil, ""))
		done := func(name, token string, pages int, k string) httpAnswer {
			t.Helper()
			return do(name, svc, "POST", complete, map[string]any{"lease_id": r.LeaseID, "status": "done", "upload_token": token,
				"page_count": pages}, k)
		}
		done("complete_not_uploaded", first.token, 1, key("g"))
		putStep(t, w, s, "put_another_type", first, "text/html", []byte("<p>not a pdf</p>"))
		putStep(t, w, s, "put_not_a_pdf", first, "application/pdf", []byte("this is not a PDF"))
		putStep(t, w, s, "put_twice", first, "application/pdf", []byte("%PDF-1.4\n"))
		done("complete_not_a_pdf", first.token, 1, key("h"))
		done("complete_deleted_upload", first.token, 1, key("i"))
		second := uploadOf(t, do("upload_url_again", svc, "GET", base+"/"+r.RenditionID+"/upload-url?"+lease, nil, ""))
		pdf := []byte("%PDF-1.4\n% the handout's PDF\n%%EOF\n")
		putStep(t, w, s, "put_pdf", second, "application/pdf", pdf)
		done("complete_too_many_pages", second.token, 100001, key("j"))
		done("complete_done", second.token, 3, key("k"))
		done("complete_done_replayed", second.token, 3, key("k"))
		do("complete_after_done", svc, "POST", complete, map[string]any{"lease_id": r.LeaseID, "status": "failed", "reason": "timeout"}, key("l"))
		do("renew_after_done", svc, "POST", base+"/"+r.RenditionID+"/renew", map[string]any{"lease_id": r.LeaseID}, "")
		do("file_after_done", svc, "GET", base+"/"+r.RenditionID+"/file?"+lease, nil, "")

		// What the file's readers are shown of it now, and the PDF.
		got := readAs(sato, "get_done", "document_get", doc)
		readAs(sato, "versions_done", "document_versions", doc)
		readAs(w.agent(), "file_done_as_the_tutor", "document_file", inCourseArgs(w, "document_id", docID, "file_id", fileIDs[0]))
		downloadStep(t, w, s, "download_the_pdf", renditionURLOf(t, got))

		// A message's files: a deck declared of no particular type, and a text.
		up := uploadStep(t, w, s, yuki, "deck", "application/octet-stream", []byte("PK\x03\x04 Yuki's slides"))
		upNotes := uploadStep(t, w, s, yuki, "notes", "text/plain", []byte("My notes.\n"))
		opened := callAs(t, yuki, s, "open_with_a_deck", "conversation_open", inCourseArgs(w, "respondent_member_id", w.tutorSeat(),
			"body", "Can you look at my slides?", "attachments", []map[string]any{
				{"upload_token": up.token, "filename": "第四週 slides.pptx"}, {"upload_token": upNotes.token, "filename": "notes.txt"}},
			"idempotency_key", "renditions:open:"+w.course()))
		wantStatus(t, opened, actExecuted)
		conv := opened.str("result", "conversation_id")
		msgs := readAs(w.agent(), "messages_queued", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		files := attachmentIDs(msgs)
		if len(files) != 2 {
			t.Fatalf("the message carries %d files: %s", len(files), msgs.Body)
		}
		readAs(w.agent(), "attachment_queued", "conversation_attachment", inCourseArgs(w, "attachment_id", files[0]))
		readAs(w.agent(), "attachment_not_converted", "conversation_attachment", inCourseArgs(w, "attachment_id", files[1]))
		deck := claimedOne(t, do("claim_the_deck", svc, "POST", base+"/claim", map[string]any{"max": 10}, ""))
		readAs(yuki, "attachment_claimed", "conversation_attachment", inCourseArgs(w, "attachment_id", files[0]))
		do("complete_skipped", svc, "POST", base+"/"+deck.RenditionID+"/complete", map[string]any{"lease_id": deck.LeaseID,
			"status": "skipped", "reason": "password_protected"}, "rendition:"+deck.RenditionID+":1")
		readAs(w.agent(), "attachment_skipped", "conversation_attachment", inCourseArgs(w, "attachment_id", files[0]))
		readAs(w.agent(), "messages_skipped", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		do("claim_nothing", svc, "POST", base+"/claim", map[string]any{"max": 10}, "")
	}}

// namedFile is a file a world puts in a version, under its name.
type namedFile struct {
	name, contentType string
	data              []byte
}

// claimedRendition is what a scenario needs of a claim.
type claimedRendition struct {
	RenditionID string `json:"rendition_id"`
	LeaseID     string `json:"lease_id"`
}

// claimedOne is the one rendition a claim's answer holds.
func claimedOne(t *testing.T, a httpAnswer) claimedRendition {
	t.Helper()
	var env struct {
		Result struct {
			Claimed []claimedRendition `json:"claimed"`
		} `json:"result"`
	}
	if err := json.Unmarshal(a.Body, &env); err != nil || len(env.Result.Claimed) != 1 {
		t.Fatalf("the claim: %d %s", a.Status, a.Body)
	}
	return env.Result.Claimed[0]
}

// uploadOf is the upload an upload URL's answer gives.
func uploadOf(t *testing.T, a httpAnswer) uploaded {
	t.Helper()
	var env struct {
		Result struct {
			UploadURL   string `json:"upload_url"`
			UploadToken string `json:"upload_token"`
		} `json:"result"`
	}
	if err := json.Unmarshal(a.Body, &env); err != nil || env.Result.UploadURL == "" || env.Result.UploadToken == "" {
		t.Fatalf("the upload URL: %d %s", a.Status, a.Body)
	}
	return uploaded{token: env.Result.UploadToken, url: env.Result.UploadURL}
}

// renditionURLOf is the URL of the PDF of the first file of a document_get.
func renditionURLOf(t *testing.T, a toolAnswer) string {
	t.Helper()
	var env struct {
		Result struct {
			Version struct {
				Files []struct {
					Rendition *struct {
						DownloadURL string `json:"download_url"`
					} `json:"rendition"`
				} `json:"files"`
			} `json:"version"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(a.Text), &env); err != nil || len(env.Result.Version.Files) == 0 ||
		env.Result.Version.Files[0].Rendition == nil || env.Result.Version.Files[0].Rendition.DownloadURL == "" {
		t.Fatalf("document_get gives no URL of the PDF: %s", a.Text)
	}
	return env.Result.Version.Files[0].Rendition.DownloadURL
}

// drainRenditions gives back, skipped, every rendition waiting in the
// Core the scenario runs against, unrecorded: a real Core's queue is the
// site's, and other worlds of it may have queued some.
func drainRenditions(t *testing.T, svc *restClient) {
	t.Helper()
	ctx := context.Background()
	for range 100 {
		a, err := svc.do(ctx, "POST", "/v1/services/agent_runtime/renditions/claim", map[string]any{"max": 10}, "")
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Result struct {
				Claimed []claimedRendition `json:"claimed"`
			} `json:"result"`
		}
		if err := json.Unmarshal(a.Body, &env); err != nil || a.Status != http.StatusOK {
			t.Fatalf("draining the queue: %d %s", a.Status, a.Body)
		}
		if len(env.Result.Claimed) == 0 {
			return
		}
		for _, r := range env.Result.Claimed {
			if _, err := svc.do(ctx, "POST", "/v1/services/agent_runtime/renditions/"+r.RenditionID+"/complete",
				map[string]any{"lease_id": r.LeaseID, "status": "skipped", "reason": "unsupported"}, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("the queue does not drain")
}

// dropResultField leaves field out of the last step's result.
func dropResultField(s *steps, field string) {
	step := s.list[len(s.list)-1]
	env, _ := step["envelope"].(map[string]any)
	res, _ := env["result"].(map[string]any)
	if res == nil {
		return
	}
	res = maps.Clone(res)
	delete(res, field)
	env = maps.Clone(env)
	env["result"] = res
	step["envelope"] = env
}

// scrubURLs replaces every download_url and upload_url the last step
// holds, at any depth, by a placeholder: they name the server, and are
// credentials besides.
func scrubURLs(s *steps) {
	step := s.list[len(s.list)-1]
	for _, k := range []string{"envelope", "body"} {
		if v, ok := step[k]; ok {
			step[k] = scrubbed(v)
		}
	}
}

func scrubbed(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			switch s, isString := e.(string); {
			case isString && s != "" && (k == "download_url" || k == "upload_url"):
				out[k] = "<" + k + ">"
			default:
				out[k] = scrubbed(e)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = scrubbed(e)
		}
		return out
	}
	return v
}
