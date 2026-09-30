package config

import (
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
)

// schoolRuntime is baseRuntime with the school's plan: one offer on the
// school's Anthropic key.
func schoolRuntime() map[string]any {
	rt := baseRuntime()
	rt["school"] = map[string]any{
		"offers": []any{map[string]any{
			"id": "standard", "label": "School AI", "adapter": "anthropic", "model": "claude-haiku-4-5",
			"key_ref": "secret://school/keys/anthropic", "params": map[string]any{"max_output_tokens": 1200},
		}},
	}
	return rt
}

// planAgent is an agent on the plan's offer as the registry writes one: the
// offer's settings, its tenant, and no quotas of its own.
func planAgent() map[string]any {
	a := baseAgent()
	a["tenant_id"] = "ten_owner"
	a["model"] = map[string]any{
		"adapter": "anthropic", "model": "claude-haiku-4-5", "key_ref": "secret://school/keys/anthropic", "key_source": "school",
		"offer": "standard", "fallback": map[string]any{"adapter": "openai_chat", "model": "gpt-x", "key_ref": "env://OWN_KEY", "key_source": "own"},
	}
	delete(a, "budgets")
	return a
}

// The school's plan is checked with the runtime's settings: each offer a
// model on the school's key, under secret://school/keys/, of a model the
// lists allow, with an id and a label; and its quotas. An agent on an
// offer is held by the plan's quotas and needs its tenant alone, but must
// be the offer as the registry writes it.
func TestValidateSchool(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime map[string]any // paths to set in schoolRuntime's school
		agent   map[string]any // paths to set in planAgent; nil for baseAgent
		want    []problem
	}{
		{name: "an offer, and an agent on it with no quotas of its own", agent: map[string]any{}},
		{name: "quotas in answers and dollars", runtime: map[string]any{
			"per_owner_day": map[string]any{"answers": 50, "usd": 2}, "per_asker_day": map[string]any{"answers": 5}, "per_day": map[string]any{"answers": 1000},
		}},
		{name: "quotas must be positive", runtime: map[string]any{
			"per_owner_day": map[string]any{"answers": 0}, "per_asker_day": map[string]any{"usd": -1}, "per_day": map[string]any{"answers": -3},
		}, want: []problem{
			{path: "runtime.school.per_owner_day.answers"}, {path: "runtime.school.per_asker_day.usd"}, {path: "runtime.school.per_day.answers"},
		}},
		{name: "an offer's id and label", runtime: map[string]any{"offers": []any{
			map[string]any{"id": "a b", "label": " ", "adapter": "anthropic", "model": "claude-haiku-4-5", "key_ref": "secret://school/keys/a"},
			map[string]any{"id": "x", "label": strings.Repeat("L", 81), "adapter": "anthropic", "model": "claude-haiku-4-5", "key_ref": "secret://school/keys/a"},
			map[string]any{"id": "x", "label": "Two\nlines", "adapter": "anthropic", "model": "claude-haiku-4-5", "key_ref": "secret://school/keys/a"},
		}}, want: []problem{
			{path: "runtime.school.offers[0].id", msg: "required"}, {path: "runtime.school.offers[0].label", msg: "required"},
			{path: "runtime.school.offers[1].label", msg: "one line"},
			{path: "runtime.school.offers[2].id", msg: "offers[1] has this id too"}, {path: "runtime.school.offers[2].label", msg: "one line"},
		}},
		{name: "an offer's model", runtime: map[string]any{"offers": []any{
			map[string]any{"id": "a", "label": "A", "adapter": "palm", "model": " ", "key_ref": "secret://school/keys/a", "params": map[string]any{"max_output_tokens": -1}},
		}}, want: []problem{
			{path: "runtime.school.offers[0].adapter"}, {path: "runtime.school.offers[0].model", msg: "required"},
			{path: "runtime.school.offers[0].params.max_output_tokens"},
		}},
		{name: "an offer's key is the school's", runtime: map[string]any{"offers": []any{
			map[string]any{"id": "a", "label": "A", "adapter": "anthropic", "model": "claude-haiku-4-5", "key_ref": "secret://ten_1/keys/a"},
			map[string]any{"id": "b", "label": "B", "adapter": "anthropic", "model": "claude-haiku-4-5", "key_ref": "env://SCHOOL_KEY"},
			map[string]any{"id": "c", "label": "C", "adapter": "anthropic", "model": "claude-haiku-4-5"},
		}}, want: []problem{
			{path: "runtime.school.offers[0].key_ref", msg: "secret://school/keys/<name>"},
			{path: "runtime.school.offers[1].key_ref", msg: "secret://school/keys/<name>"},
			{path: "runtime.school.offers[2].key_ref", msg: "required"},
		}},
		{name: "a key pasted as an offer's key is not repeated", runtime: map[string]any{"offers": []any{
			map[string]any{"id": "a", "label": "A", "adapter": "anthropic", "model": "claude-haiku-4-5", "key_ref": "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"},
		}}, want: []problem{{path: "runtime.school.offers[0].key_ref", msg: "never written in configuration"}}},
		{name: "an offer the school's lists do not allow", runtime: map[string]any{"offers": []any{
			map[string]any{"id": "a", "label": "A", "adapter": "anthropic", "model": "claude-x-preview", "key_ref": "secret://school/keys/a"},
			map[string]any{"id": "b", "label": "B", "adapter": "gemini", "model": "gemini-pro", "key_ref": "secret://school/keys/b"},
		}}, want: []problem{
			{path: "runtime.school.offers[0]", msg: "denied by runtime.denied_models"},
			{path: "runtime.school.offers[1]", msg: "gemini:gemini:gemini-pro is not in runtime.allowed_models"},
		}},
		{name: "the notice", runtime: map[string]any{"on_quota_text": strings.Repeat("x", 1001)}, want: []problem{{path: "runtime.school.on_quota_text", msg: "at most 1000"}}},
		{name: "a blank notice", runtime: map[string]any{"on_quota_text": "  "}, want: []problem{{path: "runtime.school.on_quota_text", msg: "holds no text"}}},

		{name: "an agent on an offer needs its tenant", agent: map[string]any{"tenant_id": del{}}, want: []problem{{agent: "a1", path: "agent.tenant_id", msg: "required on the school's key"}}},
		{name: "an agent on an offer the school no longer has", agent: map[string]any{"model.offer": "gone"}, want: []problem{{agent: "a1", path: "agent.model.offer", msg: `"gone" is not an offer of runtime.school`}}},
		{name: "an agent on an offer calling something else", agent: map[string]any{"model.model": "claude-sonnet-4-5"}, want: []problem{{agent: "a1", path: "agent.model", msg: "is not runtime.school's offer standard"}}},
		{name: "an agent on an offer with another key", agent: map[string]any{"model.key_ref": "secret://school/keys/other"}, want: []problem{{agent: "a1", path: "agent.model", msg: "is not runtime.school's offer standard"}}},
		{name: "an offer on the owner's key", agent: map[string]any{"model.key_source": "own"}, want: []problem{{agent: "a1", path: "agent.model.offer", msg: "on the school's key"}}},
		{name: "a fallback on an offer", agent: map[string]any{"model.fallback.offer": "standard", "model.fallback.key_source": "school"}, want: []problem{
			{agent: "a1", path: "agent.model.fallback.offer", msg: "a fallback is not an offer"},
			{agent: "a1", path: "agent.budgets.per_agent_day", msg: "required on the school's key"},
			{agent: "a1", path: "agent.budgets.per_asker_day", msg: "required on the school's key"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := schoolRuntime()
			school := rt["school"].(map[string]any)
			for p, v := range tc.runtime {
				set(school, p, v)
			}
			agent := baseAgent()
			if tc.agent != nil {
				agent = planAgent()
				for p, v := range tc.agent {
					set(agent, p, v)
				}
			}
			dir := write(t, map[string]string{
				"agent.yaml":   yamlOf(t, map[string]any{"agent": agent}),
				"runtime.yaml": yamlOf(t, map[string]any{"runtime": rt}),
			})
			_, err := Load(dir+"/agent.yaml", dir+"/runtime.yaml")
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			expectProblems(t, err, tc.want)
			for _, p := range Problems(err) {
				if strings.Contains(p.Msg, "sk-ant") {
					t.Fatalf("a problem repeats a key: %v", p)
				}
			}
		})
	}
}

