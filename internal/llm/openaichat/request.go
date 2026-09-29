package openaichat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

// chatRequest is the body of POST /chat/completions.
type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          string          `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	Reasoning           *reasoningParam `json:"reasoning,omitempty"`
	Store               *bool           `json:"store,omitempty"`
	Stream              bool            `json:"stream"`
	// StreamOptions asks a streamed answer's last chunk to carry the
	// call's usage (stream.go).
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type reasoningParam struct {
	Effort string `json:"effort"`
}

// chatMessage is one message. Content is a string, a []contentPart, or nil
// for an assistant message that only calls tools. Extra holds fields a
// provider asked to have back (reasoning_content, reasoning_details), put
// beside the others as they came.
type chatMessage struct {
	Role       string                     `json:"role"`
	Content    any                        `json:"content"`
	ToolCalls  []chatToolCall             `json:"tool_calls,omitempty"`
	ToolCallID string                     `json:"tool_call_id,omitempty"`
	Extra      map[string]json.RawMessage `json:"-"`
}

func (m chatMessage) MarshalJSON() ([]byte, error) {
	type plain chatMessage
	return withExtra(plain(m), m.Extra)
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	File     *fileData `json:"file,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type fileData struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

type imageURL struct {
	URL string `json:"url"`
}

// chatToolCall is a call in an assistant message. Extra carries what rode
// on it (Gemini's extra_content with its thought signature).
type chatToolCall struct {
	ID       string                     `json:"id"`
	Type     string                     `json:"type"`
	Function chatFunction               `json:"function"`
	Extra    map[string]json.RawMessage `json:"-"`
}

func (c chatToolCall) MarshalJSON() ([]byte, error) {
	type plain chatToolCall
	return withExtra(plain(c), c.Extra)
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string          `json:"type"`
	Function chatDeclaration `json:"function"`
}

type chatDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict,omitempty"`
}

// withExtra marshals v and adds extra's fields beside its own, each value
// as it came: the encoder may write a string's escapes its own way, never
// its value. A field v already has wins: a fragment cannot replace a
// message's role or content.
func withExtra(v any, extra map[string]json.RawMessage) ([]byte, error) {
	b, err := marshal(v)
	if err != nil || len(extra) == 0 {
		return b, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	for k, raw := range extra {
		if _, taken := fields[k]; !taken && json.Valid(raw) {
			fields[k] = raw
		}
	}
	return marshalObject(fields)
}

// marshalObject writes fields with sorted keys, each value's bytes as they
// are.
func marshalObject(fields map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, err := marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(name)
		b.WriteByte(':')
		b.Write(fields[k])
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// request translates req into the provider's body.
func (a *Adapter) request(req *llm.Request) *chatRequest {
	out := &chatRequest{Model: a.model}

	// ForceAnswer (§3.3): tool_choice none where the provider takes it;
	// elsewhere the tools are left out. A server that does not take
	// tool_choice is most often a local one, whose chat template breaks on
	// tool messages when no tools are declared, so the history's calls and
	// results are then given as text.
	sendTools := len(req.Tools) > 0 && (req.ToolMode != llm.ToolNone || a.caps.ToolChoiceNone)
	msgs := req.Messages
	if !sendTools && !a.caps.ToolChoiceNone && hasToolHistory(msgs) {
		msgs = llm.FlattenToolHistory(msgs)
	}

	if req.System != "" {
		out.Messages = append(out.Messages, chatMessage{Role: a.systemRole(), Content: req.System})
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, a.messages(m)...)
	}

	if sendTools {
		out.Tools = a.tools(req.Tools)
		// Only none is ever sent: auto is the default wherever tools are
		// declared, and required or a named tool is refused by several
		// providers (§3.3). Ollama takes no tool_choice at all.
		if req.ToolMode == llm.ToolNone {
			out.ToolChoice = "none"
		}
		out.ParallelToolCalls = a.parallelToolCalls()
	}

	maxOut := req.Limits.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = a.params.MaxOutputTokens
	}
	if maxOut > 0 {
		// OpenAI deprecated max_tokens, and its reasoning models refuse it;
		// every other server knows only max_tokens.
		if isOpenAI(a.provider) {
			out.MaxCompletionTokens = maxOut
		} else {
			out.MaxTokens = maxOut
		}
	}

	// OpenAI's reasoning models refuse any temperature or top_p but the
	// default with a 400, so a sampling setting meant for another model
	// would stop every answer; it is left out for them instead.
	if !isOpenAI(a.provider) || !reasoningModel(a.model) {
		out.Temperature = a.params.Temperature
		out.TopP = a.params.TopP
	}

	// Reasoning effort has a field only on OpenAI and Azure, and on
	// OpenRouter, which maps it to each upstream. DeepSeek, Kimi, GLM and
	// Qwen choose thinking by model or by parameters of their own that an
	// effort does not translate to; what Gemini's compatible endpoint makes
	// of an effort differs by model family, which the gemini adapter maps
	// itself; a local server would ignore it or, if strict, refuse the
	// call. So nothing is sent elsewhere. OpenAI refuses the field from a
	// model that does not reason, so an effort configured for one is left
	// out rather than stop every answer; an Azure deployment's name need
	// not name its model, so Azure gets the effort its operator configured.
	if a.effort != "" {
		switch {
		case a.provider == llm.ProviderAzure, a.provider == llm.ProviderOpenAI && reasoningModel(a.model):
			out.ReasoningEffort = a.effort
		case a.provider == llm.ProviderOpenRouter:
			out.Reasoning = &reasoningParam{Effort: a.effort}
		}
	}

	// A school's data is not kept by OpenAI (§5.2). false is already the
	// default for Chat Completions; it is said anyway, so that a change of
	// default cannot start storing it.
	if a.provider == llm.ProviderOpenAI {
		f := false
		out.Store = &f
	}
	return out
}

