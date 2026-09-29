package storetest

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// scope is where a ledger row falls.
type scope struct{ agent, tenant, course, opener, key string }

func call(id string, when time.Time, sc scope, cost int64) store.LLMCall {
	return store.LLMCall{
		ID: id, At: when, TenantID: sc.tenant, AgentID: sc.agent, CourseID: sc.course,
		MemberID: "seat-" + sc.agent, ConversationID: "x1", MessageID: "q1", OpenerMemberID: sc.opener,
		Adapter: "openai_chat", Provider: "openai", Model: "gpt-test", Stop: "end", RawStop: "stop",
		Input: 1200, CacheRead: 1000, CacheWrite: 0, Output: 210, Reasoning: 64,
		RawUsage:     json.RawMessage(`{"prompt_tokens":1200,"completion_tokens":210}`),
		PriceVersion: "2026-09-01", CostPUSD: cost, KeySource: sc.key, LatencyMS: 850,
	}
}

func answerRecord(id string, when time.Time, sc scope, billable bool, cost int64) store.AnswerRecord {
	outcome := store.OutcomePosted
	if !billable {
		outcome = store.OutcomeQuota
	}
	return store.AnswerRecord{
		ID: id, At: when, TenantID: sc.tenant, AgentID: sc.agent, CourseID: sc.course,
		MemberID: "seat-" + sc.agent, ConversationID: "x1", MessageID: "q1", OpenerMemberID: sc.opener,
		Key: "answer:x1:q1:1", Outcome: outcome, Billable: billable, Turns: 2, ToolCalls: 1,
		InputTokens: 2400, OutputTokens: 420, CostPUSD: cost, KeySource: sc.key,
		PromptHash: "sha256:abc", LatencyMS: 4200,
	}
}

func record(t *testing.T, s store.Store, calls []store.LLMCall, answers []store.AnswerRecord) {
	t.Helper()
	for _, c := range calls {
		if err := s.RecordLLMCall(t.Context(), c); err != nil {
			t.Fatalf("RecordLLMCall(%s): %v", c.ID, err)
		}
	}
	for _, a := range answers {
		if err := s.RecordAnswer(t.Context(), a); err != nil {
			t.Fatalf("RecordAnswer(%s): %v", a.ID, err)
		}
	}
}

func spend(t *testing.T, s store.Store, sc store.SpendScope, since time.Time) store.Spend {
	t.Helper()
	got, err := s.Spend(t.Context(), sc, since)
	if err != nil {
		t.Fatalf("Spend(%+v): %v", sc, err)
	}
	return got
}

func recentCosts(t *testing.T, s store.Store, agent string, n int) []int64 {
	t.Helper()
	got, err := s.RecentAnswerCosts(t.Context(), agent, n)
	if err != nil {
		t.Fatalf("RecentAnswerCosts(%s, %d): %v", agent, n, err)
	}
	return got
}

