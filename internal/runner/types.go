// Package runner is the Go control plane's client and process supervisor for
// the loopback TypeScript model runner. Provider credentials and Core tokens
// cross this package only in request bodies; they are never logged.
package runner

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/provider"
)

// Model is one provider configuration in the runner wire contract.
type Model struct {
	Adapter         string              `json:"adapter"`
	Provider        string              `json:"provider"`
	ModelID         string              `json:"model_id"`
	BaseURL         *string             `json:"base_url"`
	APIKey          string              `json:"api_key"`
	Region          string              `json:"region"`
	Headers         map[string]string   `json:"headers"`
	OpenRouter      *openrouter.Routing `json:"openrouter"`
	MaxOutputTokens int                 `json:"max_output_tokens"`
	Temperature     *float64            `json:"temperature"`
	TopP            *float64            `json:"top_p"`
	ReasoningEffort string              `json:"reasoning_effort"`
	Hosted          bool                `json:"hosted"`
}

// ModelOf translates a validated configuration section and its resolved key.
// hosted enables the runner's SSRF guard for a model chosen by an agent owner.
func ModelOf(m config.Model, key string, hosted bool) Model {
	out := Model{
		Adapter: m.Adapter, Provider: m.EffectiveProvider(), ModelID: m.Model, APIKey: key, Region: m.Region,
		Headers: nonNilHeaders(m.Headers), OpenRouter: m.OpenRouter.Canonical(), MaxOutputTokens: m.Params.MaxOutputTokens,
		Temperature: m.Params.Temperature, TopP: m.Params.TopP, ReasoningEffort: m.Reasoning.Effort, Hosted: hosted,
	}
	if m.BaseURL != "" {
		base := m.BaseURL
		out.BaseURL = &base
	}
	return out
}

func nonNilHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Seat identifies the seats the runner's write guards must never change.
type Seat struct {
	SelfMemberID      string `json:"self_member_id"`
	PrincipalMemberID string `json:"principal_member_id"`
	OpenerMemberID    string `json:"opener_member_id"`
	ActionDecideLevel string `json:"action_decide_level"`
}

// Limits bound one runner request.
type Limits struct {
	MaxSteps       int   `json:"max_steps"`
	MaxToolCalls   int   `json:"max_tool_calls"`
	MaxWrites      int   `json:"max_writes"`
	TimeoutMS      int64 `json:"timeout_ms"`
	MaxResultBytes int   `json:"max_result_bytes"`
}

// DefaultMaxResultBytes is the runner's per-tool-result safety bound.
const DefaultMaxResultBytes = 32768

// RunRequest is POST /run's single JSON body.
type RunRequest struct {
	CoreMCPURL     string           `json:"core_mcp_url"`
	CoreToken      string           `json:"core_token"`
	CourseID       string           `json:"course_id"`
	ConversationID string           `json:"conversation_id"`
	MessageID      string           `json:"message_id"`
	Attempt        int              `json:"attempt"`
	AllowWrites    bool             `json:"allow_writes"`
	AllowedTools   []string         `json:"allowed_tools"`
	DeniedTools    []string         `json:"denied_tools"`
	Seat           Seat             `json:"seat"`
	SystemPrompt   string           `json:"system_prompt"`
	Messages       []prompt.Message `json:"messages"`
	Model          Model            `json:"model"`
	Limits         Limits           `json:"limits"`
}

// ProbeRequest is POST /probe's body. The runner also accepts a bare Model;
// the Go client uses this explicit form so its deadline is unambiguous.
type ProbeRequest struct {
	Model     Model `json:"model"`
	TimeoutMS int64 `json:"timeout_ms"`
}

// Probe results shared with the management API.
const (
	ResultOK             = "ok"
	ResultKeyRefused     = "key_refused"
	ResultModelNotFound  = "model_not_found"
	ResultKeyAccepted    = "key_accepted"
	ResultUnreachable    = "unreachable"
	ProbeTimeout         = 25 * time.Second
	maxProbeTimeoutMS    = 60000
	maxNDJSONEventBytes  = 4 << 20
	maxErrorResponseBody = 4096
)

// ProbeResult is the safe classification returned by /probe. It never
// includes an upstream error message.
type ProbeResult struct {
	Result       string  `json:"result"`
	HTTPStatus   *int    `json:"http_status"`
	ProviderCode *string `json:"provider_code"`
	LatencyMS    int64   `json:"latency_ms"`
}

// WriteCounts are the write outcomes reported by a completed run.
type WriteCounts struct {
	Executed int `json:"executed"`
	Proposed int `json:"proposed"`
	Denied   int `json:"denied"`
	Failed   int `json:"failed"`
	Refused  int `json:"refused"`
	Guarded  int `json:"guarded"`
}

// Done is a run's terminal success event.
type Done struct {
	Text          string         `json:"text"`
	Sources       []core.Source  `json:"sources"`
	SourcesStated bool           `json:"sources_stated"`
	Usage         provider.Usage `json:"usage"`
	Steps         int            `json:"steps"`
	ToolCalls     int            `json:"tool_calls"`
	Writes        WriteCounts    `json:"writes"`
	FinishReason  string         `json:"finish_reason"`
}

// Event is one NDJSON event. Only fields of Type are populated.
type Event struct {
	Type          string         `json:"type"`
	Kind          string         `json:"kind,omitempty"`
	ID            string         `json:"id,omitempty"`
	Name          string         `json:"name,omitempty"`
	Target        string         `json:"target,omitempty"`
	Delta         string         `json:"delta,omitempty"`
	Text          string         `json:"text,omitempty"`
	Sources       []core.Source  `json:"sources,omitempty"`
	SourcesStated bool           `json:"sources_stated,omitempty"`
	Usage         provider.Usage `json:"usage,omitempty"`
	Steps         int            `json:"steps,omitempty"`
	ToolCalls     int            `json:"tool_calls,omitempty"`
	Writes        WriteCounts    `json:"writes,omitempty"`
	FinishReason  string         `json:"finish_reason,omitempty"`
	Retryable     bool           `json:"retryable,omitempty"`
	HTTPStatus    *int           `json:"http_status,omitempty"`
	Message       string         `json:"message,omitempty"`
}

// Done converts a terminal done event to its compact result.
func (e Event) Done() Done {
	return Done{Text: e.Text, Sources: e.Sources, SourcesStated: e.SourcesStated, Usage: e.Usage,
		Steps: e.Steps, ToolCalls: e.ToolCalls, Writes: e.Writes, FinishReason: e.FinishReason}
}

// Error is a terminal runner error. Message comes from the runner's sanitized
// classification, never from a Go HTTP response body.
type Error struct {
	Kind       string
	Retryable  bool
	HTTPStatus int
	Message    string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("runner: %s: %s", e.Kind, e.Message)
	}
	return "runner: " + e.Kind
}

// RawUsage returns a copy of u as the provider usage object for ledgers that
// retain the original aggregate. It contains numbers only.
func RawUsage(u provider.Usage) json.RawMessage {
	b, _ := json.Marshal(struct {
		Input      int64 `json:"input"`
		Output     int64 `json:"output"`
		Reasoning  int64 `json:"reasoning"`
		CacheRead  int64 `json:"cache_read"`
		CacheWrite int64 `json:"cache_write"`
	}{u.Input, u.Output, u.Reasoning, u.CacheRead, u.CacheWrite})
	return b
}
