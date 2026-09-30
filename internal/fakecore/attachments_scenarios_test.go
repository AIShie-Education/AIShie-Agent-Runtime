package fakecore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// attachments is a message's files (Core's conversation attachments): a
// student uploads files for a question (conversation_upload_url, the PUT
// to its URL, and the PUT's refusals), asks with them and follows up with
// another; what the refusals of a message's files are; the tutor reading the
// conversation, each file's download URL, and the download itself; a file
// that is none, one of another student's conversation, which the tutor
// reads and the student does not; and a retracted message's files
// withheld, and its news.
var attachments = scenario{name: "attachments", about: "a message's files: conversation_upload_url and its PUT, conversation_open and _ask " +
	"with attachments and their refusals, conversation_messages listing them, conversation_attachment and its download, a file " +
	"that is none or another's, a retracted message's files withheld, and the news of a message that carries files",
	run: func(t *testing.T, w world, s *steps) {
		yuki, ken := w.as("yuki"), w.as("ken")
		notes := []byte("Chapter 1 notes: a loop repeats.\n")
		essay := []byte("%PDF-1.4\n% an essay\n")
		graph := []byte("\x89PNG\r\n\x1a\nnot really a picture")

		// Uploads, and the PUT's answers.
		upNotes := uploadStep(t, w, s, yuki, "notes", "text/plain; charset=utf-8", notes)
		putStep(t, w, s, "put_again", upNotes, "text/plain; charset=utf-8", notes)
		upEssay := uploadStep(t, w, s, yuki, "essay", "application/pdf", nil)
		putStep(t, w, s, "put_wrong_type", upEssay, "text/html", essay)
		putStep(t, w, s, "put_essay", upEssay, "application/pdf", essay)
		upEmpty := uploadStep(t, w, s, yuki, "never_put", "application/pdf", nil)

		// The refusals of a question's files, each writing nothing.
		open := func(name string, attachments []map[string]any, body any) toolAnswer {
			args := inCourseArgs(w, "respondent_member_id", w.tutorSeat(), "attachments", attachments,
				"idempotency_key", "files:"+name+":"+w.course())
			if body != nil {
				args["body"] = body
			}
			return callAs(t, yuki, s, name, "conversation_open", args)
		}
		file := func(up uploaded, name string) map[string]any {
			return map[string]any{"upload_token": up.token, "filename": name}
		}
		open("files_without_body", []map[string]any{file(upNotes, "notes.txt")}, nil)
		open("bad_upload_token", []map[string]any{{"upload_token": "not-a-token", "filename": "notes.txt"}}, "Read this.")
		open("bad_filename", []map[string]any{file(upNotes, "../notes.txt")}, "Read this.")
		open("duplicate_attachment", []map[string]any{file(upNotes, "a.txt"), file(upNotes, "b.txt")}, "Read this.")
		open("not_uploaded", []map[string]any{file(upEmpty, "empty.pdf")}, "Read this.")
		callAs(t, ken, s, "not_your_upload", "conversation_open", inCourseArgs(w, "respondent_member_id", w.tutorSeat(), "body", "Mine.",
			"attachments", []map[string]any{file(upNotes, "notes.txt")}, "idempotency_key", "files:not_yours:"+w.course()))

		// The question, with two files, and a follow-up with a third.
		asked := open("open_with_files", []map[string]any{file(upNotes, "筆記 1.txt"), file(upEssay, "essay.pdf")},
			"Please read my notes and my essay.")
		wantStatus(t, asked, actExecuted)
		conv := asked.str("result", "conversation_id")
		open("already_attached", []map[string]any{file(upNotes, "notes.txt")}, "Again.")
		upGraph := uploadStep(t, w, s, yuki, "graph", "image/png", graph)
		followed := callAs(t, yuki, s, "ask_with_file", "conversation_ask", inCourseArgs(w, "conversation_id", conv, "body", "And my graph.",
			"attachments", []map[string]any{file(upGraph, "graph.png")}, "idempotency_key", "files:ask:"+w.course()))
		wantStatus(t, followed, actExecuted)
		followUp := followed.str("result", "message_id")

		// What the tutor reads of them.
		read := call(t, w, s, "messages", "conversation_messages", inCourseArgs(w, "conversation_id", conv))
		ids := attachmentIDs(read)
		if len(ids) != 3 {
			t.Fatalf("the messages list %d files, want 3: %s", len(ids), read.Body)
		}
		got := attachmentStep(t, w, s, w.agent(), "attachment", ids[0])
		downloadStep(t, w, s, "download", got.str("result", "download_url"))
		attachmentStep(t, w, s, w.agent(), "attachment_pdf", ids[1])
		attachmentStep(t, w, s, yuki, "attachment_by_its_uploader", ids[2])
		attachmentStep(t, w, s, w.agent(), "attachment_that_is_none", uuid.NewString())

		// Another student's conversation with the tutor: the tutor reads its
		// file, which names that conversation; the first student does not.
		upKen := uploadStep(t, w, s, ken, "kens", "text/plain", []byte("Ken's work.\n"))
		kens := callAs(t, ken, s, "ken_opens_with_file", "conversation_open", inCourseArgs(w, "respondent_member_id", w.tutorSeat(),
			"body", "Is this right?", "attachments", []map[string]any{file(upKen, "work.txt")}, "idempotency_key", "files:ken:"+w.course()))
		kenRead := call(t, w, s, "kens_messages", "conversation_messages", inCourseArgs(w, "conversation_id", kens.str("result", "conversation_id")))
		kenFile := attachmentIDs(kenRead)
		if len(kenFile) != 1 {
			t.Fatalf("Ken's conversation lists %d files: %s", len(kenFile), kenRead.Body)
		}
		attachmentStep(t, w, s, w.agent(), "tutor_reads_kens_file", kenFile[0])
		attachmentStep(t, w, s, yuki, "yuki_reads_kens_file", kenFile[0])

		// The follow-up retracted: its file is withheld, as its text is.
		callAs(t, yuki, s, "retract_follow_up", "conversation_retract", inCourseArgs(w, "message_id", followUp,
			"idempotency_key", "files:retract:"+w.course()))
		attachmentStep(t, w, s, w.agent(), "attachment_of_a_retracted_message", ids[2])
		call(t, w, s, "messages_after_the_retraction", "conversation_messages", inCourseArgs(w, "conversation_id", conv))

		// The news of the messages, to the tutor.
		ev := call(t, w, s, "events", "event_list", inCourseArgs(w, "since_seq", 0))
		keepConversationNews(s, ev)
	}}