// systemRole is developer for OpenAI's reasoning models and the GPT-5
// family, which take their instructions under that role, and system
// everywhere else. An Azure deployment is matched by its name, which
// usually names its model; one that does not gets system, which Azure
// still accepts.
func (a *Adapter) systemRole() string {
	if isOpenAI(a.provider) && developerRole(a.model) {
		return "developer"
	}
	return "system"
}

// developerRole reports whether model is an o-series reasoning model (o1,
// o3, o4-mini, …) or of the GPT-5 family.
func developerRole(model string) bool {
	m := family(model)
	return oSeries(m) || strings.HasPrefix(m, "gpt-5")
}

// reasoningModel reports whether model reasons: it then takes a
// reasoning_effort, and only the default temperature and top_p. GPT-5's
// chat variants do not reason.
func reasoningModel(model string) bool {
	m := family(model)
	return oSeries(m) || (strings.HasPrefix(m, "gpt-5") && !strings.Contains(m, "-chat"))
}

// family is a model id as its family names it: lower case, without the
// ft: of a fine-tuned model (ft:gpt-4.1-mini:org::id), which behaves as its
// base model does.
func family(model string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "ft:")
}

func oSeries(m string) bool {
	return len(m) >= 2 && m[0] == 'o' && m[1] >= '1' && m[1] <= '9'
}

// parallelToolCalls is sent only to the APIs that document it, and only
// when it differs from their default: true for OpenAI, Azure and
// OpenRouter, false for Qwen. Leaving a default unsaid changes nothing, and
// some of OpenAI's reasoning models have refused the parameter outright.
func (a *Adapter) parallelToolCalls() *bool {
	v := a.caps.ParallelToolCalls
	switch a.provider {
	case llm.ProviderOpenAI, llm.ProviderAzure, llm.ProviderOpenRouter:
		if !v {
			return &v
		}
	case llm.ProviderQwen:
		if v {
			return &v
		}
	}
	return nil
}

