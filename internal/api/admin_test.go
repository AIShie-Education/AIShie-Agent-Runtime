package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// GET /admin/school-plan/usage is today's use of the school's key per
// tenant, for the runtime's administrators alone: an owner's with their
// actor id and name as last seen, and the total; nothing of yesterday or of
// the owners' own keys.
func TestSchoolPlanUsage(t *testing.T) {
	per := 500
	rt := schoolRuntime()
	rt.School.PerDay.Answers = &per
	f := newFixture(t, func(o *Options) { o.Hosting = &fakeHosting{yaml: &config.Config{Runtime: rt}} })
	ctx := context.Background()
	ok := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ok(f.st.PutPerson(ctx, store.Person{CoreActorID: yuki, DisplayName: "Yuki Tanaka", LastSeenAt: at}))
	n := 0
	rec := func(tenant, key string, when time.Time, billable bool, cost int64) {
		n++
		id := fmt.Sprintf("r%d", n)
		ok(f.st.RecordAnswer(ctx, store.AnswerRecord{ID: id, At: when, TenantID: tenant, AgentID: "agt_" + tenant, CourseID: "c", ConversationID: "x",
			MessageID: id, Outcome: store.OutcomePosted, Billable: billable, KeySource: key}))
		ok(f.st.RecordLLMCall(ctx, store.LLMCall{ID: id, At: when, TenantID: tenant, AgentID: "agt_" + tenant, CourseID: "c", ConversationID: "x",
			MessageID: id, CostPUSD: cost, KeySource: key}))
	}
	rec("ten_"+yuki, config.KeySchool, at, true, 1_000_000)
	rec("ten_"+yuki, config.KeySchool, at.Add(-time.Hour), true, 2_000_000)
	rec("ten_"+yuki, config.KeyOwn, at, true, 9_000_000)
	rec("ten_"+yuki, config.KeySchool, at.AddDate(0, 0, -1), true, 9_000_000)
	rec("ten_"+ken, config.KeySchool, at, false, 0)
	rec("ten_instr_42", config.KeySchool, at, true, 500_000)

	a := f.get(Prefix+"admin/school-plan/usage", f.core.assert(t, claims(ken, "admin", nil)))
	wantSecured(t, a, "no-store")
	var got SchoolPlanUsage
	a.decode(t, &got)
	if a.code != 200 || !got.Since.Equal(store.UTCDay(at)) || got.Limits != (SchoolPlanLimits{PerOwnerDay: 3, PerAskerDay: 20, PerDay: got.Limits.PerDay}) ||
		got.Limits.PerDay == nil || *got.Limits.PerDay != 500 {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if got.Total != (PlanUse{Answers: 3, ModelCalls: 4, CostUSD: "0.000004"}) || len(got.Owners) != 3 {
		t.Fatalf("the total and owners: %s", a.body)
	}
	byTenant := map[string]OwnerPlanUse{}
	for _, o := range got.Owners {
		byTenant[o.TenantID] = o
	}
	if y := byTenant["ten_"+yuki]; y.OwnerActorID == nil || *y.OwnerActorID != yuki || y.DisplayName == nil || *y.DisplayName != "Yuki Tanaka" ||
		y.PlanUse != (PlanUse{Answers: 2, ModelCalls: 2, CostUSD: "0.000003"}) {
		t.Errorf("Yuki's: %+v", y)
	}
	if k := byTenant["ten_"+ken]; k.OwnerActorID == nil || k.DisplayName != nil || k.Answers != 0 || k.ModelCalls != 1 {
		t.Errorf("Ken's, not seen by the API: %+v", k)
	}
	if o := byTenant["ten_instr_42"]; o.OwnerActorID != nil || o.DisplayName != nil || o.Answers != 1 {
		t.Errorf("a YAML tenant's: %+v", o)
	}
	// Anyone else: forbidden.
	for _, role := range []string{"", "instructor"} {
		wantRefused(t, f.get(Prefix+"admin/school-plan/usage", f.core.assert(t, claims(yuki, role, nil))), 403, CodeForbidden, ReasonNotAdmin)
	}
}
