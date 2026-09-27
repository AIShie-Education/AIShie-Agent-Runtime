package anthropic

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

// wireRequest is the body of POST /v1/messages.
type wireRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// System is one text block marked for caching, so that the tools and
	// the system prompt, which stay the same through a loop, are read from
	// the cache on every turn after the first. Below a model's minimum
	// cacheable length (512 to 4096 tokens) the mark is ignored, not
	// refused.
	System       []block       `json:"system,omitempty"`
	Messages     []wireMessage `json:"messages"`
	Tools        []wireTool    `json:"tools,omitempty"`
	ToolChoice   *toolChoice   `json:"tool_choice,omitempty"`
	Thinking     *thinking     `json:"thinking,omitempty"`
	OutputConfig *outputConfig `json:"output_config,omitempty"`
	Temperature  *float64      `json:"temperature,omitempty"`
	TopP         *float64      `json:"top_p,omitempty"`
}

type wireMessage struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// block is one content block of a request. A replayed thinking block is
// sent as it came (raw); every other block is built from the fields.
type block struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      string          `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	Source       *source         `json:"source,omitempty"`
	Title        string          `json:"title,omitempty"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`

	raw json.RawMessage
}

// MarshalJSON writes a replayed block verbatim and any other block from
// its fields, without HTML escaping, which only bloats tool results.
func (b block) MarshalJSON() ([]byte, error) {
	if b.raw != nil {
		return b.raw, nil
	}
	type plain block
	return marshal(plain(b))
}

type source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type cacheControl struct {
	Type string `json:"type"`
}

var ephemeral = &cacheControl{Type: "ephemeral"}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	// Strict is sent only when the agent turns strict tools on for the
	// model (capabilities.strict_tools), once its contract tests pass
	// (§3.8); the default is non-strict with local validation.
	Strict       bool          `json:"strict,omitempty"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// toolChoice is only ever auto or none. Some current models, and every
