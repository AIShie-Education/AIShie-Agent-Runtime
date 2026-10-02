package fakecore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake tested directly: what its fixtures do not reach, and the test
// controls themselves.

// clock is a clock a test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func mustCall(t *testing.T, c *mcpClient, tool string, args map[string]any) toolAnswer {
	t.Helper()
	a, err := c.call(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if a.Status != http.StatusOK || a.RPCError != nil {
		t.Fatalf("%s: HTTP %d %s", tool, a.Status, a.Body)
	}
	return a
}

func wantEnvelope(t *testing.T, a toolAnswer, status, code, reason string) {
	t.Helper()
	if a.status() != status || a.str("error", "code") != code || a.str("error", "details", "reason") != reason {
		t.Fatalf("got %s/%s/%s, want %s/%s/%s: %s", a.status(), a.str("error", "code"), a.str("error", "details", "reason"),
			status, code, reason, a.Text)
	}
}

func list(a toolAnswer, key string) []any {
	res, _ := a.Structured["result"].(map[string]any)
	l, _ := res[key].([]any)
	return l
}

func TestCatalogueSnapshot(t *testing.T) {
	core, err := os.ReadFile("../core/testdata/catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(core, catalogueJSON) {
		t.Fatal("testdata/catalogue.json is not internal/core/testdata/catalogue.json: copy it again, and record the fixtures again")
	}
	cat, err := loadCatalogue(catalogueJSON)
	if err != nil {
		t.Fatal(err)
	}
	reads, writes, ephemeral := 0, 0, 0
	for _, tl := range cat.tools {
		switch {
		case tl.write:
			writes++
		case tl.ephemeral:
			ephemeral++
		default:
			reads++
		}
	}
	if len(cat.tools) != 167 || reads != 63 || writes != 99 || ephemeral != 5 {
		t.Errorf("%d tools, %d reads, %d writes, %d ephemeral; the snapshot holds 167, 63, 99, 5", len(cat.tools), reads, writes, ephemeral)
	}
	for _, name := range []string{"agent_runtime.agent", "agent_runtime.check_owner", "agent_runtime.issue_token", "agent_runtime.revoke_token",
		"agent_runtime.rendition_claim", "agent_runtime.rendition_file", "agent_runtime.rendition_renew",
		"agent_runtime.rendition_upload_url", "agent_runtime.rendition_complete"} {
		if tl := cat.byName[name]; tl == nil || !tl.restOnly || tl.service != scopeAgentRuntime {
			t.Errorf("%s is not the agent runtime's alone, over REST: %+v", name, tl)
		}
	}
	if tl := cat.byName["document_text.queue"]; tl == nil || !tl.restOnly || tl.service != scopeDocumentText {
		t.Errorf("document_text.queue is not the transcription service's alone, over REST: %+v", tl)
	}
	older, err := catalogueOf(Options{WithoutHosting: true})
	if err != nil {
		t.Fatal(err)
	}
	for name := range implemented() {
		if cat.byName[name] == nil {
			t.Errorf("the fake implements %s, which the catalogue does not have", name)
		}
	}
	if d := cat.byName["conversation.draft"]; d == nil || !d.ephemeral || d.write {
		t.Errorf("conversation.draft: %+v", d)
	}
	before, err := catalogueOf(Options{WithoutDraft: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.tools) != len(cat.tools)-1 || before.byName["conversation.draft"] != nil {
		t.Errorf("the catalogue without conversation.draft has %d tools", len(before.tools))
	}
	// The service's four tools of hosting, the five of its renditions, and
	// the two that send a rendition back.
	if len(older.tools) != len(cat.tools)-11 || older.byName["agent_runtime.issue_token"] != nil || older.byName["document.rendition_retry"] != nil ||
		older.byName["agent_runtime.rendition_claim"] != nil {
		t.Errorf("the catalogue without the agent runtime's service has %d tools", len(older.tools))
	}
}

// TestHosting: an agent is asked in the site while it is a runtime agent
// with a live runtime token, it and its owner active; an mcp agent never.
// There is no me_site_chat to declare anything with. The controls issue
// and revoke a runtime agent's one token, as the agent runtime's service
// does, and refuse an owner's token for one.
func TestHosting(t *testing.T) {
	w := newFakeWorld(t, Options{})
	if !w.fc.SiteChat(w.tutorA.ID) || w.fc.Hosting(w.tutorA.ID) != hostingRuntime {
		t.Fatal("a runtime agent the runtime holds a token for is not asked in the site")
	}
	if w.fc.SiteChat(w.satoA.ID) || w.fc.Hosting(w.satoA.ID) != "" {
		t.Error("a person is asked in the site")
	}
	// me_site_chat is no tool: tools/call answers as for any name it does
	// not know, and the agent is asked as before.
	if a, err := w.agentC.call(context.Background(), "me_site_chat", map[string]any{"on": false, "idempotency_key": "site-chat:1"}); err != nil ||
		a.RPCError == nil || !strings.Contains(string(a.RPCError), `unknown tool \"me_site_chat\"`) || !w.fc.SiteChat(w.tutorA.ID) {
		t.Errorf("me_site_chat: %v %s", err, a.RPCError)
	}
	if _, err := w.fc.IssueToken(w.tutorA.ID); err == nil {
		t.Error("an owner's token issued for a runtime agent")
	}

	mcpID, mcpSeat, _ := w.mcpAgent()
	if w.fc.SiteChat(mcpID) || w.fc.Hosting(mcpID) != hostingMCP {
		t.Error("an mcp agent is asked in the site")
	}
	if _, _, err := w.fc.Ask(w.co.ID, w.seats[1].ID, mcpSeat, "Q"); !isRefusal(err, "mcp_agent") {
		t.Errorf("a question to an mcp agent: %v", err)
	}
	if _, err := w.fc.IssueRuntimeToken(mcpID); err == nil {
		t.Error("an mcp agent was issued a runtime token")
	}

	// A new runtime token replaces the one before; revoked, the agent is
	// not asked until another is issued.
	first := w.tutorA.Token
	next, err := w.fc.IssueRuntimeToken(w.tutorA.ID)
	w.ok(err)
	if a, err := newMCPClient(w.srv.URL, first, nil).call(context.Background(), "me_get", map[string]any{}); err != nil || a.Status != http.StatusUnauthorized {
		t.Errorf("the token a new one replaced: %d %v", a.Status, err)
	}
	if got := w.fc.RuntimeToken(w.tutorA.ID); got != next {
		t.Errorf("RuntimeToken is %+v, not the one issued", got)
	}
	creds := w.fc.Credentials(w.tutorA.ID)
	if len(creds) != 2 || creds[0].ID != next.CredentialID || creds[0].IssuedTo != scopeAgentRuntime || creds[1].RevokedAt == nil {
		t.Errorf("the tutor's credentials: %+v", creds)
	}
	revoked, err := w.fc.RevokeRuntimeToken(w.tutorA.ID)
	w.ok(err)
	if len(revoked) != 1 || revoked[0] != next.CredentialID || w.fc.SiteChat(w.tutorA.ID) || w.fc.RuntimeToken(w.tutorA.ID) != (Token{}) {
		t.Errorf("revoked %v; asked %v", revoked, w.fc.SiteChat(w.tutorA.ID))
	}
	if _, _, err := w.fc.Ask(w.co.ID, w.seats[0].ID, w.tutorM.ID, "Q"); !isRefusal(err, "agent_not_hosted") {
		t.Errorf("a question to a runtime agent not hosted: %v", err)
	}
	if _, err := w.fc.IssueRuntimeToken(w.tutorA.ID); err != nil {
		t.Fatal(err)
	}
	w.ok(w.fc.SuspendActor(w.satoA.ID))
	if w.fc.SiteChat(w.tutorA.ID) {
		t.Error("an agent whose owner is suspended is asked")
	}
	if _, err := w.fc.IssueRuntimeToken(w.tutorA.ID); err == nil {
		t.Error("an agent whose owner is suspended was issued a token")
	}
	w.ok(w.fc.ReactivateActor(w.satoA.ID))
	if !w.fc.SiteChat(w.tutorA.ID) || w.fc.RuntimeIssues(w.tutorA.ID) != 3 {
		t.Errorf("asked %v after %d issues", w.fc.SiteChat(w.tutorA.ID), w.fc.RuntimeIssues(w.tutorA.ID))
	}
}

// TestServiceScopes: each site service calls its own tools and nothing
// else, and nobody else calls them; over MCP, a service's credential is
// refused at the door.
func TestServiceScopes(t *testing.T) {
	w := newFakeWorld(t, Options{})
	ctx := t.Context()
	text := &restClient{base: w.srv.URL, token: w.fc.IssueServiceToken("transcriber").Token, hc: w.srv.Client()}
	for _, c := range []struct {
		name   string
		r      *restClient
		path   string
		status int
		reason string
	}{
		{"the transcriber at the runtime's", text, "/v1/services/agent_runtime/agents/" + w.tutorA.ID, http.StatusForbidden, "not_for_services"},
		{"the runtime at the transcriber's", w.runtimeService(), "/v1/services/document_text/queue", http.StatusForbidden, "not_for_services"},
		{"the runtime at an agent's", w.runtimeService(), "/v1/me", http.StatusForbidden, "not_for_services"},
		{"an agent at the runtime's", w.rest(), "/v1/services/agent_runtime/agents/" + w.tutorA.ID, http.StatusForbidden, "service_only"},
		{"the runtime at its own", w.runtimeService(), "/v1/services/agent_runtime/agents/" + w.tutorA.ID, http.StatusOK, ""},
	} {
		method := "GET"
		var body any
		if strings.HasSuffix(c.path, "/queue") {
			method, body = "POST", map[string]any{"max": 1}
		}
		h, err := c.r.do(ctx, method, c.path, body, "k:"+c.name)
		if err != nil {
			t.Fatal(err)
		}
		if h.Status != c.status || (c.reason != "" && !strings.Contains(string(h.Body), c.reason)) {
			t.Errorf("%s: %d %s", c.name, h.Status, h.Body)
		}
	}
	a, err := newMCPClient(w.srv.URL, w.svc.Token, nil).call(ctx, "me_get", map[string]any{})
	if err != nil || a.Status != http.StatusUnauthorized || !strings.Contains(string(a.Body), "service's REST routes alone") {
		t.Errorf("the runtime's credential over MCP: %d %s %v", a.Status, a.Body, err)
	}
}

func TestPresetsAreCores(t *testing.T) {
	cases := []struct {
		preset, perm string
		want         level
	}{
		{"student", permConversationAsk, autonomous},
		{"student", permAgentDelegate, confirmRequired},
		{"student", permConversationAnswer, denied},
		{"student", permSubmissionWrite, autonomous},
		{"instructor", permMemberManage, autonomous},
		{"ta", permActionDecide, denied},
		{"ta", permGradeSubmit, autonomous},
		{"course_tutor", permDocumentRead, autonomous},
		{"course_tutor", permConversationAnswer, autonomous},
		{"course_tutor", permSubmissionRead, denied},
		{"delegate", permGradeRead, autonomous},
		{"grader", permGradeSubmit, confirmRequired},
	}
	for _, c := range cases {
		p := presets[c.preset]
		for i, name := range allPerms {
			if name == c.perm && p.levels[i] != c.want {
				t.Errorf("%s %s = %s, want %s", c.preset, c.perm, p.levels[i], c.want)
			}
		}
	}
	if s := presets["course_tutor"]; s.role != "assistant" || s.studentScope != scopeListed {
		t.Errorf("course_tutor: %+v", s)
	}
}

func TestCanonicalize(t *testing.T) {
	cases := []struct{ a, b string }{
		{`{"b":1,"a":"x"}`, `{"a":"x","b":1}`},
		{`{"n":1}`, `{"n":1.0}`},
		{`{"n":10e-1}`, `{"n":1}`},
		{`{"n":-2.50}`, `{"n":-25e-1}`},
		{`{"s":"<é>"}`, `{"s":"<é>"}`},
		{`{ "x" : [ 1 , 2 ] }`, `{"x":[1,2]}`},
	}
	for _, c := range cases {
		x, err := canonicalize([]byte(c.a))
		if err != nil {
			t.Fatal(err)
		}
		y, err := canonicalize([]byte(c.b))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(x, y) {
			t.Errorf("%s and %s canonicalize to %s and %s", c.a, c.b, x, y)
		}
	}
	if _, err := canonicalize([]byte(`[1]`)); err == nil {
		t.Error("an array canonicalized")
	}
	for lit, want := range map[string]string{`{"n":1e2}`: `{"n":100}`, `{"n":-0.0}`: `{"n":0}`, `{"n":12.340e-3}`: `{"n":0.01234}`} {
		if got, err := canonicalize([]byte(lit)); err != nil || string(got) != want {
			t.Errorf("%s: %s %v, want %s", lit, got, err, want)
		}
	}
}

func TestIdempotency(t *testing.T) {
	w := newFakeWorld(t, Options{})
	conv, m1 := w.ask(0, "Q1")
	args := answer(w, conv, m1, "A1", 1)
	first := mustCall(t, w.agentC, "conversation_answer", args)
	wantEnvelope(t, first, "executed", "", "")

	t.Run("the same key and arguments replay", func(t *testing.T) {
		// The same call written differently is the same call.
		raw := fmt.Sprintf(`{"body":"A1","in_reply_to_message_id":%q,"idempotency_key":%q,"conversation_id":%q,"course_id":%q}`,
			m1, args["idempotency_key"], conv, w.course())
		var reordered map[string]any
		if err := json.Unmarshal([]byte(raw), &reordered); err != nil {
			t.Fatal(err)
		}
		a := mustCall(t, w.agentC, "conversation_answer", reordered)
		wantEnvelope(t, a, "executed", "", "")
		if a.Structured["replayed"] != true || a.str("action_id") != first.str("action_id") ||
			a.str("result", "message_id") != first.str("result", "message_id") {
			t.Errorf("replay: %s", a.Text)
		}
		if n := len(w.fc.Answers(conv)); n != 1 {
			t.Errorf("%d answers after a replay", n)
		}
	})
	t.Run("the same key with other arguments is a conflict and records nothing", func(t *testing.T) {
		before := len(list(mustCall(t, w.agentC, "action_list_mine", inCourseArgs(w)), "actions"))
		a := mustCall(t, w.agentC, "conversation_answer", answer(w, conv, m1, "A1, again", 1))
		wantEnvelope(t, a, "error", codeIdempotencyConflict, "")
		if a.str("error", "details", "action_id") != first.str("action_id") {
			t.Errorf("details.action_id: %s", a.Text)
		}
		if after := len(list(mustCall(t, w.agentC, "action_list_mine", inCourseArgs(w)), "actions")); after != before {
			t.Errorf("%d actions, then %d", before, after)
		}
	})
	t.Run("keys are 1 to 200 characters", func(t *testing.T) {
		conv, m := w.ask(1, "Q")
		for _, c := range []struct {
			key    string
			status string
		}{{"", "error"}, {strings.Repeat("あ", 201), "error"}, {strings.Repeat("あ", 200), "executed"}} {
			args := answer(w, conv, m, "A", 1)
			args["idempotency_key"] = c.key
			a := mustCall(t, w.agentC, "conversation_answer", args)
			if a.status() != c.status || (c.status == "error" && a.str("error", "code") != codeInvalidArgument) {
				t.Errorf("a key of %d characters: %s", len([]rune(c.key)), a.Text)
			}
		}
	})
	t.Run("keys are unique per actor", func(t *testing.T) {
		own := w.ownAgent()
		conv, m := w.askOwn("Q")
		args := answer(w, conv, m, "A1", 1)
		args["idempotency_key"] = "shared-key"
		wantEnvelope(t, mustCall(t, own, "conversation_answer", args), "executed", "", "")
		tconv, tm := w.ask(0, "Q again")
		targs := answer(w, tconv, tm, "A", 1)
		targs["idempotency_key"] = "shared-key"
		wantEnvelope(t, mustCall(t, w.agentC, "conversation_answer", targs), "executed", "", "")
	})
}

func TestProposalsReplayAsTheyStandNow(t *testing.T) {
	cases := []struct {
		name   string
		decide func(w *fakeWorld, actionID string)
		status string
		code   string
	}{
		{"approved replays executed", func(w *fakeWorld, id string) { w.approve(id) }, "executed", ""},
		{"rejected replays rejected", func(w *fakeWorld, id string) { w.reject(id, "Too terse") }, "rejected", ""},
		{"expired replays cancelled", func(w *fakeWorld, id string) { w.expire(id) }, "cancelled", codeFailedPrecondition},
		{"withdrawn by removal replays cancelled", func(w *fakeWorld, _ string) { w.removeTutor() }, "cancelled", codeFailedPrecondition},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newFakeWorld(t, Options{})
			w.setTutorLevel("confirm_required")
			conv, m1 := w.ask(0, "Q")
			args := answer(w, conv, m1, "A", 1)
			a := mustCall(t, w.agentC, "conversation_answer", args)
			wantEnvelope(t, a, "proposed", "", "")
			if !strings.HasPrefix(a.str("note"), "Not executed.") || a.IsError {
				t.Errorf("a proposal's note and isError: %v %q", a.IsError, a.str("note"))
			}
			c.decide(w, a.str("action_id"))
			r := mustCall(t, w.agentC, "conversation_answer", args)
			if r.status() != c.status || r.str("error", "code") != c.code || r.Structured["replayed"] != true {
				t.Errorf("replay: %s", r.Text)
			}
			if r.IsError != (c.status != "executed") {
				t.Errorf("isError %v for %s", r.IsError, c.status)
			}
		})
	}
}

