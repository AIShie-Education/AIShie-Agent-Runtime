package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// serviceCalls counts the calls of the agent runtime's tool Core was
// made.
func (h *hostWorld) serviceCalls(tool string) int {
	n := 0
	for _, c := range h.fc.Calls() {
		if c.Tool == tool {
			n++
		}
	}
	return n
}

// inspect says what an agent of the caller's is in Core, by its id, asked
// with the runtime's own credential: whether it may be hosted here, and
// why not, and whether it is; another's, nobody's, a person's or no agent
// at all is 404, as Core's check_owner says nothing of them. Nothing is
// written, and each request is audited.
func TestInspect(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	a := h.call("POST", "agents/inspect", h.yuki, agentBody(h.helper.ID))
	wantSecured(t, a, "no-store")
	var ins Inspection
	a.decode(t, &ins)
	if a.code != http.StatusOK || ins.CoreActorID != h.helper.ID || ins.OwnerActorID != h.yuki.ID || ins.DisplayName != "Yuki's helper" ||
		ins.Hosting != core.HostingRuntime || !ins.Hostable || ins.Reason != nil || ins.LiveSeats != 1 || !ins.SiteChat || ins.Hosted != nil {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if !strings.Contains(a.body, `"reason":null`) || !strings.Contains(a.body, `"hosted":null`) || strings.Contains(a.body, "token") {
		t.Errorf("the answer: %s", a.body)
	}
	if n, err := h.st.HostedAgents(context.Background()); err != nil || len(n) != 0 {
		t.Errorf("inspect wrote %+v %v", n, err)
	}
	if ev := h.events("agent.inspect"); len(ev) != 1 || ev[0].Outcome != "ok" || ev[0].TargetType != "core_actor" || ev[0].TargetID != h.helper.ID {
		t.Errorf("the audit: %+v", ev)
	}
	// In any case, as Core writes it.
	a = h.call("POST", "agents/inspect", h.yuki, agentBody(strings.ToUpper(h.helper.ID)))
	if a.decode(t, &ins); a.code != http.StatusOK || ins.CoreActorID != h.helper.ID {
		t.Errorf("an id in upper case: %d %s", a.code, a.body)
	}

	tools, err := h.fc.AddMCPAgent("Yuki's tools", h.yuki.ID)
	h.ok(err)
	h.tokens = append(h.tokens, tools.Token)
	suspended, err := h.fc.AddAgent("Suspended", h.yuki.ID)
	h.ok(err)
	h.tokens = append(h.tokens, suspended.Token)
	h.ok(h.fc.SuspendActor(suspended.ID))
	yaml, err := h.fc.AddAgent("Yuki's operator agent", h.yuki.ID)
	h.ok(err)
	h.tokens = append(h.tokens, yaml.Token)
	h.actors.mu.Lock()
	h.actors.yaml[yaml.ID] = "yuki-yaml"
	h.actors.mu.Unlock()
	for _, tc := range []struct {
		name, id, reason, hosting string
	}{
		{"an mcp agent", tools.ID, ReasonMCPAgent, core.HostingMCP},
		{"a suspended agent", suspended.ID, ReasonAgentSuspended, core.HostingRuntime},
		{"an agent the operator runs", yaml.ID, ReasonOperatorAgent, core.HostingRuntime},
	} {
		var got Inspection
		a := h.call("POST", "agents/inspect", h.yuki, agentBody(tc.id))
		if a.decode(t, &got); a.code != http.StatusOK || got.Hostable || got.Reason == nil || *got.Reason != tc.reason || got.Hosting != tc.hosting {
			t.Errorf("%s: %d %s", tc.name, a.code, a.body)
		}
	}

	kens, err := h.fc.AddAgent("Ken's helper", h.ken.ID)
	h.ok(err)
	h.tokens = append(h.tokens, kens.Token)
	unowned := h.fc.AddUnownedAgent("Nobody's")
	h.tokens = append(h.tokens, unowned.Token)
	for _, tc := range []struct {
		name, body string
		code       int
		reason     string
	}{
		{"another's agent", agentBody(kens.ID), 404, ReasonAgentNotFound},
		{"an agent nobody owns", agentBody(unowned.ID), 404, ReasonAgentNotFound},
		{"a person", agentBody(h.ken.ID), 404, ReasonAgentNotFound},
		{"no one", agentBody("0192f3c1-0000-7000-8000-00000000abcd"), 404, ReasonAgentNotFound},
		{"not a UUID", agentBody("agent-1"), 400, ReasonInvalidField},
		{"a UUID braced", agentBody("{" + h.helper.ID + "}"), 400, ReasonInvalidField},
		{"no id", `{}`, 400, ReasonMissingField},
		{"a token", `{"token":"` + h.helper.Token + `"}`, 400, ReasonUnknownField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.call("POST", "agents/inspect", h.yuki, tc.body)
			wantSecured(t, a, "no-store")
			wantRefused(t, a, tc.code, map[int]string{400: CodeInvalidArgument, 404: CodeNotFound}[tc.code], tc.reason)
			ev := h.events("agent.inspect")
			if len(ev) == 0 || ev[len(ev)-1].Outcome != tc.reason || ev[len(ev)-1].ActorID != h.yuki.ID {
				t.Errorf("the audit: %+v", ev)
			}
		})
	}

	// Hosted here: by Yuki, or by someone else.
	v := h.host(h.yuki, h.helper.ID)
	a = h.call("POST", "agents/inspect", h.yuki, agentBody(h.helper.ID))
	a.decode(t, &ins)
	if ins.Hosted == nil || !ins.Hosted.ByYou || ins.Hosted.ID == nil || *ins.Hosted.ID != v.ID {
		t.Errorf("hosted by Yuki: %s", a.body)
	}
	h.noSecrets(a)
}

