package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// recorded is one request a test server saw.
type recorded struct {
	method, path string
	header       http.Header
	body         []byte
}

// server answers every request with status, header and body, and keeps
// what it was sent.
func server(t *testing.T, status int, header map[string]string, body string) (*httptest.Server, func() []recorded) {
	t.Helper()
	var mu sync.Mutex
	var seen []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, recorded{r.Method, r.URL.Path, r.Header.Clone(), b})
		mu.Unlock()
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), seen...)
	}
}

// One call, end to end: the request's URL, headers and body, and the
// answer read back.
func TestCallEndToEnd(t *testing.T) {
	srv, seen := server(t, http.StatusOK, map[string]string{"request-id": "req_e2e"}, string(readGolden(t, "response_parallel_tools.anthropic.json")))
	a := newAdapter(t, llm.Config{
		BaseURL: srv.URL,
		Headers: map[string]string{"anthropic-beta": "some-feature-2026-01-01", "anthropic-version": "1999-01-01", "x-api-key": "not-this-one"},
	})
	resp, err := a.Call(context.Background(), &llm.Request{
		System: "You are CS101's tutor.", Messages: []llm.Message{question}, ToolMode: llm.ToolAuto,
		Tools: []llm.Tool{gradeList, assignmentGet},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	reqs := seen()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPost || r.path != "/v1/messages" {
		t.Errorf("%s %s, want POST /v1/messages", r.method, r.path)
	}
	for k, want := range map[string]string{
		"X-Api-Key":         testKey,
		"Anthropic-Version": APIVersion,
		"Anthropic-Beta":    "some-feature-2026-01-01",
		"Content-Type":      "application/json",
		"Authorization":     "",
	} {
		if got := r.header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	var body wireRequest
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatalf("the body does not decode: %v", err)
	}
	if body.Model != "claude-sonnet-4-5" || body.MaxTokens != DefaultMaxTokens || len(body.Tools) != 2 || len(body.Messages) != 1 {
		t.Errorf("body = %s", r.body)
	}

	if resp.Stop != llm.StopToolCalls || resp.RawStop != "tool_use" {
		t.Errorf("stop %q (%q), want tool_calls (tool_use)", resp.Stop, resp.RawStop)
	}
	calls := resp.ToolCalls()
	if len(calls) != 2 || calls[0].Name != "grade_list" || calls[1].ID != "toolu_01B18r81rx81mr826724mr8" {
		t.Errorf("calls = %+v", calls)
	}
	if resp.RequestID != "req_e2e" || resp.Model != "claude-sonnet-4-5-20250929" {
		t.Errorf("request id %q, model %q", resp.RequestID, resp.Model)
	}
	if resp.Usage.Input != 1402 || resp.Usage.Output != 118 || len(resp.Usage.Raw) == 0 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestEndpoints(t *testing.T) {
	cases := []struct{ base, want string }{
		{"", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1/messages", "https://api.anthropic.com/v1/messages"},
		{"https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/messages"},
		{"https://openrouter.ai/api/v1/", "https://openrouter.ai/api/v1/messages"},
		{"https://openrouter.ai/api", "https://openrouter.ai/api/v1/messages"},
		{"https://api.deepseek.com/anthropic", "https://api.deepseek.com/anthropic/v1/messages"},
		{"http://localhost:11434", "http://localhost:11434/v1/messages"},
		{"http://127.0.0.1:1234/v1", "http://127.0.0.1:1234/v1/messages"},
		{"https://gateway.example.edu/llm/anthropic?team=cs", "https://gateway.example.edu/llm/anthropic/v1/messages?team=cs"},
	}
	for _, c := range cases {
		a := newAdapter(t, llm.Config{BaseURL: c.base})
		if got := a.Endpoint(); got != c.want {
			t.Errorf("base %q: endpoint %q, want %q", c.base, got, c.want)
		}
	}
}

func TestNewRefuses(t *testing.T) {
	cases := []struct {
		name string
		cfg  llm.Config
	}{
		{"another adapter", llm.Config{Adapter: llm.AdapterOpenAIChat, Model: "m"}},
		{"no model", llm.Config{Adapter: llm.AdapterAnthropic}},
		{"an unknown effort", llm.Config{Model: "m", Reasoning: llm.Reasoning{Effort: "xhigh"}}},
		{"a negative cap", llm.Config{Model: "m", Params: llm.Params{MaxOutputTokens: -1}}},
		{"a base that is not a URL", llm.Config{Model: "m", BaseURL: "api.anthropic.com"}},
		{"a base with another scheme", llm.Config{Model: "m", BaseURL: "ftp://api.anthropic.com"}},
		{"a key in the base", llm.Config{Model: "m", BaseURL: "https://user:" + testKey + "@api.anthropic.com"}},
		{"an unknown dialect", llm.Config{Model: "m", Dialect: "yaml"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.cfg)
			if err == nil {
				t.Fatal("New accepted it")
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the error shows the key: %v", err)
			}
		})
	}
}

func TestIdentity(t *testing.T) {
	cases := []struct {
		name     string
		cfg      llm.Config
		provider string
		maker    string
		caps     llm.Capabilities
		dialect  toolschema.Dialect
	}{
		{
			name:     "Anthropic",
			cfg:      llm.Config{Model: "claude-sonnet-4-5"},
			provider: llm.ProviderAnthropic,
			maker:    "anthropic|https://api.anthropic.com|claude-sonnet-4-5",
			caps:     llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true},
			dialect:  toolschema.Anthropic,
		},
		{
			name:     "OpenRouter takes no files by default",
			cfg:      llm.Config{Model: "anthropic/claude-sonnet-4.5", BaseURL: "https://openrouter.ai/api/v1/"},
			provider: llm.ProviderOpenRouter,
			maker:    "anthropic|https://openrouter.ai/api/v1|anthropic/claude-sonnet-4.5",
			caps:     llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true},
			dialect:  toolschema.Anthropic,
		},
		{
			name: "overrides",
			cfg: llm.Config{
				Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/anthropic", Dialect: toolschema.FullCommon,
				Capabilities: llm.CapabilityOverrides{ParallelToolCalls: ptr(false), StrictTools: ptr(true), FileInput: ptr(true)},
			},
			provider: llm.ProviderDeepSeek,
			maker:    "anthropic|https://api.deepseek.com/anthropic|deepseek-chat",
			caps:     llm.Capabilities{StrictTools: true, ToolChoiceNone: true, FileInput: true},
			dialect:  toolschema.FullCommon,
		},
		{
			name:     "a provider named",
			cfg:      llm.Config{Model: "claude-sonnet-4-5", BaseURL: "https://gateway.example.edu", Provider: llm.ProviderAnthropic},
			provider: llm.ProviderAnthropic,
			maker:    "anthropic|https://gateway.example.edu|claude-sonnet-4-5",
			caps:     llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true},
			dialect:  toolschema.Anthropic,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAdapter(t, c.cfg)
			if a.Name() != llm.AdapterAnthropic || a.Model() != c.cfg.Model {
				t.Errorf("name %q, model %q", a.Name(), a.Model())
			}
			if a.Provider() != c.provider || a.Maker() != c.maker || a.Capabilities() != c.caps || a.Dialect() != c.dialect {
				t.Errorf("provider %q, maker %q, caps %+v, dialect %q; want %q, %q, %+v, %q",
					a.Provider(), a.Maker(), a.Capabilities(), a.Dialect(), c.provider, c.maker, c.caps, c.dialect)
			}
		})
	}
}

