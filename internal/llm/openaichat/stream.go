package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/httpx"
)

// Stream is Call with the answer streamed: stream true, and
// stream_options.include_usage, so that the last chunk carries the call's
// usage, as OpenAI, Azure, DeepSeek, Qwen, OpenRouter, vLLM and Ollama
// document it; Kimi and GLM put theirs in the last chunk unasked, Kimi in
// its choice. onText is told each piece of the answer's content as its
// chunk comes. The chunks are built up into the body the provider would
// have sent whole, which is then read as Call reads it: the same parts,
// text byte for byte, stop, calls and usage.
//
// A stream cut off before its last chunk ([DONE], or at least a
// finish_reason) is ErrNetwork, and one that ran out of time ErrTimeout,
// both tried again as a failed request is; an error chunk part way
// (OpenRouter's, when its upstream fails) is classified as an error in a
// 200 is. A provider that refuses to stream, with a 400 that names the
// stream (an Azure API version that knows no stream_options), is called
// again without streaming, and this adapter streams no more: the call is
// answered whole, as before.
func (a *Adapter) Stream(ctx context.Context, req *llm.Request, onText llm.TextFunc) (*llm.Response, error) {
	if req == nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: "openaichat: no request"}
	}
	if onText == nil || a.noStream.Load() {
		return a.Call(ctx, req)
	}
	wire := a.request(req)
	wire.Stream, wire.StreamOptions = true, &streamOptions{IncludeUsage: true}
	body, err := marshal(wire)
	if err != nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: llm.Clip("openaichat: the request does not encode: " + err.Error())}
	}
	s, err := httpx.PostStream(ctx, a.client, a.endpoint, a.headers, body)
	if err != nil {
		err = a.refused(err)
		if streamRefused(err) {
			a.noStream.Store(true)
			return a.Call(ctx, req)
		}
		return nil, err
	}
	defer func() { _ = s.Close() }()
	out, err := a.readStream(s, req, onText)
	if err != nil {
		return nil, a.refused(err)
	}
	return out, nil
}

// streamRefused reports whether err is a provider refusing the stream
// itself, in its own words: a bad request that names it.
func streamRefused(err error) bool {
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(e.Code+" "+e.Message), "stream")
}

// readStream reads the answer's chunks up to [DONE], and translates what
// they built up. A server that answered whole, ignoring stream, is read as
// Call reads it, and onText told its text at once.
func (a *Adapter) readStream(s *httpx.Stream, req *llm.Request, onText llm.TextFunc) (*llm.Response, error) {
	if !s.EventStream() {
		resp, err := s.ReadAll()
		if err != nil {
			return nil, err
		}
		out, err := a.response(resp, req)
		if err == nil && out.Stop != llm.StopRefusal {
			if text := out.Text(); text != "" {
				onText(text)
			}
		}
		return out, err
	}
	var b builder
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			if !b.finished() {
				return nil, httpx.Broken(s.Status)
			}
			break
		}
		if err != nil {
			return nil, err
		}
		data := bytes.TrimSpace(ev.Data)
		if string(data) == "[DONE]" {
			break
		}
		var c chatChunk
		if err := json.Unmarshal(data, &c); err != nil {
			e := httpx.DecodeError(err)
			e.Status = s.Status
			return nil, e
		}
		if present(c.Error) {
			return nil, errorIn200(&httpx.Response{Status: s.Status, Header: s.Header, Body: data})
		}
		b.add(c, onText)
	}
	return a.translate(b.response(), &httpx.Response{Status: s.Status, Header: s.Header}, req)
}

