// Package config is each agent's configuration: the YAML of Core's
// docs/agent-runtime.md §4, one document per agent, with the runtime's own
// settings beside them.
//
// Precedence, lowest first: built-in defaults (Defaults), the runtime's
// defaults section, the agent, then courses[course_id] for that course. Maps
// merge key by key; anything else is replaced whole. Over all of it stand
// the seat's perms in Core, which the runtime only narrows.
//
// Secrets are references (token_ref, key_ref), never values: secret://…,
// env://NAME or file:///path (package secrets).
package config

import "time"

// Agent is one hosted agent: the `agent:` document of §4, and its courses.
type Agent struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
	TenantID    string `yaml:"tenant_id"`
	// Paused stops every call to Core for the agent, so that Core's
	// presence tells the truth (§2.5).
	Paused  bool    `yaml:"paused"`
	Core    Core    `yaml:"core"`
	Model   Model   `yaml:"model"`
	Prompt  Prompt  `yaml:"prompt"`
	Tools   Tools   `yaml:"tools"`
	Answer  Answer  `yaml:"answer"`
	Budgets Budgets `yaml:"budgets"`
	Polling Polling `yaml:"polling"`
	Memory  Memory  `yaml:"memory"`

	// Courses are the per-course overrides by course_id, as written. Use
	// ForCourse for a course's effective configuration.
	Courses map[string]map[string]any `yaml:"-"`
	// Dir is the directory of the file the agent came from; a relative
	// *_ref path resolves against it.
	Dir string `yaml:"-"`
	// File is the file the agent came from, as Load was given it; problems
	// name it.
	File string `yaml:"-"`
	// merged is the agent's configuration as a generic map, after the
	// defaults, for ForCourse to merge a course into.
	merged map[string]any //nolint:unused // set by the loader, read by ForCourse
}

// Core is how the agent reaches Core.
type Core struct {
	BaseURL string `yaml:"base_url"`
	// Transport is mcp (the default) or rest.
	Transport string `yaml:"transport"`
	// MCPProtocol is the pinned MCP revision.
	MCPProtocol string `yaml:"mcp_protocol"`
	TokenRef    string `yaml:"token_ref"`
}

// Model is the model the agent answers with.
type Model struct {
	// Adapter is openai_chat, openai_responses, anthropic, gemini or
	// bedrock_converse.
	Adapter string `yaml:"adapter"`
	Model   string `yaml:"model"`
	// BaseURL is the API's base; empty for the adapter's default.
	BaseURL string `yaml:"base_url"`
	// KeyRef is the provider key's reference; empty where none is needed.
	KeyRef string `yaml:"key_ref"`
	// KeySource is school or own (§5.2).
	KeySource string `yaml:"key_source"`
	// Provider overrides the provider detected from the base URL.
	Provider string `yaml:"provider"`
	// Region is Bedrock's.
	Region       string            `yaml:"region"`
	Params       ModelParams       `yaml:"params"`
	Reasoning    Reasoning         `yaml:"reasoning"`
	Capabilities Capabilities      `yaml:"capabilities"`
	Headers      map[string]string `yaml:"headers"`
	// Fallback is tried when the model's provider cannot be reached.
	Fallback *Model `yaml:"fallback"`
}

// ModelParams are sampling settings.
type ModelParams struct {
	MaxOutputTokens int      `yaml:"max_output_tokens"`
	Temperature     *float64 `yaml:"temperature"`
	TopP            *float64 `yaml:"top_p"`
}

// Reasoning asks the model to think, mapped per adapter.
type Reasoning struct {
	Effort string `yaml:"effort"`
}

// Capabilities override the adapter's table where set.
type Capabilities struct {
	ParallelToolCalls *bool `yaml:"parallel_tool_calls"`
	StrictTools       *bool `yaml:"strict_tools"`
	ToolChoiceNone    *bool `yaml:"tool_choice_none"`
	FileInput         *bool `yaml:"file_input"`
	// SchemaDialect overrides the dialect (toolschema.Dialect).
	SchemaDialect string `yaml:"schema_dialect"`
}

// Prompt is what the model is told.
type Prompt struct {
	// SystemRef is a prompt file; empty for the built-in prompt for the
	// seat's kind (a person's own agent, or a course tutor). Its hash is kept
	// per answer.
	SystemRef string `yaml:"system_ref"`
	// AnswerLanguage is opener (answer in the asker's language) or
	// fixed:<bcp47>.
	AnswerLanguage string `yaml:"answer_language"`
	OnRefusalText  string `yaml:"on_refusal_text"`
	OnBudgetText   string `yaml:"on_budget_text"`
	// OnQuotaText is the canned notice posted when the asker, the agent or
	// the tenant is out of quota.
	OnQuotaText string `yaml:"on_quota_text"`
	// CloseReasonText is the reason given when the runtime closes a
	// conversation after max_attempts.
	CloseReasonText string `yaml:"close_reason_text"`
}