func TestInbox(t *testing.T) {
	clk := newClock()
	w := newFakeWorld(t, Options{Now: clk.now})
	inboxOf := func(w *fakeWorld, args map[string]any) []string {
		t.Helper()
		var ids []string
		for _, row := range list(mustCall(t, w.agentC, "conversation_inbox", args), "conversations") {
			ids = append(ids, row.(map[string]any)["id"].(string))
		}
		return ids
	}
	inbox := func(args map[string]any) []string { return inboxOf(w, args) }
	yuki, _ := w.ask(0, "first")
	clk.add(time.Second)
	ken, _ := w.ask(1, "second")
	clk.add(time.Second)
	if got := inbox(inCourseArgs(w)); fmt.Sprint(got) != fmt.Sprint([]string{yuki, ken}) {
		t.Fatalf("inbox %v, want Yuki's then Ken's", got)
	}
	w.followUp(yuki, "and another thing")
	if got := inbox(inCourseArgs(w)); fmt.Sprint(got) != fmt.Sprint([]string{ken, yuki}) {
		t.Fatalf("inbox %v: the one waiting longest comes first", got)
	}

	t.Run("each row carries the message an answer replies to", func(t *testing.T) {
		row := list(mustCall(t, w.agentC, "conversation_inbox", inCourseArgs(w)), "conversations")[1].(map[string]any)
		msgs := w.fc.Messages(yuki)
		if row["latest_opener_message_id"] != msgs[len(msgs)-1].ID {
			t.Errorf("latest_opener_message_id %v, want %s", row["latest_opener_message_id"], msgs[len(msgs)-1].ID)
		}
	})
	t.Run("limits: 20 by default, 100 at most", func(t *testing.T) {
		w := newFakeWorld(t, Options{Now: clk.now})
		inbox := func(args map[string]any) []string { return inboxOf(w, args) }
		for range 105 {
			clk.add(time.Millisecond)
			w.ask(0, "q")
		}
		for _, c := range []struct {
			limit any
			want  int
		}{{nil, 20}, {0, 20}, {-3, 20}, {1, 1}, {100, 100}, {1000, 100}} {
			args := inCourseArgs(w)
			if c.limit != nil {
				args["limit"] = c.limit
			}
			if got := len(inbox(args)); got != c.want {
				t.Errorf("limit %v: %d rows, want %d", c.limit, got, c.want)
			}
		}
	})
	t.Run("what the inbox leaves out", func(t *testing.T) {
		w := newFakeWorld(t, Options{Now: clk.now})
		inbox := func(args map[string]any) []string { return inboxOf(w, args) }
		answered, m := w.ask(0, "answered")
		wantEnvelope(t, mustCall(t, w.agentC, "conversation_answer", answer(w, answered, m, "A", 1)), "executed", "", "")
		closed, _ := w.ask(0, "closed")
		w.closeAsOpener(closed, "never mind")
		_, r := w.ask(0, "retracted")
		w.retract(r, "")
		pending, pm := w.ask(1, "pending")
		w.setTutorLevel("confirm_required")
		wantEnvelope(t, mustCall(t, w.agentC, "conversation_answer", answer(w, pending, pm, "A", 1)), "proposed", "", "")
		w.setTutorLevel("autonomous")
		keep, _ := w.ask(1, "waiting")
		if got := inbox(inCourseArgs(w)); fmt.Sprint(got) != fmt.Sprint([]string{keep}) {
			t.Errorf("inbox %v, want only %s", got, keep)
		}
		w.pauseStudent(1)
		if got := inbox(inCourseArgs(w)); len(got) != 0 {
			t.Errorf("an opener who may no longer address the agent: %v", got)
		}
	})
}