// inspect, and POST /agents, refuse as Core cannot be asked: Core not
// answering (core_unavailable); the runtime with no credential of its own,
// or one Core refuses (runtime_misconfigured); a Core from before hosting
// by id (core_too_old).
func TestInspectCoreRefusals(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	h.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == core.ToolRuntimeCheckOwner {
			return &fakecore.Injection{Status: http.StatusBadGateway}
		}
		return nil
	})
	for _, path := range []string{"agents/inspect", "agents"} {
		wantRefused(t, h.call("POST", path, h.yuki, agentBody(h.helper.ID)), 503, CodeUnavailable, ReasonCoreUnavailable)
	}
	h.fc.Inject(nil)

	revoked := h.fc.IssueRuntimeServiceToken("revoked")
	h.ok(h.fc.RevokeServiceToken(revoked.CredentialID))
	transcriber := h.fc.IssueServiceToken("transcriber")
	h.tokens = append(h.tokens, revoked.Token, transcriber.Token)
	for name, rs := range map[string]*core.RuntimeService{
		"no credential":                 nil,
		"a revoked credential":          h.runtime(revoked.Token),
		"the transcription service's":   h.runtime(transcriber.Token),
		"a credential not a service's":  h.runtime(h.helper.Token),
		"a credential not read at hand": core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: h.srv.URL, Once: true, Credential: func(context.Context) (string, error) { return "", errors.New("secrets: no such file") }})),
	} {
		h.s.o.Runtime = rs
		for _, path := range []string{"agents/inspect", "agents"} {
			a := h.call("POST", path, h.yuki, agentBody(h.helper.ID))
			if e := wantRefused(t, a, 503, CodeUnavailable, ReasonRuntimeMisconfigured); e.Message == "" {
				t.Errorf("%s: %s", name, a.body)
			}
		}
	}
	if rows, _ := h.st.HostedAgents(context.Background()); len(rows) != 0 {
		t.Errorf("hosted without a credential: %+v", rows)
	}

	old := newHostWorld(t, fakecore.Options{WithoutHosting: true}, nil)
	for _, path := range []string{"agents/inspect", "agents"} {
		wantRefused(t, old.call("POST", path, old.yuki, agentBody(old.helper.ID)), 422, CodeFailedPrecondition, ReasonCoreTooOld)
	}
	h.noSecrets()
}

// POST /agents hosts the caller's agent by its id: 201, needs_model, its
// row naming the agent and its owner, holding no token (the worker running
// it is issued one once it has a model); asked again, it replays the row;
// every outcome is audited.
func TestHost(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	issues := h.serviceCalls(core.ToolRuntimeIssueToken)
	a := h.call("POST", "agents", h.yuki, agentBody(h.helper.ID))
	wantSecured(t, a, "no-store")
	var v HostedAgent
	a.decode(t, &v)
	if a.code != http.StatusCreated || !store.IsHostedAgentID(v.ID) || v.Status != StatusNeedsModel || v.Version != 1 ||
		v.CoreActorID != h.helper.ID || v.OwnerActorID != h.yuki.ID || v.DisplayName != "Yuki's helper" || v.Paused || v.Problem != nil ||
		v.Model.Own != nil || v.Model.School != nil || v.OwnKey != nil {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if a.header.Get("Location") != Prefix+"agents/"+v.ID || a.header.Get("ETag") != `"1"` {
		t.Errorf("Location %q, ETag %q", a.header.Get("Location"), a.header.Get("ETag"))
	}
	if len(v.Seats) != 0 || v.SeatsAsOf != nil || v.Today.CostUSD != "0.000000" || !v.Today.Since.Equal(store.UTCDay(at)) {
		t.Errorf("seats and today: %s", a.body)
	}
	if !strings.Contains(a.body, `"model":{"own":null,"school":null}`) || !strings.Contains(a.body, `"problem":null`) ||
		strings.Contains(a.body, `"token"`) {
		t.Errorf("the members: %s", a.body)
	}
	row, err := h.st.HostedAgent(context.Background(), v.ID)
	h.ok(err)
	if row.OwnerActorID != h.yuki.ID || !row.OwnerVerified || row.TenantID != "ten_"+h.yuki.ID || string(row.Settings) != `{}` ||
		row.TokenSecretID != "" || row.TokenIssued {
		t.Errorf("the row: %+v", row)
	}
	if n := h.serviceCalls(core.ToolRuntimeIssueToken); n != issues {
		t.Errorf("the API was issued %d tokens", n-issues)
	}
	ev := h.events("agent.host")
	if len(ev) != 1 || ev[0].Outcome != "ok" || ev[0].TargetID != v.ID || !strings.Contains(string(ev[0].Detail), `"core_actor_id":"`+h.helper.ID) {
		t.Errorf("the audit: %+v", ev)
	}

	// Again: the agent as it is, nothing written.
	b := h.call("POST", "agents", h.yuki, agentBody(h.helper.ID))
	var again HostedAgent
	b.decode(t, &again)
	if b.code != http.StatusOK || b.header.Get("Idempotency-Replayed") != "true" || again.ID != v.ID || again.Version != 1 {
		t.Errorf("a replay: %d %s", b.code, b.body)
	}
	if ev := h.events("agent.host"); len(ev) != 2 || ev[1].Outcome != "ok" || !strings.Contains(string(ev[1].Detail), `"replayed":true`) {
		t.Errorf("the audit: %+v", ev)
	}
	h.noSecrets(a, b)
}

// POST /agents refuses an agent Core says may not be hosted, saying why:
// an mcp agent (mcp_agent), one suspended (agent_suspended), or whose
// owner is (owner_suspended); another's agent is 404; one the operator
// runs is operator_agent; nothing is written.
func TestHostRefuses(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	tools, err := h.fc.AddMCPAgent("Yuki's tools", h.yuki.ID)
	h.ok(err)
	suspended, err := h.fc.AddAgent("Suspended", h.yuki.ID)
	h.ok(err)
	h.ok(h.fc.SuspendActor(suspended.ID))
	kens, err := h.fc.AddAgent("Ken's helper", h.ken.ID)
	h.ok(err)
	yaml, err := h.fc.AddAgent("Yuki's operator agent", h.yuki.ID)
	h.ok(err)
	h.actors.mu.Lock()
	h.actors.yaml[yaml.ID] = "yuki-yaml"
	h.actors.mu.Unlock()
	h.tokens = append(h.tokens, tools.Token, suspended.Token, kens.Token, yaml.Token)
	for _, tc := range []struct {
		name, id     string
		code         int
		errCode      string
		reason       string
		suspendOwner bool
	}{
		{"an mcp agent", tools.ID, 422, CodeFailedPrecondition, ReasonMCPAgent, false},
		{"a suspended agent", suspended.ID, 422, CodeFailedPrecondition, ReasonAgentSuspended, false},
		{"another's agent", kens.ID, 404, CodeNotFound, ReasonAgentNotFound, false},
		{"an agent the operator runs", yaml.ID, 409, CodeConflict, ReasonOperatorAgent, false},
		{"an agent whose owner is suspended", h.helper.ID, 422, CodeFailedPrecondition, ReasonOwnerSuspended, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.suspendOwner {
				h.ok(h.fc.SuspendActor(h.yuki.ID))
				defer func() { h.ok(h.fc.ReactivateActor(h.yuki.ID)) }()
			}
			wantRefused(t, h.call("POST", "agents", h.yuki, agentBody(tc.id)), tc.code, tc.errCode, tc.reason)
			if ev := h.events("agent.host"); len(ev) == 0 || ev[len(ev)-1].Outcome != tc.reason {
				t.Errorf("the audit: %+v", ev)
			}
		})
	}
	if rows, _ := h.st.HostedAgents(context.Background()); len(rows) != 0 {
		t.Errorf("hosted: %+v", rows)
	}
	if h.fc.Hosting(tools.ID) != core.HostingMCP || h.fc.SiteChat(tools.ID) {
		t.Error("the mcp agent changed")
	}

	// No vault to seal the token the worker is issued with: nothing hosted.
	nv := newHostWorld(t, fakecore.Options{}, func(o *Options) { o.Vault = nil })
	nv.s.o.Vault = nil
	wantRefused(t, nv.call("POST", "agents", nv.yuki, agentBody(nv.helper.ID)), 500, CodeInternal, ReasonInternal)
	h.noSecrets()
}

