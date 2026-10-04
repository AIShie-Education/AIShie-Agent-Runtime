package openairesponses

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

// minOutputTokens is the smallest max_output_tokens the API takes; a
// smaller one is refused with a 400, so it is raised to this.
const minOutputTokens = 16

// includeEncryptedReasoning asks for reasoning items to carry their content,
// encrypted, so that they can be replayed to a store that kept nothing.
const includeEncryptedReasoning = "reasoning.encrypted_content"

// wireRequest is the body of POST /responses.
type wireRequest struct {
	Model string `json:"model"`
	// Instructions is the system prompt. The API does not carry it from one
	// response to the next, so it is sent on every turn.
	Instructions      string            `json:"instructions,omitempty"`
	Input             []json.RawMessage `json:"input"`
	Tools             []wireTool        `json:"tools,omitempty"`
	ToolChoice        string            `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens   int               `json:"max_output_tokens,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	Reasoning         *wireReasoning    `json:"reasoning,omitempty"`
	Include           []string          `json:"include,omitempty"`
	// Store is always false: see the package's doc. The API stores by
	// default, so it is never left out.
	Store bool `json:"store"`
}

type wireReasoning struct {
	Effort string `json:"effort"`
}

// wireTool is a function tool. Strict is always sent: the API tries strict
// validation when it is left out, and Core's schemas are not made for it
// unless the dialect is openai_strict.
type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

