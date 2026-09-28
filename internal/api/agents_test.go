package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/probe"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// inspect says what a token is before it is connected, and refuses it in
// the contract's order, each refusal audited with its reason; a token not
// of Core's shape is never sent to Core. Nothing is written.
func TestInspect(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	a := h.call("POST", "agents/inspect", h.yuki, tokenBody(h.helper.Token, h.helper.ID))
	wantSecured(t, a, "no-store")
	var ins Inspection
	a.decode(t, &ins)
	if a.code != http.StatusOK || ins.CoreActorID != h.helper.ID || ins.OwnerActorID != h.yuki.ID || ins.DisplayName != "Yuki's helper" ||
		ins.Token.Prefix != probe.Prefix(h.helper.Token) || ins.Token.Hint != "ais_"+ins.Token.Prefix+"…" || ins.Hosted != nil {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if len(ins.Seats) != 1 || ins.Seats[0].Kind != "delegate" || ins.Seats[0].CourseCode != "CS101" || !ins.Seats[0].Answers ||
		ins.Seats[0].CourseStatus == nil || *ins.Seats[0].CourseStatus != "active" || ins.Seats[0].ProposalsWaiting != 0 {
		t.Errorf("seats: %+v", ins.Seats)
	}
	if n, err := h.st.HostedAgents(context.Background()); err != nil || len(n) != 0 {
		t.Errorf("inspect wrote %+v %v", n, err)
	}

	unowned, err := h.fc.AddAgent("Nobody's", h.ken.ID)
	h.ok(err)
	h.ok(h.fc.SetOwner(unowned.ID, ""))
	unownedToken := h.token(unowned.ID).Token
	revoked := h.token(h.helper.ID).Token
	h.ok(h.fc.Revoke(revoked))
	suspended, err := h.fc.AddAgent("Suspended", h.yuki.ID)
	h.ok(err)
	h.tokens = append(h.tokens, suspended.Token)
	h.ok(h.fc.SuspendActor(suspended.ID))
	kens, err := h.fc.AddAgent("Ken's helper", h.ken.ID)
	h.ok(err)
	h.tokens = append(h.tokens, kens.Token)
	old := newHostWorld(t, fakecore.Options{BeforeOwners: true}, nil)

	for _, tc := range []struct {
		name, token, actor string
		w                  *hostWorld
		code               int
		reason             string
	}{
		{"not a token", "sk-proj-abcdefghijklmnopqrstuvwxyz", "", h, 400, ReasonTokenMalformed},
		{"an invitation", "aisinv_abcdefghijkl_" + strings.Repeat("A", 43), "", h, 400, ReasonTokenMalformed},
		{"a core_actor_id not a UUID", h.helper.Token, "agent-1", h, 400, ReasonInvalidField},
		{"revoked", revoked, "", h, 422, probe.ReasonTokenRefused},
		{"a person's", h.yuki.Token, "", h, 422, probe.ReasonTokenNotAgent},
		{"a suspended agent's", suspended.Token, "", h, 422, probe.ReasonAgentSuspended},
		{"another agent's than meant", h.helper.Token, suspended.ID, h, 422, probe.ReasonTokenOtherAgent},
		{"a Core that does not name owners", old.helper.Token, "", old, 422, probe.ReasonCoreTooOld},
		{"an agent nobody owns", unownedToken, "", h, 403, probe.ReasonAgentUnowned},
		{"another's agent", kens.Token, "", h, 403, probe.ReasonNotOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(tc.w.fc.Calls())
			a := tc.w.call("POST", "agents/inspect", tc.w.yuki, tokenBody(tc.token, tc.actor))
			wantSecured(t, a, "no-store")
			wantRefused(t, a, tc.code, map[int]string{400: CodeInvalidArgument, 422: CodeFailedPrecondition, 403: CodeForbidden}[tc.code], tc.reason)
			if tc.code == 400 && len(tc.w.fc.Calls()) != before {
				t.Error("Core was asked about a token not of its shape")
			}
			ev := tc.w.events("agent.inspect")
			if len(ev) == 0 || ev[len(ev)-1].Outcome != tc.reason || ev[len(ev)-1].ActorID != tc.w.yuki.ID {
				t.Errorf("the audit: %+v", ev)
			}
		})
	}

	// Core not answering: 503 core_unavailable.
	h.fc.Inject(func(fakecore.InjectedCall) *fakecore.Injection {
		return &fakecore.Injection{Status: http.StatusBadGateway}
	})
	wantRefused(t, h.call("POST", "agents/inspect", h.yuki, tokenBody(h.helper.Token, "")), 503, CodeUnavailable, probe.ReasonCoreUnavailable)
	h.fc.Inject(nil)

	// Hosted here: by Yuki, with this token, or another.
	v := h.connect(h.yuki, h.helper.Token)
	a = h.call("POST", "agents/inspect", h.yuki, tokenBody(h.helper.Token, ""))
	a.decode(t, &ins)
	if ins.Hosted == nil || !ins.Hosted.ByYou || !ins.Hosted.SameToken || ins.Hosted.AgentID == nil || *ins.Hosted.AgentID != v.ID {
		t.Errorf("hosted with this token: %s", a.body)
	}
	next := h.token(h.helper.ID)
	a = h.call("POST", "agents/inspect", h.yuki, tokenBody(next.Token, ""))
	a.decode(t, &ins)
	if ins.Hosted == nil || !ins.Hosted.ByYou || ins.Hosted.SameToken {
		t.Errorf("hosted with another token: %s", a.body)
	}
	h.noSecrets(a)
}