// Hosting one agent several times at once: one is created, the others
// replay it.
func TestHostConcurrently(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Go(func() { codes[i] = h.call("POST", "agents", h.yuki, agentBody(h.helper.ID)).code })
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Errorf("a request answered %d", c)
		}
	}
	if rows, _ := h.st.HostedAgents(context.Background()); created != 1 || len(rows) != 1 {
		t.Errorf("%d created, %d rows", created, len(rows))
	}
}

// An earlier owner's row of an agent Core says is the caller's (one Core
// gave them before an agent's owner was fixed in Core) is taken over: the
// row deleted and purged, the token the runtime held for it revoked, and
// audited; the earlier owner's GET of it is 404 then.
func TestHostTakesOver(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	ctx := context.Background()
	old, err := h.st.CreateHostedAgent(ctx, store.HostedAgent{ID: "agt_earlier", CoreActorID: h.helper.ID, OwnerActorID: h.ken.ID,
		OwnerVerified: true, TenantID: "ten_" + h.ken.ID, DisplayName: "Ken's once", Settings: []byte(`{}`)})
	h.ok(err)
	h.issued(old.ID)
	h.ok(h.st.SetAgentState(ctx, store.AgentState{AgentID: old.ID, State: store.AgentOwnerChanged, Reason: store.ReasonOwnerChanged}))
	v := h.host(h.yuki, h.helper.ID)
	if v.ID == old.ID || v.OwnerActorID != h.yuki.ID {
		t.Errorf("taken over: %+v", v)
	}
	if _, err := h.st.HostedAgent(ctx, old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the earlier row: %v", err)
	}
	if _, err := h.st.AgentState(ctx, old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the earlier row not purged: %v", err)
	}
	if tok := h.fc.RuntimeToken(h.helper.ID); tok.Token != "" {
		t.Error("the earlier row's token is still live")
	}
	ev := h.events("agent.takeover")
	if len(ev) != 1 || ev[0].TargetID != old.ID || ev[0].ActorID != h.yuki.ID || !strings.Contains(string(ev[0].Detail), `"revocation":"revoked"`) {
		t.Errorf("the audit: %+v", ev)
	}
	wantRefused(t, h.call("GET", "agents/"+old.ID, h.ken, ""), 404, CodeNotFound, ReasonAgentNotFound)
	h.noSecrets()
}

