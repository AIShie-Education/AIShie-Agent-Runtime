package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// streamServer answers every request with the SSE body sse, as the
// Messages API streams, and keeps the bodies it was sent.
func streamServer(t *testing.T, sse string) (*httptest.Server, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Request-Id", "req_011CSHoEeqs5C35K2UUqR7Fy")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

// TestStreamGolden streams each of testdata/stream/*.sse, written as the
// Messages API sends its events, into stream_*.internal.json: the response
// or the error, and the pieces of text told. Where the same message is in
// testdata/golden/response_*.anthropic.json as the API sends it whole,
// Stream must make exactly what Call makes of that.
func TestStreamGolden(t *testing.T) {
	for _, c := range []struct{ name, whole string }{
		{"text", "text"},                     // text in pieces, a ping, null cache counts
		{"parallel_tools", "parallel_tools"}, // a preamble, then two calls whose input comes in pieces
		{"thinking", "thinking"},             // thinking and its signature in deltas, redacted_thinking whole
		// max_tokens part way through a call's input: the call is dropped,
		// as it is from the whole message, and message_delta's usage
		// carries every count.
		{"max_tokens_cut_call", "max_tokens_cut_call"},
		{"overloaded_mid_stream", ""}, // an error event part way: overloaded, retryable
		{"cut_off", ""},               // the connection closed part way: a network error, retryable
	} {
		t.Run(c.name, func(t *testing.T) {
			sse, err := os.ReadFile(filepath.Join("testdata", "stream", c.name+".sse"))
			if err != nil {
				t.Fatal(err)
			}
			srv, bodies := streamServer(t, string(sse))
			// Anthropic's own base, so that the golden's maker stays put.
			a := newAdapter(t, llm.Config{HTTPClient: &http.Client{Transport: toServer{srv}}})
			req := &llm.Request{System: "You are CS101's tutor.", Messages: []llm.Message{question}, ToolMode: llm.ToolAuto,
				Tools: []llm.Tool{gradeList, assignmentGet}}
			var deltas []string
			resp, callErr := a.Stream(context.Background(), req, func(d string) { deltas = append(deltas, d) })
			if sent := bodies(); len(sent) != 1 || !bytes.Contains(sent[0], []byte(`"stream":true`)) {
				t.Fatalf("sent %q; want one request asking for a stream", sent)
			}
			got := map[string]any{"deltas": deltas}
			if callErr != nil {
				var e *llm.Error
				if !errors.As(callErr, &e) {
					t.Fatalf("the error is %T, not *llm.Error: %v", callErr, callErr)
				}
				got["error"] = map[string]any{"kind": e.Kind, "status": e.Status, "code": e.Code, "message": e.Message, "retryable": e.Retryable()}
			} else {
				got["response"] = resp
				if text := strings.Join(deltas, ""); text != resp.Text() {
					t.Errorf("the pieces told are %q; the response's text is %q", text, resp.Text())
				}
			}
			checkGolden(t, "stream_"+c.name+".internal.json", encode(t, got))
			if c.whole == "" {
				return
			}
			if callErr != nil {
				t.Fatalf("the stream failed, and the same message is whole in response_%s: %v", c.whole, callErr)
			}
			want, err := a.decodeResponse(httpResponse(readGolden(t, "response_"+c.whole+".anthropic.json")))
			if err != nil {
				t.Fatal(err)
			}
			g, _ := canonical(encode(t, resp))
			w, _ := canonical(encode(t, want))
			if !bytes.Equal(g, w) {
				t.Errorf("streamed, the response is\n%s\nwhole, it is\n%s", g, w)
			}
		})
	}
}

// toServer sends every request to srv, whatever its URL.
type toServer struct{ srv *httptest.Server }

func (s toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	u, err := url.Parse(s.srv.URL)
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, ""
	return http.DefaultTransport.RoundTrip(r)
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Replayed thinking the API refuses is sent again without it, streamed as
// the first request was.
func TestStreamStaleThinkingIsSentAgain(t *testing.T) {
	sse, err := os.ReadFile(filepath.Join("testdata", "stream", "text.sse"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: Invalid `+"`signature`"+` in `+"`thinking`"+` block"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(sse)
	}))
	defer srv.Close()
	a := newAdapter(t, llm.Config{BaseURL: srv.URL})
	thought := llm.Part{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: json.RawMessage(`{"type":"thinking","thinking":"x","signature":"old"}`)}
	req := &llm.Request{Messages: []llm.Message{question, {Role: llm.RoleAssistant, Parts: []llm.Part{thought, llm.Text("Hm.")}}, llm.UserText("And?")}}
	var told strings.Builder
	resp, err := a.Stream(context.Background(), req, func(d string) { told.WriteString(d) })
	if err != nil || resp.Stop != llm.StopEnd || told.String() != resp.Text() {
		t.Fatalf("%+v %v", resp, err)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"signature":"old"`) || strings.Contains(bodies[1], `"signature":"old"`) ||
		!strings.Contains(bodies[1], `"stream":true`) {
		t.Errorf("bodies:\n%s", strings.Join(bodies, "\n"))
	}
}

// A server that refuses to stream is called again whole, and not asked to
// stream again; any other refusal is the refusal.
func TestStreamRefused(t *testing.T) {
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
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"stream: streaming is not supported"}}`)
			return
		}
		_, _ = w.Write(readGolden(t, "response_text.anthropic.json"))
	}))
	defer srv.Close()
	a := newAdapter(t, llm.Config{BaseURL: srv.URL})
	for range 2 {
		var told []string
		resp, err := a.Stream(context.Background(), &llm.Request{Messages: []llm.Message{question}}, func(d string) { told = append(told, d) })
		if err != nil || resp.Stop != llm.StopEnd || len(told) != 0 {
			t.Fatalf("%+v %q %v", resp, told, err)
		}
	}
	if len(bodies) != 3 {
		t.Errorf("%d requests; want a stream refused, then two whole calls", len(bodies))
	}
}

// A stream that stops coming is cut off by the call's own time.
func TestStreamTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":5}}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"You lost\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	a := newAdapter(t, llm.Config{BaseURL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var told []string
	_, err := a.Stream(ctx, &llm.Request{Messages: []llm.Message{question}}, func(d string) { told = append(told, d) })
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrTimeout || !e.Retryable() || len(told) != 1 {
		t.Fatalf("%v, told %q", err, told)
	}
}
