package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

func TestNew(t *testing.T) {
	yes, no := ptr(true), ptr(false)
	tests := []struct {
		name         string
		cfg          llm.Config
		wantProvider string
		wantEndpoint string
		wantMaker    string
		wantDialect  toolschema.Dialect
		wantCaps     llm.Capabilities
	}{
		{
			name:         "OpenAI by default",
			cfg:          llm.Config{Model: "gpt-5"},
			wantProvider: llm.ProviderOpenAI,
			wantEndpoint: "https://api.openai.com/v1/responses",
			wantMaker:    "openai_responses|https://api.openai.com/v1|gpt-5",
			wantDialect:  toolschema.OpenAI,
			wantCaps:     llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true},
		},
		{
			name:         "Azure v1, detected from the host",
			cfg:          llm.Config{Adapter: llm.AdapterOpenAIResponses, Model: "tutor-deployment", BaseURL: "https://school.openai.azure.com/openai/v1/"},
			wantProvider: llm.ProviderAzure,
			wantEndpoint: "https://school.openai.azure.com/openai/v1/responses",
			wantMaker:    "openai_responses|https://school.openai.azure.com/openai/v1|tutor-deployment",
			wantDialect:  toolschema.OpenAI,
			wantCaps:     llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true},
		},
		{
			name:         "a bare Azure resource gets the v1 path",
			cfg:          llm.Config{Model: "tutor-deployment", BaseURL: "https://school.cognitiveservices.azure.com"},
			wantProvider: llm.ProviderAzure,
			wantEndpoint: "https://school.cognitiveservices.azure.com/openai/v1/responses",
			wantMaker:    "openai_responses|https://school.cognitiveservices.azure.com/openai/v1|tutor-deployment",
			wantDialect:  toolschema.OpenAI,
			wantCaps:     llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true},
		},
		{
			name:         "a proxy, provider named",
			cfg:          llm.Config{Model: "gpt-5", BaseURL: "http://litellm.internal:4000/v1", Provider: "openai"},
			wantProvider: llm.ProviderOpenAI,
			wantEndpoint: "http://litellm.internal:4000/v1/responses",
			wantMaker:    "openai_responses|http://litellm.internal:4000/v1|gpt-5",
			wantDialect:  toolschema.OpenAI,
			wantCaps:     llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true},
		},
		{
			name:         "overrides",
			cfg:          llm.Config{Model: "gpt-5", Capabilities: llm.CapabilityOverrides{ParallelToolCalls: no, ToolChoiceNone: no, FileInput: no}},
			wantProvider: llm.ProviderOpenAI,
			wantEndpoint: "https://api.openai.com/v1/responses",
			wantMaker:    "openai_responses|https://api.openai.com/v1|gpt-5",
			wantDialect:  toolschema.OpenAI,
			wantCaps:     llm.Capabilities{},
		},
		{
			name:         "strict tools choose the strict dialect",
			cfg:          llm.Config{Model: "gpt-5", Capabilities: llm.CapabilityOverrides{StrictTools: yes}},
			wantProvider: llm.ProviderOpenAI,
			wantEndpoint: "https://api.openai.com/v1/responses",
			wantMaker:    "openai_responses|https://api.openai.com/v1|gpt-5",
			wantDialect:  toolschema.OpenAIStrict,
			wantCaps:     llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true, StrictTools: true},
		},
		{
			name:         "a non-strict dialect named outranks strict tools",
			cfg:          llm.Config{Model: "gpt-5", Dialect: toolschema.OpenAI, Capabilities: llm.CapabilityOverrides{StrictTools: yes}},
			wantProvider: llm.ProviderOpenAI,
			wantEndpoint: "https://api.openai.com/v1/responses",
			wantMaker:    "openai_responses|https://api.openai.com/v1|gpt-5",
			wantDialect:  toolschema.OpenAI,
			wantCaps:     llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true},
		},
		{
			name:         "the strict dialect named",
			cfg:          llm.Config{Model: "gpt-5", Dialect: toolschema.OpenAIStrict},
			wantProvider: llm.ProviderOpenAI,
			wantEndpoint: "https://api.openai.com/v1/responses",
			wantMaker:    "openai_responses|https://api.openai.com/v1|gpt-5",
			wantDialect:  toolschema.OpenAIStrict,
			wantCaps:     llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true, StrictTools: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := New(tt.cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if a.Name() != llm.AdapterOpenAIResponses {
				t.Errorf("Name = %q", a.Name())
			}
			if a.Model() != tt.cfg.Model {
				t.Errorf("Model = %q, want %q", a.Model(), tt.cfg.Model)
			}
			if a.Provider() != tt.wantProvider {
				t.Errorf("Provider = %q, want %q", a.Provider(), tt.wantProvider)
			}
			if a.endpoint != tt.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", a.endpoint, tt.wantEndpoint)
			}
			if a.Maker() != tt.wantMaker {
				t.Errorf("Maker = %q, want %q", a.Maker(), tt.wantMaker)
			}
			if a.Dialect() != tt.wantDialect {
				t.Errorf("Dialect = %q, want %q", a.Dialect(), tt.wantDialect)
			}
			if a.Capabilities() != tt.wantCaps {
				t.Errorf("Capabilities = %+v, want %+v", a.Capabilities(), tt.wantCaps)
			}
		})
	}
}

