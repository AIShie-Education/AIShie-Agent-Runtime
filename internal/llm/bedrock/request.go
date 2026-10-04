package bedrock

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// converseRequest is Converse's request body, in the order AWS documents
// it. Fields left empty are left out.
type converseRequest struct {
	Messages                     []message          `json:"messages"`
	System                       []textBlock        `json:"system,omitempty"`
	InferenceConfig              *inferenceConfig   `json:"inferenceConfig,omitempty"`
	ToolConfig                   *toolConfig        `json:"toolConfig,omitempty"`
	AdditionalModelRequestFields *additionalRequest `json:"additionalModelRequestFields,omitempty"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// block is one ContentBlock, a union: exactly one field is set.
type block struct {
	Text             string          `json:"text,omitempty"`
	Image            *imageBlock     `json:"image,omitempty"`
	Document         *documentBlock  `json:"document,omitempty"`
	ToolUse          *toolUseBlock   `json:"toolUse,omitempty"`
	ToolResult       *toolResult     `json:"toolResult,omitempty"`
	ReasoningContent json.RawMessage `json:"reasoningContent,omitempty"`
}

type textBlock struct {
	Text string `json:"text"`
}

type imageBlock struct {
	Format string `json:"format"`
	Source source `json:"source"`
}

type documentBlock struct {
	Format string `json:"format"`
	Name   string `json:"name"`
	Source source `json:"source"`
}

// source holds a file's bytes, which encoding/json sends as base64.
type source struct {
	Bytes []byte `json:"bytes"`
}

type toolUseBlock struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type toolResult struct {
	ToolUseID string          `json:"toolUseId"`
	Content   []resultContent `json:"content"`
	// Status is left out for a model not known to take it (family).
	Status string `json:"status,omitempty"`
}

// resultContent is a ToolResultContentBlock: json or text.
type resultContent struct {
	JSON json.RawMessage `json:"json,omitempty"`
	Text string          `json:"text,omitempty"`
}

type inferenceConfig struct {
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
}

type toolConfig struct {
	Tools []toolEntry `json:"tools"`
}

type toolEntry struct {
	ToolSpec toolSpec `json:"toolSpec"`
}

type toolSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema inputSchema `json:"inputSchema"`
	// Strict is sent only when the agent's capabilities ask for it
	// (strict_tools), once the model's contract tests pass (§3.8).
	Strict bool `json:"strict,omitempty"`
}

type inputSchema struct {
	JSON json.RawMessage `json:"json"`
}

// additionalRequest is additionalModelRequestFields, passed through to the
// model as its own request fields.
type additionalRequest struct {
	Thinking     *thinking     `json:"thinking,omitempty"`
	OutputConfig *outputConfig `json:"output_config,omitempty"`
}

// thinking is Anthropic's thinking, as Claude on Bedrock takes it: enabled
// with a budget, or adaptive.
type thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// outputConfig carries an adaptive model's effort.
type outputConfig struct {
	Effort string `json:"effort"`
}

// emptySchema is what a tool with no schema is declared with: Converse
// requires inputSchema.json.
var emptySchema = json.RawMessage(`{"type":"object","properties":{}}`)

// translate turns an internal request into Converse's.
//
// Tools are declared only when the model may call them: there are some,
// and the mode is auto. Converse has no tool choice none, and refuses
// toolUse and toolResult blocks in the history when no toolConfig is
// given, so ForceAnswer (ToolNone) leaves toolConfig out, and a history
// holding tool calls without toolConfig is flattened to text first
// (llm.FlattenToolHistory). The handout marks that refusal [UNVERIFIED];
// flattening is safe either way.
//
// toolChoice is left out too. auto is Converse's default when it is
// absent, and AWS documents toolChoice as taken by some models only
// (Anthropic's, Mistral Large), so sending {"auto":{}} means nothing more
// and could only break a call to a model that refuses the field.
func (a *Adapter) translate(req *llm.Request) (*converseRequest, error) {
	if req == nil {
		return nil, badRequest("no request")
	}
	declare := req.ToolMode != llm.ToolNone && len(req.Tools) > 0
	msgs := req.Messages
	if !declare && holdsToolParts(msgs) {
		msgs = llm.FlattenToolHistory(msgs)
	}

	out := &converseRequest{}
	files := newFileState()
	ids := newCallIDs()
	for _, m := range msgs {
		role, content := a.message(m, files, ids)
		if len(content) == 0 {
			continue
		}
		// Converse wants user and assistant to alternate; a tool message
		// (user) next to a user message becomes one, results first.
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			merged := append(out.Messages[n-1].Content, content...)
			if role == "user" {
				merged = resultsFirst(merged)
			}
			out.Messages[n-1].Content = merged
			continue
		}
		out.Messages = append(out.Messages, message{Role: role, Content: content})
	}
	if len(out.Messages) == 0 {
		return nil, badRequest("the request has no message with content")
	}

	if strings.TrimSpace(req.System) != "" {
		out.System = []textBlock{{Text: req.System}}
	}

	if declare {
		tc := &toolConfig{Tools: make([]toolEntry, 0, len(req.Tools))}
		for _, t := range req.Tools {
			schema := t.Schema
			if len(bytes.TrimSpace(schema)) == 0 {
				schema = emptySchema
			} else if !json.Valid(schema) {
				return nil, badRequest("the schema of tool " + t.Name + " is not JSON")
			}
			tc.Tools = append(tc.Tools, toolEntry{ToolSpec: toolSpec{
				Name: t.Name, Description: t.Description, InputSchema: inputSchema{JSON: schema}, Strict: a.caps.StrictTools,
			}})
		}
		out.ToolConfig = tc
	}

	maxTokens := req.Limits.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = a.params.MaxOutputTokens
	}
	ic := inferenceConfig{MaxTokens: maxTokens, Temperature: a.params.Temperature, TopP: a.params.TopP}
	out.AdditionalModelRequestFields = a.reasoning(maxTokens, out.Messages, req.LeastReasoning)
	if out.AdditionalModelRequestFields != nil || a.family.noSampling {
		// Claude refuses a changed temperature or top_p while thinking,
		// and its newest models refuse them always; the configured
		// reasoning wins over sampling, rather than the call failing.
		ic.Temperature, ic.TopP = nil, nil
	}
	if ic != (inferenceConfig{}) {
		out.InferenceConfig = &ic
	}
	return out, nil
}

// resultsFirst is a user message's content with its toolResult blocks
// first, each group in its order: Converse, and Claude behind it, take the
// results only at the start of the message after the toolUse message.
func resultsFirst(content []block) []block {
	out := make([]block, 0, len(content))
	for _, b := range content {
		if b.ToolResult != nil {
			out = append(out, b)
		}
	}
	for _, b := range content {
		if b.ToolResult == nil {
			out = append(out, b)
		}
	}
	return out
}

// holdsToolParts reports whether any message holds a tool call or result.
func holdsToolParts(msgs []llm.Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == llm.PartToolCall || p.Type == llm.PartToolResult {
				return true
			}
		}
	}
	return false
}

// message translates one internal message into a role and its content
// blocks. A tool message becomes a user message holding one toolResult per
// result, in call order, and then its files; a reasoning part goes back
// only to the adapter that made it (rule 4).
func (a *Adapter) message(m llm.Message, files *fileState, ids *callIDs) (string, []block) {
	if m.Role == llm.RoleAssistant {
		ids.newTurn()
		var content []block
		for _, p := range m.Parts {
			switch p.Type {
			case llm.PartText:
				if strings.TrimSpace(p.Text) != "" {
					content = append(content, block{Text: p.Text})
				}
			case llm.PartToolCall:
				content = append(content, block{ToolUse: &toolUseBlock{
					ToolUseID: ids.call(p.ID), Name: p.Name, Input: objectOrEmpty(p.Args),
				}})
			case llm.PartReasoning:
				if p.Maker == a.maker && isObject(p.Opaque) {
					content = append(content, block{ReasoningContent: p.Opaque})
				}
			}
		}
		return "assistant", content
	}

	// User and tool messages: results first, as Converse (and Claude
	// behind it) wants them straight after the toolUse message.
	var results, rest []block
	for _, p := range m.Parts {
		switch p.Type {
		case llm.PartToolResult:
			results = append(results, block{ToolResult: a.resultBlock(p, ids.result(p.CallID))})
		case llm.PartText:
			if strings.TrimSpace(p.Text) != "" {
				rest = append(rest, block{Text: p.Text})
			}
		case llm.PartFile:
			if p.File != nil {
				rest = append(rest, a.fileBlocks(p.File, files)...)
			}
		}
	}
	return "user", append(results, rest...)
}

// resultBlock is a toolResult answering the toolUse given id: Core's
// envelope as a json block when it parses as a JSON object (it does, unless
// it was cut to size), else as text. Its status says whether it is an error,
// for the models that take the field.
func (a *Adapter) resultBlock(p llm.Part, id string) *toolResult {
	var content resultContent
	switch {
	case isObject(json.RawMessage(p.Content)):
		content.JSON = json.RawMessage(p.Content)
	case strings.TrimSpace(p.Content) == "":
		// Converse refuses a blank text block.
		content.Text = "(no content)"
	default:
		content.Text = p.Content
	}
	r := &toolResult{ToolUseID: id, Content: []resultContent{content}}
	if a.family.toolStatus {
		r.Status = "success"
		if p.IsError {
			r.Status = "error"
		}
	}
	return r
}

// callIDs gives each tool call of one request its toolUseId, and each
// result the id of the call it answers. Converse needs the two to match,
// and Claude behind it refuses a request in which two toolUse blocks share
// an id, which happens when another adapter numbered its calls call_1,
// call_2, … afresh on every turn before a fallback. A result answers a call
// of the assistant turn before it, in order among calls that share an id.
type callIDs struct {
	used map[string]bool
	turn map[string][]string
}

func newCallIDs() *callIDs {
	return &callIDs{used: map[string]bool{}, turn: map[string][]string{}}
}

// newTurn starts an assistant turn: later results answer its calls.
func (c *callIDs) newTurn() { clear(c.turn) }

// call is the toolUseId for a call with id: the id itself where Converse
// takes it and no earlier call has it, else a mapped one (toolUseID) or,
// for a repeat, one mapped from the id and its repeat's number.
func (c *callIDs) call(id string) string {
	w := toolUseID(id)
	for n := 2; c.used[w]; n++ {
		w = mappedID(fmt.Sprintf("%s#%d", id, n))
	}
	c.used[w] = true
	c.turn[id] = append(c.turn[id], w)
	return w
}

// result is the toolUseId of the call id answers: the first call of the
// last assistant turn with that id not yet answered, or id as toolUseID
// maps it when there is none.
func (c *callIDs) result(id string) string {
	if ws := c.turn[id]; len(ws) > 0 {
		c.turn[id] = ws[1:]
		return ws[0]
	}
	return toolUseID(id)
}

// toolUseID is id as Converse takes it: at most 64 characters of
// [a-zA-Z0-9_-]. Bedrock's own ids (tooluse_…) and the runtime's call_{n}
// are kept; any other id, one another provider made before a fallback, is
// mapped (mappedID), deterministically, so that a call and its result,
// mapped apart, still match. The handout also allows "." and ":"; AWS's
// API reference gives ^[a-zA-Z0-9_-]+$, and ids within that are valid
// under both.
func toolUseID(id string) string {
	if validToolUseID(id) {
		return id
	}
	return mappedID(id)
}

// mappedID is tooluse_ and 32 hex digits of s's sha256.
func mappedID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "tooluse_" + hex.EncodeToString(sum[:16])
}

func validToolUseID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !idByte(id[i]) {
			return false
		}
	}
	return true
}

func idByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// objectOrEmpty is args when it is a JSON object, else {}: a call whose
// arguments did not parse was answered with an error, and Converse takes
// only an object as input.
func objectOrEmpty(args json.RawMessage) json.RawMessage {
	if isObject(args) {
		return args
	}
	return json.RawMessage("{}")
}

// isObject reports whether raw is one valid JSON object.
func isObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{' && json.Valid(t)
}

// thinkingBudgets are budget_tokens by effort, for the Claude models that
// think with a budget. Anthropic's minimum is 1024.
var thinkingBudgets = map[string]int{"minimal": 1024, "low": 2048, "medium": 8192, "high": 16384}

// adaptiveEffort is output_config.effort by effort, for the Claude models
// that think adaptively. Anthropic's API has no minimal.
var adaptiveEffort = map[string]string{"minimal": "low", "low": "low", "medium": "medium", "high": "high"}

const minThinkingBudget = 1024

// reasoning is additionalModelRequestFields asking a Claude model on
// Bedrock to think at the configured effort, or nil to ask for nothing.
// Effort is mapped for Anthropic's models only [UNVERIFIED]; no other model
// is sent anything, and neither is a Claude from before thinking.
//
// Models from Claude 4.6 on are asked with {type: adaptive} and
// output_config.effort, older ones with {type: enabled, budget_tokens}
// (family); that Converse passes output_config through as it passes
// thinking is [UNVERIFIED]. A budget is at least 1024 tokens and below
// max_tokens, so it is at most half of maxTokens, leaving the rest for the
// answer itself; with no cap known, or too small a one, it asks for no
// thinking rather than for a call Claude would refuse.
//
// Claude also refuses thinking when the last assistant turn holds a tool
// call without the thinking that came with it (a turn another model made,
// before a fallback), so such a turn gets no thinking either.
//
// A call asking for the least reasoning (least: ForceAnswer's, a
// continuation's) is made at low in place of medium or high
// (llm.LeastEffort), and a Claude that thinks unasked (from Opus 5) is
// asked at low as if low were configured, adaptively, as the anthropic
// adapter asks it: never {type: disabled}, which some of those models
// refuse.
func (a *Adapter) reasoning(maxTokens int, msgs []message, least bool) *additionalRequest {
	effort := a.effort
	if least {
		effort = llm.LeastEffort(effort)
		if effort == "" && a.family.thinksByDefault {
			effort = "low"
		}
	}
	budget, ok := thinkingBudgets[effort]
	if !ok || !a.family.thinks || !lastToolTurnThinks(msgs) {
		return nil
	}
	if a.family.adaptive {
		return &additionalRequest{Thinking: &thinking{Type: "adaptive"}, OutputConfig: &outputConfig{Effort: adaptiveEffort[effort]}}
	}
	if maxTokens <= 0 {
		return nil
	}
	budget = min(budget, maxTokens/2)
	if budget < minThinkingBudget {
		return nil
	}
	return &additionalRequest{Thinking: &thinking{Type: "enabled", BudgetTokens: budget}}
}

// lastToolTurnThinks reports whether the last assistant message starts
// with its reasoning, or holds no tool call.
func lastToolTurnThinks(msgs []message) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		c := msgs[i].Content
		return !hasToolUse(c) || c[0].ReasoningContent != nil
	}
	return true
}

func hasToolUse(content []block) bool {
	for _, b := range content {
		if b.ToolUse != nil {
			return true
		}
	}
	return false
}

func badRequest(msg string) *llm.Error {
	return &llm.Error{Kind: llm.ErrBadRequest, Message: "bedrock: " + msg}
}