// Tools is what the model may call.
type Tools struct {
	// Mode is derived (the seat's perms ∩ allow − deny) or none.
	Mode  string   `yaml:"mode"`
	Allow []string `yaml:"allow"`
	// Deny is beside the built-in list, which always applies.
	Deny             []string `yaml:"deny"`
	MaxParallelTools int      `yaml:"max_parallel_tools"`
}

// Answer is how questions are answered.
type Answer struct {
	MaxAttempts int `yaml:"max_attempts"`
	// OnAttemptsExhausted is close or skip.
	OnAttemptsExhausted string `yaml:"on_attempts_exhausted"`
	// OnQuotaExhausted is canned or silent.
	OnQuotaExhausted string `yaml:"on_quota_exhausted"`
	MaxBodyChars     int    `yaml:"max_body_chars"`
	// HistoryMessages is how many of the conversation's newest messages
	// the model is given.
	HistoryMessages int `yaml:"history_messages"`
	// MaxConcurrent and MaxConcurrentPerCourse bound the answers in
	// progress at once (§7.4).
	MaxConcurrent          int `yaml:"max_concurrent"`
	MaxConcurrentPerCourse int `yaml:"max_concurrent_per_course"`
}

// Budgets bound spending.
type Budgets struct {
	PerAnswer   PerAnswer `yaml:"per_answer"`
	PerAgentDay Quota     `yaml:"per_agent_day"`
	// PerAskerDay is keyed on (course_id, opener member_id).
	PerAskerDay Quota `yaml:"per_asker_day"`
}

// PerAnswer are the hard caps inside one answer's loop (§7.1).
type PerAnswer struct {
	Turns        int     `yaml:"turns"`
	ToolCalls    int     `yaml:"tool_calls"`
	InputTokens  int64   `yaml:"input_tokens"`
	OutputTokens int64   `yaml:"output_tokens"`
	WallClockS   float64 `yaml:"wall_clock_s"`
}

// WallClock is WallClockS as a duration.
func (p PerAnswer) WallClock() time.Duration { return Seconds(p.WallClockS) }

// Quota is a daily allowance; nil fields are unlimited.
type Quota struct {
	USD     *float64 `yaml:"usd"`
	Answers *int     `yaml:"answers"`
}

// Polling is how often Core is asked (§7.2, §7.3).
type Polling struct {
	InboxHotS             float64 `yaml:"inbox_hot_s"`
	HotWindowS            float64 `yaml:"hot_window_s"`
	InboxIdleS            float64 `yaml:"inbox_idle_s"`
	InboxMaxS             float64 `yaml:"inbox_max_s"`
	EventsS               float64 `yaml:"events_s"`
	MembershipsS          float64 `yaml:"memberships_s"`
	Jitter                float64 `yaml:"jitter"`
	MaxRateShare          float64 `yaml:"max_rate_share"`
	AssumedCoreRatePerMin int     `yaml:"assumed_core_rate_per_min"`
	// AssumedCoreBurst is Core's RATE_LIMIT_BURST.
	AssumedCoreBurst int `yaml:"assumed_core_burst"`
}

// Memory is what the agent remembers.
type Memory struct {
	Enabled                   bool `yaml:"enabled"`
	RetentionDaysAfterRemoval int  `yaml:"retention_days_after_removal"`
}

// Effective is an agent's configuration for one course: the agent with
// courses[course_id] merged in.
type Effective struct {
	Agent
	// Enabled is false when courses[course_id].enabled says so.
	Enabled bool
	// PromptAppendRef is courses[course_id].prompt_append_ref.
	PromptAppendRef string
}

// Runtime is the process's own settings, from the `runtime:` document.
type Runtime struct {
	// Defaults are agent settings every agent starts from, over the
	// built-in ones.
	Defaults map[string]any `yaml:"defaults"`
	// Tenants' daily quotas, across a tenant's agents on the school's key.
	Tenants map[string]Tenant `yaml:"tenants"`
	// PricesRef is the price table (package pricing).
	PricesRef string `yaml:"prices_ref"`
	// AllowedModels, when set, is the school's list: an agent on the
	// school's key may use only these (adapter/provider/model patterns).
	AllowedModels []string `yaml:"allowed_models"`
	// DeniedModels an administrator denies even on an owner's key.
	DeniedModels []string `yaml:"denied_models"`
	// File is the file the runtime document came from; problems name it.
	File string `yaml:"-"`
}

// Tenant is one tenant's quotas.
type Tenant struct {
	PerDay Quota `yaml:"per_day"`
}

// Config is everything loaded: the runtime's settings and every agent.
type Config struct {
	Runtime Runtime
	Agents  []*Agent
	// Dir is the directory relative runtime refs resolve against.
	Dir string
}

// Seconds turns a float number of seconds into a duration.
func Seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
