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
				Files []struct {
					Text *core.TextView `json:"text"`
				} `json:"files"`
			} `json:"version"`
		} `json:"result"`
	}
	text := func() *core.TextView {
		if len(got.Result.Version.Files) != 1 || got.Result.Version.Files[0].Text == nil {
			return &core.TextView{}
		}
		return got.Result.Version.Files[0].Text
	}
	if err := json.Unmarshal([]byte(a.Text), &got); err != nil || text().Status != core.TextPending || text().Revision != 1 || text().Body != nil {
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
	if f, err := s.File(ctx, core.ClaimOf(c)); err != nil || f.DownloadURL == "" || f.ContentType != "application/pdf" {
		t.Errorf("File = %+v, %v", f, err)
	}
	until, err := s.Renew(ctx, core.ClaimOf(c), time.Hour)
	if err != nil || until.Before(time.Now().Add(59*time.Minute)) {
		t.Errorf("Renew = %s, %v", until, err)
	}

	long := "## 第 1 頁\n\n" + strings.Repeat("一二三四五六七八九十\n", 3000) + "## 第 2 頁\n\n" + strings.Repeat("abcdefghij\n", 3000)
	rev, err := s.Complete(ctx, core.ClaimOf(c), core.Completion{Status: core.TextDone, Body: long, Pages: 2, Model: "Gemini Flash-Lite"})
	if err != nil || rev != 2 {
		t.Fatalf("Complete = %d, %v", rev, err)
	}
	// Sent again under its key, it is Core's replay.
	if rev, err := s.Complete(ctx, core.ClaimOf(c), core.Completion{Status: core.TextDone, Body: long, Pages: 2,
		Model: "Gemini Flash-Lite"}); err != nil || rev != 2 {
		t.Errorf("the completion again: %d, %v", rev, err)
	}
	if rec, _ := w.fc.Text(first); rec.Status != core.TextDone || rec.Source != core.SourceAI || rec.Body != long || rec.Pages != 2 ||
		rec.Model != "Gemini Flash-Lite" || rec.Claimed {
		t.Errorf("done: %+v", rec)
	}
	if _, err := s.Renew(ctx, core.ClaimOf(c), 0); !core.IsReason(err, core.ReasonLeaseLost) {
		t.Errorf("a renewal after the completion: %v", err)
	}

	// Longer than one part: document_get says so, and document_text reads
	// it in parts, each cut after a whole line (or before a page's
	// heading, where that leaves it half full).
	g := mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", first))
	if err := json.Unmarshal([]byte(g.Text), &got); err != nil || text().Status != core.TextDone || text().Body != nil ||
		text().Bytes != len(long) || text().Source != core.SourceAI {
		t.Errorf("the text done, as document_get gives it: %s", g.Text[:min(len(g.Text), 600)])
	}
	var joined strings.Builder
	for part := 1; ; part++ {
		p := mustCall(t, w.agentC, "document_text", inCourseArgs(w, "document_id", first, "file_id", c.FileID, "part", part))
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
			found = p["version_id"] == c.VersionID && p["file_id"] == c.FileID && p["status"] == "done" && p["source"] == "ai" &&
				ev["subject_id"] == first
		}
	}
	if !found {
		t.Errorf("no text event: %v", evs)
	}
}