// connect hosts the agent: 201, needs_model, its token sealed and never
// shown, its seats recorded at once; the same token again replays it; the
// agent's other token is already_hosted; every outcome is audited.
func TestConnect(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	a := h.call("POST", "agents", h.yuki, tokenBody(h.helper.Token, h.helper.ID))
	wantSecured(t, a, "no-store")
	var v Connected
	a.decode(t, &v)
	if a.code != http.StatusCreated || !store.IsHostedAgentID(v.ID) || v.Status != StatusNeedsModel || v.Version != 1 ||
		v.CoreActorID != h.helper.ID || v.OwnerActorID != h.yuki.ID || v.DisplayName != "Yuki's helper" || v.Paused || v.Problem != nil ||
		v.Model.Own != nil || v.Model.School != nil || v.OwnKey != nil || v.Token.Prefix != probe.Prefix(h.helper.Token) {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if a.header.Get("Location") != Prefix+"agents/"+v.ID || a.header.Get("ETag") != `"1"` {
		t.Errorf("Location %q, ETag %q", a.header.Get("Location"), a.header.Get("ETag"))
	}
	if len(v.Seats) != 1 || v.Seats[0].Kind != "delegate" || v.SeatsAsOf == nil || v.Today.CostUSD != "0.000000" || !v.Today.Since.Equal(store.UTCDay(at)) {
		t.Errorf("seats and today: %s", a.body)
	}
	if !strings.Contains(a.body, `"model":{"own":null,"school":null}`) || !strings.Contains(a.body, `"problem":null`) {
		t.Errorf("the null members: %s", a.body)
	}
	row, err := h.st.HostedAgent(context.Background(), v.ID)
	h.ok(err)
	if row.OwnerActorID != h.yuki.ID || !row.OwnerVerified || row.TenantID != "ten_"+h.yuki.ID || string(row.Settings) != `{}` {
		t.Errorf("the row: %+v", row)
	}
	sec, err := h.st.Secret(context.Background(), row.TokenSecretID)
	h.ok(err)
	if sec.Kind != store.SecretCoreToken || sec.TenantID != row.TenantID || sec.CreatedBy != h.yuki.ID {
		t.Errorf("the secret: %+v", sec)
	}
	if opened, err := h.v.Open(context.Background(), sec); err != nil || opened != h.helper.Token {
		t.Error("the sealed token does not open to the token")
	}
	if seats, err := h.st.KnownSeats(context.Background(), v.ID); err != nil || len(seats) != 1 || seats[0].CourseStatus != "active" {
		t.Errorf("seats recorded: %+v %v", seats, err)
	}
	ev := h.events("agent.connect")
	if len(ev) != 1 || ev[0].Outcome != "ok" || ev[0].TargetID != v.ID || !strings.Contains(string(ev[0].Detail), `"seats":1`) {
		t.Errorf("the audit: %+v", ev)
	}

	// The same token again: the agent as it is, nothing written.
	b := h.call("POST", "agents", h.yuki, tokenBody(h.helper.Token, ""))
	var again Connected
	b.decode(t, &again)
	if b.code != http.StatusOK || b.header.Get("Idempotency-Replayed") != "true" || again.ID != v.ID || again.Version != 1 {
		t.Errorf("a replay: %d %s", b.code, b.body)
	}
	// Another token of the agent's: already hosted.
	next := h.token(h.helper.ID)
	c := h.call("POST", "agents", h.yuki, tokenBody(next.Token, ""))
	if e := wantRefused(t, c, 409, CodeConflict, ReasonAlreadyHosted); e.Details["agent_id"] != v.ID {
		t.Errorf("already hosted: %+v", e)
	}
	if ev := h.events("agent.connect"); len(ev) != 3 || ev[2].Outcome != ReasonAlreadyHosted {
		t.Errorf("the audit: %+v", ev)
	}
	h.noSecrets(a, b, c)
}

// Two connections of one token at once: one is created, the other
// replays it.
func TestConnectConcurrently(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Go(func() { codes[i] = h.call("POST", "agents", h.yuki, tokenBody(h.helper.Token, "")).code })
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Errorf("a connection answered %d", c)
		}
	}
	if rows, _ := h.st.HostedAgents(context.Background()); created != 1 || len(rows) != 1 {
		t.Errorf("%d created, %d rows", created, len(rows))
	}
}

