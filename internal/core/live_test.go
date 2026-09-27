package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/ratelimit"
)

// liveREST is Core's REST API as the tests' people use it: plain requests,
// apart from the code under test.
type liveREST struct {
	t    *testing.T
	base string
	run  string
	n    int
}

// call makes one request and fails the test unless Core answers want. It
// returns the body's result.
func (c *liveREST) call(want int, method, path, token string, body any) map[string]any {
	c.t.Helper()
	c.n++
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", c.run+"-"+string(rune('a'+c.n%26))+"-"+time.Now().Format("150405.000000"))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, resp.StatusCode, want, redact(string(raw), token))
	}
	var out struct {
		Result map[string]any `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Result
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok || s == "" {
		t.Fatalf("no %s in %v", key, m)
	}
	return s
}

// sameEnvelope compares two envelopes field by field, results as JSON
// values rather than bytes.
func sameEnvelope(a, b *Envelope) bool {
	if a.Status != b.Status || a.ActionID != b.ActionID || a.ReviewState != b.ReviewState ||
		a.Replayed != b.Replayed || a.Note != b.Note || !reflect.DeepEqual(a.Error, b.Error) {
		return false
	}
	var ra, rb any
	_ = json.Unmarshal(a.Result, &ra)
	_ = json.Unmarshal(b.Result, &rb)
	return reflect.DeepEqual(ra, rb)
}

func show(e *Envelope) string {
	b, _ := json.Marshal(e)
	return string(b)
}

// TestLiveContract drives both transports through a real Core: a course
// tutor seated over REST as Core's own scripts/e2e.sh seats one, a student's
// question, and the calls a runtime makes, over MCP and over REST, which
// must give the same envelopes. It runs only against a throwaway Core
// (scripts/ci-core.sh start), named by E2E_CORE_URL and E2E_ROOT_TOKEN.
func TestLiveContract(t *testing.T) {
	base, root := strings.TrimRight(os.Getenv("E2E_CORE_URL"), "/"), os.Getenv("E2E_ROOT_TOKEN")
	if base == "" || root == "" {
		t.Skip("E2E_CORE_URL and E2E_ROOT_TOKEN are not set: no Core to test against")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	run := hex.EncodeToString(suffix)
	rest := &liveREST{t: t, base: base, run: "live-" + run}

	// Root makes an admin; the admin registers Sato (an instructor) and Yuki
	// (a student), and opens a course with Sato in it.
	adminID := str(t, rest.call(200, "POST", "/v1/actors", root, map[string]any{"kind": "human", "display_name": "Admin " + run, "platform_role": "admin"}), "actor_id")
	admin := str(t, rest.call(200, "POST", "/v1/actors/"+adminID+"/tokens", root, map[string]any{"label": "live"}), "token")
	register := func(name string) (id, token string) {
		id = str(t, rest.call(200, "POST", "/v1/actors", admin, map[string]any{"kind": "human", "display_name": name}), "actor_id")
		return id, str(t, rest.call(200, "POST", "/v1/actors/"+id+"/tokens", admin, map[string]any{"label": "live"}), "token")
	}
	satoID, sato := register("Sato " + run)
	yukiID, yuki := register("Yuki " + run)
	term := str(t, rest.call(200, "POST", "/v1/terms", admin, map[string]any{"name": "Term " + run, "starts_on": "2026-09-01", "ends_on": "2026-12-20"}), "id")
	dept := str(t, rest.call(200, "POST", "/v1/departments", admin, map[string]any{"name": "Computing " + run}), "id")
	course := str(t, rest.call(200, "POST", "/v1/courses", admin, map[string]any{
		"dept_id": dept, "term_id": term, "code": "CS" + run, "section": "A", "title": "Introduction to Computing"}), "course_id")
	C := "/v1/courses/" + course
	rest.call(200, "POST", C+"/activate", admin, map[string]any{})
	rest.call(200, "POST", C+"/instructors", admin, map[string]any{"actor_id": satoID})
	rest.call(200, "POST", C+"/members", sato, map[string]any{"actor_id": yukiID, "preset": "student"})

	// Sato's own agent, seated as the course's tutor; Yuki asks it.
	tutorID := str(t, rest.call(200, "POST", "/v1/me/agents", sato, map[string]any{"display_name": "CS101 Tutor"}), "actor_id")
	tutor := str(t, rest.call(200, "POST", "/v1/me/agents/"+tutorID+"/tokens", sato, map[string]any{"label": "runtime"}), "token")
	tutorM := str(t, rest.call(200, "POST", C+"/delegates", sato, map[string]any{"actor_id": tutorID, "preset": "course_tutor"}), "member_id")
	conv := str(t, rest.call(200, "POST", C+"/conversations", yuki, map[string]any{"respondent_member_id": tutorM, "body": "What does HW3 ask for?"}), "conversation_id")

	cat, err := FetchCatalogue(ctx, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := testCatalogue(t)
	if cat.Hash() != snapshot.Hash() {
		t.Errorf("Core's catalogue hashes %s; the snapshot in testdata, %s", cat.Hash(), snapshot.Hash())
	}
	mcp := NewMCPCaller(MCPOptions{BaseURL: base, Token: tutor})
	rst := NewRESTCaller(RESTOptions{BaseURL: base, Token: tutor, Catalogue: cat})

	if err := mcp.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if mcp.Protocol() != DefaultProtocol || !strings.Contains(mcp.Instructions(), "me_memberships") {
		t.Fatalf("protocol %q, instructions %.80q", mcp.Protocol(), mcp.Instructions())
	}
	listed, err := mcp.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != cat.Len() {
		t.Errorf("tools/list offers %d tools, GET /v1/tools %d", len(listed), cat.Len())
	}
	for _, tl := range listed {
		if _, ok := cat.Tool(tl.Name); !ok {
			t.Errorf("tools/list offers %s, which the catalogue lacks", tl.Name)
		}
	}

	// both makes a call over each transport; the envelopes must be the same.
	both := func(tool string, args any) *Envelope {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		a, err := mcp.Call(ctx, tool, raw)
		if err != nil {
			t.Fatalf("%s over MCP: %v", tool, err)
		}
		b, err := rst.Call(ctx, tool, raw)
		if err != nil {
			t.Fatalf("%s over REST: %v", tool, err)
		}
		if !sameEnvelope(a, b) {
			t.Errorf("%s: MCP and REST differ:\nMCP  %s\nREST %s", tool, show(a), show(b))
		}
		return a
	}

	var me Actor
	if err := both("me_get", struct{}{}).Decode(&me); err != nil || me.ID != tutorID || me.Kind != "agent" {
		t.Fatalf("me_get: %+v %v", me, err)
	}
	var seats struct {
		Memberships []Membership `json:"memberships"`
	}
	if err := both("me_memberships", struct{}{}).Decode(&seats); err != nil || len(seats.Memberships) != 1 {
		t.Fatalf("me_memberships: %+v %v", seats, err)
	}
	if s := seats.Memberships[0]; s.MemberID != tutorM || s.CourseID != course || !s.AnswersCourse || !s.Answers() {
		t.Fatalf("the seat: %+v", s)
	}
	var inbox Inbox
	if err := both("conversation_inbox", map[string]any{"course_id": course}).Decode(&inbox); err != nil || len(inbox.Conversations) != 1 {
		t.Fatalf("conversation_inbox: %+v %v", inbox, err)
	}
	row := inbox.Conversations[0]
	if row.ID != conv || row.LatestOpenerMessageID == nil {
		t.Fatalf("the inbox row: %+v", row)
	}
	question := *row.LatestOpenerMessageID
	var msgs Messages
	if err := both("conversation_messages", map[string]any{"course_id": course, "conversation_id": conv, "limit": 30}).Decode(&msgs); err != nil ||
		len(msgs.Messages) != 1 || msgs.Messages[0].ID != question || msgs.Messages[0].Body == nil || *msgs.Messages[0].Body != "What does HW3 ask for?" {
		t.Fatalf("conversation_messages: %+v %v", msgs, err)
	}
	// The typed client, over the stack the worker uses.
	stack := NewClient(NewRetrying(Limited(mcp, ratelimit.New(ratelimit.CoreShare*600, 90)), RetryOptions{}))
	if got, err := stack.Messages(WithPriority(ctx, PriorityAnswer), course, conv, MessagesQuery{Limit: 30}); err != nil || len(got.Messages) != 1 {
		t.Fatalf("Client.Messages: %+v %v", got, err)
	}

	// Answering: executed, then replayed over either transport, then the
	// same key with another body, then a second answer to the same message.
	answerArgs := func(attempt int, body string) []byte {
		b, err := json.Marshal(AnswerArgs{CourseID: course, ConversationID: conv, InReplyToMessageID: question, Body: body,
			IdempotencyKey: AnswerKey(conv, question, attempt)})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first := answerArgs(1, "An essay with a thesis.")
	posted, err := mcp.Call(ctx, "conversation_answer", first)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		MessageID string `json:"message_id"`
	}
	if posted.Status != StatusExecuted || posted.ReviewState != ReviewNone || posted.Replayed || posted.ActionID == "" ||
		posted.Decode(&result) != nil || result.MessageID == "" {
		t.Fatalf("the answer: %s", show(posted))
	}
	for _, c := range []Caller{mcp, rst} {
		again, err := c.Call(ctx, "conversation_answer", first)
		if err != nil {
			t.Fatal(err)
		}
		if again.Status != StatusExecuted || !again.Replayed || again.ActionID != posted.ActionID {
			t.Fatalf("%T replay: %s", c, show(again))
		}
	}
	both("conversation_answer", json.RawMessage(first))

	conflict := both("conversation_answer", json.RawMessage(answerArgs(1, "Another body under the same key.")))
	if conflict.Status != StatusError || conflict.Code() != CodeIdempotencyConflict || conflict.Detail("action_id") != posted.ActionID || conflict.ActionID != "" {
		t.Fatalf("idempotency_conflict: %s", show(conflict))
	}

	second, err := mcp.Call(ctx, "conversation_answer", answerArgs(2, "A second answer."))
	if err != nil {
		t.Fatal(err)
	}
	third, err := rst.Call(ctx, "conversation_answer", answerArgs(3, "A third answer."))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Envelope{second, third} {
		if e.Status != StatusFailed || e.Code() != CodeConflict || e.Reason() != ReasonAlreadyAnswered || e.ActionID == "" || e.Replayed {
			t.Fatalf("a second answer: %s", show(e))
		}
	}
	if replay := both("conversation_answer", json.RawMessage(answerArgs(2, "A second answer."))); replay.Status != StatusFailed ||
		!replay.Replayed || replay.ActionID != second.ActionID || replay.Reason() != ReasonAlreadyAnswered {
		t.Fatalf("the second answer replayed: %s", show(replay))
	}
	var empty Inbox
	if err := both("conversation_inbox", map[string]any{"course_id": course, "limit": 5}).Decode(&empty); err != nil || len(empty.Conversations) != 0 {
		t.Fatalf("the inbox after the answer: %+v %v", empty, err)
	}
	notFound := both("conversation_get", map[string]any{"course_id": course, "conversation_id": "01a0e201-0000-7000-8000-000000000000"})
	if notFound.Status != StatusError || notFound.Code() != CodeNotFound {
		t.Fatalf("no such conversation: %s", show(notFound))
	}

	// The query string's forms, and the generic route, read back by Core
	// as MCP carries them.
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"event_list", map[string]any{"course_id": course, "since_seq": 0, "limit": 10}},
		{"action_list_mine", map[string]any{"course_id": course, "exclude_types": []string{"conversation.ask", "conversation.open"}}},
		{"action_list_mine", map[string]any{"course_id": course, "exclude_types": []string{}}},
		{"document_list", map[string]any{"course_id": course, "include_archived": true}},
		{"conversation_messages", map[string]any{"course_id": course, "conversation_id": conv, "after_seq": nil, "before_seq": 1000, "limit": 30}},
	} {
		if e := both(c.tool, c.args); e.Status != StatusExecuted {
			t.Errorf("%s %v: %s", c.tool, c.args, show(e))
		}
	}
	if e := both("conversation_inbox", map[string]any{"course_id": course, "limit": "5"}); e.Status != StatusError || e.Code() != CodeInvalidArgument {
		t.Errorf("a limit given as text: %s", show(e))
	}

	// 401: a token that was never issued, then the tutor's own, revoked.
	for _, c := range []Caller{
		NewMCPCaller(MCPOptions{BaseURL: base, Token: "ais_bogus_" + run}),
		NewRESTCaller(RESTOptions{BaseURL: base, Token: "ais_bogus_" + run, Catalogue: cat}),
	} {
		if _, err := c.Call(ctx, "me_get", nil); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%T with a bogus token: %v", c, err)
		}
	}
	var creds struct {
		Credentials []struct {
			ID string `json:"id"`
		} `json:"credentials"`
	}
	raw, _ := json.Marshal(rest.call(200, "GET", "/v1/me/agents/"+tutorID+"/credentials", sato, nil))
	if err := json.Unmarshal(raw, &creds); err != nil || len(creds.Credentials) != 1 {
		t.Fatalf("the tutor's credentials: %s %v", raw, err)
	}
	rest.call(200, "POST", "/v1/me/agents/"+tutorID+"/credentials/"+creds.Credentials[0].ID+"/revoke", sato, map[string]any{})
	for _, c := range []Caller{mcp, rst, NewRetrying(rst, RetryOptions{})} {
		_, err := c.Call(ctx, "me_get", nil)
		if !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%T with a revoked token: %v", c, err)
		}
		if strings.Contains(err.Error(), tutor) {
			t.Fatal("the error carries the token")
		}
	}
}
