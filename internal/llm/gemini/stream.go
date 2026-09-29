package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/httpx"
)

// Stream is Call with the answer streamed: streamGenerateContent?alt=sse,
// whose events are each a GenerateContentResponse holding the parts that
// came since the last. onText is told each piece of text that is not a
// thought. The parts are joined into those the whole answer holds (the
// pieces of one text, or of one thought, into one part, with the
// signature that came on any of them; a functionCall comes whole), the
// finishReason and the usage are the last chunk's, and the whole is read
// as Call reads it.
//
// A stream cut off before a finishReason is ErrNetwork, and one that ran
// out of time ErrTimeout, both tried again as a failed request is; an
// error part way is classified as Google's refusals are. A refusal that
// names the stream is answered by a whole call, and this adapter streams
// no more.
func (a *Adapter) Stream(ctx context.Context, req *llm.Request, onText llm.TextFunc) (*llm.Response, error) {
	if req == nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: "gemini: no request"}
	}
	if onText == nil || a.noStream.Load() {
		return a.Call(ctx, req)
	}
	wire, err := a.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: llm.Clip("gemini: encoding the request: " + err.Error())}
	}
	client, refusal := a.capturingClient()
	s, err := httpx.PostStream(ctx, client, a.streamEndpoint(), a.headers, body)
	if err != nil {
		var le *llm.Error
		if errors.As(err, &le) {
			a.refine(le, refusal.body)
			if le.Kind == llm.ErrBadRequest && le.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(le.Message), "stream") {
				a.noStream.Store(true)
				return a.Call(ctx, req)
			}
		}
		return nil, err
	}
	defer func() { _ = s.Close() }()
	out, err := a.readStream(s, onText)
	if err != nil {
		return nil, err
	}
	out.Normalize()
	return out, nil
}

// streamEndpoint is the endpoint's streaming twin, as server-sent events.
func (a *Adapter) streamEndpoint() string {
	return strings.TrimSuffix(a.endpoint, ":generateContent") + ":streamGenerateContent?alt=sse"
}

// readStream reads the chunks to the end and decodes the answer they make.
// Without alt=sse honoured, the chunks come as one JSON list.
func (a *Adapter) readStream(s *httpx.Stream, onText llm.TextFunc) (*llm.Response, error) {
	var b streamed
	if !s.EventStream() {
		whole, err := s.ReadAll()
		if err != nil {
			return nil, err
		}
		body := bytes.TrimSpace(whole.Body)
		if len(body) == 0 || body[0] != '[' {
			out, err := decodeResponse(whole.Body, a.maker)
			if err == nil {
				tellText(out, onText)
			}
			return out, err
		}
		var chunks []json.RawMessage
		if err := json.Unmarshal(body, &chunks); err != nil {
			return nil, httpx.DecodeError(err)
		}
		for _, c := range chunks {
			if err := a.addChunk(&b, s, c, onText); err != nil {
				return nil, err
			}
		}
		return a.finish(&b, s)
	}
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := a.addChunk(&b, s, ev.Data, onText); err != nil {
			return nil, err
		}
	}
	return a.finish(&b, s)
}

// tellText tells onText a whole answer's text, but its thoughts.
func tellText(out *llm.Response, onText llm.TextFunc) {
	if text := out.Text(); text != "" && out.Stop != llm.StopContentFilter {
		onText(text)
	}
}

