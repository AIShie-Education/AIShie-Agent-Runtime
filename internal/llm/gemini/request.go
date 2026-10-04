package gemini

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

// wireRequest is generateContent's request body, as far as the runtime
// uses it.
type wireRequest struct {
	Contents          []wireContent   `json:"contents"`
	SystemInstruction *wireContent    `json:"systemInstruction,omitempty"`
	Tools             []wireTool      `json:"tools,omitempty"`
	ToolConfig        *wireToolConfig `json:"toolConfig,omitempty"`
	GenerationConfig  *wireGeneration `json:"generationConfig,omitempty"`
}

// wireContent is one turn. Each part is a wirePart, or a json.RawMessage
// for a part replayed verbatim.
type wireContent struct {
	Role  string `json:"role,omitempty"`
	Parts []any  `json:"parts"`
}

// wirePart is a part the adapter writes itself.
type wirePart struct {
	// Text is a pointer so that an empty text part carrying a signature can
	// still be sent.
	Text             *string               `json:"text,omitempty"`
	InlineData       *wireBlob             `json:"inlineData,omitempty"`
	FunctionCall     *wireFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *wireFunctionResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string                `json:"thoughtSignature,omitempty"`
}

type wireBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type wireFunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type wireFunctionResponse struct {
	ID       string                     `json:"id,omitempty"`
	Name     string                     `json:"name"`
	Response map[string]json.RawMessage `json:"response"`
}

type wireTool struct {
	FunctionDeclarations []wireFunctionDeclaration `json:"functionDeclarations"`
}

type wireFunctionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	Parameters           json.RawMessage `json:"parameters,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type wireToolConfig struct {
	FunctionCallingConfig wireFunctionCallingConfig `json:"functionCallingConfig"`
}

type wireFunctionCallingConfig struct {
	Mode string `json:"mode"`
}

type wireGeneration struct {
	MaxOutputTokens int           `json:"maxOutputTokens,omitempty"`
	Temperature     *float64      `json:"temperature,omitempty"`
	TopP            *float64      `json:"topP,omitempty"`
	ThinkingConfig  *wireThinking `json:"thinkingConfig,omitempty"`
}

// wireThinking always says includeThoughts false: thought summaries are
// text the runtime would pay for and never show.
type wireThinking struct {
	IncludeThoughts bool   `json:"includeThoughts"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
}

// Function calling modes. The runtime uses only these two (§3.3): AUTO
// normally, NONE for ForceAnswer.
const (
	modeAuto = "AUTO"
	modeNone = "NONE"
)

// opaque is what the adapter keeps in the Opaque of a text or tool_call
// part it made. A reasoning part's Opaque is the Gemini part itself.
type opaque struct {
	// ThoughtSignature goes back on the same part, to the same maker.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	// ID is the functionCall id Gemini gave. It is empty when Gemini gave
	// none, so that an id Normalize made up is never sent.
	ID string `json:"id,omitempty"`
}

// callRef is what a tool result needs from its call: the tool's name, and
// the id Gemini gave the call, if any.
type callRef struct {
	name string
	id   string
}

// buildRequest translates the internal request.
//
// ForceAnswer (ToolNone) keeps the tools and says NONE, since gemini takes
// it (ToolChoiceNone). Where an agent's capabilities say otherwise the tools
// are left out, and then so is the tool history, flattened to text: whether
// generateContent takes functionCall parts with no tools declared is not
// documented, and plain text cannot break a call.
func (a *Adapter) buildRequest(req *llm.Request) (*wireRequest, error) {
	force := req.ToolMode == llm.ToolNone
	declare := len(req.Tools) > 0 && (!force || a.caps.ToolChoiceNone)
	msgs := req.Messages
	if !declare && hasToolHistory(msgs) {
		msgs = llm.FlattenToolHistory(msgs)
	}
	w := &wireRequest{Contents: a.contents(msgs)}
	if len(w.Contents) == 0 {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: "gemini: the request has no message to send"}
	}
	if req.System != "" {
		system := req.System
		w.SystemInstruction = &wireContent{Parts: []any{wirePart{Text: &system}}}
	}
	if declare {
		decls := make([]wireFunctionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, a.declaration(t))
		}
		w.Tools = []wireTool{{FunctionDeclarations: decls}}
		mode := modeAuto
		if force {
			mode = modeNone
		}
		w.ToolConfig = &wireToolConfig{FunctionCallingConfig: wireFunctionCallingConfig{Mode: mode}}
	}
	w.GenerationConfig = a.generation(req.Limits, req.LeastReasoning)
	return w, nil
}