// An agent Core has given to someone else since its earlier owner hosted
// it is taken over: the earlier row deleted and purged, and audited. An
// agent the operator runs is operator_agent.
func TestConnectTakeoverAndOperator(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	old := h.connect(h.yuki, h.helper.Token)
	h.ok(h.st.SetAgentState(context.Background(), store.AgentState{AgentID: old.ID, State: store.AgentRunning}))
	// Core gives the agent to Ken: its seats first go, and every token it
	// had is revoked.
	for _, m := range h.seatsOf(h.helper.ID) {
		h.ok(h.fc.RemoveSeat(m))
	}
	h.ok(h.fc.SetOwner(h.helper.ID, h.ken.ID))
	tok := h.token(h.helper.ID)
	v := h.connect(h.ken, tok.Token)
	if v.ID == old.ID || v.OwnerActorID != h.ken.ID {
		t.Errorf("taken over: %+v", v)
	}
	if _, err := h.st.HostedAgent(context.Background(), old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the earlier row: %v", err)
	}
	if _, err := h.st.AgentState(context.Background(), old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the earlier row not purged: %v", err)
	}
	if ev := h.events("agent.takeover"); len(ev) != 1 || ev[0].TargetID != old.ID || ev[0].ActorID != h.ken.ID {
		t.Errorf("the audit: %+v", ev)
	}
	// Yuki's GET of her old agent is 404 now.
	wantRefused(t, h.call("GET", "agents/"+old.ID, h.yuki, ""), 404, CodeNotFound, ReasonAgentNotFound)

	other, err := h.fc.AddAgent("Yuki's second", h.yuki.ID)
	h.ok(err)
	h.tokens = append(h.tokens, other.Token)
	h.actors.mu.Lock()
	h.actors.yaml[other.ID] = "yuki-yaml"
	h.actors.mu.Unlock()
	wantRefused(t, h.call("POST", "agents", h.yuki, tokenBody(other.Token, "")), 409, CodeConflict, ReasonOperatorAgent)
}