func TestProposals(t *testing.T) {
	t.Run("approved: posted under the proposal, followed through event_list", func(t *testing.T) {
		w := newFakeWorld(t, Options{})
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Q")
		args := answer(w, conv, m1, "A", 1)
		a := mustCall(t, w.agentC, "conversation_answer", args)
		id := a.str("action_id")
		if got := w.fc.Proposals(w.co.ID); len(got) != 1 || got[0].ActionID != id || got[0].IdempotencyKey != args["idempotency_key"] {
			t.Fatalf("proposals: %+v", got)
		}
		if out, err := w.fc.Approve(id); err != nil || out != "executed" {
			t.Fatalf("approve: %s %v", out, err)
		}
		ans := w.fc.Answers(conv)
		if len(ans) != 1 || ans[0].ActionID != id || ans[0].IdempotencyKey != args["idempotency_key"] || ans[0].InReplyTo != m1 {
			t.Fatalf("answers: %+v", ans)
		}
		ev := mustCall(t, w.agentC, "event_list", inCourseArgs(w, "since_seq", 0))
		var approved map[string]any
		for _, e := range list(ev, "events") {
			if e := e.(map[string]any); e["type"] == "action.approved" {
				approved = e
			}
		}
		if approved == nil || approved["action_id"] != id || approved["payload"].(map[string]any)["outcome"] != "executed" {
			t.Fatalf("no action.approved for %s: %s", id, ev.Text)
		}
	})
	t.Run("approved after the opener wrote again: failed", func(t *testing.T) {
		w := newFakeWorld(t, Options{})
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Q")
		a := mustCall(t, w.agentC, "conversation_answer", answer(w, conv, m1, "A", 1))
		w.followUp(conv, "Q2")
		if out, err := w.fc.Approve(a.str("action_id")); err != nil || out != "failed" {
			t.Fatalf("approve: %s %v", out, err)
		}
		if n := len(w.fc.Answers(conv)); n != 0 {
			t.Errorf("%d answers posted", n)
		}
	})
	t.Run("rejected: the reason in action_list_mine", func(t *testing.T) {
		w := newFakeWorld(t, Options{})
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Q")
		a := mustCall(t, w.agentC, "conversation_answer", answer(w, conv, m1, "A", 1))
		w.reject(a.str("action_id"), "Explain the thesis.")
		mine := list(mustCall(t, w.agentC, "action_list_mine", inCourseArgs(w)), "actions")
		last := mine[len(mine)-1].(map[string]any)
		reason := last["result"].(map[string]any)["decision"].(map[string]any)["reason"]
		if last["status"] != "rejected" || reason != "Explain the thesis." {
			t.Errorf("action_list_mine: %v", last)
		}
	})
	t.Run("expired by the proposal TTL when a person gets to it", func(t *testing.T) {
		clk := newClock()
		w := newFakeWorld(t, Options{Now: clk.now, ProposalTTL: time.Hour})
		w.setTutorLevel("confirm_required")
		conv, m1 := w.ask(0, "Q")
		a := mustCall(t, w.agentC, "conversation_answer", answer(w, conv, m1, "A", 1))
		clk.add(2 * time.Hour)
		if out, err := w.fc.Approve(a.str("action_id")); err != nil || out != "cancelled" {
			t.Fatalf("approve: %s %v", out, err)
		}
	})
	// An agent's proposal is its owner's to decide where they could make
	// it themselves without anyone's confirmation, and nobody's else of
	// its party: with no one else to decide it, the owner does, until
	// their own level for it is below autonomous.
	t.Run("an agent's owner decides its proposal only where they could make it themselves", func(t *testing.T) {
		fc := New(Options{})
		co := fc.AddCourse("CS101")
		sato := fc.AddPerson("Sato")
		satoM, _ := fc.Seat(sato.ID, co.ID, SeatOptions{Preset: "instructor"})
		yuki := fc.AddPerson("Yuki")
		yukiM, _ := fc.Seat(yuki.ID, co.ID, SeatOptions{Preset: "student"})
		tutor, _ := fc.AddAgent("Tutor", sato.ID)
		tutorM, err := fc.Seat(tutor.ID, co.ID, SeatOptions{Preset: "course_tutor", Principal: satoM.ID,
			Perms: map[string]string{permConversationAnswer: "confirm_required"}})
		if err != nil {
			t.Fatal(err)
		}
		propose := func(body string) string {
			t.Helper()
			conv, m, err := fc.Ask(co.ID, yukiM.ID, tutorM.ID, "Q")
			if err != nil {
				t.Fatal(err)
			}
			fc.mu.Lock()
			out := fc.invoke(fc.actors[tutor.ID], fc.cat.byName["conversation.answer"],
				[]byte(fmt.Sprintf(`{"course_id":%q,"conversation_id":%q,"in_reply_to_message_id":%q,"body":%q}`, co.ID, conv.ID, m.ID, body)), "k:"+body, "")
			fc.mu.Unlock()
			if out.Status != "proposed" {
				t.Fatalf("%+v", out)
			}
			return out.ActionID
		}
		// No person answers: an owner decides their agent's answer as far as
		// they decide actions.
		if out, err := fc.Approve(propose("A")); err != nil || out != "executed" {
			t.Errorf("its owner, who decides actions at autonomous, approved: %s %v", out, err)
		}
		if err := fc.SetLevel(satoM.ID, permActionDecide, "confirm_required"); err != nil {
			t.Fatal(err)
		}
		if _, err := fc.Approve(propose("B")); err == nil || !strings.Contains(err.Error(), "nobody in the course may judge") {
			t.Errorf("approved by its owner's party: %v", err)
		}
	})
}

func TestEventVisibility(t *testing.T) {
	w := newFakeWorld(t, Options{})
	own := w.ownAgent()
	w.setTutorLevel("confirm_required")
	tconv, tm := w.ask(1, "Ken asks the tutor")
	mustCall(t, w.agentC, "conversation_answer", answer(w, tconv, tm, "A", 1))
	oconv, _ := w.askOwn("Yuki asks her agent")
	types := func(c *mcpClient) map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, e := range list(mustCall(t, c, "event_list", inCourseArgs(w, "since_seq", 0)), "events") {
			e := e.(map[string]any)
			out[e["type"].(string)]++
			if id, _ := e["subject_id"].(string); id == oconv && c == w.agentC || id == tconv && c == own {
				t.Errorf("a conversation's news reached someone not in it: %v", e)
			}
		}
		return out
	}
	tutor, mine := types(w.agentC), types(own)
	if tutor["action.proposed"] != 1 || tutor["conversation.opened"] != 1 || tutor["course.created"] != 1 {
		t.Errorf("the tutor sees %v", tutor)
	}
	if mine["action.proposed"] != 0 || mine["conversation.opened"] != 1 {
		t.Errorf("Yuki's agent sees %v", mine)
	}

	t.Run("paging: since_seq, limit, next_seq, more", func(t *testing.T) {
		all := list(mustCall(t, w.agentC, "event_list", inCourseArgs(w, "since_seq", 0)), "events")
		var seen []any
		since := json.Number("0")
		for range 10 {
			page := mustCall(t, w.agentC, "event_list", inCourseArgs(w, "since_seq", since, "limit", 2))
			evs := list(page, "events")
			seen = append(seen, evs...)
			since = page.Structured["result"].(map[string]any)["next_seq"].(json.Number)
			if page.Structured["result"].(map[string]any)["more"] != (len(evs) == 2) {
				t.Fatalf("more: %s", page.Text)
			}
			if len(evs) < 2 {
				break
			}
		}
		if fmt.Sprint(seen) != fmt.Sprint(all) {
			t.Errorf("paged %d events, listed %d", len(seen), len(all))
		}
	})
	t.Run("a decider sees the proposals", func(t *testing.T) {
		mori := w.fc.AddPerson("Mori 2")
		if _, err := w.fc.Seat(mori.ID, w.co.ID, SeatOptions{Preset: "ta", Perms: map[string]string{permActionDecide: "autonomous"}}); err != nil {
			t.Fatal(err)
		}
		c := newMCPClient(w.srv.URL, mori.Token, w.srv.Client())
		if got := types(c); got["action.proposed"] != 1 || got["conversation.opened"] != 0 || got["member.added"] == 0 {
			t.Errorf("a decider sees %v", got)
		}
		for _, e := range list(mustCall(t, c, "event_list", inCourseArgs(w, "since_seq", 0)), "events") {
			e := e.(map[string]any)
			if _, ok := e["payload"].(map[string]any); !ok || e["action_id"] == nil {
				t.Errorf("an event without a payload object or an action: %v", e)
			}
		}
	})
}