// GET /agents and /agents/{id} give the caller's own agents alone; any
// other id, of another's, not there, or of the wrong shape, is 404.
func TestListAndGet(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	mine := h.host(h.yuki, h.helper.ID)
	second, err := h.fc.AddAgent("Yuki's second", h.yuki.ID)
	h.ok(err)
	h.tokens = append(h.tokens, second.Token)
	h.add(time.Second)
	mine2 := h.host(h.yuki, second.ID)
	kens, err := h.fc.AddAgent("Ken's", h.ken.ID)
	h.ok(err)
	h.tokens = append(h.tokens, kens.Token)
	theirs := h.host(h.ken, kens.ID)

	var list AgentList
	a := h.call("GET", "agents", h.yuki, "")
	a.decode(t, &list)
	if a.code != 200 || len(list.Agents) != 2 || list.Agents[0].ID != mine.ID || list.Agents[1].ID != mine2.ID {
		t.Errorf("Yuki's list: %s", a.body)
	}
	a = h.call("GET", "agents/"+mine.ID, h.yuki, "")
	if a.code != 200 || a.header.Get("ETag") != `"1"` {
		t.Errorf("get: %d %v", a.code, a.header)
	}
	for _, id := range []string{theirs.ID, "agt_nothere", "not-an-id", "agt_" + strings.Repeat("x", 61)} {
		wantRefused(t, h.call("GET", "agents/"+id, h.yuki, ""), 404, CodeNotFound, ReasonAgentNotFound)
	}
	var kl AgentList
	h.call("GET", "agents", h.ken, "").decode(t, &kl)
	if len(kl.Agents) != 1 || kl.Agents[0].ID != theirs.ID {
		t.Errorf("Ken's list: %+v", kl)
	}
	var me Me
	h.call("GET", "me", h.yuki, "").decode(t, &me)
	if me.HostedAgents != 2 {
		t.Errorf("/me: %+v", me)
	}
}

// statusOf works a status out of the row and the worker's state: paused
// first, then no model, then a state of an older version (or none) is
// starting, then the state and its reason.
func TestStatusOf(t *testing.T) {
	since := at.Add(-time.Minute)
	row := func(paused bool) *store.HostedAgent { return &store.HostedAgent{Version: 5, Paused: paused} }
	st := func(state, reason string, version int) *store.AgentState {
		return &store.AgentState{State: state, Reason: reason, Detail: "why " + strings.Repeat("x", 600), ConfigVersion: version, UpdatedAt: since}
	}
	for _, tc := range []struct {
		name     string
		row      *store.HostedAgent
		model    bool
		st       *store.AgentState
		status   string
		problem  string
		detailed bool
	}{
		{"paused, whatever the worker says", row(true), true, st(store.AgentRunning, "", 5), StatusPaused, "", false},
		{"paused with no model", row(true), false, nil, StatusPaused, "", false},
		{"no model", row(false), false, st(store.AgentRunning, "", 5), StatusNeedsModel, "", false},
		{"no state yet", row(false), true, nil, StatusStarting, "", false},
		{"a state of an older version", row(false), true, st(store.AgentRunning, "", 4), StatusStarting, "", false},
		{"a version-only change applied", row(false), true, st(store.AgentRunning, "", 5), StatusRunning, "", false},
		{"a state of a newer version", row(false), true, st(store.AgentRunning, "", 6), StatusRunning, "", false},
		{"starting", row(false), true, st(store.AgentStarting, "", 5), StatusStarting, "", false},
		{"a stale paused", row(false), true, st(store.AgentPaused, "", 5), StatusStarting, "", false},
		{"stopped", row(false), true, st(store.AgentStopped, "", 5), StatusStopped, "", false},
		{"unauthorized", row(false), true, st(store.AgentUnauthorized, store.ReasonTokenRefused, 5), StatusNeedsToken, store.ReasonTokenRefused, true},
		{"unauthorized, no reason recorded", row(false), true, st(store.AgentUnauthorized, "", 5), StatusNeedsToken, store.ReasonTokenRefused, true},
		{"owner changed", row(false), true, st(store.AgentOwnerChanged, store.ReasonOwnerChanged, 5), StatusError, store.ReasonOwnerChanged, true},
		{"an error with no reason", row(false), true, st(store.AgentError, "", 5), StatusError, store.ReasonFailing, true},
		{"an error of a reason not the contract's", row(false), true, st(store.AgentError, "boom", 5), StatusError, store.ReasonFailing, true},
		{"an unknown state", row(false), true, st("dreaming", "", 5), StatusStarting, "", false},
	} {
		status, problem := statusOf(tc.row, tc.model, tc.st)
		if status != tc.status || (problem == nil) != (tc.problem == "") {
			t.Errorf("%s: %s %+v", tc.name, status, problem)
			continue
		}
		if problem != nil && (problem.Reason != tc.problem || !problem.Since.Equal(since) || len([]rune(problem.Detail)) > maxDetail) {
			t.Errorf("%s: %+v", tc.name, problem)
		}
	}
	for _, r := range []string{store.ReasonTokenRefused, store.ReasonSettingsRejected, store.ReasonRuntimeMisconfigured,
		store.ReasonOperatorAgent, store.ReasonActorInUse, store.ReasonTokenOtherAgent, store.ReasonOwnerChanged,
		store.ReasonCoreTooOld, store.ReasonAgentSuspended, store.ReasonOwnerSuspended, store.ReasonMCPAgent,
		store.ReasonAgentNotFound, store.ReasonFailing} {
		if _, p := statusOf(row(false), true, st(store.AgentError, r, 5)); p == nil || p.Reason != r {
			t.Errorf("reason %s: %+v", r, p)
		}
	}
}

