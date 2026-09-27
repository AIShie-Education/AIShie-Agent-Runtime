package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestMCP(url string) *MCPCaller {
	return NewMCPCaller(MCPOptions{BaseURL: url, Token: testToken, ClientVersion: "test"})
}

// TestMCPHandshake checks what goes on the wire: initialize once, then
// notifications/initialized with no id, then tools/call, every request with
// the same headers, ids increasing. The requests are golden.
func TestMCPHandshake(t *testing.T) {
	f := newFakeMCP(t)
	f.call = answerWith(`{"status":"executed","result":{"id":"a1","kind":"agent","display_name":"CS101 Tutor","status":"active"}}`)
	c := newTestMCP(f.URL + "/")

	if c.Instructions() != "" || c.Protocol() != "" {
		t.Fatal("instructions or protocol before initialize")
	}
	ctx := context.Background()
	for range 2 {
		env, err := c.Call(ctx, "me_get", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var a Actor
		if err := env.Decode(&a); err != nil || a.DisplayName != "CS101 Tutor" {
			t.Fatalf("me_get: %+v %v", a, err)
		}
	}
	if _, err := c.Call(ctx, "conversation_inbox", json.RawMessage(`{"course_id":"c1","limit":20}`)); err != nil {
		t.Fatal(err)
	}
	if c.Instructions() != "Start with me_memberships." || c.Protocol() != DefaultProtocol {
		t.Fatalf("instructions %q, protocol %q", c.Instructions(), c.Protocol())
	}

	reqs := f.requests()
	var methods []string
	for _, r := range reqs {
		methods = append(methods, r.Method)
	}
	if want := "initialize notifications/initialized tools/call tools/call tools/call"; strings.Join(methods, " ") != want {
		t.Fatalf("methods %v, want %s", methods, want)
	}
	var wire bytes.Buffer
	for i, r := range reqs {
		for _, h := range []string{"Authorization", "Content-Type", "Accept", "Mcp-Protocol-Version"} {
			if len(r.Header.Values(h)) != 1 {
				t.Errorf("request %d has %d %s headers", i, len(r.Header.Values(h)), h)
			}
		}
		keys := make([]string, 0, len(r.Header))
		for k := range r.Header {
			switch k {
			case "Content-Length", "Accept-Encoding":
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&wire, "POST /mcp\n")
		for _, k := range keys {
			fmt.Fprintf(&wire, "%s: %s\n", k, strings.Join(r.Header.Values(k), ", "))
		}
		fmt.Fprintf(&wire, "\n%s\n\n", r.Body)
	}
	golden(t, "mcp_requests.golden", wire.Bytes())
}

func TestMCPAnswers(t *testing.T) {
	executed := `{"status":"executed","action_id":"act1","review_state":"none","result":{"message_id":"m2"}}`
	denied := `{"status":"denied","action_id":"act2","error":{"code":"forbidden","message":"your seat may not answer now"}}`
	conflict := `{"status":"failed","action_id":"act3","error":{"code":"conflict","message":"the opener wrote again","details":{"reason":"moved_on","latest_opener_message_id":"m3"}}}`
	notFound := `{"status":"error","error":{"code":"not_found","message":"no such conversation"}}`

	result := func(v any) func(http.ResponseWriter, json.RawMessage) {
		return func(w http.ResponseWriter, id json.RawMessage) { writeResult(w, id, v) }
	}
	raw := func(status int, contentType, body string) func(http.ResponseWriter, json.RawMessage) {
		return func(w http.ResponseWriter, _ json.RawMessage) {
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	sse := func(events ...string) func(http.ResponseWriter, json.RawMessage) {
		return func(w http.ResponseWriter, id json.RawMessage) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, e := range events {
				_, _ = w.Write([]byte(strings.ReplaceAll(e, "$ID", string(id))))
			}
		}
	}
	envelope := func(status Status, code, reason string) func(*testing.T, *Envelope, error) {
		return func(t *testing.T, env *Envelope, err error) {
			t.Helper()
			if err != nil {
				t.Fatalf("got error %v, want an envelope", err)
			}
			if env.Status != status || env.Code() != code || env.Reason() != reason {
				t.Fatalf("got %+v, want %s %s %s", env, status, code, reason)
			}
		}
	}
	transient := func(status int) func(*testing.T, *Envelope, error) {
		return func(t *testing.T, env *Envelope, err error) {
			t.Helper()
			var te *TransientError
			if !errors.As(err, &te) || te.Status != status || env != nil {
				t.Fatalf("got %v %v, want a *TransientError with status %d", env, err, status)
			}
		}
	}
	protocol := func(code int64, contains string) func(*testing.T, *Envelope, error) {
		return func(t *testing.T, env *Envelope, err error) {
			t.Helper()
			var pe *ProtocolError
			if !errors.As(err, &pe) || pe.Code != code || !strings.Contains(pe.Message, contains) || env != nil {
				t.Fatalf("got %v %v, want a *ProtocolError %d containing %q", env, err, code, contains)
			}
		}
	}
	limited := func(want time.Duration) func(*testing.T, *Envelope, error) {
		return func(t *testing.T, env *Envelope, err error) {
			t.Helper()
			var rl *RateLimitedError
			if !errors.As(err, &rl) || rl.RetryAfter != want {
				t.Fatalf("got %v %v, want rate limited for %s", env, err, want)
			}
		}
	}
	rateLimitedBody := `{"error":{"code":"rate_limited","message":"too many calls; try again in 3 seconds","details":{"retry_after_seconds":3}}}`

	for _, tc := range []struct {
		name   string
		answer func(http.ResponseWriter, json.RawMessage)
		check  func(*testing.T, *Envelope, error)
	}{
		{"executed", result(toolResult(executed)), func(t *testing.T, env *Envelope, err error) {
			envelope(StatusExecuted, "", "")(t, env, err)
			var r struct {
				MessageID string `json:"message_id"`
			}
			if env.ActionID != "act1" || env.ReviewState != ReviewNone || env.Decode(&r) != nil || r.MessageID != "m2" {
				t.Fatalf("got %+v", env)
			}
		}},
		{"isError denied is an envelope", result(toolResult(denied)), envelope(StatusDenied, CodeForbidden, "")},
		{"isError failed conflict", result(toolResult(conflict)), func(t *testing.T, env *Envelope, err error) {
			envelope(StatusFailed, CodeConflict, ReasonMovedOn)(t, env, err)
			if env.Detail("latest_opener_message_id") != "m3" {
				t.Fatalf("details: %+v", env.Error.Details)
			}
		}},
		{"isError error not found", result(toolResult(notFound)), envelope(StatusError, CodeNotFound, "")},
		{"no structuredContent: content[0].text", result(map[string]any{
			"content": []any{map[string]any{"type": "text", "text": denied}}, "isError": true,
		}), envelope(StatusDenied, CodeForbidden, "")},
		{"structuredContent not an envelope: content[0].text", result(map[string]any{
			"content": []any{map[string]any{"type": "text", "text": executed}}, "structuredContent": map[string]any{"x": 1},
		}), envelope(StatusExecuted, "", "")},
		{"no envelope anywhere", result(map[string]any{"content": []any{map[string]any{"type": "text", "text": "hello"}}}),
			protocol(0, "no envelope")},
		{"empty result", result(map[string]any{}), protocol(0, "no envelope")},
		{"null result", result(nil), protocol(0, "neither a result nor an error")},
		{"401 text/plain", raw(http.StatusUnauthorized, "text/plain; charset=utf-8", "invalid token: the credential is missing or not valid\n"),
			func(t *testing.T, env *Envelope, err error) {
				if !errors.Is(err, ErrUnauthenticated) || env != nil {
					t.Fatalf("got %v %v", env, err)
				}
			}},
		{"429 with Retry-After", func(w http.ResponseWriter, _ json.RawMessage) {
			w.Header().Set("Retry-After", "7")
			raw(http.StatusTooManyRequests, "application/json", rateLimitedBody)(w, nil)
		}, limited(7 * time.Second)},
		{"429 with Retry-After as a date", func(w http.ResponseWriter, _ json.RawMessage) {
			w.Header().Set("Retry-After", time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
			raw(http.StatusTooManyRequests, "application/json", rateLimitedBody)(w, nil)
		}, func(t *testing.T, env *Envelope, err error) {
			var rl *RateLimitedError
			if !errors.As(err, &rl) || rl.RetryAfter < 85*time.Second || rl.RetryAfter > 90*time.Second {
				t.Fatalf("got %v", err)
			}
		}},
		{"429 without Retry-After: the body's", raw(http.StatusTooManyRequests, "application/json", rateLimitedBody), limited(3 * time.Second)},
		{"429 with neither", raw(http.StatusTooManyRequests, "text/plain", "slow down"), limited(time.Second)},
		{"429 asking for days", func(w http.ResponseWriter, _ json.RawMessage) {
			w.Header().Set("Retry-After", "999999")
			raw(http.StatusTooManyRequests, "", "")(w, nil)
		}, limited(time.Hour)},
		{"500", raw(http.StatusInternalServerError, "text/plain", "storing stream: boom"), transient(500)},
		{"502 from a proxy", raw(http.StatusBadGateway, "text/html", "<html>bad gateway</html>"), transient(502)},
		{"503 with a JSON-RPC error", func(w http.ResponseWriter, id json.RawMessage) {
			writeJSON(w, 503, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "down"}})
		}, transient(503)},
		{"JSON-RPC error", func(w http.ResponseWriter, id json.RawMessage) { writeRPCError(w, id, -32602, `unknown tool "nope"`) },
			protocol(-32602, "unknown tool")},
		{"400 JSON-RPC error with a null id", func(w http.ResponseWriter, _ json.RawMessage) {
			writeJSON(w, 400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32600, "message": "one message per request"}})
		}, protocol(-32600, "HTTP 400: one message per request")},
		{"400 text/plain", raw(http.StatusBadRequest, "text/plain", "Bad Request: Unsupported protocol version"), protocol(0, "HTTP 400: Bad Request")},
		{"404", raw(http.StatusNotFound, "text/plain", "404 page not found"), protocol(0, "HTTP 404")},
		{"408", raw(http.StatusRequestTimeout, "text/plain", ""), transient(408)},
		{"another request's id", func(w http.ResponseWriter, _ json.RawMessage) {
			writeResult(w, json.RawMessage(`999`), toolResult(executed))
		}, protocol(0, "request 999")},
		{"not JSON", raw(http.StatusOK, "application/json", "<html>hello</html>"), protocol(0, "not JSON-RPC")},
		{"JSON cut short", raw(http.StatusOK, "application/json", `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"te`), transient(200)},
		{"body cut short", func(w http.ResponseWriter, _ json.RawMessage) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0",`))
		}, transient(200)},
		{"no body", raw(http.StatusOK, "application/json", ""), protocol(0, "no answer")},
		{"202 to a request", raw(http.StatusAccepted, "", ""), protocol(0, "no answer")},
		{"SSE", sse(
			"id: 1\ndata:\n\n", // the priming event of 2025-11-25
			`data: {"jsonrpc":"2.0","method":"notifications/message","params":{}}`+"\n\n",
			`data: {"jsonrpc":"2.0","id":$ID,"result":`+mustJSON(toolResult(denied))+"}\n\n",
		), envelope(StatusDenied, CodeForbidden, "")},
		{"SSE over several data lines, CRLF, no space", sse(
			": a comment\r\nevent: message\r\ndata:{\"jsonrpc\":\"2.0\",\r\ndata: \"id\":$ID,\r\ndata: \"result\":" + mustJSON(toolResult(executed)) + "}\r\n\r\n",
		), envelope(StatusExecuted, "", "")},
		{"SSE last event without a blank line", sse(
			`data: {"jsonrpc":"2.0","id":$ID,"result":` + mustJSON(toolResult(executed)) + "}",
		), envelope(StatusExecuted, "", "")},
		{"SSE JSON-RPC error", sse(`data: {"jsonrpc":"2.0","id":$ID,"error":{"code":-32602,"message":"bad params"}}` + "\n\n"),
			protocol(-32602, "bad params")},
		{"SSE ends without the answer", sse(`data: {"jsonrpc":"2.0","id":999,"result":{}}` + "\n\n"), transient(200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeMCP(t)
			f.call = func(w http.ResponseWriter, id json.RawMessage, _ string, _ json.RawMessage) { tc.answer(w, id) }
			env, err := newTestMCP(f.URL).Call(context.Background(), "conversation_answer", json.RawMessage(`{"course_id":"c"}`))
			tc.check(t, env, err)
			noSecret(t, err)
		})
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestMCPNetworkErrorsAreTransient(t *testing.T) {
	t.Run("nothing listens", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		_, err := newTestMCP(url).Call(context.Background(), "me_get", nil)
		var te *TransientError
		if !errors.As(err, &te) || te.Status != 0 {
			t.Fatalf("got %v", err)
		}
		noSecret(t, err)
	})
	t.Run("timeout", func(t *testing.T) {
		f := newFakeMCP(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f.call = func(http.ResponseWriter, json.RawMessage, string, json.RawMessage) { <-release }
		c := newTestMCP(f.URL)
		if err := c.Initialize(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := c.Call(ctx, "me_get", nil)
		var te *TransientError
		if !errors.As(err, &te) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("the client's own timeout", func(t *testing.T) {
		f := newFakeMCP(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f.call = func(http.ResponseWriter, json.RawMessage, string, json.RawMessage) { <-release }
		c := NewMCPCaller(MCPOptions{BaseURL: f.URL, Token: testToken, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}})
		_, err := c.Call(context.Background(), "me_get", nil)
		var te *TransientError
		if !errors.As(err, &te) {
			t.Fatalf("got %v", err)
		}
		noSecret(t, err)
	})
}

func TestMCPTooLarge(t *testing.T) {
	f := newFakeMCP(t)
	f.call = answerWith(`{"status":"executed","result":{"body":"` + strings.Repeat("x", 4096) + `"}}`)
	c := NewMCPCaller(MCPOptions{BaseURL: f.URL, Token: testToken, MaxResponseBytes: 2048})
	_, err := c.Call(context.Background(), "document_get", nil)
	var pe *ProtocolError
	if !errors.As(err, &pe) || !strings.Contains(pe.Message, "larger") {
		t.Fatalf("got %v", err)
	}

	f.call = func(w http.ResponseWriter, id json.RawMessage, _ string, _ json.RawMessage) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n\n", id,
			mustJSON(toolResult(`{"status":"executed","result":{"body":"`+strings.Repeat("x", 4096)+`"}}`)))
	}
	_, err = c.Call(context.Background(), "document_get", nil)
	if !errors.As(err, &pe) || !strings.Contains(pe.Message, "larger") {
		t.Fatalf("stream: got %v", err)
	}
}

// Nothing a server says puts the token into an error, even a server that
// repeats it.
func TestMCPErrorsNeverCarryTheToken(t *testing.T) {
	for _, status := range []int{400, 404, 500, 502} {
		f := newFakeMCP(t)
		f.call = func(w http.ResponseWriter, id json.RawMessage, _ string, _ json.RawMessage) {
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, "you sent Bearer %s, and ais_OtherToken too", testToken)
		}
		_, err := newTestMCP(f.URL).Call(context.Background(), "me_get", nil)
		if err == nil {
			t.Fatalf("HTTP %d: no error", status)
		}
		noSecret(t, err)
	}
	f := newFakeMCP(t)
	f.call = func(w http.ResponseWriter, id json.RawMessage, _ string, _ json.RawMessage) {
		writeRPCError(w, id, -32603, "token "+testToken+" refused")
	}
	_, err := newTestMCP(f.URL).Call(context.Background(), "me_get", nil)
	noSecret(t, err)
	if err == nil {
		t.Fatal("no error")
	}
}

func TestMCPInitialize(t *testing.T) {
	ctx := context.Background()
	t.Run("another revision is refused, and initialize is tried again", func(t *testing.T) {
		f := newFakeMCP(t)
		f.protocol = "2025-06-18"
		f.call = answerWith(`{"status":"executed","result":{}}`)
		c := newTestMCP(f.URL)
		_, err := c.Call(ctx, "me_get", nil)
		var pe *ProtocolError
		if !errors.As(err, &pe) || !strings.Contains(pe.Message, `"2025-06-18"`) || !strings.Contains(pe.Message, `"2025-11-25"`) {
			t.Fatalf("got %v", err)
		}
		if c.Protocol() != "" || f.count("tools/call") != 0 || f.count("notifications/initialized") != 0 {
			t.Fatal("a call went ahead on the wrong revision")
		}
		f.set(func() { f.protocol = "" })
		if _, err := c.Call(ctx, "me_get", nil); err != nil {
			t.Fatal(err)
		}
		if f.count("initialize") != 2 || c.Protocol() != DefaultProtocol {
			t.Fatalf("initialize sent %d times, protocol %q", f.count("initialize"), c.Protocol())
		}
	})
	t.Run("a pinned revision of the caller's own", func(t *testing.T) {
		f := newFakeMCP(t)
		f.call = answerWith(`{"status":"executed","result":{}}`)
		c := NewMCPCaller(MCPOptions{BaseURL: f.URL, Token: testToken, Protocol: "2025-06-18"})
		if _, err := c.Call(ctx, "me_get", nil); err != nil {
			t.Fatal(err)
		}
		for _, r := range f.requests() {
			if r.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
				t.Fatalf("%s sent %q", r.Method, r.Header.Get("MCP-Protocol-Version"))
			}
		}
	})
	t.Run("401 at initialize", func(t *testing.T) {
		f := newFakeMCP(t)
		c := NewMCPCaller(MCPOptions{BaseURL: f.URL, Token: "ais_revoked"})
		if _, err := c.Call(ctx, "me_get", nil); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("got %v", err)
		}
		if f.count("initialize") != 1 || f.count("tools/call") != 0 {
			t.Fatal("went on after a 401")
		}
	})
	t.Run("failures are retried by the next call", func(t *testing.T) {
		f := newFakeMCP(t)
		f.call = answerWith(`{"status":"executed","result":{}}`)
		var failInit, failNote atomic.Bool
		failInit.Store(true)
		failNote.Store(true)
		f.intercept = func(w http.ResponseWriter, _ *http.Request, m rpcSeen) bool {
			switch {
			case m.Method == "initialize" && failInit.CompareAndSwap(true, false):
				http.Error(w, "down", http.StatusServiceUnavailable)
				return true
			case m.Method == "notifications/initialized" && failNote.CompareAndSwap(true, false):
				http.Error(w, "down", http.StatusInternalServerError)
				return true
			}
			return false
		}
		c := newTestMCP(f.URL)
		var te *TransientError
		if _, err := c.Call(ctx, "me_get", nil); !errors.As(err, &te) || te.Status != 503 {
			t.Fatalf("first: %v", err)
		}
		if _, err := c.Call(ctx, "me_get", nil); !errors.As(err, &te) || te.Status != 500 {
			t.Fatalf("second: %v", err)
		}
		if _, err := c.Call(ctx, "me_get", nil); err != nil {
			t.Fatalf("third: %v", err)
		}
		if n := f.count("initialize"); n != 3 {
			t.Fatalf("initialize sent %d times, want 3", n)
		}
	})
	t.Run("a notification answered 200 or 204 is taken too; a 401 is not", func(t *testing.T) {
		for status, want := range map[int]error{http.StatusOK: nil, http.StatusNoContent: nil, http.StatusUnauthorized: ErrUnauthenticated} {
			f := newFakeMCP(t)
			f.call = answerWith(`{"status":"executed","result":{}}`)
			f.intercept = func(w http.ResponseWriter, _ *http.Request, m rpcSeen) bool {
				if m.Method != "notifications/initialized" {
					return false
				}
				if len(m.ID) != 0 {
					t.Errorf("the notification has an id: %s", m.ID)
				}
				w.WriteHeader(status)
				return true
			}
			if _, err := newTestMCP(f.URL).Call(ctx, "me_get", nil); !errors.Is(err, want) {
				t.Errorf("notification answered %d: got %v, want %v", status, err, want)
			}
		}
	})
	t.Run("concurrent first calls initialize once", func(t *testing.T) {
		f := newFakeMCP(t)
		f.call = answerWith(`{"status":"executed","result":{}}`)
		c := newTestMCP(f.URL)
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := c.Call(ctx, "me_get", nil); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if f.count("initialize") != 1 || f.count("tools/call") != 20 {
			t.Fatalf("initialize %d, tools/call %d", f.count("initialize"), f.count("tools/call"))
		}
		ids := map[string]bool{}
		for _, r := range f.requests() {
			if r.Method != "notifications/initialized" && ids[string(r.ID)] {
				t.Fatalf("id %s used twice", r.ID)
			}
			ids[string(r.ID)] = true
		}
	})
	t.Run("a caller waiting for another's initialize gives up with its context", func(t *testing.T) {
		f := newFakeMCP(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		entered := make(chan struct{}, 1)
		f.intercept = func(http.ResponseWriter, *http.Request, rpcSeen) bool {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return false
		}
		c := newTestMCP(f.URL)
		go func() { _ = c.Initialize(context.Background()) }()
		<-entered
		cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		var te *TransientError
		if _, err := c.Call(cctx, "me_get", nil); !errors.As(err, &te) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestMCPListTools(t *testing.T) {
	f := newFakeMCP(t)
	f.list = func(w http.ResponseWriter, id json.RawMessage, cursor string) {
		switch cursor {
		case "":
			writeResult(w, id, map[string]any{"tools": []any{map[string]any{"name": "me_get", "inputSchema": map[string]any{"type": "object"}}}, "nextCursor": "p2"})
		case "p2":
			writeResult(w, id, map[string]any{"tools": []any{map[string]any{"name": "me_memberships", "description": "Every seat."}}})
		}
	}
	tools, err := newTestMCP(f.URL).ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "me_get" || string(tools[0].InputSchema) != `{"type":"object"}` || tools[1].Description != "Every seat." {
		t.Fatalf("got %+v", tools)
	}

	f.list = func(w http.ResponseWriter, id json.RawMessage, _ string) {
		writeResult(w, id, map[string]any{"tools": []any{}, "nextCursor": "same"})
	}
	var pe *ProtocolError
	if _, err := newTestMCP(f.URL).ListTools(context.Background()); !errors.As(err, &pe) {
		t.Fatalf("a cursor that never ends: %v", err)
	}
}

// The arguments go out as the caller wrote them, HTML characters and all:
// they are the bytes written ahead (§2.2).
func TestMCPSendsArgumentsAsWritten(t *testing.T) {
	f := newFakeMCP(t)
	var got json.RawMessage
	f.call = func(w http.ResponseWriter, id json.RawMessage, _ string, args json.RawMessage) {
		got = args
		writeResult(w, id, toolResult(`{"status":"executed","result":{}}`))
	}
	args := json.RawMessage(`{"body":"a <b> & c","idempotency_key":"answer:x:m:1","course_id":"c"}`)
	if _, err := newTestMCP(f.URL).Call(context.Background(), "conversation_answer", args); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, args) {
		t.Fatalf("sent %s, want %s", got, args)
	}
}

func TestMCPFollowsNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
			return
		}
		writeResult(w, json.RawMessage(`1`), map[string]any{"protocolVersion": DefaultProtocol})
	}))
	defer srv.Close()
	for name, client := range map[string]*http.Client{"the default client": nil, "a client that follows": srv.Client()} {
		c := NewMCPCaller(MCPOptions{BaseURL: srv.URL, Token: testToken, HTTPClient: client})
		var pe *ProtocolError
		if _, err := c.Call(context.Background(), "me_get", nil); !errors.As(err, &pe) || !strings.Contains(pe.Message, "redirected") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
