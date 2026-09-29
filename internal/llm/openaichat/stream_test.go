package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// A stream case is one streamed call: the internal request, the SSE body
// in testdata/stream/<fixture>.sse, written as the provider sends it, and
// two goldens, testdata/golden/stream_<name>.request.json (what was sent)
// and stream_<name>.response.json (the response, or the error, and the
// pieces of text onText was told). Where testdata/stream/<fixture>.json
// holds the same answer as the provider sends it whole, Stream must make
// of the stream exactly what Call makes of that.
type streamCase struct {
	name, fixture string
	cfg           llm.Config
	req           *llm.Request
}

func streamCases() []streamCase {
	simple := &llm.Request{System: system, Messages: []llm.Message{question()}, ToolMode: llm.ToolAuto}
	withTools := &llm.Request{System: system, Messages: []llm.Message{question()}, Tools: tools, ToolMode: llm.ToolAuto}
	openrouter := cfg(openrouterBase, "anthropic/claude-sonnet-4.5")
	openrouter.Reasoning = llm.Reasoning{Effort: "medium"}
	return []streamCase{
		// Text in pieces, a keep-alive comment, the usage in the chunk that
		// finishes, a piece escaped as <.
		{name: "deepseek_text", fixture: "deepseek_text", cfg: cfg(deepseekBase, "deepseek-chat"), req: simple},
		// reasoning_content streamed before the content: kept for the next
		// turn, never told as text.
		{name: "deepseek_reasoner", fixture: "deepseek_reasoner", cfg: cfg(deepseekBase, "deepseek-reasoner"), req: simple},
		// Two calls, their arguments split across chunks and interleaved,
		// and the usage alone in the last chunk before [DONE].
		{name: "openai_tool_calls_split", fixture: "openai_tool_calls_split", cfg: cfg("", "gpt-4.1-mini"), req: withTools},
		// Kimi's usage in the last chunk's choice; a preamble before a call.
		{name: "kimi_usage_in_choice", fixture: "kimi_usage_in_choice", cfg: cfg(moonshotBase, "kimi-k2-0905-preview"), req: withTools},
		// OpenRouter's comments, and reasoning_details in pieces joined into
		// the one detail the whole answer holds.
		{name: "openrouter_reasoning_details", fixture: "openrouter_reasoning_details", cfg: openrouter, req: simple},
		// A call whole in one chunk, with Gemini's thought signature.
		{name: "gemini_thought_signature", fixture: "gemini_thought_signature", cfg: cfg(geminiBase, "gemini-2.5-flash"), req: withTools},
		// A refusal streamed, with CRLF line endings and no blank line after
		// [DONE]: no text is told.
		{name: "openai_refusal_crlf", fixture: "openai_refusal_crlf", cfg: cfg("", "gpt-4.1-mini"), req: simple},
		// GLM's stream ends at its finish_reason without [DONE]: complete.
		{name: "glm_finish_without_done", fixture: "glm_finish_without_done", cfg: cfg(glmBase, "glm-4.6"), req: simple},
		// An upstream failing part way: an error chunk, retryable.
		{name: "openrouter_error_mid_stream", fixture: "openrouter_error_mid_stream", cfg: cfg(openrouterBase, "deepseek/deepseek-chat-v3.1"), req: simple},
		// The connection closed part way, before any finish_reason: a
		// network error, retryable.
		{name: "cut_off", fixture: "cut_off", cfg: cfg(deepseekBase, "deepseek-chat"), req: withTools},
	}
}

func TestStreamGolden(t *testing.T) {
	for _, c := range streamCases() {
		t.Run(c.name, func(t *testing.T) {
			sse, err := os.ReadFile(filepath.Join("testdata", "stream", c.fixture+".sse"))
			if err != nil {
				t.Fatal(err)
			}
			rt := &replay{status: http.StatusOK, body: string(sse), header: http.Header{"X-Request-Id": {"req_golden"}}}
			c.cfg.HTTPClient = &http.Client{Transport: eventStream{rt}}
			a, err := New(c.cfg)
			if err != nil {
				t.Fatal(err)
			}
			var deltas []string
			resp, callErr := a.Stream(context.Background(), c.req, func(d string) { deltas = append(deltas, d) })
			if rt.sent == nil {
				t.Fatalf("nothing was sent: %v", callErr)
			}
			checkGolden(t, "stream_"+c.name+".request.json", rt.sent)
			var sent struct {
				Body struct {
					Stream        bool `json:"stream"`
					StreamOptions struct {
						IncludeUsage bool `json:"include_usage"`
					} `json:"stream_options"`
				} `json:"body"`
			}
			if json.Unmarshal(rt.sent, &sent) != nil || !sent.Body.Stream || !sent.Body.StreamOptions.IncludeUsage {
				t.Errorf("the request does not ask for a stream with its usage: %s", rt.sent)
			}

			got := map[string]any{"deltas": deltas}
			if callErr != nil {
				var e *llm.Error
				if !errors.As(callErr, &e) {
					t.Fatalf("the error is %T, not *llm.Error: %v", callErr, callErr)
				}
				got["error"] = map[string]any{"kind": e.Kind, "status": e.Status, "code": e.Code, "message": e.Message,
					"retryable": e.Retryable(), "retry_after": e.RetryAfter.String()}
			} else {
				got["response"] = resp
				if text := strings.Join(deltas, ""); resp.Stop != llm.StopRefusal && text != resp.Text() {
					t.Errorf("the pieces told are %q; the response's text is %q", text, resp.Text())
				}
			}
			out, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, "stream_"+c.name+".response.json", out)

			whole, err := os.ReadFile(filepath.Join("testdata", "stream", c.fixture+".json"))
			if errors.Is(err, os.ErrNotExist) {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if callErr != nil {
				t.Fatalf("the stream failed, and the same answer whole is in %s.json: %v", c.fixture, callErr)
			}
			wrt := &replay{status: http.StatusOK, body: string(whole), header: http.Header{"X-Request-Id": {"req_golden"}}}
			wcfg := c.cfg
			wcfg.HTTPClient = &http.Client{Transport: wrt}
			wa, err := New(wcfg)
			if err != nil {
				t.Fatal(err)
			}
			want, err := wa.Call(context.Background(), c.req)
			if err != nil {
				t.Fatal(err)
			}
			sameResponse(t, resp, want)
		})
	}
}