// The plan's answer quotas default to 100 per owner and 20 per asker, each
// kept when set; dollars are only ever the operator's.
func TestSchoolQuotas(t *testing.T) {
	var s School
	if o, a := s.OwnerQuota(), s.AskerQuota(); *o.Answers != DefaultPerOwnerDay || *a.Answers != DefaultPerAskerDay || o.USD != nil || a.USD != nil {
		t.Fatalf("defaults: %+v %+v", o, a)
	}
	if s.USD() || s.Offered() {
		t.Fatal("an empty plan has no dollars and no offers")
	}
	fifty, usd := 50, 1.5
	s = School{PerOwnerDay: Quota{Answers: &fifty}, PerAskerDay: Quota{USD: &usd}, Offers: []SchoolOffer{{ID: "a"}}}
	if o, a := s.OwnerQuota(), s.AskerQuota(); *o.Answers != 50 || *a.Answers != DefaultPerAskerDay || *a.USD != 1.5 {
		t.Fatalf("set: %+v %+v", o, a)
	}
	if !s.USD() || !s.Offered() {
		t.Fatal("dollars and an offer")
	}
	if _, ok := s.OfferOf("a"); !ok {
		t.Fatal("OfferOf")
	}
	if _, ok := s.OfferOf("b"); ok {
		t.Fatal("OfferOf an offer not there")
	}
	// The defaults' own maps are not touched.
	if s.PerAskerDay.Answers != nil {
		t.Fatal("AskerQuota wrote into the plan")
	}
}