// seatsOf are the agent's seats in the fake Core, for tests that take it
// out.
func (h *hostWorld) seatsOf(actorID string) []string {
	h.t.Helper()
	c := probe.NewClient(h.srv.URL, h.tokenOf(actorID), nil)
	ms, err := c.Memberships(context.Background())
	h.ok(err)
	var out []string
	for _, m := range ms {
		out = append(out, m.MemberID)
	}
	return out
}

// tokenOf is a live token of the actor's the test knows.
func (h *hostWorld) tokenOf(actorID string) string {
	switch actorID {
	case h.helper.ID:
		return h.helper.Token
	}
	h.t.Fatalf("no token of %s", actorID)
	return ""
}

// GET /agents and /agents/{id} give the caller's own agents alone; any
// other id, of another's, not there, or of the wrong shape, is 404.
func TestListAndGet(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	mine := h.connect(h.yuki, h.helper.Token)
	second, err := h.fc.AddAgent("Yuki's second", h.yuki.ID)
	h.ok(err)
	h.tokens = append(h.tokens, second.Token)
	h.add(time.Second)
	mine2 := h.connect(h.yuki, second.Token)
	kens, err := h.fc.AddAgent("Ken's", h.ken.ID)
	h.ok(err)
	h.tokens = append(h.tokens, kens.Token)
	theirs := h.connect(h.ken, kens.Token)

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
		store.ReasonOperatorAgent, store.ReasonActorInUse, store.ReasonTokenOtherAgent, store.ReasonTokenNotAgent,
		store.ReasonOwnerChanged, store.ReasonCoreTooOld, store.ReasonAgentSuspended, store.ReasonFailing} {
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
	v := h.connect(h.yuki, h.helper.Token)
	ctx := context.Background()
	seats, err := h.st.KnownSeats(ctx, v.ID)
	h.ok(err)
	member := seats[0].MemberID
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

// PUT /token replaces the token with another of the agent's own, which
// revokes the one it replaces in Core; the same token again changes
// nothing; a token of another agent is token_other_agent; If-Match, when
// given, must be the agent's version.
func TestReplaceToken(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	v := h.connect(h.yuki, h.helper.Token)
	ctx := context.Background()
	row, err := h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	oldSecret := row.TokenSecretID
	path := "agents/" + v.ID + "/token"

	other, err := h.fc.AddAgent("Yuki's second", h.yuki.ID)
	h.ok(err)
	h.tokens = append(h.tokens, other.Token)
	wantRefused(t, h.call("PUT", path, h.yuki, tokenBody(other.Token, "")), 422, CodeFailedPrecondition, probe.ReasonTokenOtherAgent)
	wantRefused(t, h.call("PUT", path, h.yuki, tokenBody(h.helper.Token, h.helper.ID)), 400, CodeInvalidArgument, ReasonUnknownField)
	wantRefused(t, h.call("PUT", "agents/agt_nothere/token", h.yuki, tokenBody(h.helper.Token, "")), 404, CodeNotFound, ReasonAgentNotFound)
	wantRefused(t, h.call("PUT", path, h.ken, tokenBody(h.helper.Token, "")), 404, CodeNotFound, ReasonAgentNotFound)

	// The same token: nothing written.
	var same TokenReplaced
	a := h.call("PUT", path, h.yuki, tokenBody(h.helper.Token, ""))
	a.decode(t, &same)
	if a.code != 200 || a.header.Get("Idempotency-Replayed") != "true" || same.PreviousToken.Revocation != probe.NotAttempted ||
		same.Agent.Version != 1 || same.PreviousToken.Problem != nil {
		t.Errorf("the same token: %d %s", a.code, a.body)
	}

	next := h.token(h.helper.ID)
	wantRefused(t, h.call("PUT", path, h.yuki, tokenBody(next.Token, ""), "If-Match", `"7"`), 412, CodeVersionMismatch, ReasonVersionMismatch)
	wantRefused(t, h.call("PUT", path, h.yuki, tokenBody(next.Token, ""), "If-Match", `W/"1"`), 400, CodeInvalidArgument, ReasonBadIfMatch)
	var done TokenReplaced
	b := h.call("PUT", path, h.yuki, tokenBody(next.Token, ""), "If-Match", `"1"`)
	b.decode(t, &done)
	if b.code != 200 || done.Agent.Version != 2 || done.Agent.Token.Prefix != next.Prefix || done.PreviousToken.Prefix != probe.Prefix(h.helper.Token) ||
		done.PreviousToken.Revocation != probe.Revoked || b.header.Get("ETag") != `"2"` || done.Agent.Status != StatusNeedsModel {
		t.Fatalf("replaced: %d %s", b.code, b.body)
	}
	if _, err := h.st.Secret(ctx, oldSecret); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old token's secret: %v", err)
	}
	for _, c := range h.fc.Credentials(h.helper.ID) {
		live := c.RevokedAt == nil
		if (c.ID == next.CredentialID) != live {
			t.Errorf("credential %s (%s): revoked %v", c.ID, c.Label, !live)
		}
	}
	ev := h.events("agent.token_replace")
	last := ev[len(ev)-1]
	if last.Outcome != "ok" || !strings.Contains(string(last.Detail), `"revocation":"revoked"`) || !strings.Contains(string(last.Detail), `"old_hint":"ais_`) {
		t.Errorf("the audit: %+v", last)
	}
	// Once the worker runs the new version, it shows running.
	h.ok(h.st.SetAgentState(ctx, store.AgentState{AgentID: v.ID, State: store.AgentRunning, ConfigVersion: 2}))
	h.noSecrets(a, b)
}