// contents translates the messages into turns: user and tool messages are
// user turns, assistant messages model turns. A turn left with no parts is
// dropped, and two turns in a row by the same role are merged, since
// generateContent expects them to alternate.
func (a *Adapter) contents(msgs []llm.Message) []wireContent {
	var out []wireContent
	calls := map[string]callRef{}
	files := maxFileBytes
	for _, m := range msgs {
		role := "user"
		var parts []any
		if m.Role == llm.RoleAssistant {
			role = "model"
			// Results answer the calls of the assistant message just before
			// them; call_{n} ids repeat from one response to the next.
			calls = map[string]callRef{}
			parts = a.modelParts(m.Parts, calls)
		} else {
			parts = a.userParts(m.Parts, calls, &files)
		}
		if len(parts) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Parts = append(out[n-1].Parts, parts...)
			continue
		}
		out = append(out, wireContent{Role: role, Parts: parts})
	}
	return out
}

// modelParts translates an assistant message, recording its calls in calls.
// Signatures and reasoning go back only to the maker that made them; the
// parts of another maker go without. Results and files never ride in a
// model turn.
//
// A model that checks signatures (Gemini 3) refuses a step whose first
// functionCall has none, which is every step another model made: a
// fallback taken in the middle of a loop. That call carries the stand-in
// Google documents for a history from another model, skipSignature, and
// not another maker's signature.
func (a *Adapter) modelParts(parts []llm.Part, calls map[string]callRef) []any {
	var out []any
	first := true
	for _, p := range parts {
		switch p.Type {
		case llm.PartText:
			// generateContent refuses an empty text part, but one that
			// carries a signature goes back as it came: Google asks for
			// every signature back, and streamed answers end in such parts.
			sig := a.opaqueOf(p).ThoughtSignature
			if p.Text == "" && sig == "" {
				continue
			}
			text := p.Text
			out = append(out, wirePart{Text: &text, ThoughtSignature: sig})
		case llm.PartToolCall:
			o := a.opaqueOf(p)
			calls[p.ID] = callRef{name: p.Name, id: o.ID}
			if first && o.ThoughtSignature == "" && a.checksSignatures {
				o.ThoughtSignature = skipSignature
			}
			first = false
			out = append(out, wirePart{
				FunctionCall:     &wireFunctionCall{ID: o.ID, Name: p.Name, Args: argsObject(p.Args)},
				ThoughtSignature: o.ThoughtSignature,
			})
		case llm.PartReasoning:
			if p.Maker == a.maker && isObject(p.Opaque) {
				out = append(out, p.Opaque)
			}
		}
	}
	return out
}

// userParts translates a user or tool message. Results come first, as
// functionResponse parts in call order, then everything else in order: the
// files the results brought, and text.
//
// A functionResponse goes back in a user turn. The handout marks the role
// [UNVERIFIED]; it is the role Google's documentation and SDKs use.
func (a *Adapter) userParts(parts []llm.Part, calls map[string]callRef, files *int) []any {
	var responses, rest []any
	for _, p := range parts {
		switch p.Type {
		case llm.PartToolResult:
			responses = append(responses, functionResponse(p, calls))
		case llm.PartText:
			if p.Text != "" {
				text := p.Text
				rest = append(rest, wirePart{Text: &text})
			}
		case llm.PartFile:
			if p.File != nil {
				rest = append(rest, a.filePart(p.File, files))
			}
		}
	}
	return append(responses, rest...)
}