func testLedger(t *testing.T, open Opener) {
	day := at(24 * time.Hour)
	var (
		a1   = scope{"a1", "t1", "c1", "p1", "school"}
		a1p2 = scope{"a1", "t1", "c1", "p2", "school"}
		a2   = scope{"a2", "t1", "c2", "p1", "own"}
		a3   = scope{"a3", "t2", "c1", "p1", "school"}
	)
	calls := []store.LLMCall{
		call("call-1", day.Add(time.Hour), a1, 100),
		call("call-2", day.Add(2*time.Hour), a1p2, 200),
		call("call-3", day.Add(3*time.Hour), a2, 400),
		call("call-4", day.Add(-time.Hour), a1, 800), // the day before
		call("call-5", day.Add(time.Hour), a3, 1600),
	}
	answers := []store.AnswerRecord{
		answerRecord("ans-1", day.Add(time.Hour), a1, true, 10),
		answerRecord("ans-2", day.Add(2*time.Hour), a1p2, true, 20),
		answerRecord("ans-3", day.Add(2*time.Hour), a1, false, 0), // the canned notice
		answerRecord("ans-4", day.Add(3*time.Hour), a2, true, 40),
		answerRecord("ans-5", day.Add(-time.Hour), a1, true, 80), // the day before
		answerRecord("ans-6", day.Add(time.Hour), a3, true, 160),
	}

	t.Run("Spend counts billable answers and sums model calls, by each scope field and together", func(t *testing.T) {
		s := open(t)
		record(t, s, calls, answers)
		for _, c := range []struct {
			name  string
			scope store.SpendScope
			since time.Time
			want  store.Spend
		}{
			{"agent", store.SpendScope{AgentID: "a1"}, day, store.Spend{Answers: 2, CostPUSD: 300}},
			{"tenant", store.SpendScope{TenantID: "t1"}, day, store.Spend{Answers: 3, CostPUSD: 700}},
			{"course", store.SpendScope{CourseID: "c1"}, day, store.Spend{Answers: 3, CostPUSD: 1900}},
			{"opener", store.SpendScope{OpenerMemberID: "p1"}, day, store.Spend{Answers: 3, CostPUSD: 2100}},
			{"key source", store.SpendScope{KeySource: "own"}, day, store.Spend{Answers: 1, CostPUSD: 400}},
			{"asker", store.SpendScope{CourseID: "c1", OpenerMemberID: "p1"}, day, store.Spend{Answers: 2, CostPUSD: 1700}},
			{"agent and asker", store.SpendScope{AgentID: "a1", CourseID: "c1", OpenerMemberID: "p1"}, day, store.Spend{Answers: 1, CostPUSD: 100}},
			{"tenant on the school's key", store.SpendScope{TenantID: "t1", KeySource: "school"}, day, store.Spend{Answers: 2, CostPUSD: 300}},
			{"every field", store.SpendScope{AgentID: "a2", TenantID: "t1", CourseID: "c2", OpenerMemberID: "p1", KeySource: "own"}, day, store.Spend{Answers: 1, CostPUSD: 400}},
			{"nothing in scope", store.SpendScope{TenantID: "t9"}, day, store.Spend{}},
			{"fields that exclude each other", store.SpendScope{AgentID: "a1", KeySource: "own"}, day, store.Spend{}},
			{"since forever", store.SpendScope{AgentID: "a1"}, time.Time{}, store.Spend{Answers: 3, CostPUSD: 1100}},
			{"since is inclusive", store.SpendScope{AgentID: "a1"}, day.Add(time.Hour), store.Spend{Answers: 2, CostPUSD: 300}},
			{"since, a microsecond on", store.SpendScope{AgentID: "a1"}, day.Add(time.Hour + time.Microsecond), store.Spend{Answers: 1, CostPUSD: 200}},
			{"since after everything", store.SpendScope{AgentID: "a1"}, day.Add(48 * time.Hour), store.Spend{}},
		} {
			if got := spend(t, s, c.scope, c.since); got != c.want {
				t.Errorf("%s: Spend = %+v, want %+v", c.name, got, c.want)
			}
		}
	})

	t.Run("Spend needs a scope", func(t *testing.T) {
		s := open(t)
		record(t, s, calls, answers)
		if got, err := s.Spend(t.Context(), store.SpendScope{}, time.Time{}); err == nil {
			t.Fatalf("Spend of no scope = %+v, want an error", got)
		}
	})

	t.Run("a row recorded again counts once; ids are per agent", func(t *testing.T) {
		s := open(t)
		record(t, s, calls, answers)
		again := call("call-1", day.Add(5*time.Hour), a1, 5000)
		repeat := answerRecord("ans-1", day.Add(5*time.Hour), a1, true, 5000)
		record(t, s, []store.LLMCall{again}, []store.AnswerRecord{repeat})
		if got, want := spend(t, s, store.SpendScope{AgentID: "a1"}, day), (store.Spend{Answers: 2, CostPUSD: 300}); got != want {
			t.Errorf("Spend after recording rows again = %+v, want %+v", got, want)
		}
		if got, want := recentCosts(t, s, "a1", 10), []int64{20, 10, 80}; !slices.Equal(got, want) {
			t.Errorf("RecentAnswerCosts after recording a row again = %v, want %v", got, want)
		}

		// Another agent's rows under the same ids are its own.
		other := scope{"a4", "t1", "c1", "p1", "school"}
		record(t, s,
			[]store.LLMCall{call("call-1", day.Add(time.Hour), other, 7)},
			[]store.AnswerRecord{answerRecord("ans-1", day.Add(time.Hour), other, true, 7)})
		if got, want := spend(t, s, store.SpendScope{AgentID: "a4"}, day), (store.Spend{Answers: 1, CostPUSD: 7}); got != want {
			t.Errorf("Spend(a4) = %+v, want %+v", got, want)
		}
	})

	t.Run("RecentAnswerCosts are an agent's newest billable answers, newest first", func(t *testing.T) {
		s := open(t)
		record(t, s, calls, answers)
		// Two at one instant come back newest written first.
		record(t, s, nil, []store.AnswerRecord{
			answerRecord("ans-7", day.Add(4*time.Hour), a1, true, 1),
			answerRecord("ans-8", day.Add(4*time.Hour), a1, true, 2),
		})
		for _, c := range []struct {
			agent string
			n     int
			want  []int64
		}{
			{"a1", 10, []int64{2, 1, 20, 10, 80}},
			{"a1", 3, []int64{2, 1, 20}},
			{"a1", 0, nil},
			{"a1", -1, nil},
			{"a2", 10, []int64{40}},
			{"a9", 10, nil},
		} {
			if got := recentCosts(t, s, c.agent, c.n); !slices.Equal(got, c.want) {
				t.Errorf("RecentAnswerCosts(%s, %d) = %v, want %v", c.agent, c.n, got, c.want)
			}
		}
	})

	t.Run("a zero time is the store's now", func(t *testing.T) {
		s := open(t)
		before := time.Now()
		record(t, s,
			[]store.LLMCall{call("call-now", time.Time{}, a1, 3)},
			[]store.AnswerRecord{answerRecord("ans-now", time.Time{}, a1, true, 3)})
		after := time.Now()
		if got, want := spend(t, s, store.SpendScope{AgentID: "a1"}, before.Add(-clockSlack)), (store.Spend{Answers: 1, CostPUSD: 3}); got != want {
			t.Errorf("Spend since just before = %+v, want %+v", got, want)
		}
		if got := spend(t, s, store.SpendScope{AgentID: "a1"}, after.Add(clockSlack)); got != (store.Spend{}) {
			t.Errorf("Spend since just after = %+v, want nothing", got)
		}
	})

	t.Run("refuses a row without an id or agent, or with raw usage that is not JSON", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for _, mutate := range []func(*store.LLMCall){
			func(c *store.LLMCall) { c.ID = "" },
			func(c *store.LLMCall) { c.AgentID = "" },
			func(c *store.LLMCall) { c.RawUsage = json.RawMessage(`{"prompt_tokens":`) },
		} {
			c := call("call-1", day, a1, 1)
			mutate(&c)
			if err := s.RecordLLMCall(ctx, c); err == nil {
				t.Errorf("RecordLLMCall(%+v) was taken", c)
			}
		}
		for _, mutate := range []func(*store.AnswerRecord){
			func(a *store.AnswerRecord) { a.ID = "" },
			func(a *store.AnswerRecord) { a.AgentID = "" },
		} {
			a := answerRecord("ans-1", day, a1, true, 1)
			mutate(&a)
			if err := s.RecordAnswer(ctx, a); err == nil {
				t.Errorf("RecordAnswer(%+v) was taken", a)
			}
		}
		// Raw usage is optional.
		c := call("call-2", day, a1, 1)
		c.RawUsage = nil
		record(t, s, []store.LLMCall{c}, nil)
		if got, want := spend(t, s, store.SpendScope{AgentID: "a1"}, day), (store.Spend{CostPUSD: 1}); got != want {
			t.Errorf("Spend = %+v, want %+v: a refused row was counted", got, want)
		}
	})
}