// The notice is the language answers are fixed to, else English and
// Traditional Chinese; the plan's own text wins.
func TestSchoolQuotaText(t *testing.T) {
	both := SchoolQuotaTextEn + "\n\n" + SchoolQuotaTextZhHant
	for _, tc := range []struct{ lang, want string }{
		{LanguageOpener, both},
		{"fixed:en", SchoolQuotaTextEn},
		{"fixed:en-GB", SchoolQuotaTextEn},
		{"fixed:zh-Hant", SchoolQuotaTextZhHant},
		{"fixed:zh-TW", SchoolQuotaTextZhHant},
		{"fixed:zh", SchoolQuotaTextZhHant},
		{"fixed:zh-Hans", SchoolQuotaTextZhHans},
		{"fixed:zh-hans-CN", SchoolQuotaTextZhHans},
		{"fixed:zh-CN", SchoolQuotaTextZhHans},
		{"fixed:ja", both},
	} {
		if got := SchoolQuotaText("", tc.lang); got != tc.want {
			t.Errorf("%s: %q", tc.lang, got)
		}
	}
	if got := SchoolQuotaText("Ask again on Monday.", "fixed:zh-Hant"); got != "Ask again on Monday." {
		t.Errorf("the plan's own text: %q", got)
	}
}

// An offer is the model section the registry writes: the offer's settings
// on the school's key, where the model is always written out, and no more
// than it sets otherwise.
func TestSchoolOfferSection(t *testing.T) {
	temp, strict := 0.2, false
	o := SchoolOffer{ID: "std", Label: "Std", Adapter: "openai_chat", Model: "m", BaseURL: "https://api.deepseek.com", KeyRef: "secret://school/keys/ds",
		Params: ModelParams{Temperature: &temp}, Reasoning: Reasoning{Effort: "low"}, Capabilities: Capabilities{StrictTools: &strict}}
	got := yamlOf(t, o.Section())
	want := "adapter: openai_chat\nbase_url: https://api.deepseek.com\ncapabilities:\n    strict_tools: false\nkey_ref: secret://school/keys/ds\nkey_source: school\n" +
		"model: m\noffer: std\nparams:\n    temperature: 0.2\nprovider: \"\"\nreasoning:\n    effort: low\nregion: \"\"\n"
	if got != want {
		t.Fatalf("section:\n%s\nwant\n%s", got, want)
	}
	if m := o.AsModel(); m.KeySource != KeySchool || m.Offer != "std" || m.KeyRef != o.KeyRef || m.Params.MaxOutputTokens != 0 {
		t.Fatalf("model: %+v", m)
	}
}

