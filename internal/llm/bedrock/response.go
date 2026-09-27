package bedrock

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
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
	InputTokens           int64  `json:"inputTokens"`
	OutputTokens          int64  `json:"outputTokens"`
	TotalTokens           *int64 `json:"totalTokens"`
	CacheReadInputTokens  int64  `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int64  `json:"cacheWriteInputTokens"`
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
	out.Normalize()
	return out, nil
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
// object; a JSON string holding one is taken too. Anything else is kept in
// argsErr, with {} as the arguments, and the loop answers it as an error.
func arguments(input json.RawMessage) (args json.RawMessage, argsErr string) {
	if isObject(input) {
		return compact(input), ""
	}
	var s string
	if json.Unmarshal(input, &s) == nil && isObject(json.RawMessage(s)) {
		return compact(json.RawMessage(s)), ""
	}
	if len(bytes.TrimSpace(input)) == 0 {
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
// tokens. AWS documents cacheReadInputTokens and cacheWriteInputTokens
// beside inputTokens, and its prompt-caching examples have totalTokens =
// inputTokens + outputTokens + both cache counts: inputTokens is the
// uncached part, as Anthropic's input_tokens is, so Input is the sum.
// Where a response says otherwise, with totalTokens = inputTokens +
// outputTokens and some cache, inputTokens already held the cache and is
// Input as it stands; the response's own arithmetic decides, so the count
// is right either way.
func usage(raw json.RawMessage) (llm.Usage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return llm.Usage{}, nil
	}
	var u usageOut
	if err := json.Unmarshal(raw, &u); err != nil {
		return llm.Usage{}, err
	}
	cache := u.CacheReadInputTokens + u.CacheWriteInputTokens
	input := u.InputTokens + cache
	if cache > 0 && u.TotalTokens != nil && *u.TotalTokens == u.InputTokens+u.OutputTokens {
		input = u.InputTokens
	}
	return llm.Usage{
		Input:      input,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheWriteInputTokens,
		Output:     u.OutputTokens,
		Raw:        raw,
	}, nil
}