func TestNewRefuses(t *testing.T) {
	tests := []struct {
		name string
		cfg  llm.Config
		want string
	}{
		{"another adapter's configuration", llm.Config{Adapter: llm.AdapterOpenAIChat, Model: "gpt-5"}, "adapter"},
		{"no model", llm.Config{Model: " "}, "no model"},
		{"Azure without a base", llm.Config{Model: "d", Provider: llm.ProviderAzure}, "Azure needs base_url"},
		{"not a URL", llm.Config{Model: "gpt-5", BaseURL: "://nope"}, "not a URL"},
		{"no scheme", llm.Config{Model: "gpt-5", BaseURL: "api.openai.com/v1"}, "http or https"},
		{"another scheme", llm.Config{Model: "gpt-5", BaseURL: "ftp://api.openai.com/v1"}, "http or https"},
		{"a query string", llm.Config{Model: "d", BaseURL: "https://school.openai.azure.com/openai/v1?api-version=preview"}, "query string"},
		{"credentials in the URL", llm.Config{Model: "gpt-5", BaseURL: "https://user:secret@proxy.example/v1"}, "credentials"},
		{"an unknown dialect", llm.Config{Model: "gpt-5", Dialect: "openapi"}, "dialect"},
		{"Authorization among the headers", llm.Config{Model: "gpt-5", Headers: map[string]string{"authorization": "Bearer x"}}, "may not be configured"},
		{"api-key among the headers", llm.Config{Model: "gpt-5", Headers: map[string]string{"API-KEY": "x"}}, "may not be configured"},
		{"a header name that is no token", llm.Config{Model: "gpt-5", Headers: map[string]string{"X Trace": "1"}}, "not a valid HTTP header"},
		{"a header value that splits the request", llm.Config{Model: "gpt-5", Headers: map[string]string{"X-Trace": "1\r\nX-Injected: secret"}}, "not a valid HTTP header"},
		{"a key no header can carry", llm.Config{Model: "gpt-5", APIKey: "sk-secret\x00key"}, "API key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if err == nil {
				t.Fatal("New accepted it")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not say %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error %q repeats a credential", err)
			}
		})
	}
}

