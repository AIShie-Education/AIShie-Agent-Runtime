package fakecore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// service is the runtime's client of the fake's transcription service,
// with the credential token.
func (w *fakeWorld) service(t *testing.T, token string) *core.Service {
	t.Helper()
	cat, err := core.FetchCatalogue(t.Context(), w.srv.Client(), w.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return core.NewService(core.NewRESTCaller(core.RESTOptions{BaseURL: w.srv.URL, Token: token, Catalogue: cat, HTTPClient: w.srv.Client()}))
}

// A file added is queued, and the service claims it (uploads first, those
// waiting longest first), fetches it, holds its claim and completes it:
// done, with the text, which document_get then gives beside the version,
// a text event says so to whoever reads the document, and document_text
// reads in parts.
func TestTextQueue(t *testing.T) {
	w := newFakeWorld(t, Options{})
	ctx := t.Context()
	first, err := w.fc.AddFile(w.co.ID, "Week 1", "application/pdf", []byte("%PDF-1.4 week 1"))
	w.ok(err)
	second, err := w.fc.AddFile(w.co.ID, "Week 2", "application/pdf", []byte("%PDF-1.4 week 2"))
	w.ok(err)
	a := mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", first))
	var got struct {
		Result struct {
			Version struct {
				Text *core.TextView `json:"text"`
			} `json:"version"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(a.Text), &got); err != nil || got.Result.Version.Text == nil || got.Result.Version.Text.Status != core.TextPending ||
		got.Result.Version.Text.Revision != 1 || got.Result.Version.Text.Body != nil {
		t.Fatalf("a text waiting: %s", a.Text)
	}
	if syl := mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", w.co.SyllabusID)); strings.Contains(syl.Text, `"text"`) {
		t.Errorf("a version of text alone has a text version: %s", syl.Text)
	}

	tok := w.fc.IssueServiceToken("runtime")
	s := w.service(t, tok.Token)
	claimed, err := s.Queue(ctx, 10, 5*time.Minute, 0)
	if err != nil || len(claimed) != 2 || claimed[0].DocumentID != first || claimed[1].DocumentID != second {
		t.Fatalf("Queue = %+v, %v", claimed, err)
	}
	c := claimed[0]
	if c.Attempt != 1 || c.Backfill || c.ContentType != "application/pdf" || c.CourseID != w.co.ID || c.ByteSize != 15 ||
		c.LeaseExpiresAt.Before(time.Now().Add(4*time.Minute)) {
		t.Errorf("the claim: %+v", c)
	}
	resp, err := w.srv.Client().Get(c.DownloadURL) //nolint:noctx // the fake's own URL.
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "%PDF-1.4 week 1" {
		t.Errorf("the file: %q", body)
	}
	if again, err := s.Queue(ctx, 10, 0, 0); err != nil || len(again) != 0 {
		t.Errorf("claimed twice: %+v, %v", again, err)
	}
	if rec, _ := w.fc.Text(first); rec.Status != core.TextWorking || !rec.Claimed || rec.Attempts != 1 {
		t.Errorf("while it works: %+v", rec)
	}
	if f, err := s.File(ctx, c.VersionID, c.LeaseID); err != nil || f.DownloadURL == "" || f.ContentType != "application/pdf" {
		t.Errorf("File = %+v, %v", f, err)
	}
	until, err := s.Renew(ctx, c.VersionID, c.LeaseID, time.Hour)
	if err != nil || until.Before(time.Now().Add(59*time.Minute)) {
		t.Errorf("Renew = %s, %v", until, err)
	}

	long := "## 第 1 頁\n\n" + strings.Repeat("一二三四五六七八九十\n", 3000) + "## 第 2 頁\n\n" + strings.Repeat("abcdefghij\n", 3000)
	rev, err := s.Complete(ctx, c.VersionID, c.LeaseID, core.Completion{Status: core.TextDone, Body: long, Pages: 2, Model: "Gemini Flash-Lite"})
	if err != nil || rev != 2 {
		t.Fatalf("Complete = %d, %v", rev, err)
	}
	// Sent again under its key, it is Core's replay.
	if rev, err := s.Complete(ctx, c.VersionID, c.LeaseID, core.Completion{Status: core.TextDone, Body: long, Pages: 2,
		Model: "Gemini Flash-Lite"}); err != nil || rev != 2 {
		t.Errorf("the completion again: %d, %v", rev, err)
	}
	if rec, _ := w.fc.Text(first); rec.Status != core.TextDone || rec.Source != core.SourceAI || rec.Body != long || rec.Pages != 2 ||
		rec.Model != "Gemini Flash-Lite" || rec.Claimed {
		t.Errorf("done: %+v", rec)
	}
	if _, err := s.Renew(ctx, c.VersionID, c.LeaseID, 0); !core.IsReason(err, core.ReasonLeaseLost) {
		t.Errorf("a renewal after the completion: %v", err)
	}

	// Longer than one part: document_get says so, and document_text reads
	// it in parts, each cut after a whole line (or before a page's
	// heading, where that leaves it half full).
	g := mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", first))
	if err := json.Unmarshal([]byte(g.Text), &got); err != nil || got.Result.Version.Text.Status != core.TextDone ||
		got.Result.Version.Text.Body != nil || got.Result.Version.Text.Bytes != len(long) || got.Result.Version.Text.Source != core.SourceAI {
		t.Errorf("the text done, as document_get gives it: %s", g.Text[:min(len(g.Text), 600)])
	}
	var joined strings.Builder
	for part := 1; ; part++ {
		p := mustCall(t, w.agentC, "document_text", inCourseArgs(w, "document_id", first, "part", part))
		var tp core.TextPart
		if err := json.Unmarshal([]byte(p.Text), &struct {
			Result *core.TextPart `json:"result"`
		}{&tp}); err != nil || tp.Text.Body == nil || tp.Part != part {
			t.Fatalf("part %d: %s", part, p.Text[:min(len(p.Text), 300)])
		}
		joined.WriteString(*tp.Text.Body)
		if part == tp.Parts {
			break
		}
		if !strings.HasSuffix(*tp.Text.Body, "\n") || len(*tp.Text.Body) > 65536 {
			t.Errorf("part %d is cut within a line, or is longer than a part", part)
		}
	}
	if joined.String() != long {
		t.Error("the parts are not the text")
	}

	// The course's feed says the text is done, with no text in it.
	evs := list(mustCall(t, w.as("sato"), "event_list", inCourseArgs(w, "since_seq", 0)), "events")
	found := false
	for _, e := range evs {
		ev := e.(map[string]any)
		if ev["type"] == "document.text_updated" {
			p := ev["payload"].(map[string]any)
			found = p["version_id"] == c.VersionID && p["status"] == "done" && p["source"] == "ai" && ev["subject_id"] == first
		}
	}
	if !found {
		t.Errorf("no text event: %v", evs)
	}
}

// A claim lost (lapsed, and claimed again) and a text staff wrote stop the
// work; a claim lapsed five times fails the version; a revoked credential
// is refused, and its claims go back to the queue.
func TestTextQueueRefusals(t *testing.T) {
	w := newFakeWorld(t, Options{})
	ctx := t.Context()
	doc, err := w.fc.AddFile(w.co.ID, "Week 1", "application/pdf", []byte("%PDF-1.4"))
	w.ok(err)
	tok := w.fc.IssueServiceToken("runtime")
	s := w.service(t, tok.Token)
	claim := func() core.ClaimedText {
		t.Helper()
		got, err := s.Queue(ctx, 1, 0, 0)
		if err != nil || len(got) != 1 {
			t.Fatalf("Queue = %+v, %v", got, err)
		}
		return got[0]
	}
	c1 := claim()
	w.ok(w.fc.LapseTextClaim(doc))
	c2 := claim()
	if c2.Attempt != 2 || c2.LeaseID == c1.LeaseID {
		t.Errorf("claimed again: %+v", c2)
	}
	for name, err := range map[string]error{
		"renew": func() error { _, err := s.Renew(ctx, c1.VersionID, c1.LeaseID, 0); return err }(),
		"file":  func() error { _, err := s.File(ctx, c1.VersionID, c1.LeaseID); return err }(),
		"complete": func() error {
			_, err := s.Complete(ctx, c1.VersionID, c1.LeaseID, core.Completion{Status: core.TextSkipped, Reason: "too_many_pages"})
			return err
		}(),
	} {
		if !core.IsReason(err, core.ReasonLeaseLost) {
			t.Errorf("%s with the lease lost: %v", name, err)
		}
	}

	w.ok(w.fc.EditText(doc, w.sato.ID, "## 第 1 頁\n\nStaff's"))
	if _, err := s.Renew(ctx, c2.VersionID, c2.LeaseID, 0); !core.IsReason(err, core.ReasonEditedByStaff) {
		t.Errorf("a renewal of a text staff wrote: %v", err)
	}
	if _, err := s.Complete(ctx, c2.VersionID, c2.LeaseID, core.Completion{Status: core.TextDone, Body: "x", Pages: 1, Model: "m"}); !core.IsReason(err,
		core.ReasonEditedByStaff) {
		t.Errorf("a completion of a text staff wrote: %v", err)
	}
	if rec, _ := w.fc.Text(doc); rec.Source != core.SourceStaff || rec.Body != "## 第 1 頁\n\nStaff's" {
		t.Errorf("the staff's text written over: %+v", rec)
	}

	// Five claims lapsed, and the version fails.
	w.ok(w.fc.Retranscribe(doc))
	for range 5 {
		claim()
		w.ok(w.fc.LapseTextClaim(doc))
	}
	if got, err := s.Queue(ctx, 1, 0, 0); err != nil || len(got) != 0 {
		t.Errorf("claimed a sixth time: %+v, %v", got, err)
	}
	if rec, _ := w.fc.Text(doc); rec.Status != core.TextFailed || rec.Reason != core.ReasonAttemptsExhausted {
		t.Errorf("after five claims lapsed: %+v", rec)
	}

	// The credential revoked: 401, and its claim back in the queue.
	w.ok(w.fc.Retranscribe(doc))
	claim()
	w.ok(w.fc.RevokeServiceToken(tok.CredentialID))
	if _, err := s.Queue(ctx, 1, 0, 0); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("a revoked credential: %v", err)
	}
	next := w.fc.IssueServiceToken("runtime 2")
	if got, err := w.service(t, next.Token).Queue(ctx, 1, 0, 0); err != nil || len(got) != 1 || got[0].DocumentID != doc {
		t.Errorf("the claim released: %+v, %v", got, err)
	}

	// The service's tools are the service's alone, and it calls nothing
	// else; over MCP, it is not let in at all.
	agent := core.NewService(core.NewRESTCaller(core.RESTOptions{BaseURL: w.srv.URL, Token: w.tutorA.Token, Catalogue: mustCatalogue(t, w),
		HTTPClient: w.srv.Client()}))
	if _, err := agent.Queue(ctx, 1, 0, 0); !core.IsReason(err, core.ReasonServiceOnly) {
		t.Errorf("an agent's token at the queue: %v", err)
	}
	me := &restClient{base: w.srv.URL, token: next.Token, hc: w.srv.Client()}
	if a, err := me.do(ctx, http.MethodGet, "/v1/me", nil, ""); err != nil || a.Status != http.StatusForbidden ||
		!strings.Contains(string(a.Body), "not_for_services") {
		t.Errorf("the service's me_get: %v %d %s", err, a.Status, a.Body)
	}
	if a, err := newMCPClient(w.srv.URL, next.Token, w.srv.Client()).initialize(ctx); err != nil || a.Status != http.StatusUnauthorized {
		t.Errorf("the service over MCP: %v %d", err, a.Status)
	}
}

// A call of the queue that finds nothing waits (wait_s) for a version to
// be queued, and claims it as soon as it is.
func TestTextQueueWaits(t *testing.T) {
	w := newFakeWorld(t, Options{})
	s := w.service(t, w.fc.IssueServiceToken("runtime").Token)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.fc.AddFile(w.co.ID, "Late", "application/pdf", []byte("%PDF-1.4"))
	}()
	start := time.Now()
	got, err := s.Queue(ctx, 1, 0, 5*time.Second)
	if err != nil || len(got) != 1 || time.Since(start) > 3*time.Second {
		t.Fatalf("Queue waiting = %+v, %v after %s", got, err, time.Since(start))
	}
	start = time.Now()
	if got, err := s.Queue(ctx, 1, 0, time.Second); err != nil || len(got) != 0 || time.Since(start) < 900*time.Millisecond {
		t.Errorf("Queue with nothing to wait for = %+v, %v after %s", got, err, time.Since(start))
	}
}

func mustCatalogue(t *testing.T, w *fakeWorld) *core.Catalogue {
	t.Helper()
	cat, err := core.FetchCatalogue(t.Context(), w.srv.Client(), w.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}
