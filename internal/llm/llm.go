// Package llm is the runtime's one format for talking to models (Core's
// docs/agent-runtime.md §3.1). Everything inside the runtime speaks it; an
// adapter per model API translates at the edge, and nothing else knows a
// provider's wire format.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// Role is who wrote a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool carries the results of the tool calls in the assistant message
	// before it, one tool_result part per call, in call order. Adapters group
	// them as their API asks (one user message for Anthropic, one message per
	// result for OpenAI Chat, …).
	RoleTool Role = "tool"
)

// PartType is what a part holds.
type PartType string

const (
	PartText       PartType = "text"
	PartReasoning  PartType = "reasoning"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
	PartFile       PartType = "file"
)

// Part is one piece of a message. Which fields mean anything depends on Type.
type Part struct {
	Type PartType `json:"type"`

	// Text is a text part's text, and a reasoning part's readable summary
	// where the provider gives one.
	Text string `json:"text,omitempty"`

	// ID is a tool call's id. Every call has one: where a provider gives
	// none, the adapter makes call_{n} (rule 2).
	ID string `json:"id,omitempty"`
	// Name is the tool's name, on a tool_call and on its tool_result.
	Name string `json:"name,omitempty"`
	// Args is always a JSON object (rule 1). Adapters parse and serialise the
	// strings some APIs use.
	Args json.RawMessage `json:"args,omitempty"`
	// ArgsError holds the provider's raw arguments when they did not parse as
	// a JSON object. Args is then {}, and the loop answers the call with an
	// is_error result and no call to Core.
	ArgsError string `json:"args_error,omitempty"`

	// CallID links a tool_result to its tool_call's ID.
	CallID string `json:"call_id,omitempty"`
	// Content is a tool result: Core's envelope as JSON text, cut to size.
	Content string `json:"content,omitempty"`
	IsError bool   `json:"is_error,omitempty"`

	// File is a file part's content. A file part in a RoleTool message
	// belongs to the results before it; adapters place it where their API
	// takes files (rule 6). The model never sees a URL.
	File *File `json:"file,omitempty"`

	// Maker and Opaque carry a provider's fragment verbatim: a reasoning
	// part's thinking block or reasoning item, or a signature that rides on a
	// text or tool_call part (Gemini's thoughtSignature). Only the adapter
	// whose Maker() equals Maker reads Opaque; every other adapter drops a
	// reasoning part it did not make, and ignores Opaque on other parts
	// (rule 4).
	Maker  string          `json:"maker,omitempty"`
	Opaque json.RawMessage `json:"opaque,omitempty"`
}

// File is a file given to the model: the runtime fetched it, the model gets
// the bytes.
type File struct {
	Name string `json:"name"`
	MIME string `json:"mime"`
	Data []byte `json:"data"`
}

// Message is one turn.
type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
}

// Text returns a text-only part.
func Text(s string) Part { return Part{Type: PartText, Text: s} }

// UserText returns a user message holding s.
func UserText(s string) Message { return Message{Role: RoleUser, Parts: []Part{Text(s)}} }

// Tool is a tool offered to the model. Schema is already sanitised for the
// adapter's dialect (toolschema); the adapter only wraps it.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// ToolMode says whether the model may call tools on this turn.
type ToolMode string

const (
	ToolAuto ToolMode = "auto"
	// ToolNone forces a text answer: the last turn of a loop whose budget is
	// spent (ForceAnswer, §3.3). An adapter whose Capabilities lack
	// ToolChoiceNone leaves the tools out instead, and one whose API refuses
	// tool history without tools declared flattens that history to text
	// (FlattenToolHistory).
	ToolNone ToolMode = "none"
)