// tools declares each tool as a function. strict is said only for the
// dialects whose schemas are shaped for it: openai_strict, and Kimi's,
// where strict is the default anyway.
func (a *Adapter) tools(tools []llm.Tool) []chatTool {
	strict := a.dialect == toolschema.OpenAIStrict || a.dialect == toolschema.Kimi
	out := make([]chatTool, 0, len(tools))
	for _, t := range tools {
		params := t.Schema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, chatTool{Type: "function", Function: chatDeclaration{
			Name: t.Name, Description: t.Description, Parameters: params, Strict: strict,
		}})
	}
	return out
}

// messages translates one internal message; a tool message becomes one
// message per result, and perhaps a user message for its files.
func (a *Adapter) messages(m llm.Message) []chatMessage {
	switch m.Role {
	case llm.RoleUser:
		if content, ok := a.content(m.Parts); ok {
			return []chatMessage{{Role: "user", Content: content}}
		}
	case llm.RoleAssistant:
		if msg, ok := a.assistant(m.Parts); ok {
			return []chatMessage{msg}
		}
	case llm.RoleTool:
		return a.toolResults(m.Parts)
	}
	return nil
}

// assistant is an assistant message: its text, its calls, and the
// reasoning this adapter made for it. Reasoning another maker wrote is
// dropped (rule 4): no other endpoint could read it, and some refuse it.
func (a *Adapter) assistant(parts []llm.Part) (chatMessage, bool) {
	var texts []string
	var calls []chatToolCall
	var extra map[string]json.RawMessage
	for _, p := range parts {
		switch p.Type {
		case llm.PartText:
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		case llm.PartToolCall:
			call := chatToolCall{ID: p.ID, Type: "function", Function: chatFunction{Name: p.Name, Arguments: a.arguments(p)}}
			if p.Maker == a.maker {
				call.Extra = fragment(p.Opaque, callFields)
			}
			calls = append(calls, call)
		case llm.PartReasoning:
			if p.Maker == a.maker {
				extra = mergeFragments(extra, fragment(p.Opaque, reasoningFields))
			}
		}
	}
	msg := chatMessage{Role: "assistant", ToolCalls: calls, Extra: extra}
	switch {
	case len(texts) > 0:
		msg.Content = strings.Join(texts, "\n\n")
	case len(calls) > 0:
		msg.Content = nil
	case len(extra) > 0:
		msg.Content = ""
	default:
		return chatMessage{}, false
	}
	return msg, true
}

// arguments is a call's arguments as the string the API carries. Arguments
// that did not parse go back to OpenAI and Azure as the model wrote them,
// since they keep arguments as an opaque string, so that the model sees its
// own mistake beside the error it caused. Everywhere else they go back as
// {}: Ollama parses the arguments of the history and refuses the whole
// request when they do not parse (as vLLM did before it learnt to coerce
// them), and the endpoints that translate to another API (Gemini's,
// OpenRouter's) need an object there. The tool result says what was wrong.
func (a *Adapter) arguments(p llm.Part) string {
	switch {
	case p.ArgsError != "" && isOpenAI(a.provider):
		return p.ArgsError
	case p.ArgsError != "", len(p.Args) == 0:
		return "{}"
	}
	return string(p.Args)
}

// toolResults is one tool message per result, in order, then one user
// message with whatever else the tool message held (files, text). The
// results come first because the API wants every call answered straight
// after the assistant message that made it.
func (a *Adapter) toolResults(parts []llm.Part) []chatMessage {
	var out []chatMessage
	var rest []llm.Part
	for _, p := range parts {
		switch p.Type {
		case llm.PartToolResult:
			out = append(out, chatMessage{Role: "tool", ToolCallID: p.CallID, Content: p.Content})
		case llm.PartText, llm.PartFile:
			rest = append(rest, p)
		}
	}
	if content, ok := a.content(rest); ok {
		out = append(out, chatMessage{Role: "user", Content: content})
	}
	return out
}