// uploaded is an upload's token and the URL its file is PUT to.
type uploaded struct{ token, url string }

// uploadStep is a person asking for somewhere to upload a file, recorded
// with its URL and token as placeholders, and, when data is given, its
// file PUT.
func uploadStep(t *testing.T, w world, s *steps, c *mcpClient, name, contentType string, data []byte) uploaded {
	t.Helper()
	a := callAs(t, c, s, "upload_url_"+name, "conversation_upload_url", inCourseArgs(w, "content_type", contentType))
	wantStatus(t, a, actExecuted)
	up := uploaded{token: a.str("result", "upload_token"), url: a.str("result", "upload_url")}
	if !strings.HasPrefix(up.url, w.base()+blobPath) || up.token == "" {
		t.Fatalf("upload_url %q, upload_token %q", up.url, up.token)
	}
	placeholders(s, map[string]string{"upload_url": "<upload_url>", "upload_token": "<upload_token>"})
	if data != nil {
		putStep(t, w, s, "put_"+name, up, contentType, data)
	}
	return up
}

// putStep PUTs data to an upload's URL with contentType, as a browser
// does, with no Authorization header, and records the answer.
func putStep(t *testing.T, w world, s *steps, name string, up uploaded, contentType string, data []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, up.url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	a := doHTTP(t, req)
	s.http(name, a)
}

// attachmentStep is conversation_attachment as whoever c is, recorded with
// its download URL as a placeholder.
func attachmentStep(t *testing.T, w world, s *steps, c *mcpClient, name, id string) toolAnswer {
	t.Helper()
	a := callAs(t, c, s, name, "conversation_attachment", inCourseArgs(w, "attachment_id", id))
	if u := a.str("result", "download_url"); u != "" && !strings.HasPrefix(u, w.base()+blobPath) {
		t.Fatalf("download_url %q is not the server's", u)
	}
	placeholders(s, map[string]string{"download_url": "<download_url>"})
	return a
}

// downloadStep GETs a download URL as it is, with no Authorization header,
// and records its status and headers and a digest of what it served.
func downloadStep(t *testing.T, _ world, s *steps, name, url string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := doHTTP(t, req)
	sum := sha256.Sum256(a.Body)
	step := map[string]any{"step": name, "http_status": a.Status, "body_sha256": hex.EncodeToString(sum[:]), "body_bytes": len(a.Body)}
	for _, h := range []string{"Content-Type", "Content-Disposition", "Content-Length", "X-Content-Type-Options", "Content-Security-Policy",
		"Cache-Control"} {
		step[strings.ToLower(h)] = a.Header.Get(h)
	}
	s.list = append(s.list, step)
}

func doHTTP(t *testing.T, req *http.Request) httpAnswer {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", req.Method, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpAnswer{Status: resp.StatusCode, Header: resp.Header, Body: b}
}

// placeholders replaces fields of the last step's result by placeholders:
// what differs between two servers (their URLs, and the form of their
// tokens), not what the fake is held to.
func placeholders(s *steps, fields map[string]string) {
	step := s.list[len(s.list)-1]
	env, _ := step["envelope"].(map[string]any)
	res, _ := env["result"].(map[string]any)
	if res == nil {
		return
	}
	// A copy: the answer's own map is the caller's still.
	env, res = maps.Clone(env), maps.Clone(res)
	for k, v := range fields {
		if _, ok := res[k]; ok {
			res[k] = v
		}
	}
	env["result"] = res
	step["envelope"] = env
}

// attachmentIDs are the ids of every file the messages of a
// conversation_messages answer list, in order.
func attachmentIDs(a toolAnswer) []string {
	res, _ := a.Structured["result"].(map[string]any)
	msgs, _ := res["messages"].([]any)
	var out []string
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		files, _ := mm["attachments"].([]any)
		for _, f := range files {
			ff, _ := f.(map[string]any)
			if id, ok := ff["id"].(string); ok {
				out = append(out, id)
			}
		}
	}
	return out
}

// keepConversationNews keeps, of the last step's event_list, the news of
// conversations alone: the course's own, made as it was set up, differ
// between a real Core and the fake.
func keepConversationNews(s *steps, a toolAnswer) {
	step := s.list[len(s.list)-1]
	res, _ := a.Structured["result"].(map[string]any)
	evs, _ := res["events"].([]any)
	var kept []any
	for _, e := range evs {
		em, _ := e.(map[string]any)
		if typ, _ := em["type"].(string); strings.HasPrefix(typ, "conversation.") {
			kept = append(kept, em)
		}
	}
	step["envelope"] = map[string]any{"status": a.Structured["status"], "conversation_events": kept}
}
