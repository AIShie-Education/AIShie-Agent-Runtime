package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// jsonAgent is a hosted agent's document as the registry makes one: JSON,
// which is YAML.
func jsonAgent(t *testing.T, agent map[string]any, courses map[string]any) []byte {
	t.Helper()
	doc := map[string]any{"agent": agent}
	if courses != nil {
		doc["courses"] = courses
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ownAgent(id string) map[string]any {
	return map[string]any{
		"id": id, "display_name": "Agent " + id, "tenant_id": "ten_owner", "paused": false,
		"core":  map[string]any{"base_url": "https://lms.example.edu", "token_ref": "sealed://sec_t_" + id},
		"model": map[string]any{"adapter": "openai_chat", "model": "gpt-4.1-mini", "key_ref": "sealed://sec_k_" + id, "key_source": "own"},
	}
}

// LoadDocuments loads each source on its own: those that pass are agents,
// over the runtime's defaults and held to its rules and the allowlist;
// each that does not is rejected with every problem, and keeps no other
// from loading.
func TestLoadDocuments(t *testing.T) {
	base, err := Load(write(t, map[string]string{"runtime.yaml": `
runtime:
  defaults:
    polling: {inbox_idle_s: 20, inbox_max_s: 40}
    prompt: {on_quota_text: "Out of quota for today."}
  denied_models: ["*:*:*-preview"]
`}))
	if err != nil {
		t.Fatal(err)
	}
	bad := func(mutate func(a map[string]any)) map[string]any {
		a := ownAgent("agt_bad")
		mutate(a)
		return a
	}
	sources := []Source{
		{Name: "registry:agt_ok", Data: jsonAgent(t, ownAgent("agt_ok"), map[string]any{
			tutorCourse: map[string]any{"prompt_append_text": "Answer in English.", "polling": map[string]any{"inbox_idle_s": 15}},
		})},
		{Name: "registry:agt_unknown", Data: jsonAgent(t, bad(func(a map[string]any) { a["id"] = "agt_unknown"; a["colour"] = "blue" }), nil)},
		{Name: "registry:agt_types", Data: jsonAgent(t, bad(func(a map[string]any) {
			a["id"] = "agt_types"
			a["answer"] = map[string]any{"max_attempts": 2.5, "history_messages": "many"}
		}), nil)},
		{Name: "registry:agt_outside", Data: jsonAgent(t, bad(func(a map[string]any) {
			a["id"] = "agt_outside"
			a["core"].(map[string]any)["base_url"] = "https://evil.example.net"
		}), nil)},
		{Name: "registry:agt_denied", Data: jsonAgent(t, bad(func(a map[string]any) {
			a["id"] = "agt_denied"
			a["model"].(map[string]any)["model"] = "gpt-5-preview"
		}), nil)},
		{Name: "registry:agt_course", Data: jsonAgent(t, bad(func(a map[string]any) { a["id"] = "agt_course" }),
			map[string]any{tutorCourse: map[string]any{"core": map[string]any{"token_ref": "sealed://sec_other"}}})},
		{Name: "registry:agt_token", Data: jsonAgent(t, bad(func(a map[string]any) {
			a["id"] = "agt_token"
			a["core"].(map[string]any)["token_ref"] = "ais_k7v2m4qhx3ab_9Jx2abcDEFghiJKLmnoPQRstuVWXyz0123456789_-abcd"
		}), nil)},
		{Name: "registry:runtime", Data: []byte(`{"runtime": {"tenants": {}}}`)},
		{Name: "registry:two", Data: append(append(jsonAgent(t, ownAgent("agt_a"), nil), "\n---\n"...), jsonAgent(t, ownAgent("agt_b"), nil)...)},
		{Name: "registry:garbled", Data: []byte(`{"agent": {"id": "agt_garbled", `)},
	}
	agents, rejected := LoadDocuments(base, []string{"https://lms.example.edu"}, sources...)
	if len(agents) != 1 || agents[0].ID != "agt_ok" {
		t.Fatalf("loaded %d agents, want agt_ok alone; rejected %+v", len(agents), rejected)
	}
	a := agents[0]
	if a.File != "registry:agt_ok" || a.Dir != "" || a.Polling.InboxIdleS != 20 || a.Prompt.OnQuotaText != "Out of quota for today." ||
		a.Core.TokenRef != "sealed://sec_t_agt_ok" || a.Model.KeyRef != "sealed://sec_k_agt_ok" || a.Answer.MaxAttempts != 3 {
		t.Errorf("the agent loaded: %+v", a)
	}
	e, err := a.ForCourse(tutorCourse)
	if err != nil || e.PromptAppendText != "Answer in English." || e.Polling.InboxIdleS != 15 || !e.Enabled {
		t.Errorf("its course: %+v, %v", e, err)
	}

	want := map[string]string{
		"registry:agt_unknown": "agent.colour: unknown field",
		"registry:agt_types":   "agent.answer.max_attempts: must be a whole number",
		"registry:agt_outside": "evil.example.net is not within CORE_BASE_URL_ALLOWLIST",
		"registry:agt_denied":  "is denied by runtime.denied_models",
		"registry:agt_course":  "courses." + tutorCourse + ".core: not allowed here",
		"registry:agt_token":   "tokens are never written in configuration",
		"registry:runtime":     "holds one agent document, and nothing else",
		"registry:two":         "holds one agent document, and nothing else",
		"registry:garbled":     "registry:garbled",
	}
	if len(rejected) != len(want) {
		t.Errorf("%d rejected, want %d: %+v", len(rejected), len(want), rejected)
	}
	for _, r := range rejected {
		w, ok := want[r.Source]
		if !ok {
			t.Errorf("rejected %s", r.Source)
			continue
		}
		if !strings.Contains(r.Err.Error(), w) {
			t.Errorf("%s: %v, want it to say %q", r.Source, r.Err, w)
		}
		if strings.Contains(r.Err.Error(), "ais_k7v2") || strings.Contains(r.Detail(), "ais_k7v2") {
			t.Errorf("%s: a problem repeats the token: %v", r.Source, r.Err)
		}
		if strings.HasPrefix(r.Source, "registry:agt_") && r.AgentID != strings.TrimPrefix(r.Source, "registry:") {
			t.Errorf("%s: rejected as agent %q", r.Source, r.AgentID)
		}
		if strings.Contains(r.Detail(), r.Source) {
			t.Errorf("%s: its detail names its source: %s", r.Source, r.Detail())
		}
	}
	// The types' problems both: every problem, not the first.
	for _, r := range rejected {
		if r.Source == "registry:agt_types" && !strings.Contains(r.Detail(), "agent.answer.history_messages: must be a whole number") {
			t.Errorf("agt_types' detail: %s", r.Detail())
		}
	}
}

func TestLoadDocumentsWithoutARuntime(t *testing.T) {
	agents, rejected := LoadDocuments(nil, nil, Source{Name: "registry:agt_1", Data: jsonAgent(t, ownAgent("agt_1"), nil)})
	if len(agents) != 1 || len(rejected) != 0 || agents[0].Answer.MaxAttempts != 3 {
		t.Fatalf("%+v %+v", agents, rejected)
	}
}

func TestRejectionDetail(t *testing.T) {
	r := Rejection{Err: errors.Join(
		&Problem{File: "registry:agt_1", Line: 1, Agent: "agt_1", Path: "agent.colour", Msg: "unknown field"},
		&Problem{File: "registry:agt_1", Agent: "agt_1", Path: "agent.model.model", Msg: "required"},
	)}
	if got, want := r.Detail(), "agent.colour: unknown field; agent.model.model: required"; got != want {
		t.Errorf("Detail = %q, want %q", got, want)
	}
	if got := (Rejection{Err: errors.New("plain")}).Detail(); got != "plain" {
		t.Errorf("Detail of a plain error = %q", got)
	}
}