// A new token whose revocation of the old one fails still replaces it: the
// answer says why, for the owner to revoke it.
func TestReplaceTokenRevocationFails(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	v := h.connect(h.yuki, h.helper.Token)
	next := h.token(h.helper.ID)
	h.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == "credential_revoke" {
			return &fakecore.Injection{Status: http.StatusServiceUnavailable}
		}
		return nil
	})
	var done TokenReplaced
	a := h.call("PUT", "agents/"+v.ID+"/token", h.yuki, tokenBody(next.Token, ""))
	a.decode(t, &done)
	if a.code != 200 || done.PreviousToken.Revocation != probe.Failed || done.PreviousToken.Problem == nil ||
		*done.PreviousToken.Problem != probe.ProblemCoreUnavailable || done.Agent.Token.Prefix != next.Prefix {
		t.Errorf("%d %s", a.code, a.body)
	}
}

// pause and resume set the agent's pause, once: already so, nothing is
// written or audited; If-Match, when given, is held to.
func TestPauseResume(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	v := h.connect(h.yuki, h.helper.Token)
	var got HostedAgent
	a := h.call("POST", "agents/"+v.ID+"/pause", h.yuki, "")
	a.decode(t, &got)
	if a.code != 200 || got.Status != StatusPaused || !got.Paused || got.Version != 2 {
		t.Fatalf("pause: %d %s", a.code, a.body)
	}
	h.call("POST", "agents/"+v.ID+"/pause", h.yuki, "{}").decode(t, &got)
	if got.Version != 2 {
		t.Errorf("a second pause wrote: %+v", got)
	}
	wantRefused(t, h.call("POST", "agents/"+v.ID+"/resume", h.yuki, "", "If-Match", `"1"`), 412, CodeVersionMismatch, ReasonVersionMismatch)
	wantRefused(t, h.call("POST", "agents/"+v.ID+"/resume", h.yuki, `{"paused":false}`), 400, CodeInvalidArgument, ReasonUnknownField)
	wantRefused(t, h.call("POST", "agents/"+v.ID+"/resume", h.ken, ""), 404, CodeNotFound, ReasonAgentNotFound)
	a = h.call("POST", "agents/"+v.ID+"/resume", h.yuki, "", "If-Match", `"2"`)
	a.decode(t, &got)
	if a.code != 200 || got.Paused || got.Version != 3 || got.Status != StatusNeedsModel {
		t.Errorf("resume: %d %s", a.code, a.body)
	}
	if p, r := h.events("agent.pause"), h.events("agent.resume"); len(p) != 1 || p[0].Outcome != "ok" || len(r) != 3 {
		t.Errorf("the audit: pause %+v, resume %+v", p, r)
	}
}