// An agent's view counts the answers waiting for approval in each current
// seat, and today's answers and cost, in dollars to six places; a seat
// gone is not shown.
func TestViewCountsProposalsAndSpend(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	v := h.host(h.yuki, h.helper.ID)
	ctx := context.Background()
	member := "seat-1"
	h.ok(h.st.SeatSeen(ctx, store.SeatRef{AgentID: v.ID, MemberID: member, CourseID: h.co.ID, CourseCode: "CS101", Status: "active", SeenAt: at}))
	for i, state := range []store.AttemptState{store.AttemptProposed, store.AttemptProposed, store.AttemptSending} {
		key := "answer:c1:m" + string(rune('1'+i)) + ":1"
		_, err := h.st.PutAttempt(ctx, store.Attempt{Key: key, AgentID: v.ID, MemberID: member, CourseID: h.co.ID, ConversationID: "c1",
			MessageID: "m", No: 1, Tool: "conversation_answer", Args: []byte(`{}`), Kind: "model", State: state})
		h.ok(err)
	}
	h.ok(h.st.SeatSeen(ctx, store.SeatRef{AgentID: v.ID, MemberID: "gone-seat", CourseID: "c2", CourseCode: "AA000", SeenAt: at}))
	h.ok(h.st.SeatGone(ctx, v.ID, "gone-seat", at))
	for i, cost := range []int64{1_234_567_890, 3_000_000_000_000, 499_999} {
		h.ok(h.st.RecordLLMCall(ctx, store.LLMCall{ID: "call" + string(rune('a'+i)), AgentID: v.ID, At: at.Add(-time.Minute), CostPUSD: cost}))
	}
	h.ok(h.st.RecordLLMCall(ctx, store.LLMCall{ID: "yesterday", AgentID: v.ID, At: at.Add(-24 * time.Hour), CostPUSD: 1_000_000_000_000}))
	h.ok(h.st.RecordAnswer(ctx, store.AnswerRecord{ID: "r1", AgentID: v.ID, At: at.Add(-time.Minute), Outcome: store.OutcomePosted, Billable: true}))
	var got HostedAgent
	a := h.call("GET", "agents/"+v.ID, h.yuki, "")
	a.decode(t, &got)
	if got.ProposalsWaiting != 2 || len(got.Seats) != 1 || got.Seats[0].ProposalsWaiting != 2 {
		t.Errorf("proposals: %s", a.body)
	}
	if got.Today.Answers != 1 || got.Today.CostUSD != "3.001235" {
		t.Errorf("today: %+v", got.Today)
	}
	for pusd, want := range map[int64]string{0: "0.000000", 499_999: "0.000000", 500_000: "0.000001", 1_000_000_000_000: "1.000000",
		-2_500_000: "-0.000003"} {
		if got := costUSD(pusd); got != want {
			t.Errorf("costUSD(%d) = %s, want %s", pusd, got, want)
		}
	}
}

// POST …/token has the agent issued a new token, after Core refused the
// one the runtime held: Core must still say it is the caller's and may be
// hosted; the row's token is dropped (its secret destroyed), for the
// worker to be issued another; a row holding none changes nothing.
func TestRenewToken(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	v := h.host(h.yuki, h.helper.ID)
	h.issued(v.ID)
	ctx := context.Background()
	row, err := h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	h.ok(h.st.SetAgentState(ctx, store.AgentState{AgentID: v.ID, State: store.AgentUnauthorized, Reason: store.ReasonTokenRefused,
		ConfigVersion: row.Version}))
	path := "agents/" + v.ID + "/token"

	wantRefused(t, h.call("POST", "agents/agt_nothere/token", h.yuki, ""), 404, CodeNotFound, ReasonAgentNotFound)
	wantRefused(t, h.call("POST", path, h.ken, ""), 404, CodeNotFound, ReasonAgentNotFound)
	wantRefused(t, h.call("POST", path, h.yuki, `{"token":"x"}`), 400, CodeInvalidArgument, ReasonUnknownField)
	wantRefused(t, h.call("POST", path, h.yuki, "", "If-Match", `"7"`), 412, CodeVersionMismatch, ReasonVersionMismatch)
	h.ok(h.fc.SuspendActor(h.helper.ID))
	wantRefused(t, h.call("POST", path, h.yuki, ""), 422, CodeFailedPrecondition, ReasonAgentSuspended)
	h.ok(h.fc.ReactivateActor(h.helper.ID))

	var got HostedAgent
	a := h.call("POST", path, h.yuki, "", "If-Match", `"2"`)
	a.decode(t, &got)
	if a.code != http.StatusOK || got.Version != 3 || a.header.Get("ETag") != `"3"` || got.Status != StatusNeedsModel {
		t.Fatalf("renewed: %d %s", a.code, a.body)
	}
	renewed, err := h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	if renewed.TokenSecretID != "" || renewed.TokenIssued || renewed.TokenCredentialID != "" {
		t.Errorf("the row: %+v", renewed)
	}
	if _, err := h.st.Secret(ctx, row.TokenSecretID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old token's secret: %v", err)
	}
	if ev := h.events("agent.token_renew"); len(ev) == 0 || ev[len(ev)-1].Outcome != "ok" || !strings.Contains(string(ev[len(ev)-1].Detail), `"version":3`) {
		t.Errorf("the audit: %+v", ev)
	}
	// Again: nothing to drop.
	b := h.call("POST", path, h.yuki, "{}")
	if b.code != http.StatusOK || b.header.Get("Idempotency-Replayed") != "true" || b.header.Get("ETag") != `"3"` {
		t.Errorf("again: %d %s", b.code, b.body)
	}
	h.noSecrets(a, b)
}

// A hosted agent whose owner of record is not its owner in Core (a row
// from before an agent's owner was fixed there) is not issued another
// token: owner_changed, for its owner of record to delete it.
func TestRenewTokenOfAnotherOwnersAgent(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	ctx := context.Background()
	row, err := h.st.CreateHostedAgent(ctx, store.HostedAgent{ID: "agt_earlier", CoreActorID: h.helper.ID, OwnerActorID: h.ken.ID,
		OwnerVerified: true, TenantID: "ten_" + h.ken.ID, DisplayName: "Ken's once", Settings: []byte(`{}`)})
	h.ok(err)
	h.issued(row.ID)
	wantRefused(t, h.call("POST", "agents/"+row.ID+"/token", h.ken, ""), 422, CodeFailedPrecondition, ReasonOwnerChanged)
	if again, err := h.st.HostedAgent(ctx, row.ID); err != nil || again.TokenSecretID == "" {
		t.Errorf("the row: %+v %v", again, err)
	}
}

