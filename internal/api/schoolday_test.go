package api

import (
	"context"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

func hongKong(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Hong_Kong")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestSchoolPlanUsageUsesSchoolMidnight(t *testing.T) {
	hk := hongKong(t)
	f := newFixture(t, func(o *Options) {
		o.Hosting = &fakeHosting{yaml: &config.Config{Runtime: schoolRuntime()}}
		o.Location = func() *time.Location { return hk }
	})
	day := store.Day(at, hk)
	if err := f.st.RecordAnswer(context.Background(), store.AnswerRecord{
		ID: "before-school-day", At: day.Add(-time.Hour), TenantID: "ten_" + yuki, AgentID: "agt_y", CourseID: "c", ConversationID: "x",
		MessageID: "before-school-day", Outcome: store.OutcomePosted, Billable: true, KeySource: config.KeySchool,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RecordAnswer(context.Background(), store.AnswerRecord{
		ID: "in-school-day", At: day, TenantID: "ten_" + yuki, AgentID: "agt_y", CourseID: "c", ConversationID: "x",
		MessageID: "in-school-day", Outcome: store.OutcomePosted, Billable: true, KeySource: config.KeySchool,
	}); err != nil {
		t.Fatal(err)
	}
	a := f.get(Prefix+"admin/school-plan/usage", f.core.assert(t, claims(ken, "admin", nil)))
	var got SchoolPlanUsage
	a.decode(t, &got)
	if a.code != 200 || !got.Since.Equal(day) || got.Total.Answers != 1 {
		t.Fatalf("%d %s", a.code, a.body)
	}
}

func TestCostReportUsesSchoolCivilDates(t *testing.T) {
	h, _, _ := newModelWorld(t, schoolRuntime())
	hk := hongKong(t)
	h.s.o.Location = func() *time.Location { return hk }
	for _, c := range []struct {
		id string
		at time.Time
	}{
		{"before-school-day", time.Date(2026, 9, 27, 15, 59, 59, 0, time.UTC)},
		{"school-day-early", time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)}, // 04:00 on 28 September in Hong Kong.
		{"school-day-late", time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)},   // 10:00 on 28 September in Hong Kong.
		{"next-school-day", time.Date(2026, 9, 28, 16, 0, 0, 0, time.UTC)},  // 00:00 on 29 September in Hong Kong.
	} {
		if err := h.st.RecordLLMCall(context.Background(), store.LLMCall{
			ID: c.id, At: c.at, TenantID: "ten_" + yuki, AgentID: "agt_y", CourseID: "c", ConversationID: "x", MessageID: c.id,
			Provider: "openai", Model: "gpt-test", CostPUSD: 1_000_000, PriceVersion: "test/1", KeySource: config.KeySchool,
		}); err != nil {
			t.Fatal(err)
		}
	}
	a := h.admin("GET", "admin/costs?since=2026-09-28&until=2026-09-28", "")
	var got CostReport
	a.decode(t, &got)
	if a.code != 200 || got.Since != "2026-09-28" || got.Until != "2026-09-28" || len(got.Rows) != 1 {
		t.Fatalf("%d %s", a.code, a.body)
	}
	row := got.Rows[0]
	if row.Day == nil || *row.Day != "2026-09-28" || len(row.Lines) != 1 || row.Lines[0].Calls != 2 || row.CostUSD != "0.000002" {
		t.Errorf("the school day's row: %+v", row)
	}
}
