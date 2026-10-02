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
	"slices"
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
// returns the body's result.
func (c *liveREST) call(want int, method, path, token string, body any) map[string]any {
	c.t.Helper()
	status, raw := c.do(method, path, token, body)
	if status != want {
		c.t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, status, want, redact(string(raw), token))
	}
	var out struct {
		Result map[string]any `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Result
}

// refused makes one request that Core must refuse, and returns its
// envelope, whatever the HTTP status: the test fails if it was executed.
func (c *liveREST) refused(method, path, token string, body any) *Envelope {
	c.t.Helper()
	status, raw := c.do(method, path, token, body)
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Status == StatusExecuted || env.Error == nil {
		c.t.Fatalf("%s %s: HTTP %d, want a refusal: %s", method, path, status, redact(string(raw), token))
	}
	return &env
}

// do makes one request under a key of its own, and returns Core's answer.
// A 429 is waited out, under a Core started with a small limit.
func (c *liveREST) do(method, path, token string, body any) (int, []byte) {
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
		if status != http.StatusTooManyRequests {
			return status, raw
		}
		time.Sleep(retryAfter)
	}
	c.t.Fatalf("%s %s: refused as too many calls twenty times", method, path)
	return 0, nil
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

// runtimeService is the site's agent runtime's client of Core (runtime.go),
// with a credential of the agent_runtime service's own, which root issues
// for the test and revokes after it. It replaces none: whatever else holds
// one on this Core keeps it.
func (c *liveREST) runtimeService(root string) *RuntimeService {
	c.t.Helper()
	issued := c.call(200, "POST", "/v1/services/agent_runtime/credentials", root, map[string]any{"label": "live " + c.run})
	credential, id := str(c.t, issued, "token"), str(c.t, issued, "credential_id")
	if !strings.HasPrefix(credential, ServiceTokenPrefix) {
		c.t.Fatalf("Core issued the agent runtime no credential (%s…): is it older than AIShie-Core #52?", ServiceTokenPrefix)
	}
	c.t.Cleanup(func() {
		c.call(200, "POST", "/v1/services/agent_runtime/credentials/"+id+"/revoke", root, map[string]any{})
	})
	return NewRuntimeService(RuntimeCaller(RuntimeOptions{BaseURL: c.base,
		Credential: func(context.Context) (string, error) { return credential, nil }}))
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
// tutor seated over REST as Core's own scripts/e2e.sh seats one, hosted by
// its id as the site's agent runtime hosts it (AIShie-Core #52: issued its
// one token with the agent_runtime service's credential, and asked nothing
// before, nor after the token is revoked; an mcp agent never asked, nor
// hosted), a student's question, and the calls a runtime makes, over MCP
// and over REST, which must give the same envelopes. It runs only against a
// throwaway Core (scripts/ci-core.sh start, or make live-core), named by
// E2E_CORE_URL and E2E_ROOT_TOKEN. The calls go through Retrying, which
// leaves every envelope as it came, so that a Core started with a small
// limit is waited out.
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

	// Sato's own agent, the course's tutor, is hosted by the site's agent
	// runtime by its id (AIShie-Core #52): Sato holds no token of it, and
	// nobody asks it in the site until the runtime is issued its one token.
	svc := rest.runtimeService(root)
	tutorID := str(t, rest.call(200, "POST", "/v1/me/agents", sato, map[string]any{"display_name": "CS101 Tutor", "hosting": HostingRuntime}), "actor_id")
	if e := rest.refused("POST", "/v1/me/agents/"+tutorID+"/tokens", sato, map[string]any{"label": "mine"}); e.Reason() != "hosted_by_runtime" {
		t.Fatalf("Sato issued a token of his runtime agent: %s", show(e))
	}
	tutorM := str(t, rest.call(200, "POST", C+"/delegates", sato, map[string]any{"actor_id": tutorID, "preset": "course_tutor"}), "member_id")
	ask := func(respondent, body string) *Envelope {
		return rest.refused("POST", C+"/conversations", yuki, map[string]any{"respondent_member_id": respondent, "body": body})
	}
	if e := ask(tutorM, "Anyone there?"); e.Code() != CodeFailedPrecondition || e.Reason() != "agent_not_hosted" {
		t.Fatalf("a question to the tutor before the runtime hosts it: %s", show(e))
	}
	if a, err := svc.Agent(ctx, tutorID); err != nil || a.Hosting != HostingRuntime || !a.Hostable || a.RuntimeToken != nil || a.SiteChat ||
		a.OwnerActorID != satoID || a.LiveSeats != 1 {
		t.Fatalf("the tutor before it is hosted: %+v %v", a, err)
	}
	if owns, a, err := svc.CheckOwner(ctx, satoID, tutorID); err != nil || !owns || a.AgentID != tutorID {
		t.Fatalf("Sato owns the tutor: %v %+v %v", owns, a, err)
	}
	if owns, a, err := svc.CheckOwner(ctx, yukiID, tutorID); err != nil || owns || a != nil {
		t.Fatalf("Yuki owns the tutor: %v %+v %v", owns, a, err)
	}
	issued, err := svc.IssueToken(ctx, tutorID, "")
	if err != nil {
		t.Fatal(err)
	}
	tutor := issued.Token
	if a, err := svc.Agent(ctx, tutorID); err != nil || a.RuntimeToken == nil || a.RuntimeToken.CredentialID != issued.CredentialID || !a.SiteChat {
		t.Fatalf("the tutor once hosted: %+v %v", a, err)
	}

	// An agent of Sato's own tools (hosting mcp) is never the runtime's,
	// and nobody asks it in the site.
	scriptsID := str(t, rest.call(200, "POST", "/v1/me/agents", sato, map[string]any{"display_name": "Sato's scripts", "hosting": HostingMCP}), "actor_id")
	scriptsM := str(t, rest.call(200, "POST", C+"/delegates", sato, map[string]any{"actor_id": scriptsID, "preset": "course_tutor"}), "member_id")
	if _, err := svc.IssueToken(ctx, scriptsID, ""); !IsReason(err, ReasonNotRuntimeHosted) {
		t.Fatalf("the runtime issued a token of an mcp agent: %v", err)
	}
	if e := ask(scriptsM, "Anyone there?"); e.Code() != CodeFailedPrecondition || e.Reason() != "mcp_agent" {
		t.Fatalf("a question to an mcp agent: %s", show(e))
	}
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
	// tools/list offers an agent every tool of the catalogue but the site
	// services' own, which Core serves over REST alone, to their own
	// credentials: the agent runtime's hosting and renditions, and the
	// transcriber's queue.
	restOnly := []string{ToolRuntimeAgent, ToolRuntimeCheckOwner, ToolRuntimeIssueToken, ToolRuntimeRevokeToken,
		ToolRenditionClaim, ToolRenditionFile, ToolRenditionRenew, ToolRenditionUploadURL, ToolRenditionComplete,
		ToolTextQueue, ToolTextFile, ToolTextRenew, ToolTextComplete}
	offered := map[string]bool{}
	for _, tl := range listed {
		offered[tl.Name] = true
		if _, ok := cat.Tool(tl.Name); !ok {
			t.Errorf("tools/list offers %s, which the catalogue lacks", tl.Name)
		}
		if slices.Contains(restOnly, tl.Name) {
			t.Errorf("tools/list offers %s, a site service's, to an agent", tl.Name)
		}
	}
	for _, name := range restOnly {
		if _, ok := cat.Tool(name); !ok {
			t.Errorf("the catalogue lacks %s", name)
		}
	}
	for _, tl := range cat.Tools() {
		if !offered[tl.MCPName] && !slices.Contains(restOnly, tl.MCPName) {
			t.Errorf("tools/list does not offer %s", tl.MCPName)
		}
	}
	if len(listed) != cat.Len()-len(restOnly) {
		t.Errorf("tools/list offers %d tools, GET /v1/tools %d, of which %d are the services' over REST alone", len(listed), cat.Len(), len(restOnly))
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
	if err := both("me_get", struct{}{}).Decode(&me); err != nil || me.ID != tutorID || me.Kind != "agent" || me.Hosting != HostingRuntime {
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

	// 401: a token that was never issued, then the tutor's own, revoked by
	// the runtime as it stops hosting the tutor. Sato sees it listed as the
	// runtime's, live and then revoked, and Yuki can ask the tutor nothing
	// more.
	for _, c := range []Caller{
		NewMCPCaller(MCPOptions{BaseURL: base, Token: "ais_bogus_" + run}),
		NewRESTCaller(RESTOptions{BaseURL: base, Token: "ais_bogus_" + run, Catalogue: cat}),
	} {
		if _, err := c.Call(ctx, "me_get", nil); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%T with a bogus token: %v", c, err)
		}
	}
	// tutorCredential is the tutor's one credential as Sato sees it: the
	// runtime's, and revoked or not as revoked says.
	tutorCredential := func(revoked bool) {
		t.Helper()
		var creds struct {
			Credentials []struct {
				ID        string  `json:"id"`
				IssuedTo  *string `json:"issued_to"`
				RevokedAt *string `json:"revoked_at"`
			} `json:"credentials"`
		}
		raw, _ := json.Marshal(rest.call(200, "GET", "/v1/me/agents/"+tutorID+"/credentials", sato, nil))
		if err := json.Unmarshal(raw, &creds); err != nil || len(creds.Credentials) != 1 || creds.Credentials[0].ID != issued.CredentialID ||
			creds.Credentials[0].IssuedTo == nil || *creds.Credentials[0].IssuedTo != "agent_runtime" ||
			(creds.Credentials[0].RevokedAt != nil) != revoked {
			t.Fatalf("the tutor's credentials (revoked %v): %s %v", revoked, raw, err)
		}
	}
	tutorCredential(false)
	if revoked, err := svc.RevokeToken(ctx, tutorID); err != nil || len(revoked) != 1 || revoked[0] != issued.CredentialID {
		t.Fatalf("the runtime revoked the tutor's token: %v %v", revoked, err)
	}
	tutorCredential(true)
	if revoked, err := svc.RevokeToken(ctx, tutorID); err != nil || len(revoked) != 0 {
		t.Fatalf("the runtime revoked the tutor's token again: %v %v", revoked, err)
	}
	for _, c := range []Caller{mcp, rst, mcpR, rstR} {
		_, err := c.Call(ctx, "me_get", nil)
		if !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%T with a revoked token: %v", c, err)
		}
		if strings.Contains(err.Error(), tutor) {
			t.Fatal("the error carries the token")
		}
	}
	if e := ask(tutorM, "Are you still there?"); e.Code() != CodeFailedPrecondition || e.Reason() != "agent_not_hosted" {
		t.Fatalf("a question to the tutor once the runtime stopped hosting it: %s", show(e))
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
	// nobody else's. An agent, since only agents are given API tokens, and
	// an mcp one, whose tokens are issued to whoever runs it, not to the
	// site's agent runtime alone.
	id := str(t, rest.call(200, "POST", "/v1/actors", root, map[string]any{"kind": "agent", "display_name": "Busy " + run, "hosting": HostingMCP}), "actor_id")
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
