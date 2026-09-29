package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

func TestNewDerivesProviderCapabilitiesAndDialect(t *testing.T) {
	for _, c := range []struct {
		name     string
		cfg      llm.Config
		provider string
		caps     llm.Capabilities
		dialect  toolschema.Dialect
		maker    string
	}{
		{"openai by default", llm.Config{Model: "gpt-4.1"}, llm.ProviderOpenAI,
			llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true}, toolschema.OpenAI,
			"openai_chat|https://api.openai.com/v1|gpt-4.1"},
		{"deepseek from its host", llm.Config{Model: "deepseek-chat", BaseURL: "https://api.deepseek.com/"}, llm.ProviderDeepSeek,
			llm.Capabilities{ParallelToolCalls: true}, toolschema.OpenAI, "openai_chat|https://api.deepseek.com|deepseek-chat"},
		{"kimi is strict", llm.Config{Model: "kimi-k2", BaseURL: moonshotBase}, llm.ProviderMoonshot,
			llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, StrictTools: true}, toolschema.Kimi, ""},
		{"an unknown host gets the careful defaults", llm.Config{Model: "m", BaseURL: "https://llm.school.example/v1"}, llm.ProviderOpenAICompat,
			llm.Capabilities{}, toolschema.FullCommon, ""},
		{"the configured provider wins over the host", llm.Config{Model: "m", BaseURL: "https://proxy.school.example/v1", Provider: " OpenRouter "},
			llm.ProviderOpenRouter, llm.Capabilities{ParallelToolCalls: true}, toolschema.FullCommon, ""},
		{"overrides apply", llm.Config{Model: "qwen3", BaseURL: qwenBase,
			Capabilities: llm.CapabilityOverrides{ParallelToolCalls: yes(), FileInput: yes(), ToolChoiceNone: no()}},
			llm.ProviderQwen, llm.Capabilities{ParallelToolCalls: true, FileInput: true}, toolschema.FullCommon, ""},
		{"strict_tools turns OpenAI strict", llm.Config{Model: "gpt-4.1", Capabilities: llm.CapabilityOverrides{StrictTools: yes()}},
			llm.ProviderOpenAI, llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true, StrictTools: true},
			toolschema.OpenAIStrict, ""},
		{"strict_tools leaves DeepSeek's dialect to be named", llm.Config{Model: "deepseek-chat", BaseURL: deepseekBase,
			Capabilities: llm.CapabilityOverrides{StrictTools: yes()}},
			llm.ProviderDeepSeek, llm.Capabilities{ParallelToolCalls: true, StrictTools: true}, toolschema.OpenAI, ""},
		{"a query string is not part of the maker", llm.Config{Model: "m", BaseURL: "https://proxy.school.example/v1/?api_key=secret-in-query#x"},
			llm.ProviderOpenAICompat, llm.Capabilities{}, toolschema.FullCommon, "openai_chat|https://proxy.school.example/v1|m"},
		{"a named dialect wins", llm.Config{Model: "gpt-4.1", Dialect: toolschema.FullCommon, Capabilities: llm.CapabilityOverrides{StrictTools: yes()}},
			llm.ProviderOpenAI, llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true, StrictTools: true},
			toolschema.FullCommon, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := New(c.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if a.Name() != llm.AdapterOpenAIChat || a.Provider() != c.provider || a.Capabilities() != c.caps || a.Dialect() != c.dialect {
				t.Errorf("got %s %s %+v %s, want %s %+v %s", a.Name(), a.Provider(), a.Capabilities(), a.Dialect(), c.provider, c.caps, c.dialect)
			}
			if a.Model() != strings.TrimSpace(c.cfg.Model) {
				t.Errorf("model %q", a.Model())
			}
			if c.maker != "" && a.Maker() != c.maker {
				t.Errorf("maker %q, want %q", a.Maker(), c.maker)
			}
		})
	}
}

func TestNewRefusesBadConfiguration(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  llm.Config
		want string
	}{
		{"another adapter", llm.Config{Adapter: llm.AdapterAnthropic, Model: "m"}, "anthropic"},
		{"no model", llm.Config{Model: "  "}, "no model"},
		{"a relative base", llm.Config{Model: "m", BaseURL: "api.openai.com/v1"}, "absolute"},
		{"another scheme", llm.Config{Model: "m", BaseURL: "ftp://example.com/v1"}, "absolute"},
		{"credentials in the base", llm.Config{Model: "m", BaseURL: "https://user:sk-secret-in-url@example.com/v1"}, "credentials"},
		{"an unknown dialect", llm.Config{Model: "m", Dialect: "yaml"}, "dialect"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.cfg.APIKey = testKey
			_, err := New(c.cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one saying %q", err, c.want)
			}
			if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "sk-secret-in-url") {
				t.Errorf("the error holds a secret: %v", err)
			}
		})
	}
}

