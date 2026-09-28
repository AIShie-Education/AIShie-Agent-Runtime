package registry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
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
	src, err := Document(a, courses, core, defaultKS)
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
	if _, err := Document(a, nil, core, config.KeyOwn); err == nil || !strings.Contains(err.Error(), "no key of the owner's is stored") {
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
// file or secret; and the school's key is not offered to hosted agents yet.
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
		{name: "the school's key", settings: `{"model": {"adapter": "anthropic", "model": "m", "key_source": "school"}}`,
			want: []string{"agent.model: the school's key is not offered to hosted agents yet"}},
		{name: "the school's key by default", settings: `{"model": {"adapter": "anthropic", "model": "m"}}`, defKS: config.KeySchool,
			want: []string{"agent.model: the school's key is not offered"}},
		{name: "a fallback inheriting the school's key", settings: `{"model": {"adapter": "anthropic", "model": "m", "key_source": "school", "fallback": {"adapter": "anthropic", "model": "n"}}}`,
			want: []string{"agent.model: the school's", "agent.model.fallback: the school's"}},
		{name: "a course on the school's key", settings: ownModel,
			courses: []store.HostedCourse{{AgentID: "agt_1", CourseID: course1, Settings: json.RawMessage(`{"model": {"key_source": "school"}}`)}},
			want:    []string{"courses." + course1 + ".model: the school's key"}},
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
			_, err := Document(row("agt_1", c.settings), c.courses, core, defKS)
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
	if err := checkModels(a, "sealed://sec_other"); err == nil || !strings.Contains(err.Error(), "has a key that is not the owner's") {
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
	if err := checkModels(&a, "sealed://sec_k_agt_empty"); err == nil || !strings.Contains(err.Error(), "agent.model.fallback: is on the school's key") {
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
	for _, f := range []failing{{rev: true}, {agents: true}, {courses: true}} {
		if _, _, err := Build(context.Background(), yamlConfig(t, ""), f, Options{CoreBaseURL: core}); err == nil {
			t.Errorf("%+v: no error", f)
		}
	}
}

type failing struct{ rev, agents, courses bool }

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
