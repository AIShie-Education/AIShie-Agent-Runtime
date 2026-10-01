package api

import (
	"context"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// schoolRuntime is a runtime with the school's plan: two offers, one the
// price table prices, and three answers a day per owner.
func schoolRuntime() config.Runtime {
	three := 3
	return config.Runtime{School: config.School{
		Offers: []config.SchoolOffer{
			{ID: "standard", Label: "School AI", Adapter: "openai_chat", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com",
				KeyRef: "secret://school/keys/deepseek"},
			{ID: "haiku", Label: "School AI (Claude)", Adapter: "anthropic", Model: "claude-haiku-4-5", KeyRef: "secret://school/keys/anthropic"},
		},
		PerOwnerDay: config.Quota{Answers: &three},
	}}
}

// noSchoolKey fails when anything the API said, logged, audited or stored
// refers to a key of the school's.
func (h *hostWorld) noSchoolKey(answers ...answer) {
	h.t.Helper()
	var b strings.Builder
	for _, a := range answers {
		b.WriteString(a.body)
	}
	b.WriteString(h.log.String())
	events, err := h.st.AuditEvents(context.Background(), at.AddDate(-1, 0, 0), 1000)
	h.ok(err)
	for _, e := range events {
		b.Write(e.Detail)
	}
	rows, err := h.st.HostedAgents(context.Background())
	h.ok(err)
	for _, r := range rows {
		b.Write(r.Settings)
	}
	if text := b.String(); strings.Contains(text, "school/keys") || strings.Contains(text, "secret://") {
		h.t.Errorf("a key of the school's is referred to: %s", text)
	}
	h.noSecrets(answers...)
}

// The school's plan through the API (D8): /info and /models offer it, with
// its offers' labels and models and its quotas, never their keys; PATCH
// puts an agent on an offer by its id alone, and the owner's own model and
// key behind it as its fallback; the agent then has a model; the view says
// the offer and the owner's use of the plan today across their agents; and
// null takes the agent off the plan.
func TestSchoolPlan(t *testing.T) {
	h, _, fh := newModelWorld(t, schoolRuntime())
	ctx := context.Background()
	var info Info
	infoAnswer := h.call("GET", "info", h.yuki, "")
	infoAnswer.decode(t, &info)
	if !info.Features.SchoolKey || !info.Features.OwnKey {
		t.Errorf("features: %+v", info.Features)
	}
	models := h.call("GET", "models", h.yuki, "")
	var m Models
	models.decode(t, &m)
	want := SchoolKeyOffers{Offered: true, Offers: []SchoolOffer{
		{ID: "standard", Label: "School AI", Provider: "deepseek", Model: "deepseek-chat", Priced: true},
		{ID: "haiku", Label: "School AI (Claude)", Provider: "anthropic", Model: "claude-haiku-4-5"},
	}, Limits: SchoolLimits{PerOwnerDay: 3, PerAskerDay: config.DefaultPerAskerDay}}
	if got := m.SchoolKey; len(got.Offers) != 2 || got.Offers[0] != want.Offers[0] || got.Offers[1] != want.Offers[1] || got.Limits != want.Limits || !got.Offered {
		t.Errorf("GET /models' school_key: %+v", got)
	}

	v := h.host(h.yuki, h.helper.ID)
	if v.Status != StatusNeedsModel || v.Model.School != nil || v.Today.School != nil {
		t.Fatalf("connected: %+v", v)
	}
	var got HostedAgent
	onPlan := h.patch(v.ID, `"1"`, `{"model":{"school":{"offer":"standard"}}}`)
	onPlan.decode(t, &got)
	if onPlan.code != 200 || got.Version != 2 || got.Status != StatusStarting || got.Model.Own != nil || got.OwnKey != nil {
		t.Fatalf("on the plan: %d %s", onPlan.code, onPlan.body)
	}
	if s := got.Model.School; s == nil || *s != (SchoolModel{Offer: "standard", Label: "School AI", Model: "deepseek-chat", Provider: "deepseek", Offered: true}) {
		t.Errorf("model.school: %+v", got.Model.School)
	}
	if u := got.Today.School; u == nil || *u != (SchoolUse{Scope: "owner", Used: 0, Limit: 3, UsedUSD: "0.000000", PerAskerLimit: 20}) {
		t.Errorf("today.school: %+v", got.Today.School)
	}
	row, err := h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	if string(row.Settings) != `{"model":{"key_source":"school","offer":"standard"}}` || row.KeySecretID != "" {
		t.Errorf("the row: %s", row.Settings)
	}
	ev := h.events("agent.update")
	if len(ev) != 1 || !strings.Contains(string(ev[0].Detail), `"changed":["model.school"]`) || !strings.Contains(string(ev[0].Detail), `"offer":"standard"`) {
		t.Errorf("the audit: %+v", ev)
	}
	// The registry runs it on the offer, with the school's key.
	yaml := fh.YAML()
	cfg, _, err := registry.Build(ctx, yaml, h.st, registry.Options{CoreBaseURL: h.srv.URL})
	h.ok(err)
	if len(cfg.Agents) != 1 || cfg.Agents[0].Model.KeyRef != "secret://school/keys/deepseek" || cfg.Agents[0].Model.Offer != "standard" {
		t.Fatalf("the build: %+v %+v", cfg.Agents, cfg.Rejected)
	}

	// The owner's own model and key behind it: the fallback.
	withOwn := h.patch(v.ID, `"2"`, ownOpenAI)
	withOwn.decode(t, &got)
	if withOwn.code != 200 || got.Version != 3 || got.Model.Own == nil || got.Model.Own.Model != "gpt-4.1-mini" || got.Model.School == nil ||
		!got.Model.School.Fallback || got.OwnKey == nil {
		t.Fatalf("with the owner's key behind: %d %s", withOwn.code, withOwn.body)
	}
	row, err = h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	if string(row.Settings) != `{"model":{"key_source":"school","offer":"standard","fallback":{"adapter":"openai_chat","model":"gpt-4.1-mini","provider":"openai","key_source":"own"}}}` {
		t.Errorf("the row: %s", row.Settings)
	}
	cfg, _, err = registry.Build(ctx, yaml, h.st, registry.Options{CoreBaseURL: h.srv.URL})
	h.ok(err)
	if len(cfg.Agents) != 1 || cfg.Agents[0].Model.Fallback == nil || cfg.Agents[0].Model.Fallback.KeyRef != "sealed://"+row.KeySecretID {
		t.Fatalf("the build with a fallback: %+v %+v", cfg.Agents, cfg.Rejected)
	}

	// Today's use of the plan is the owner's, across their agents, on the
	// school's key alone.
	for i, a := range []store.AnswerRecord{
		{AgentID: v.ID, KeySource: config.KeySchool, Billable: true},
		{AgentID: "agt_yukis_other", KeySource: config.KeySchool, Billable: true},
		{AgentID: v.ID, KeySource: config.KeyOwn, Billable: true},
		{AgentID: v.ID, KeySource: config.KeySchool, Billable: false, Outcome: store.OutcomeQuota},
	} {
		a.ID, a.At, a.TenantID, a.CourseID, a.ConversationID, a.MessageID = "ans-"+string(rune('a'+i)), at, row.TenantID, h.co.ID, "c", "m"
		if a.Outcome == "" {
			a.Outcome = store.OutcomePosted
		}
		h.ok(h.st.RecordAnswer(ctx, a))
	}
	h.call("GET", "agents/"+v.ID, h.yuki, "").decode(t, &got)
	if u := got.Today.School; u == nil || u.Used != 2 || u.Limit != 3 || u.Scope != "owner" || got.Today.Answers != 2 {
		t.Errorf("today: %+v %+v", got.Today, got.Today.School)
	}

	// Another offer; then off the plan, with the owner's model kept.
	other := h.patch(v.ID, `"3"`, `{"model":{"school":{"offer":"haiku"}}}`)
	other.decode(t, &got)
	if other.code != 200 || got.Model.School == nil || got.Model.School.Label != "School AI (Claude)" || got.Model.Own == nil {
		t.Fatalf("another offer: %d %s", other.code, other.body)
	}
	off := h.patch(v.ID, `"4"`, `{"model":{"school":null}}`)
	off.decode(t, &got)
	if off.code != 200 || got.Model.School != nil || got.Today.School != nil || got.Model.Own == nil || got.Status == StatusNeedsModel {
		t.Fatalf("off the plan: %d %s", off.code, off.body)
	}
	row, err = h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	if string(row.Settings) != `{"model":{"adapter":"openai_chat","model":"gpt-4.1-mini","provider":"openai","key_source":"own"}}` {
		t.Errorf("the row off the plan: %s", row.Settings)
	}
	ev = h.events("agent.update")
	if len(ev) != 4 || !strings.Contains(string(ev[3].Detail), `"changed":["model.school"]`) {
		t.Errorf("the audit: %+v", ev)
	}

	// On the plan again, and the school withdraws the offer: the view says
	// so, and the agent keeps its model.
	again := h.patch(v.ID, `"5"`, `{"model":{"school":{"offer":"standard"}}}`)
	if again.code != 200 {
		t.Fatalf("again: %d %s", again.code, again.body)
	}
	fh.mu.Lock()
	fh.yaml = &config.Config{}
	fh.mu.Unlock()
	withdrawn := h.call("GET", "agents/"+v.ID, h.yuki, "")
	withdrawn.decode(t, &got)
	if s := got.Model.School; s == nil || s.Offered || s.Label != "standard" || s.Model != "" || !s.Fallback {
		t.Errorf("an offer withdrawn: %+v", s)
	}
	var none Info
	h.call("GET", "info", h.yuki, "").decode(t, &none)
	if none.Features.SchoolKey {
		t.Error("no offers, and the school's key is a feature")
	}
	h.noSchoolKey(infoAnswer, models, onPlan, withOwn, other, off, again, withdrawn)
}

// PATCH refuses an offer the school does not have, and one not named, at
// the offer's pointer; and writes nothing for them.
func TestSchoolPlanRefuses(t *testing.T) {
	h, _, _ := newModelWorld(t, schoolRuntime())
	v := h.host(h.yuki, h.helper.ID)
	for _, tc := range []struct {
		name, body          string
		status              int
		code, reason, field string
	}{
		{"an offer the school does not have", `{"model":{"school":{"offer":"premium"}}}`, 400, CodeInvalidArgument, ReasonUnknownOffer, "/model/school/offer"},
		{"no offer", `{"model":{"school":{}}}`, 400, CodeInvalidArgument, ReasonMissingField, "/model/school/offer"},
		{"an empty offer", `{"model":{"school":{"offer":""}}}`, 400, CodeInvalidArgument, ReasonMissingField, "/model/school/offer"},
		{"the school's model written in", `{"model":{"school":{"offer":"standard","model":"o3"}}}`, 400, CodeInvalidArgument, ReasonUnknownField, "/model/school/model"},
		{"not an object", `{"model":{"school":"standard"}}`, 400, CodeInvalidArgument, ReasonInvalidField, "/model/school"},
		{"the owner's model with no key behind the plan", `{"model":{"school":{"offer":"standard"},"own":{"provider":"openai","model":"gpt-4.1-mini"}}}`, 422,
			CodeFailedPrecondition, ReasonOwnKeyRequired, "/own_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := wantRefused(t, h.patch(v.ID, `"1"`, tc.body), tc.status, tc.code, tc.reason)
			if e.Details["field"] != tc.field {
				t.Errorf("field %v, want %s", e.Details["field"], tc.field)
			}
		})
	}
	if row, _ := h.st.HostedAgent(context.Background(), v.ID); row.Version != 1 {
		t.Errorf("a refused patch wrote: version %d", row.Version)
	}
}
