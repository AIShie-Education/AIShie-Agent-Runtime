package fakecore

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// agentClient is the runtime's client of the fake, over MCP, as the tutor.
func (w *fakeWorld) agentClient() *core.Client {
	return core.NewClient(core.NewMCPCaller(core.MCPOptions{BaseURL: w.srv.URL, Token: w.tutorA.Token, HTTPClient: w.srv.Client()}))
}

// A question asked with files, and a follow-up with another, as the test
// controls ask them: the runtime's client reads them in the messages, in
// order, and each file's download URL serves its bytes under its name; a
// message's news says what it carries; a file of the conversation is
// another student's to nobody; and once its message is retracted, a file
// is not listed, and is refused as retracted.
func TestAttachmentsThroughTheClient(t *testing.T) {
	w := newFakeWorld(t, Options{})
	ctx := t.Context()
	cl := w.agentClient()
	notes := File{Filename: "notes.txt", ContentType: "text/plain", Data: []byte("a loop repeats\n")}
	essay := File{Filename: "論文 1.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4 an essay")}
	cv, q, err := w.fc.AskWithFiles(w.co.ID, w.seats[0].ID, w.tutorM.ID, "Please read these.", notes, essay)
	w.ok(err)
	graph := File{Filename: "graph.png", ContentType: "image/png", Data: []byte("\x89PNG graph")}
	f, err := w.fc.FollowUpWithFiles(cv.ID, "And this.", graph)
	w.ok(err)
	if ids := w.fc.Attachments(q.ID); len(ids) != 2 {
		t.Fatalf("the question carries %v", ids)
	}

	read, err := cl.Messages(ctx, w.co.ID, cv.ID, core.MessagesQuery{})
	w.ok(err)
	if len(read.Messages) != 2 || len(read.Messages[0].Attachments) != 2 || len(read.Messages[1].Attachments) != 1 {
		t.Fatalf("messages: %+v", read.Messages)
	}
	first := read.Messages[0].Attachments[1]
	if first.Filename != essay.Filename || first.ContentType != "application/pdf" || first.ByteSize != int64(len(essay.Data)) ||
		first.Checksum == nil || first.CreatedAt == "" {
		t.Errorf("the essay is listed as %+v", first)
	}
	got, err := cl.Attachment(ctx, w.co.ID, first.ID)
	w.ok(err)
	if got.ConversationID != cv.ID || got.MessageID != q.ID || got.MessageSeq != 1 || got.AuthorMemberID != w.seats[0].ID ||
		got.DownloadURL == "" || got.ExpiresAt == "" || got.ID != first.ID || got.Filename != first.Filename || got.Checksum == nil ||
		*got.Checksum != *first.Checksum || got.CreatedAt != first.CreatedAt {
		t.Errorf("conversation_attachment: %+v", got.Attachment)
	}
	resp, err := w.srv.Client().Get(got.DownloadURL)
	w.ok(err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, essay.Data) || resp.Header.Get("Content-Type") != "application/pdf" ||
		resp.Header.Get("Content-Disposition") != "attachment; filename*=utf-8''%E8%AB%96%E6%96%87%201.pdf" {
		t.Errorf("the download: %d %q %q", resp.StatusCode, resp.Header, body)
	}

	evs, err := cl.Events(ctx, w.co.ID, 0, 0, 0)
	w.ok(err)
	var posted [][]core.Attachment
	for _, e := range evs.Events {
		if e.Type == core.EventConversationMessagePosted {
			posted = append(posted, core.PostedAttachments(e))
		}
	}
	if len(posted) != 2 || len(posted[0]) != 2 || posted[0][0].Filename != "notes.txt" || posted[1][0].ContentType != "image/png" {
		t.Errorf("the news: %+v", posted)
	}

	// Ken reads nothing of Yuki's conversation: the same not_found as a
	// file that is none.
	ken := w.as("ken")
	if a := mustCall(t, ken, "conversation_attachment", inCourseArgs(w, "attachment_id", first.ID)); a.str("error", "code") != "not_found" ||
		a.str("error", "details", "reason") != "" {
		t.Errorf("Ken reads Yuki's file: %s", a.Text)
	}

	w.author[f.ID] = 0
	w.retract(f.ID, "")
	_, err = cl.Attachment(ctx, w.co.ID, read.Messages[1].Attachments[0].ID)
	if !core.IsRetracted(err) || !core.IsMissing(err) {
		t.Errorf("a retracted message's file: %v", err)
	}
	_, err = cl.Attachment(ctx, w.co.ID, "0192f3c1-0000-7000-8000-00000000abcd")
	if core.IsRetracted(err) || !core.IsMissing(err) {
		t.Errorf("a file that is none: %v", err)
	}
	var ee *core.EnvelopeError
	if !errors.As(err, &ee) {
		t.Errorf("a file that is none: %T", err)
	}
	read, err = cl.Messages(ctx, w.co.ID, cv.ID, core.MessagesQuery{})
	w.ok(err)
	if len(read.Messages[1].Attachments) != 0 || read.Messages[1].Retracted == nil || len(read.Messages[0].Attachments) != 2 {
		t.Errorf("after the retraction: %+v", read.Messages)
	}
}

