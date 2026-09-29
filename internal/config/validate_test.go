package config

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// baseRuntime and baseAgent are a valid configuration: a course tutor on
// the school's key, with a tenant and both quotas.
func baseRuntime() map[string]any {
	return map[string]any{
		"tenants":        map[string]any{"ten_1": map[string]any{"per_day": map[string]any{"usd": 10}}},
		"allowed_models": []any{"anthropic:anthropic:claude-*", "openai_chat:deepseek:*"},
		"denied_models":  []any{"*:*:*-preview"},
	}
}

func baseAgent() map[string]any {
	return map[string]any{
		"id":           "a1",
		"display_name": "A1",
		"tenant_id":    "ten_1",
		"core":         map[string]any{"base_url": "https://lms.example.edu", "token_ref": "secret://ten_1/agents/a1/core_token"},
		"model": map[string]any{
			"adapter": "anthropic", "model": "claude-sonnet-4-5", "key_ref": "secret://school/keys/anthropic", "key_source": "school",
		},
		"budgets": map[string]any{
			"per_agent_day": map[string]any{"usd": 5},
			"per_asker_day": map[string]any{"answers": 10},
		},
	}
}

// del marks a key to be removed.
type del struct{}

// set sets the dotted path in m to v, making maps on the way.
func set(m map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	if _, ok := v.(del); ok {
		delete(m, keys[len(keys)-1])
		return
	}
	m[keys[len(keys)-1]] = v
}