// Limits bound one call.
type Limits struct {
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

// Request is one call to a model.
type Request struct {
	// System is one string; each adapter places it (a system or developer
	// message, a top-level field, instructions, …).
	System   string    `json:"system"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
	ToolMode ToolMode  `json:"tool_mode"`
	Limits   Limits    `json:"limits"`
}

// Stop is why the model stopped, in the runtime's terms (§3.4).
type Stop string

const (
	StopEnd             Stop = "end"
	StopToolCalls       Stop = "tool_calls"
	StopMaxTokens       Stop = "max_tokens"
	StopContentFilter   Stop = "content_filter"
	StopRefusal         Stop = "refusal"
	StopContextOverflow Stop = "context_overflow"
	// StopToolError is a malformed tool call the provider caught itself: the
	// loop retries the turn once.
	StopToolError Stop = "tool_error"
	StopError     Stop = "error"
)

// Usage is what a call used. Input is all input tokens, cached ones
// included (§3.5).
type Usage struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	// Raw is the provider's usage object, verbatim, always kept.
	Raw json.RawMessage `json:"raw,omitempty"`
	// Estimated is true when the provider reported nothing and the numbers
	// were counted here.
	Estimated bool `json:"estimated"`
}

// Add returns u plus v; Raw is v's.
func (u Usage) Add(v Usage) Usage {
	return Usage{
		Input: u.Input + v.Input, CacheRead: u.CacheRead + v.CacheRead, CacheWrite: u.CacheWrite + v.CacheWrite,
		Output: u.Output + v.Output, Reasoning: u.Reasoning + v.Reasoning,
		Raw: v.Raw, Estimated: u.Estimated || v.Estimated,
	}
}

// Response is what one call returned.
type Response struct {
	Parts []Part `json:"parts"`
	Stop  Stop   `json:"stop"`
	// RawStop is the provider's own stop reason, logged beside Stop.
	// OpenRouter's native_finish_reason goes here after a "/".
	RawStop string `json:"raw_stop"`
	Usage   Usage  `json:"usage"`
	// Model is the model the provider says served the call, when it says.
	Model string `json:"model,omitempty"`
	// RequestID is the provider's id for the call, for logs.
	RequestID string `json:"request_id,omitempty"`
}

// Text is the response's text parts, joined.
func (r *Response) Text() string {
	var b strings.Builder
	for _, p := range r.Parts {
		if p.Type == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ToolCalls is the response's tool_call parts, in order.
func (r *Response) ToolCalls() []Part {
	var calls []Part
	for _, p := range r.Parts {
		if p.Type == PartToolCall {
			calls = append(calls, p)
		}
	}
	return calls
}

// Normalize applies rule 3, that any tool_call part means StopToolCalls
// whatever the provider said, and makes sure every call has an id (rule 2)
// and object arguments (rule 1). Every adapter calls it last.
func (r *Response) Normalize() {
	n := 0
	for i := range r.Parts {
		p := &r.Parts[i]
		if p.Type != PartToolCall {
			continue
		}
		n++
		if p.ID == "" {
			p.ID = fmt.Sprintf("call_%d", n)
		}
		if len(p.Args) == 0 {
			p.Args = json.RawMessage("{}")
		}
	}
	if n > 0 {
		r.Stop = StopToolCalls
	}
}

// Capabilities are what an adapter can do against its endpoint and model.
// Each adapter has defaults by provider (Defaults); an agent's configuration
// overrides them.
type Capabilities struct {
	ParallelToolCalls bool `json:"parallel_tool_calls"`
	StrictTools       bool `json:"strict_tools"`
	// ToolChoiceNone: the API can be told not to call tools. Without it,
	// ForceAnswer leaves the tools out.
	ToolChoiceNone bool `json:"tool_choice_none"`
	// FileInput: the model takes file parts. Without it, a file is offered
	// as extracted text or not at all.
	FileInput bool `json:"file_input"`
	// ToolsWithHistory: the API refuses tool calls in the history unless
	// tools are declared (Bedrock). ForceAnswer then flattens the history.
	ToolsWithHistory bool `json:"tools_with_history"`
}

// Adapter translates the internal format to one model API and back.
type Adapter interface {
	// Name is the adapter's name in configuration: openai_chat,
	// openai_responses, anthropic, gemini, bedrock_converse.
	Name() string
	// Provider is who serves the endpoint (openai, deepseek, anthropic, …;
	// DetectProvider), for prices, capabilities and logs.
	Provider() string
	// Model is the configured model id (Azure: the deployment name).
	Model() string
	// Maker identifies what made a reasoning part: adapter, endpoint and
	// model. A part is replayed only to the adapter with the same Maker.
	Maker() string
	// Dialect is the JSON Schema dialect its tools must be given in.
	Dialect() toolschema.Dialect
	Capabilities() Capabilities
	// Call makes one model call. A provider's refusal of the call is an
	// *Error; a completed call that stopped for any reason is a Response.
	Call(ctx context.Context, req *Request) (*Response, error)
}

// ErrorKind classifies a call that did not complete.
type ErrorKind string

const (
	ErrRateLimited     ErrorKind = "rate_limited"
	ErrOverloaded      ErrorKind = "overloaded"
	ErrAuth            ErrorKind = "auth"
	ErrBadRequest      ErrorKind = "bad_request"
	ErrContextOverflow ErrorKind = "context_overflow"
	ErrContentFilter   ErrorKind = "content_filter"
	ErrServer          ErrorKind = "server"
	ErrNetwork         ErrorKind = "network"
	ErrTimeout         ErrorKind = "timeout"
)

// Error is a model call that did not complete.
type Error struct {
	Kind ErrorKind
	// Status is the HTTP status, 0 for a network error.
	Status int
	// Code is the provider's error code or type, as it gave it.
	Code string
	// Message is the provider's message, cut to 400 bytes. It never holds a
	// key: adapters build it from the response body only.
	Message    string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	s := string(e.Kind)
	if e.Status != 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Retryable reports whether the same call may succeed later.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case ErrRateLimited, ErrOverloaded, ErrServer, ErrNetwork, ErrTimeout:
		return true
	}
	return false
}

// Clip cuts s to at most 400 bytes, on a rune boundary, as Core cuts its
// messages.
func Clip(s string) string {
	const max = 400
	if len(s) <= max {
		return s
	}
	cut := max - len("…")
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