// content is a user message's content: a string when it is all text, the
// most widely taken form, and a list of parts when it holds a file.
func (a *Adapter) content(parts []llm.Part) (any, bool) {
	var out []contentPart
	for _, p := range parts {
		switch p.Type {
		case llm.PartText:
			if p.Text != "" {
				out = append(out, contentPart{Type: "text", Text: p.Text})
			}
		case llm.PartFile:
			out = append(out, a.fileParts(p.File)...)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	texts := make([]string, 0, len(out))
	for _, c := range out {
		if c.Type != "text" {
			return out, true
		}
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, "\n\n"), true
}

// fileParts gives a file to the model (rule 6): a PDF as a file part and an
// image as an image_url, each as a data URL and labelled with its name; a
// text file as its text. Anything the model cannot take, or everything when
// the model takes no files, becomes a note saying so, so that the model
// does not answer as if it had read it.
func (a *Adapter) fileParts(f *llm.File) []contentPart {
	if f == nil {
		return nil
	}
	name := f.Name
	if name == "" {
		name = "file"
	}
	mime := strings.ToLower(strings.TrimSpace(f.MIME))
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if isText(mime) && utf8.Valid(f.Data) {
		return []contentPart{{Type: "text", Text: fmt.Sprintf("[file: %s]\n%s", name, f.Data)}}
	}
	if a.caps.FileInput && (mime == "application/pdf" || isImage(mime)) {
		label := contentPart{Type: "text", Text: fmt.Sprintf("[file: %s]", name)}
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(f.Data)
		if mime == "application/pdf" {
			return []contentPart{label, {Type: "file", File: &fileData{Filename: name, FileData: dataURL}}}
		}
		return []contentPart{label, {Type: "image_url", ImageURL: &imageURL{URL: dataURL}}}
	}
	return []contentPart{{Type: "text", Text: fmt.Sprintf("[The file %q (%s) could not be given to you here.]", name, mime)}}
}

func isText(mime string) bool {
	return strings.HasPrefix(mime, "text/") || mime == "application/json"
}

// isImage reports the image types OpenAI's vision input takes.
func isImage(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	}
	return false
}

// hasToolHistory reports whether any message holds a call or a result.
func hasToolHistory(msgs []llm.Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == llm.PartToolCall || p.Type == llm.PartToolResult {
				return true
			}
		}
	}
	return false
}

// The wire fields an Opaque may put back: on an assistant message, the
// reasoning of §3.6; on a tool call, what Gemini rode on it. They are the
// only fields this adapter writes into an Opaque, and the only ones read
// back, so that a fragment can never add tool calls, a name or anything
// else to a message.
var (
	reasoningFields = []string{"reasoning_content", "reasoning_details"}
	callFields      = []string{"extra_content"}
)

// fragment reads an Opaque this adapter wrote: a JSON object of the wire
// fields to put back on the object they came on, of which only allowed are
// kept. Anything else is ignored.
func fragment(opaque json.RawMessage, allowed []string) map[string]json.RawMessage {
	if len(opaque) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(opaque, &fields) != nil {
		return nil
	}
	kept := make(map[string]json.RawMessage, len(allowed))
	for _, k := range allowed {
		if v, ok := fields[k]; ok {
			kept[k] = v
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// mergeFragments adds b's fields to a. Two reasoning parts on one message
// (which one response never makes) keep both: strings are joined, lists
// appended; otherwise the first wins.
func mergeFragments(a, b map[string]json.RawMessage) map[string]json.RawMessage {
	if len(b) == 0 {
		return a
	}
	if a == nil {
		a = make(map[string]json.RawMessage, len(b))
	}
	for k, v := range b {
		old, ok := a[k]
		if !ok {
			a[k] = v
			continue
		}
		var s1, s2 string
		if json.Unmarshal(old, &s1) == nil && json.Unmarshal(v, &s2) == nil {
			if joined, err := marshal(s1 + s2); err == nil {
				a[k] = joined
			}
			continue
		}
		var l1, l2 []json.RawMessage
		if json.Unmarshal(old, &l1) == nil && json.Unmarshal(v, &l2) == nil {
			if joined, err := marshal(append(l1, l2...)); err == nil {
				a[k] = joined
			}
		}
	}
	return a
}
