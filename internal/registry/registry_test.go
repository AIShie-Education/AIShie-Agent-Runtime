package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

const (
	core    = "https://lms.example.edu"
	course1 = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b"
	course2 = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1c"
)

// row is a hosted agent as the API would store it: its token and its
// owner's key sealed, and settings as JSON.
func row(id string, settings string) store.HostedAgent {
	return store.HostedAgent{
		ID: id, CoreActorID: "actor-" + id, OwnerActorID: "owner-" + id, OwnerVerified: true, TenantID: "ten_owner",
		DisplayName: "Agent " + id, TokenSecretID: "sec_t_" + id, KeySecretID: "sec_k_" + id,
		Settings: json.RawMessage(settings),
	}
}

const ownModel = `{"model": {"adapter": "openai_chat", "model": "gpt-4.1-mini", "key_source": "own"}}`

// doc is the agent document Document makes, read back.
func doc(t *testing.T, a store.HostedAgent, courses []store.HostedCourse, defaultKS string) map[string]any {
	t.Helper()
	src, err := Document(a, courses, core, defaultKS, schoolPlan())
	if err != nil {
		t.Fatalf("Document(%s): %v", a.ID, err)
	}
	if src.Name != "registry:"+a.ID {
		t.Errorf("the source is named %q", src.Name)
	}
	var m map[string]any
	if err := json.Unmarshal(src.Data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func at(m map[string]any, path string) any {
	var v any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[k]
	}
	return v
}

func TestDocument(t *testing.T) {
	a := row("agt_1", `{"model": {"adapter": "anthropic", "model": "claude-test", "key_source": "own",
		"fallback": {"adapter": "openai_chat", "model": "gpt-test"}}, "prompt": {"system_text": "Be kind."}}`)
	a.Paused = true
	courses := []store.HostedCourse{
		{AgentID: "agt_1", CourseID: course1, Settings: json.RawMessage(`{"prompt_append_text": "In English.", "model": {"model": "claude-other"}}`)},
		{AgentID: "agt_1", CourseID: course2, Settings: json.RawMessage(`{"enabled": false}`)},
	}
	m := doc(t, a, courses, config.KeyOwn)
	for path, want := range map[string]any{
		"agent.id": "agt_1", "agent.display_name": "Agent agt_1", "agent.tenant_id": "ten_owner", "agent.paused": true,
		"agent.core.base_url": core, "agent.core.token_ref": "sealed://sec_t_agt_1",
		"agent.model.key_ref":                        "sealed://sec_k_agt_1",
		"agent.model.fallback.key_ref":               "sealed://sec_k_agt_1",
		"agent.prompt.system_text":                   "Be kind.",
		"courses." + course1 + ".prompt_append_text": "In English.",
		"courses." + course1 + ".model.key_ref":      "sealed://sec_k_agt_1",
		"courses." + course2 + ".enabled":            false,
	} {
		if got := at(m, path); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}

	// No key stored: a section on the owner's key is refused.
	a.KeySecretID = ""
	if _, err := Document(a, nil, core, config.KeyOwn, config.School{}); err == nil || !strings.Contains(err.Error(), "no key of the owner's is stored") {
		t.Errorf("with no key stored: %v", err)
	}
	// No model section at all: one is made, to take the key.
	b := row("agt_2", `{}`)
	if got := at(doc(t, b, nil, config.KeyOwn), "agent.model.key_ref"); got != "sealed://sec_k_agt_2" {
		t.Errorf("with no model section, key_ref = %v", got)
	}
	// None given, the settings are {}.
	b.Settings = nil
	if got := at(doc(t, b, nil, config.KeyOwn), "agent.id"); got != "agt_2" {
		t.Errorf("with no settings: %v", got)
	}
}

// What the registry sets, settings may not; a hosted agent refers to no
// file or secret; and a model on the school's key is an offer of the
// school's plan, named and nothing else, for the agent, not a course.
func TestDocumentRefuses(t *testing.T) {
	for _, c := range []struct {
		name     string
		settings string
		courses  []store.HostedCourse
		defKS    string
		want     []string
	}{
		{name: "its id", settings: `{"id": "agt_other"}`, want: []string{"agent.id: set by the registry"}},
		{name: "its Core", settings: `{"core": {"base_url": "https://evil.example.net"}}`, want: []string{"agent.core: set by the registry"}},
		{name: "its name, tenant and pause", settings: `{"display_name": "x", "tenant_id": "ten_x", "paused": false}`,
			want: []string{"agent.display_name: set", "agent.tenant_id: set", "agent.paused: set"}},
		{name: "a key reference", settings: `{"model": {"adapter": "openai_chat", "model": "m", "key_ref": "secret://kek/v1"}}`,
			want: []string{"agent.model.key_ref: a hosted agent refers to no file or secret"}},
		{name: "a prompt file", settings: `{"prompt": {"system_ref": "/etc/passwd"}}`, want: []string{"agent.prompt.system_ref: a hosted agent refers to no file"}},
		{name: "a reference deep in a list", settings: `{"tools": {"allow": [{"token_ref": "env://X"}]}}`, want: []string{"agent.tools.allow[0].token_ref"}},
		{name: "a course's prompt file", settings: ownModel,
			courses: []store.HostedCourse{{AgentID: "agt_1", CourseID: course1, Settings: json.RawMessage(`{"prompt_append_ref": "/etc/shadow"}`)}},
			want:    []string{"courses." + course1 + ".prompt_append_ref: a hosted agent refers to no file"}},
		{name: "the school's key with no offer", settings: `{"model": {"adapter": "anthropic", "model": "m", "key_source": "school"}}`,
			want: []string{"agent.model.adapter: set by the school's offer", "agent.model.model: set by the school's offer",
				"agent.model: on the school's key, and names no offer"}},
		{name: "the school's key by default", settings: `{"model": {"adapter": "anthropic", "model": "m"}}`, defKS: config.KeySchool,
			want: []string{"agent.model: on the school's key, and names no offer"}},
		{name: "an offer the school does not have", settings: `{"model": {"key_source": "school", "offer": "premium"}}`,
			want: []string{`agent.model.offer "premium": the school no longer offers it, and no model of the owner's stands behind it`}},
		{name: "an offer that is no text", settings: `{"model": {"key_source": "school", "offer": 7}}`,
			want: []string{"agent.model: on the school's key, and names no offer"}},
		{name: "an offer with settings of its own", settings: `{"model": {"key_source": "school", "offer": "standard", "base_url": "https://evil.example", "params": {"max_output_tokens": 9000}}}`,
			want: []string{"agent.model.base_url: set by the school's offer", "agent.model.params: set by the school's offer"}},
		{name: "a fallback on the school's key", settings: `{"model": {"key_source": "school", "offer": "standard", "fallback": {"adapter": "anthropic", "model": "n", "key_source": "school"}}}`,
			want: []string{"agent.model.fallback: a fallback is on the owner's key"}},
		{name: "a course on the school's key", settings: ownModel,
			courses: []store.HostedCourse{{AgentID: "agt_1", CourseID: course1, Settings: json.RawMessage(`{"model": {"key_source": "school", "offer": "standard"}}`)}},
			want:    []string{"courses." + course1 + ".model: a course's model is not on the school's plan"}},
		{name: "a course of an agent on the plan", settings: `{"model": {"key_source": "school", "offer": "standard"}}`,
			courses: []store.HostedCourse{{AgentID: "agt_1", CourseID: course1, Settings: json.RawMessage(`{"model": {"reasoning": {"effort": "high"}}}`)}},
			want:    []string{"courses." + course1 + ".model: a course's model is not on the school's plan"}},
		{name: "settings that are no object", settings: `[1]`, want: []string{"its settings: not a JSON object"}},
		{name: "a model that is no mapping", settings: `{"model": "gpt-4.1"}`, want: []string{"agent.model: must be a mapping"}},
		{name: "a course's that are no object", settings: ownModel,
			courses: []store.HostedCourse{{AgentID: "agt_1", CourseID: course1, Settings: json.RawMessage(`"x"`)}},
			want:    []string{"courses." + course1 + ": not a JSON object"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			defKS := c.defKS
			if defKS == "" {
				defKS = config.KeyOwn
			}
			_, err := Document(row("agt_1", c.settings), c.courses, core, defKS, schoolPlan())
			if err == nil {
				t.Fatal("made")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%v\nwant it to say %q", err, w)
				}
			}
		})
	}
}