// pause drops the agent's token from its row and revokes it in Core after
// the write, saying what became of it; paused already, nothing is written
// or audited, but the token is revoked again. resume writes, and the
// worker is issued another. If-Match, when given, is held to.
func TestPauseResume(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	v := h.host(h.yuki, h.helper.ID)
	h.issued(v.ID)
	ctx := context.Background()
	held, err := h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	var got Paused
	a := h.call("POST", "agents/"+v.ID+"/pause", h.yuki, "")
	a.decode(t, &got)
	if a.code != 200 || got.Status != StatusPaused || !got.Paused || got.Version != 3 || got.Revocation.Outcome != RevocationRevoked ||
		got.Revocation.Problem != nil || !strings.Contains(a.body, `"revocation":{"outcome":"revoked","problem":null}`) {
		t.Fatalf("pause: %d %s", a.code, a.body)
	}
	if h.fc.RuntimeToken(h.helper.ID).Token != "" || h.fc.SiteChat(h.helper.ID) {
		t.Error("the paused agent's token is still live")
	}
	if row, err := h.st.HostedAgent(ctx, v.ID); err != nil || row.TokenSecretID != "" || row.TokenIssued {
		t.Errorf("the paused row: %+v %v", row, err)
	}
	if _, err := h.st.Secret(ctx, held.TokenSecretID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the token's secret: %v", err)
	}
	h.call("POST", "agents/"+v.ID+"/pause", h.yuki, "{}").decode(t, &got)
	if got.Version != 3 || got.Revocation.Outcome != RevocationNone {
		t.Errorf("a second pause: %+v", got)
	}
	wantRefused(t, h.call("POST", "agents/"+v.ID+"/resume", h.yuki, "", "If-Match", `"1"`), 412, CodeVersionMismatch, ReasonVersionMismatch)
	wantRefused(t, h.call("POST", "agents/"+v.ID+"/resume", h.yuki, `{"paused":false}`), 400, CodeInvalidArgument, ReasonUnknownField)
	wantRefused(t, h.call("POST", "agents/"+v.ID+"/resume", h.ken, ""), 404, CodeNotFound, ReasonAgentNotFound)
	var resumed HostedAgent
	a = h.call("POST", "agents/"+v.ID+"/resume", h.yuki, "", "If-Match", `"3"`)
	a.decode(t, &resumed)
	if a.code != 200 || resumed.Paused || resumed.Version != 4 || resumed.Status != StatusNeedsModel || strings.Contains(a.body, "revocation") {
		t.Errorf("resume: %d %s", a.code, a.body)
	}
	p, r := h.events("agent.pause"), h.events("agent.resume")
	if len(p) != 1 || p[0].Outcome != "ok" || !strings.Contains(string(p[0].Detail), `"revocation":"revoked"`) || len(r) != 3 {
		t.Errorf("the audit: pause %+v, resume %+v", p, r)
	}
	h.noSecrets(a)
}

// A pause whose revocation fails pauses all the same, and says why (Core
// not answering: core_unavailable); paused again, the token is revoked.
// An agent the operator's configuration runs is not revoked
// (operator_agent): its token is the operator's agent's.
func TestPauseRevocationOutcomes(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	v := h.host(h.yuki, h.helper.ID)
	h.issued(v.ID)
	h.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == core.ToolRuntimeRevokeToken {
			return &fakecore.Injection{Status: http.StatusServiceUnavailable}
		}
		return nil
	})
	var got Paused
	a := h.call("POST", "agents/"+v.ID+"/pause", h.yuki, "")
	a.decode(t, &got)
	if a.code != 200 || !got.Paused || got.Revocation.Outcome != RevocationFailed || got.Revocation.Problem == nil ||
		*got.Revocation.Problem != ReasonCoreUnavailable {
		t.Fatalf("pause: %d %s", a.code, a.body)
	}
	if h.fc.RuntimeToken(h.helper.ID).Token == "" {
		t.Fatal("revoked, Core not answering")
	}
	h.fc.Inject(nil)
	h.call("POST", "agents/"+v.ID+"/pause", h.yuki, "").decode(t, &got)
	if got.Revocation.Outcome != RevocationRevoked || h.fc.RuntimeToken(h.helper.ID).Token != "" {
		t.Errorf("paused again: %+v", got.Revocation)
	}

	_, err := h.fc.IssueRuntimeToken(h.helper.ID)
	h.ok(err)
	h.actors.mu.Lock()
	h.actors.yaml[h.helper.ID] = "yuki-yaml"
	h.actors.mu.Unlock()
	h.call("POST", "agents/"+v.ID+"/pause", h.yuki, "").decode(t, &got)
	if got.Revocation.Outcome != RevocationNotAttempted || got.Revocation.Problem == nil || *got.Revocation.Problem != ReasonOperatorAgent ||
		h.fc.RuntimeToken(h.helper.ID).Token == "" {
		t.Errorf("the operator's agent: %+v", got.Revocation)
	}
}

