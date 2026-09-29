package storetest

import (
	"reflect"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// asked is a ledger row's place for a report: its agent, course, asker and
// time.
type asked struct {
	agent, course, opener string
	at                    time.Time
}

func (a asked) call(id string, input, output, cost int64) store.LLMCall {
	c := call(id, a.at, scope{agent: a.agent, tenant: "t1", course: a.course, opener: a.opener, key: "own"}, cost)
	c.Input, c.CacheRead, c.CacheWrite, c.Output, c.Reasoning = input, 1, 2, output, 3
	return c
}

func (a asked) answer(id, outcome string, billable bool) store.AnswerRecord {
	r := answerRecord(id, a.at, scope{agent: a.agent, tenant: "t1", course: a.course, opener: a.opener, key: "own"}, billable, 0)
	r.Outcome = outcome
	return r
}

// withWrites is a with the writes its model made.
func withWrites(a store.AnswerRecord, w store.WriteCounts) store.AnswerRecord {
	a.Writes = w
	return a
}

func testReports(t *testing.T, open Opener) {
	// Two UTC days, the first from its first instant to its last.
	day1 := time.Date(2026, time.March, 10, 0, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	var (
		early  = asked{"a1", "c1", "p1", day1}
		late   = asked{"a1", "c2", "p2", day2.Add(-time.Microsecond)}
		next   = asked{"a1", "c1", "p2", day2.Add(3 * time.Hour)}
		after  = asked{"a1", "c1", "p1", day2.Add(24 * time.Hour)}
		other  = asked{"a2", "c1", "p1", day1.Add(time.Hour)}
		before = asked{"a1", "c1", "p1", day1.Add(-time.Microsecond)}
	)
	fill := func(t *testing.T) store.Store {
		s := open(t)
		record(t, s,
			[]store.LLMCall{
				early.call("k1", 100, 10, 1000), early.call("k2", 200, 20, 2000),
				late.call("k3", 300, 30, 3000),
				next.call("k4", 400, 40, 4000),
				after.call("k5", 1, 1, 1), other.call("k6", 1, 1, 1), before.call("k7", 1, 1, 1),
			},
			[]store.AnswerRecord{
				early.answer("r1", store.OutcomePosted, true), early.answer("r2", store.OutcomeQuota, false),
				late.answer("r3", store.OutcomeProposed, true),
				withWrites(next.answer("r4", store.OutcomePosted, true), store.WriteCounts{Sent: 4, Executed: 1, Proposed: 1, Denied: 1}),
				withWrites(next.answer("r5", store.OutcomeError, false), store.WriteCounts{Sent: 2, Failed: 1}),
				after.answer("r6", store.OutcomePosted, true), other.answer("r7", store.OutcomePosted, true),
				before.answer("r8", store.OutcomePosted, true),
			})
		return s
	}

	t.Run("Usage is a row per UTC day and course, since inclusive and until exclusive", func(t *testing.T) {
		s := fill(t)
		got, err := s.Usage(t.Context(), "a1", day1, day2.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		want := []store.UsageRow{
			{Day: day1, CourseID: "c1", Answers: 1, Outcomes: map[string]int{store.OutcomePosted: 1, store.OutcomeQuota: 1},
				ModelCalls: 2, InputTokens: 300, CacheReadTokens: 2, CacheWriteTokens: 4, OutputTokens: 30, ReasoningTokens: 6, CostPUSD: 3000},
			{Day: day1, CourseID: "c2", Answers: 1, Outcomes: map[string]int{store.OutcomeProposed: 1},
				ModelCalls: 1, InputTokens: 300, CacheReadTokens: 1, CacheWriteTokens: 2, OutputTokens: 30, ReasoningTokens: 3, CostPUSD: 3000},
			{Day: day2, CourseID: "c1", Answers: 1, Outcomes: map[string]int{store.OutcomePosted: 1, store.OutcomeError: 1},
				Writes:     store.WriteCounts{Sent: 6, Executed: 1, Proposed: 1, Denied: 1, Failed: 1},
				ModelCalls: 1, InputTokens: 400, CacheReadTokens: 1, CacheWriteTokens: 2, OutputTokens: 40, ReasoningTokens: 3, CostPUSD: 4000},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Usage:\n got %+v\nwant %+v", got, want)
		}
		// A day's rows, and only those.
		got, err = s.Usage(t.Context(), "a1", day2, day2.Add(24*time.Hour))
		if err != nil || len(got) != 1 || got[0].Day != day2 {
			t.Errorf("Usage of the second day = %+v, %v", got, err)
		}
		// Nothing in the span is no rows.
		if got, err := s.Usage(t.Context(), "a9", day1, day2); err != nil || len(got) != 0 {
			t.Errorf("Usage of an agent with nothing = %+v, %v", got, err)
		}
	})

	t.Run("AskerUsage counts per asker of one course, and never holds text", func(t *testing.T) {
		s := fill(t)
		got, err := s.AskerUsage(t.Context(), "a1", "c1", day1, day2.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		want := []store.AskerUsage{
			{OpenerMemberID: "p1", Answers: 1, Outcomes: map[string]int{store.OutcomePosted: 1, store.OutcomeQuota: 1},
				ModelCalls: 2, InputTokens: 300, OutputTokens: 30, CostPUSD: 3000},
			{OpenerMemberID: "p2", Answers: 1, Outcomes: map[string]int{store.OutcomePosted: 1, store.OutcomeError: 1},
				ModelCalls: 1, InputTokens: 400, OutputTokens: 40, CostPUSD: 4000},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("AskerUsage:\n got %+v\nwant %+v", got, want)
		}
		if got, err := s.AskerUsage(t.Context(), "a1", "c9", day1, day2); err != nil || len(got) != 0 {
			t.Errorf("AskerUsage of a course with nothing = %+v, %v", got, err)
		}
	})

	t.Run("TenantUsage counts a key's use per tenant, since inclusive and until exclusive", func(t *testing.T) {
		s := open(t)
		school := func(agent, tenant string) scope {
			return scope{agent: agent, tenant: tenant, course: "c1", opener: "p1", key: "school"}
		}
		yuki1, yuki2, ken := school("a1", "ten_yuki"), school("a2", "ten_yuki"), school("a3", "ten_ken")
		own := scope{agent: "a1", tenant: "ten_yuki", course: "c1", opener: "p1", key: "own"}
		record(t, s,
			[]store.LLMCall{
				call("k1", day1, yuki1, 100), call("k2", day1.Add(time.Hour), yuki2, 200), call("k3", day1, ken, 50),
				call("k4", day1, own, 7000), call("k5", day2, yuki1, 9000), call("k6", day1.Add(-time.Microsecond), yuki1, 9000),
			},
			[]store.AnswerRecord{
				answerRecord("r1", day1, yuki1, true, 100), answerRecord("r2", day1.Add(time.Hour), yuki2, true, 200),
				answerRecord("r3", day1, yuki1, false, 0), answerRecord("r4", day1, ken, true, 50),
				answerRecord("r5", day1, own, true, 7000), answerRecord("r6", day2, yuki1, true, 9000),
			})
		got, err := s.TenantUsage(t.Context(), "school", day1, day2)
		if err != nil {
			t.Fatal(err)
		}
		want := []store.TenantUsage{
			{TenantID: "ten_ken", Answers: 1, ModelCalls: 1, CostPUSD: 50},
			{TenantID: "ten_yuki", Answers: 2, ModelCalls: 2, CostPUSD: 300},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("TenantUsage:\n got %+v\nwant %+v", got, want)
		}
		if got, err := s.TenantUsage(t.Context(), "school", day2.Add(24*time.Hour), day2.Add(48*time.Hour)); err != nil || len(got) != 0 {
			t.Errorf("TenantUsage of a day with nothing = %+v, %v", got, err)
		}
		for _, c := range []struct {
			key          string
			since, until time.Time
		}{{"", day1, day2}, {"school", day2, day1}, {"school", day1, day1}} {
			if _, err := s.TenantUsage(t.Context(), c.key, c.since, c.until); err == nil {
				t.Errorf("TenantUsage(%q, %s, %s) was taken", c.key, c.since, c.until)
			}
		}
	})

	t.Run("refuses a report of no agent or of an empty span", func(t *testing.T) {
		s := open(t)
		for _, c := range []struct {
			agent        string
			since, until time.Time
		}{{"", day1, day2}, {"a1", day2, day1}, {"a1", day1, day1}} {
			if _, err := s.Usage(t.Context(), c.agent, c.since, c.until); err == nil {
				t.Errorf("Usage(%q, %s, %s) was taken", c.agent, c.since, c.until)
			}
			if _, err := s.AskerUsage(t.Context(), c.agent, "c1", c.since, c.until); err == nil {
				t.Errorf("AskerUsage(%q, %s, %s) was taken", c.agent, c.since, c.until)
			}
		}
	})
}