// functionResponse translates a tool result. Its response is an object, as
// generateContent requires: {"output": …} for a result and {"error": …} for
// an is_error one, the two keys Google documents for a function's output
// and its error. The value is the content itself when it is JSON (Core's
// envelope), else the content as a string. The id is sent only when Gemini
// gave the call one.
func functionResponse(p llm.Part, calls map[string]callRef) wirePart {
	ref := calls[p.CallID]
	name := p.Name
	if name == "" {
		name = ref.name
	}
	key := "output"
	if p.IsError {
		key = "error"
	}
	return wirePart{FunctionResponse: &wireFunctionResponse{
		ID:       ref.id,
		Name:     name,
		Response: map[string]json.RawMessage{key: resultValue(p.Content)},
	}}
}

// resultValue is a tool result's content as one JSON value: the JSON
// itself, or, for anything that does not parse (a result cut short, a
// message of the runtime's own), the text as a string.
func resultValue(content string) json.RawMessage {
	trimmed := strings.TrimSpace(content)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	b, _ := json.Marshal(content) // a string always encodes
	return b
}

// maxFileBytes bounds the files one request carries, counted as they are
// sent (inlineData in base64, text as it is). Google takes 100 MB of inline
// data in a request; the history, the tools and JSON's own weight need the
// rest.
const maxFileBytes = 64 << 20

// inlineTypes are the media types generateContent is documented to take as
// inlineData: PDF, the text types it reads as text, and its image, audio
// and video formats. It refuses any other type with a 400 that no retry
// mends (a Word document, a zip), so no other type is sent inline.
var inlineTypes = map[string]bool{
	"application/pdf": true,

	"text/plain": true, "text/html": true, "text/css": true, "text/csv": true,
	"text/xml": true, "text/rtf": true, "text/javascript": true,

	"image/png": true, "image/jpeg": true, "image/webp": true, "image/heic": true, "image/heif": true,

	"audio/wav": true, "audio/mp3": true, "audio/aiff": true, "audio/aac": true, "audio/ogg": true, "audio/flac": true,

	"video/mp4": true, "video/mpeg": true, "video/mov": true, "video/quicktime": true, "video/avi": true,
	"video/x-flv": true, "video/mpg": true, "video/webm": true, "video/wmv": true, "video/3gpp": true,
}

// filePart is a file as the model can take it (rule 6): inlineData where
// the model is given files and Gemini takes the type; the file's text,
// under its name, where it is text of another type (Markdown, JSON) or the
// model is not given files; and otherwise a line saying it was left out.
// Files are taken in order until the request's allowance is spent, so a
// file sent on one turn of a loop is sent on every later one.
func (a *Adapter) filePart(f *llm.File, files *int) wirePart {
	typ := mediaType(f)
	var why string
	switch {
	case a.caps.FileInput && inlineTypes[typ]:
		if n := base64.StdEncoding.EncodedLen(len(f.Data)); n <= *files {
			*files -= n
			return wirePart{InlineData: &wireBlob{MimeType: typ, Data: base64.StdEncoding.EncodeToString(f.Data)}}
		}
		why = "it is too large to send"
	case isText(typ) && utf8.Valid(f.Data):
		if len(f.Data) <= *files {
			*files -= len(f.Data)
			text := fmt.Sprintf("[file %q]\n%s", f.Name, f.Data)
			return wirePart{Text: &text}
		}
		why = "it is too large to send"
	case !a.caps.FileInput:
		why = "this model is not given files"
	case typ == "":
		why = "its type is not known"
	default:
		why = "Gemini does not take files of its type"
	}
	note := fmt.Sprintf("[file %q left out: %s]", f.Name, why)
	return wirePart{Text: &note}
}

// isText reports whether a media type is text a model can read as it is.
func isText(typ string) bool {
	switch {
	case strings.HasPrefix(typ, "text/"), strings.HasSuffix(typ, "+json"), strings.HasSuffix(typ, "+xml"):
		return true
	}
	switch typ {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/javascript":
		return true
	}
	return false
}