func TestEndpointKeepsAQueryString(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.openai.com/v1":                           "https://api.openai.com/v1/chat/completions",
		"https://school.openai.azure.com/openai/v1/":          "https://school.openai.azure.com/openai/v1/chat/completions",
		"https://proxy.example/v1?api-version=preview#ignore": "https://proxy.example/v1/chat/completions?api-version=preview",
		"http://localhost:11434":                              "http://localhost:11434/chat/completions",
		"https://api.deepseek.com/chat/completions/":          "https://api.deepseek.com/chat/completions",
	} {
		got, err := endpointURL(base)
		if err != nil || got != want {
			t.Errorf("endpointURL(%q) = %q, %v; want %q", base, got, err, want)
		}
	}
}

func TestHeadersKeepTheKeyOverConfiguredOnes(t *testing.T) {
	h := authHeaders(llm.ProviderOpenAI, testKey, map[string]string{"authorization": "Bearer other", "x-title": "AIshie"})
	if h["Authorization"] != "Bearer "+testKey || h["X-Title"] != "AIshie" || len(h) != 2 {
		t.Errorf("headers = %v", h)
	}
	h = authHeaders(llm.ProviderAzure, testKey, nil)
	if h["Api-Key"] != testKey || h["Authorization"] != "" {
		t.Errorf("azure headers = %v", h)
	}
	if h := authHeaders(llm.ProviderOllama, "", map[string]string{"Authorization": "Bearer local"}); h["Authorization"] != "Bearer local" {
		t.Errorf("without a key the configured header should stand: %v", h)
	}
}

func TestModelFamilies(t *testing.T) {
	for _, c := range []struct {
		model              string
		developer, samples bool
	}{
		{"gpt-4.1", false, true},
		{"gpt-4o-mini", false, true},
		{"o1", true, false},
		{"o3-mini", true, false},
		{"o4-mini-2025-04-16", true, false},
		{"GPT-5", true, false},
		{"gpt-5-mini", true, false},
		{"gpt-5.1", true, false},
		{"gpt-5-chat-latest", true, true},
		{"omni-moderation", false, true},
		{"tutor-deployment", false, true},
		{"ft:gpt-4.1-mini:school::abc123", false, true},
		{"ft:o4-mini-2025-04-16:school::abc123", true, false},
	} {
		if got := developerRole(c.model); got != c.developer {
			t.Errorf("developerRole(%q) = %v", c.model, got)
		}
		if got := !reasoningModel(c.model); got != c.samples {
			t.Errorf("reasoningModel(%q) = %v", c.model, !got)
		}
	}
}

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		raw, args, argsErr string
	}{
		{`"{\"a\":1}"`, `{"a":1}`, ""},
		{`" {\"a\":1} "`, `{"a":1}`, ""},
		{`{"a":1}`, `{"a":1}`, ""},
		{``, `{}`, ""},
		{`null`, `{}`, ""},
		{`""`, `{}`, ""},
		{`"null"`, `{}`, ""},
		{`"{\"a\":"`, `{}`, `{"a":`},
		{`"[1]"`, `{}`, `[1]`},
		{`"42"`, `{}`, `42`},
		{`[1]`, `{}`, `[1]`},
		{`"{\"a\":1} trailing"`, `{}`, `{"a":1} trailing`},
	} {
		args, argsErr := parseArgs(json.RawMessage(c.raw))
		if string(args) != c.args || argsErr != c.argsErr {
			t.Errorf("parseArgs(%s) = %s, %q; want %s, %q", c.raw, args, argsErr, c.args, c.argsErr)
		}
	}
}