// OpenRouter documents a bearer token; Anthropic refuses a request with
// both, so only OpenRouter gets one.
func TestKeyHeaders(t *testing.T) {
	for base, bearer := range map[string]bool{
		"https://openrouter.ai/api/v1":       true,
		"https://api.anthropic.com":          false,
		"https://api.deepseek.com/anthropic": false,
	} {
		h := newAdapter(t, llm.Config{BaseURL: base}).headers
		if h["X-Api-Key"] != testKey {
			t.Errorf("%s: x-api-key = %q", base, h["X-Api-Key"])
		}
		if got := h["Authorization"] == "Bearer "+testKey; got != bearer {
			t.Errorf("%s: bearer %v, want %v", base, got, bearer)
		}
	}
	// A server that takes no key gets none.
	a, err := New(llm.Config{Model: "qwen3", BaseURL: "http://localhost:11434"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.headers["X-Api-Key"]; ok {
		t.Error("a key header was sent without a key")
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		header     map[string]string
		body       string
		kind       llm.ErrorKind
		code       string
		retryAfter time.Duration
	}{
		{"overloaded", 529, nil, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_1"}`,
			llm.ErrOverloaded, "overloaded_error", 0},
		{"overloaded under another status", http.StatusBadGateway, nil, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			llm.ErrOverloaded, "overloaded_error", 0},
		{"rate limited", http.StatusTooManyRequests, map[string]string{"retry-after": "7"},
			`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`,
			llm.ErrRateLimited, "rate_limit_error", 7 * time.Second},
		{"prompt too long", http.StatusBadRequest, nil,
			`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 208310 tokens > 200000 maximum"}}`,
			llm.ErrContextOverflow, "invalid_request_error", 0},
		{"input and max_tokens over the context", http.StatusBadRequest, nil,
			`{"type":"error","error":{"type":"invalid_request_error","message":"input length and ` + "`max_tokens`" + ` exceed context limit: 188240 + 21333 > 200000, decrease input length or ` + "`max_tokens`" + ` and try again"}}`,
			llm.ErrContextOverflow, "invalid_request_error", 0},
		{"request too large", http.StatusRequestEntityTooLarge, nil,
			`{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`,
			llm.ErrContextOverflow, "request_too_large", 0},
		{"too large, from a proxy", http.StatusRequestEntityTooLarge, nil, `<html>413 Request Entity Too Large</html>`,
			llm.ErrContextOverflow, "", 0},
		{"a bad request", http.StatusBadRequest, nil,
			`{"type":"error","error":{"type":"invalid_request_error","message":"messages: roles must alternate between \"user\" and \"assistant\""}}`,
			llm.ErrBadRequest, "invalid_request_error", 0},
		{"no credit", http.StatusBadRequest, nil,
			`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`,
			llm.ErrAuth, "invalid_request_error", 0},
		{"a bad key", http.StatusUnauthorized, nil, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			llm.ErrAuth, "authentication_error", 0},
		{"billing", http.StatusPaymentRequired, nil, `{"type":"error","error":{"type":"billing_error","message":"billing"}}`,
			llm.ErrAuth, "billing_error", 0},
		{"forbidden", http.StatusForbidden, nil, `{"type":"error","error":{"type":"permission_error","message":"no"}}`,
			llm.ErrAuth, "permission_error", 0},
		{"no such model", http.StatusNotFound, nil, `{"type":"error","error":{"type":"not_found_error","message":"model: claude-nope"}}`,
			llm.ErrBadRequest, "not_found_error", 0},
		{"a fault of Anthropic's", http.StatusInternalServerError, nil, `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
			llm.ErrServer, "api_error", 0},
		{"a timeout of Anthropic's", http.StatusGatewayTimeout, nil, `{"type":"error","error":{"type":"timeout_error","message":"Request timed out"}}`,
			llm.ErrTimeout, "timeout_error", 0},
		{"a 2xx that does not decode", http.StatusOK, nil, `not json`, llm.ErrServer, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := server(t, c.status, c.header, c.body)
			a := newAdapter(t, llm.Config{BaseURL: srv.URL})
			_, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{question}})
			var le *llm.Error
			if !errors.As(err, &le) {
				t.Fatalf("err = %v, want an *llm.Error", err)
			}
			if le.Kind != c.kind || le.Code != c.code || le.RetryAfter != c.retryAfter {
				t.Errorf("kind %s, code %q, retry after %s; want %s, %q, %s", le.Kind, le.Code, le.RetryAfter, c.kind, c.code, c.retryAfter)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the error shows the key: %v", err)
			}
		})
	}
}

func TestTransportErrors(t *testing.T) {
	// A server that has gone away.
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	a := newAdapter(t, llm.Config{BaseURL: base})
	_, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{question}})
	var le *llm.Error
	if !errors.As(err, &le) || le.Kind != llm.ErrNetwork {
		t.Errorf("a closed server: err = %v, want network", err)
	}
	if err != nil && strings.Contains(err.Error(), testKey) {
		t.Errorf("the error shows the key: %v", err)
	}

	// A server slower than the call's deadline.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	a = newAdapter(t, llm.Config{BaseURL: slow.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = a.Call(ctx, &llm.Request{Messages: []llm.Message{question}})
	if !errors.As(err, &le) || le.Kind != llm.ErrTimeout {
		t.Errorf("a slow server: err = %v, want timeout", err)
	}
}

// A request the adapter cannot make is refused before anything is sent.
func TestBadRequestsSendNothing(t *testing.T) {
	srv, seen := server(t, http.StatusOK, nil, `{}`)
	a := newAdapter(t, llm.Config{BaseURL: srv.URL})
	_, err := a.Call(context.Background(), &llm.Request{})
	var le *llm.Error
	if !errors.As(err, &le) || le.Kind != llm.ErrBadRequest {
		t.Errorf("err = %v, want bad_request", err)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("%d requests sent, want none", n)
	}
}

func TestFamilyOf(t *testing.T) {
	budget := family{}
	adaptive46 := family{adaptive: true}
	adaptive := family{adaptive: true, noSampling: true}
	always := family{adaptive: true, thinksByDefault: true, noSampling: true}
	for model, want := range map[string]family{
		"claude-3-7-sonnet-20250219":                   budget,
		"claude-3-5-haiku-latest":                      budget,
		"claude-sonnet-4-20250514":                     budget,
		"claude-opus-4-1":                              budget,
		"claude-opus-4-5@20251101":                     budget,
		"claude-haiku-4-5":                             budget,
		"claude-sonnet-4-5-20250929":                   budget,
		"anthropic/claude-sonnet-4.5":                  budget,
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0": budget,
		"claude-sonnet-4-6":                            adaptive46,
		"Claude-Opus-4-6":                              adaptive46,
		"claude-opus-4-7":                              adaptive,
		"anthropic/claude-opus-4.8":                    adaptive,
		"claude-opus-5":                                always,
		"claude-opus-5-5":                              always,
		"claude-sonnet-5":                              always,
		"claude-fable-5-1":                             always,
		"claude-mythos-5":                              always,
		"claude-haiku-6":                               always,
		"deepseek-chat":                                budget,
		"glm-4.6":                                      budget,
		"":                                             budget,
	} {
		if got := familyOf(model); got != want {
			t.Errorf("familyOf(%q) = %+v, want %+v", model, got, want)
		}
	}
}