// TestAuth checks where each provider's key goes, and that extra headers
// ride along.
func TestAuth(t *testing.T) {
	tests := []struct {
		name       string
		cfg        func(*llm.Config)
		wantHost   string
		wantPath   string
		wantHeader map[string]string
	}{
		{
			name:       "OpenAI: a bearer",
			cfg:        func(c *llm.Config) { c.Headers = map[string]string{"OpenAI-Organization": "org-school"} },
			wantHost:   "api.openai.com",
			wantPath:   "/v1/responses",
			wantHeader: map[string]string{"Authorization": "Bearer " + testKey, "Api-Key": "", "Openai-Organization": "org-school", "Content-Type": "application/json"},
		},
		{
			name: "Azure: api-key",
			cfg: func(c *llm.Config) {
				c.BaseURL = "https://school.openai.azure.com/openai/v1"
				c.Model = "tutor-deployment"
			},
			wantHost:   "school.openai.azure.com",
			wantPath:   "/openai/v1/responses",
			wantHeader: map[string]string{"Api-Key": testKey, "Authorization": ""},
		},
		{
			name:       "a key read from a file, newline and all",
			cfg:        func(c *llm.Config) { c.APIKey = " " + testKey + "\n" },
			wantHost:   "api.openai.com",
			wantPath:   "/v1/responses",
			wantHeader: map[string]string{"Authorization": "Bearer " + testKey},
		},
		{
			name:       "no key: no credentials",
			cfg:        func(c *llm.Config) { c.APIKey = ""; c.BaseURL = "http://litellm.internal:4000/v1" },
			wantHost:   "litellm.internal:4000",
			wantPath:   "/v1/responses",
			wantHeader: map[string]string{"Api-Key": "", "Authorization": ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := fixture(t, "text")
			a := newAdapter(t, p, tt.cfg)
			if _, err := a.Call(context.Background(), plainRequest(a)); err != nil {
				t.Fatalf("Call: %v", err)
			}
			got := p.last(t)
			if got.Host != tt.wantHost || got.Path != tt.wantPath {
				t.Errorf("sent to %s%s, want %s%s", got.Host, got.Path, tt.wantHost, tt.wantPath)
			}
			for k, want := range tt.wantHeader {
				if v := got.Header.Get(k); v != want {
					t.Errorf("header %s = %q, want %q", k, v, want)
				}
			}
			var body map[string]any
			if err := json.Unmarshal(got.Body, &body); err != nil {
				t.Fatal(err)
			}
			if body["model"] != a.Model() {
				t.Errorf("model = %v, want %q", body["model"], a.Model())
			}
			if store, ok := body["store"].(bool); !ok || store {
				t.Errorf("store = %v, want false, always", body["store"])
			}
			if strings.Contains(string(got.Body), testKey) {
				t.Error("the key is in the request body")
			}
		})
	}
}

// TestErrors checks that refusals come back classified, and that no error
// repeats the key.
func TestErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		header     map[string]string
		body       string
		wantKind   llm.ErrorKind
		wantCode   string
		wantRetry  time.Duration
		retryable  bool
		wantInText string
	}{
		{
			name: "a bad key", status: http.StatusUnauthorized,
			body:     `{"error":{"message":"Incorrect API key provided: sk-test-************wxyz.","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`,
			wantKind: llm.ErrAuth, wantCode: "invalid_api_key", wantInText: "Incorrect API key",
		},
		{
			name: "rate limited", status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "7"},
			body:     `{"error":{"message":"Rate limit reached for gpt-test.","type":"requests","param":null,"code":"rate_limit_exceeded"}}`,
			wantKind: llm.ErrRateLimited, wantCode: "rate_limit_exceeded", wantRetry: 7 * time.Second, retryable: true,
		},
		{
			name: "out of quota", status: http.StatusTooManyRequests,
			body:     `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`,
			wantKind: llm.ErrAuth, wantCode: "insufficient_quota",
		},
		{
			name: "too long", status: http.StatusBadRequest,
			body:     `{"error":{"message":"Your input exceeds the context window of this model.","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`,
			wantKind: llm.ErrContextOverflow, wantCode: "context_length_exceeded",
		},
		{
			name: "Azure's content filter", status: http.StatusBadRequest,
			body:     `{"error":{"message":"The response was filtered due to the prompt triggering Azure OpenAI's content management policy.","type":null,"param":"prompt","code":"content_filter","status":400}}`,
			wantKind: llm.ErrContentFilter, wantCode: "content_filter",
		},
		{
			name: "a bad request", status: http.StatusBadRequest,
			body:     `{"error":{"message":"Invalid value: 'input_text'. Supported values are: 'output_text' and 'refusal'.","type":"invalid_request_error","param":"input[1].content[0]","code":"invalid_value"}}`,
			wantKind: llm.ErrBadRequest, wantCode: "invalid_value",
		},
		{
			name: "overloaded", status: http.StatusServiceUnavailable, body: `{"error":{"message":"The server is overloaded.","type":"server_error"}}`,
			wantKind: llm.ErrOverloaded, wantCode: "server_error", retryable: true,
		},
		{
			name: "a server fault", status: http.StatusInternalServerError, body: `{"error":{"message":"The server had an error.","type":"server_error"}}`,
			wantKind: llm.ErrServer, wantCode: "server_error", retryable: true,
		},
		{
			name: "a proxy that echoes the key", status: http.StatusUnauthorized,
			body:     `{"error":{"message":"Bearer ` + testKey + ` was refused upstream.","code":"invalid_api_key"}}`,
			wantKind: llm.ErrAuth, wantCode: "invalid_api_key", wantInText: "Bearer [redacted] was refused",
		},
		{
			name: "an echo of the key cut short", status: http.StatusBadGateway,
			body:     strings.Repeat("x", 380) + "key=" + testKey + " was refused",
			wantKind: llm.ErrServer, retryable: true, wantInText: "key=[redacted]",
		},
		{
			name: "a 200 that is not a response", status: http.StatusOK, body: `<html>gateway</html>`,
			wantKind: llm.ErrServer, retryable: true, wantInText: "does not decode",
		},
		{
			name: "an output item that is not an object", status: http.StatusOK, body: `{"id":"resp_x","status":"completed","output":["text"]}`,
			wantKind: llm.ErrServer, retryable: true, wantInText: "does not decode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProvider(t, tt.status, []byte(tt.body))
			for k, v := range tt.header {
				p.header.Set(k, v)
			}
			a := newAdapter(t, p, nil)
			resp, err := a.Call(context.Background(), plainRequest(a))
			if err == nil {
				t.Fatalf("Call returned %+v, want an error", resp)
			}
			var le *llm.Error
			if !errors.As(err, &le) {
				t.Fatalf("error %T %v is not an *llm.Error", err, err)
			}
			// A 2xx that does not decode is no refusal: it has no status.
			wantStatus := tt.status
			if wantStatus == http.StatusOK {
				wantStatus = 0
			}
			if le.Kind != tt.wantKind || le.Code != tt.wantCode || le.Status != wantStatus {
				t.Errorf("got kind %s code %q status %d, want %s %q %d", le.Kind, le.Code, le.Status, tt.wantKind, tt.wantCode, wantStatus)
			}
			if le.RetryAfter != tt.wantRetry {
				t.Errorf("RetryAfter = %s, want %s", le.RetryAfter, tt.wantRetry)
			}
			if le.Retryable() != tt.retryable {
				t.Errorf("Retryable = %v, want %v", le.Retryable(), tt.retryable)
			}
			if !strings.Contains(err.Error(), tt.wantInText) {
				t.Errorf("error %q does not say %q", err, tt.wantInText)
			}
			if strings.Contains(err.Error(), testKey[:minKeyPrefix+4]) {
				t.Errorf("error %q holds the key", err)
			}
		})
	}
}