// DELETE destroys the agent, its courses and its secrets, then revokes its
// token in Core, and purges what the store held of it, its ledger kept;
// the next GET, and a second DELETE, are 404. It takes no query.
func TestDelete(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	v := h.host(h.yuki, h.helper.ID)
	h.issued(v.ID)
	ctx := context.Background()
	row, err := h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	h.ok(h.st.PutHostedCourse(ctx, store.HostedCourse{AgentID: v.ID, CourseID: h.co.ID, Settings: []byte(`{"enabled":true}`)}))
	h.ok(h.st.SetAgentState(ctx, store.AgentState{AgentID: v.ID, State: store.AgentRunning, ConfigVersion: 2}))
	h.ok(h.st.RecordAnswer(ctx, store.AnswerRecord{ID: "r1", AgentID: v.ID, At: at, Outcome: store.OutcomePosted, Billable: true}))

	wantRefused(t, h.call("DELETE", "agents/"+v.ID+"?revoke_token=false", h.yuki, ""), 400, CodeInvalidArgument, ReasonUnknownParameter)
	wantRefused(t, h.call("DELETE", "agents/"+v.ID, h.yuki, `{"force":true}`), 400, CodeInvalidArgument, ReasonUnknownField)
	wantRefused(t, h.call("DELETE", "agents/"+v.ID, h.yuki, "", "If-Match", `"9"`), 412, CodeVersionMismatch, ReasonVersionMismatch)
	wantRefused(t, h.call("DELETE", "agents/"+v.ID, h.ken, ""), 404, CodeNotFound, ReasonAgentNotFound)
	if h.fc.RuntimeToken(h.helper.ID).Token == "" {
		t.Fatal("a refused DELETE revoked the token")
	}

	var d Deleted
	a := h.call("DELETE", "agents/"+v.ID, h.yuki, "", "If-Match", `"2"`)
	a.decode(t, &d)
	if a.code != 200 || d.Deleted.ID != v.ID || d.Deleted.CoreActorID != h.helper.ID || d.Revocation.Outcome != RevocationRevoked ||
		d.Revocation.Problem != nil {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if h.fc.RuntimeToken(h.helper.ID).Token != "" || h.fc.SiteChat(h.helper.ID) {
		t.Error("the token is not revoked in Core")
	}
	if _, err := h.st.HostedAgent(ctx, v.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the row: %v", err)
	}
	if courses, _ := h.st.HostedCourses(ctx, v.ID); len(courses) != 0 {
		t.Errorf("its courses: %+v", courses)
	}
	if _, err := h.st.Secret(ctx, row.TokenSecretID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("its token's secret: %v", err)
	}
	if _, err := h.st.AgentState(ctx, v.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("its state: %v", err)
	}
	if sp, _ := h.st.Spend(ctx, store.SpendScope{AgentID: v.ID}, at.Add(-time.Hour)); sp.Answers != 1 {
		t.Errorf("its ledger: %+v", sp)
	}
	ev := h.events("agent.delete")
	if last := ev[len(ev)-1]; last.Outcome != "ok" || !strings.Contains(string(last.Detail), `"revocation":"revoked"`) {
		t.Errorf("the audit: %+v", last)
	}
	wantRefused(t, h.call("GET", "agents/"+v.ID, h.yuki, ""), 404, CodeNotFound, ReasonAgentNotFound)
	wantRefused(t, h.call("DELETE", "agents/"+v.ID, h.yuki, ""), 404, CodeNotFound, ReasonAgentNotFound)
	h.noSecrets(a)
}

// DELETE deletes the agent whatever became of the revocation: failed for
// Core not answering (core_unavailable) and for a credential Core refuses
// (runtime_misconfigured), none when Core held none.
func TestDeleteRevocationOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prepare    func(h *hostWorld)
		outcome    string
		problem    string
		stillLives bool
	}{
		{"Core not answering", func(h *hostWorld) {
			h.fc.Inject(func(fakecore.InjectedCall) *fakecore.Injection {
				return &fakecore.Injection{Status: http.StatusBadGateway}
			})
		}, RevocationFailed, ReasonCoreUnavailable, true},
		{"a credential Core refuses", func(h *hostWorld) {
			h.ok(h.fc.RevokeServiceToken(h.svc.CredentialID))
		}, RevocationFailed, ReasonRuntimeMisconfigured, true},
		{"revoked already", func(h *hostWorld) {
			_, err := h.fc.RevokeRuntimeToken(h.helper.ID)
			h.ok(err)
		}, RevocationNone, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostWorld(t, fakecore.Options{}, nil)
			v := h.host(h.yuki, h.helper.ID)
			h.issued(v.ID)
			tc.prepare(h)
			var d Deleted
			a := h.call("DELETE", "agents/"+v.ID, h.yuki, "")
			a.decode(t, &d)
			problem := ""
			if d.Revocation.Problem != nil {
				problem = *d.Revocation.Problem
			}
			if a.code != 200 || d.Revocation.Outcome != tc.outcome || problem != tc.problem {
				t.Fatalf("%d %s", a.code, a.body)
			}
			if _, err := h.st.HostedAgent(context.Background(), v.ID); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("the row is kept: %v", err)
			}
			h.fc.Inject(nil)
			if lives := h.fc.RuntimeToken(h.helper.ID).Token != ""; lives != tc.stillLives {
				t.Errorf("the token lives: %v", lives)
			}
		})
	}
}

// Every request of the routes that ask Core about an agent takes from the
// caller's allowance of them, and past it is 429.
func TestTokenBucket(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	h.s.token.reset(Rate{60, 2})
	for range 2 {
		h.call("POST", "agents/inspect", h.yuki, agentBody("nope"))
	}
	a := h.call("POST", "agents", h.yuki, agentBody(h.helper.ID))
	wantRefused(t, a, 429, CodeRateLimited, ReasonRateLimited)
	if a.header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// Other routes are not the bucket's.
	if a := h.call("GET", "agents", h.yuki, ""); a.code != 200 {
		t.Errorf("GET /agents: %d", a.code)
	}
}

