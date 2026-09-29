package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
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
// returns the body's result. A 429 is waited out, under a Core started with
// a small limit.
func (c *liveREST) call(want int, method, path, token string, body any) map[string]any {
	c.t.Helper()
	c.n++
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			c.t.Fatal(err)
		}
	}
	key := fmt.Sprintf("%s-%d", c.run, c.n)
	for range 20 {
		status, raw, retryAfter := c.send(method, path, token, key, b)
		if status == http.StatusTooManyRequests {
			time.Sleep(retryAfter)
			continue
		}
		if status != want {
			c.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, status, want, redact(string(raw), token))
		}
		var out struct {
			Result map[string]any `json:"result"`
		}
		_ = json.Unmarshal(raw, &out)
		return out.Result
	}
	c.t.Fatalf("%s %s: refused as too many calls twenty times", method, path)
	return nil
}

func (c *liveREST) send(method, path, token, key string, body []byte) (status int, raw []byte, retryAfter time.Duration) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ = io.ReadAll(resp.Body)
	return resp.StatusCode, raw, retryAfterHeader(resp.Header.Get("Retry-After"), time.Now())
}

// person is registrar registering a person, with the fields more adds
// (a platform role, say), who then signs in with a password as people do:
// people hold no API tokens, only agents do. They are given an email of
// this run's, invited to choose a password (actor.invite), choose one as
// the front end's page for invitations does (POST /v1/auth/invite), and
// sign in with it (POST /v1/auth/login). It returns their actor's id and
// that session, which Core takes as a bearer token.
func (c *liveREST) person(registrar, name string, more map[string]any) (id, session string) {
	c.t.Helper()
	c.n++
	email := fmt.Sprintf("person-%s-%d@live.test", c.run, c.n)
	body := map[string]any{"kind": "human", "display_name": name, "email": email}
	maps.Copy(body, more)
	id = str(c.t, c.call(200, "POST", "/v1/actors", registrar, body), "actor_id")
	invite := str(c.t, c.call(200, "POST", "/v1/actors/"+id+"/invite", registrar, map[string]any{}), "token")
	password := rand.Text()
	c.session("/v1/auth/invite", map[string]string{"token": invite, "password": password})
	return id, c.session("/v1/auth/login", map[string]string{"email": email, "password": password})
}

// session posts body to path, where Core signs someone in, and returns the
// session Core sets as its cookie. A 429 is waited out.
func (c *liveREST) session(path string, body map[string]string) string {
	c.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		c.t.Fatal(err)
	}
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
		if err != nil {
			cancel()
			c.t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			c.t.Fatalf("POST %s: %v", path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusTooManyRequests {
			time.Sleep(retryAfterHeader(resp.Header.Get("Retry-After"), time.Now()))
			continue
		}
		for _, ck := range resp.Cookies() {
			if ck.Name == "ais_session" && ck.Value != "" {
				return ck.Value
			}
		}
		c.t.Fatalf("POST %s: HTTP %d, and no session: %s", path, resp.StatusCode, raw)
	}
	c.t.Fatalf("POST %s: refused as too many sign-ins twenty times", path)
	return ""
}