// A version of three files and text (AIShie-Core #49): document_get lists
// the files in order, each named, with a URL that serves it under its name
// and its own text version, and the version says nothing of a file of its
// own (AIShie-Core #61); document_versions lists them without either;
// document_file gives one again by its id, and nothing of another
// document. The service claims each file on its own, in order, and every
// call names it: one that names no file is refused, and one whose lease
// the file does not hold is lease_lost. Each text is written back, read by
// its file_id, and its news names the file.
func TestTextFilesOfAVersion(t *testing.T) {
	w := newFakeWorld(t, Options{})
	ctx := t.Context()
	doc, ids, err := w.fc.AddFiles(w.co.ID, "Week 3", "Slides first, then run the program.",
		File{Filename: "week3-slides.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7 week three")},
		File{ContentType: "text/plain", Data: []byte("The handout for week three.")},
		File{Filename: "loops.py", ContentType: "text/x-python", Data: []byte("for i in range(3):\n    print(i)\n")})
	w.ok(err)
	if len(ids) != 3 {
		t.Fatalf("file ids %v", ids)
	}
	type fileView struct {
		ID          string         `json:"id"`
		Position    int            `json:"position"`
		Filename    string         `json:"filename"`
		ContentType string         `json:"content_type"`
		ByteSize    int64          `json:"byte_size"`
		Checksum    string         `json:"checksum"`
		DownloadURL string         `json:"download_url"`
		Text        *core.TextView `json:"text"`
	}
	var got struct {
		Result struct {
			Version struct {
				BodyMD string     `json:"body_md"`
				Files  []fileView `json:"files"`
			} `json:"version"`
		} `json:"result"`
	}
	a := mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", doc))
	w.ok(json.Unmarshal([]byte(a.Text), &got))
	v := got.Result.Version
	if len(v.Files) != 3 || v.BodyMD != "Slides first, then run the program." {
		t.Fatalf("the version: %s", a.Text)
	}
	// What a version said of its first file alone went with AIShie-Core #61.
	var own struct {
		Result struct {
			Version map[string]any `json:"version"`
		} `json:"result"`
	}
	w.ok(json.Unmarshal([]byte(a.Text), &own))
	for _, gone := range []string{"download_url", "content_type", "byte_size", "checksum", "text"} {
		if _, ok := own.Result.Version[gone]; ok {
			t.Errorf("the version gives %s of its own: %s", gone, a.Text)
		}
	}
	names := []string{"week3-slides.pdf", "Week 3.txt", "loops.py"}
	for i, f := range v.Files {
		if f.ID != ids[i] || f.Position != i+1 || f.Filename != names[i] || !strings.HasPrefix(f.Checksum, "sha256:") || f.Text == nil ||
			f.Text.Status != core.TextPending {
			t.Errorf("file %d: %+v", i+1, f)
		}
		resp, err := w.srv.Client().Get(f.DownloadURL) //nolint:noctx // the fake's own URL.
		w.ok(err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, names[i]) {
			t.Errorf("file %d downloads as %q", i+1, cd)
		}
	}
	vs := mustCall(t, w.as("sato"), "document_versions", inCourseArgs(w, "document_id", doc))
	if strings.Contains(vs.Text, "download_url") || strings.Count(vs.Text, `"filename"`) != 3 {
		t.Errorf("document_versions: %s", vs.Text)
	}
	one := mustCall(t, w.agentC, "document_file", inCourseArgs(w, "document_id", doc, "file_id", ids[2]))
	if !strings.Contains(one.Text, `"filename":"loops.py"`) || !strings.Contains(one.Text, `"download_url"`) || !strings.Contains(one.Text, `"position":3`) {
		t.Errorf("document_file: %s", one.Text)
	}
	wantEnvelope(t, mustCall(t, w.agentC, "document_file", inCourseArgs(w, "document_id", w.co.SlidesID, "file_id", ids[0])), "error", "not_found", "")

	s := w.service(t, w.fc.IssueServiceToken("runtime").Token)
	claimed, err := s.Queue(ctx, 10, 5*time.Minute, 0)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("Queue = %+v, %v", claimed, err)
	}
	for i, c := range claimed {
		if c.FileID != ids[i] || c.Position != i+1 || c.Filename != names[i] || c.DocumentID != doc || c.VersionID != claimed[0].VersionID {
			t.Errorf("claim %d: %+v", i+1, c)
		}
	}
	// A call naming no file is refused, as AIShie-Core #61 refuses it; one
	// of a lease the file does not hold is lease_lost.
	var se *core.ServiceError
	if _, err := s.File(ctx, core.Claim{VersionID: claimed[1].VersionID, LeaseID: claimed[1].LeaseID}); !errors.As(err, &se) ||
		se.Code != core.CodeInvalidArgument {
		t.Errorf("File naming no file: %v", err)
	}
	if _, err := s.Renew(ctx, core.Claim{VersionID: claimed[1].VersionID, FileID: ids[1], LeaseID: "01a0f2de-0000-7000-8000-000000000000"}, 0); !core.IsReason(err,
		core.ReasonLeaseLost) {
		t.Errorf("a renewal of a lease the file does not hold: %v", err)
	}
	for i, c := range claimed {
		if f, err := s.File(ctx, core.ClaimOf(c)); err != nil || f.FileID != ids[i] || f.Filename != names[i] {
			t.Errorf("File %d = %+v, %v", i+1, f, err)
		}
		if _, err := s.Complete(ctx, core.ClaimOf(c), core.Completion{Status: core.TextDone, Body: "## " + names[i], Pages: 1, Model: "m"}); err != nil {
			t.Errorf("Complete %d: %v", i+1, err)
		}
	}
	for i, id := range ids {
		p := mustCall(t, w.agentC, "document_text", inCourseArgs(w, "document_id", doc, "file_id", id))
		var tp core.TextPart
		w.ok(json.Unmarshal([]byte(p.Text), &struct {
			Result *core.TextPart `json:"result"`
		}{&tp}))
		if tp.FileID != id || tp.Filename != names[i] || tp.Text.Body == nil || *tp.Text.Body != "## "+names[i] {
			t.Errorf("the text of file %d: %s", i+1, p.Text)
		}
	}
	a = mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", doc))
	w.ok(json.Unmarshal([]byte(a.Text), &got))
	for i, f := range got.Result.Version.Files {
		if f.Text == nil || f.Text.Body == nil || *f.Text.Body != "## "+names[i] {
			t.Errorf("file %d's text in document_get: %+v", i+1, f.Text)
		}
	}
	evs := list(mustCall(t, w.as("sato"), "event_list", inCourseArgs(w, "since_seq", 0)), "events")
	files := map[string]bool{}
	for _, e := range evs {
		ev := e.(map[string]any)
		if p, _ := ev["payload"].(map[string]any); ev["type"] == "document.text_updated" && p["version_id"] == claimed[0].VersionID {
			files[p["file_id"].(string)] = true
		}
	}
	if len(files) != 3 {
		t.Errorf("text events of the files %v", files)
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
		"renew": func() error { _, err := s.Renew(ctx, core.ClaimOf(c1), 0); return err }(),
		"file":  func() error { _, err := s.File(ctx, core.ClaimOf(c1)); return err }(),
		"complete": func() error {
			_, err := s.Complete(ctx, core.ClaimOf(c1), core.Completion{Status: core.TextSkipped, Reason: "too_many_pages"})
			return err
		}(),
	} {
		if !core.IsReason(err, core.ReasonLeaseLost) {
			t.Errorf("%s with the lease lost: %v", name, err)
		}
	}

	w.ok(w.fc.EditText(doc, w.sato.ID, "## 第 1 頁\n\nStaff's"))
	if _, err := s.Renew(ctx, core.ClaimOf(c2), 0); !core.IsReason(err, core.ReasonEditedByStaff) {
		t.Errorf("a renewal of a text staff wrote: %v", err)
	}
	if _, err := s.Complete(ctx, core.ClaimOf(c2), core.Completion{Status: core.TextDone, Body: "x", Pages: 1, Model: "m"}); !core.IsReason(err,
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

// The runtime's client reads a file of a version again by its id
// (document_file), and the text of one file of several (TextPart with its
// file_id); naming no file is refused, as AIShie-Core #61 refuses it.
func TestFilesThroughTheClient(t *testing.T) {
	w := newFakeWorld(t, Options{})
	ctx := t.Context()
	cl := w.agentClient()
	doc, ids, err := w.fc.AddFiles(w.co.ID, "Week 4", "",
		File{Filename: "slides.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7 week four")},
		File{Filename: "notes.md", ContentType: "text/markdown", Data: []byte("# Notes")})
	w.ok(err)
	f, err := cl.DocumentFile(ctx, w.co.ID, doc, ids[1])
	w.ok(err)
	if f.ID != ids[1] || f.Position != 2 || f.Filename != "notes.md" || f.ContentType != "text/markdown" || f.ByteSize != 7 ||
		f.DocumentID != doc || f.DownloadURL == "" || f.ExpiresAt.IsZero() || f.Text == nil || f.Text.Status != core.TextPending {
		t.Errorf("DocumentFile = %+v", f)
	}
	if _, err := cl.DocumentFile(ctx, w.co.ID, w.co.SlidesID, ids[0]); !core.IsMissing(err) {
		t.Errorf("a file of another document: %v", err)
	}
	m := w.sato.ID
	w.ok(w.fc.EditFileText(doc, ids[1], m, "## 第 1 頁\n\nThe notes."))
	w.ok(w.fc.EditText(doc, m, "## 第 1 頁\n\nThe slides."))
	tp, err := cl.TextPart(ctx, w.co.ID, doc, "", ids[1], 1)
	w.ok(err)
	if tp.FileID != ids[1] || tp.Filename != "notes.md" || tp.Position != 2 || tp.Text.Body == nil || *tp.Text.Body != "## 第 1 頁\n\nThe notes." {
		t.Errorf("TextPart of the notes = %+v", tp)
	}
	tp, err = cl.TextPart(ctx, w.co.ID, doc, "", ids[0], 1)
	w.ok(err)
	if tp.FileID != ids[0] || tp.Text.Body == nil || *tp.Text.Body != "## 第 1 頁\n\nThe slides." {
		t.Errorf("TextPart of the slides = %+v", tp)
	}
	var ee *core.EnvelopeError
	if _, err := cl.TextPart(ctx, w.co.ID, doc, "", "", 1); !errors.As(err, &ee) || ee.Envelope.Code() != core.CodeInvalidArgument {
		t.Errorf("TextPart naming no file: %v", err)
	}
}