// model thinking with a budget, refuse any and tool with a 400 (§3.3).
type toolChoice struct {
	Type                   string `json:"type"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// thinkingBudgets are the tokens each reasoning effort may think for. A
// budget is at least 1024, the API's minimum, so minimal is low.
var thinkingBudgets = map[string]int{
	"minimal": 1024,
	"low":     1024,
	"medium":  4096,
	"high":    16384,
}

// adaptiveEffort is output_config.effort for each reasoning effort. The API
// has no minimal.
var adaptiveEffort = map[string]string{
	"minimal": "low",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
}

// defaultThinkingAllowance is what max_tokens allows for thinking on a
// model that thinks unasked, at its own default effort. It is high's
// budget: those models default to high or below.
const defaultThinkingAllowance = 16384

// Limits of the API that a request must stay within, or it is refused.
const (
	// maxImageBytes is an image's size, base64-encoded; the API refuses
	// images over 5 MB, and counting the encoded bytes keeps well inside
	// it whichever size it measures.
	maxImageBytes = 5 << 20
	// maxFileBytes is what all the files of one request may take, encoded:
	// the API refuses requests over 32 MB, and the rest of the request
	// needs room too.
	maxFileBytes = 24 << 20
)

// imageTypes are the image formats the API takes.
var imageTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// encodeRequest is the request's body.
func (a *Adapter) encodeRequest(req *llm.Request) ([]byte, error) {
	w, err := a.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := marshal(w)
	if err != nil {
		return nil, badRequest("the request does not encode: %v", err)
	}
	return body, nil
}

// buildRequest translates req. Failures are the caller's bugs, reported as
// ErrBadRequest without a call.
func (a *Adapter) buildRequest(req *llm.Request) (*wireRequest, error) {
	if req == nil {
		return nil, badRequest("no request")
	}
	w := &wireRequest{Model: a.model}
	if strings.TrimSpace(req.System) != "" {
		w.System = []block{{Type: "text", Text: req.System, CacheControl: ephemeral}}
	}

	// Tools are declared on every turn but a forced answer to a model that
	// cannot be told tool_choice none. The API refuses tool_use and
	// tool_result blocks in a request that declares no tools, so a history
	// holding them is flattened to text then (llm.FlattenToolHistory).
	mode := req.ToolMode
	switch mode {
	case "":
		mode = llm.ToolAuto
	case llm.ToolAuto, llm.ToolNone:
	default:
		return nil, badRequest("tool mode %q is not auto or none", mode)
	}
	declare := len(req.Tools) > 0 && (mode == llm.ToolAuto || a.caps.ToolChoiceNone)
	msgs := req.Messages
	if !declare && hasToolParts(msgs) {
		msgs = llm.FlattenToolHistory(msgs)
	}
	var err error
	if w.Messages, err = a.messages(msgs); err != nil {
		return nil, err
	}
	if len(w.Messages) == 0 {
		return nil, badRequest("the request has no message with content")
	}
	if declare {
		if w.Tools, err = a.tools(req.Tools); err != nil {
			return nil, err
		}
		if mode == llm.ToolNone {
			w.ToolChoice = &toolChoice{Type: "none"}
		} else {
			w.ToolChoice = &toolChoice{Type: "auto", DisableParallelToolUse: !a.caps.ParallelToolCalls}
		}
	}

	r := a.reasoning(w.Messages)
	w.Thinking, w.OutputConfig = r.thinking, r.output
	w.MaxTokens = outputCap(req.Limits.MaxOutputTokens, a.params.MaxOutputTokens) + r.allowance
	// The API refuses temperature and top_p while the model thinks, and the
	// newest models refuse them always. Claude 4 models also refuse both at
	// once, so temperature wins when both are configured. Both are held to
	// the API's range, 0 to 1, outside which it refuses them.
	if !r.on && !a.family.noSampling {
		switch {
		case a.params.Temperature != nil:
			t := min(max(*a.params.Temperature, 0), 1)
			w.Temperature = &t
		case a.params.TopP != nil:
			p := min(max(*a.params.TopP, 0), 1)
			w.TopP = &p
		}
	}
	return w, nil
}

// outputCap is the call's cap, else the agent's, else DefaultMaxTokens.
func outputCap(call, agent int) int {
	switch {
	case call > 0:
		return call
	case agent > 0:
		return agent
	}
	return DefaultMaxTokens
}

// reasoningConfig is how one call asks the model to think.
type reasoningConfig struct {
	thinking *thinking
	output   *outputConfig
	// allowance is added to max_tokens, which counts thinking too, so that
	// thinking does not eat the answer's share.
	allowance int
	// on: the model thinks on this call, so sampling settings stay out.
	on bool
}

// reasoning maps the agent's reasoning effort to the model's family.
//
// Models from Opus 4.6 on are asked with adaptive thinking and an effort;
// older ones with {type: enabled, budget_tokens}. Which shape each model
// takes, and whether a newer model still accepts the older one, is
// [UNVERIFIED] per model beyond Anthropic's documentation (familyOf).
func (a *Adapter) reasoning(msgs []wireMessage) reasoningConfig {
	switch {
	case a.effort == "":
		if a.family.thinksByDefault {
			return reasoningConfig{allowance: defaultThinkingAllowance, on: true}
		}
		return reasoningConfig{}
	case a.family.adaptive:
		return reasoningConfig{
			thinking:  &thinking{Type: "adaptive"},
			output:    &outputConfig{Effort: adaptiveEffort[a.effort]},
			allowance: thinkingBudgets[a.effort],
			on:        true,
		}
	case !turnThinks(msgs):
		// With a thinking budget, the API refuses a request that continues
		// an assistant turn which did not start with a thinking block, and
		// a turn cannot change its thinking mode half way. That happens
		// only when another model began the turn (a fallback mid-loop), or
		// its thinking was stripped (Call): the rest of the turn goes
		// without thinking rather than fail.
		return reasoningConfig{}
	}
	b := thinkingBudgets[a.effort]
	return reasoningConfig{thinking: &thinking{Type: "enabled", BudgetTokens: b}, allowance: b, on: true}
}

// turnThinks reports whether the assistant turn msgs continue began with a
// thinking block; msgs that do not end in tool results continue no turn,
// and may think.
//
// A turn runs from the first assistant message after the last user message
// that is not tool results, through every round of tool calls and results
// since: the API sees one assistant turn. Without interleaved thinking,
// which the adapter does not ask for, a model thinking with a budget thinks
// only at the start of the turn, so the later rounds' messages carry no
// thinking of their own, and only the first says how the turn thinks.
func turnThinks(msgs []wireMessage) bool {
	n := len(msgs)
	if n < 2 || !isToolResults(msgs[n-1]) {
		return true
	}
	// Neighbours of one role are merged, so roles alternate: msgs[i-1] is
	// the user message before the assistant message msgs[i].
	i := n - 2
	for i >= 2 && isToolResults(msgs[i-1]) {
		i -= 2
	}
	t := msgs[i].Content[0].Type
	return t == "thinking" || t == "redacted_thinking"
}

// isToolResults reports whether m is a user message answering tool calls.
// Its tool_result blocks come first (messages).
func isToolResults(m wireMessage) bool {
	return m.Role == "user" && len(m.Content) > 0 && m.Content[0].Type == "tool_result"
}

// messages translates the history. Tool messages become user messages;
// messages left empty are dropped, and neighbours of one role are merged,
// since the API takes neither. In every user message the tool_result
// blocks come first, as the API requires.
func (a *Adapter) messages(msgs []llm.Message) ([]wireMessage, error) {
	var out []wireMessage
	files := maxFileBytes
	for i, m := range msgs {
		var role string
		var blocks []block
		switch m.Role {
		case llm.RoleAssistant:
			role, blocks = "assistant", a.assistantBlocks(m.Parts)
		case llm.RoleUser, llm.RoleTool:
			role, blocks = "user", a.userBlocks(m.Parts, &files)
		default:
			return nil, badRequest("message %d has role %q", i, m.Role)
		}
		if len(blocks) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}
		out = append(out, wireMessage{Role: role, Content: blocks})
	}
	for i := range out {
		if out[i].Role == "user" {
			slices.SortStableFunc(out[i].Content, func(x, y block) int {
				return resultRank(x) - resultRank(y)
			})
		}
	}
	return out, nil
}

func resultRank(b block) int {
	if b.Type == "tool_result" {
		return 0
	}
	return 1
}

// assistantBlocks are an assistant message's text, tool calls and, when
// this adapter made them, thinking blocks, in their order.
func (a *Adapter) assistantBlocks(parts []llm.Part) []block {
	var out []block
	for _, p := range parts {
		switch p.Type {
		case llm.PartText:
			if strings.TrimSpace(p.Text) != "" {
				out = append(out, block{Type: "text", Text: p.Text})
			}
		case llm.PartReasoning:
			// Rule 4: reasoning goes back only to the model that made it,
			// unchanged, where it was.
			if p.Maker != a.maker {
				continue
			}
			if t, ok := replayable(p.Opaque); ok {
				out = append(out, block{Type: t, raw: p.Opaque})
			}
		case llm.PartToolCall:
			out = append(out, block{Type: "tool_use", ID: wireID(p.ID), Name: p.Name, Input: toolInput(p)})
		}
	}
	return out
}

// userBlocks are a user or tool message's tool results, text and files.
// files is what the request's files may still take.
func (a *Adapter) userBlocks(parts []llm.Part, files *int) []block {
	var out []block
	for _, p := range parts {
		switch p.Type {
		case llm.PartText:
			if strings.TrimSpace(p.Text) != "" {
				out = append(out, block{Type: "text", Text: p.Text})
			}
		case llm.PartToolResult:
			out = append(out, block{Type: "tool_result", ToolUseID: wireID(p.CallID), Content: p.Content, IsError: p.IsError})
		case llm.PartFile:
			if p.File != nil {
				out = append(out, a.fileBlock(p.File, files))
			}
		}
	}
	return out
}

// fileBlock is a file as the model can take it (rule 6): a PDF as a
// document and an image as an image when the model takes files, text as
// text, and otherwise a note saying the file is there but not shown.
// Files are taken in order until the request's allowance is spent, so a
// file shown on one turn is shown on every later one.
func (a *Adapter) fileBlock(f *llm.File, files *int) block {
	mt := mediaType(f.MIME)
	switch {
	case isText(mt) && utf8.Valid(f.Data):
		if len(f.Data) > *files {
			return fileNote(f, "is too large to show")
		}
		*files -= len(f.Data)
		return block{Type: "text", Text: fmt.Sprintf("[file %q]\n%s", f.Name, f.Data)}
	case a.caps.FileInput && (mt == "application/pdf" || imageTypes[mt]):
		data := base64.StdEncoding.EncodeToString(f.Data)
		if len(data) > *files || (mt != "application/pdf" && len(data) > maxImageBytes) {
			return fileNote(f, "is too large to show")
		}
		*files -= len(data)
		src := &source{Type: "base64", MediaType: mt, Data: data}
		if mt == "application/pdf" {
			return block{Type: "document", Source: src, Title: f.Name}
		}
		return block{Type: "image", Source: src}
	}
	return fileNote(f, "cannot be shown to this model")
}

func fileNote(f *llm.File, why string) block {
	return block{Type: "text", Text: fmt.Sprintf("[The file %q (%s, %d bytes) %s.]", f.Name, f.MIME, len(f.Data), why)}
}

// mediaType is m without parameters, in lower case, with the image/jpg
// some servers send read as image/jpeg.
func mediaType(m string) string {
	mt, _, err := mime.ParseMediaType(m)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(m))
	}
	if mt == "image/jpg" {
		return "image/jpeg"
	}
	return mt
}

func isText(mt string) bool {
	return strings.HasPrefix(mt, "text/") || mt == "application/json"
}

// replayable reports whether opaque is a thinking or redacted_thinking
// block, and which.
func replayable(opaque json.RawMessage) (string, bool) {
	var b struct {
		Type string `json:"type"`
	}
	if len(opaque) == 0 || json.Unmarshal(opaque, &b) != nil {
		return "", false
	}
	if b.Type != "thinking" && b.Type != "redacted_thinking" {
		return "", false
	}
	return b.Type, true
}

// toolInput is a tool call's arguments as the object the API requires. A
// call whose arguments did not parse (ArgsError) is replayed with {}, as
// the loop answered it.
func toolInput(p llm.Part) json.RawMessage {
	args := bytes.TrimSpace(p.Args)
	if p.ArgsError != "" || len(args) == 0 || args[0] != '{' || !json.Valid(args) {
		return json.RawMessage("{}")
	}
	return args
}

// wireID is id as the API takes a tool_use id, [a-zA-Z0-9_-]+: other
// providers' ids (Kimi's functions.name:0) have other characters. Calls
// and results are mapped alike, so they still match.
func wireID(id string) string {
	if id == "" {
		return "call"
	}
	b := []byte(id)
	for i, c := range b {
		if !idByte(c) {
			b[i] = '_'
		}
	}
	return string(b)
}

func idByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// tools declares the tools, the last one marked for caching.
func (a *Adapter) tools(tools []llm.Tool) ([]wireTool, error) {
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		schema := bytes.TrimSpace(t.Schema)
		switch {
		case len(schema) == 0:
			schema = []byte(`{"type":"object"}`)
		case schema[0] != '{' || !json.Valid(schema):
			return nil, badRequest("tool %q: the schema is not a JSON object", t.Name)
		}
		out = append(out, wireTool{Name: t.Name, Description: t.Description, InputSchema: schema, Strict: a.caps.StrictTools})
	}
	if n := len(out); n > 0 {
		out[n-1].CacheControl = ephemeral
	}
	return out, nil
}

func hasToolParts(msgs []llm.Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == llm.PartToolCall || p.Type == llm.PartToolResult {
				return true
			}
		}
	}
	return false
}

// marshal is json.Marshal without HTML escaping.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func badRequest(format string, args ...any) *llm.Error {
	return &llm.Error{Kind: llm.ErrBadRequest, Message: llm.Clip("anthropic: " + fmt.Sprintf(format, args...))}
}