// liveCore is the Core under test, or a skip when there is none. root is
// E2E_ROOT_TOKEN: the session root signed in with (scripts/ci-core.sh), not
// an API token, since people hold none.
func liveCore(t *testing.T) (base, root, run string) {
	t.Helper()
	base, root = strings.TrimRight(os.Getenv("E2E_CORE_URL"), "/"), os.Getenv("E2E_ROOT_TOKEN")
	if base == "" || root == "" {
		t.Skip("E2E_CORE_URL and E2E_ROOT_TOKEN are not set: no Core to test against")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	return base, root, hex.EncodeToString(suffix)
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
// The calls go through Retrying, which leaves every envelope as it came, so
// that a Core started with a small limit is waited out.
func TestLiveContract(t *testing.T) {
	base, root, run := liveCore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rest := &liveREST{t: t, base: base, run: "live-" + run}

	// Root makes an admin; the admin registers Sato (an instructor) and Yuki
	// (a student), and opens a course with Sato in it. Each of them signs in
	// with a password (person).
	_, admin := rest.person(root, "Admin "+run, map[string]any{"platform_role": "admin"})
	satoID, sato := rest.person(admin, "Sato "+run, nil)
	yukiID, yuki := rest.person(admin, "Yuki "+run, nil)
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
	var tooMany atomic.Int32
	retry := RetryOptions{OnRateLimited: func(time.Duration) { tooMany.Add(1) }}
	mcpR, rstR := NewRetrying(mcp, retry), NewRetrying(rst, retry)
	defer func() { t.Logf("Core refused %d calls as too many, and Retrying waited each out", tooMany.Load()) }()

	// The first call initializes, through Retrying.
	if _, err := mcpR.Call(ctx, "me_get", nil); err != nil {
		t.Fatal(err)
	}
	if mcp.Protocol() != DefaultProtocol || !strings.Contains(mcp.Instructions(), "me_memberships") {
		t.Fatalf("protocol %q, instructions %.80q", mcp.Protocol(), mcp.Instructions())
	}
	var listed []MCPTool
	for {
		var rl *RateLimitedError
		if listed, err = mcp.ListTools(ctx); !errors.As(err, &rl) {
			break
		}
		time.Sleep(rl.RetryAfter)
	}
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
		a, err := mcpR.Call(ctx, tool, raw)
		if err != nil {
			t.Fatalf("%s over MCP: %v", tool, err)
		}
		b, err := rstR.Call(ctx, tool, raw)
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
	posted, err := mcpR.Call(ctx, "conversation_answer", first)
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
	for _, c := range []Caller{mcpR, rstR} {
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

	second, err := mcpR.Call(ctx, "conversation_answer", answerArgs(2, "A second answer."))
	if err != nil {
		t.Fatal(err)
	}
	third, err := rstR.Call(ctx, "conversation_answer", answerArgs(3, "A third answer."))
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
	// Arguments Core refuses, refused alike, word for word: those the route
	// cannot carry exactly go to the generic route, and a path parameter is
	// escaped into one segment.
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"conversation_inbox", map[string]any{"course_id": course, "limit": "5"}},
		{"conversation_list", map[string]any{"course_id": course, "after": 5}},
		{"conversation_get", map[string]any{"course_id": "a/b c", "conversation_id": conv}},
		{"document_get", map[string]any{"course_id": course, "document_id": ".."}},
		{"conversation_messages", map[string]any{"course_id": course, "conversation_id": conv, "limit": nil}},
	} {
		if e := both(c.tool, c.args); e.Status != StatusError || e.Code() != CodeInvalidArgument {
			t.Errorf("%s %v: %s", c.tool, c.args, show(e))
		}
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
	for _, c := range []Caller{mcp, rst, mcpR, rstR} {
		_, err := c.Call(ctx, "me_get", nil)
		if !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%T with a revoked token: %v", c, err)
		}
		if strings.Contains(err.Error(), tutor) {
			t.Fatal("the error carries the token")
		}
	}
}

// TestLiveRateLimited spends an actor's allowance until Core refuses a call
// with a real 429, over each transport, and then sees Retrying wait one
// out: told of it, asleep for Retry-After, and the call made. It needs a
// Core started with a limit, whose calls a minute CORE_RATE_LIMIT_PER_MINUTE
// names, as scripts/ci-core.sh takes it (with RATE_LIMIT_BURST, if set,
// passed on to Core).
func TestLiveRateLimited(t *testing.T) {
	base, root, run := liveCore(t)
	perMinute, _ := strconv.Atoi(os.Getenv("CORE_RATE_LIMIT_PER_MINUTE"))
	if perMinute <= 0 {
		t.Skip("CORE_RATE_LIMIT_PER_MINUTE does not name the limit Core was started with")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rest := &liveREST{t: t, base: base, run: "limited-" + run}
	// An actor of its own: nobody else spends its allowance, and it spends
	// nobody else's. An agent, since only agents are given API tokens.
	id := str(t, rest.call(200, "POST", "/v1/actors", root, map[string]any{"kind": "agent", "display_name": "Busy " + run}), "actor_id")
	token := str(t, rest.call(200, "POST", "/v1/actors/"+id+"/tokens", root, map[string]any{"label": "live"}), "token")
	cat, err := FetchCatalogue(ctx, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	const most = 20000 // calls, far past any burst a test Core is given
	for _, tc := range []struct {
		name string
		c    Caller
	}{
		{"MCP", NewMCPCaller(MCPOptions{BaseURL: base, Token: token})},
		{"REST", NewRESTCaller(RESTOptions{BaseURL: base, Token: token, Catalogue: cat})},
	} {
		var rl *RateLimitedError
		for n := 0; !errors.As(err, &rl); n++ {
			if n == most {
				t.Fatalf("%s: no 429 in %d calls", tc.name, most)
			}
			var env *Envelope
			if env, err = tc.c.Call(ctx, "me_get", nil); err == nil && !env.OK() {
				t.Fatalf("%s: %s", tc.name, show(env))
			} else if err != nil && !errors.As(err, &rl) {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		if rl.RetryAfter < time.Second || rl.RetryAfter > time.Minute {
			t.Fatalf("%s: Retry-After %s", tc.name, rl.RetryAfter)
		}
		noSecret(t, err)
		err = nil

		// Through Retrying, calls go on until one meets a 429 of its own,
		// which it waits out: every call comes back executed.
		var told []time.Duration
		r := NewRetrying(tc.c, RetryOptions{OnRateLimited: func(d time.Duration) { told = append(told, d) }})
		for n := 0; len(told) == 0; n++ {
			if n == most {
				t.Fatalf("%s: no 429 through Retrying in %d calls", tc.name, most)
			}
			start := time.Now()
			env, err := r.Call(ctx, "me_get", nil)
			if err != nil || !env.OK() {
				t.Fatalf("%s through Retrying: %v %v", tc.name, env, err)
			}
			if len(told) > 0 && time.Since(start) < told[0] {
				t.Fatalf("%s: Retrying came back after %s, before Retry-After %s", tc.name, time.Since(start), told[0])
			}
		}
		t.Logf("%s: a 429 with Retry-After %s; Retrying was told %v and waited it out", tc.name, rl.RetryAfter, told)
	}
}
