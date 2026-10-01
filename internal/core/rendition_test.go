package core

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// renditionCore answers the renditions' routes as serviceCore does the
// transcription service's, the calls made through a RuntimeCaller with the
// agent runtime's credential, once each.
func renditionCore(t *testing.T, answers map[string]func(r *http.Request) (int, string)) (*RuntimeService, *[]serviceSeen) {
	t.Helper()
	svc, seen := serviceCore(t, answers)
	rest := svc.c.(*RESTCaller)
	c := RuntimeCaller(RuntimeOptions{BaseURL: rest.base, HTTPClient: rest.client, Catalogue: rest.cat, Once: true,
		Credential: func(context.Context) (string, error) { return serviceToken, nil }})
	return NewRuntimeService(c), seen
}

const renditionID = "3a03c65c-9f17-46dd-97dc-6212e7a18496"

// The renditions' five calls go to their routes with the runtime's
// credential: the claim and the renewal in the body, the file and the
// upload URL with the lease in the query, the completion with its key in
// the header and only what its status takes; each reads what Core answers,
// and a refusal is the reason Core gives.
func TestRenditionCalls(t *testing.T) {
	base := "/v1/services/agent_runtime/renditions/"
	claimed := `{"status":"executed","result":{"claimed":[{"rendition_id":"` + renditionID + `","lease_id":"` + leaseID + `",` +
		`"lease_expires_at":"2026-10-01T09:10:00Z","attempt":2,"backfill":true,"course_id":"01a0f6d3-732c-7631-a95d-d51c0cc4fe46",` +
		`"source":"attachment","attachment_id":"01a0f6d3-aae4-7592-b89e-d6eab8de2691","filename":"第四週 handout.docx",` +
		`"content_type":"application/octet-stream","byte_size":48213,"checksum":"sha256:9f2c","download_url":"http://files/x?sig=1",` +
		`"download_expires_at":"2026-10-01T09:15:00Z","max_bytes":104857600}]}}`
	s, seen := renditionCore(t, map[string]func(*http.Request) (int, string){
		"POST " + base + "claim": fixed(200, claimed),
		"GET " + base + renditionID + "/file": fixed(200, `{"status":"executed","result":{"rendition_id":"`+renditionID+`",`+
			`"filename":"a.docx","content_type":"application/octet-stream","byte_size":3,"download_url":"http://files/y",`+
			`"download_expires_at":"2026-10-01T09:15:00Z","lease_expires_at":"2026-10-01T09:20:00Z"}}`),
		"POST " + base + renditionID + "/renew": fixed(200, `{"status":"executed","result":{"lease_expires_at":"2026-10-01T09:20:00Z"}}`),
		"GET " + base + renditionID + "/upload-url": fixed(200, `{"status":"executed","result":{"upload_url":"http://files/put",`+
			`"headers":{"Content-Type":"application/pdf"},"upload_token":"eyJ.secret","expires_at":"2026-10-01T09:15:00Z","max_bytes":104857600}}`),
		"POST " + base + renditionID + "/complete": func(r *http.Request) (int, string) {
			if strings.HasSuffix(r.Header.Get("Idempotency-Key"), ":2") {
				return 422, `{"status":"failed","action_id":"a1","review_state":"none","error":{"code":"failed_precondition",` +
					`"message":"not a PDF","details":{"reason":"not_a_pdf"}}}`
			}
			return 200, `{"status":"executed","action_id":"a2","review_state":"none","result":{"rendition_id":"` + renditionID +
				`","state":"done","byte_size":532114,"checksum":"sha256:ab"}}`
		},
	})
	ctx := t.Context()
	got, err := s.ClaimRenditions(ctx, 20, 30*time.Second, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("ClaimRenditions = %+v, %v", got, err)
	}
	c := got[0]
	if c.RenditionID != renditionID || c.Attempt != 2 || !c.Backfill || c.Source != RenditionOfAttachment || c.AttachmentID == "" ||
		c.FileID != "" || c.Filename != "第四週 handout.docx" || c.ByteSize != 48213 || c.MaxBytes != 104857600 ||
		!c.LeaseExpiresAt.Equal(time.Date(2026, 10, 1, 9, 10, 0, 0, time.UTC)) {
		t.Errorf("the claim: %+v", c)
	}
	cl := ClaimOfRendition(c)
	if f, err := s.RenditionFile(ctx, cl); err != nil || f.DownloadURL != "http://files/y" {
		t.Errorf("RenditionFile = %+v, %v", f, err)
	}
	if until, err := s.RenewRendition(ctx, cl, 10*time.Minute); err != nil || until.IsZero() {
		t.Errorf("RenewRendition = %v, %v", until, err)
	}
	up, err := s.RenditionUploadURL(ctx, cl)
	if err != nil || up.UploadURL != "http://files/put" || up.Headers["Content-Type"] != "application/pdf" || up.UploadToken != "eyJ.secret" {
		t.Errorf("RenditionUploadURL = %+v, %v", up, err)
	}
	done, err := s.CompleteRendition(ctx, cl, RenditionKey(cl, 1), RenditionCompletion{Status: RenditionDone, UploadToken: "eyJ.secret", PageCount: 12,
		Reason: "ignored"})
	if err != nil || done.State != RenditionDone || done.ByteSize != 532114 {
		t.Errorf("CompleteRendition = %+v, %v", done, err)
	}
	if _, err := s.CompleteRendition(ctx, cl, RenditionKey(cl, 2), RenditionCompletion{Status: RenditionDone, UploadToken: "eyJ.secret",
		PageCount: 12}); !IsReason(err, ReasonNotAPDF) {
		t.Errorf("a PDF Core refuses: %v", err)
	}
	if _, err := s.CompleteRendition(ctx, cl, RenditionKey(cl, 3), RenditionCompletion{Status: RenditionSkipped, Reason: RenditionTooLarge,
		PageCount: 4, UploadToken: "x"}); err != nil {
		t.Fatal(err)
	}

	calls := *seen
	if len(calls) != 7 {
		t.Fatalf("%d calls", len(calls))
	}
	for _, c := range calls {
		if c.auth != "Bearer "+serviceToken {
			t.Errorf("%s %s: authorization %q", c.method, c.path, c.auth)
		}
	}
	if b := calls[0].body; b["max"] != float64(10) || b["lease_s"] != float64(60) || b["wait_s"] != nil || calls[0].key != "" {
		t.Errorf("the claim: %+v", calls[0])
	}
	for _, i := range []int{1, 3} {
		if calls[i].method != "GET" || calls[i].query != "lease_id="+leaseID || calls[i].body != nil {
			t.Errorf("call %d: %+v", i, calls[i])
		}
	}
	if b := calls[2].body; b["lease_id"] != leaseID || b["lease_s"] != float64(600) || calls[2].key != "" {
		t.Errorf("the renewal: %+v", calls[2])
	}
	if b := calls[4].body; calls[4].key != "rendition:"+renditionID+":"+leaseID+":1" || b["status"] != "done" ||
		b["upload_token"] != "eyJ.secret" || b["page_count"] != float64(12) || b["reason"] != nil || b["lease_id"] != leaseID || b["rendition_id"] != nil {
		t.Errorf("the completion done: %+v", calls[4])
	}
	if b := calls[6].body; b["status"] != "skipped" || b["reason"] != RenditionTooLarge || b["upload_token"] != nil || b["page_count"] != nil {
		t.Errorf("the completion skipped: %+v", calls[6])
	}
}

// TestHasRenditions: the snapshot offers the renditions' five tools, and
// a catalogue without one of them has none.
func TestHasRenditions(t *testing.T) {
	cat := testCatalogue(t)
	if !HasRenditions(cat) || HasRenditions(nil) {
		t.Fatal("the snapshot offers no renditions")
	}
	raw := strings.Replace(string(readCatalogue(t)), `"agent_runtime.rendition_renew"`, `"agent_runtime.rendition_renew_x"`, 1)
	older, err := ParseCatalogue([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if HasRenditions(older) {
		t.Error("a catalogue without rendition_renew offers renditions")
	}
}
