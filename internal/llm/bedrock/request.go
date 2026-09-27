package bedrock

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
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
	Status    string          `json:"status"`
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
}

type inputSchema struct {
	JSON json.RawMessage `json:"json"`
}

// additionalRequest is additionalModelRequestFields, passed through to the
// model as its own request fields.
type additionalRequest struct {
	Thinking *thinking `json:"thinking,omitempty"`
}

// thinking is Anthropic's extended thinking, as Claude on Bedrock takes it.
type thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
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
	for _, m := range msgs {
		role, content := a.message(m, files)
		if len(content) == 0 {
			continue
		}
		// Converse wants user and assistant to alternate; a tool message
		// (user) followed by a user message becomes one, results first.
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, content...)
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
				Name: t.Name, Description: t.Description, InputSchema: inputSchema{JSON: schema},
			}})
		}
		out.ToolConfig = tc
	}

	maxTokens := req.Limits.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = a.params.MaxOutputTokens
	}
	ic := inferenceConfig{MaxTokens: maxTokens, Temperature: a.params.Temperature, TopP: a.params.TopP}
	if budget := a.thinkingBudget(maxTokens, out.Messages); budget > 0 {
		out.AdditionalModelRequestFields = &additionalRequest{Thinking: &thinking{Type: "enabled", BudgetTokens: budget}}
		// Claude refuses a changed temperature or top_p while thinking;
		// the configured reasoning wins over sampling, rather than the
		// call failing.
		ic.Temperature, ic.TopP = nil, nil
	}
	if ic != (inferenceConfig{}) {
		out.InferenceConfig = &ic
	}
	return out, nil
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
func (a *Adapter) message(m llm.Message, files *fileState) (string, []block) {
	if m.Role == llm.RoleAssistant {
		var content []block
		for _, p := range m.Parts {
			switch p.Type {
			case llm.PartText:
				if strings.TrimSpace(p.Text) != "" {
					content = append(content, block{Text: p.Text})
				}
			case llm.PartToolCall:
				content = append(content, block{ToolUse: &toolUseBlock{
					ToolUseID: toolUseID(p.ID), Name: p.Name, Input: objectOrEmpty(p.Args),
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
			results = append(results, block{ToolResult: resultBlock(p)})
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

// resultBlock is a toolResult: Core's envelope as a json block when it
// parses as a JSON object (it does, unless it was cut to size), else as
// text.
func resultBlock(p llm.Part) *toolResult {
	status := "success"
	if p.IsError {
		status = "error"
	}
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
	return &toolResult{ToolUseID: toolUseID(p.CallID), Content: []resultContent{content}, Status: status}
}

// toolUseID is id as Converse takes it: at most 64 characters of
// [a-zA-Z0-9_-]. Bedrock's own ids (tooluse_…) and the runtime's call_{n}
// are kept; any other id, one another provider made before a fallback, is
// mapped to tooluse_ and 32 hex digits of its sha256, so that a call and
// its result, mapped apart, still match. The handout also allows "." and
// ":"; AWS's API reference gives ^[a-zA-Z0-9_-]+$, and ids within that are
// valid under both.
func toolUseID(id string) string {
	if validToolUseID(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
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

// Thinking budgets by effort, in tokens. Anthropic's minimum is 1024.
var thinkingBudgets = map[string]int{"minimal": 1024, "low": 2048, "medium": 8192, "high": 16384}

const minThinkingBudget = 1024

// thinkingBudget is the budget_tokens to ask a Claude model on Bedrock for,
// or 0 to ask for no thinking. Reasoning effort is mapped for Anthropic's
// models only, through additionalModelRequestFields [UNVERIFIED]; no other
// model is sent anything.
//
// Claude takes a budget of at least 1024 tokens, below max_tokens, so the
// budget is at most half of maxTokens, leaving the rest for the answer
// itself; with no cap known, or too small a one, it asks for no thinking
// rather than a call Claude would refuse. Claude also refuses thinking when
// the last assistant turn holds a tool call without the thinking that came
// with it (a turn another model made, before a fallback), so such a turn
// gets no thinking either.
func (a *Adapter) thinkingBudget(maxTokens int, msgs []message) int {
	budget, ok := thinkingBudgets[a.effort]
	if !ok || !anthropicModel(a.model) || maxTokens <= 0 {
		return 0
	}
	budget = min(budget, maxTokens/2)
	if budget < minThinkingBudget {
		return 0
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		c := msgs[i].Content
		if hasToolUse(c) && c[0].ReasoningContent == nil {
			return 0
		}
		break
	}
	return budget
}

func hasToolUse(content []block) bool {
	for _, b := range content {
		if b.ToolUse != nil {
			return true
		}
	}
	return false
}

// anthropicModel reports whether a Bedrock model id names one of
// Anthropic's models: anthropic.claude-…, a cross-region profile
// (us.anthropic.claude-…), or an ARN that holds one. An application
// inference profile's ARN does not say; it gets no thinking.
func anthropicModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "anthropic.") || strings.Contains(m, "claude")
}

func badRequest(msg string) *llm.Error {
	return &llm.Error{Kind: llm.ErrBadRequest, Message: "bedrock: " + msg}
}