// sameResponse fails unless got and want are the same response: every
// field equal, text byte for byte, and the provider's fragments (Opaque,
// the usage kept raw) the same JSON however they are spaced or escaped.
func sameResponse(t *testing.T, got, want *llm.Response) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical(t, g), canonical(t, w)) {
		t.Errorf("streamed, the response is\n%s\nwhole, it is\n%s", canonical(t, g), canonical(t, w))
	}
	if got.Text() != want.Text() {
		t.Errorf("text %q, whole %q", got.Text(), want.Text())
	}
}

// eventStream is a transport that answers as next does, as server-sent
// events.
type eventStream struct{ next http.RoundTripper }

func (e eventStream) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := e.next.RoundTrip(req)
	if err == nil {
		resp.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
	}
	return resp, err
}

// A provider that refuses stream_options (an Azure API version before
// them) is called again whole, and the adapter asks it for no stream again.
func TestStreamRefusedFallsBackToAWholeAnswer(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), `"stream":true`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":null,"message":"Unrecognized request argument supplied: stream_options","param":null,"type":"invalid_request_error"}}`)
			return
		}
		_, _ = io.WriteString(w, textReply("stop", "On Friday."))
	}))
	defer srv.Close()
	a, err := New(llm.Config{BaseURL: srv.URL + "/openai/v1", Provider: llm.ProviderAzure, Model: "gpt-4.1", APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	req := &llm.Request{Messages: []llm.Message{question()}, ToolMode: llm.ToolAuto}
	for range 2 {
		var told []string
		resp, err := a.Stream(context.Background(), req, func(d string) { told = append(told, d) })
		if err != nil || resp.Text() != "On Friday." || resp.Usage.Input != 40 {
			t.Fatalf("%+v, %v", resp, err)
		}
		if len(told) != 0 {
			t.Errorf("a whole answer told pieces: %q", told)
		}
	}
	if len(bodies) != 3 || !strings.Contains(bodies[0], `"stream":true`) || strings.Contains(bodies[1], `"stream":true`) ||
		strings.Contains(bodies[2], `"stream":true`) {
		t.Errorf("sent %d bodies; want a stream refused, then two whole calls:\n%s", len(bodies), strings.Join(bodies, "\n"))
	}
}

// Any other refusal of a streamed call is the refusal, as from Call, with
// the key kept out of it.
func TestStreamRefusalIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit reached for key `+testKey+`","type":"requests","code":"rate_limit_exceeded"}}`)
	}))
	defer srv.Close()
	a, err := New(llm.Config{BaseURL: srv.URL + "/v1", Model: "deepseek-chat", APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Stream(context.Background(), &llm.Request{Messages: []llm.Message{question()}}, func(string) {})
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrRateLimited || e.RetryAfter != 3*time.Second || strings.Contains(e.Message, testKey) {
		t.Fatalf("%v", err)
	}
	if a.noStream.Load() {
		t.Error("a rate limit turned streaming off")
	}
}

// A server that ignores stream and answers whole is read as Call reads it,
// its text told at once.
func TestStreamAnsweredWhole(t *testing.T) {
	rt := &replay{status: http.StatusOK, body: textReply("stop", "On Friday.")}
	a, err := New(llm.Config{BaseURL: "http://localhost:8080/v1", Model: "local", HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	var told []string
	resp, err := a.Stream(context.Background(), &llm.Request{Messages: []llm.Message{question()}}, func(d string) { told = append(told, d) })
	if err != nil || resp.Text() != "On Friday." || len(told) != 1 || told[0] != "On Friday." {
		t.Fatalf("%+v %q %v", resp, told, err)
	}
}

// A stream that stops coming is cut off by the call's own time: a timeout,
// which the loop may try again, with the text told so far no answer.
func TestStreamTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"content":"The dead"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	a, err := New(llm.Config{BaseURL: srv.URL + "/v1", Model: "deepseek-chat"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var told []string
	_, err = a.Stream(ctx, &llm.Request{Messages: []llm.Message{question()}}, func(d string) { told = append(told, d) })
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrTimeout || !e.Retryable() {
		t.Fatalf("%v", err)
	}
	if strings.Join(told, "") != "The dead" {
		t.Errorf("told %q before the stream stopped", told)
	}
}

// Without a function for the text, or through llm.Stream with none, the
// call is made whole.
func TestStreamWithoutTextIsACall(t *testing.T) {
	rt := &replay{status: http.StatusOK, body: textReply("stop", "On Friday.")}
	a, err := New(llm.Config{Model: "gpt-4.1", APIKey: testKey, HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := llm.Stream(context.Background(), a, &llm.Request{Messages: []llm.Message{question()}}, nil)
	if err != nil || resp.Text() != "On Friday." || strings.Contains(string(rt.sent), `"stream":true`) {
		t.Fatalf("%+v %v\n%s", resp, err, rt.sent)
	}
}
