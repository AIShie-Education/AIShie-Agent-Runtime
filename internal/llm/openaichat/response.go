package openaichat

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/httpx"
)

// chatResponse is a completed call's body. Only what the runtime reads is
// declared; usage is kept whole, as it came.
type chatResponse struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices []chatChoice    `json:"choices"`
	Usage   json.RawMessage `json:"usage"`
	// Error is set by the compatible servers that answer a failure with a
	// 200.
	Error json.RawMessage `json:"error"`
}

type chatChoice struct {
	Message      wireMessage `json:"message"`
	FinishReason *string     `json:"finish_reason"`
	// NativeFinishReason is OpenRouter's: the upstream provider's own.
	NativeFinishReason *string `json:"native_finish_reason"`
}

type wireMessage struct {
	// Content is a string, null, or a list of parts, which some servers
	// send.
	Content json.RawMessage `json:"content"`
	Refusal *string         `json:"refusal"`
	// ToolCalls, and FunctionCall before them.
	ToolCalls    []wireToolCall `json:"tool_calls"`
	FunctionCall *wireFunction  `json:"function_call"`
	// ReasoningContent is DeepSeek's, Kimi's and GLM's; ReasoningDetails is
	// OpenRouter's; Reasoning is OpenRouter's and vLLM's readable text.
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	ReasoningDetails json.RawMessage `json:"reasoning_details"`
	Reasoning        json.RawMessage `json:"reasoning"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Function wireFunction `json:"function"`
	// ExtraContent is where Gemini's compatible endpoint puts a call's
	// thought signature, which must come back on the same call.
	ExtraContent json.RawMessage `json:"extra_content"`
}

type wireFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON string by the API's definition; a few servers
	// send the object itself.
	Arguments json.RawMessage `json:"arguments"`
}

// response reads a 200's body into the internal format.
func (a *Adapter) response(resp *httpx.Response, req *llm.Request) (*llm.Response, error) {
	var w chatResponse
	if err := json.Unmarshal(resp.Body, &w); err != nil {
		e := httpx.DecodeError(err)
		e.Status = resp.Status
		return nil, e
	}
	if len(w.Choices) == 0 {
		if present(w.Error) {
			return nil, errorIn200(resp)
		}
		return nil, &llm.Error{Kind: llm.ErrServer, Status: resp.Status, Message: "openaichat: the response has no choices"}
	}
	c := w.Choices[0]
	out := &llm.Response{Model: w.Model, RequestID: w.ID}
	if out.RequestID == "" {
		out.RequestID = resp.Header.Get("X-Request-Id")
	}
	finish := deref(c.FinishReason)
	out.RawStop = finish
	if native := deref(c.NativeFinishReason); native != "" {
		out.RawStop += "/" + native
	}

	text, refusal := content(c.Message.Content)
	if c.Message.Refusal != nil && *c.Message.Refusal != "" {
		refusal = *c.Message.Refusal
	}
	if refusal != "" {
		// A refusal is not an answer: its words are the provider's, and the
		// loop posts the configured refusal text instead.
		out.Stop = llm.StopRefusal
	} else {
		out.Parts = a.parts(c.Message, text)
		out.Stop = stop(finish, strings.TrimSpace(text) != "", len(out.ToolCalls()) > 0)
	}
	out.Usage = a.usage(w.Usage, req, out.Parts)
	out.Normalize()
	return out, nil
}

// parts are the message's reasoning, text and calls, in that order.
func (a *Adapter) parts(m wireMessage, text string) []llm.Part {
	var parts []llm.Part
	if r, ok := a.reasoning(m); ok {
		parts = append(parts, r)
	}
	if text != "" {
		parts = append(parts, llm.Text(text))
	}
	for _, tc := range m.ToolCalls {
		parts = append(parts, a.toolCall(tc))
	}
	if m.FunctionCall != nil && len(m.ToolCalls) == 0 {
		parts = append(parts, a.toolCall(wireToolCall{Function: *m.FunctionCall}))
	}
	return parts
}

// stop maps finish_reason (§3.4), and the Anthropic-style reasons some
// proxies pass through. A tool call is told from the content (rule 3), so
// tool_calls said without one is treated as a malformed call the loop
// retries once, or as the end when there is text; and a reason nobody
// documented is the end when there is text, else an error.
func stop(finish string, hasText, hasCalls bool) llm.Stop {
	switch finish {
	case "stop", "end_turn", "stop_sequence":
		return llm.StopEnd
	case "tool_calls", "function_call", "tool_use":
		switch {
		case hasCalls:
			return llm.StopToolCalls
		case hasText:
			return llm.StopEnd
		}
		return llm.StopToolError
	case "length", "max_tokens":
		return llm.StopMaxTokens
	case "content_filter", "sensitive":
		return llm.StopContentFilter
	case "model_context_window_exceeded":
		return llm.StopContextOverflow
	case "network_error", "error", "insufficient_system_resource":
		// GLM's network_error, OpenRouter's error, and DeepSeek's
		// insufficient_system_resource: the provider failed mid-answer.
		return llm.StopError
	}
	if hasText {
		return llm.StopEnd
	}
	return llm.StopError
}

// content reads message.content: a string, null, or a list of parts whose
// text parts are joined. A refusal part is returned apart.
func content(raw json.RawMessage) (text, refusal string) {
	if !present(raw) {
		return "", ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, ""
	}
	var parts []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", ""
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "output_text":
			b.WriteString(p.Text)
		case "refusal":
			refusal += p.Refusal
		}
	}
	return b.String(), refusal
}

// reasoning is the message's reasoning as one part. Opaque holds the fields
// to send back on this assistant message, verbatim: reasoning_content
// (DeepSeek, Kimi, GLM) and reasoning_details (OpenRouter), §3.6. Text is
// the readable reasoning, for nobody but a debug capture.
func (a *Adapter) reasoning(m wireMessage) (llm.Part, bool) {
	fields := map[string]json.RawMessage{}
	var text string
	if isString(m.ReasoningContent) {
		fields["reasoning_content"] = m.ReasoningContent
		_ = json.Unmarshal(m.ReasoningContent, &text)
	}
	var details []json.RawMessage
	if json.Unmarshal(m.ReasoningDetails, &details) == nil && len(details) > 0 {
		fields["reasoning_details"] = m.ReasoningDetails
	}
	if len(fields) == 0 {
		return llm.Part{}, false
	}
	if text == "" && isString(m.Reasoning) {
		_ = json.Unmarshal(m.Reasoning, &text)
	}
	opaque, err := marshalObject(fields)
	if err != nil {
		return llm.Part{}, false
	}
	return llm.Part{Type: llm.PartReasoning, Text: text, Maker: a.maker, Opaque: opaque}, true
}

// toolCall is one call. Its id may be missing, for Normalize to make; its
// arguments become an object, or {} with the raw text in ArgsError. What
// rode on it (extra_content) is kept in Opaque, stamped with this maker.
func (a *Adapter) toolCall(tc wireToolCall) llm.Part {
	p := llm.Part{Type: llm.PartToolCall, ID: tc.ID, Name: tc.Function.Name}
	p.Args, p.ArgsError = parseArgs(tc.Function.Arguments)
	if present(tc.ExtraContent) {
		if opaque, err := marshalObject(map[string]json.RawMessage{"extra_content": tc.ExtraContent}); err == nil {
			p.Maker, p.Opaque = a.maker, opaque
		}
	}
	return p
}

// parseArgs turns the arguments the model wrote into an object (rule 1).
// Nothing at all, an empty string or null is no arguments. Anything that is
// not one JSON object comes back as {} with the raw text, which the loop
// answers with an is_error result.
func parseArgs(raw json.RawMessage) (json.RawMessage, string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}"), ""
	}
	text := string(raw)
	if raw[0] == '"' {
		if json.Unmarshal(raw, &text) != nil {
			return json.RawMessage("{}"), string(raw)
		}
		switch strings.TrimSpace(text) {
		case "", "null":
			return json.RawMessage("{}"), ""
		}
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed), ""
	}
	return json.RawMessage("{}"), text
}

// errorIn200 classifies an error body that came with a 200. A code that is
// an HTTP status (OpenRouter's) is classified as that status; otherwise the
// code and text decide, and what they do not explain is taken as the
// server's fault, which a retry may get past. Status stays 200, which is
// what the server sent.
func errorIn200(resp *httpx.Response) *llm.Error {
	var v struct {
		Error struct {
			Code json.RawMessage `json:"code"`
		} `json:"error"`
	}
	status := 0
	if json.Unmarshal(resp.Body, &v) == nil {
		if n, err := strconv.Atoi(strings.Trim(string(v.Error.Code), `"`)); err == nil && n >= 400 && n <= 599 {
			status = n
		}
	}
	if status == 0 {
		// As a 400 first: that is where the text is read for a context
		// overflow or a content filter.
		e := httpx.Classify(http.StatusBadRequest, resp.Header, resp.Body)
		code := strings.ToLower(e.Code)
		switch {
		case e.Kind != llm.ErrBadRequest, strings.Contains(code, "invalid"):
			status = http.StatusBadRequest
		case strings.Contains(code, "rate_limit"), strings.Contains(code, "quota"):
			status = http.StatusTooManyRequests
		case strings.Contains(code, "auth"), strings.Contains(code, "api_key"):
			status = http.StatusUnauthorized
		default:
			status = http.StatusBadGateway
		}
	}
	e := httpx.Classify(status, resp.Header, resp.Body)
	e.Status = resp.Status
	return e
}