func TestRedactKey(t *testing.T) {
	const key = "sk-proj-0123456789abcdef"
	tests := []struct{ name, in, want string }{
		{"no key", "Rate limit reached.", "Rate limit reached."},
		{"the key twice", "key " + key + ", again " + key, "key [redacted], again [redacted]"},
		{"cut in the key", "key=" + key[:12] + "…", "key=[redacted]…"},
		{"cut in the key after a whole one", key + " and " + key[:10] + "…", "[redacted] and [redacted]…"},
		{"cut before the key's eighth character", "key=" + key[:7] + "…", "key=" + key[:7] + "…"},
		{"cut elsewhere", "a long message…", "a long message…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactKey(tt.in, key); got != tt.want {
				t.Errorf("redactKey(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNetworkError(t *testing.T) {
	p := fixture(t, "text")
	a := newAdapter(t, p, nil)
	p.srv.Close()
	_, err := a.Call(context.Background(), plainRequest(a))
	var le *llm.Error
	if !errors.As(err, &le) || le.Kind != llm.ErrNetwork {
		t.Fatalf("got %v, want a network error", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error %q holds the key", err)
	}
}

func TestCallRefusesBeforeSending(t *testing.T) {
	tests := []struct {
		name string
		req  *llm.Request
	}{
		{"no request", nil},
		{"an unknown role", &llm.Request{Messages: []llm.Message{{Role: "system", Parts: []llm.Part{llm.Text("x")}}}}},
		{"a schema that is not JSON", &llm.Request{Messages: []llm.Message{llm.UserText("x")}, Tools: []llm.Tool{{Name: "t", Schema: json.RawMessage(`{"type":`)}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := fixture(t, "text")
			a := newAdapter(t, p, nil)
			_, err := a.Call(context.Background(), tt.req)
			var le *llm.Error
			if !errors.As(err, &le) || le.Kind != llm.ErrBadRequest {
				t.Fatalf("got %v, want a bad request", err)
			}
			if n := p.count(); n != 0 {
				t.Errorf("the provider was called %d times", n)
			}
		})
	}
}

// TestRequestIDFromHeader: a server that gives the response no id is
// still traceable by its request id header.
func TestRequestIDFromHeader(t *testing.T) {
	p := newProvider(t, http.StatusOK, []byte(`{"status":"completed","output":[]}`))
	p.header.Set("X-Request-Id", "req_123")
	a := newAdapter(t, p, nil)
	resp, err := a.Call(context.Background(), plainRequest(a))
	if err != nil {
		t.Fatal(err)
	}
	if resp.RequestID != "req_123" {
		t.Errorf("RequestID = %q, want req_123", resp.RequestID)
	}
}

func TestConcurrentCalls(t *testing.T) {
	p := fixture(t, "tools_parallel")
	a := newAdapter(t, p, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			resp, err := a.Call(context.Background(), &llm.Request{System: system, Messages: []llm.Message{llm.UserText(question)}, Tools: testTools})
			if err == nil && len(resp.ToolCalls()) != 2 {
				err = errors.New("the calls were lost")
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestOutputCap(t *testing.T) {
	tests := []struct{ limit, configured, want int }{
		{0, 0, 0},
		{2000, 1500, 2000},
		{0, 1500, 1500},
		{-1, 1500, 1500},
		{8, 1500, 16},
		{0, 1, 16},
		{16, 0, 16},
	}
	for _, tt := range tests {
		if got := outputCap(tt.limit, tt.configured); got != tt.want {
			t.Errorf("outputCap(%d, %d) = %d, want %d", tt.limit, tt.configured, got, tt.want)
		}
	}
}

func TestParseArguments(t *testing.T) {
	tests := []struct {
		name, raw, wantArgs, wantErr string
	}{
		{"an object in a string", `"{\"a\": 1}"`, `{"a":1}`, ""},
		{"an empty object", `"{}"`, `{}`, ""},
		{"an empty string", `""`, `{}`, ""},
		{"blank", `"  "`, `{}`, ""},
		{"absent", ``, `{}`, ""},
		{"null", `null`, `{}`, ""},
		{"an object, unquoted", `{"a": [1, 2]}`, `{"a":[1,2]}`, ""},
		{"cut short", `"{\"a\": \"01"`, `{}`, `{"a": "01`},
		{"an array", `"[1]"`, `{}`, `[1]`},
		{"a string of null", `"null"`, `{}`, `null`},
		{"two objects", `"{}{}"`, `{}`, `{}{}`},
		{"a number", `5`, `{}`, `5`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, argsErr := parseArguments(json.RawMessage(tt.raw))
			if string(args) != tt.wantArgs || argsErr != tt.wantErr {
				t.Errorf("parseArguments(%s) = %s, %q; want %s, %q", tt.raw, args, argsErr, tt.wantArgs, tt.wantErr)
			}
		})
	}
}

// TestReplayOnlyToMaker covers the rules for what goes back to whom, part
// by part.
func TestReplayOnlyToMaker(t *testing.T) {
	a, err := New(llm.Config{Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	azure, err := New(llm.Config{Model: "gpt-5", BaseURL: "https://school.openai.azure.com/openai/v1"})
	if err != nil {
		t.Fatal(err)
	}
	reasoning := llm.Part{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}`)}
	unencrypted := llm.Part{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: json.RawMessage(`{"type":"reasoning","id":"rs_2","summary":[]}`)}
	call := llm.Part{Type: llm.PartToolCall, ID: "call_1", Name: "grade_list", Args: json.RawMessage(`{}`), Maker: a.Maker(), Opaque: json.RawMessage(`{"id":"fc_1"}`)}
	text := llm.Part{Type: llm.PartText, Text: "Checking.", Maker: a.Maker(), Opaque: json.RawMessage(`{"id":"msg_1","phase":"commentary"}`)}
	parts := []llm.Part{reasoning, text, call}

	tests := []struct {
		name    string
		adapter *Adapter
		parts   []llm.Part
		want    []string
		notWant []string
	}{
		{"the maker", a, parts, []string{`"rs_1"`, `"msg_1"`, `"fc_1"`, `"commentary"`}, nil},
		{"the same model elsewhere", azure, parts, []string{`"call_1"`, `"Checking."`}, []string{`rs_1`, `msg_1`, `fc_1`, `commentary`}},
		{"reasoning that cannot go back", a, []llm.Part{unencrypted, text, call}, []string{`"call_1"`, `"commentary"`}, []string{`rs_2`, `msg_1`, `fc_1`}},
		{"reasoning that nothing follows", a, []llm.Part{text, reasoning}, []string{`"msg_1"`}, []string{`rs_1`, `enc`}},
		{"a turn of reasoning alone", a, []llm.Part{reasoning}, []string{`"input":[]`}, []string{`rs_1`}},
		{"unencrypted reasoning that nothing follows", a, []llm.Part{reasoning, call, unencrypted}, []string{`"rs_1"`, `"fc_1"`}, []string{`rs_2`}},
		{"reasoning that is not an object", a, []llm.Part{{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: json.RawMessage(`"x"`)}, call}, []string{`"call_1"`}, []string{`fc_1`, `reasoning`}},
		{"reasoning of another type", a, []llm.Part{{Type: llm.PartReasoning, Maker: a.Maker(), Opaque: json.RawMessage(`{"type":"message","encrypted_content":"enc"}`)}}, nil, []string{`enc`}},
		{"reasoning without content", a, []llm.Part{{Type: llm.PartReasoning, Maker: a.Maker()}, call}, []string{`"call_1"`}, []string{`fc_1`}},
		{"an opaque that is not JSON", a, []llm.Part{{Type: llm.PartToolCall, ID: "call_2", Name: "x", Maker: a.Maker(), Opaque: json.RawMessage(`{`)}}, []string{`"call_2"`, `"arguments":"{}"`}, nil},
		{"empty text", a, []llm.Part{{Type: llm.PartText}}, nil, []string{`output_text`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := tt.adapter.encode(&llm.Request{Tools: testTools, Messages: []llm.Message{{Role: llm.RoleAssistant, Parts: tt.parts}}})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tt.want {
				if !strings.Contains(string(body), w) {
					t.Errorf("the body lacks %s:\n%s", w, body)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(string(body), w) {
					t.Errorf("the body holds %s:\n%s", w, body)
				}
			}
		})
	}
}

// TestPhaseSplitsMessages: text parts in a row become one assistant
// message, and a change of item or phase starts another.
func TestPhaseSplitsMessages(t *testing.T) {
	a, err := New(llm.Config{Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	item := func(id, phase string) json.RawMessage {
		return json.RawMessage(`{"id":"` + id + `","phase":"` + phase + `"}`)
	}
	body, err := a.encode(&llm.Request{Messages: []llm.Message{{Role: llm.RoleAssistant, Parts: []llm.Part{
		{Type: llm.PartText, Text: "one", Maker: a.Maker(), Opaque: item("msg_1", "commentary")},
		{Type: llm.PartText, Text: "two", Maker: a.Maker(), Opaque: item("msg_1", "commentary")},
		{Type: llm.PartText, Text: "three", Maker: a.Maker(), Opaque: item("msg_2", "commentary")},
		{Type: llm.PartText, Text: "four", Maker: a.Maker(), Opaque: item("msg_2", "final_answer")},
		llm.Text("five"),
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Input []struct {
			ID      string `json:"id"`
			Phase   string `json:"phase"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range w.Input {
		var texts []string
		for _, c := range it.Content {
			texts = append(texts, c.Text)
		}
		got = append(got, it.ID+":"+it.Phase+":"+strings.Join(texts, "+"))
	}
	want := []string{"msg_1:commentary:one+two", "msg_2:commentary:three", "msg_2:final_answer:four", "::five"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("messages %q, want %q", got, want)
	}
}

func TestEncodeEdges(t *testing.T) {
	a, err := New(llm.Config{Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := a.encode(&llm.Request{
		Tools: []llm.Tool{{Name: "course_get"}, {Name: "document_list", Schema: json.RawMessage(" null ")}},
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ID: "call_1", Name: "course_get"}}},
			{Role: llm.RoleTool, Parts: []llm.Part{
				{Type: llm.PartToolResult, CallID: "call_1", Name: "course_get", Content: `{"status":"executed"}`},
				llm.Text("A note beside the results."),
				{Type: llm.PartFile},
			}},
			{Role: llm.RoleUser, Parts: []llm.Part{llm.Text(""), {Type: llm.PartFile}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"name":"course_get","parameters":{"type":"object","properties":{}}`,
		`"name":"document_list","parameters":{"type":"object","properties":{}}`,
		`{"type":"function_call","call_id":"call_1","name":"course_get","arguments":"{}"}`,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"A note beside the results."}]}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the body lacks %s:\n%s", want, body)
		}
	}
	if n := strings.Count(string(body), `"role":"user"`); n != 1 {
		t.Errorf("%d user messages, want 1: an empty one is left out\n%s", n, body)
	}
}

func TestDecodeEdges(t *testing.T) {
	a, err := New(llm.Config{Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, body    string
		wantRawStop   string
		wantInput     int64
		wantRawUsage  string
		wantStop      llm.Stop
		wantPartCount int
	}{
		{"a numeric error code", `{"status":"failed","error":{"code":500,"message":"m"},"output":[]}`, "failed/500", 0, "", llm.StopError, 0},
		{"an error with no code", `{"status":"failed","error":{"message":"m"},"output":[]}`, "failed", 0, "", llm.StopError, 0},
		{"usage that does not decode", `{"status":"completed","output":[],"usage":{"input_tokens":"12"}}`, "completed", 0, `{"input_tokens":"12"}`, llm.StopEnd, 0},
		{"null usage", `{"status":"completed","output":null,"usage":null}`, "completed", 0, "", llm.StopEnd, 0},
		{"a call with no status, cut off last", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"Let me"}]},{"type":"function_call","call_id":"c","name":"grade_list","arguments":"{\"a\":"}]}`, "incomplete/max_output_tokens", 0, "", llm.StopMaxTokens, 1},
		{"a call with no status before the cut", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","call_id":"c","name":"grade_list","arguments":"{}"},{"type":"message","content":[{"type":"output_text","text":"Let me"}]}]}`, "incomplete/max_output_tokens", 0, "", llm.StopToolCalls, 2},
		{"a call in progress in a failed response", `{"status":"failed","error":{"code":"server_error"},"output":[{"type":"function_call","status":"in_progress","call_id":"c","name":"grade_list","arguments":"{"}]}`, "failed/server_error", 0, "", llm.StopError, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := a.decode([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if resp.Stop != tt.wantStop || resp.RawStop != tt.wantRawStop || len(resp.Parts) != tt.wantPartCount {
				t.Errorf("stop %s (%s) with %d parts, want %s (%s) with %d", resp.Stop, resp.RawStop, len(resp.Parts), tt.wantStop, tt.wantRawStop, tt.wantPartCount)
			}
			if resp.Usage.Input != tt.wantInput || string(resp.Usage.Raw) != tt.wantRawUsage {
				t.Errorf("usage %d, raw %s; want %d, %s", resp.Usage.Input, resp.Usage.Raw, tt.wantInput, tt.wantRawUsage)
			}
		})
	}
	for _, bad := range []string{`{"output":[{"type":"message","content":"text"}]}`, `{"output":[{"type":"function_call","call_id":7}]}`, `{"output":[{"type":"reasoning","summary":"x"}]}`} {
		if _, err := a.decode([]byte(bad)); err == nil {
			t.Errorf("decode(%s) accepted a known item it cannot read", bad)
		}
	}
}

// TestLeastReasoning: a call asking for the least reasoning (ForceAnswer's,
// a continuation's) is made at low in place of a configured medium or
// high; minimal and low stay; with no effort configured, none is sent.
func TestLeastReasoning(t *testing.T) {
	for _, c := range []struct{ effort, ordinary, least string }{
		{"high", "high", "low"},
		{"medium", "medium", "low"},
		{"low", "low", "low"},
		{"minimal", "minimal", "minimal"},
		{"", "", ""},
	} {
		a, err := New(llm.Config{Model: "gpt-5", Reasoning: llm.Reasoning{Effort: c.effort}})
		if err != nil {
			t.Fatal(err)
		}
		sent := func(least bool) string {
			body, err := a.encode(&llm.Request{Messages: []llm.Message{llm.UserText("Why did I lose marks?")}, ToolMode: llm.ToolNone,
				LeastReasoning: least})
			if err != nil {
				t.Fatal(err)
			}
			var w struct {
				Reasoning *struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if err := json.Unmarshal(body, &w); err != nil {
				t.Fatal(err)
			}
			if w.Reasoning == nil {
				return ""
			}
			return w.Reasoning.Effort
		}
		if got := sent(false); got != c.ordinary {
			t.Errorf("%q: an ordinary call is made at %q, want %q", c.effort, got, c.ordinary)
		}
		if got := sent(true); got != c.least {
			t.Errorf("%q: a call asking for the least reasoning is made at %q, want %q", c.effort, got, c.least)
		}
	}
}