// chatChunk is one event of a streamed answer: a chat.completion.chunk.
type chatChunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	// Usage is in the last chunk, whose choices are empty, when
	// include_usage asked for it.
	Usage json.RawMessage `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type chunkChoice struct {
	Index              int        `json:"index"`
	Delta              chunkDelta `json:"delta"`
	FinishReason       *string    `json:"finish_reason"`
	NativeFinishReason *string    `json:"native_finish_reason"`
	// Usage is Kimi's, in the last chunk's choice.
	Usage json.RawMessage `json:"usage"`
}

// chunkDelta is what a chunk adds to the message: every field of
// wireMessage, a piece at a time.
type chunkDelta struct {
	Content          json.RawMessage `json:"content"`
	Refusal          *string         `json:"refusal"`
	ToolCalls        []chunkToolCall `json:"tool_calls"`
	FunctionCall     *chunkFunction  `json:"function_call"`
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	ReasoningDetails json.RawMessage `json:"reasoning_details"`
	Reasoning        json.RawMessage `json:"reasoning"`
}

// chunkToolCall is a piece of one call: its index, and with the first
// piece its id and name; its arguments come a piece at a time.
type chunkToolCall struct {
	Index        *int            `json:"index"`
	ID           string          `json:"id"`
	Function     chunkFunction   `json:"function"`
	ExtraContent json.RawMessage `json:"extra_content"`
}

type chunkFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// builder builds up a streamed answer's first choice as the whole body
// would have held it.
type builder struct {
	id, model string
	choice    bool
	usage     json.RawMessage
	// choiceUsage is Kimi's, used when no top-level usage came.
	choiceUsage    json.RawMessage
	finish, native *string

	content, refusal, reasoningContent, reasoning texts
	details                                       []map[string]json.RawMessage
	calls                                         []*callBuilder
	function                                      *callBuilder
}

// texts is a string built up from pieces, and whether any piece came.
type texts struct {
	b    strings.Builder
	seen bool
}

func (t *texts) add(s string) { t.b.WriteString(s); t.seen = true }

// raw is the text as a JSON string, or nil when no piece came.
func (t *texts) raw() json.RawMessage {
	if !t.seen {
		return nil
	}
	b, _ := marshal(t.b.String()) // a string always encodes
	return b
}

type callBuilder struct {
	index    int
	hasIndex bool
	id, name string
	args     strings.Builder
	// whole is arguments a server sent as the object itself, not as a
	// string in pieces.
	whole json.RawMessage
	extra json.RawMessage
}

// finished reports whether the answer came to its end: a finish_reason
// came, though the [DONE] after it did not.
func (b *builder) finished() bool { return b.finish != nil }

// add takes one chunk's pieces, telling onText the content's.
func (b *builder) add(c chatChunk, onText llm.TextFunc) {
	if b.id == "" {
		b.id = c.ID
	}
	if c.Model != "" {
		b.model = c.Model
	}
	if present(c.Usage) {
		b.usage = c.Usage
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue // the runtime asks for one choice
		}
		b.choice = true
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			b.finish = ch.FinishReason
		}
		if ch.NativeFinishReason != nil && *ch.NativeFinishReason != "" {
			b.native = ch.NativeFinishReason
		}
		if present(ch.Usage) {
			b.choiceUsage = ch.Usage
		}
		b.delta(ch.Delta, onText)
	}
}

func (b *builder) delta(d chunkDelta, onText llm.TextFunc) {
	if present(d.Content) {
		text, refusal := content(d.Content)
		if text != "" || isString(d.Content) {
			b.content.add(text)
			if text != "" {
				onText(text)
			}
		}
		if refusal != "" {
			b.refusal.add(refusal)
		}
	}
	if d.Refusal != nil {
		b.refusal.add(*d.Refusal)
	}
	if isString(d.ReasoningContent) {
		var s string
		if json.Unmarshal(d.ReasoningContent, &s) == nil {
			b.reasoningContent.add(s)
		}
	}
	if isString(d.Reasoning) {
		var s string
		if json.Unmarshal(d.Reasoning, &s) == nil {
			b.reasoning.add(s)
		}
	}
	var details []map[string]json.RawMessage
	if json.Unmarshal(d.ReasoningDetails, &details) == nil {
		for _, item := range details {
			b.detail(item)
		}
	}
	for _, tc := range d.ToolCalls {
		b.call(tc).add(tc.ID, tc.Function, tc.ExtraContent)
	}
	if d.FunctionCall != nil {
		if b.function == nil {
			b.function = &callBuilder{}
		}
		b.function.add("", *d.FunctionCall, nil)
	}
}

// call is the call a piece belongs to: the one of its index; without an
// index (servers that send each call whole), the last one, unless the
// piece names another id.
func (b *builder) call(tc chunkToolCall) *callBuilder {
	if tc.Index != nil {
		for _, c := range b.calls {
			if c.hasIndex && c.index == *tc.Index {
				return c
			}
		}
		c := &callBuilder{index: *tc.Index, hasIndex: true}
		b.calls = append(b.calls, c)
		return c
	}
	if n := len(b.calls); n > 0 {
		if last := b.calls[n-1]; tc.ID == "" || last.id == "" || last.id == tc.ID {
			return last
		}
	}
	c := &callBuilder{index: len(b.calls)}
	b.calls = append(b.calls, c)
	return c
}

func (c *callBuilder) add(id string, f chunkFunction, extra json.RawMessage) {
	if c.id == "" {
		c.id = id
	}
	if c.name == "" {
		c.name = f.Name
	}
	switch args := bytes.TrimSpace(f.Arguments); {
	case !present(args):
	case args[0] == '"':
		var s string
		if json.Unmarshal(args, &s) == nil {
			c.args.WriteString(s)
		}
	default:
		c.whole = append(json.RawMessage(nil), args...)
	}
	if present(extra) {
		c.extra = extra
	}
}

// wire is the call as the whole body would have held it.
func (c *callBuilder) wire() wireToolCall {
	tc := wireToolCall{ID: c.id, Function: wireFunction{Name: c.name}, ExtraContent: c.extra}
	switch {
	case c.args.Len() > 0:
		tc.Function.Arguments, _ = marshal(c.args.String()) // a string always encodes
	case c.whole != nil:
		tc.Function.Arguments = c.whole
	}
	return tc
}

// detail adds one piece of reasoning_details (OpenRouter's). A piece of the
// same type as the last, at the same index or with none, continues it:
// its text, summary or data are joined on, and any other field it gives
// is taken. Anything else is a detail of its own.
func (b *builder) detail(item map[string]json.RawMessage) {
	if n := len(b.details); n > 0 {
		last := b.details[n-1]
		if bytes.Equal(last["type"], item["type"]) && bytes.Equal(last["index"], item["index"]) {
			for k, v := range item {
				switch k {
				case "text", "summary", "data":
					var s1, s2 string
					if json.Unmarshal(last[k], &s1) == nil && json.Unmarshal(v, &s2) == nil {
						last[k], _ = marshal(s1 + s2)
						continue
					}
				}
				if present(v) || last[k] == nil {
					last[k] = v
				}
			}
			return
		}
	}
	b.details = append(b.details, item)
}

// response is the body the provider would have sent whole.
func (b *builder) response() chatResponse {
	w := chatResponse{ID: b.id, Model: b.model, Usage: b.usage}
	if !present(w.Usage) {
		w.Usage = b.choiceUsage
	}
	if !b.choice {
		return w
	}
	m := wireMessage{
		Content: b.content.raw(), ReasoningContent: b.reasoningContent.raw(), Reasoning: b.reasoning.raw(),
	}
	if b.refusal.seen {
		s := b.refusal.b.String()
		m.Refusal = &s
	}
	if len(b.details) > 0 {
		m.ReasoningDetails, _ = marshal(b.details) // raw values always encode
	}
	calls := slices.Clone(b.calls)
	slices.SortStableFunc(calls, func(x, y *callBuilder) int { return x.index - y.index })
	for _, c := range calls {
		m.ToolCalls = append(m.ToolCalls, c.wire())
	}
	if b.function != nil {
		f := b.function.wire().Function
		m.FunctionCall = &f
	}
	w.Choices = []chatChoice{{Message: m, FinishReason: b.finish, NativeFinishReason: b.native}}
	return w
}