// wireUsage is the usage fields of §3.5, each read leniently: a server that
// writes a count as a float or a string still counts.
type wireUsage struct {
	PromptTokens        *count `json:"prompt_tokens"`
	CompletionTokens    *count `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens     *count `json:"cached_tokens"`
		CacheWriteTokens *count `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *count `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// PromptCacheHitTokens is DeepSeek's cache read; CachedTokens is
	// Moonshot's.
	PromptCacheHitTokens *count `json:"prompt_cache_hit_tokens"`
	CachedTokens         *count `json:"cached_tokens"`
}

// usage maps the provider's usage (§3.5), keeping it verbatim in Raw. With
// none, or none that counts anything, it is estimated from the text at four
// bytes a token and marked so: every budget still sees the call.
func (a *Adapter) usage(raw json.RawMessage, req *llm.Request, parts []llm.Part) llm.Usage {
	var u llm.Usage
	if present(raw) {
		u.Raw = append(json.RawMessage(nil), raw...)
	}
	var w wireUsage
	if !present(raw) || json.Unmarshal(raw, &w) != nil || (w.PromptTokens.value() == 0 && w.CompletionTokens.value() == 0) {
		u.Input = estimate(requestBytes(req))
		u.Output = estimate(responseBytes(parts))
		u.Estimated = true
		return u
	}
	u.Input = w.PromptTokens.value()
	u.Output = w.CompletionTokens.value()
	switch {
	case w.PromptTokensDetails != nil && w.PromptTokensDetails.CachedTokens != nil:
		u.CacheRead = w.PromptTokensDetails.CachedTokens.value()
	case w.PromptCacheHitTokens != nil:
		u.CacheRead = w.PromptCacheHitTokens.value()
	case w.CachedTokens != nil:
		u.CacheRead = w.CachedTokens.value()
	}
	if w.PromptTokensDetails != nil {
		u.CacheWrite = w.PromptTokensDetails.CacheWriteTokens.value()
	}
	if w.CompletionTokensDetails != nil {
		u.Reasoning = w.CompletionTokensDetails.ReasoningTokens.value()
	}
	return u
}

// estimate is n bytes of text at four a token, rounded up. Bytes rather
// than characters, so that text in scripts of several bytes a character,
// which also take more tokens, is not counted short.
func estimate(n int) int64 { return int64((n + 3) / 4) }

// requestBytes is the length of the text the model was given.
func requestBytes(req *llm.Request) int {
	n := len(req.System)
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			n += len(p.Text) + len(p.Name) + len(p.Args) + len(p.ArgsError) + len(p.Content)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	return n
}

// responseBytes is the length of the text the model wrote.
func responseBytes(parts []llm.Part) int {
	n := 0
	for _, p := range parts {
		n += len(p.Text) + len(p.Name) + len(p.Args) + len(p.ArgsError)
	}
	return n
}

// count is a token count as servers write it: an integer, a float, a
// numeric string, or null.
type count int64

func (c *count) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*c = 0
		return nil
	}
	// A count that makes no sense is no count: it is not worth failing the
	// call for.
	*c = 0
	if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 && f <= math.MaxInt64/2 {
		*c = count(f)
	}
	return nil
}

func (c *count) value() int64 {
	if c == nil {
		return 0
	}
	return int64(*c)
}

// present reports whether raw holds a value other than null.
func present(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && string(raw) != "null"
}

func isString(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"'
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