// A message's files are held to Core's limits and rules: at most ten, each
// no larger than 50 MiB (and deleted when it is), named as a file is named,
// and attached once.
func TestAttachmentLimits(t *testing.T) {
	w := newFakeWorld(t, Options{})
	eleven := make([]File, 11)
	for i := range eleven {
		eleven[i] = File{Filename: "f.txt", ContentType: "text/plain", Data: []byte{'x'}}
	}
	refusedWith := func(err error, reason string) {
		t.Helper()
		var re *RefusedError
		if !errors.As(err, &re) || re.Reason != reason {
			t.Errorf("%v, want %s", err, reason)
		}
	}
	_, _, err := w.fc.AskWithFiles(w.co.ID, w.seats[0].ID, w.tutorM.ID, "Many.", eleven...)
	refusedWith(err, "too_many_attachments")
	_, _, err = w.fc.AskWithFiles(w.co.ID, w.seats[0].ID, w.tutorM.ID, "Big.",
		File{Filename: "big.bin", ContentType: "application/octet-stream", Data: make([]byte, attachmentMaxBytes+1)})
	refusedWith(err, "file_too_large")
	_, _, err = w.fc.AskWithFiles(w.co.ID, w.seats[0].ID, w.tutorM.ID, "Named.",
		File{Filename: "a\u202egpj.exe", ContentType: "image/jpeg", Data: []byte{1}})
	refusedWith(err, "bad_filename")
	cv, _, err := w.fc.AskWithFiles(w.co.ID, w.seats[0].ID, w.tutorM.ID, "None, but fine.")
	w.ok(err)
	if _, err := w.fc.FollowUpWithFiles(cv.ID, "One.", File{Filename: " spaced.txt ", ContentType: "text/plain", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	a := mustCall(t, w.agentC, "conversation_messages", inCourseArgs(w, "conversation_id", cv.ID))
	msgs := list(a, "messages")
	last, _ := msgs[len(msgs)-1].(map[string]any)
	files, _ := last["attachments"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["filename"] != "spaced.txt" {
		t.Errorf("a name is trimmed: %s", a.Text)
	}
	if first, _ := msgs[0].(map[string]any); first["attachments"] != nil {
		t.Errorf("a message with no files lists %v", first["attachments"])
	}
	// The upload URL is taken once, with the type it was issued for.
	up := mustCall(t, w.as("yuki"), "conversation_upload_url", inCourseArgs(w, "content_type", "application/pdf"))
	put := func(ct string) int {
		req, _ := http.NewRequest(http.MethodPut, up.str("result", "upload_url"), bytes.NewReader([]byte("%PDF")))
		req.Header.Set("Content-Type", ct)
		resp, err := w.srv.Client().Do(req)
		w.ok(err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if a, b, c := put("text/html"), put("application/pdf"), put("application/pdf"); a != 400 || b != 200 || c != 409 {
		t.Errorf("the PUTs: %d %d %d", a, b, c)
	}
	// Someone who neither asks nor answers gets no upload URL.
	w.ok(w.fc.SetLevel(w.seats[1].ID, permConversationAsk, "denied"))
	if a := mustCall(t, w.as("ken"), "conversation_upload_url", inCourseArgs(w, "content_type", "text/plain")); a.str("error", "details", "reason") != "permission_denied" {
		t.Errorf("an upload URL for Ken, who asks nothing: %s", a.Text)
	}
}
