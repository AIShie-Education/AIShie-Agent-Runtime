package openairesponses

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/httpx"
)

// wireResponse is the parts of a response object the runtime reads.
type wireResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Model  string `json:"model"`
	// Error is read only for its code, and only if it is an object.
	Error             json.RawMessage   `json:"error"`
	IncompleteDetails *wireIncomplete   `json:"incomplete_details"`
	Output            []json.RawMessage `json:"output"`
	Usage             json.RawMessage   `json:"usage"`
}

type wireIncomplete struct {
	Reason string `json:"reason"`
}

// The output items the runtime reads. Others (the built-in tools', which
// the runtime never offers, and any added later) are skipped unread.
type (
	wireMessage struct {
		ID      string        `json:"id"`
		Phase   string        `json:"phase"`
		Content []wireContent `json:"content"`
	}
	wireFunctionCall struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
		// Arguments is a JSON string; a server that sends an object instead
		// is read too.
		Arguments json.RawMessage `json:"arguments"`
	}
	wireReasoningItem struct {
		Summary []wireContent `json:"summary"`
	}
	wireContent struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
)

type wireUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	InputTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int64 `json:"output_tokens"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// Statuses and incomplete reasons of a response.
const (
	statusCompleted  = "completed"
	statusIncomplete = "incomplete"
	statusFailed     = "failed"
	statusCancelled  = "cancelled"

	reasonMaxOutputTokens = "max_output_tokens"
	reasonContentFilter   = "content_filter"
)

// decode translates a 2xx body into a Response.
func (a *Adapter) decode(body []byte) (*llm.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, httpx.DecodeError(err)
	}
	out := &llm.Response{Parts: []llm.Part{}, Model: w.Model, RequestID: w.ID}
	refused := false
	// unconfirmed is the index in out.Parts of the last call that came
	// with no status, from a server that gives none.
	unconfirmed := -1
	for _, raw := range w.Output {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, httpx.DecodeError(err)
		}
		var err error
		switch head.Type {
		case "message":
			var m wireMessage
			if err = json.Unmarshal(raw, &m); err == nil {
				out.Parts, refused = a.messageParts(out.Parts, m, refused)
			}
		case "function_call":
			var fc wireFunctionCall
			if err = json.Unmarshal(raw, &fc); err != nil {
				break
			}
			switch fc.Status {
			case "":
				unconfirmed = len(out.Parts)
			case statusCompleted:
			default:
				// in_progress or incomplete: the model did not finish the
				// call, its arguments are cut short, and it must not run.
				continue
			}
			out.Parts = append(out.Parts, a.toolCallPart(fc))
		case "reasoning":
			var r wireReasoningItem
			if err = json.Unmarshal(raw, &r); err == nil {
				out.Parts = append(out.Parts, a.reasoningPart(r, raw))
			}
		}
		if err != nil {
			return nil, httpx.DecodeError(err)
		}
	}
	out.Stop, out.RawStop = stopOf(&w, refused)
	out.Parts = dropUnsafeCalls(out.Parts, out.Stop, unconfirmed)
	out.Usage = usageOf(w.Usage)
	out.Normalize()
	return out, nil
}

// dropUnsafeCalls removes the calls that must not run, before rule 3 makes
// any call left mean tool_calls. A response cut off at max_output_tokens
// may end in a call whose arguments are cut short: the API marks it
// incomplete and decode leaves it out, and one from a server that gives no
// status is dropped here when it is the last part. The stop then stands,
// unless complete calls came before it. A filtered or refused turn's calls
// are disowned, every one, so that the loop answers content_filter and
// refusal with the refusal text (§3.4) instead of running them.
func dropUnsafeCalls(parts []llm.Part, stop llm.Stop, unconfirmed int) []llm.Part {
	switch stop {
	case llm.StopMaxTokens:
		if n := len(parts); n > 0 && unconfirmed == n-1 {
			return parts[:n-1]
		}
	case llm.StopContentFilter, llm.StopRefusal:
		kept := parts[:0]
		for _, p := range parts {
			if p.Type != llm.PartToolCall {
				kept = append(kept, p)
			}
		}
		return kept
	}
	return parts
}

// messageParts appends a message's text parts to parts, and reports
// whether it, or an earlier message, refused. The refusal's words are not
// an answer: the loop answers a refusal with the configured text.
func (a *Adapter) messageParts(parts []llm.Part, m wireMessage, refused bool) ([]llm.Part, bool) {
	for _, c := range m.Content {
		switch c.Type {
		case "output_text":
			parts = append(parts, a.textPart(c.Text, m))
		case "refusal":
			refused = true
		}
	}
	return parts, refused
}

// textPart keeps the message's item id and phase for the maker, when it
// has them.
func (a *Adapter) textPart(text string, m wireMessage) llm.Part {
	p := llm.Text(text)
	if m.ID != "" || m.Phase != "" {
		p.Maker = a.maker
		p.Opaque = mustOpaque(opaqueItem{ID: m.ID, Phase: m.Phase})
	}
	return p
}

// toolCallPart links the call by call_id, not by the item's id, which is
// kept for the maker (§3.2).
func (a *Adapter) toolCallPart(fc wireFunctionCall) llm.Part {
	p := llm.Part{Type: llm.PartToolCall, ID: fc.CallID, Name: fc.Name}
	p.Args, p.ArgsError = parseArguments(fc.Arguments)
	if fc.ID != "" {
		p.Maker = a.maker
		p.Opaque = mustOpaque(opaqueItem{ID: fc.ID})
	}
	return p
}

// reasoningPart keeps the whole item, compacted, for the maker; its text
// is the readable summary, when the API gave one.
func (a *Adapter) reasoningPart(r wireReasoningItem, raw json.RawMessage) llm.Part {
	texts := make([]string, 0, len(r.Summary))
	for _, s := range r.Summary {
		if s.Text != "" {
			texts = append(texts, s.Text)
		}
	}
	return llm.Part{Type: llm.PartReasoning, Text: strings.Join(texts, "\n\n"), Maker: a.maker, Opaque: compact(raw)}
}

// parseArguments reads a call's arguments into a JSON object. None at all
// (absent, null or empty) is an empty object; anything else that is not an
// object comes back as ArgsError, verbatim, with Args {} (rule 1).
func parseArguments(raw json.RawMessage) (json.RawMessage, string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return json.RawMessage("{}"), ""
	}
	text := string(raw)
	var s string
	if json.Unmarshal(raw, &s) == nil {
		text = s
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return json.RawMessage("{}"), ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil || obj == nil {
		return json.RawMessage("{}"), text
	}
	return compact(json.RawMessage(trimmed)), ""
}

// stopOf maps the response's status to a Stop (§3.4), with the raw stop
// as status or status/reason. A tool call is told from the content, later,
// by Normalize (rule 3).
func stopOf(w *wireResponse, refused bool) (llm.Stop, string) {
	switch w.Status {
	case statusCompleted, "":
		// A server that leaves the status out answered in full, or it
		// would have said otherwise: what it wrote is the answer.
		if refused {
			return llm.StopRefusal, w.Status
		}
		return llm.StopEnd, w.Status
	case statusIncomplete:
		reason := ""
		if w.IncompleteDetails != nil {
			reason = w.IncompleteDetails.Reason
		}
		raw := join(w.Status, reason)
		switch reason {
		case reasonMaxOutputTokens:
			if refused {
				return llm.StopRefusal, raw
			}
			return llm.StopMaxTokens, raw
		case reasonContentFilter:
			return llm.StopContentFilter, raw
		}
		// max_messages, steered, and any reason added later: the loop
		// cannot make more of it than an error.
		return llm.StopError, raw
	case statusFailed, statusCancelled:
		return llm.StopError, join(w.Status, errorCode(w.Error))
	}
	// queued and in_progress come only from background mode, which the
	// runtime never asks for.
	return llm.StopError, w.Status
}

func join(status, reason string) string {
	if reason == "" {
		return status
	}
	return status + "/" + reason
}

// usageOf maps usage (§3.5) and keeps it verbatim in Raw. Usage that does
// not decode leaves the numbers at zero rather than lose an answer the
// model gave; Raw still holds what the provider said.
func usageOf(raw json.RawMessage) llm.Usage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return llm.Usage{}
	}
	u := llm.Usage{Raw: compact(raw)}
	var w wireUsage
	if json.Unmarshal(raw, &w) != nil {
		return u
	}
	u.Input = w.InputTokens
	u.Output = w.OutputTokens
	if d := w.InputTokensDetails; d != nil {
		u.CacheRead = d.CachedTokens
		u.CacheWrite = d.CacheWriteTokens
	}
	if d := w.OutputTokensDetails; d != nil {
		u.Reasoning = d.ReasoningTokens
	}
	return u
}

// compact is raw without insignificant whitespace; raw itself when it is
// not valid JSON, which a decoded field always is.
func compact(raw json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

// mustOpaque encodes what the adapter keeps for itself; a struct of
// strings always encodes.
func mustOpaque(o opaqueItem) json.RawMessage {
	b, _ := json.Marshal(o)
	return b
}

// errorCode is error.code, when error is an object that has one.
func errorCode(raw json.RawMessage) string {
	var e struct {
		Code json.RawMessage `json:"code"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &e) != nil || len(e.Code) == 0 {
		return ""
	}
	return rawString(e.Code)
}

// rawString is a JSON string's value, or a number's text.
func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}
