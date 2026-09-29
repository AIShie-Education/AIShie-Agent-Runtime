package bedrock

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// converseResponse is the part of Converse's response the runtime reads.
type converseResponse struct {
	Output struct {
		Message *struct {
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string          `json:"stopReason"`
	Usage      json.RawMessage `json:"usage"`
}

type toolUseOut struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

// reasoningOut is a reasoningContent block: readable reasoning with its
// signature, or reasoning the provider encrypted.
type reasoningOut struct {
	ReasoningText *struct {
		Text string `json:"text"`
	} `json:"reasoningText"`
}

// usageOut is Converse's TokenUsage.
type usageOut struct {
	InputTokens           int64 `json:"inputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	CacheReadInputTokens  int64 `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
}

// stops maps Converse's stopReason to the runtime's (§3.4). A reason not
// here is StopError, with RawStop saying what it was.
var stops = map[string]llm.Stop{
	"end_turn":                      llm.StopEnd,
	"stop_sequence":                 llm.StopEnd,
	"tool_use":                      llm.StopToolCalls,
	"max_tokens":                    llm.StopMaxTokens,
	"guardrail_intervened":          llm.StopContentFilter,
	"content_filtered":              llm.StopContentFilter,
	"model_context_window_exceeded": llm.StopContextOverflow,
	"malformed_model_output":        llm.StopToolError,
	"malformed_tool_use":            llm.StopToolError,
}

// parse turns Converse's response into the internal one: text, toolUse as
// a tool_call with object arguments, reasoningContent as a reasoning part
// kept verbatim for replay; content blocks of other kinds are not the
// runtime's and are dropped.
func (a *Adapter) parse(body []byte) (*llm.Response, error) {
	var r converseResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := &llm.Response{Parts: []llm.Part{}, RawStop: r.StopReason}
	if m := r.Output.Message; m != nil {
		for i, b := range m.Content {
			p, ok, err := a.part(b)
			if err != nil {
				return nil, fmt.Errorf("content block %d: %w", i, err)
			}
			if ok {
				out.Parts = append(out.Parts, p)
			}
		}
	}
	if s, ok := stops[r.StopReason]; ok {
		out.Stop = s
	} else {
		out.Stop = llm.StopError
	}
	u, err := usage(r.Usage)
	if err != nil {
		return nil, fmt.Errorf("usage: %w", err)
	}
	out.Usage = u
	out.Parts = dropUnsafeCalls(out.Parts, out.Stop)
	out.Normalize()
	if out.Stop == llm.StopToolCalls && len(out.ToolCalls()) == 0 {
		// tool_use with no call to make: nothing the loop can go on with.
		out.Stop = llm.StopError
	}
	return out, nil
}

// dropUnsafeCalls removes the tool calls that must not run, before rule 3
// makes any call left mean tool_calls. An answer cut off by max_tokens or
// by the context window may end in a toolUse whose input is a valid but
// partial object: it is dropped, and the stop stands unless complete calls
// came before it. A filtered answer and a malformed tool use have calls
// the provider itself disowned: all are dropped, so that content_filter is
// answered with the refusal text and tool_error retries the turn, as §3.4
// asks, instead of running them.
func dropUnsafeCalls(parts []llm.Part, stop llm.Stop) []llm.Part {
	switch stop {
	case llm.StopMaxTokens, llm.StopContextOverflow:
		if n := len(parts); n > 0 && parts[n-1].Type == llm.PartToolCall {
			return parts[:n-1]
		}
	case llm.StopContentFilter, llm.StopToolError:
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

// part translates one content block; ok is false for a kind the runtime
// does not use.
func (a *Adapter) part(b map[string]json.RawMessage) (llm.Part, bool, error) {
	if raw, ok := b["text"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return llm.Part{}, false, err
		}
		return llm.Text(s), true, nil
	}
	if raw, ok := b["toolUse"]; ok {
		var t toolUseOut
		if err := json.Unmarshal(raw, &t); err != nil {
			return llm.Part{}, false, err
		}
		args, argsErr := arguments(t.Input)
		return llm.Part{Type: llm.PartToolCall, ID: t.ToolUseID, Name: t.Name, Args: args, ArgsError: argsErr}, true, nil
	}
	if raw, ok := b["reasoningContent"]; ok {
		var rc reasoningOut
		if err := json.Unmarshal(raw, &rc); err != nil {
			return llm.Part{}, false, err
		}
		p := llm.Part{Type: llm.PartReasoning, Maker: a.maker, Opaque: raw}
		if rc.ReasoningText != nil {
			p.Text = rc.ReasoningText.Text
		}
		return p, true, nil
	}
	return llm.Part{}, false, nil
}

// arguments is a toolUse's input as an object (rule 1). Converse gives an
// object; a JSON string holding one is taken too, and no input or null is a
// call without arguments. Anything else is kept in argsErr, with {} as the
// arguments, and the loop answers it as an error.
func arguments(input json.RawMessage) (args json.RawMessage, argsErr string) {
	if isObject(input) {
		return compact(input), ""
	}
	var s string
	if json.Unmarshal(input, &s) == nil && isObject(json.RawMessage(s)) {
		return compact(json.RawMessage(s)), ""
	}
	if t := bytes.TrimSpace(input); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return json.RawMessage("{}"), ""
	}
	return json.RawMessage("{}"), string(input)
}

func compact(raw json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

// usage reads Converse's usage (§3.5), keeping it verbatim in Raw.
//
// The handout marks it [UNVERIFIED] whether inputTokens includes cached
// tokens. AWS documents cacheReadInputTokens and cacheWriteInputTokens as
// counts of their own beside inputTokens, and its prompt-caching examples
// have totalTokens = inputTokens + outputTokens + both cache counts:
// inputTokens is the uncached part, as Anthropic's input_tokens is. So
// Input, which holds every input token, is the sum. Were a model to count
// the cache within inputTokens too, the sum would count it twice: a cost
// the ledger overstates, never one it misses, and Raw keeps what AWS said.
func usage(raw json.RawMessage) (llm.Usage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return llm.Usage{}, nil
	}
	var u usageOut
	if err := json.Unmarshal(raw, &u); err != nil {
		return llm.Usage{}, err
	}
	return llm.Usage{
		Input:      u.InputTokens + u.CacheReadInputTokens + u.CacheWriteInputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheWriteInputTokens,
		Output:     u.OutputTokens,
		Raw:        raw,
	}, nil
}