// The site's settings over runtime.yaml's plan (WithSite): the site's
// offers after runtime.yaml's, but one whose id runtime.yaml's has, or
// whose model the lists do not allow, which Withheld says why of; and the
// site's quotas, in answers and dollars, in place of runtime.yaml's, nil
// for none. Applied again, its offers replace those applied before.
func TestWithSite(t *testing.T) {
	ten, two := 10, 2.5
	rt := Runtime{
		DeniedModels: []string{"*:*:*-preview"},
		School: School{
			Offers:      []SchoolOffer{{ID: "standard", Label: "School AI", Adapter: "anthropic", Model: "claude-haiku-4-5", KeyRef: "secret://school/keys/a"}},
			PerOwnerDay: Quota{USD: &two},
			PerDay:      Quota{Answers: &ten},
		},
	}
	site := func(id, model string) SchoolOffer {
		return SchoolOffer{ID: id, Label: "Site " + id, Adapter: "openai_chat", Provider: "deepseek", Model: model, BaseURL: "https://api.deepseek.com",
			KeyRef: "sealed://sec_" + id, Site: true}
	}
	fifty, five := 50, 5
	s := Site{
		Offers: []SchoolOffer{site("fast", "deepseek-chat"), site("standard", "deepseek-chat"), site("preview", "deepseek-v9-preview")},
		Quotas: &SiteQuotas{PerOwnerDay: fifty, PerAskerDay: five},
	}
	for _, c := range []struct {
		o    SchoolOffer
		want string
	}{{s.Offers[0], ""}, {s.Offers[1], WithheldIDTaken}, {s.Offers[2], WithheldModelNotAllowed}} {
		if got := rt.Withheld(c.o); got != c.want {
			t.Errorf("Withheld(%s) = %q, want %q", c.o.ID, got, c.want)
		}
	}
	allowed := rt
	allowed.AllowedModels = []string{"anthropic:*:*"}
	if got := allowed.Withheld(s.Offers[0]); got != WithheldModelNotAllowed {
		t.Errorf("an offer runtime.allowed_models does not list: %q", got)
	}

	got := rt.WithSite(s)
	var ids []string
	for _, o := range got.School.Offers {
		ids = append(ids, o.ID)
	}
	if strings.Join(ids, " ") != "standard fast" || got.School.Offers[0].Site || !got.School.Offers[1].Site {
		t.Fatalf("the plan's offers: %+v", got.School.Offers)
	}
	sc := got.School
	if *sc.OwnerQuota().Answers != 50 || *sc.AskerQuota().Answers != 5 || sc.PerDay.Answers != nil || sc.PerOwnerDay.USD != nil || sc.USD() {
		t.Errorf("the plan's quotas: %+v", sc)
	}
	one := 1.25
	withUSD := rt.WithSite(Site{Quotas: &SiteQuotas{PerOwnerDay: 50, PerAskerDay: 5, PerAskerDayUSD: &one}}).School
	if withUSD.PerAskerDay.USD == nil || *withUSD.PerAskerDay.USD != 1.25 || withUSD.PerOwnerDay.USD != nil || !withUSD.USD() {
		t.Errorf("the plan's quotas in dollars: %+v", withUSD)
	}
	if len(got.Site.Offers) != 3 || got.Site.Quotas == nil {
		t.Errorf("the site's settings are not kept: %+v", got.Site)
	}
	if len(rt.School.Offers) != 1 || *rt.School.PerDay.Answers != 10 {
		t.Errorf("runtime.yaml's settings were changed: %+v", rt.School)
	}

	// Applied again: the site's offers replace those applied before; with
	// no site settings, runtime.yaml's quotas and offers stand.
	again := got.WithSite(Site{Offers: []SchoolOffer{site("fast", "deepseek-chat")}, Quotas: &SiteQuotas{PerOwnerDay: 1, PerAskerDay: 1, PerDay: &ten}})
	if len(again.School.Offers) != 2 || *again.School.PerDay.Answers != 10 || *again.School.OwnerQuota().Answers != 1 {
		t.Errorf("applied again: %+v", again.School)
	}
	none := rt.WithSite(Site{})
	if len(none.School.Offers) != 1 || *none.School.OwnerQuota().Answers != DefaultPerOwnerDay || *none.School.PerDay.Answers != 10 {
		t.Errorf("no site settings: %+v", none.School)
	}
}