func TestStop(t *testing.T) {
	for _, c := range []struct {
		finish            string
		hasText, hasCalls bool
		want              llm.Stop
	}{
		{"stop", false, false, llm.StopEnd},
		{"tool_calls", false, true, llm.StopToolCalls},
		{"function_call", false, true, llm.StopToolCalls},
		{"tool_calls", true, false, llm.StopToolError},
		{"tool_calls", false, false, llm.StopToolError},
		{"length", true, false, llm.StopMaxTokens},
		{"content_filter", false, false, llm.StopContentFilter},
		{"sensitive", false, false, llm.StopContentFilter},
		{"model_context_window_exceeded", false, false, llm.StopContextOverflow},
		{"network_error", false, false, llm.StopError},
		{"error", true, false, llm.StopError},
		{"insufficient_system_resource", false, false, llm.StopError},
		{"", true, false, llm.StopEnd},
		{"", false, false, llm.StopError},
		{"weird", true, false, llm.StopEnd},
	} {
		if got := stop(c.finish, c.hasText, c.hasCalls); got != c.want {
			t.Errorf("stop(%q, %v, %v) = %s, want %s", c.finish, c.hasText, c.hasCalls, got, c.want)
		}
	}
}

// TestFragmentsGoBackVerbatim holds §3.6: what a provider asked to have
// back (usage too, for the ledger) is kept byte for byte, escapes and all.
func TestFragmentsGoBackVerbatim(t *testing.T) {
	const reasoning = `"a <b> & é \"quoted\""`
	const usage = `{ "prompt_tokens" : 12,  "completion_tokens":3, "x": [ 1, 2 ] }`
	reply := `{"id":"v","choices":[{"message":{"content":"ok","reasoning_content":` + reasoning + `},"finish_reason":"stop"}],"usage":` + usage + `}`
	rt := &replay{status: http.StatusOK, body: reply}
	c := cfg(deepseekBase, "deepseek-reasoner")
	c.HTTPClient = &http.Client{Transport: rt}
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{question()}})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Usage.Raw) != usage {
		t.Errorf("raw usage = %s, want %s", resp.Usage.Raw, usage)
	}
	if want := `{"reasoning_content":` + reasoning + `}`; string(resp.Parts[0].Opaque) != want {
		t.Errorf("opaque = %s, want %s", resp.Parts[0].Opaque, want)
	}

	next := &llm.Request{Messages: []llm.Message{question(), {Role: llm.RoleAssistant, Parts: resp.Parts}, llm.UserText("And?")}}
	if _, err := a.Call(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	// On the wire the fragment is the same JSON value; the encoder may
	// write its string's escapes differently, which no reader can tell.
	var sent struct {
		Body struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		} `json:"body"`
	}
	if err := json.Unmarshal(rt.sent, &sent); err != nil {
		t.Fatal(err)
	}
	var got, want string
	if err := json.Unmarshal(sent.Body.Messages[1]["reasoning_content"], &got); err != nil {
		t.Fatalf("no reasoning_content went back: %v\n%s", err, rt.sent)
	}
	_ = json.Unmarshal([]byte(reasoning), &want)
	if got != want {
		t.Errorf("reasoning_content went back as %q, want %q", got, want)
	}
}

