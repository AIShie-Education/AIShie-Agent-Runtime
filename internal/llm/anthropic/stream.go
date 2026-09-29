package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/httpx"
)

// Stream is Call with the answer streamed, as the Messages API's
// server-sent events: message_start, each content block's start, deltas
// and stop, message_delta with the stop reason and the usage so far, and
// message_stop. onText is told each text_delta as it comes. The events are
// built up into the message the API would have sent whole, which is then
// read as Call reads it: the same blocks, text byte for byte, stop, calls
// and usage (message_start's, with message_delta's counts over it).
//
// A stream cut off before message_stop (or at least a message_delta with
// its stop reason) is ErrNetwork, and one that ran out of time ErrTimeout,
// both tried again as a failed request is; an error event part way
// (overloaded_error, api_error) is classified as the API's refusals are,
// and those are retryable too. A server that refuses to stream, with a 400
// that names the stream, is called again without streaming, and this
// adapter streams no more.
func (a *Adapter) Stream(ctx context.Context, req *llm.Request, onText llm.TextFunc) (*llm.Response, error) {
	if onText == nil || a.noStream.Load() {
		return a.call(ctx, req, nil)
	}
	out, err := a.call(ctx, req, onText)
	if streamRefused(err) {
		a.noStream.Store(true)
		return a.call(ctx, req, nil)
	}
	return out, err
}

// streamRefused reports whether err is a server refusing the stream
// itself, in its own words: a bad request that names it.
func streamRefused(err error) bool {
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrBadRequest || e.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(e.Code+" "+e.Message), "stream")
}

// readStream reads the events up to message_stop and translates the
// message they built up. A server that answered whole, ignoring stream, is
// read as Call reads it, and onText told its text at once.
func (a *Adapter) readStream(s *httpx.Stream, onText llm.TextFunc) (*llm.Response, error) {
	whole := &httpx.Response{Status: s.Status, Header: s.Header}
	if !s.EventStream() {
		r, err := s.ReadAll()
		if err != nil {
			return nil, err
		}
		out, err := a.decodeResponse(r)
		if err == nil && out.Stop != llm.StopRefusal {
			if text := out.Text(); text != "" {
				onText(text)
			}
		}
		return out, err
	}
	var m message
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			if m.stop == "" {
				return nil, httpx.Broken(s.Status)
			}
			break
		}
		if err != nil {
			return nil, err
		}
		done, err := m.add(ev.Data, onText)
		if err != nil {
			var le *llm.Error
			if errors.As(err, &le) {
				le.Status = s.Status // what the server sent
			}
			return nil, err
		}
		if done {
			break
		}
	}
	return a.translate(m.wire(), whole)
}

// message builds up a streamed message as the whole body would have held
// it.
type message struct {
	id, model, stop string
	usage           map[string]json.RawMessage
	blocks          []*streamBlock
}

// streamBlock is one content block, built up from its start and deltas.
type streamBlock struct {
	index int
	// start is content_block_start's block, as it came.
	start                           json.RawMessage
	kind                            string
	id, name                        string
	text, thinking, signature, json strings.Builder
}

// streamEvent is what the adapter reads of any event.
type streamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage json.RawMessage `json:"usage"`
}

// add takes one event; done is message_stop.
func (m *message) add(data []byte, onText llm.TextFunc) (done bool, err error) {
	var ev streamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return false, httpx.DecodeError(err)
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			m.id, m.model = ev.Message.ID, ev.Message.Model
			m.addUsage(ev.Message.Usage)
		}
	case "content_block_start":
		var head struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.ContentBlock, &head); err != nil {
			return false, httpx.DecodeError(err)
		}
		b := &streamBlock{index: ev.Index, start: ev.ContentBlock, kind: head.Type, id: head.ID, name: head.Name}
		b.text.WriteString(head.Text)
		if head.Type == "text" && head.Text != "" {
			onText(head.Text)
		}
		m.blocks = append(m.blocks, b)
	case "content_block_delta":
		b := m.block(ev.Index)
		if b == nil || ev.Delta == nil {
			return false, httpx.DecodeError(errors.New("a delta of a content block that never started"))
		}
		switch ev.Delta.Type {
		case "text_delta":
			b.text.WriteString(ev.Delta.Text)
			if b.kind == "text" && ev.Delta.Text != "" {
				onText(ev.Delta.Text)
			}
		case "input_json_delta":
			b.json.WriteString(ev.Delta.PartialJSON)
		case "thinking_delta":
			b.thinking.WriteString(ev.Delta.Thinking)
		case "signature_delta":
			b.signature.WriteString(ev.Delta.Signature)
		}
		// citations_delta and the deltas of features the runtime does
		// not use are passed over, as their blocks are.
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			m.stop = ev.Delta.StopReason
		}
		m.addUsage(ev.Usage)
	case "message_stop":
		return true, nil
	case "error":
		// Classified as a 502 would be, then by the error's type: an
		// overload or an api_error part way is tried again.
		return false, refine(httpx.Classify(http.StatusBadGateway, nil, data))
	}
	// ping, content_block_stop, and events newer than the adapter.
	return false, nil
}

func (m *message) block(index int) *streamBlock {
	for _, b := range m.blocks {
		if b.index == index {
			return b
		}
	}
	return nil
}

// addUsage lays usage's counts over those so far: message_delta's are the
// message's totals.
func (m *message) addUsage(raw json.RawMessage) {
	var u map[string]json.RawMessage
	if isNull(raw) || json.Unmarshal(raw, &u) != nil {
		return
	}
	if m.usage == nil {
		m.usage = map[string]json.RawMessage{}
	}
	for k, v := range u {
		if !isNull(v) || m.usage[k] == nil {
			m.usage[k] = v
		}
	}
}

// wire is the message as the API would have sent it whole.
func (m *message) wire() wireResponse {
	w := wireResponse{ID: m.id, Type: "message", Model: m.model, StopReason: m.stop}
	if m.usage != nil {
		w.Usage, _ = marshal(m.usage) // raw values always encode
	}
	blocks := slices.Clone(m.blocks)
	slices.SortStableFunc(blocks, func(x, y *streamBlock) int { return x.index - y.index })
	for _, b := range blocks {
		w.Content = append(w.Content, b.wire())
	}
	return w
}

// wire is the block as the whole message would have held it. A tool_use's
// input is its partial JSON joined, or the start's input when no delta
// came; input that does not parse, as a call cut off at max_tokens may
// be, is kept as a string, which toolCall reads as a model's malformed
// arguments.
func (b *streamBlock) wire() json.RawMessage {
	var v any
	switch b.kind {
	case "text":
		v = struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{b.kind, b.text.String()}
	case "thinking":
		v = struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{b.kind, b.thinking.String(), b.signature.String()}
	case "tool_use":
		input := json.RawMessage(bytes.TrimSpace([]byte(b.json.String())))
		switch {
		case len(input) == 0:
			var start struct {
				Input json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(b.start, &start)
			input = start.Input
		case !json.Valid(input):
			input, _ = marshal(b.json.String())
		}
		v = struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{b.kind, b.id, b.name, input}
	default:
		// redacted_thinking comes whole in its start, and other blocks
		// are passed over by translate.
		return b.start
	}
	raw, _ := marshal(v) // strings and valid JSON always encode
	return raw
}