func testStatus(t *testing.T, open Opener) {
	t.Run("set, replaced, and listed by agent", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for _, st := range []store.AgentState{
			{AgentID: "a2", State: store.AgentPaused, Detail: "paused by its owner", Worker: "w1", UpdatedAt: at(0)},
			{AgentID: "a1", State: store.AgentRunning, Worker: "w1", UpdatedAt: at(time.Minute)},
			{AgentID: "a1", State: store.AgentUnauthorized, Detail: "Core said 401", Worker: "w2", UpdatedAt: at(2 * time.Minute)},
		} {
			if err := s.SetAgentState(ctx, st); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.AgentStates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := []store.AgentState{
			{AgentID: "a1", State: store.AgentUnauthorized, Detail: "Core said 401", Worker: "w2", UpdatedAt: us(at(2 * time.Minute))},
			{AgentID: "a2", State: store.AgentPaused, Detail: "paused by its owner", Worker: "w1", UpdatedAt: us(at(0))},
		}
		if len(got) != len(want) {
			t.Fatalf("AgentStates = %+v, want %+v", got, want)
		}
		for i := range want {
			sameTime(t, "updated_at", got[i].UpdatedAt, want[i].UpdatedAt)
			got[i].UpdatedAt, want[i].UpdatedAt = time.Time{}, time.Time{}
			if got[i] != want[i] {
				t.Errorf("AgentStates[%d] = %+v, want %+v", i, got[i], want[i])
			}
		}
	})

	t.Run("one agent's state, with why and the version in force", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		if _, err := s.AgentState(ctx, "agt_1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("AgentState before any: %v", err)
		}
		for _, st := range []store.AgentState{
			{AgentID: "agt_1", State: store.AgentStarting, Worker: "w1", ConfigVersion: 3, UpdatedAt: at(0)},
			{AgentID: "agt_1", State: store.AgentError, Reason: store.ReasonSettingsRejected, Detail: "not run: agent.model", Worker: "w2",
				ConfigVersion: 4, UpdatedAt: at(time.Minute)},
			{AgentID: "agt_2", State: store.AgentRunning, Worker: "w1", UpdatedAt: at(0)},
		} {
			if err := s.SetAgentState(ctx, st); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.AgentState(ctx, "agt_1")
		if err != nil {
			t.Fatal(err)
		}
		sameTime(t, "updated_at", got.UpdatedAt, us(at(time.Minute)))
		got.UpdatedAt = time.Time{}
		if want := (store.AgentState{AgentID: "agt_1", State: store.AgentError, Reason: store.ReasonSettingsRejected,
			Detail: "not run: agent.model", Worker: "w2", ConfigVersion: 4}); *got != want {
			t.Errorf("AgentState = %+v, want %+v", *got, want)
		}
		all, err := s.AgentStates(ctx)
		if err != nil || len(all) != 2 || all[0].Reason != store.ReasonSettingsRejected || all[0].ConfigVersion != 4 || all[1].ConfigVersion != 0 {
			t.Errorf("AgentStates = %+v, %v", all, err)
		}
	})

	t.Run("never back to an older version", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for _, st := range []store.AgentState{
			{AgentID: "agt_1", State: store.AgentRunning, Worker: "w1", ConfigVersion: 4, UpdatedAt: at(time.Minute)},
			// A slower worker, which put version 3 in force, writes late.
			{AgentID: "agt_1", State: store.AgentPaused, Worker: "w2", ConfigVersion: 3, UpdatedAt: at(2 * time.Minute)},
		} {
			if err := s.SetAgentState(ctx, st); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.AgentState(ctx, "agt_1")
		if err != nil || got.State != store.AgentRunning || got.Worker != "w1" || got.ConfigVersion != 4 {
			t.Fatalf("an older version's state written over a newer: %+v %v", got, err)
		}
		sameTime(t, "updated_at", got.UpdatedAt, us(at(time.Minute)))
		// The same version, and a later one, replace it.
		for i, st := range []store.AgentState{
			{AgentID: "agt_1", State: store.AgentUnauthorized, Reason: store.ReasonTokenRefused, Worker: "w1", ConfigVersion: 4},
			{AgentID: "agt_1", State: store.AgentStarting, Worker: "w2", ConfigVersion: 5},
		} {
			if err := s.SetAgentState(ctx, st); err != nil {
				t.Fatal(err)
			}
			if got, err := s.AgentState(ctx, "agt_1"); err != nil || got.State != st.State || got.ConfigVersion != st.ConfigVersion {
				t.Errorf("write %d: %+v %v, want %s at %d", i, got, err, st.State, st.ConfigVersion)
			}
		}
	})

	t.Run("a zero UpdatedAt is the store's now", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		before := time.Now()
		if err := s.SetAgentState(ctx, store.AgentState{AgentID: "a1", State: store.AgentStarting}); err != nil {
			t.Fatal(err)
		}
		after := time.Now()
		got, err := s.AgentStates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("AgentStates = %+v, want one", got)
		}
		recent(t, "updated_at", got[0].UpdatedAt, before, after)
	})

	t.Run("none before any is set", func(t *testing.T) {
		s := open(t)
		got, err := s.AgentStates(t.Context())
		if err != nil || len(got) != 0 {
			t.Fatalf("AgentStates = %+v, %v; want none", got, err)
		}
	})

	t.Run("refuses a state without an agent or state", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for _, st := range []store.AgentState{{State: store.AgentRunning}, {AgentID: "a1"}} {
			if err := s.SetAgentState(ctx, st); err == nil {
				t.Errorf("SetAgentState(%+v) was taken", st)
			}
		}
	})
}