func TestDelegates(t *testing.T) {
	w := newFakeWorld(t, Options{})
	own := w.ownAgent()
	seat := func(c *mcpClient) map[string]any {
		t.Helper()
		return list(mustCall(t, c, "me_memberships", map[string]any{}), "memberships")[0].(map[string]any)
	}
	m := seat(own)
	perms := m["perms"].(map[string]any)
	if m["principal_member_id"] != w.seats[0].ID || m["answers_course"] != false || m["student_scope"] != "listed" ||
		perms[permConversationAnswer] != "autonomous" || perms[permSubmissionRead] != "autonomous" || perms[permMemberManage] != "denied" {
		t.Errorf("Yuki's agent's seat: %v", m)
	}
	t.Run("capped by its principal", func(t *testing.T) {
		if err := w.fc.SetLevel(w.sato.ID, permConversationAsk, "confirm_required"); err != nil {
			t.Fatal(err)
		}
		if got := seat(w.agentC)["perms"].(map[string]any)[permConversationAnswer]; got != "confirm_required" {
			t.Errorf("the tutor's conversation_answer under a principal who must confirm asking: %v", got)
		}
		conv, m1 := w.ask(1, "Q")
		wantEnvelope(t, mustCall(t, w.agentC, "conversation_answer", answer(w, conv, m1, "A", 1)), "proposed", "", "")
		// Its ceilings: agent_delegate never, action_decide by proposal,
		// member_manage as far as a principal who manages members holds
		// it, and nothing past a student's for a student's agent.
		if err := w.fc.SetLevel(w.tutorM.ID, permAgentDelegate, "autonomous"); err == nil {
			t.Error("a delegate was given agent_delegate")
		}
		if err := w.fc.SetLevel(w.tutorM.ID, permActionDecide, "autonomous"); err == nil {
			t.Error("an agent was given action_decide past confirm_required")
		}
		if err := w.fc.SetLevel(w.tutorM.ID, permMemberManage, "autonomous"); err != nil {
			t.Errorf("an instructor's delegate may manage members as its principal does: %v", err)
		}
		if err := w.fc.SetLevel(w.ownM.ID, permMemberManage, "autonomous"); err == nil {
			t.Error("a student's agent widened past its principal")
		}
		ceil := seat(own)
		if ceil["perm_ceilings"].(map[string]any)[permMemberManage] != "denied" ||
			ceil["perm_ceiling_reasons"].(map[string]any)[permMemberManage] != "principal_level" ||
			ceil["perm_ceiling_reasons"].(map[string]any)[permDocumentWrite] != "principal_level" ||
			ceil["perm_ceilings"].(map[string]any)[permConversationAsk] != "confirm_required" {
			t.Errorf("a student's agent's ceilings: %v %v", ceil["perm_ceilings"], ceil["perm_ceiling_reasons"])
		}
	})
	t.Run("paused with its principal", func(t *testing.T) {
		w.pauseStudent(0)
		m := seat(own)
		for p, l := range m["perms"].(map[string]any) {
			if l != "denied" {
				t.Errorf("%s is %v while the principal is paused", p, l)
			}
		}
		if m["status"] != "active" {
			t.Errorf("status %v: the delegate's own seat is not paused", m["status"])
		}
		wantEnvelope(t, mustCall(t, own, "conversation_inbox", inCourseArgs(w)), "denied", codeForbidden, reasonPrincipalNotActive)
		if err := w.fc.ResumeSeat(w.seats[0].ID); err != nil {
			t.Fatal(err)
		}
		wantEnvelope(t, mustCall(t, own, "conversation_inbox", inCourseArgs(w)), "executed", "", "")
	})
}

func TestModelReads(t *testing.T) {
	w := newFakeWorld(t, Options{})
	own := w.ownAgent()
	yukiWork, err := w.fc.AddWork(w.co.ID, w.seats[0].ID, "My answers", "85")
	if err != nil {
		t.Fatal(err)
	}
	kenWork, err := w.fc.AddWork(w.co.ID, w.seats[1].ID, "Ken's answers", "70")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("material", func(t *testing.T) {
		docs := list(mustCall(t, w.agentC, "document_list", inCourseArgs(w)), "documents")
		if len(docs) != 3 {
			t.Errorf("%d documents", len(docs))
		}
		a := mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", w.co.SyllabusID))
		if !strings.Contains(a.str("result", "version", "body_md"), "syllabus") {
			t.Errorf("syllabus: %s", a.Text)
		}
		a = mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", w.co.SlidesID))
		url := a.str("result", "version", "files", "0", "download_url")
		if !strings.HasPrefix(url, w.srv.URL+blobPath) || a.str("result", "version", "files", "0", "content_type") != "application/pdf" {
			t.Fatalf("slides: %s", a.Text)
		}
		resp, err := w.srv.Client().Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(resp.Body); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || !strings.HasPrefix(buf.String(), "%PDF") ||
			resp.Header.Get("Content-Disposition") != `attachment; filename="Lecture 1 slides.pdf"` {
			t.Errorf("download: %d %q %s", resp.StatusCode, buf.String(), resp.Header.Get("Content-Disposition"))
		}
		wantEnvelope(t, mustCall(t, w.agentC, "assignment_get", inCourseArgs(w, "assignment_id", w.co.AssignmentID)), "executed", "", "")
		if n := len(list(mustCall(t, w.agentC, "assignment_list", inCourseArgs(w)), "assignments")); n != 1 {
			t.Errorf("%d assignments", n)
		}
		wantEnvelope(t, mustCall(t, w.agentC, "course_get", inCourseArgs(w)), "executed", "", "")
	})
	t.Run("a course tutor reads nobody's work", func(t *testing.T) {
		for _, c := range []struct {
			tool string
			args map[string]any
		}{
			{"submission_list", inCourseArgs(w)},
			{"submission_get", inCourseArgs(w, "submission_id", yukiWork.SubmissionID)},
			{"grade_list", inCourseArgs(w)},
			{"grade_get", inCourseArgs(w, "grade_id", yukiWork.GradeID)},
			{"component_tree", inCourseArgs(w)},
			{"gradebook_get", inCourseArgs(w, "student_member_id", w.seats[0].ID)},
		} {
			wantEnvelope(t, mustCall(t, w.agentC, c.tool, c.args), "denied", codeForbidden, reasonPermDenied)
		}
	})
	t.Run("a student's agent reads its principal's work and nobody else's", func(t *testing.T) {
		subs := list(mustCall(t, own, "submission_list", inCourseArgs(w)), "submissions")
		if len(subs) != 1 || subs[0].(map[string]any)["id"] != yukiWork.SubmissionID {
			t.Errorf("submissions: %v", subs)
		}
		if g := list(mustCall(t, own, "grade_list", inCourseArgs(w)), "grades"); len(g) != 1 {
			t.Errorf("grades: %v", g)
		}
		wantEnvelope(t, mustCall(t, own, "grade_get", inCourseArgs(w, "grade_id", yukiWork.GradeID)), "executed", "", "")
		wantEnvelope(t, mustCall(t, own, "grade_get", inCourseArgs(w, "grade_id", kenWork.GradeID)), "denied", codeForbidden, reasonStudentScope)
		wantEnvelope(t, mustCall(t, own, "submission_get", inCourseArgs(w, "submission_id", kenWork.SubmissionID)), "denied", codeForbidden, reasonStudentScope)
		wantEnvelope(t, mustCall(t, own, "gradebook_get", inCourseArgs(w, "student_member_id", w.seats[1].ID)), "denied", codeForbidden, reasonStudentScope)
		book := mustCall(t, own, "gradebook_get", inCourseArgs(w, "student_member_id", w.seats[0].ID))
		lines := list(book, "components")
		if len(lines) != 2 || lines[0].(map[string]any)["percent"] != "85" {
			t.Errorf("gradebook: %s", book.Text)
		}
		wantEnvelope(t, mustCall(t, own, "document_get", inCourseArgs(w, "document_id", w.co.InstructionsID)), "executed", "", "")
	})
	t.Run("no such thing", func(t *testing.T) {
		wantEnvelope(t, mustCall(t, own, "grade_get", inCourseArgs(w, "grade_id", w.co.SyllabusID)), "error", codeNotFound, "")
		wantEnvelope(t, mustCall(t, own, "document_get", inCourseArgs(w, "document_id", w.co.AssignmentID)), "error", codeNotFound, "")
	})
}

func TestUnimplementedTools(t *testing.T) {
	w := newFakeWorld(t, Options{})
	wantEnvelope(t, mustCall(t, w.agentC, "member_delegate_defaults", inCourseArgs(w)), "error", codeForbidden, "not_implemented")
	wantEnvelope(t, mustCall(t, w.agentC, "course_archive", inCourseArgs(w, "idempotency_key", "k")), "error", codeForbidden, "not_implemented")
	wantEnvelope(t, mustCall(t, w.agentC, "member_delegate_defaults", map[string]any{"course_id": "00000000-0000-7000-8000-000000000000"}),
		"error", codeNotFound, "")
	wantEnvelope(t, mustCall(t, w.agentC, "member_delegate_defaults", map[string]any{"course_id": w.co.ID, "no_such_argument": 1}),
		"error", codeInvalidArgument, "")
}