// raceStore runs each hook once, at the first call of its method after it
// is set: another request that lands between a request's read of a row and
// its write.
type raceStore struct {
	store.Store
	mu    sync.Mutex
	hooks map[string]func()
}

// raced is a host world whose store runs raceStore's hooks.
func raced(t *testing.T) (*hostWorld, *raceStore) {
	rs := &raceStore{hooks: map[string]func(){}}
	h := newHostWorld(t, fakecore.Options{}, func(o *Options) { rs.Store, o.Store = o.Store, rs })
	return h, rs
}

// on runs f once, before the next call of method.
func (s *raceStore) on(method string, f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks[method] = f
}

func (s *raceStore) fire(method string) {
	s.mu.Lock()
	f := s.hooks[method]
	delete(s.hooks, method)
	s.mu.Unlock()
	if f != nil {
		f()
	}
}

func (s *raceStore) UpdateHostedAgent(ctx context.Context, a store.HostedAgent, secrets ...store.Secret) (*store.HostedAgent, error) {
	s.fire("UpdateHostedAgent")
	return s.Store.UpdateHostedAgent(ctx, a, secrets...)
}

func (s *raceStore) SetHostedAgentPaused(ctx context.Context, id string, paused bool, version int) (*store.HostedAgent, error) {
	s.fire("SetHostedAgentPaused")
	return s.Store.SetHostedAgentPaused(ctx, id, paused, version)
}

func (s *raceStore) DeleteHostedAgent(ctx context.Context, id string, cond store.DeleteIf) error {
	s.fire("DeleteHostedAgent")
	return s.Store.DeleteHostedAgent(ctx, id, cond)
}

// pause and resume hold If-Match to their write, not only to their read: a
// write that lands in between is 412, and nothing is paused nor revoked;
// without If-Match, the pause is written over it.
func TestPauseRacesAWrite(t *testing.T) {
	h, rs := raced(t)
	v := h.host(h.yuki, h.helper.ID)
	ctx := context.Background()
	rename := func() {
		row, err := h.st.HostedAgent(ctx, v.ID)
		h.ok(err)
		row.DisplayName = "Renamed meanwhile"
		_, err = h.st.UpdateHostedAgent(ctx, *row)
		h.ok(err)
	}
	rs.on("SetHostedAgentPaused", rename)
	revokes := h.serviceCalls(core.ToolRuntimeRevokeToken)
	a := h.call("POST", "agents/"+v.ID+"/pause", h.yuki, "", "If-Match", `"1"`)
	if e := wantRefused(t, a, http.StatusPreconditionFailed, CodeVersionMismatch, ReasonVersionMismatch); e.Details["current_version"] != float64(2) {
		t.Errorf("current_version: %v", e.Details)
	}
	if row, err := h.st.HostedAgent(ctx, v.ID); err != nil || row.Paused || row.Version != 2 {
		t.Errorf("paused over another write: %+v %v", row, err)
	}
	if n := h.serviceCalls(core.ToolRuntimeRevokeToken); n != revokes {
		t.Error("a pause refused revoked the token")
	}
	rs.on("SetHostedAgentPaused", rename)
	var got Paused
	a = h.call("POST", "agents/"+v.ID+"/pause", h.yuki, "")
	a.decode(t, &got)
	if a.code != http.StatusOK || !got.Paused || got.Version != 4 {
		t.Errorf("without If-Match: %d %s", a.code, a.body)
	}
	if ev := h.events("agent.pause"); len(ev) != 2 || ev[0].Outcome != ReasonVersionMismatch || ev[1].Outcome != "ok" {
		t.Errorf("the audit: %+v", ev)
	}
}

// DELETE and POST …/token hold If-Match to their write: a write that lands
// in between is 412, and nothing is deleted, revoked or dropped.
func TestWritesRaceAWrite(t *testing.T) {
	h, rs := raced(t)
	v := h.host(h.yuki, h.helper.ID)
	h.issued(v.ID)
	ctx := context.Background()
	rename := func() {
		row, err := h.st.HostedAgent(ctx, v.ID)
		h.ok(err)
		row.DisplayName = "Renamed meanwhile"
		_, err = h.st.UpdateHostedAgent(ctx, *row)
		h.ok(err)
	}
	rs.on("DeleteHostedAgent", rename)
	wantRefused(t, h.call("DELETE", "agents/"+v.ID, h.yuki, "", "If-Match", `"2"`), http.StatusPreconditionFailed, CodeVersionMismatch,
		ReasonVersionMismatch)
	if _, err := h.st.HostedAgent(ctx, v.ID); err != nil || h.fc.RuntimeToken(h.helper.ID).Token == "" {
		t.Errorf("deleted, or revoked, over another write: %v", err)
	}
	rs.on("UpdateHostedAgent", rename)
	wantRefused(t, h.call("POST", "agents/"+v.ID+"/token", h.yuki, "", "If-Match", `"3"`), http.StatusPreconditionFailed, CodeVersionMismatch,
		ReasonVersionMismatch)
	if row, err := h.st.HostedAgent(ctx, v.ID); err != nil || row.TokenSecretID == "" {
		t.Errorf("the token dropped over another write: %+v %v", row, err)
	}
	// Without If-Match, the token is dropped over it.
	rs.on("UpdateHostedAgent", rename)
	if a := h.call("POST", "agents/"+v.ID+"/token", h.yuki, ""); a.code != http.StatusOK {
		t.Errorf("without If-Match: %d %s", a.code, a.body)
	}
	if row, err := h.st.HostedAgent(ctx, v.ID); err != nil || row.TokenSecretID != "" || row.DisplayName != "Renamed meanwhile" {
		t.Errorf("the row: %+v %v", row, err)
	}
}