// addChunk adds one chunk, or fails with the error it carries.
func (a *Adapter) addChunk(b *streamed, s *httpx.Stream, data []byte, onText llm.TextFunc) error {
	var c struct {
		Candidates []struct {
			Content *struct {
				Parts []json.RawMessage `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback json.RawMessage `json:"promptFeedback"`
		UsageMetadata  json.RawMessage `json:"usageMetadata"`
		ModelVersion   string          `json:"modelVersion"`
		ResponseID     string          `json:"responseId"`
		Error          json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		e := httpx.DecodeError(err)
		e.Status = s.Status
		return e
	}
	if !isNull(c.Error) {
		// The call failed part way: classified as the refusal it would
		// have been, and tried again as the server's fault it is.
		e := httpx.Classify(http.StatusBadGateway, s.Header, data)
		a.refine(e, data)
		e.Status = s.Status
		return e
	}
	if c.ModelVersion != "" {
		b.model = c.ModelVersion
	}
	if c.ResponseID != "" {
		b.id = c.ResponseID
	}
	if !isNull(c.UsageMetadata) {
		b.usage = c.UsageMetadata
	}
	if !isNull(c.PromptFeedback) {
		b.feedback = c.PromptFeedback
	}
	if len(c.Candidates) == 0 {
		return nil
	}
	cand := c.Candidates[0]
	b.candidate = true
	if cand.FinishReason != "" {
		b.finish = cand.FinishReason
	}
	if cand.Content == nil {
		return nil
	}
	for _, raw := range cand.Content.Parts {
		var p map[string]json.RawMessage
		if err := json.Unmarshal(raw, &p); err != nil {
			return httpx.DecodeError(err)
		}
		if text, thought, ok := textOf(p); ok {
			if !thought && text != "" {
				onText(text)
			}
			if n := len(b.parts); n > 0 && b.joins(n-1, thought) {
				b.join(n-1, p, text)
				continue
			}
		}
		b.parts = append(b.parts, p)
	}
	return nil
}

// streamed is a streamed answer built up as the whole one would hold it.
type streamed struct {
	candidate         bool
	parts             []map[string]json.RawMessage
	finish, model, id string
	usage, feedback   json.RawMessage
}

// textOf is a part's text, and whether it is a thought, when it is a text
// part and nothing else (no call, no code).
func textOf(p map[string]json.RawMessage) (text string, thought, ok bool) {
	raw, has := p["text"]
	if !has {
		return "", false, false
	}
	for k := range p {
		switch k {
		case "text", "thought", "thoughtSignature":
		default:
			return "", false, false
		}
	}
	if json.Unmarshal(raw, &text) != nil {
		return "", false, false
	}
	_ = json.Unmarshal(p["thought"], &thought)
	return text, thought, true
}

// joins reports whether a text part (a thought or not) continues part i.
func (b *streamed) joins(i int, thought bool) bool {
	_, was, ok := textOf(b.parts[i])
	return ok && was == thought
}

// join adds p's text to part i, with p's signature when it has one.
func (b *streamed) join(i int, p map[string]json.RawMessage, text string) {
	last := b.parts[i]
	var had string
	_ = json.Unmarshal(last["text"], &had)
	last["text"], _ = json.Marshal(had + text) // a string always encodes
	if sig, ok := p["thoughtSignature"]; ok && !isNull(sig) {
		last["thoughtSignature"] = sig
	}
}

// finish decodes the answer the chunks built, once the stream came to its
// end: a finishReason, or a prompt blocked.
func (a *Adapter) finish(b *streamed, s *httpx.Stream) (*llm.Response, error) {
	if b.finish == "" && isNull(b.feedback) {
		return nil, httpx.Broken(s.Status)
	}
	whole := map[string]any{"modelVersion": b.model, "responseId": b.id}
	if !isNull(b.usage) {
		whole["usageMetadata"] = b.usage
	}
	if !isNull(b.feedback) {
		whole["promptFeedback"] = b.feedback
	}
	if b.candidate {
		parts := make([]any, 0, len(b.parts))
		for _, p := range b.parts {
			parts = append(parts, p)
		}
		whole["candidates"] = []any{map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "finishReason": b.finish}}
	}
	body, err := json.Marshal(whole)
	if err != nil {
		return nil, httpx.DecodeError(err)
	}
	return decodeResponse(body, a.maker)
}

func isNull(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || string(raw) == "null"
}