// mediaType is a file's media type without parameters, in lower case, from
// its MIME or else from its name's extension; "" when neither says more
// than application/octet-stream. image/jpg, which some servers send, is
// image/jpeg.
func mediaType(f *llm.File) string {
	for _, t := range []string{f.MIME, mime.TypeByExtension(path.Ext(f.Name))} {
		mt, _, err := mime.ParseMediaType(t)
		if err != nil || mt == "application/octet-stream" {
			continue
		}
		if mt == "image/jpg" {
			return "image/jpeg"
		}
		return mt
	}
	return ""
}

// declaration is a tool's functionDeclaration: its schema, already
// sanitised for the dialect, as parameters (OpenAPI 3.0) or as
// parametersJsonSchema (every JSON Schema dialect).
func (a *Adapter) declaration(t llm.Tool) wireFunctionDeclaration {
	d := wireFunctionDeclaration{Name: t.Name, Description: t.Description}
	if !takesArguments(t.Schema) {
		return d
	}
	if a.dialect == toolschema.GeminiOpenAPI {
		d.Parameters = t.Schema
	} else {
		d.ParametersJSONSchema = t.Schema
	}
	return d
}

// takesArguments reports whether a tool's schema declares any argument. A
// tool that declares none (course_get, once course_id is bound) is sent with
// no schema: generateContent refuses an OBJECT with empty properties in
// parameters, and whether parametersJsonSchema takes one is not documented.
// No schema means the same, a call with no arguments, and cannot break a
// call.
func takesArguments(schema json.RawMessage) bool {
	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return false
	}
	var s map[string]json.RawMessage
	if json.Unmarshal(trimmed, &s) != nil {
		return true // not an object: sent as it is, for the provider to judge
	}
	for k, v := range s {
		v = bytes.TrimSpace(v)
		switch k {
		case "description", "title", "$schema":
		case "type":
			if string(v) != `"object"` {
				return true
			}
		case "properties":
			var props map[string]json.RawMessage
			if json.Unmarshal(v, &props) != nil || len(props) > 0 {
				return true
			}
		case "additionalProperties":
			if string(v) != "false" {
				return true
			}
		case "required":
			var req []string
			if json.Unmarshal(v, &req) != nil || len(req) > 0 {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// generation is generationConfig, or nil when there is nothing to say. The
// call's own cap wins over the configured one: the loop raises it to retry
// a max_tokens stop. maxOutputTokens counts thinking too, so a model that
// thinks is given the cap and its thinking allowance, as the anthropic
// adapter gives max_tokens; the tokens it spends are counted in Usage.Output
// all the same.
//
// A call asking for the least reasoning (least: ForceAnswer's, a
// continuation's) is made at low in place of medium or high
// (llm.LeastEffort), and a model that thinks unasked is asked at low as if
// low were configured: a budget of 1,024, which every 2.5 model that
// thinks takes, or LOW, which every Gemini 3 model takes.
func (a *Adapter) generation(l llm.Limits, least bool) *wireGeneration {
	effort := a.effort
	if least {
		effort = llm.LeastEffort(effort)
		if effort == "" && ThinksUnasked(a.model) {
			effort = "low"
		}
	}
	g := wireGeneration{Temperature: a.params.Temperature, TopP: a.params.TopP, ThinkingConfig: thinking(a.model, effort)}
	limit := a.params.MaxOutputTokens
	if l.MaxOutputTokens > 0 {
		limit = l.MaxOutputTokens
	}
	if limit > 0 {
		g.MaxOutputTokens = outputCap(limit, thinkingAllowance(a.model, effort))
	}
	if g == (wireGeneration{}) {
		return nil
	}
	return &g
}

// opaqueOf reads what the adapter kept on a part, if this adapter made it:
// another maker's signature or id means nothing here.
func (a *Adapter) opaqueOf(p llm.Part) opaque {
	var o opaque
	if p.Maker != a.maker || len(p.Opaque) == 0 {
		return o
	}
	if json.Unmarshal(p.Opaque, &o) != nil {
		return opaque{}
	}
	return o
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

// argsObject is a call's arguments as sent back: the object, or {} for
// arguments that never parsed (the call was answered with an error).
func argsObject(args json.RawMessage) json.RawMessage {
	if isObject(args) {
		return args
	}
	return json.RawMessage("{}")
}

// isObject reports whether raw is one JSON object.
func isObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}