func TestMessagesWithNothingToSayAreLeftOut(t *testing.T) {
	a, err := New(cfg(deepseekBase, "deepseek-chat"))
	if err != nil {
		t.Fatal(err)
	}
	body := a.request(&llm.Request{Messages: []llm.Message{
		question(),
		assistant(llm.Part{Type: llm.PartReasoning, Maker: "someone else", Opaque: json.RawMessage(`{"reasoning_content":"x"}`)}),
		{Role: llm.RoleUser, Parts: []llm.Part{llm.Text("")}},
		assistant(llm.Part{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: json.RawMessage(`{"reasoning_content":"mine"}`)}),
		assistant(
			llm.Part{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: json.RawMessage(`{"reasoning_content":"one ","reasoning_details":[1]}`)},
			llm.Part{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: json.RawMessage(`{"reasoning_content":"two","reasoning_details":[2]}`)},
			llm.Text("Hi.")),
	}})
	b, err := marshal(body.Messages)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"content":"Why did I lose marks on HW3?","role":"user"},` +
		`{"content":"","reasoning_content":"mine","role":"assistant"},` +
		`{"content":"Hi.","reasoning_content":"one two","reasoning_details":[1,2],"role":"assistant"}]`
	if !bytes.Equal(canonical(t, b), canonical(t, []byte(want))) {
		t.Errorf("messages\n got %s\nwant %s", b, want)
	}
}

func TestCallWithoutARequest(t *testing.T) {
	a, err := New(cfg("", "gpt-4.1"))
	if err != nil {
		t.Fatal(err)
	}
	var e *llm.Error
	if _, err := a.Call(context.Background(), nil); !errors.As(err, &e) || e.Kind != llm.ErrBadRequest {
		t.Errorf("err = %v", err)
	}
	bad := &llm.Request{Messages: []llm.Message{question()}, Tools: []llm.Tool{{Name: "x", Schema: json.RawMessage(`{not json`)}}}
	if _, err := a.Call(context.Background(), bad); !errors.As(err, &e) || e.Kind != llm.ErrBadRequest {
		t.Errorf("a schema that is not JSON: err = %v", err)
	}
}

// server is an httptest server that answers every call with status, headers
// and body. seen returns a copy of each request so far, with its body.
func server(t *testing.T, status int, header http.Header, body string) (ts *httptest.Server, seen func() ([]*http.Request, [][]byte)) {
	t.Helper()
	var mu sync.Mutex
	var reqs []*http.Request
	var bodies [][]byte
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, r.Clone(context.Background()))
		bodies = append(bodies, b)
		mu.Unlock()
		for k, v := range header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts, func() ([]*http.Request, [][]byte) {
		mu.Lock()
		defer mu.Unlock()
		return append([]*http.Request(nil), reqs...), append([][]byte(nil), bodies...)
	}
}

func TestHTTPEndToEnd(t *testing.T) {
	ts, seen := server(t, http.StatusOK, nil, textReply("stop", "Hello."))
	c := cfg(ts.URL+"/v1/", "gpt-4.1")
	c.Provider = llm.ProviderOpenAI
	c.Headers = map[string]string{"X-Title": "AIshie"}
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Call(context.Background(), &llm.Request{System: system, Messages: []llm.Message{question()}, Tools: tools, ToolMode: llm.ToolAuto})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "Hello." || resp.Stop != llm.StopEnd || resp.Usage.Input != 40 || resp.Usage.Output != 9 {
		t.Errorf("resp = %+v", resp)
	}
	reqs, bodies := seen()
	r := reqs[0]
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+testKey ||
		r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Title") != "AIshie" {
		t.Errorf("request %s %s %v", r.Method, r.URL, r.Header)
	}
	var sent struct {
		Model    string            `json:"model"`
		Stream   *bool             `json:"stream"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "gpt-4.1" || sent.Stream == nil || *sent.Stream || len(sent.Tools) != 2 || len(sent.Messages) != 2 {
		t.Errorf("body = %s", bodies[0])
	}
}