// yamlConfig loads a YAML configuration of one agent, a1, from a file.
func yamlConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	body := `
runtime:
  defaults:
    polling: {inbox_idle_s: 20, inbox_max_s: 40}
---
agent:
  id: a1
  display_name: A1
  core: {base_url: "https://lms.example.edu", token_ref: "env://A1_TOKEN"}
  model: {adapter: openai_chat, model: gpt-4.1-mini, key_ref: "env://OPENAI_API_KEY"}
` + extra
	if err := os.WriteFile(filepath.Join(dir, "agents.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// hostedStore is a memstore holding rows, each with its secrets, and
// courses.
func hostedStore(t *testing.T, rows []store.HostedAgent, courses ...store.HostedCourse) *memstore.Store {
	t.Helper()
	st := memstore.New()
	for _, a := range rows {
		secrets := []store.Secret{fakeSecret(a.TokenSecretID, a.TenantID, store.SecretCoreToken)}
		if a.KeySecretID != "" {
			secrets = append(secrets, fakeSecret(a.KeySecretID, a.TenantID, store.SecretModelKey))
		}
		if _, err := st.CreateHostedAgent(t.Context(), a, secrets...); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range courses {
		if err := st.PutHostedCourse(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// fakeSecret is a secret row for a store: its bytes are not a vault's, and
// nothing here opens it.
func fakeSecret(id, tenant, kind string) store.Secret {
	return store.Secret{ID: id, TenantID: tenant, Kind: kind, KEKID: "local:v1", WrappedDEK: []byte{1}, Nonce: []byte{2}, Ciphertext: []byte{3}}
}

func agentIDs(cfg *config.Config) []string {
	var out []string
	for _, a := range cfg.Agents {
		out = append(out, a.ID)
	}
	return out
}

func rejectedWhy(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for _, r := range cfg.Rejected {
		out[r.AgentID] = r.Detail()
	}
	return out
}

// Build is YAML ∪ registry: every hosted agent that passes, over the
// runtime's defaults, after the YAML agents; each that does not, with why,
// keeping none of the others from running; and a hosted agent whose id is a
// YAML agent's loses to it.
func TestBuild(t *testing.T) {
	yaml := yamlConfig(t, `---
agent:
  id: agt_taken
  display_name: A YAML agent with a registry-shaped id
  core: {base_url: "https://lms.example.edu", token_ref: "env://X"}
  model: {adapter: openai_chat, model: gpt-4.1-mini, key_ref: "env://Y"}
`)
	rows := []store.HostedAgent{
		row("agt_ok", ownModel),
		row("agt_paused", ownModel),
		row("agt_taken", ownModel),
		row("agt_unknown", `{"model": {"adapter": "openai_chat", "model": "m", "key_source": "own"}, "colour": "blue"}`),
		row("agt_ref", `{"model": {"adapter": "openai_chat", "model": "m", "key_source": "own"}, "prompt": {"system_ref": "/etc/passwd"}}`),
		row("agt_nokey", ownModel),
		row("agt_bedrock", `{"model": {"adapter": "bedrock_converse", "model": "anthropic.claude", "region": "us-east-1", "key_source": "own"}}`),
		row("agt_local", `{"model": {"adapter": "openai_chat", "model": "llama", "base_url": "https://10.0.0.5/v1", "key_source": "own"}}`),
		row("agt_http", `{"model": {"adapter": "openai_chat", "model": "deepseek-chat", "base_url": "http://api.deepseek.com", "key_source": "own"}}`),
		row("agt_elsewhere", `{"model": {"adapter": "anthropic", "model": "claude", "base_url": "https://anthropic.evil.example/v1", "key_source": "own"}}`),
		row("agt_headers", `{"model": {"adapter": "openai_chat", "model": "gpt-4.1", "key_source": "own", "headers": {"X-Title": "mine"}}}`),
		row("agt_official", `{"model": {"adapter": "openai_chat", "model": "deepseek-chat", "base_url": "https://api.deepseek.com", "key_source": "own"}}`),
		row("agt_course", ownModel),
	}
	rows[1].Paused = true
	rows[5].KeySecretID = "" // agt_nokey
	rows[6].KeySecretID = "" // agt_bedrock: Bedrock would sign with the host's credentials
	st := hostedStore(t, rows,
		store.HostedCourse{AgentID: "agt_ok", CourseID: course1, Settings: json.RawMessage(`{"prompt_append_text": "In English."}`)},
		store.HostedCourse{AgentID: "agt_course", CourseID: course1, Settings: json.RawMessage(`{"model": {"base_url": "https://ollama.internal:11434/v1"}}`)},
	)
	before := len(yaml.Agents)
	cfg, rev, err := Build(t.Context(), yaml, st, Options{CoreBaseURL: core, Allowlist: []string{core}})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := st.RegistryRev(t.Context()); rev != want {
		t.Errorf("rev = %d, want %d", rev, want)
	}
	if got, want := agentIDs(cfg), []string{"a1", "agt_taken", "agt_official", "agt_ok", "agt_paused"}; !slices.Equal(got, want) {
		t.Fatalf("agents %v, want %v; rejected %v", got, want, rejectedWhy(cfg))
	}
	if len(yaml.Agents) != before {
		t.Error("Build changed the YAML configuration")
	}
	ok := cfg.Agents[3]
	if ok.Hosted == nil || ok.Hosted.CoreActorID != "actor-agt_ok" || ok.Hosted.OwnerActorID != "owner-agt_ok" || !ok.Hosted.OwnerVerified ||
		ok.Core.BaseURL != core || ok.Core.TokenRef != "sealed://sec_t_agt_ok" || ok.Model.KeyRef != "sealed://sec_k_agt_ok" ||
		ok.Polling.InboxIdleS != 20 || ok.File != "registry:agt_ok" || ok.TenantID != "ten_owner" {
		t.Errorf("the hosted agent: %+v", ok)
	}
	if cfg.Agents[0].Hosted != nil || cfg.Agents[1].Hosted != nil {
		t.Error("a YAML agent is marked hosted")
	}
	if e, err := ok.ForCourse(course1); err != nil || e.PromptAppendText != "In English." {
		t.Errorf("its course: %+v, %v", e, err)
	}
	if !cfg.Agents[4].Paused {
		t.Error("the paused agent is not paused")
	}

	why := rejectedWhy(cfg)
	for id, want := range map[string]string{
		"agt_taken":     "a YAML agent has this id",
		"agt_unknown":   "agent.colour: unknown field",
		"agt_ref":       "agent.prompt.system_ref: a hosted agent refers to no file or secret",
		"agt_nokey":     "agent.model: on the owner's key, and no key of the owner's is stored",
		"agt_bedrock":   "agent.model: on the owner's key, and no key of the owner's is stored",
		"agt_local":     "agent.model: base_url 10.0.0.5 is not an official provider's endpoint",
		"agt_http":      "agent.model: base_url must be https",
		"agt_elsewhere": "base_url anthropic.evil.example is not an official provider's endpoint for anthropic",
		"agt_headers":   "sends extra headers",
		"agt_course":    "courses." + course1 + ".model: base_url ollama.internal:11434 is not an official",
	} {
		if !strings.Contains(why[id], want) {
			t.Errorf("%s: rejected %q, want it to say %q", id, why[id], want)
		}
		delete(why, id)
	}
	if len(why) > 0 {
		t.Errorf("rejected too: %v", why)
	}
}

// TestBuildTakesDeprecatedSettings: a hosted agent whose settings were
// written when the runtime closed a conversation whose attempts were spent
// still runs: on_attempts_exhausted close is taken as skip, in the agent's
// settings and a course's, and close_reason_text is taken and unused; each
// is listed as deprecated, under the registry's name for the agent.
func TestBuildTakesDeprecatedSettings(t *testing.T) {
	st := hostedStore(t, []store.HostedAgent{row("agt_old", `{"model": {"adapter": "openai_chat", "model": "gpt-4.1-mini", "key_source": "own"},
		"answer": {"on_attempts_exhausted": "close"}, "prompt": {"close_reason_text": "Closed after three tries."}}`)},
		store.HostedCourse{AgentID: "agt_old", CourseID: course1, Settings: json.RawMessage(`{"answer": {"on_attempts_exhausted": "close"}}`)})
	cfg, _, err := Build(t.Context(), yamlConfig(t, ""), st, Options{CoreBaseURL: core, Allowlist: []string{core}})
	if err != nil {
		t.Fatal(err)
	}
	if got := agentIDs(cfg); !slices.Equal(got, []string{"a1", "agt_old"}) {
		t.Fatalf("agents %v; rejected %v", got, rejectedWhy(cfg))
	}
	old := cfg.Agents[1]
	e, err := old.ForCourse(course1)
	if err != nil || old.Answer.OnAttemptsExhausted != config.OnExhaustedSkip || e.Answer.OnAttemptsExhausted != config.OnExhaustedSkip {
		t.Errorf("on_attempts_exhausted %q, in its course %+v, %v", old.Answer.OnAttemptsExhausted, e.Answer, err)
	}
	var paths []string
	for _, p := range old.Deprecated() {
		if p.File != "registry:agt_old" {
			t.Errorf("%v", p)
		}
		paths = append(paths, p.Path)
	}
	if want := []string{"agent.answer.on_attempts_exhausted", "agent.prompt.close_reason_text", "courses." + course1 + ".answer.on_attempts_exhausted"}; !slices.Equal(paths, want) {
		t.Errorf("deprecated %v, want %v", paths, want)
	}
}

// TestBuildWrites: a hosted agent's owner is known, so its model is
// offered its writes unless its owner turned them off, whatever the
// runtime's defaults say; a YAML agent has none unless its configuration
// turns them on; a course may turn a hosted agent's off.
func TestBuildWrites(t *testing.T) {
	yaml := yamlConfig(t, "")
	yaml.Runtime.Defaults["tools"] = map[string]any{"writes": false}
	withTools := func(tools string) string {
		return `{"model": {"adapter": "openai_chat", "model": "gpt-4.1-mini", "key_source": "own"}, "tools": ` + tools + `}`
	}
	rows := []store.HostedAgent{
		row("agt_default", ownModel),
		row("agt_null", withTools(`null`)),
		row("agt_off", withTools(`{"writes": false}`)),
		row("agt_on", withTools(`{"writes": true}`)),
		row("agt_deny", withTools(`{"deny": ["grade_*"], "writes": null}`)),
		row("agt_course", ownModel),
		row("agt_bad", withTools(`{"writes": [true]}`)),
	}
	st := hostedStore(t, rows, store.HostedCourse{AgentID: "agt_course", CourseID: course1, Settings: json.RawMessage(`{"tools": {"writes": false}}`)})
	cfg, _, err := Build(t.Context(), yaml, st, Options{CoreBaseURL: core, Allowlist: []string{core}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, a := range cfg.Agents {
		got[a.ID] = a.Tools.Writes
	}
	want := map[string]bool{"a1": false, "agt_default": true, "agt_null": true, "agt_off": false, "agt_on": true, "agt_deny": true, "agt_course": true}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("writes %v\nwant   %v; rejected %v", got, want, rejectedWhy(cfg))
	}
	if why := rejectedWhy(cfg)["agt_bad"]; !strings.Contains(why, "agent.tools.writes") {
		t.Errorf("tools.writes [true] was not refused: %q", why)
	}
	for _, a := range cfg.Agents {
		if a.ID != "agt_course" {
			continue
		}
		if e, err := a.ForCourse(course1); err != nil || e.Tools.Writes {
			t.Errorf("the course that turns writes off: %+v, %v", e.Tools, err)
		}
		if e, err := a.ForCourse(course2); err != nil || !e.Tools.Writes {
			t.Errorf("another course: %+v, %v", e.Tools, err)
		}
	}
	for settings, want := range map[string]bool{
		ownModel: true, withTools(`{"writes": false}`): false, withTools(`{"writes": true}`): true, withTools(`null`): true, ``: true,
	} {
		if got := WritesOf(json.RawMessage(settings)); got != want {
			t.Errorf("WritesOf(%s) = %v, want %v", settings, got, want)
		}
	}
}

// A hosted agent takes none of the operator's keys from the runtime's
// defaults: not a fallback's, which it does not inherit at all, nor a
// model's, which its own key, or none, replaces.
func TestBuildTakesNoKeyFromTheDefaults(t *testing.T) {
	yaml := yamlConfig(t, "")
	yaml.Runtime.Defaults = map[string]any{"model": map[string]any{
		"key_ref":  "secret://school/keys/openai",
		"fallback": map[string]any{"adapter": "openai_chat", "model": "gpt-4.1", "key_source": "own", "key_ref": "secret://school/keys/openai"},
	}}
	st := hostedStore(t, []store.HostedAgent{row("agt_1", ownModel)})
	cfg, _, err := Build(t.Context(), yaml, st, Options{CoreBaseURL: core})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("agents %v, rejected %v", agentIDs(cfg), rejectedWhy(cfg))
	}
	a := cfg.Agents[1]
	if a.Model.KeyRef != "sealed://sec_k_agt_1" || a.Model.Fallback != nil {
		t.Errorf("the hosted agent's model: key %q, fallback %+v", a.Model.KeyRef, a.Model.Fallback)
	}
	if err := checkModels(a, "sealed://sec_other", config.School{}); err == nil || !strings.Contains(err.Error(), "has a key that is not the owner's") {
		t.Errorf("a model with another key: %v", err)
	}
}

// A section that names no key source is keyed as its parent's, and runs
// so: merged over the runtime's defaults, a fallback that does not say would
// take the key source of the defaults' fallback, the school's, and run on
// the owner's key with its spend counted as the school's, held to the
// school's quotas and reported as the school's cost.
func TestBuildTakesNoKeySourceFromTheDefaults(t *testing.T) {
	yaml := yamlConfig(t, "")
	answers := 100
	yaml.Runtime.Tenants = map[string]config.Tenant{"ten_owner": {PerDay: config.Quota{Answers: &answers}}}
	yaml.Runtime.Defaults = map[string]any{"model": map[string]any{
		"fallback": map[string]any{"adapter": "openai_chat", "model": "gpt-4.1", "key_source": "school", "key_ref": "secret://school/keys/openai"},
	}}
	const budgets = `"budgets": {"per_agent_day": {"answers": 10}, "per_asker_day": {"answers": 5}}`
	st := hostedStore(t, []store.HostedAgent{
		row("agt_empty", `{"model": {"adapter": "openai_chat", "model": "gpt-4.1-mini", "key_source": "own", "fallback": {}}, `+budgets+`}`),
		row("agt_named", `{"model": {"adapter": "openai_chat", "model": "gpt-4.1-mini", "key_source": "own",
		  "fallback": {"adapter": "anthropic", "model": "claude-test"}}, `+budgets+`}`),
	})
	cfg, _, err := Build(t.Context(), yaml, st, Options{CoreBaseURL: core})
	if err != nil {
		t.Fatal(err)
	}
	if got := agentIDs(cfg); !slices.Equal(got, []string{"a1", "agt_empty", "agt_named"}) {
		t.Fatalf("agents %v, rejected %v", got, rejectedWhy(cfg))
	}
	for _, a := range cfg.Agents[1:] {
		fb := a.Model.Fallback
		if fb == nil || fb.KeySource != config.KeyOwn || fb.KeyRef != "sealed://sec_k_"+a.ID {
			t.Errorf("%s's fallback: %+v", a.ID, fb)
		}
	}

	// And a model that is on the school's key as merged, however it came
	// to be, is not run.
	a := *cfg.Agents[1]
	fb := *a.Model.Fallback
	fb.KeySource = config.KeySchool
	a.Model.Fallback = &fb
	if err := checkModels(&a, "sealed://sec_k_agt_empty", config.School{}); err == nil || !strings.Contains(err.Error(), "agent.model.fallback: is on the school's key") {
		t.Errorf("a fallback on the school's key: %v", err)
	}
}

// Without CORE_BASE_URL, or with one outside the allowlist, no hosted
// agent runs, and each says why; the YAML agents run on.
func TestBuildWithoutACore(t *testing.T) {
	st := hostedStore(t, []store.HostedAgent{row("agt_1", ownModel)})
	for _, c := range []struct {
		o    Options
		want string
	}{
		{Options{}, "CORE_BASE_URL is not set"},
		{Options{CoreBaseURL: core, Allowlist: []string{"https://other.example.edu"}}, "not within CORE_BASE_URL_ALLOWLIST"},
	} {
		cfg, _, err := Build(t.Context(), yamlConfig(t, ""), st, c.o)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(agentIDs(cfg), []string{"a1"}) || !strings.Contains(rejectedWhy(cfg)["agt_1"], c.want) {
			t.Errorf("%+v: agents %v, rejected %v", c.o, agentIDs(cfg), rejectedWhy(cfg))
		}
	}
}

// A store that cannot be read is an error, not a configuration without
// hosted agents: the one in force stays.
func TestBuildOfAStoreThatFails(t *testing.T) {
	for _, f := range []failing{{rev: true}, {agents: true}, {courses: true}, {settings: true}, {offers: true}, {prices: true}, {tenants: true}} {
		if _, _, err := Build(context.Background(), yamlConfig(t, ""), f, Options{CoreBaseURL: core}); err == nil {
			t.Errorf("%+v: no error", f)
		}
	}
}

type failing struct{ rev, agents, courses, settings, offers, prices, tenants bool }

var errDown = errors.New("the database is down")

func (f failing) RegistryRev(context.Context) (int64, error) {
	if f.rev {
		return 0, errDown
	}
	return 1, nil
}

func (f failing) HostedAgents(context.Context) ([]store.HostedAgent, error) {
	if f.agents {
		return nil, errDown
	}
	return nil, nil
}

func (f failing) ListHostedCourses(context.Context) ([]store.HostedCourse, error) {
	if f.courses {
		return nil, errDown
	}
	return nil, nil
}

func (f failing) SiteSettings(context.Context) ([]store.SiteSetting, error) {
	if f.settings {
		return nil, errDown
	}
	return nil, nil
}

func (f failing) SchoolOffers(context.Context) ([]store.SchoolOffer, error) {
	if f.offers {
		return nil, errDown
	}
	return nil, nil
}

func (f failing) SitePrices(context.Context) ([]store.SitePrice, time.Time, error) {
	if f.prices {
		return nil, time.Time{}, errDown
	}
	return nil, time.Time{}, nil
}

func (f failing) TenantQuotas(context.Context) ([]store.TenantQuota, error) {
	if f.tenants {
		return nil, errDown
	}
	return nil, nil
}

// The official endpoints, adapter by adapter (Core's
// docs/agent-runtime.md §3.9).
func TestOfficialEndpoints(t *testing.T) {
	for _, c := range []struct {
		adapter, url string
		ok           bool
	}{
		{"openai_chat", "https://api.openai.com/v1", true},
		{"openai_chat", "https://api.deepseek.com", true},
		{"openai_chat", "https://myres.openai.azure.com/openai/v1", true},
		{"openai_chat", "https://openrouter.ai/api/v1", true},
		{"openai_chat", "https://dashscope.aliyuncs.com/compatible-mode/v1", true},
		{"openai_chat", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", true},
		{"openai_chat", "https://dashscope-us.aliyuncs.com/compatible-mode/v1", true},
		{"openai_chat", "https://ws-1a2b3c.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", true},
		// Alibaba Cloud's names that anyone can take: a bucket, a function.
		{"openai_chat", "https://mybucket.oss-cn-hangzhou.aliyuncs.com/v1", false},
		{"openai_chat", "https://1234567890.cn-hangzhou.fc.aliyuncs.com/2016-08-15/proxy/svc/fn", false},
		{"openai_chat", "https://dashscope.aliyuncs.com.evil.example/v1", false},
		{"openai_chat", "https://api.anthropic.com/v1", true},
		{"openai_chat", "https://llm.internal/v1", false},
		{"openai_chat", "https://127.0.0.1/v1", false},
		{"openai_chat", "https://api.openai.com.evil.example/v1", false},
		{"openai_chat", "https://host:11434/v1", false},
		{"openai_responses", "https://api.openai.com/v1", true},
		{"openai_responses", "https://myres.openai.azure.com/openai/v1", true},
		{"openai_responses", "https://api.deepseek.com", false},
		{"anthropic", "https://api.anthropic.com", true},
		{"anthropic", "https://api.deepseek.com/anthropic", false},
		{"gemini", "https://generativelanguage.googleapis.com/v1beta", true},
		{"gemini", "https://gemini.example.net", false},
		{"bedrock_converse", "https://bedrock-runtime.us-east-1.amazonaws.com", true},
		{"bedrock_converse", "https://bedrock-runtime-fips.us-east-1.amazonaws.com", true},
		{"bedrock_converse", "https://bedrock-runtime.cn-north-1.amazonaws.com.cn", true},
		{"bedrock_converse", "https://s3.amazonaws.com", false},
		// AWS's names that anyone can take with "bedrock" in them: a
		// bucket, a load balancer.
		{"bedrock_converse", "https://bedrock-mine.s3.amazonaws.com", false},
		{"bedrock_converse", "https://bedrock-lb-1234567890.us-east-1.elb.amazonaws.com", false},
		{"cohere", "https://api.cohere.com", false},
	} {
		m := config.Model{Adapter: c.adapter, BaseURL: c.url, KeyRef: "sealed://sec_k"}
		if got := checkModel(m) == ""; got != c.ok {
			t.Errorf("%s at %s: allowed %v, want %v (%s)", c.adapter, c.url, got, c.ok, checkModel(m))
		}
	}
}

// schoolPlan is a school's plan of one offer, on the school's DeepSeek key.
func schoolPlan() config.School {
	return config.School{Offers: []config.SchoolOffer{{
		ID: "standard", Label: "School AI", Adapter: "openai_chat", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com",
		KeyRef: "secret://school/keys/deepseek", Params: config.ModelParams{MaxOutputTokens: 1500},
	}}}
}

// A hosted agent on the school's plan names the offer and nothing else: the
// document takes the offer's settings, its key's reference among them, and
// the owner's own model behind it on their key. The row keeps the offer's
// id alone, never the key's reference.
func TestDocumentOnTheSchoolPlan(t *testing.T) {
	a := row("agt_1", `{"model": {"key_source": "school", "offer": "standard",
		"fallback": {"adapter": "anthropic", "model": "claude-test", "key_source": "own"}}}`)
	m := doc(t, a, nil, config.KeyOwn)
	for path, want := range map[string]any{
		"agent.model.adapter": "openai_chat", "agent.model.model": "deepseek-chat", "agent.model.base_url": "https://api.deepseek.com",
		"agent.model.key_source": "school", "agent.model.key_ref": "secret://school/keys/deepseek", "agent.model.offer": "standard",
		"agent.model.params.max_output_tokens": float64(1500),
		"agent.model.fallback.key_ref":         "sealed://sec_k_agt_1", "agent.model.fallback.key_source": "own",
		"agent.model.fallback.model": "claude-test",
	} {
		if got := at(m, path); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if strings.Contains(string(a.Settings), "secret://") {
		t.Error("the row holds a key's reference")
	}
	// With no key of the owner's and no fallback: the plan alone.
	b := row("agt_2", `{"model": {"key_source": "school", "offer": "standard"}}`)
	b.KeySecretID = ""
	m = doc(t, b, nil, config.KeyOwn)
	if at(m, "agent.model.key_ref") != "secret://school/keys/deepseek" || at(m, "agent.model.fallback") != nil {
		t.Errorf("the plan alone: %v", m["agent"])
	}
	// A fallback that names no key source is the owner's.
	c := row("agt_3", `{"model": {"key_source": "school", "offer": "standard", "fallback": {"adapter": "anthropic", "model": "claude-test"}}}`)
	if got := at(doc(t, c, nil, config.KeyOwn), "agent.model.fallback.key_source"); got != config.KeyOwn {
		t.Errorf("the fallback's key source: %v", got)
	}
}

// Build runs a hosted agent on an offer of the school's plan, with the
// offer's key, where the offer says, whatever the runtime's defaults say
// of a model; and not one on an offer the school has withdrawn, with no
// model of its owner's behind it, which says so; nor one whose merged
// model is on the school's key otherwise than by its offer.
func TestBuildOnTheSchoolPlan(t *testing.T) {
	yaml := yamlConfig(t, "")
	yaml.Runtime.School = schoolPlan()
	yaml.Runtime.Defaults["model"] = map[string]any{"provider": "openrouter", "region": "us-east-1", "base_url": "https://openrouter.ai/api/v1"}
	st := hostedStore(t, []store.HostedAgent{
		row("agt_plan", `{"model": {"key_source": "school", "offer": "standard", "fallback": {"adapter": "anthropic", "model": "claude-test", "key_source": "own"}}}`),
		row("agt_gone", `{"model": {"key_source": "school", "offer": "premium"}}`),
		row("agt_alone", `{"model": {"key_source": "school", "offer": "standard"}}`),
	})
	cfg, _, err := Build(t.Context(), yaml, st, Options{CoreBaseURL: core})
	if err != nil {
		t.Fatal(err)
	}
	if got := agentIDs(cfg); !slices.Equal(got, []string{"a1", "agt_alone", "agt_plan"}) {
		t.Fatalf("agents %v, rejected %v", got, rejectedWhy(cfg))
	}
	if why := rejectedWhy(cfg)["agt_gone"]; !strings.Contains(why, `agent.model.offer "premium": the school no longer offers it`) {
		t.Errorf("an offer withdrawn: %q", why)
	}
	if r := cfg.Rejected[0]; r.AgentID != "agt_gone" || r.Reason != store.ReasonOfferWithdrawn || !errors.Is(r.Err, ErrOfferWithdrawn) {
		t.Errorf("an offer withdrawn is rejected as %+v", r)
	}
	a := cfg.Agents[2]
	if a.Model.KeySource != config.KeySchool || a.Model.Offer != "standard" || a.Model.KeyRef != "secret://school/keys/deepseek" ||
		a.Model.Model != "deepseek-chat" || a.Model.Params.MaxOutputTokens != 1500 || a.TenantID != "ten_owner" ||
		a.Model.BaseURL != "https://api.deepseek.com" || a.Model.Provider != "" || a.Model.Region != "" {
		t.Errorf("the model on the plan: %+v", a.Model)
	}
	if fb := a.Model.Fallback; fb == nil || fb.KeySource != config.KeyOwn || fb.KeyRef != "sealed://sec_k_agt_plan" || fb.Offer != "" {
		t.Errorf("the owner's fallback: %+v", fb)
	}
	// The same row with no plan in the runtime's settings is not run.
	if err := Check(t.Context(), yamlConfig(t, ""), row("agt_plan", `{"model": {"key_source": "school", "offer": "standard"}}`), nil,
		Options{CoreBaseURL: core}); !errors.Is(err, ErrOfferWithdrawn) {
		t.Errorf("with no plan: %v", err)
	}
	// A model on the school's key with another key, or no offer.
	b := *a
	bm := b.Model
	bm.KeyRef = "secret://school/keys/other"
	b.Model = bm
	if err := checkModels(&b, "sealed://sec_k_agt_plan", yaml.Runtime.School); err == nil || !strings.Contains(err.Error(), "has a key that is not its offer's") {
		t.Errorf("another key: %v", err)
	}
	bm.KeyRef, bm.Offer = "secret://school/keys/deepseek", ""
	b.Model = bm
	if err := checkModels(&b, "sealed://sec_k_agt_plan", yaml.Runtime.School); err == nil || !strings.Contains(err.Error(), "not on the agent's offer") {
		t.Errorf("no offer: %v", err)
	}
}

// siteOffer is an offer of the school's plan as the API makes one, on the
// sealed key sec_school_<id>.
func siteOffer(t *testing.T, st *memstore.Store, id string, enabled bool, baseURL string) {
	t.Helper()
	o := store.SchoolOffer{ID: id, Label: "Site " + id, Adapter: "openai_chat", Provider: "deepseek", Model: "deepseek-chat", BaseURL: baseURL,
		MaxOutputTokens: 900, Enabled: enabled, KeySecretID: "sec_school_" + id, KeyHint: "sk-…aaaa", CreatedBy: "admin"}
	if _, err := st.CreateSchoolOffer(t.Context(), o, fakeSecret(o.KeySecretID, store.SchoolTenantID, store.SecretModelKey)); err != nil {
		t.Fatal(err)
	}
}

func putSetting(t *testing.T, st *memstore.Store, name, value string) {
	t.Helper()
	if err := st.PutSiteSetting(t.Context(), store.SiteSetting{Name: name, Value: json.RawMessage(value), UpdatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
}

// Build puts the site's settings in force over runtime.yaml's: its offers
// that are turned on join the plan, on their sealed keys, but one whose
// id runtime.yaml's has, which wins; an agent on one runs with its key; an
// agent on one turned off runs on its owner's model behind it, or, with
// none, is not run, saying the offer was withdrawn, until it is turned on
// again. An offer the site made is held to a provider's own endpoint, as
// an owner's model is. The site's quotas stand in place of runtime.yaml's,
// and its OCR setting is carried in the runtime's settings.
func TestBuildWithTheSitesPlan(t *testing.T) {
	yaml := yamlConfig(t, "")
	yaml.Runtime.School = schoolPlan()
	st := hostedStore(t, []store.HostedAgent{
		row("agt_fast", `{"model": {"key_source": "school", "offer": "fast"}}`),
		row("agt_off_own", `{"model": {"key_source": "school", "offer": "off", "fallback": {"adapter": "anthropic", "model": "claude-test", "key_source": "own"}}}`),
		row("agt_off_alone", `{"model": {"key_source": "school", "offer": "off"}}`),
		row("agt_std", `{"model": {"key_source": "school", "offer": "standard"}}`),
		row("agt_odd", `{"model": {"key_source": "school", "offer": "odd"}}`),
	})
	siteOffer(t, st, "fast", true, "https://api.deepseek.com")
	siteOffer(t, st, "off", false, "https://api.deepseek.com")
	siteOffer(t, st, "standard", true, "https://api.deepseek.com")
	siteOffer(t, st, "odd", true, "https://llm.school.example/v1")
	putSetting(t, st, store.SettingSchoolQuotas, `{"per_owner_day": 7, "per_asker_day": 3, "per_day": 40}`)
	putSetting(t, st, store.SettingOCR, `{"enabled": false, "languages": ["eng"]}`)

	cfg, _, err := Build(t.Context(), yaml, st, Options{CoreBaseURL: core})
	if err != nil {
		t.Fatal(err)
	}
	if got := agentIDs(cfg); !slices.Equal(got, []string{"a1", "agt_fast", "agt_off_own", "agt_std"}) {
		t.Fatalf("agents %v, rejected %v", got, rejectedWhy(cfg))
	}
	var offers []string
	for _, o := range cfg.Runtime.School.Offers {
		offers = append(offers, o.ID)
	}
	if !slices.Equal(offers, []string{"standard", "fast", "odd"}) || cfg.Runtime.School.Offers[0].Site {
		t.Errorf("the plan's offers: %v", offers)
	}
	sc := cfg.Runtime.School
	if *sc.OwnerQuota().Answers != 7 || *sc.AskerQuota().Answers != 3 || sc.PerDay.Answers == nil || *sc.PerDay.Answers != 40 {
		t.Errorf("the plan's quotas: %+v", sc)
	}
	if o := cfg.Runtime.Site.OCR; o.Enabled == nil || *o.Enabled || !slices.Equal(o.Languages, []string{"eng"}) {
		t.Errorf("the site's OCR: %+v", o)
	}
	if len(yaml.Runtime.School.Offers) != 1 || yaml.Runtime.School.PerOwnerDay.Answers != nil {
		t.Errorf("runtime.yaml's settings were changed: %+v", yaml.Runtime.School)
	}
	byID := map[string]*config.Agent{}
	for _, a := range cfg.Agents {
		byID[a.ID] = a
	}
	if m := byID["agt_fast"].Model; m.KeySource != config.KeySchool || m.Offer != "fast" || m.KeyRef != "sealed://sec_school_fast" ||
		m.BaseURL != "https://api.deepseek.com" || m.Params.MaxOutputTokens != 900 {
		t.Errorf("on the site's offer: %+v", m)
	}
	if m := byID["agt_off_own"].Model; m.KeySource != config.KeyOwn || m.Offer != "" || m.KeyRef != "sealed://sec_k_agt_off_own" ||
		m.Model != "claude-test" || m.Fallback != nil {
		t.Errorf("on an offer turned off, with the owner's model behind it: %+v", m)
	}
	if m := byID["agt_std"].Model; m.KeyRef != "secret://school/keys/deepseek" {
		t.Errorf("on runtime.yaml's offer, whose id the site's has too: %+v", m)
	}
	reasons := map[string]string{}
	for _, r := range cfg.Rejected {
		reasons[r.AgentID] = r.Reason
	}
	why := rejectedWhy(cfg)
	if reasons["agt_off_alone"] != store.ReasonOfferWithdrawn || !strings.Contains(why["agt_off_alone"], `"off": the school no longer offers it`) {
		t.Errorf("on an offer turned off, alone: %s %q", reasons["agt_off_alone"], why["agt_off_alone"])
	}
	if reasons["agt_odd"] != store.ReasonSettingsRejected || !strings.Contains(why["agt_odd"], "is not an official provider's endpoint") {
		t.Errorf("on the site's offer at an endpoint of no provider's: %s %q", reasons["agt_odd"], why["agt_odd"])
	}

	// The offer turned on again: both agents on it are on it again.
	off, err := st.SchoolOffer(t.Context(), "off")
	if err != nil {
		t.Fatal(err)
	}
	off.Enabled = true
	if _, err := st.UpdateSchoolOffer(t.Context(), *off); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = Build(t.Context(), yaml, st, Options{CoreBaseURL: core})
	if err != nil {
		t.Fatal(err)
	}
	byID = map[string]*config.Agent{}
	for _, a := range cfg.Agents {
		byID[a.ID] = a
	}
	for _, id := range []string{"agt_off_own", "agt_off_alone"} {
		if a := byID[id]; a == nil || a.Model.Offer != "off" || a.Model.KeyRef != "sealed://sec_school_off" {
			t.Errorf("%s on the offer turned on again: %+v", id, a)
		}
	}
	if fb := byID["agt_off_own"].Model.Fallback; fb == nil || fb.KeyRef != "sealed://sec_k_agt_off_own" {
		t.Errorf("the owner's model behind it again: %+v", fb)
	}
}

// A setting written by hand that does not decode as the API writes it
// counts as not set.
func TestReadSiteIgnoresASettingItCannotRead(t *testing.T) {
	st := memstore.New()
	putSetting(t, st, store.SettingSchoolQuotas, `{"per_owner_day": 7, "per_asker_day": 3, "burst": 1}`)
	putSetting(t, st, store.SettingOCR, `{"enabled": "yes"}`)
	putSetting(t, st, "other", `{"x": 1}`)
	site, err := ReadSite(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	if site.Quotas != nil || site.OCR.Enabled != nil || len(site.Offers) != 0 {
		t.Errorf("ReadSite = %+v", site)
	}
}

// Build puts the site's money in force: its prices are read with when they
// last changed; a tenant's quota replaces runtime.tenants'; and the
// agents' daily budgets by default hold every hosted agent that sets none,
// in place of runtime.defaults', while one that sets its own keeps them,
// and runtime.yaml's agents keep what they were built with.
func TestBuildWithTheSitesMoney(t *testing.T) {
	yaml := yamlConfig(t, "")
	yaml.Runtime.Defaults["budgets"] = map[string]any{"per_agent_day": map[string]any{"answers": 300}}
	st := hostedStore(t, []store.HostedAgent{
		row("agt_default", ownModel),
		row("agt_own", `{"model": {"adapter": "openai_chat", "model": "gpt-4.1-mini", "key_source": "own"}, "budgets": {"per_agent_day": {"answers": 7}}}`),
	})
	from := time.Date(2025, 4, 14, 0, 0, 0, 0, time.UTC)
	if _, err := st.CreateSitePrice(t.Context(), store.SitePrice{ID: "mini", Provider: "openai", Model: "gpt-4.1-mini", From: from,
		InputPUSD: 400_000, CacheReadPUSD: 100_000, CacheWritePUSD: 400_000, OutputPUSD: 1_600_000, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	ten, cents := 10, int64(500_000_000_000)
	if err := st.PutTenantQuota(t.Context(), store.TenantQuota{TenantID: "ten_owner", Answers: &ten, USDPUSD: &cents, UpdatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	putSetting(t, st, store.SettingAgentBudgets, `{"per_agent_day": {"answers": null, "usd": 1.5}, "per_asker_day": {"answers": 12, "usd": null}}`)
	cfg, _, err := Build(t.Context(), yaml, st, Options{CoreBaseURL: core})
	if err != nil {
		t.Fatal(err)
	}
	site := cfg.Runtime.Site
	if len(site.Prices) != 1 || site.Prices[0].ID != "mini" || site.Prices[0].Out != 1_600_000 || site.PricesChanged.IsZero() {
		t.Errorf("the site's prices: %+v %s", site.Prices, site.PricesChanged)
	}
	if p, ok := site.PriceTable(nil).Lookup("openai", "gpt-4.1-mini", time.Now()); !ok || p.Version != pricing.SiteVersion(site.PricesChanged)+"/mini" {
		t.Errorf("the price table in force: %+v", p)
	}
	if q := cfg.Runtime.Tenants["ten_owner"].PerDay; q.Answers == nil || *q.Answers != 10 || q.USD == nil || *q.USD != 0.5 {
		t.Errorf("the tenant's quota: %+v", q)
	}
	byID := map[string]*config.Agent{}
	for _, a := range cfg.Agents {
		byID[a.ID] = a
	}
	if b := byID["agt_default"].Budgets; b.PerAgentDay.Answers != nil || b.PerAgentDay.USD == nil || *b.PerAgentDay.USD != 1.5 ||
		b.PerAskerDay.Answers == nil || *b.PerAskerDay.Answers != 12 || b.PerAskerDay.USD != nil {
		t.Errorf("the budgets by default: %+v", b)
	}
	if b := byID["agt_own"].Budgets; b.PerAgentDay.Answers == nil || *b.PerAgentDay.Answers != 7 {
		t.Errorf("an agent's own budgets: %+v", b)
	}
	if a := cfg.Agents[0]; a.ID != "a1" || a.Budgets.PerAgentDay.USD != nil || a.Budgets.PerAskerDay.Answers != nil {
		t.Errorf("runtime.yaml's agent, built on runtime.yaml's defaults, took the site's: %+v", a.Budgets)
	}
}
