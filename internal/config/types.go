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

import (
	"strings"
	"time"
)

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
	// Hosted is set for an agent of the registry (docs/design.md §11.2),
	// nil for one of YAML.
	Hosted *Hosted `yaml:"-"`
	// merged is the agent's configuration as a generic map, after the
	// defaults, for ForCourse to merge a course into.
	merged map[string]any
}

// Hosted is what the registry knows of a hosted agent beside its
// configuration: the Core actor its token must be, its owner, and the
// version of its row this configuration was built from.
type Hosted struct {
	// CoreActorID is the actor me_get must name: a token of any other is
	// not this agent's.
	CoreActorID   string
	OwnerActorID  string
	OwnerVerified bool
	// Version is the row's version (store.HostedAgent.Version): the worker
	// records it with every state it writes, so that the API tells a
	// change not yet in force from one that is.
	Version int
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
	// Offer names the school plan's offer (runtime.school.offers) this
	// model is, on the school's key: the registry writes it, with the
	// offer's settings, for a hosted agent on the plan, whose quotas are
	// then the plan's. Empty for any other model.
	Offer string `yaml:"offer"`
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
	// SystemText is the prompt itself, instead of SystemRef, for an agent
	// with no file of its own (a hosted agent); at most MaxSystemText
	// characters.
	SystemText string `yaml:"system_text"`
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
	// Deny is beside the built-in list, which always applies; an entry
	// ending in * denies every tool whose name begins so (design §4).
	Deny []string `yaml:"deny"`
	// Writes lets the model be offered the writes the seat's perms allow,
	// in a conversation the agent's owner opened (design §4): off by
	// default, and on for a hosted agent unless its owner turns it off,
	// since only a hosted agent's owner is known to the runtime.
	Writes           bool `yaml:"writes"`
	MaxParallelTools int  `yaml:"max_parallel_tools"`
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
	Turns int `yaml:"turns"`
	// ToolCalls bounds every tool call, reads and writes; MaxWrites bounds
	// the writes among them, counted apart from the reads.
	ToolCalls    int     `yaml:"tool_calls"`
	MaxWrites    int     `yaml:"max_writes"`
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
	// PromptAppendText is courses[course_id].prompt_append_text: the text
	// to append itself, instead of a file.
	PromptAppendText string
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
	// School is the school's AI plan: the models the school offers hosted
	// agents on its own key, and the quotas that hold them.
	School School `yaml:"school"`
	// File is the file the runtime document came from; problems name it.
	File string `yaml:"-"`
}

// Tenant is one tenant's quotas.
type Tenant struct {
	PerDay Quota `yaml:"per_day"`
}

// School is the school's AI plan (the product owner's D8): the models it
// offers hosted agents, each on a key of the school's that stays a file on
// the server, and the daily quotas that hold every agent on one. A hosted
// agent's owner chooses an offer by its id; the registry writes the
// offer's settings into the agent's configuration. The quotas count
// answers on the school's key since the start of the UTC day; those in
// dollars need a price table. It is one plan for the whole school: budgets
// of a department's or a course's would stand beside these quotas.
type School struct {
	Offers []SchoolOffer `yaml:"offers"`
	// PerOwnerDay holds a hosted agent's tenant, its owner, across all
	// their agents on the plan: DefaultPerOwnerDay answers unless set.
	PerOwnerDay Quota `yaml:"per_owner_day"`
	// PerAskerDay holds one asker, keyed on (agent, course, opener
	// member_id), on the plan: DefaultPerAskerDay answers unless set.
	PerAskerDay Quota `yaml:"per_asker_day"`
	// PerDay, when set, is a ceiling across every answer on the school's
	// key, whichever agent gives it.
	PerDay Quota `yaml:"per_day"`
	// OnQuotaText, when set, is posted in place of the built-in notice
	// (SchoolQuotaText) when a quota of the school's is spent and the
	// agent has no key of its owner's to go on with.
	OnQuotaText string `yaml:"on_quota_text"`
}

// SchoolOffer is one model the school offers: its id, which owners
// choose it by, the label people are shown, and the model section it
// stands for, on the school's key. Its key is key_ref,
// secret://school/keys/<name>, which never leaves the runtime's host.
type SchoolOffer struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label"`
	// Adapter to Capabilities are a model section's (Model), but its key
	// source (school) and its fallback (the owner's, if any).
	Adapter      string       `yaml:"adapter"`
	Model        string       `yaml:"model"`
	Provider     string       `yaml:"provider"`
	BaseURL      string       `yaml:"base_url"`
	Region       string       `yaml:"region"`
	KeyRef       string       `yaml:"key_ref"`
	Params       ModelParams  `yaml:"params"`
	Reasoning    Reasoning    `yaml:"reasoning"`
	Capabilities Capabilities `yaml:"capabilities"`
}