func TestHTTPErrorsAreClassified(t *testing.T) {
	for _, c := range []struct {
		name       string
		status     int
		header     http.Header
		body       string
		kind       llm.ErrorKind
		code       string
		retryAfter time.Duration
		retryable  bool
	}{
		{"429 with Retry-After", 429, http.Header{"Retry-After": {"7"}},
			`{"error":{"message":"Rate limit reached for gpt-4.1","type":"requests","code":"rate_limit_exceeded"}}`,
			llm.ErrRateLimited, "rate_limit_exceeded", 7 * time.Second, true},
		{"429 insufficient_quota", 429, nil,
			`{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","code":"insufficient_quota"}}`,
			llm.ErrAuth, "insufficient_quota", 0, false},
		{"400 context_length_exceeded", 400, nil,
			`{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
			llm.ErrContextOverflow, "context_length_exceeded", 0, false},
		{"400 content policy", 400, nil,
			`{"error":{"message":"The response was filtered due to the prompt triggering Azure OpenAI's content management policy.","type":null,"param":"prompt","code":"content_filter"}}`,
			llm.ErrContentFilter, "content_filter", 0, false},
		{"400 content policy with Azure's numeric status", 400, nil,
			`{"error":{"message":"The response was filtered due to the prompt triggering Azure OpenAI's content management policy.","type":null,"param":"prompt","code":"content_filter","status":400,"innererror":{"code":"ResponsibleAIPolicyViolation"}}}`,
			llm.ErrContentFilter, "content_filter", 0, false},
		{"a numeric status on a server error", 500, nil, `{"error":{"message":"boom","code":"server_error","status":500}}`, llm.ErrServer, "server_error", 0, true},
		{"400 bad request", 400, nil,
			`{"error":{"message":"Invalid value for 'tool_choice'.","type":"invalid_request_error","code":"invalid_value"}}`,
			llm.ErrBadRequest, "invalid_value", 0, false},
		{"401", 401, nil,
			`{"error":{"message":"Incorrect API key provided: sk-test-****6789.","type":"invalid_request_error","code":"invalid_api_key"}}`,
			llm.ErrAuth, "invalid_api_key", 0, false},
		{"500", 500, nil, `{"error":{"message":"The server had an error.","type":"server_error"}}`, llm.ErrServer, "server_error", 0, true},
		{"503 overloaded", 503, http.Header{"Retry-After-Ms": {"1500"}}, `{"error":{"message":"overloaded"}}`, llm.ErrOverloaded, "", 1500 * time.Millisecond, true},
		{"502 with an HTML page", 502, nil, `<html>Bad gateway</html>`, llm.ErrServer, "", 0, true},
		{"a 200 that is not JSON", 200, nil, `<html>oops</html>`, llm.ErrServer, "", 0, true},
		{"a 200 without choices", 200, nil, `{"id":"x","choices":[]}`, llm.ErrServer, "", 0, true},
		{"a 200 with an error", 200, nil, `{"error":{"message":"model overloaded, try later","type":"server_error"}}`, llm.ErrServer, "server_error", 0, true},
		{"a 200 with a context error", 200, nil, `{"error":{"message":"prompt is too long","type":"invalid_request_error"}}`, llm.ErrContextOverflow, "invalid_request_error", 0, false},
		{"a 200 with an invalid request", 200, nil, `{"error":{"message":"unknown field","code":"invalid_request_error"}}`, llm.ErrBadRequest, "invalid_request_error", 0, false},
		{"a 200 with a rate limit", 200, http.Header{"Retry-After": {"2"}}, `{"error":{"message":"slow down","code":"rate_limit_exceeded"}}`, llm.ErrRateLimited, "rate_limit_exceeded", 2 * time.Second, true},
		{"a 200 with a quota", 200, nil, `{"error":{"message":"no credit","code":"insufficient_quota"}}`, llm.ErrAuth, "insufficient_quota", 0, false},
		{"a 200 with a bad key", 200, nil, `{"error":{"message":"bad key","type":"authentication_error"}}`, llm.ErrAuth, "authentication_error", 0, false},
		{"a 200 with a plain error", 200, nil, `{"error":"model not loaded"}`, llm.ErrServer, "", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts, _ := server(t, c.status, c.header, c.body)
			a, err := New(cfg(ts.URL+"/v1", "gpt-4.1"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Call(context.Background(), &llm.Request{Messages: []llm.Message{question()}})
			var e *llm.Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v (%T), want *llm.Error", err, err)
			}
			if e.Kind != c.kind || e.Code != c.code || e.RetryAfter != c.retryAfter || e.Retryable() != c.retryable || e.Status != c.status {
				t.Errorf("got %s status %d code %q retry after %s retryable %v; want %s %d %q %s %v",
					e.Kind, e.Status, e.Code, e.RetryAfter, e.Retryable(), c.kind, c.status, c.code, c.retryAfter, c.retryable)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the error holds the key: %v", err)
			}
		})
	}
}

func TestTheKeyNeverReachesAnError(t *testing.T) {
	ts, _ := server(t, 401, nil, fmt.Sprintf(`{"error":{"message":"Bad key %s","code":"%s"}}`, testKey, testKey))
	a, err := New(cfg(ts.URL+"/v1", "gpt-4.1"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Call(context.Background(), &llm.Request{Messages: []llm.Message{question()}})
	if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("err = %v", err)
	}

	ts.Close() // nothing listens now: a network error
	_, err = a.Call(context.Background(), &llm.Request{Messages: []llm.Message{question()}})
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrNetwork || strings.Contains(err.Error(), testKey) {
		t.Errorf("err = %v", err)
	}
}

func TestACallEndsWithItsContext(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer ts.Close()
	defer close(release)
	a, err := New(cfg(ts.URL, "gpt-4.1"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = a.Call(ctx, &llm.Request{Messages: []llm.Message{question()}})
	var e *llm.Error
	if !errors.As(err, &e) || e.Kind != llm.ErrTimeout || !e.Retryable() {
		t.Errorf("err = %v", err)
	}
}

func TestConcurrentCalls(t *testing.T) {
	ts, seen := server(t, http.StatusOK, nil, textReply("stop", "Hello."))
	a, err := New(cfg(ts.URL, "gpt-4.1"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText(fmt.Sprint("q", i))}})
			if err == nil && resp.Text() != "Hello." {
				err = fmt.Errorf("text %q", resp.Text())
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if reqs, _ := seen(); len(reqs) != 16 {
		t.Errorf("%d requests", len(reqs))
	}
}