// Input items and their content.
type (
	messageItem struct {
		Type string `json:"type"`
		// ID is an assistant message's msg_… item id, sent only to the
		// model that made it.
		ID      string `json:"id,omitempty"`
		Role    string `json:"role"`
		Content []any  `json:"content"`
		// Phase labels an assistant message commentary or final_answer.
		// Newer models do worse when it is dropped from their own messages.
		Phase string `json:"phase,omitempty"`
	}
	functionCallItem struct {
		Type string `json:"type"`
		// ID is the fc_… item id, sent only to the model that made it.
		ID        string `json:"id,omitempty"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	functionCallOutputItem struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	textContent struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	imageContent struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
		Detail   string `json:"detail"`
	}
	fileContent struct {
		Type     string `json:"type"`
		Filename string `json:"filename"`
		FileData string `json:"file_data"`
	}
)

// opaqueItem is what the adapter keeps in a part's Opaque besides a whole
// reasoning item: the item id of the message or function call the part
// came in, and a message's phase.
type opaqueItem struct {
	ID    string `json:"id,omitempty"`
	Phase string `json:"phase,omitempty"`
}

// encode builds the body of one call.
func (a *Adapter) encode(req *llm.Request) ([]byte, error) {
	tools := req.Tools
	if req.ToolMode == llm.ToolNone && !a.caps.ToolChoiceNone {
		tools = nil
	}
	msgs := req.Messages
	// Whether the API takes tool calls in a history that declares no tools
	// is not documented; as text they cannot be refused, and the model
	// still sees what it looked up.
	if len(tools) == 0 && hasToolHistory(msgs) {
		msgs = llm.FlattenToolHistory(msgs)
	}
	input, err := a.input(msgs)
	if err != nil {
		return nil, err
	}

	w := wireRequest{
		Model:        a.model,
		Instructions: req.System,
		Input:        input,
		Temperature:  a.params.Temperature,
		TopP:         a.params.TopP,
		Store:        false,
	}
	if len(tools) > 0 {
		w.Tools = a.tools(tools)
		w.ToolChoice = "auto"
		if req.ToolMode == llm.ToolNone {
			w.ToolChoice = "none"
		}
		parallel := a.caps.ParallelToolCalls
		w.ParallelToolCalls = &parallel
	}
	w.MaxOutputTokens = outputCap(req.Limits.MaxOutputTokens, a.params.MaxOutputTokens)
	if a.effort != "" {
		// A call asking for the least reasoning (ForceAnswer's, a
		// continuation's) is made at low in place of medium or high.
		effort := a.effort
		if req.LeastReasoning {
			effort = llm.LeastEffort(effort)
		}
		w.Reasoning = &wireReasoning{Effort: effort}
		w.Include = []string{includeEncryptedReasoning}
	}
	body, err := json.Marshal(w)
	if err != nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: llm.Clip("encoding the request: " + err.Error())}
	}
	return body, nil
}

// outputCap is the call's cap, else the configured one, raised to the
// API's minimum; 0 leaves it to the API.
func outputCap(limit, configured int) int {
	n := limit
	if n <= 0 {
		n = configured
	}
	if n <= 0 {
		return 0
	}
	return max(n, minOutputTokens)
}

// hasToolHistory reports whether any message holds a tool call or result.
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

// tools declares the tools as functions. A tool without a schema takes no
// arguments: the API needs a schema all the same.
func (a *Adapter) tools(tools []llm.Tool) []wireTool {
	strict := a.dialect == toolschema.OpenAIStrict
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		params := t.Schema
		if s := bytes.TrimSpace(params); len(s) == 0 || bytes.Equal(s, []byte("null")) {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, wireTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: params, Strict: strict})
	}
	return out
}

// input translates the history into input items, in order.
func (a *Adapter) input(msgs []llm.Message) ([]json.RawMessage, error) {
	items := make([]any, 0, len(msgs))
	for i, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			if content := a.userContent(m.Parts); len(content) > 0 {
				items = append(items, messageItem{Type: "message", Role: "user", Content: content})
			}
		case llm.RoleAssistant:
			items = append(items, a.assistantItems(m.Parts)...)
		case llm.RoleTool:
			items = append(items, a.toolItems(m.Parts)...)
		default:
			return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: fmt.Sprintf("message %d has role %q, which the adapter does not know", i, m.Role)}
		}
	}
	out := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		if raw, ok := it.(json.RawMessage); ok {
			out = append(out, raw)
			continue
		}
		raw, err := json.Marshal(it)
		if err != nil {
			return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: llm.Clip("encoding the input: " + err.Error())}
		}
		out = append(out, raw)
	}
	return out, nil
}

// userContent is a user message's text and files.
func (a *Adapter) userContent(parts []llm.Part) []any {
	var content []any
	for _, p := range parts {
		switch p.Type {
		case llm.PartText:
			if p.Text != "" {
				content = append(content, textContent{Type: "input_text", Text: p.Text})
			}
		case llm.PartFile:
			if p.File != nil {
				content = append(content, a.fileContent(p.File))
			}
		}
	}
	return content
}

// assistantItems replays one assistant turn: its reasoning (to its maker
// only), its text as assistant messages, its calls as function_call items.
//
// To its maker the turn goes back as the API gave it, which is how OpenAI
// documents a loop that stores nothing: the reasoning items with their
// encrypted_content, then the message and function_call items with their
// ids. The API ties an item to the reasoning before it by id, and refuses
// an item whose reasoning is missing, so when this model's reasoning cannot
// go back (it came without encrypted_content) the turn's ids stay behind
// too: call_id alone links a call to its output. The API also refuses a
// reasoning item that nothing follows, so one at the end of the turn is
// left out.
func (a *Adapter) assistantItems(parts []llm.Part) []any {
	last := -1 // the last part that is an item of its own
	for i, p := range parts {
		if (p.Type == llm.PartText && p.Text != "") || p.Type == llm.PartToolCall {
			last = i
		}
	}
	keepIDs := true
	for i, p := range parts {
		if i < last && p.Type == llm.PartReasoning && p.Maker == a.maker && replayableReasoning(p.Opaque) == nil {
			keepIDs = false
		}
	}

	var items []any
	var msg *messageItem
	flush := func() {
		if msg != nil && len(msg.Content) > 0 {
			items = append(items, *msg)
		}
		msg = nil
	}
	for i, p := range parts {
		switch p.Type {
		case llm.PartReasoning:
			if i > last || p.Maker != a.maker {
				continue
			}
			if raw := replayableReasoning(p.Opaque); raw != nil {
				flush()
				items = append(items, raw)
			}
		case llm.PartText:
			if p.Text == "" {
				continue
			}
			o := a.opaque(p)
			if !keepIDs {
				o.ID = ""
			}
			if msg == nil || msg.ID != o.ID || msg.Phase != o.Phase {
				flush()
				msg = &messageItem{Type: "message", ID: o.ID, Role: "assistant", Phase: o.Phase}
			}
			msg.Content = append(msg.Content, textContent{Type: "output_text", Text: p.Text})
		case llm.PartToolCall:
			flush()
			call := functionCallItem{Type: "function_call", CallID: p.ID, Name: p.Name, Arguments: arguments(p)}
			if keepIDs {
				call.ID = a.opaque(p).ID
			}
			items = append(items, call)
		}
	}
	flush()
	return items
}

// toolItems is one function_call_output per result, in call order, then
// the files and any text as one user message: function_call_output takes a
// string, and the API takes files only in a message.
func (a *Adapter) toolItems(parts []llm.Part) []any {
	var items, extra []any
	for _, p := range parts {
		switch p.Type {
		case llm.PartToolResult:
			// There is no error flag: Core's envelope begins with its
			// status and speaks for itself (§3.2).
			items = append(items, functionCallOutputItem{Type: "function_call_output", CallID: p.CallID, Output: p.Content})
		case llm.PartFile:
			if p.File != nil {
				extra = append(extra, a.fileContent(p.File))
			}
		case llm.PartText:
			if p.Text != "" {
				extra = append(extra, textContent{Type: "input_text", Text: p.Text})
			}
		}
	}
	if len(extra) > 0 {
		items = append(items, messageItem{Type: "message", Role: "user", Content: extra})
	}
	return items
}

// fileContent gives the model a file (rule 6). Text is given as text,
// which every model reads. When the model takes files, an image goes as
// input_image and a PDF as input_file, each as a data URL. Anything else,
// or any file for a model that takes none, is a note that the file was
// left out: input_file is documented for PDFs, and a type the API refuses
// would cost the whole call a 400.
func (a *Adapter) fileContent(f *llm.File) any {
	mt := mediaType(f.MIME)
	switch {
	case isText(mt) && utf8.Valid(f.Data):
		return textContent{Type: "input_text", Text: "[" + fileLabel(f) + "]\n" + string(f.Data)}
	case !a.caps.FileInput:
		return fileNote(f, "this model cannot read files")
	case isImage(mt):
		return imageContent{Type: "input_image", ImageURL: dataURL(mt, f.Data), Detail: "auto"}
	case mt == "application/pdf":
		name := f.Name
		if name == "" {
			name = "file.pdf"
		}
		return fileContent{Type: "input_file", Filename: name, FileData: dataURL(mt, f.Data)}
	}
	return fileNote(f, "this model cannot read a file of type "+mt)
}

func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// fileNote stands in for a file the model is not given.
func fileNote(f *llm.File, why string) textContent {
	return textContent{Type: "input_text", Text: "[" + fileLabel(f) + " is left out: " + why + "]"}
}

func fileLabel(f *llm.File) string {
	if f.Name == "" {
		return "a file"
	}
	return "the file " + strconv.Quote(f.Name)
}

// mediaType is the MIME type without parameters, lower case, with the
// image/jpg some servers send read as image/jpeg;
// application/octet-stream when there is none.
func mediaType(s string) string {
	t, _, err := mime.ParseMediaType(s)
	if err != nil || t == "" {
		return "application/octet-stream"
	}
	if t == "image/jpg" {
		return "image/jpeg"
	}
	return t
}

// isImage reports whether the API takes the type as an image input: PNG,
// JPEG, WEBP and GIF.
func isImage(t string) bool {
	switch t {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	}
	return false
}

// isText reports whether a file of the type is text the model can read as
// it is.
func isText(t string) bool {
	return strings.HasPrefix(t, "text/") || t == "application/json"
}

// arguments is a call's arguments as the model wrote them: its raw text
// when they did not parse, so that it sees its own mistake beside the error.
func arguments(p llm.Part) string {
	switch {
	case p.ArgsError != "":
		return p.ArgsError
	case len(bytes.TrimSpace(p.Args)) == 0:
		return "{}"
	}
	return string(p.Args)
}

// opaque is what this adapter kept on a text or tool_call part it made; the
// zero value for a part another maker made.
func (a *Adapter) opaque(p llm.Part) opaqueItem {
	var o opaqueItem
	if p.Maker != a.maker || len(p.Opaque) == 0 {
		return o
	}
	if err := json.Unmarshal(p.Opaque, &o); err != nil {
		return opaqueItem{}
	}
	return o
}

// replayableReasoning is a reasoning item, verbatim and compacted, when it
// can be sent back: a JSON object of type reasoning with encrypted_content.
// Without it the API would look the item up by id in a store that kept
// nothing, and refuse the call.
func replayableReasoning(raw json.RawMessage) json.RawMessage {
	var item struct {
		Type             string `json:"type"`
		EncryptedContent string `json:"encrypted_content"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &item) != nil {
		return nil
	}
	if item.Type != "reasoning" || strings.TrimSpace(item.EncryptedContent) == "" {
		return nil
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return nil
	}
	return b.Bytes()
}