func TestRateLimit(t *testing.T) {
	clk := newClock()
	w := newFakeWorld(t, Options{RatePerMinute: 60, RateBurst: 4, Now: clk.now})
	// initialize and notifications/initialized took two of the four.
	for range 2 {
		mustCall(t, w.agentC, "me_get", map[string]any{})
	}
	a, err := w.agentC.call(context.Background(), "me_get", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(a.Body, &body); err != nil {
		t.Fatalf("%v: %s", err, a.Body)
	}
	if a.Status != http.StatusTooManyRequests || a.Header.Get("Retry-After") != "2" || body.Error.Code != codeRateLimited ||
		fmt.Sprint(body.Error.Details["retry_after_seconds"]) != "2" {
		t.Fatalf("%d %v %s", a.Status, a.Header, a.Body)
	}
	t.Run("REST shares the allowance", func(t *testing.T) {
		h, err := w.rest().do(context.Background(), "GET", "/v1/me", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if h.Status != http.StatusTooManyRequests || h.Header.Get("Retry-After") != "1" {
			t.Errorf("REST: %d %v %s", h.Status, h.Header, h.Body)
		}
	})
	t.Run("the bucket fills again", func(t *testing.T) {
		clk.add(time.Second)
		mustCall(t, w.agentC, "me_get", map[string]any{})
	})
	t.Run("each actor has its own", func(t *testing.T) {
		mustCall(t, w.ownAgent(), "me_get", map[string]any{})
	})
	calls := w.fc.Calls()
	var limited int
	for _, c := range calls {
		if c.HTTPStatus == http.StatusTooManyRequests {
			limited++
			if c.ActorID != w.tutorA.ID || c.Status != "" {
				t.Errorf("a 429 logged as %+v", c)
			}
		}
	}
	if limited != 2 {
		t.Errorf("%d calls logged as 429", limited)
	}
}

func TestRevoke(t *testing.T) {
	w := newFakeWorld(t, Options{})
	mustCall(t, w.agentC, "me_get", map[string]any{})
	w.revokeTutorToken()
	a, err := w.agentC.call(context.Background(), "me_get", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != http.StatusUnauthorized || !strings.HasPrefix(a.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("MCP after Revoke: %d %s", a.Status, a.Body)
	}
	h, err := w.rest().do(context.Background(), "GET", "/v1/me", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != http.StatusUnauthorized || h.Header.Get("WWW-Authenticate") == "" || !strings.Contains(string(h.Body), codeUnauthenticated) {
		t.Errorf("REST after Revoke: %d %v %s", h.Status, h.Header, h.Body)
	}
	var logged int
	for _, c := range w.fc.Calls() {
		if c.HTTPStatus == http.StatusUnauthorized && c.ActorID == w.tutorA.ID {
			logged++
		}
	}
	if logged != 2 {
		t.Errorf("%d 401s logged for the tutor", logged)
	}
	if err := w.fc.Revoke("ais_nosuchtoken"); err == nil {
		t.Error("an unknown token revoked")
	}
}

// Tokens are in Core's shape, listed by their prefix: a runtime agent's
// one token is issued to the site's agent runtime, by its service; an mcp
// agent's by its owner, as many as they like.
func TestCredentials(t *testing.T) {
	w := newFakeWorld(t, Options{})
	tokenRe := regexp.MustCompile(`^ais_([a-z2-7]{12})_[A-Za-z0-9_-]{43}$`)
	m := tokenRe.FindStringSubmatch(w.tutorA.Token)
	if m == nil {
		t.Fatalf("the tutor's token is not in Core's shape")
	}
	creds := w.fc.Credentials(w.tutorA.ID)
	if len(creds) != 1 || creds[0].Prefix != m[1] || creds[0].Label != defaultRuntimeLabel || creds[0].IssuedTo != scopeAgentRuntime ||
		creds[0].RevokedAt != nil {
		t.Fatalf("the tutor's credentials: %+v", creds)
	}
	tools, err := w.fc.AddMCPAgent("Ken's tools", w.people[1].ID)
	w.ok(err)
	second, err := w.fc.IssueLabelledToken(tools.ID, "second")
	w.ok(err)
	c := w.fc.Credentials(tools.ID)
	if len(c) != 2 || c[0].ID != second.CredentialID || c[0].Label != "second" || c[0].IssuedTo != "" || c[1].RevokedAt != nil {
		t.Errorf("an mcp agent's credentials: %+v", c)
	}
	w.ok(w.fc.Revoke(second.Token))
	if a, err := newMCPClient(w.srv.URL, second.Token, nil).call(context.Background(), "me_get", map[string]any{}); err != nil ||
		a.Status != http.StatusUnauthorized {
		t.Errorf("the revoked token: %d %v", a.Status, err)
	}
	wantEnvelope(t, mustCall(t, w.client(tools.Token), "me_get", map[string]any{}), "executed", "", "")
}

func TestInject(t *testing.T) {
	w := newFakeWorld(t, Options{})
	conv, m1 := w.ask(0, "Q")
	t.Run("an HTTP status instead of the call", func(t *testing.T) {
		for _, c := range []struct {
			in     Injection
			status int
			ctype  string
			retry  string
		}{
			{Injection{Status: 429, RetryAfter: 7 * time.Second}, 429, "application/json", "7"},
			{Injection{Status: 500}, 500, "text/plain; charset=utf-8", ""},
			{Injection{Status: 401}, 401, "text/plain; charset=utf-8", ""},
		} {
			w.fc.Inject(func(call InjectedCall) *Injection {
				if call.Tool == "conversation_inbox" {
					return &c.in
				}
				return nil
			})
			a, err := w.agentC.call(context.Background(), "conversation_inbox", inCourseArgs(w))
			if err != nil {
				t.Fatal(err)
			}
			if a.Status != c.status || a.Header.Get("Content-Type") != c.ctype || a.Header.Get("Retry-After") != c.retry {
				t.Errorf("%+v: %d %v %s", c.in, a.Status, a.Header, a.Body)
			}
			if c.status == 429 && !strings.Contains(string(a.Body), `"retry_after_seconds":7`) {
				t.Errorf("429 body: %s", a.Body)
			}
		}
		w.fc.Inject(nil)
		mustCall(t, w.agentC, "conversation_inbox", inCourseArgs(w))
	})
	t.Run("a delay after the call is carried out", func(t *testing.T) {
		w.fc.Inject(func(call InjectedCall) *Injection {
			if call.Tool == "conversation_answer" {
				return &Injection{DelayAfter: 500 * time.Millisecond}
			}
			return nil
		})
		defer w.fc.Inject(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		args := answer(w, conv, m1, "A", 1)
		if _, err := w.agentC.call(ctx, "conversation_answer", args); err == nil {
			t.Fatal("the call did not time out")
		}
		for deadline := time.Now().Add(2 * time.Second); len(w.fc.Answers(conv)) == 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		if n := len(w.fc.Answers(conv)); n != 1 {
			t.Fatalf("%d answers: the call should have been carried out", n)
		}
		w.fc.Inject(nil)
		a := mustCall(t, w.agentC, "conversation_answer", args)
		if a.Structured["replayed"] != true {
			t.Errorf("the retry: %s", a.Text)
		}
	})
	t.Run("a delay before", func(t *testing.T) {
		w.fc.Inject(func(InjectedCall) *Injection { return &Injection{Delay: 30 * time.Millisecond} })
		defer w.fc.Inject(nil)
		start := time.Now()
		mustCall(t, w.agentC, "me_get", map[string]any{})
		if time.Since(start) < 30*time.Millisecond {
			t.Error("not delayed")
		}
	})
	t.Run("over REST", func(t *testing.T) {
		w.fc.Inject(func(call InjectedCall) *Injection {
			if call.Transport == "rest" && call.Tool == "me_memberships" {
				return &Injection{Status: 503}
			}
			return nil
		})
		defer w.fc.Inject(nil)
		h, err := w.rest().do(context.Background(), "GET", "/v1/me/memberships", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if h.Status != 503 || !strings.Contains(string(h.Body), codeInternal) {
			t.Errorf("%d %s", h.Status, h.Body)
		}
	})
}

func TestOnCallTheOpenerWritesDuringGeneration(t *testing.T) {
	w := newFakeWorld(t, Options{})
	conv, m1 := w.ask(0, "Q1")
	var m2 string
	w.fc.OnCall(func(tool string, _ json.RawMessage) {
		if tool == "course_get" && m2 == "" {
			var err error
			m, err := w.fc.FollowUp(conv, "Q2, while you think")
			if err != nil {
				t.Error(err)
			}
			m2 = m.ID
		}
	})
	mustCall(t, w.agentC, "course_get", inCourseArgs(w))
	a := mustCall(t, w.agentC, "conversation_answer", answer(w, conv, m1, "A1", 1))
	wantEnvelope(t, a, "failed", codeConflict, "moved_on")
	if a.str("error", "details", "latest_opener_message_id") != m2 {
		t.Errorf("latest_opener_message_id: %s", a.Text)
	}
	wantEnvelope(t, mustCall(t, w.agentC, "conversation_answer", answer(w, conv, m2, "A2", 1)), "executed", "", "")
	var logged []Call
	for _, c := range w.fc.Calls() {
		if c.Tool == "conversation_answer" {
			logged = append(logged, c)
		}
	}
	if len(logged) != 2 || logged[0].Status != "failed" || logged[0].Code != codeConflict || logged[1].Status != "executed" ||
		logged[1].IdempotencyKey != "answer:"+conv+":"+m2+":1" || logged[1].ActorID != w.tutorA.ID {
		t.Errorf("the log: %+v", logged)
	}
}

func TestREST(t *testing.T) {
	w := newFakeWorld(t, Options{})
	r := w.rest()
	ctx := context.Background()
	conv, m1 := w.ask(0, "Q")
	do := func(method, path string, body any, key string) httpAnswer {
		t.Helper()
		a, err := r.do(ctx, method, path, body, key)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if a := do("GET", "/v1/courses/"+w.co.ID+"/conversations/inbox?limit=1", nil, ""); a.Status != 200 || !strings.Contains(string(a.Body), conv) {
		t.Errorf("inbox: %d %s", a.Status, a.Body)
	}
	if a := do("GET", "/v1/courses/"+w.co.ID+"/conversations/inbox?limit=many", nil, ""); a.Status != 400 {
		t.Errorf("a limit that is not a number: %d %s", a.Status, a.Body)
	}
	body := map[string]any{"course_id": w.co.ID, "conversation_id": conv, "in_reply_to_message_id": m1, "body": "A"}
	if a := do("POST", "/v1/tools/conversation.answer", body, "k1"); a.Status != 200 || a.Header.Get("Idempotency-Replayed") != "" {
		t.Errorf("by name: %d %s", a.Status, a.Body)
	}
	if a := do("POST", "/v1/tools/conversation.answer", body, "k1"); a.Status != 200 || a.Header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("replayed by name: %d %v", a.Status, a.Header)
	}
	if a := do("POST", "/v1/courses/"+w.co.ID+"/conversations/"+conv+"/answer",
		map[string]any{"course_id": w.fc.AddCourse("OTHER").ID, "in_reply_to_message_id": m1, "body": "A"}, "k2"); a.Status != 400 {
		t.Errorf("a body that disagrees with the path: %d %s", a.Status, a.Body)
	}
	if a := do("GET", "/v1/nowhere", nil, ""); a.Status != 404 || !strings.Contains(string(a.Body), codeNotFound) {
		t.Errorf("no route: %d %s", a.Status, a.Body)
	}
	if a := do("GET", "/v1/tools", nil, ""); a.Status != 200 || !bytes.Equal(a.Body, catalogueJSON) {
		t.Errorf("GET /v1/tools: %d", a.Status)
	}
	if a := do("GET", "/healthz", nil, ""); a.Status != 200 {
		t.Errorf("healthz: %d", a.Status)
	}
}

func TestScreened(t *testing.T) {
	w := newFakeWorld(t, Options{})
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		body   string
		status int
	}{
		{"a batch", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, 400},
		{"a long id", `{"jsonrpc":"2.0","id":"` + strings.Repeat("x", 300) + `","method":"tools/list"}`, 400},
		{"subscriptions/listen", `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}`, 404},
		{"tools/list", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 200},
	} {
		a, err := w.agentC.postRaw(ctx, []byte(c.body))
		if err != nil {
			t.Fatal(err)
		}
		if a.Status != c.status {
			t.Errorf("%s: %d %s", c.name, a.Status, a.Body)
		}
	}
}

func TestControlsFollowCoresRules(t *testing.T) {
	w := newFakeWorld(t, Options{})
	conv, _ := w.ask(0, "Q")
	w.closeAsOpener(conv, "done")
	_, err := w.fc.FollowUp(conv, "more")
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != "closed" || refused.Tool != "conversation_ask" {
		t.Errorf("a follow-up in a closed conversation: %v", err)
	}
	if _, _, err := w.fc.Ask(w.co.ID, w.seats[0].ID, w.mori.ID, "Q"); !errors.As(err, &refused) || refused.Reason != "conversations_are_with_agents" {
		t.Errorf("a student asking an instructor, a person: %v", err)
	}
	if _, _, err := w.fc.Ask(w.co.ID, w.seats[1].ID, w.ownSeat(), "Q"); !errors.As(err, &refused) || refused.Reason != "not_addressable" {
		t.Errorf("Ken asking Yuki's own agent: %v", err)
	}
	for _, c := range []struct {
		name string
		err  error
	}{
		{"an owned agent without its principal", func() error {
			a, _ := w.fc.AddAgent("stray", w.people[0].ID)
			_, err := w.fc.Seat(a.ID, w.co.ID, SeatOptions{Preset: "delegate"})
			return err
		}()},
		{"a person as a delegate", func() error {
			p := w.fc.AddPerson("Nobody")
			_, err := w.fc.Seat(p.ID, w.co.ID, SeatOptions{Preset: "delegate", Principal: w.seats[0].ID})
			return err
		}()},
		{"seated twice", func() error {
			_, err := w.fc.Seat(w.people[0].ID, w.co.ID, SeatOptions{Preset: "student"})
			return err
		}()},
		{"no such preset", func() error {
			p := w.fc.AddPerson("Nobody else")
			_, err := w.fc.Seat(p.ID, w.co.ID, SeatOptions{Preset: "dean"})
			return err
		}()},
		{"answers_course without a manager", func() error {
			a, _ := w.fc.AddAgent("helper", w.people[1].ID)
			yes := true
			_, err := w.fc.Seat(a.ID, w.co.ID, SeatOptions{Preset: "delegate", Principal: w.seats[1].ID, AnswersCourse: &yes})
			return err
		}()},
		{"pausing a paused seat", func() error {
			w.pauseStudent(1)
			return w.fc.PauseSeat(w.seats[1].ID)
		}()},
		{"a level that is not one", w.fc.SetLevel(w.seats[0].ID, permDocumentRead, "sometimes")},
	} {
		if c.err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}
}

// TestAnswersGivenAsFarAsDecided: no person holds conversation_answer, and
// an instructor, who decides actions, still seats an agent that answers
// (Core's grantable); a seat that neither answers nor decides actions gives
// no answers.
func TestAnswersGivenAsFarAsDecided(t *testing.T) {
	w := newFakeWorld(t, Options{})
	add := func(c *mcpClient, key string) toolAnswer {
		t.Helper()
		helper := w.fc.AddUnownedAgent("Helper " + key)
		return mustCall(t, c, "member_add", inCourseArgs(w, "idempotency_key", key, "actor_id", helper.ID, "preset", "course_tutor"))
	}
	sato := w.as("sato")
	made := add(sato, "add:1")
	wantEnvelope(t, made, "executed", "", "")
	got := mustCall(t, sato, "member_get", inCourseArgs(w, "member_id", made.str("result", "member_id")))
	if level := got.str("result", "perms", permConversationAnswer); level != "autonomous" {
		t.Errorf("the agent Sato seated answers at %q", level)
	}
	_, registrar := w.registrar(map[string]string{permMemberManage: "autonomous"})
	if refused := add(registrar, "add:2"); refused.status() != "failed" || refused.str("error", "details", "permission") != permConversationAnswer {
		t.Errorf("a seat that decides nothing gave answers: %s", refused.Text)
	}
}

func TestConcurrentAnswersPostOnce(t *testing.T) {
	w := newFakeWorld(t, Options{})
	conv, m1 := w.ask(0, "Q")
	var wg sync.WaitGroup
	statuses := make([]string, 8)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := w.agentC.call(context.Background(), "conversation_answer", answer(w, conv, m1, fmt.Sprintf("A%d", i), i+1))
			if err != nil {
				t.Error(err)
				return
			}
			statuses[i] = a.status() + " " + a.str("error", "details", "reason")
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.fc.Messages(conv)
			_ = w.fc.Calls()
		}()
	}
	wg.Wait()
	executed := 0
	for _, s := range statuses {
		switch s {
		case "executed ":
			executed++
		case "failed already_answered":
		default:
			t.Errorf("an answer came back %q", s)
		}
	}
	if executed != 1 || len(w.fc.Answers(conv)) != 1 {
		t.Errorf("%d executed, %d answers: %v", executed, len(w.fc.Answers(conv)), statuses)
	}
}

func TestMessagesPaging(t *testing.T) {
	w := newFakeWorld(t, Options{})
	conv, _ := w.ask(0, "1")
	for i := 2; i <= 230; i++ {
		w.followUp(conv, strconv.Itoa(i))
	}
	seqs := func(args map[string]any) (first, last, n int, more bool) {
		t.Helper()
		a := mustCall(t, w.agentC, "conversation_messages", args)
		msgs := list(a, "messages")
		if len(msgs) == 0 {
			return 0, 0, 0, a.Structured["result"].(map[string]any)["more"] == true
		}
		seq := func(m any) int {
			n, _ := m.(map[string]any)["seq"].(json.Number).Int64()
			return int(n)
		}
		return seq(msgs[0]), seq(msgs[len(msgs)-1]), len(msgs), a.Structured["result"].(map[string]any)["more"] == true
	}
	for _, c := range []struct {
		name           string
		args           map[string]any
		first, last, n int
		more           bool
	}{
		{"the newest 50 by default, oldest first", inCourseArgs(w, "conversation_id", conv), 181, 230, 50, true},
		{"at most 200", inCourseArgs(w, "conversation_id", conv, "limit", 500), 31, 230, 200, true},
		{"after a seq", inCourseArgs(w, "conversation_id", conv, "after_seq", 225), 226, 230, 5, false},
		{"after a seq, a page", inCourseArgs(w, "conversation_id", conv, "after_seq", 0, "limit", 10), 1, 10, 10, true},
		{"before a seq", inCourseArgs(w, "conversation_id", conv, "before_seq", 11, "limit", 3), 8, 10, 3, true},
		{"before the first", inCourseArgs(w, "conversation_id", conv, "before_seq", 1), 0, 0, 0, false},
	} {
		first, last, n, more := seqs(c.args)
		if first != c.first || last != c.last || n != c.n || more != c.more {
			t.Errorf("%s: seq %d to %d, %d messages, more %v; want %d to %d, %d, %v", c.name, first, last, n, more, c.first, c.last, c.n, c.more)
		}
	}
}

func TestReview(t *testing.T) {
	w := newFakeWorld(t, Options{})
	w.setTutorLevel("pending_review")
	conv, m1 := w.ask(0, "Q")
	args := answer(w, conv, m1, "A", 1)
	a := mustCall(t, w.agentC, "conversation_answer", args)
	if a.status() != "executed" || a.str("review_state") != "pending" {
		t.Fatalf("an answer at pending_review: %s", a.Text)
	}
	id := a.str("action_id")
	replayed := func() string {
		t.Helper()
		return mustCall(t, w.agentC, "conversation_answer", args).str("review_state")
	}
	if err := w.fc.Review(id, "escalated"); err != nil {
		t.Fatal(err)
	}
	if got := replayed(); got != "escalated" {
		t.Errorf("replayed after an escalation: %s", got)
	}
	// Mori escalated it, and may not close it; Sato owns the agent, and,
	// answering at autonomous himself, closes it as its owner.
	morisReview := mustCall(t, w.as("mori"), "action_review", inCourseArgs(w, "action_id", id, "outcome", "reviewed", "idempotency_key", "r1"))
	wantEnvelope(t, morisReview, "failed", codeForbidden, "")
	if err := w.fc.Review(id, "reviewed"); err != nil {
		t.Fatal(err)
	}
	if got := replayed(); got != "reviewed" {
		t.Errorf("replayed after a review: %s", got)
	}
	var refused *RefusedError
	if err := w.fc.Review(id, "reviewed"); !errors.As(err, &refused) || refused.Code != codeConflict {
		t.Errorf("reviewed twice: %v", err)
	}
	var seen []string
	for _, e := range list(mustCall(t, w.agentC, "event_list", inCourseArgs(w, "since_seq", 0)), "events") {
		if e := e.(map[string]any); e["action_id"] == id && strings.HasPrefix(e["type"].(string), "action.") {
			seen = append(seen, e["type"].(string))
		}
	}
	if fmt.Sprint(seen) != "[action.escalated action.reviewed]" {
		t.Errorf("the agent's own feed: %v", seen)
	}
	mine := list(mustCall(t, w.agentC, "action_list_mine", inCourseArgs(w)), "actions")
	if row := mine[0].(map[string]any); row["review_state"] != "reviewed" || row["reviewed_by_member_id"] == nil || row["reviewed_at"] == nil {
		t.Errorf("action_list_mine: %v", row)
	}
}

// A decision that confirming would refuse is refused at once, never
// proposed for a person to confirm and fail (AIShie-Core #60): an agent
// whose decisions a person confirms, deciding its own proposal; and an
// owner whose agent's proposal approving would now refuse is refused that
// refusal (owner_would_be_refused), whether they approve or reject, while
// someone else may still reject it.
func TestDecisionsApprovingWouldRefuseAreRefusedAtOnce(t *testing.T) {
	w := newFakeWorld(t, Options{})
	w.setTutorLevel("confirm_required")
	conv, m1 := w.ask(0, "Q")
	p := mustCall(t, w.agentC, "conversation_answer", answer(w, conv, m1, "A", 1))
	wantEnvelope(t, p, "proposed", "", "")
	id := p.str("action_id")
	// The tutor decides actions as far as a person confirms them, for a
	// while (Yuki may not address it meanwhile: it can do what she cannot).
	w.ok(w.fc.SetLevel(w.tutorM.ID, permActionDecide, "confirm_required"))
	decide := func(c *mcpClient, decision, key string) toolAnswer {
		return mustCall(t, c, "action_decide", inCourseArgs(w, "action_id", id, "decision", decision, "idempotency_key", key, "reason", "r"))
	}
	if d := decide(w.agentC, "approve", "d1"); d.status() != "failed" || d.str("error", "code") != codeForbidden ||
		!strings.Contains(d.str("error", "message"), "nobody decides their own proposal") {
		t.Errorf("the agent decides its own proposal: %s", d.Text)
	}
	if got := w.fc.Proposals(w.co.ID); len(got) != 1 || got[0].ActionID != id {
		t.Errorf("proposals: %+v", got)
	}
	// Not a decision at all is refused as the arguments are read, at any
	// level, recording nothing.
	wantEnvelope(t, decide(w.agentC, "maybe", "d2"), "error", codeInvalidArgument, "")
	w.ok(w.fc.SetLevel(w.tutorM.ID, permActionDecide, "denied"))

	w.followUp(conv, "Q2")
	for _, decision := range []string{"approve", "reject"} {
		d := decide(w.as("sato"), decision, "owner:"+decision)
		wantEnvelope(t, d, "failed", codeForbidden, "owner_would_be_refused")
		refusal, _ := d.Structured["error"].(map[string]any)["details"].(map[string]any)["refusal"].(map[string]any)
		if refusal["code"] != codeConflict || refusal["details"].(map[string]any)["reason"] != "moved_on" {
			t.Errorf("its owner %ss: %s", decision, d.Text)
		}
	}
	if d := decide(w.as("mori"), "reject", "mori"); d.status() != "executed" || d.str("result", "outcome") != "rejected" {
		t.Errorf("someone else rejects it: %s", d.Text)
	}
}

func TestNobodyDecidesTheirOwnAtOneRemove(t *testing.T) {
	w := newFakeWorld(t, Options{})
	w.setTutorLevel("confirm_required")
	conv, m1 := w.ask(0, "Q")
	p := mustCall(t, w.agentC, "conversation_answer", answer(w, conv, m1, "A", 1))
	wantEnvelope(t, p, "proposed", "", "")
	// A TA whose decisions a person confirms approves the tutor's answer.
	ta := w.fc.AddPerson("Ito")
	if _, err := w.fc.Seat(ta.ID, w.co.ID, SeatOptions{Preset: "ta", Perms: map[string]string{permActionDecide: "confirm_required"}}); err != nil {
		t.Fatal(err)
	}
	taC := w.client(ta.Token)
	d := mustCall(t, taC, "action_decide", inCourseArgs(w, "action_id", p.str("action_id"), "decision", "approve", "idempotency_key", "d1"))
	wantEnvelope(t, d, "proposed", "", "")
	// Sato owns the tutor, so confirming the approval would carry out his
	// own agent's proposal.
	c := mustCall(t, w.as("sato"), "action_decide", inCourseArgs(w, "action_id", d.str("action_id"), "decision", "approve", "idempotency_key", "d2"))
	wantEnvelope(t, c, "failed", codeForbidden, "")
	if !strings.Contains(c.str("error", "message"), "at one remove") {
		t.Errorf("the refusal: %s", c.Text)
	}
	// Mori may; the decision is carried out, and with it the answer.
	c = mustCall(t, w.as("mori"), "action_decide", inCourseArgs(w, "action_id", d.str("action_id"), "decision", "approve", "idempotency_key", "d3"))
	if c.status() != "executed" || c.str("result", "outcome") != "executed" {
		t.Fatalf("Mori confirms the TA's approval: %s", c.Text)
	}
	if n := len(w.fc.Answers(conv)); n != 1 {
		t.Errorf("%d answers posted", n)
	}
}

func TestDocumentRules(t *testing.T) {
	w := newFakeWorld(t, Options{})
	t.Run("kind must be a course's", func(t *testing.T) {
		wantEnvelope(t, mustCall(t, w.agentC, "document_list", inCourseArgs(w, "kind", "submission")), "error", codeInvalidArgument, "")
	})
	t.Run("document_read governs the list, or the named kind's permission", func(t *testing.T) {
		p := w.fc.AddPerson("Rubric reader")
		if _, err := w.fc.Seat(p.ID, w.co.ID, SeatOptions{Perms: map[string]string{permRubricRead: "autonomous"}}); err != nil {
			t.Fatal(err)
		}
		c := w.client(p.Token)
		wantEnvelope(t, mustCall(t, c, "document_list", inCourseArgs(w)), "denied", codeForbidden, reasonPermDenied)
		if docs := list(mustCall(t, c, "document_list", inCourseArgs(w, "kind", "rubric")), "documents"); len(docs) != 0 {
			t.Errorf("rubrics: %v", docs)
		}
	})
	t.Run("instructions follow their assignment", func(t *testing.T) {
		p := w.fc.AddPerson("Grader for nothing")
		if _, err := w.fc.Seat(p.ID, w.co.ID, SeatOptions{Preset: "grader", AssignmentScope: scopeListed}); err != nil {
			t.Fatal(err)
		}
		c := w.client(p.Token)
		for _, d := range list(mustCall(t, c, "document_list", inCourseArgs(w)), "documents") {
			if d.(map[string]any)["kind"] == kindInstructions {
				t.Errorf("instructions of an assignment out of scope listed: %v", d)
			}
		}
		wantEnvelope(t, mustCall(t, c, "document_get", inCourseArgs(w, "document_id", w.co.InstructionsID)), "error", codeNotFound, "")
		wantEnvelope(t, mustCall(t, c, "document_get", inCourseArgs(w, "document_id", w.co.SyllabusID)), "executed", "", "")
		wantEnvelope(t, mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", w.co.InstructionsID)), "executed", "", "")
	})
	t.Run("a submission's body comes with submission_get, not the list", func(t *testing.T) {
		work, err := w.fc.AddWork(w.co.ID, w.seats[0].ID, "My answers", "90")
		if err != nil {
			t.Fatal(err)
		}
		own := w.ownAgent()
		subs := list(mustCall(t, own, "submission_list", inCourseArgs(w)), "submissions")
		if len(subs) != 1 || subs[0].(map[string]any)["body"] != nil {
			t.Errorf("submission_list: %v", subs)
		}
		if a := mustCall(t, own, "submission_get", inCourseArgs(w, "submission_id", work.SubmissionID)); a.str("result", "body") != "My answers" {
			t.Errorf("submission_get: %s", a.Text)
		}
	})
}

// TestDocumentCreate is document.create through Core's pipeline, by an
// instructor's own agent, as a model's write through its seat's perms
// reaches Core: proposed at confirm_required and made when a person
// approves it, executed at autonomous, denied at denied; replayed under
// its key, and a conflict under its key with other arguments; the kind's
// permission governing; a draft Sato reads and Yuki does not.
func TestDocumentCreate(t *testing.T) {
	w := newFakeWorld(t, Options{})
	agent, err := w.fc.AddAgent("Sato's assistant", w.satoA.ID)
	if err != nil {
		t.Fatal(err)
	}
	m, err := w.fc.Seat(agent.ID, w.co.ID, SeatOptions{Preset: "delegate", Principal: w.sato.ID,
		Perms: map[string]string{permDocumentWrite: "confirm_required"}})
	if err != nil {
		t.Fatal(err)
	}
	c := w.client(agent.Token)
	create := func(key, title string, more ...any) map[string]any {
		args := inCourseArgs(w, "kind", "material", "title", title, "body_md", "# "+title, "idempotency_key", key)
		for i := 0; i+1 < len(more); i += 2 {
			args[more[i].(string)] = more[i+1]
		}
		return args
	}
	proposed := mustCall(t, c, "document_create", create("tool:x:m:1:1", "Week 1 notes"))
	wantEnvelope(t, proposed, "proposed", "", "")
	if a := mustCall(t, c, "document_create", create("tool:x:m:1:1", "Week 1 notes")); a.status() != "proposed" || a.Structured["replayed"] != true ||
		a.str("action_id") != proposed.str("action_id") {
		t.Errorf("the replay: %s", a.Text)
	}
	wantEnvelope(t, mustCall(t, c, "document_create", create("tool:x:m:1:1", "Week 2 notes")), "error", codeIdempotencyConflict, "")
	if docs := w.fc.Documents(w.co.ID); len(docs) != 3 {
		t.Fatalf("a proposal made a document: %+v", docs)
	}
	if out, err := w.fc.Approve(proposed.str("action_id")); err != nil || out != "executed" {
		t.Fatalf("approved: %s %v", out, err)
	}
	docs := w.fc.Documents(w.co.ID)
	if len(docs) != 4 || docs[3].Title != "Week 1 notes" || !docs[3].Draft || docs[3].BodyMD != "# Week 1 notes" || docs[3].AuthorMemberID != m.ID {
		t.Fatalf("the approved document: %+v", docs)
	}

	w.ok(w.fc.SetLevel(m.ID, permDocumentWrite, "autonomous"))
	made := mustCall(t, c, "document_create", create("tool:x:m:2:1", "Week 3 notes", "sort_order", 3))
	wantEnvelope(t, made, "executed", "", "")
	id := made.str("result", "document_id")
	if id == "" || made.str("result", "version_id") == "" {
		t.Fatalf("executed: %s", made.Text)
	}
	empty := mustCall(t, c, "document_create", inCourseArgs(w, "kind", "rubric", "title", "Rubric", "idempotency_key", "tool:x:m:2:2"))
	if wantEnvelope(t, empty, "executed", "", ""); empty.str("result", "version_id") != "" {
		t.Errorf("a document made without text has a version: %s", empty.Text)
	}
	// A title of spaces is refused as the arguments are read (AIShie-Core
	// #60), recording nothing.
	wantEnvelope(t, mustCall(t, c, "document_create", create("tool:x:m:2:3", "  ")), "error", codeInvalidArgument, "")
	wantEnvelope(t, mustCall(t, c, "document_create", create("tool:x:m:2:4", "Exam", "kind", "exam")), "error", codeInvalidArgument, "")
	// A submission's file needs its draft, as Core resolves it.
	wantEnvelope(t, mustCall(t, c, "document_create", create("tool:x:m:2:5", "Mine", "kind", "submission")), "error", codeInvalidArgument, "")
	wantEnvelope(t, mustCall(t, c, "document_create", create("tool:x:m:2:6", "No key", "idempotency_key", "")), "error", codeInvalidArgument, "")

	// Sato reads the draft; Yuki does not see it.
	wantEnvelope(t, mustCall(t, w.as("sato"), "document_get", inCourseArgs(w, "document_id", id)), "executed", "", "")
	wantEnvelope(t, mustCall(t, w.as("yuki"), "document_get", inCourseArgs(w, "document_id", id)), "error", codeNotFound, "")
	for _, d := range list(mustCall(t, w.as("yuki"), "document_list", inCourseArgs(w)), "documents") {
		if d.(map[string]any)["id"] == id {
			t.Error("Yuki lists a draft")
		}
	}

	w.ok(w.fc.SetLevel(m.ID, permDocumentWrite, "denied"))
	wantEnvelope(t, mustCall(t, c, "document_create", create("tool:x:m:3:1", "Week 4 notes")), "denied", codeForbidden, reasonPermDenied)
}

func TestAStudentSeatListsItself(t *testing.T) {
	fc := New(Options{})
	co := fc.AddCourse("CS101")
	sato := fc.AddPerson("Sato")
	if _, err := fc.Seat(sato.ID, co.ID, SeatOptions{Preset: "instructor"}); err != nil {
		t.Fatal(err)
	}
	p := fc.AddPerson("Auditor")
	// No preset: a student's role and listed scope, every level denied but
	// those named.
	m, err := fc.Seat(p.ID, co.ID, SeatOptions{Perms: map[string]string{permDocumentRead: "autonomous", permSubmissionRead: "autonomous"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fc.AddWork(co.ID, m.ID, "mine", "50"); err != nil {
		t.Fatal(err)
	}
	c := newMCPClient(httptestServer(t, fc), p.Token, nil)
	if h, err := c.initialize(context.Background()); err != nil || h.Status != http.StatusOK {
		t.Fatalf("initialize: %v %d %s", err, h.Status, h.Body)
	}
	if subs := list(mustCall(t, c, "submission_list", map[string]any{"course_id": co.ID}), "submissions"); len(subs) != 1 {
		t.Errorf("a student's own work: %v", subs)
	}
}

// me_get names the person who owns an agent, and nobody for a person or an
// agent nobody owns, and an agent's hosting; tools/list lists neither
// service's tools, which are REST's alone.
func TestOwners(t *testing.T) {
	ctx := context.Background()
	fc := New(Options{})
	srv := httptest.NewServer(fc.Handler())
	t.Cleanup(srv.Close)
	me := func(token string) map[string]any {
		t.Helper()
		c := newMCPClient(srv.URL, token, nil)
		if h, err := c.initialize(ctx); err != nil || h.Status != http.StatusOK {
			t.Fatalf("initialize: %v %d", err, h.Status)
		}
		res, _ := mustCall(t, c, "me_get", map[string]any{}).Structured["result"].(map[string]any)
		return res
	}
	yuki := fc.AddPerson("Yuki")
	helper, err := fc.AddAgent("Yuki's helper", yuki.ID)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := fc.AddMCPAgent("Yuki's tools", yuki.ID)
	if err != nil {
		t.Fatal(err)
	}
	registrar := fc.AddUnownedAgent("Registrar")
	for _, c := range []struct {
		name, token, owner, hosting string
	}{
		{"a person", yuki.Token, "", ""},
		{"a runtime agent", helper.Token, yuki.ID, hostingRuntime},
		{"an mcp agent", tools.Token, yuki.ID, hostingMCP},
		{"an agent nobody owns", registrar.Token, "", hostingRuntime},
	} {
		res := me(c.token)
		owner, _ := res["owner_actor_id"].(string)
		hosting, _ := res["hosting"].(string)
		if owner != c.owner || hosting != c.hosting {
			t.Errorf("%s: owner %q, hosting %q", c.name, owner, hosting)
		}
	}
	c := newMCPClient(srv.URL, yuki.Token, nil)
	l, err := c.post(ctx, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err != nil || json.Unmarshal(l.Body, &list) != nil || len(list.Result.Tools) != 154 {
		t.Fatalf("tools/list: %v %d %d", err, l.Status, len(list.Result.Tools))
	}
	for _, tl := range list.Result.Tools {
		if def := fc.cat.byMCP[tl.Name]; def == nil || def.restOnly {
			t.Errorf("tools/list lists %s", tl.Name)
		}
	}
}

// TestAddFile: a file added to a course is a published material, its
// version the file, which document_get gives a download_url for and the
// fake serves as it was given, with its type; document_versions lists it
// for whoever reads drafts; without a type recorded, none is served.
func TestAddFile(t *testing.T) {
	const pptx = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	w := newFakeWorld(t, Options{})
	deck := []byte("PK\x03\x04 a deck of slides")
	id, err := w.fc.AddFile(w.co.ID, "Week 3 slides", pptx, deck)
	if err != nil {
		t.Fatal(err)
	}
	untyped, err := w.fc.AddFile(w.co.ID, "Notes", "", []byte("plain"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.fc.AddFile("0192f3c1-0000-7000-8000-00000000abcd", "x", "", nil); err == nil {
		t.Error("a file was added to no course")
	}
	for _, c := range []struct {
		id, ct string
		data   []byte
	}{{id, pptx, deck}, {untyped, "", []byte("plain")}} {
		a := mustCall(t, w.agentC, "document_get", inCourseArgs(w, "document_id", c.id))
		url := a.str("result", "version", "files", "0", "download_url")
		if !strings.HasPrefix(url, w.srv.URL+blobPath) || a.str("result", "version", "files", "0", "content_type") != c.ct ||
			a.str("result", "status") != "active" {
			t.Fatalf("document_get: %s", a.Text)
		}
		resp, err := w.srv.Client().Get(url)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Equal(buf.Bytes(), c.data) || resp.Header.Get("Content-Type") != c.ct && c.ct != "" {
			t.Errorf("download: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), buf.String())
		}
	}
	if docs := list(mustCall(t, w.agentC, "document_list", inCourseArgs(w)), "documents"); len(docs) != 5 {
		t.Errorf("%d documents, want the three canned and the two added", len(docs))
	}
	v := mustCall(t, w.as("sato"), "document_versions", inCourseArgs(w, "document_id", id))
	versions := list(v, "versions")
	if len(versions) != 1 {
		t.Fatalf("versions: %s", v.Text)
	}
	got := versions[0].(map[string]any)
	files, _ := got["files"].([]any)
	if len(files) != 1 || got["published"] != true || got["has_file"] != nil || got["content_type"] != nil {
		t.Fatalf("version %v", got)
	}
	if f := files[0].(map[string]any); f["content_type"] != pptx || fmt.Sprint(f["byte_size"]) != fmt.Sprint(len(deck)) {
		t.Errorf("its file %v", f)
	}
}

// isRefusal reports whether err is a control Core refused for reason.
func isRefusal(err error, reason string) bool {
	var re *RefusedError
	return errors.As(err, &re) && re.Reason == reason
}
