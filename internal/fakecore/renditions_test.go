package fakecore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

const docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// put PUTs data to an upload's URL with the headers it names.
func (w *fakeWorld) put(t *testing.T, u *core.RenditionUpload, data []byte) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, u.UploadURL, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range u.Headers {
		req.Header.Set(k, v)
	}
	resp, err := w.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestRenditionQueue: an Office file of a version, of any declared type
// Core takes, is queued, a PDF or a text not; the runtime claims it with
// its typed client, gets its file, renews, uploads the PDF and completes
// it done, under the key Core suggests; the PDF is served to whoever reads
// the file, inline, named with .pdf; and a message's Office file is
// claimed after it, and skipped.
func TestRenditionQueue(t *testing.T) {
	w := newFakeWorld(t, Options{})
	ctx := t.Context()
	doc, files, err := w.fc.AddFiles(w.co.ID, "Week 4", "",
		File{Filename: "Lecture 4.PPTX", ContentType: "application/octet-stream", Data: []byte("PK\x03\x04 deck")},
		File{Filename: "slides.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4 slides")},
		File{Filename: "marks.csv", ContentType: "application/vnd.ms-excel", Data: []byte("a,b\n")})
	w.ok(err)
	if _, ok := w.fc.Rendition(files[1]); ok {
		t.Error("a PDF is queued for a rendition")
	}
	if _, ok := w.fc.Rendition(files[2]); ok {
		t.Error("a .csv declared an Excel file is queued for a rendition")
	}
	if r, ok := w.fc.Rendition(files[0]); !ok || r.State != core.RenditionQueued || r.Attempts != 0 {
		t.Fatalf("the deck's rendition: %+v %v", r, ok)
	}
	_, _, err = w.fc.AskWithFiles(w.co.ID, w.seats[0].ID, w.tutorM.ID, "My essay.",
		File{Filename: "essay.docx", ContentType: docxType, Data: []byte("PK\x03\x04 essay")})
	w.ok(err)

	s := w.runtimeClient(w.svc.Token)
	claimed, err := s.ClaimRenditions(ctx, 1, 5*time.Minute, 0)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimRenditions = %+v, %v", claimed, err)
	}
	c := claimed[0]
	if c.Source != core.RenditionOfDocumentFile || c.FileID != files[0] || c.AttachmentID != "" || c.Attempt != 1 || c.Backfill ||
		c.Filename != "Lecture 4.PPTX" || c.CourseID != w.co.ID || c.ByteSize != 9 || c.MaxBytes != core.DefaultRenditionMaxBytes ||
		!strings.HasPrefix(c.Checksum, "sha256:") || c.LeaseExpiresAt.Before(time.Now().Add(4*time.Minute)) {
		t.Errorf("the claim: %+v", c)
	}
	if body := get(t, w, c.DownloadURL); body != "PK\x03\x04 deck" {
		t.Errorf("the file: %q", body)
	}
	cl := core.ClaimOfRendition(c)
	f, err := s.RenditionFile(ctx, cl)
	if err != nil || f.Filename != c.Filename || get(t, w, f.DownloadURL) != "PK\x03\x04 deck" {
		t.Errorf("RenditionFile = %+v, %v", f, err)
	}
	until, err := s.RenewRendition(ctx, cl, 20*time.Minute)
	if err != nil || until.Before(time.Now().Add(19*time.Minute)) {
		t.Errorf("RenewRendition = %v, %v", until, err)
	}
	up, err := s.RenditionUploadURL(ctx, cl)
	if err != nil || up.Headers["Content-Type"] != "application/pdf" || up.MaxBytes != core.DefaultRenditionMaxBytes || up.UploadToken == "" {
		t.Fatalf("RenditionUploadURL = %+v, %v", up, err)
	}
	pdf := []byte("%PDF-1.7\n% the deck\n")
	if st := w.put(t, up, pdf); st != http.StatusOK {
		t.Fatalf("the PUT: %d", st)
	}
	done, err := s.CompleteRendition(ctx, cl, core.RenditionKey(cl, 1),
		core.RenditionCompletion{Status: core.RenditionDone, UploadToken: up.UploadToken, PageCount: 12})
	if err != nil || done.State != core.RenditionDone || done.ByteSize != int64(len(pdf)) || !strings.HasPrefix(done.Checksum, "sha256:") {
		t.Fatalf("CompleteRendition = %+v, %v", done, err)
	}
	if again, err := s.CompleteRendition(ctx, cl, core.RenditionKey(cl, 1),
		core.RenditionCompletion{Status: core.RenditionDone, UploadToken: up.UploadToken, PageCount: 12}); err != nil || *again != *done {
		t.Errorf("the completion again, under its key: %+v, %v", again, err)
	}
	if r, _ := w.fc.Rendition(files[0]); r.State != core.RenditionDone || r.Pages != 12 || !bytes.Equal(r.PDF, pdf) || r.Claimed {
		t.Errorf("done: %+v", r)
	}
	a := mustCall(t, w.agentC, "document_file", inCourseArgs(w, "document_id", doc, "file_id", files[0]))
	var got struct {
		Result struct {
			Rendition *core.RenditionView `json:"rendition"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(a.Text), &got); err != nil || got.Result.Rendition == nil || got.Result.Rendition.State != core.RenditionDone ||
		got.Result.Rendition.PageCount != 12 || got.Result.Rendition.DownloadURL == "" {
		t.Fatalf("document_file: %s", a.Text)
	}
	resp, err := w.srv.Client().Get(got.Result.Rendition.DownloadURL) //nolint:noctx // the fake's own URL.
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(body, pdf) || resp.Header.Get("Content-Type") != "application/pdf" ||
		resp.Header.Get("Content-Disposition") != "inline; filename=\"Lecture 4.pdf\"" {
		t.Errorf("the PDF served: %q %v", body, resp.Header)
	}

	// The message's file, next.
	claimed, err = s.ClaimRenditions(ctx, 10, 0, 0)
	if err != nil || len(claimed) != 1 || claimed[0].Source != core.RenditionOfAttachment || claimed[0].AttachmentID == "" ||
		claimed[0].FileID != "" || claimed[0].Filename != "essay.docx" {
		t.Fatalf("the message's file: %+v, %v", claimed, err)
	}
	if body := get(t, w, claimed[0].DownloadURL); body != "PK\x03\x04 essay" {
		t.Errorf("the message's file: %q", body)
	}
	cl = core.ClaimOfRendition(claimed[0])
	if res, err := s.CompleteRendition(ctx, cl, core.RenditionKey(cl, 1),
		core.RenditionCompletion{Status: core.RenditionSkipped, Reason: core.RenditionPasswordProtected}); err != nil || res.State != core.RenditionSkipped {
		t.Errorf("skipped: %+v, %v", res, err)
	}
	if r, _ := w.fc.Rendition(claimed[0].AttachmentID); r.State != core.RenditionSkipped || r.Reason != core.RenditionPasswordProtected {
		t.Errorf("the message's file's rendition: %+v", r)
	}
}

// TestRenditionLapses: a claim that lapses is claimed again, attempt 2,
// and the first lease's calls are refused lease_lost; one claimed five
// times and not finished fails attempts_exhausted at the next claim; and a
// revoked credential's claims go back to the queue, uncounted, the
// revoked credential refused.
func TestRenditionLapses(t *testing.T) {
	w := newFakeWorld(t, Options{})
	ctx := t.Context()
	_, files, err := w.fc.AddFiles(w.co.ID, "Week 5", "", File{Filename: "a.docx", ContentType: docxType, Data: []byte("PK\x03\x04 a")})
	w.ok(err)
	s := w.runtimeClient(w.svc.Token)
	first, err := s.ClaimRenditions(ctx, 1, 0, 0)
	if err != nil || len(first) != 1 {
		t.Fatalf("ClaimRenditions = %+v, %v", first, err)
	}
	w.ok(w.fc.LapseRenditionClaim(files[0]))
	// Lapsed, the first claim holds until another takes it.
	if _, err := s.RenewRendition(ctx, core.ClaimOfRendition(first[0]), time.Minute); err != nil {
		t.Errorf("a lapsed claim nobody took again: %v", err)
	}
	w.ok(w.fc.LapseRenditionClaim(files[0]))
	second, err := s.ClaimRenditions(ctx, 1, 0, 0)
	if err != nil || len(second) != 1 || second[0].Attempt != 2 || second[0].LeaseID == first[0].LeaseID {
		t.Fatalf("claimed again: %+v, %v", second, err)
	}
	old := core.ClaimOfRendition(first[0])
	for name, err := range map[string]error{
		"renew":      func() error { _, err := s.RenewRendition(ctx, old, time.Minute); return err }(),
		"file":       func() error { _, err := s.RenditionFile(ctx, old); return err }(),
		"upload URL": func() error { _, err := s.RenditionUploadURL(ctx, old); return err }(),
		"complete": func() error {
			_, err := s.CompleteRendition(ctx, old, core.RenditionKey(old, 1), core.RenditionCompletion{Status: core.RenditionFailed,
				Reason: core.RenditionTimeout})
			return err
		}(),
	} {
		if !core.IsReason(err, core.ReasonLeaseLost) {
			t.Errorf("the first lease's %s: %v", name, err)
		}
	}
	// A PDF uploaded for the second claim is not the first's to name.
	up, err := s.RenditionUploadURL(ctx, core.ClaimOfRendition(second[0]))
	w.ok(err)
	if st := w.put(t, up, []byte("%PDF-1.4")); st != http.StatusOK {
		t.Fatalf("the PUT: %d", st)
	}
	_, files2, err := w.fc.AddFiles(w.co.ID, "Week 6", "", File{Filename: "b.docx", ContentType: docxType, Data: []byte("PK\x03\x04 b")})
	w.ok(err)
	other, err := s.ClaimRenditions(ctx, 1, 0, 0)
	if err != nil || len(other) != 1 || other[0].FileID != files2[0] {
		t.Fatalf("the other file: %+v, %v", other, err)
	}
	ocl := core.ClaimOfRendition(other[0])
	if _, err := s.CompleteRendition(ctx, ocl, core.RenditionKey(ocl, 1), core.RenditionCompletion{Status: core.RenditionDone,
		UploadToken: up.UploadToken, PageCount: 1}); !core.IsReason(err, core.ReasonNotYourUpload) {
		t.Errorf("another claim's upload: %v", err)
	}

	// Lapsed three times more, it fails at the claim after.
	for range 3 {
		w.ok(w.fc.LapseRenditionClaim(files[0]))
		if c, err := s.ClaimRenditions(ctx, 1, 0, 0); err != nil || len(c) != 1 || c[0].FileID != files[0] {
			t.Fatalf("claimed again: %+v, %v", c, err)
		}
	}
	if r, _ := w.fc.Rendition(files[0]); r.Attempts != 5 {
		t.Fatalf("after five claims: %+v", r)
	}
	w.ok(w.fc.LapseRenditionClaim(files[0]))
	if c, err := s.ClaimRenditions(ctx, 10, 0, 0); err != nil || len(c) != 0 {
		t.Errorf("claimed a sixth time: %+v, %v", c, err)
	}
	if r, _ := w.fc.Rendition(files[0]); r.State != core.RenditionFailed || r.Reason != core.ReasonAttemptsExhausted {
		t.Errorf("after five lapsed claims: %+v", r)
	}

	// Revoked, what the credential held goes back, uncounted.
	w.ok(w.fc.RevokeServiceToken(w.svc.CredentialID))
	if r, _ := w.fc.Rendition(files2[0]); r.State != core.RenditionQueued || r.Attempts != 0 {
		t.Errorf("released: %+v", r)
	}
	if _, err := s.ClaimRenditions(ctx, 1, 0, 0); !core.CredentialRefused(err) {
		t.Errorf("a revoked credential's claim: %v", err)
	}
}

// TestRenditionOrder: files recorded are claimed oldest first, before the
// backfill, which is claimed newest first; a claim that waits (wait_s) is
// answered as soon as a file is queued.
func TestRenditionOrder(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	w := newFakeWorld(t, Options{Now: func() time.Time { return clock() }})
	ctx := t.Context()
	var ids []string
	for i, name := range []string{"old.docx", "older-backfill.docx", "newer-backfill.docx", "new.docx"} {
		now = now.Add(time.Minute)
		_, files, err := w.fc.AddFiles(w.co.ID, name, "", File{Filename: name, ContentType: docxType, Data: []byte{'P', 'K', 3, 4, byte(i)}})
		w.ok(err)
		ids = append(ids, files[0])
	}
	w.ok(w.fc.Backfill(ids[1]))
	w.ok(w.fc.Backfill(ids[2]))
	s := w.runtimeClient(w.svc.Token)
	claimed, err := s.ClaimRenditions(ctx, 10, 0, 0)
	if err != nil || len(claimed) != 4 {
		t.Fatalf("ClaimRenditions = %+v, %v", claimed, err)
	}
	var got []string
	for _, c := range claimed {
		got = append(got, c.Filename)
		if c.Backfill != strings.Contains(c.Filename, "backfill") {
			t.Errorf("%s: backfill %v", c.Filename, c.Backfill)
		}
	}
	if strings.Join(got, " ") != "old.docx new.docx newer-backfill.docx older-backfill.docx" {
		t.Errorf("claimed in the order %v", got)
	}

	done := make(chan []core.ClaimedRendition, 1)
	go func() {
		c, _ := s.ClaimRenditions(context.Background(), 1, 0, 10*time.Second)
		done <- c
	}()
	time.Sleep(200 * time.Millisecond)
	_, _, err = w.fc.AskWithFiles(w.co.ID, w.seats[0].ID, w.tutorM.ID, "Here.",
		File{Filename: "late.pptx", ContentType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", Data: []byte("PK")})
	w.ok(err)
	select {
	case c := <-done:
		if len(c) != 1 || c[0].Filename != "late.pptx" {
			t.Errorf("the claim that waited: %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the claim that waited was not answered when a file was queued")
	}
}

// TestWithoutRenditions: a Core from before renditions offers none of
// their tools, queues no file, and shows no rendition.
func TestWithoutRenditions(t *testing.T) {
	w := newFakeWorld(t, Options{WithoutRenditions: true})
	_, files, err := w.fc.AddFiles(w.co.ID, "Week 4", "", File{Filename: "a.docx", ContentType: docxType, Data: []byte("PK")})
	w.ok(err)
	if _, ok := w.fc.Rendition(files[0]); ok {
		t.Error("a Core from before renditions queued one")
	}
	cat, err := core.FetchCatalogue(t.Context(), w.srv.Client(), w.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if core.HasRenditions(cat) || !core.HasRuntimeService(cat) {
		t.Error("a Core from before renditions offers them, or offers no hosting")
	}
	if a := mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", w.co.SyllabusID)); strings.Contains(a.Text, "rendition") {
		t.Errorf("document_get shows a rendition: %s", a.Text)
	}
}

// get is the body a GET of url answers.
func get(t *testing.T, w *fakeWorld, url string) string {
	t.Helper()
	resp, err := w.srv.Client().Get(url) //nolint:noctx // the fake's own URL.
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
