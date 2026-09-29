package config

import (
	"strings"
	"testing"
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
// on the school's key, and no more than it sets.
func TestSchoolOfferSection(t *testing.T) {
	temp, strict := 0.2, false
	o := SchoolOffer{ID: "std", Label: "Std", Adapter: "openai_chat", Model: "m", BaseURL: "https://api.deepseek.com", KeyRef: "secret://school/keys/ds",
		Params: ModelParams{Temperature: &temp}, Reasoning: Reasoning{Effort: "low"}, Capabilities: Capabilities{StrictTools: &strict}}
	got := yamlOf(t, o.Section())
	want := "adapter: openai_chat\nbase_url: https://api.deepseek.com\ncapabilities:\n    strict_tools: false\nkey_ref: secret://school/keys/ds\nkey_source: school\n" +
		"model: m\noffer: std\nparams:\n    temperature: 0.2\nreasoning:\n    effort: low\n"
	if got != want {
		t.Fatalf("section:\n%s\nwant\n%s", got, want)
	}
	if m := o.AsModel(); m.KeySource != KeySchool || m.Offer != "std" || m.KeyRef != o.KeyRef || m.Params.MaxOutputTokens != 0 {
		t.Fatalf("model: %+v", m)
	}
}
