package fakellm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// stream sends resp as OpenAI streams an answer (stream: true): a chunk
// with the role, the content a few words at a time, each call with its id
// and name and then its arguments in two pieces, a chunk with the finish
// reason, and, where stream_options.include_usage asks, a last chunk with
// the usage alone; then [DONE]. The pieces go the response's Every apart
// (the server's StreamEvery when it sets none), and its Stall after the
// first piece of text, or until the caller gives up.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, req ChatRequest, resp ChatResponse) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	every := resp.Every
	if every == 0 {
		every = time.Duration(s.every.Load())
	}
	first := true
	send := func(choices []any, usage *Usage) bool {
		if !first && every > 0 && !s.wait(r, every) {
			return false
		}
		first = false
		chunk := map[string]any{"id": resp.ID, "object": "chat.completion.chunk", "created": resp.Created, "model": resp.Model,
			"choices": choices}
		if usage != nil {
			chunk["usage"] = usage
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	delta := func(d map[string]any, finish any) []any {
		return []any{map[string]any{"index": 0, "delta": d, "finish_reason": finish}}
	}
	var c Choice
	if len(resp.Choices) > 0 {
		c = resp.Choices[0]
	}
	if !send(delta(map[string]any{"role": "assistant", "content": ""}, nil), nil) {
		return
	}
	for i, piece := range pieces(c.Message.Text()) {
		if !send(delta(map[string]any{"content": piece}, nil), nil) {
			return
		}
		if i == 0 && resp.Stall > 0 && !s.wait(r, resp.Stall) {
			return
		}
	}
	for i, tc := range c.Message.ToolCalls {
		head := map[string]any{"index": i, "id": tc.ID, "type": "function", "function": map[string]any{"name": tc.Function.Name, "arguments": ""}}
		if !send(delta(map[string]any{"tool_calls": []any{head}}, nil), nil) {
			return
		}
		args := tc.Function.Arguments
		for _, part := range []string{args[:len(args)/2], args[len(args)/2:]} {
			piece := map[string]any{"index": i, "function": map[string]any{"arguments": part}}
			if !send(delta(map[string]any{"tool_calls": []any{piece}}, nil), nil) {
				return
			}
		}
	}
	if !send(delta(map[string]any{}, c.FinishReason), nil) {
		return
	}
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage && resp.Usage != nil {
		if !send([]any{}, resp.Usage) {
			return
		}
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// pieces cuts text into pieces of about three words, spaces kept, that
// join into it again.
func pieces(text string) []string {
	var out []string
	var b strings.Builder
	words := 0
	for i, r := range text {
		b.WriteRune(r)
		if r == ' ' && i+1 < len(text) && text[i+1] != ' ' {
			if words++; words == 3 {
				out = append(out, b.String())
				b.Reset()
				words = 0
			}
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