// A Core from before several files to a version (WithoutFiles) lists no
// files, names no file in a claim, a text read or a text event, has no
// document_file, and its service refuses a call that names a file.
func TestTextWithoutFiles(t *testing.T) {
	w := newFakeWorld(t, Options{WithoutFiles: true})
	ctx := t.Context()
	doc, err := w.fc.AddFile(w.co.ID, "Week 1", "application/pdf", []byte("%PDF-1.4 week 1"))
	w.ok(err)
	a := mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", doc))
	if strings.Contains(a.Text, `"files"`) || !strings.Contains(a.Text, `"download_url"`) || !strings.Contains(a.Text, `"text":{"status":"pending"`) {
		t.Errorf("document_get: %s", a.Text)
	}
	if strings.Contains(mustCall(t, w.as("sato"), "document_versions", inCourseArgs(w, "document_id", doc)).Text, `"files"`) {
		t.Error("document_versions lists files")
	}
	s := w.service(t, w.fc.IssueServiceToken("runtime").Token)
	claimed, err := s.Queue(ctx, 1, time.Minute, 0)
	if err != nil || len(claimed) != 1 || claimed[0].FileID != "" || claimed[0].Filename != "" {
		t.Fatalf("Queue = %+v, %v", claimed, err)
	}
	c := claimed[0]
	named := core.Claim{VersionID: c.VersionID, FileID: "01a0f2de-0000-7000-8000-000000000001", LeaseID: c.LeaseID}
	var se *core.ServiceError
	if _, err := s.Renew(ctx, named, time.Minute); !errors.As(err, &se) || se.Code != core.CodeInvalidArgument {
		t.Errorf("a renewal naming a file: %v", err)
	}
	if f, err := s.File(ctx, core.ClaimOf(c)); err != nil || f.FileID != "" {
		t.Errorf("File = %+v, %v", f, err)
	}
	if _, err := s.Complete(ctx, core.ClaimOf(c), core.Completion{Status: core.TextDone, Body: "## 第 1 頁", Pages: 1, Model: "m"}); err != nil {
		t.Errorf("Complete: %v", err)
	}
	for _, call := range w.fc.Calls() {
		if call.Tool == "document_text_complete" && call.IdempotencyKey != "complete:"+c.VersionID+":"+c.LeaseID {
			t.Errorf("the completion's key: %q", call.IdempotencyKey)
		}
	}
	p := mustCall(t, w.agentC, "document_text", inCourseArgs(w, "document_id", doc))
	if strings.Contains(p.Text, "file_id") || !strings.Contains(p.Text, "## 第 1 頁") {
		t.Errorf("document_text: %s", p.Text)
	}
	for _, e := range list(mustCall(t, w.as("sato"), "event_list", inCourseArgs(w, "since_seq", 0)), "events") {
		if ev := e.(map[string]any); ev["type"] == "document.text_updated" {
			if _, ok := ev["payload"].(map[string]any)["file_id"]; ok {
				t.Errorf("a text event names a file: %v", ev)
			}
		}
	}
	if a, err := w.agentC.call(ctx, "document_file", inCourseArgs(w, "document_id", doc, "file_id", c.VersionID)); err != nil || a.RPCError == nil {
		t.Errorf("document_file: %v %s", err, a.Body)
	}
}