func yamlOf(t *testing.T, v any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const course1 = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b"

func TestValidate(t *testing.T) {
	long := strings.Repeat("x", 501)
	for _, tc := range []struct {
		name      string
		agent     map[string]any // paths to set in the agent
		runtime   map[string]any // paths to set in the runtime
		courses   map[string]any
		allowlist []string
		want      []problem // none: valid
	}{
		{name: "the base is valid"},
		{name: "id required", agent: map[string]any{"id": del{}}, want: []problem{{path: "agent.id", msg: "required"}}},
		{name: "id shape", agent: map[string]any{"id": "a b"}, want: []problem{{agent: "a b", path: "agent.id", msg: "letters, digits"}}},
		{name: "id too long", agent: map[string]any{"id": strings.Repeat("a", 65)}, want: []problem{{agent: strings.Repeat("a", 65), path: "agent.id"}}},
		{name: "display name", agent: map[string]any{"display_name": "  "}, want: []problem{{agent: "a1", path: "agent.display_name", msg: "required"}}},
		{name: "tenant id shape", agent: map[string]any{"tenant_id": "t 1"}, want: []problem{{agent: "a1", path: "agent.tenant_id", msg: "letters"}}},

		{name: "base url required", agent: map[string]any{"core.base_url": del{}}, want: []problem{{agent: "a1", path: "agent.core.base_url", msg: "required"}}},
		{name: "base url relative", agent: map[string]any{"core.base_url": "lms.example.edu"}, want: []problem{{agent: "a1", path: "agent.core.base_url", msg: "absolute URL"}}},
		{name: "base url http", agent: map[string]any{"core.base_url": "http://lms.example.edu"}, want: []problem{{agent: "a1", path: "agent.core.base_url", msg: "must be https"}}},
		{name: "base url http on this machine", agent: map[string]any{"core.base_url": "http://localhost:8080"}},
		{name: "base url http on 127.0.0.1", agent: map[string]any{"core.base_url": "http://127.0.0.1:18409"}},
		{name: "base url http on ::1", agent: map[string]any{"core.base_url": "http://[::1]:8080"}},
		{name: "base url http on 127.0.0.2", agent: map[string]any{"core.base_url": "http://127.0.0.2"}, want: []problem{{agent: "a1", path: "agent.core.base_url", msg: "must be https"}}},
		{name: "base url ftp", agent: map[string]any{"core.base_url": "ftp://lms.example.edu"}, want: []problem{{agent: "a1", path: "agent.core.base_url", msg: "must be https"}}},
		{name: "base url with a password", agent: map[string]any{"core.base_url": "https://u:p@lms.example.edu"}, want: []problem{{agent: "a1", path: "agent.core.base_url", msg: "no user name or password"}}},
		{name: "base url with a query", agent: map[string]any{"core.base_url": "https://lms.example.edu/?x=1"}, want: []problem{{agent: "a1", path: "agent.core.base_url", msg: "no query"}}},
		{name: "base url with a path", agent: map[string]any{"core.base_url": "https://example.edu/lms"}},
		{name: "allowlist origin", allowlist: []string{"https://lms.example.edu"}},
		{name: "allowlist origin with default port", agent: map[string]any{"core.base_url": "https://lms.example.edu:443"}, allowlist: []string{"https://lms.example.edu"}},
		{name: "allowlist other port", agent: map[string]any{"core.base_url": "https://lms.example.edu:8443"}, allowlist: []string{"https://lms.example.edu"}, want: []problem{{agent: "a1", path: "agent.core.base_url", msg: "not within CORE_BASE_URL_ALLOWLIST"}}},
		{name: "allowlist wildcard", allowlist: []string{"https://other.edu", "*.example.edu"}},
		{name: "allowlist wildcard is not the domain itself", agent: map[string]any{"core.base_url": "https://example.edu"}, allowlist: []string{"*.example.edu"}, want: []problem{{agent: "a1", path: "agent.core.base_url", msg: "not within"}}},
		{name: "allowlist miss", allowlist: []string{"https://lms.other.edu"}, want: []problem{{agent: "a1", path: "agent.core.base_url", msg: "lms.example.edu is not within"}}},
		{name: "allowlist plain host", allowlist: []string{"LMS.example.edu"}},
		{name: "allowlist scheme differs", agent: map[string]any{"core.base_url": "http://localhost:8080"}, allowlist: []string{"https://localhost:8080"}, want: []problem{{agent: "a1", path: "agent.core.base_url"}}},
		{name: "allowlist bad entry", allowlist: []string{"https://lms.example.edu/path?x", "ftp://x"}, want: []problem{{path: "CORE_BASE_URL_ALLOWLIST", msg: "entry 1"}, {path: "CORE_BASE_URL_ALLOWLIST", msg: "entry 2"}}},
		{name: "transport", agent: map[string]any{"core.transport": "grpc"}, want: []problem{{agent: "a1", path: "agent.core.transport", msg: `"grpc" is not mcp or rest`}}},
		{name: "rest transport", agent: map[string]any{"core.transport": "rest"}},
		{name: "mcp protocol", agent: map[string]any{"core.mcp_protocol": "2026-07-28"}, want: []problem{{agent: "a1", path: "agent.core.mcp_protocol", msg: "not a revision Core takes"}}},
		{name: "an older mcp protocol", agent: map[string]any{"core.mcp_protocol": "2024-11-05"}},

		{name: "token ref required", agent: map[string]any{"core.token_ref": del{}}, want: []problem{{agent: "a1", path: "agent.core.token_ref", msg: "required"}}},
		{name: "a token written in", agent: map[string]any{"core.token_ref": "ais_k7v2m4qhx3ab_9Jx2abcDEFghiJKLmnoPQRstuVWXyz0123456789_-abcd"}, want: []problem{{agent: "a1", path: "agent.core.token_ref", msg: "tokens are never written in configuration"}}},
		{name: "a token inside a reference", agent: map[string]any{"core.token_ref": "secret://ais_k7v2m4qhx3ab_9Jx2abcDEFghiJKLmnoPQRstuVWX"}, want: []problem{{agent: "a1", path: "agent.core.token_ref", msg: "never written"}}},
		{name: "a reference named like a token is fine", agent: map[string]any{"core.token_ref": "secret://school/ais_helper/core_token"}},
		{name: "not a reference", agent: map[string]any{"core.token_ref": "/run/secrets/token"}, want: []problem{{agent: "a1", path: "agent.core.token_ref", msg: "is not a reference"}}},
		{name: "a bad reference", agent: map[string]any{"core.token_ref": "secret://a/../b"}, want: []problem{{agent: "a1", path: "agent.core.token_ref", msg: "not '.' or '..' alone"}}},
		{name: "env and file references", agent: map[string]any{"core.token_ref": "env://A1_TOKEN", "model.key_ref": "file:///run/secrets/key"}},

		{name: "adapter", agent: map[string]any{"model.adapter": "openai"}, want: []problem{{agent: "a1", path: "agent.model.adapter", msg: "is not one of openai_chat"}}},
		{name: "model required", agent: map[string]any{"model.model": ""}, want: []problem{{agent: "a1", path: "agent.model.model", msg: "required"}}},
		{name: "provider", agent: map[string]any{"model.provider": "acme"}, want: []problem{{agent: "a1", path: "agent.model.provider", msg: "is not one of"}}},
		{name: "model base url", agent: map[string]any{"model.base_url": "api.example.com"}, want: []problem{{agent: "a1", path: "agent.model.base_url", msg: "absolute http or https"}}},
		{name: "model base url with a key in it", agent: map[string]any{"model.base_url": "https://api.example.com/v1?key=abc"}, want: []problem{{agent: "a1", path: "agent.model.base_url", msg: "no query"}}},
		{name: "key ref required", agent: map[string]any{"model.key_ref": del{}}, want: []problem{{agent: "a1", path: "agent.model.key_ref", msg: "required: give the key's reference"}}},
		{name: "a key written in", agent: map[string]any{"model.key_ref": "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx"}, want: []problem{{agent: "a1", path: "agent.model.key_ref", msg: "keys are never written in configuration"}}},
		{
			name:  "no key for a local server",
			agent: map[string]any{"model.key_ref": del{}, "model.adapter": "openai_chat", "model.base_url": "http://localhost:11434/v1", "model.model": "llama3.1:8b", "model.key_source": "own"},
		},
		{name: "no key for LM Studio", agent: map[string]any{"model.key_ref": del{}, "model.adapter": "openai_chat", "model.base_url": "http://127.0.0.1:1234/v1", "model.key_source": "own"}},
		{name: "no key for vLLM", agent: map[string]any{"model.key_ref": del{}, "model.adapter": "openai_chat", "model.base_url": "https://vllm.internal/v1", "model.provider": "vllm", "model.key_source": "own"}},
		{name: "no key for Bedrock", agent: map[string]any{"model.key_ref": del{}, "model.adapter": "bedrock_converse", "model.region": "us-east-1", "model.key_source": "own"}},
		{name: "a region", agent: map[string]any{"model.region": "US East"}, want: []problem{{agent: "a1", path: "agent.model.region"}}},
		{name: "key source", agent: map[string]any{"model.key_source": "shared"}, want: []problem{{agent: "a1", path: "agent.model.key_source", msg: `"shared" is not school or own`}}},
		{name: "max output tokens", agent: map[string]any{"model.params.max_output_tokens": 0}, want: []problem{{agent: "a1", path: "agent.model.params.max_output_tokens"}}},
		{name: "temperature", agent: map[string]any{"model.params.temperature": 2.5}, want: []problem{{agent: "a1", path: "agent.model.params.temperature"}}},
		{name: "top p", agent: map[string]any{"model.params.top_p": 0}, want: []problem{{agent: "a1", path: "agent.model.params.top_p"}}},
		{name: "reasoning effort", agent: map[string]any{"model.reasoning.effort": "max"}, want: []problem{{agent: "a1", path: "agent.model.reasoning.effort"}}},
		{name: "schema dialect", agent: map[string]any{"model.capabilities.schema_dialect": "strictest"}, want: []problem{{agent: "a1", path: "agent.model.capabilities.schema_dialect"}}},
		{name: "a schema dialect", agent: map[string]any{"model.capabilities.schema_dialect": "full_common"}},
		{name: "a credential header", agent: map[string]any{"model.headers": map[string]any{"Authorization": "Bearer x"}}, want: []problem{{agent: "a1", path: "agent.model.headers.Authorization", msg: "never written"}}},
		{name: "a key in a header", agent: map[string]any{"model.headers": map[string]any{"X-Extra": "sk-proj-abcdefghijklmnop"}}, want: []problem{{agent: "a1", path: "agent.model.headers.X-Extra", msg: "looks like a credential"}}},
		{name: "a bad header name", agent: map[string]any{"model.headers": map[string]any{"X Extra": "v"}}, want: []problem{{agent: "a1", path: "agent.model.headers.X Extra", msg: "not a header name"}}},
		{name: "a header", agent: map[string]any{"model.headers": map[string]any{"HTTP-Referer": "https://lms.example.edu"}}},

		{name: "a fallback", agent: map[string]any{"model.fallback": map[string]any{"adapter": "openai_chat", "model": "deepseek-chat", "base_url": "https://api.deepseek.com", "key_ref": "env://DEEPSEEK"}}},
		{
			name:  "a fallback's own problems",
			agent: map[string]any{"model.fallback": map[string]any{"adapter": "cohere", "model": "", "key_ref": "nope", "fallback": map[string]any{"adapter": "openai_chat"}}},
			want: []problem{
				{agent: "a1", path: "agent.model.fallback.adapter"},
				{agent: "a1", path: "agent.model.fallback.model"},
				{agent: "a1", path: "agent.model.fallback.key_ref", msg: "not a reference"},
				{agent: "a1", path: "agent.model.fallback.fallback", msg: "no fallback of its own"},
			},
		},
		{
			name:  "a fallback on the school's key outside the school's list",
			agent: map[string]any{"model.fallback": map[string]any{"adapter": "openai_chat", "model": "gpt-4.1", "key_ref": "env://OPENAI"}},
			want:  []problem{{agent: "a1", path: "agent.model.fallback", msg: "openai_chat:openai:gpt-4.1 is not in runtime.allowed_models"}},
		},

		{name: "school key without a tenant", agent: map[string]any{"tenant_id": del{}}, want: []problem{{agent: "a1", path: "agent.tenant_id", msg: "required on the school's key"}}},
		{name: "school key with an unknown tenant", agent: map[string]any{"tenant_id": "ten_2"}, want: []problem{{agent: "a1", path: "agent.tenant_id", msg: `"ten_2" is not in runtime.tenants`}}},
		{name: "school key with a tenant without a quota", runtime: map[string]any{"tenants.ten_1": map[string]any{}}, want: []problem{{agent: "a1", path: "agent.tenant_id", msg: "has no per_day quota"}}},
		{name: "school key without a per-agent quota", agent: map[string]any{"budgets.per_agent_day": del{}}, want: []problem{{agent: "a1", path: "agent.budgets.per_agent_day", msg: "required on the school's key"}}},
		{name: "school key without a per-asker quota", agent: map[string]any{"budgets.per_asker_day": del{}}, want: []problem{{agent: "a1", path: "agent.budgets.per_asker_day", msg: "required on the school's key"}}},
		{name: "school key outside the list", agent: map[string]any{"model.model": "haiku"}, want: []problem{{agent: "a1", path: "agent.model", msg: "anthropic:anthropic:haiku is not in runtime.allowed_models"}}},
		{name: "school key with no list", runtime: map[string]any{"allowed_models": del{}}, agent: map[string]any{"model.model": "haiku"}},
		{
			name:  "own key: no tenant, quotas or list needed",
			agent: map[string]any{"model.key_source": "own", "tenant_id": del{}, "budgets": del{}, "model.model": "anything"},
		},
		{name: "a denied model on an own key", agent: map[string]any{"model.key_source": "own", "model.model": "claude-x-preview"}, want: []problem{{agent: "a1", path: "agent.model", msg: "is denied by runtime.denied_models (*:*:*-preview)"}}},

		{name: "answer language fixed", agent: map[string]any{"prompt.answer_language": "fixed:zh-Hant-HK"}},
		{name: "answer language bad tag", agent: map[string]any{"prompt.answer_language": "fixed:english!"}, want: []problem{{agent: "a1", path: "agent.prompt.answer_language"}}},
		{name: "answer language other", agent: map[string]any{"prompt.answer_language": "asker"}, want: []problem{{agent: "a1", path: "agent.prompt.answer_language"}}},
		{name: "empty refusal text", agent: map[string]any{"prompt.on_refusal_text": ""}, want: []problem{{agent: "a1", path: "agent.prompt.on_refusal_text", msg: "required"}}},
		{name: "budget text too long", agent: map[string]any{"prompt.on_budget_text": strings.Repeat("x", 150), "answer.max_body_chars": 100}, want: []problem{{agent: "a1", path: "agent.prompt.on_budget_text", msg: "at most 100"}}},
		{name: "close reason too long", agent: map[string]any{"prompt.close_reason_text": long}, want: []problem{{agent: "a1", path: "agent.prompt.close_reason_text", msg: "at most 500"}}},
		{name: "system prompt missing", agent: map[string]any{"prompt.system_ref": "prompts/nope.md"}, want: []problem{{agent: "a1", path: "agent.prompt.system_ref", msg: "cannot be read"}}},
		{name: "a system prompt as text", agent: map[string]any{"prompt.system_text": "You answer in haiku."}},
		{name: "a system prompt as a file and as text", agent: map[string]any{"prompt.system_ref": "prices.yaml", "prompt.system_text": "Both."}, want: []problem{{agent: "a1", path: "agent.prompt.system_text", msg: "give system_ref or system_text, not both"}}},
		{name: "a system prompt of whitespace", agent: map[string]any{"prompt.system_text": " \n\t"}, want: []problem{{agent: "a1", path: "agent.prompt.system_text", msg: "holds no text"}}},
		{name: "a system prompt too long", agent: map[string]any{"prompt.system_text": strings.Repeat("é", 20001)}, want: []problem{{agent: "a1", path: "agent.prompt.system_text", msg: "is 20001 characters; at most 20000"}}},
		{name: "a system prompt at its limit", agent: map[string]any{"prompt.system_text": strings.Repeat("é", 20000)}},
		{name: "sealed references", agent: map[string]any{"core.token_ref": "sealed://sec_0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b", "model.key_ref": "sealed://sec_key"}},
		{name: "a sealed reference to no secret", agent: map[string]any{"core.token_ref": "sealed://token"}, want: []problem{{agent: "a1", path: "agent.core.token_ref", msg: "not a secret's id"}}},

		{name: "tools mode", agent: map[string]any{"tools.mode": "all"}, want: []problem{{agent: "a1", path: "agent.tools.mode"}}},
		{name: "tool names", agent: map[string]any{"tools.allow": []any{"course_get", "course.get"}, "tools.deny": []any{"Member_add"}}, want: []problem{{agent: "a1", path: "agent.tools.allow[1]"}, {agent: "a1", path: "agent.tools.deny[0]"}}},
		{name: "deny by the beginning of a name", agent: map[string]any{"tools.deny": []any{"grade_*", "submission_get", "*"}}},
		{name: "allow takes whole names only", agent: map[string]any{"tools.allow": []any{"grade_*"}, "tools.deny": []any{"grade*x", "Grade_*"}}, want: []problem{{agent: "a1", path: "agent.tools.allow[0]"}, {agent: "a1", path: "agent.tools.deny[0]"}, {agent: "a1", path: "agent.tools.deny[1]"}}},
		{name: "parallel tools", agent: map[string]any{"tools.max_parallel_tools": 0}, want: []problem{{agent: "a1", path: "agent.tools.max_parallel_tools"}}},

		{name: "attempts", agent: map[string]any{"answer.max_attempts": 11}, want: []problem{{agent: "a1", path: "agent.answer.max_attempts", msg: "from 1 to 10"}}},
		{name: "on attempts exhausted", agent: map[string]any{"answer.on_attempts_exhausted": "retry"}, want: []problem{{agent: "a1", path: "agent.answer.on_attempts_exhausted"}}},
		{name: "on quota exhausted", agent: map[string]any{"answer.on_quota_exhausted": "loud"}, want: []problem{{agent: "a1", path: "agent.answer.on_quota_exhausted"}}},
		{name: "body too short", agent: map[string]any{"answer.max_body_chars": 99}, want: []problem{{agent: "a1", path: "agent.answer.max_body_chars"}}},
		{name: "body over Core's limit", agent: map[string]any{"answer.max_body_chars": 20001}, want: []problem{{agent: "a1", path: "agent.answer.max_body_chars", msg: "20000"}}},
		{name: "body at Core's limit", agent: map[string]any{"answer.max_body_chars": 20000}},
		{name: "history", agent: map[string]any{"answer.history_messages": 201}, want: []problem{{agent: "a1", path: "agent.answer.history_messages"}}},
		{name: "concurrency", agent: map[string]any{"answer.max_concurrent": 0, "answer.max_concurrent_per_course": 0}, want: []problem{{agent: "a1", path: "agent.answer.max_concurrent"}, {agent: "a1", path: "agent.answer.max_concurrent_per_course"}}},

		{
			name:  "per-answer budgets",
			agent: map[string]any{"budgets.per_answer": map[string]any{"turns": 0, "tool_calls": 0, "input_tokens": 0, "output_tokens": -1, "wall_clock_s": 0}},
			want: []problem{
				{agent: "a1", path: "agent.budgets.per_answer.turns"}, {agent: "a1", path: "agent.budgets.per_answer.tool_calls"},
				{agent: "a1", path: "agent.budgets.per_answer.input_tokens"}, {agent: "a1", path: "agent.budgets.per_answer.output_tokens"},
				{agent: "a1", path: "agent.budgets.per_answer.wall_clock_s"},
			},
		},
		{name: "daily quotas positive", agent: map[string]any{"budgets.per_agent_day": map[string]any{"usd": 0, "answers": 0}}, want: []problem{{agent: "a1", path: "agent.budgets.per_agent_day.usd"}, {agent: "a1", path: "agent.budgets.per_agent_day.answers"}}},
		{name: "an infinite quota is none", agent: map[string]any{"budgets.per_asker_day.usd": ".inf"}, want: []problem{{agent: "a1", path: "agent.budgets.per_asker_day.usd"}}},

		{name: "polling order", agent: map[string]any{"polling.inbox_hot_s": 20}, want: []problem{{agent: "a1", path: "agent.polling", msg: "inbox_hot_s ≤ inbox_idle_s ≤ inbox_max_s"}}},
		{name: "polling zero", agent: map[string]any{"polling.inbox_hot_s": 0, "polling.events_s": 0, "polling.memberships_s": -1}, want: []problem{{agent: "a1", path: "agent.polling.inbox_hot_s"}, {agent: "a1", path: "agent.polling.events_s"}, {agent: "a1", path: "agent.polling.memberships_s"}}},
		{name: "jitter", agent: map[string]any{"polling.jitter": 0.95}, want: []problem{{agent: "a1", path: "agent.polling.jitter"}}},
		{name: "no jitter", agent: map[string]any{"polling.jitter": 0}},
		{name: "rate share", agent: map[string]any{"polling.max_rate_share": 0}, want: []problem{{agent: "a1", path: "agent.polling.max_rate_share"}}},
		{name: "rate share over one", agent: map[string]any{"polling.max_rate_share": 1.1}, want: []problem{{agent: "a1", path: "agent.polling.max_rate_share"}}},
		{name: "rates", agent: map[string]any{"polling.assumed_core_rate_per_min": 0, "polling.assumed_core_burst": 0, "polling.hot_window_s": -1}, want: []problem{{agent: "a1", path: "agent.polling.assumed_core_rate_per_min"}, {agent: "a1", path: "agent.polling.assumed_core_burst"}, {agent: "a1", path: "agent.polling.hot_window_s"}}},
		{name: "retention", agent: map[string]any{"memory.retention_days_after_removal": -1}, want: []problem{{agent: "a1", path: "agent.memory.retention_days_after_removal"}}},
		{name: "retention zero", agent: map[string]any{"memory.retention_days_after_removal": 0}},

		{name: "a course", courses: map[string]any{course1: map[string]any{"model": map[string]any{"model": "claude-haiku-4-5"}, "enabled": false}}},
		{name: "a course named otherwise", courses: map[string]any{"cs101": map[string]any{}}, want: []problem{{agent: "a1", path: "courses.cs101", msg: "a UUID"}}},
		{name: "a token pasted as a course", courses: map[string]any{"ais_k7v2m4qhx3ab_9Jx2abcDEFghiJKLmnoPQRstuVWX": map[string]any{}}, want: []problem{{agent: "a1", path: "courses.[redacted]", msg: "a UUID"}}},
		{
			name:    "the same course twice",
			courses: map[string]any{course1: map[string]any{}, strings.ToUpper(course1): map[string]any{}},
			want:    []problem{{agent: "a1", path: "courses." + course1, msg: "the same course as"}},
		},
		{
			name: "a course's own problems",
			courses: map[string]any{
				course1:                                map[string]any{"model": map[string]any{"model": "claude-x-preview"}, "answer": map[string]any{"max_attempts": 0}},
				"0192f3c1-7d2e-7c3a-9b1f-000000000000": map[string]any{"model": map[string]any{"adapter": "palm"}},
			},
			want: []problem{
				{agent: "a1", path: "courses." + course1 + ".answer.max_attempts"},
				{agent: "a1", path: "courses." + course1 + ".model", msg: "denied by runtime.denied_models"},
				{agent: "a1", path: "courses.0192f3c1-7d2e-7c3a-9b1f-000000000000.model.adapter"},
			},
		},
		{
			name:    "a course outside the school's list",
			courses: map[string]any{course1: map[string]any{"model": map[string]any{"model": "gemini-pro"}}},
			want:    []problem{{agent: "a1", path: "courses." + course1 + ".model", msg: "anthropic:anthropic:gemini-pro is not in runtime.allowed_models"}},
		},
		{
			name:    "a course dropping a quota the school's key needs",
			courses: map[string]any{course1: map[string]any{"budgets": map[string]any{"per_asker_day": nil}}},
			want:    []problem{{agent: "a1", path: "courses." + course1 + ".budgets.per_asker_day", msg: "required on the school's key"}},
		},
		{
			name:    "a course's prompt that is not there",
			courses: map[string]any{course1: map[string]any{"prompt_append_ref": "prompts/missing.md"}},
			want:    []problem{{agent: "a1", path: "courses." + course1 + ".prompt_append_ref", msg: "cannot be read"}},
		},
		{name: "a course's prompt appended as text", courses: map[string]any{course1: map[string]any{"prompt_append_text": "Answer in English."}}},
		{
			name:    "a course's prompt appended as a file and as text",
			courses: map[string]any{course1: map[string]any{"prompt_append_text": "Both.", "prompt_append_ref": "prices.yaml"}},
			want:    []problem{{agent: "a1", path: "courses." + course1 + ".prompt_append_text", msg: "give prompt_append_ref or prompt_append_text, not both"}},
		},
		{
			name:    "a course's prompt appended too long",
			courses: map[string]any{course1: map[string]any{"prompt_append_text": strings.Repeat("x", 4001)}},
			want:    []problem{{agent: "a1", path: "courses." + course1 + ".prompt_append_text", msg: "at most 4000"}},
		},

		{name: "a tenant id", runtime: map[string]any{"tenants.bad id": map[string]any{"per_day": map[string]any{"answers": 1}}}, want: []problem{{path: "runtime.tenants.bad id", msg: "tenant id"}}},
		{name: "a tenant quota", runtime: map[string]any{"tenants.ten_1.per_day.answers": 0}, want: []problem{{path: "runtime.tenants.ten_1.per_day.answers"}}},
		{name: "a price table that is not there", runtime: map[string]any{"prices_ref": "nope.yaml"}, want: []problem{{path: "runtime.prices_ref", msg: "nope.yaml"}}},
		{name: "a price table", runtime: map[string]any{"prices_ref": "prices.yaml"}},
		{
			name:    "model patterns",
			runtime: map[string]any{"allowed_models": []any{"anthropic:claude-*", "openai:openai:*", "*:acme:*", "anthropic:anthropic:claude-*"}},
			want: []problem{
				{path: "runtime.allowed_models[0]", msg: "not adapter:provider:model"},
				{path: "runtime.allowed_models[1]", msg: `"openai" is not one of`},
				{path: "runtime.allowed_models[2]", msg: `"acme" is not one of`},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, rt := baseAgent(), baseRuntime()
			for p, v := range tc.agent {
				set(agent, p, v)
			}
			for p, v := range tc.runtime {
				set(rt, p, v)
			}
			doc := map[string]any{"agent": agent}
			if tc.courses != nil {
				doc["courses"] = tc.courses
			}
			dir := write(t, map[string]string{
				"agent.yaml":   yamlOf(t, doc),
				"runtime.yaml": yamlOf(t, map[string]any{"runtime": rt}),
				"prices.yaml":  "version: v1\nprices: [{provider: anthropic, model: '*', from: 2025-01-01, usd_per_mtok: {input: 1, output: 1}}]\n",
			})
			cfg, err := Load(dir+"/agent.yaml", dir+"/runtime.yaml")
			if err == nil && tc.allowlist != nil {
				err = cfg.Validate(tc.allowlist)
			}
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			expectProblems(t, err, tc.want)
			for _, p := range Problems(err) {
				if strings.Contains(p.Msg, "sk-") || strings.Contains(p.Msg, "ais_k7v2") {
					t.Fatalf("a problem repeats a secret: %v", p)
				}
			}
		})
	}
}

func TestValidateReportsEverythingOnce(t *testing.T) {
	agent := baseAgent()
	set(agent, "model.adapter", "palm")
	set(agent, "answer.max_attempts", 0)
	set(agent, "core.transport", "grpc")
	doc := map[string]any{"agent": agent, "courses": map[string]any{course1: map[string]any{"polling": map[string]any{"inbox_idle_s": 5}}}}
	dir := write(t, map[string]string{"agent.yaml": yamlOf(t, doc), "runtime.yaml": yamlOf(t, map[string]any{"runtime": baseRuntime()})})
	_, err := Load(dir)
	got := Problems(err)
	// Three problems of the agent; the course repeats none of them.
	if len(got) != 3 {
		t.Fatalf("want 3 problems, got %d:\n%v", len(got), err)
	}
	for _, p := range got {
		if !strings.HasSuffix(p.File, "agent.yaml") || p.Agent != "a1" || !strings.HasPrefix(p.Path, "agent.") {
			t.Fatalf("problem %+v", p)
		}
	}
}

func TestProblemText(t *testing.T) {
	for _, tc := range []struct {
		p    Problem
		want string
	}{
		{Problem{File: "a.yaml", Line: 3, Agent: "a1", Path: "agent.model", Msg: "bad"}, `a.yaml:3: agent "a1": agent.model: bad`},
		{Problem{File: "r.yaml", Path: "runtime.tenants", Msg: "bad"}, `r.yaml: runtime.tenants: bad`},
		{Problem{Path: "CORE_BASE_URL_ALLOWLIST", Msg: "bad"}, `CORE_BASE_URL_ALLOWLIST: bad`},
	} {
		if got := tc.p.Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
	joined := errors.Join(&Problem{Msg: "a"}, errors.Join(&Problem{Msg: "b"}, errors.New("plain")))
	if got := Problems(joined); len(got) != 2 || got[0].Msg != "a" || got[1].Msg != "b" {
		t.Fatalf("Problems: %+v", got)
	}
	if Problems(nil) != nil {
		t.Fatal("no problems in nil")
	}
}

// Every tool Core has is a name tools.allow and tools.deny take (§1.2:
// the registry's names with the dot turned to an underscore).
func TestToolNamesTakeCoresCatalogue(t *testing.T) {
	b, err := os.ReadFile("../core/testdata/catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(b, &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Tools) != 134 {
		t.Fatalf("%d tools; the snapshot holds 134", len(cat.Tools))
	}
	for _, tool := range cat.Tools {
		name := strings.ReplaceAll(tool.Name, ".", "_")
		if !toolNameRe.MatchString(name) {
			t.Errorf("%s is refused", name)
		}
	}
}

// DeniedModel is runtime.denied_models' rule, for the API to refuse a model
// before anything is written: part by part, with globs.
func TestDeniedModel(t *testing.T) {
	rt := Runtime{DeniedModels: []string{"*:*:*-preview", "openai_chat:deepseek:*"}}
	for _, tc := range []struct {
		adapter, provider, model, pattern string
	}{
		{"anthropic", "anthropic", "claude-x-preview", "*:*:*-preview"},
		{"openai_chat", "deepseek", "deepseek-chat", "openai_chat:deepseek:*"},
		{"openai_chat", "openai", "gpt-4.1-mini", ""},
		{"openai_responses", "deepseek", "deepseek-chat", ""},
	} {
		p, denied := DeniedModel(rt, tc.adapter, tc.provider, tc.model)
		if p != tc.pattern || denied != (tc.pattern != "") {
			t.Errorf("DeniedModel(%s, %s, %s) = %q, %v", tc.adapter, tc.provider, tc.model, p, denied)
		}
	}
	if _, denied := DeniedModel(Runtime{}, "anthropic", "anthropic", "m"); denied {
		t.Error("no list denies")
	}
}
