package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/httpx"
)

// wireResponse is the body of a completed call, or of a refusal a server
// sent with a 2xx.
type wireResponse struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Model      string            `json:"model"`
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      json.RawMessage   `json:"usage"`
	Error      json.RawMessage   `json:"error"`
}

// wireContent is what the adapter reads of a response's content block.
type wireContent struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Thinking string          `json:"thinking"`
}

type wireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// stops maps the API's stop reasons to the runtime's (§3.4). pause_turn
// comes only from server tools, which the runtime never declares, so it is
// an error; so is any reason the adapter does not know.
var stops = map[string]llm.Stop{
	"end_turn":                      llm.StopEnd,
	"stop_sequence":                 llm.StopEnd,
	"tool_use":                      llm.StopToolCalls,
	"max_tokens":                    llm.StopMaxTokens,
	"refusal":                       llm.StopRefusal,
	"model_context_window_exceeded": llm.StopContextOverflow,
	"pause_turn":                    llm.StopError,
}

// decodeResponse translates a 2xx answer.
func (a *Adapter) decodeResponse(r *httpx.Response) (*llm.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(r.Body, &w); err != nil {
		return nil, httpx.DecodeError(err)
	}
	if w.Type == "error" || !isNull(w.Error) {
		// A proxy (OpenRouter) may send an upstream refusal with a 2xx.
		return nil, refine(httpx.Classify(r.Status, r.Header, r.Body))
	}
	resp := &llm.Response{RawStop: w.StopReason, Model: w.Model, RequestID: requestID(r, w.ID)}
	for i, raw := range w.Content {
		var c wireContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, httpx.DecodeError(fmt.Errorf("content block %d: %w", i, err))
		}
		switch c.Type {
		case "text":
			if c.Text != "" {
				resp.Parts = append(resp.Parts, llm.Text(c.Text))
			}
		case "tool_use":
			resp.Parts = append(resp.Parts, toolCall(c))
		case "thinking", "redacted_thinking":
			// The whole block goes back verbatim, signature and all; Text is
			// only for reading, and empty where the model omits it.
			resp.Parts = append(resp.Parts, llm.Part{Type: llm.PartReasoning, Text: c.Thinking, Maker: a.maker, Opaque: raw})
		}
		// Other blocks come from server tools and features the runtime does
		// not use.
	}
	resp.Usage = usage(w.Usage)
	stop, ok := stops[w.StopReason]
	if !ok {
		stop = llm.StopError
	}
	resp.Stop = stop
	resp.Parts = dropUnsafeCalls(resp.Parts, stop)
	resp.Normalize()
	if resp.Stop == llm.StopToolCalls && len(resp.ToolCalls()) == 0 {
		// tool_use with no call to make: nothing the loop can go on with.
		resp.Stop = llm.StopError
	}
	return resp, nil
}

// dropUnsafeCalls removes the tool calls that must not run. A response cut
// off at max_tokens may end in a tool_use whose input is a valid but
// partial object; it is dropped, and the stop stays max_tokens unless
// complete calls came before it. A refusal can cut a call off too, and
// none of a refused turn's calls may run, so all are dropped and the
// refusal stands. Without this, rule 3 would turn both into tool_calls and
// run what the model never finished.
func dropUnsafeCalls(parts []llm.Part, stop llm.Stop) []llm.Part {
	switch stop {
	case llm.StopMaxTokens:
		if n := len(parts); n > 0 && parts[n-1].Type == llm.PartToolCall {
			return parts[:n-1]
		}
	case llm.StopRefusal:
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

// toolCall is a tool_use block as a tool_call part. input is always an
// object from Anthropic. A server copying the format may send it as a
// string of JSON, which is parsed (rule 1); anything that is not an object
// then is kept in ArgsError, for the loop to answer as an error.
func toolCall(c wireContent) llm.Part {
	p := llm.Part{Type: llm.PartToolCall, ID: c.ID, Name: c.Name, Args: json.RawMessage("{}")}
	in := bytes.TrimSpace(c.Input)
	if isNull(in) {
		return p // a call without arguments
	}
	if in[0] == '"' {
		var s string
		if json.Unmarshal(in, &s) == nil {
			in = bytes.TrimSpace([]byte(s))
		}
	}
	if len(in) > 0 && in[0] == '{' && json.Valid(in) {
		p.Args = in
	} else {
		p.ArgsError = string(in)
	}
	return p
}

// usage is §3.5's Anthropic row: input counts every input token, cached or
// not, and reasoning is 0 because thinking is billed within output_tokens
// ([UNVERIFIED] whether any model reports it apart). A usage that does not
// decode leaves the numbers at 0 rather than lose the answer; Raw keeps it.
func usage(raw json.RawMessage) llm.Usage {
	if isNull(raw) {
		return llm.Usage{}
	}
	var u wireUsage
	_ = json.Unmarshal(raw, &u)
	return llm.Usage{
		Input:      u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
		Output:     u.OutputTokens,
		Raw:        raw,
	}
}

// requestID is the request-id header, else the message's id.
func requestID(r *httpx.Response, id string) string {
	if v := r.Header.Get("Request-Id"); v != "" {
		return v
	}
	return id
}

func isNull(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || string(raw) == "null"
}

// refine corrects httpx's classification with what Anthropic's error types
// say (§3.4, Anthropic's error reference). httpx goes by the status first;
// the type matters where a proxy passes Anthropic's error on under another
// status, or under a 2xx.
func refine(e *llm.Error) *llm.Error {
	lower := strings.ToLower(e.Message)
	if e.Status == http.StatusRequestEntityTooLarge {
		// Whoever refused it, a proxy included, the body is over a size
		// limit: a shorter history is the cure, as for a context that
		// overflows.
		e.Kind = llm.ErrContextOverflow
	}
	switch e.Code {
	case "overloaded_error":
		e.Kind = llm.ErrOverloaded
	case "rate_limit_error":
		e.Kind = llm.ErrRateLimited
	case "timeout_error":
		e.Kind = llm.ErrTimeout
	case "api_error":
		if e.Kind == llm.ErrBadRequest {
			e.Kind = llm.ErrServer
		}
	case "authentication_error", "permission_error", "billing_error":
		e.Kind = llm.ErrAuth
	case "request_too_large":
		e.Kind = llm.ErrContextOverflow
	case "invalid_request_error":
		switch {
		case strings.Contains(lower, "credit balance"):
			// The key's account cannot pay: no retry helps, the owner must.
			e.Kind = llm.ErrAuth
		case strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "exceed context limit"):
			e.Kind = llm.ErrContextOverflow
		}
	}
	return e
}