// DELETE revokes the agent's token in Core with itself, then destroys the
// agent, its courses and its secrets, and purges what the store held of
// it, its ledger kept; the next GET, and a second DELETE, are 404.
func TestDelete(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	v := h.connect(h.yuki, h.helper.Token)
	ctx := context.Background()
	row, err := h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	h.ok(h.st.PutHostedCourse(ctx, store.HostedCourse{AgentID: v.ID, CourseID: h.co.ID, Settings: []byte(`{"enabled":true}`)}))
	h.ok(h.st.SetAgentState(ctx, store.AgentState{AgentID: v.ID, State: store.AgentRunning, ConfigVersion: 1}))
	h.ok(h.st.RecordAnswer(ctx, store.AnswerRecord{ID: "r1", AgentID: v.ID, At: at, Outcome: store.OutcomePosted, Billable: true}))

	wantRefused(t, h.call("DELETE", "agents/"+v.ID+"?revoke_token=maybe", h.yuki, ""), 400, CodeInvalidArgument, ReasonInvalidField)
	wantRefused(t, h.call("DELETE", "agents/"+v.ID+"?force=1", h.yuki, ""), 400, CodeInvalidArgument, ReasonUnknownParameter)
	wantRefused(t, h.call("DELETE", "agents/"+v.ID, h.yuki, "", "If-Match", `"9"`), 412, CodeVersionMismatch, ReasonVersionMismatch)
	wantRefused(t, h.call("DELETE", "agents/"+v.ID, h.ken, ""), 404, CodeNotFound, ReasonAgentNotFound)

	var d Deleted
	a := h.call("DELETE", "agents/"+v.ID, h.yuki, "")
	a.decode(t, &d)
	if a.code != 200 || d.Deleted.ID != v.ID || d.Deleted.CoreActorID != h.helper.ID || d.Token.Revocation != probe.Revoked ||
		d.Token.Problem != nil || d.Token.Prefix != probe.Prefix(h.helper.Token) {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if c := h.fc.Credentials(h.helper.ID); c[0].RevokedAt == nil {
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
	if seats, _ := h.st.KnownSeats(ctx, v.ID); len(seats) != 0 {
		t.Errorf("its seats: %+v", seats)
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

// DELETE deletes the agent whatever became of the revocation: failed for a
// suspended agent (agent_suspended) and for a Core not answering
// (core_unavailable), not attempted with revoke_token=false.
func TestDeleteRevocationOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		query      string
		prepare    func(h *hostWorld)
		revocation string
		problem    string
		stillWorks bool
	}{
		{"a suspended agent", "", func(h *hostWorld) { h.ok(h.fc.SuspendActor(h.helper.ID)) }, probe.Failed, probe.ProblemAgentSuspended, true},
		{"Core not answering", "", func(h *hostWorld) {
			h.fc.Inject(func(fakecore.InjectedCall) *fakecore.Injection {
				return &fakecore.Injection{Status: http.StatusBadGateway}
			})
		}, probe.Failed, probe.ProblemCoreUnavailable, true},
		{"not asked to", "?revoke_token=false", func(*hostWorld) {}, probe.NotAttempted, "", true},
		{"asked to", "?revoke_token=true", func(*hostWorld) {}, probe.Revoked, "", false},
		{"revoked already", "", func(h *hostWorld) { h.ok(h.fc.Revoke(h.helper.Token)) }, probe.AlreadyInvalid, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostWorld(t, fakecore.Options{}, nil)
			v := h.connect(h.yuki, h.helper.Token)
			tc.prepare(h)
			var d Deleted
			a := h.call("DELETE", "agents/"+v.ID+tc.query, h.yuki, "")
			a.decode(t, &d)
			problem := ""
			if d.Token.Problem != nil {
				problem = *d.Token.Problem
			}
			if a.code != 200 || d.Token.Revocation != tc.revocation || problem != tc.problem {
				t.Fatalf("%d %s", a.code, a.body)
			}
			if _, err := h.st.HostedAgent(context.Background(), v.ID); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("the row is kept: %v", err)
			}
			h.fc.Inject(nil)
			if live := h.fc.Credentials(h.helper.ID)[0].RevokedAt == nil; live != tc.stillWorks {
				t.Errorf("the token works: %v", live)
			}
		})
	}
}