// The school plan's quotas when the runtime's settings give none.
const (
	DefaultPerOwnerDay = 100
	DefaultPerAskerDay = 20
)

// Offered reports whether the plan offers any model.
func (s School) Offered() bool { return len(s.Offers) > 0 }

// OfferOf is the offer whose id is id.
func (s School) OfferOf(id string) (SchoolOffer, bool) {
	for _, o := range s.Offers {
		if o.ID == id {
			return o, true
		}
	}
	return SchoolOffer{}, false
}

// OwnerQuota is per_owner_day, with DefaultPerOwnerDay answers unless it
// sets its answers.
func (s School) OwnerQuota() Quota { return withAnswers(s.PerOwnerDay, DefaultPerOwnerDay) }

// AskerQuota is per_asker_day, with DefaultPerAskerDay answers unless it
// sets its answers.
func (s School) AskerQuota() Quota { return withAnswers(s.PerAskerDay, DefaultPerAskerDay) }

// USD reports whether any of the plan's quotas is in dollars, which a
// price table must then hold.
func (s School) USD() bool {
	return s.PerOwnerDay.USD != nil || s.PerAskerDay.USD != nil || s.PerDay.USD != nil
}

func withAnswers(q Quota, n int) Quota {
	if q.Answers == nil {
		q.Answers = &n
	}
	return q
}

// Section is the offer as a model section on the school's key, as the
// registry writes it into a hosted agent's settings: JSON-shaped, with
// only what the offer sets, so that the defaults fill in the rest.
func (o SchoolOffer) Section() map[string]any {
	m := map[string]any{"adapter": o.Adapter, "model": o.Model, "key_source": KeySchool, "key_ref": o.KeyRef, "offer": o.ID}
	for k, v := range map[string]string{"provider": o.Provider, "base_url": o.BaseURL, "region": o.Region} {
		if v != "" {
			m[k] = v
		}
	}
	params := map[string]any{}
	if o.Params.MaxOutputTokens > 0 {
		params["max_output_tokens"] = o.Params.MaxOutputTokens
	}
	if o.Params.Temperature != nil {
		params["temperature"] = *o.Params.Temperature
	}
	if o.Params.TopP != nil {
		params["top_p"] = *o.Params.TopP
	}
	if len(params) > 0 {
		m["params"] = params
	}
	if o.Reasoning.Effort != "" {
		m["reasoning"] = map[string]any{"effort": o.Reasoning.Effort}
	}
	caps := map[string]any{}
	for k, v := range map[string]*bool{"parallel_tool_calls": o.Capabilities.ParallelToolCalls, "strict_tools": o.Capabilities.StrictTools,
		"tool_choice_none": o.Capabilities.ToolChoiceNone, "file_input": o.Capabilities.FileInput} {
		if v != nil {
			caps[k] = *v
		}
	}
	if o.Capabilities.SchemaDialect != "" {
		caps["schema_dialect"] = o.Capabilities.SchemaDialect
	}
	if len(caps) > 0 {
		m["capabilities"] = caps
	}
	return m
}

// AsModel is the offer as a model section on the school's key. Its output
// bound is 0 where the offer sets none: an agent's defaults give it one.
func (o SchoolOffer) AsModel() Model {
	return Model{Adapter: o.Adapter, Model: o.Model, Provider: o.Provider, BaseURL: o.BaseURL, Region: o.Region, KeyRef: o.KeyRef,
		KeySource: KeySchool, Params: o.Params, Reasoning: o.Reasoning, Capabilities: o.Capabilities, Offer: o.ID}
}

// Config is everything loaded: the runtime's settings and every agent.
type Config struct {
	Runtime Runtime
	Agents  []*Agent
	// Dir is the directory relative runtime refs resolve against.
	Dir string
	// Rejected are the agents of the registry that are not run, and why:
	// each is shown in state error, and none keeps the others from running.
	Rejected []Rejection
}

// Rejection is an agent that did not load (LoadDocuments), or that the
// registry does not run: its id, the source it came from, and why.
type Rejection struct {
	AgentID string
	Source  string
	Err     error
	// Reason is why, as the API's problem.reason names it
	// (store.ReasonSettingsRejected, …); "" when none is given.
	Reason string
	// Version is the version of the hosted agent's row that was rejected,
	// 0 when it is not known.
	Version int
}

// Detail is why, as the agent's state shows it: every problem, one after
// another, without the source's name each problem begins with.
func (r Rejection) Detail() string {
	if ps := Problems(r.Err); len(ps) > 0 {
		parts := make([]string, len(ps))
		for i, p := range ps {
			q := *p
			q.File, q.Line, q.Agent = "", 0, ""
			parts[i] = q.Error()
		}
		return strings.Join(parts, "; ")
	}
	if r.Err == nil {
		return ""
	}
	return r.Err.Error()
}

// Seconds turns a float number of seconds into a duration.
func Seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