// A hosted agent's model is called over the hosted-model client, its
// owner's and the site's offer's alike, but an offer of runtime.yaml's,
// whose endpoint is the operator's; a YAML agent's never is.
func TestOverHostedClient(t *testing.T) {
	hosted, yaml := &Agent{Hosted: &Hosted{}}, &Agent{}
	for _, c := range []struct {
		a    *Agent
		m    Model
		want bool
	}{
		{hosted, Model{KeySource: KeyOwn, KeyRef: "sealed://sec_k"}, true},
		{hosted, Model{KeySource: KeySchool, Offer: "fast", KeyRef: "sealed://sec_school"}, true},
		{hosted, Model{KeySource: KeySchool, Offer: "standard", KeyRef: "secret://school/keys/a"}, false},
		{yaml, Model{KeySource: KeyOwn, KeyRef: "sealed://sec_k"}, false},
		{yaml, Model{KeySource: KeySchool, KeyRef: "secret://school/keys/a"}, false},
	} {
		if got := c.a.OverHostedClient(c.m); got != c.want {
			t.Errorf("hosted %v, %+v: %v, want %v", c.a.Hosted != nil, c.m, got, c.want)
		}
	}
}

// The site's money over runtime.yaml's (WithSite): a tenant's quota in
// place of runtime.tenants' (none where it sets none), the others kept;
// the agents' daily budgets by default in place of runtime.defaults', none
// where the site sets none; and the price table in force, the site's rows
// under their version before the file's.
func TestWithSiteMoney(t *testing.T) {
	five, twenty, two := 5, 20, 2.0
	rt := Runtime{
		Tenants:  map[string]Tenant{"instr_42": {PerDay: Quota{Answers: &five}}, "dept_a": {PerDay: Quota{USD: &two}}},
		Defaults: map[string]any{"budgets": map[string]any{"per_agent_day": map[string]any{"answers": 200}, "per_answer": map[string]any{"turns": 4}}},
	}
	if a, k := rt.DefaultBudgets(); a.Answers == nil || *a.Answers != 200 || a.USD != nil || k.Answers != nil {
		t.Errorf("runtime.yaml's budgets: %+v %+v", a, k)
	}
	got := rt.WithSite(Site{
		Tenants: map[string]SiteQuota{"instr_42": {}, "ten_yuki": {Answers: &twenty, USD: &two}},
		Budgets: &SiteBudgets{PerAgentDay: SiteQuota{USD: &two}, PerAskerDay: SiteQuota{Answers: &five}},
	})
	if q := got.Tenants["instr_42"].PerDay; q.Answers != nil || q.USD != nil {
		t.Errorf("a tenant set to none: %+v", q)
	}
	if q := got.Tenants["ten_yuki"].PerDay; q.Answers == nil || *q.Answers != 20 || q.USD == nil || *q.USD != 2 {
		t.Errorf("a tenant of the site's: %+v", q)
	}
	if q := got.Tenants["dept_a"].PerDay; q.USD == nil || *q.USD != 2 {
		t.Errorf("runtime.yaml's other tenant: %+v", q)
	}
	if *rt.Tenants["instr_42"].PerDay.Answers != 5 || len(rt.Tenants) != 2 {
		t.Error("runtime.yaml's tenants were changed")
	}
	a, k := got.DefaultBudgets()
	if a.Answers != nil || a.USD == nil || *a.USD != 2 || k.Answers == nil || *k.Answers != 5 || k.USD != nil {
		t.Errorf("the site's budgets: %+v %+v", a, k)
	}
	if turns := got.Defaults["budgets"].(map[string]any)["per_answer"].(map[string]any)["turns"]; turns != 4 {
		t.Errorf("the per-answer budgets: %v", turns)
	}
	if a, _ := rt.DefaultBudgets(); a.Answers == nil || *a.Answers != 200 {
		t.Error("runtime.yaml's defaults were changed")
	}

	file, err := pricing.Parse([]byte("version: v1\nprices:\n  - {provider: openai, model: o3, from: 2025-01-01, usd_per_mtok: {input: 2, output: 8}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := Site{Prices: []pricing.Row{{ID: "o3", Provider: "openai", Model: "o3", From: file.Rows()[0].From, In: 1, Out: 4}},
		PricesChanged: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)}
	table := s.PriceTable(file)
	if p, ok := table.Lookup("openai", "o3", time.Now()); !ok || p.Version != "site-20260930T100000Z/o3" || table.Version != "v1+site-20260930T100000Z" {
		t.Errorf("the price table in force: %q %+v", table.Version, p)
	}
	if (Site{}).PriceTable(file) != file {
		t.Error("with no site's rows, not the file's table")
	}
}