// The one-brain rule: connecting tells the front end of the agent's other
// live tokens, and whether one was used lately, but not of the one being
// connected nor of one revoked.
func TestOtherTokens(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	elsewhere := h.token(h.helper.ID)
	stale := h.token(h.helper.ID)
	dead := h.token(h.helper.ID)
	h.ok(h.fc.Revoke(dead.Token))
	// elsewhere is in use: another runtime runs the agent with it.
	if _, err := probe.NewClient(h.srv.URL, elsewhere.Token, nil).Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.add(time.Hour)
	if _, err := probe.NewClient(h.srv.URL, elsewhere.Token, nil).Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	mine := h.token(h.helper.ID)
	var ins Inspection
	h.call("POST", "agents/inspect", h.yuki, tokenBody(mine.Token, "")).decode(t, &ins)
	ot := ins.OtherTokens
	if ot == nil || !ot.InUse || ot.WindowSeconds != 900 {
		t.Fatalf("other tokens: %+v", ot)
	}
	prefixes := map[string]bool{}
	for _, tk := range ot.Tokens {
		prefixes[tk.Prefix] = tk.Recent
	}
	if len(ot.Tokens) != 3 || !prefixes[elsewhere.Prefix] || prefixes[stale.Prefix] || prefixes[probe.Prefix(h.helper.Token)] {
		t.Errorf("the tokens: %+v", ot.Tokens)
	}
	if _, listed := prefixes[mine.Prefix]; listed {
		t.Error("the token being connected is listed")
	}
	if _, listed := prefixes[dead.Prefix]; listed {
		t.Error("a revoked token is listed")
	}
	if ot.Tokens[0].Prefix != elsewhere.Prefix || ot.Tokens[0].LastUsedAt == nil || ot.Tokens[0].Label == nil {
		t.Errorf("the most recently used first: %+v", ot.Tokens[0])
	}
	a := h.call("POST", "agents", h.yuki, tokenBody(mine.Token, ""))
	var c Connected
	a.decode(t, &c)
	if a.code != 201 || c.OtherTokens == nil || !c.OtherTokens.InUse || !strings.Contains(a.body, `"other_tokens":{"in_use":true`) {
		t.Errorf("connect: %d %s", a.code, a.body)
	}
	// Core not listing them fails nothing.
	h.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == "credential_list" {
			return &fakecore.Injection{Status: http.StatusInternalServerError}
		}
		return nil
	})
	a = h.call("POST", "agents/inspect", h.yuki, tokenBody(mine.Token, ""))
	if a.code != 200 || !strings.Contains(a.body, `"other_tokens":null`) {
		t.Errorf("unlisted: %d %s", a.code, a.body)
	}
	h.noSecrets(a)
}

// Every request of the token routes takes from the caller's token
// allowance, and past it is 429.
func TestTokenBucket(t *testing.T) {
	h := newHostWorld(t, fakecore.Options{}, nil)
	h.s.token.reset(Rate{60, 2})
	for range 2 {
		h.call("POST", "agents/inspect", h.yuki, tokenBody("nope", ""))
	}
	a := h.call("POST", "agents", h.yuki, tokenBody(h.helper.Token, ""))
	wantRefused(t, a, 429, CodeRateLimited, ReasonRateLimited)
	if a.header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	// Other routes are not the token bucket's.
	if a := h.call("GET", "agents", h.yuki, ""); a.code != 200 {
		t.Errorf("GET /agents: %d", a.code)
	}
}
