package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

func TestNew(t *testing.T) {
	const flash = "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"
	tests := []struct {
		name     string
		change   func(*llm.Config)
		endpoint string
		err      string
	}{
		{name: "the default endpoint", endpoint: flash},
		{name: "a model named as a resource", change: func(c *llm.Config) { c.Model = "models/gemini-2.5-flash" }, endpoint: flash},
		{name: "a tuned model", change: func(c *llm.Config) { c.Model = "tunedModels/cs101-tutor-7" },
			endpoint: "https://generativelanguage.googleapis.com/v1beta/tunedModels/cs101-tutor-7:generateContent"},
		{name: "a proxy with a path, given with its version", change: func(c *llm.Config) {
			c.BaseURL, c.APIKey = "https://egress.example.edu/gemini/v1beta/", ""
		}, endpoint: "https://egress.example.edu/gemini/v1beta/models/gemini-2.5-flash:generateContent"},
		{name: "the default base given explicitly", change: func(c *llm.Config) { c.BaseURL = DefaultBaseURL + "/" }, endpoint: flash},

		{name: "no model", change: func(c *llm.Config) { c.Model = " " }, err: "no model"},
		{name: "a model that climbs the path", change: func(c *llm.Config) { c.Model = "../files" }, err: "not a model id"},
		{name: "a model that adds a query", change: func(c *llm.Config) { c.Model = "gemini-2.5-flash?key=" + testKey }, err: "not a model id"},
		{name: "a model with too many segments", change: func(c *llm.Config) { c.Model = "models/a/b" }, err: "not a model id"},
		{name: "a model in another collection", change: func(c *llm.Config) { c.Model = "files/abc" }, err: "not a model id"},
		{name: "a base that is not http", change: func(c *llm.Config) { c.BaseURL = "ftp://example.edu" }, err: "absolute http"},
		{name: "a relative base", change: func(c *llm.Config) { c.BaseURL = "generativelanguage.googleapis.com" }, err: "absolute http"},
		{name: "a key in the base URL", change: func(c *llm.Config) { c.BaseURL = DefaultBaseURL + "/?key=" + testKey }, err: "no query"},
		{name: "credentials in the base URL", change: func(c *llm.Config) { c.BaseURL = "https://user:" + testKey + "@example.edu" }, err: "no query"},
		{name: "no key for Google", change: func(c *llm.Config) { c.APIKey = "" }, err: "no API key"},
		{name: "an unknown effort", change: func(c *llm.Config) { c.Reasoning.Effort = "extreme" }, err: "reasoning effort"},
		{name: "an unknown dialect", change: func(c *llm.Config) { c.Dialect = "yaml" }, err: "dialect"},
		{name: "another adapter's configuration", change: func(c *llm.Config) { c.Adapter = llm.AdapterAnthropic }, err: "adapter"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := llm.Config{Adapter: llm.AdapterGemini, Model: "gemini-2.5-flash", APIKey: testKey}
			if tc.change != nil {
				tc.change(&cfg)
			}
			a, err := New(cfg)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("New: %v, want an error saying %q", err, tc.err)
				}
				if strings.Contains(err.Error(), testKey) {
					t.Fatalf("the error shows the key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if a.endpoint != tc.endpoint {
				t.Fatalf("endpoint %s, want %s", a.endpoint, tc.endpoint)
			}
		})
	}
}

func TestAdapterDescribesItself(t *testing.T) {
	a, err := New(llm.Config{Model: "gemini-2.5-pro", APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	var _ llm.Adapter = a
	want := llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true}
	if a.Name() != "gemini" || a.Provider() != llm.ProviderGemini || a.Model() != "gemini-2.5-pro" ||
		a.Maker() != "gemini|https://generativelanguage.googleapis.com|gemini-2.5-pro" ||
		a.Dialect() != toolschema.GeminiJSONSchema || a.Capabilities() != want {
		t.Fatalf("name %s, provider %s, model %s, maker %s, dialect %s, capabilities %+v",
			a.Name(), a.Provider(), a.Model(), a.Maker(), a.Dialect(), a.Capabilities())
	}

	// The same model on the same endpoint, however written, is the same maker.
	b, err := New(llm.Config{Model: "gemini-2.5-pro", APIKey: testKey, BaseURL: DefaultBaseURL + "/v1beta/"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Maker() != a.Maker() {
		t.Fatalf("maker %s, want %s", b.Maker(), a.Maker())
	}

	c, err := New(llm.Config{
		Model: "gemini-2.5-pro", APIKey: testKey, Provider: "school-proxy", Dialect: toolschema.GeminiOpenAPI,
		Capabilities: llm.CapabilityOverrides{FileInput: ptr(false), ParallelToolCalls: ptr(false)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider() != "school-proxy" || c.Dialect() != toolschema.GeminiOpenAPI ||
		c.Capabilities() != (llm.Capabilities{ToolChoiceNone: true}) {
		t.Fatalf("overrides not applied: provider %s, dialect %s, capabilities %+v", c.Provider(), c.Dialect(), c.Capabilities())
	}
}

// The key goes in x-goog-api-key and nowhere else; a configured header
// cannot stand in for it.
func TestTheKeyGoesInItsHeader(t *testing.T) {
	f := newFake(t, ok(`{"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}]}`))
	a := f.newAdapter(t, func(c *llm.Config) {
		c.Headers = map[string]string{"X-Goog-Api-Key": "someone-elses-key", "X-Request-Source": "aishie"}
	})
	if _, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hello")}}); err != nil {
		t.Fatal(err)
	}
	r := f.last(t)
	if r.method != http.MethodPost {
		t.Errorf("method %s", r.method)
	}
	if got := r.header.Values("X-Goog-Api-Key"); len(got) != 1 || got[0] != testKey {
		t.Errorf("x-goog-api-key %q, want the configured key once", got)
	}
	if r.header.Get("X-Request-Source") != "aishie" {
		t.Errorf("the configured header is missing")
	}
	if r.header.Get("Authorization") != "" {
		t.Errorf("an Authorization header was sent")
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	if strings.Contains(r.url, testKey) || strings.Contains(r.url, "key=") {
		t.Errorf("the key is in the URL: %s", r.url)
	}

	// Behind a proxy that adds the key itself, none is sent.
	g := newFake(t, ok(`{"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}]}`))
	b := g.newAdapter(t, func(c *llm.Config) { c.BaseURL, c.APIKey = "https://egress.example.edu/gemini", "" })
	if _, err := b.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hello")}}); err != nil {
		t.Fatal(err)
	}
	if r := g.last(t); r.header.Get("X-Goog-Api-Key") != "" || r.url != "https://egress.example.edu/gemini/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Errorf("behind a proxy: key header %q, URL %s", r.header.Get("X-Goog-Api-Key"), r.url)
	}
}

// A loop's second turn sends each signature back on the part it came with,
// and each id Gemini gave back on its call and its response; an id the
// adapter made up is never sent. Another model, even on the same endpoint,
// gets neither.
func TestSignaturesAndIDsGoBackOnTheirParts(t *testing.T) {
	f := newFake(t,
		ok(`{"candidates":[{"content":{"role":"model","parts":[
			{"text":"Checking.","thoughtSignature":"U0lHLVRFWFQ="},
			{"functionCall":{"id":"fc-7","name":"grade_list","args":{"assignment_id":"a3"}},"thoughtSignature":"U0lHLUNBTEw="},
			{"functionCall":{"name":"course_get","args":{}}}]},"finishReason":"STOP"}]}`),
		ok(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Done."}]},"finishReason":"STOP"}]}`))
	a := f.newAdapter(t)
	ctx := context.Background()
	req := &llm.Request{
		Messages: []llm.Message{llm.UserText("Why did I lose marks?")},
		Tools: []llm.Tool{
			{Name: "grade_list", Schema: json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":"string"}}}`)},
			{Name: "course_get", Schema: json.RawMessage(`{"type":"object","properties":{}}`)},
		},
	}
	first, err := a.Call(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	calls := first.ToolCalls()
	if first.Stop != llm.StopToolCalls || len(calls) != 2 || calls[0].ID != "fc-7" || calls[1].ID != "call_2" {
		t.Fatalf("stop %s, calls %+v", first.Stop, calls)
	}
	results := llm.Message{Role: llm.RoleTool}
	for _, c := range calls {
		results.Parts = append(results.Parts, llm.Part{Type: llm.PartToolResult, CallID: c.ID, Name: c.Name, Content: `{"status":"executed"}`})
	}
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Parts: first.Parts}, results)

	if _, err := a.Call(ctx, req); err != nil {
		t.Fatal(err)
	}
	checkContents(t, f.last(t), `[
		{"role":"user","parts":[{"text":"Why did I lose marks?"}]},
		{"role":"model","parts":[
			{"text":"Checking.","thoughtSignature":"U0lHLVRFWFQ="},
			{"functionCall":{"id":"fc-7","name":"grade_list","args":{"assignment_id":"a3"}},"thoughtSignature":"U0lHLUNBTEw="},
			{"functionCall":{"name":"course_get","args":{}}}]},
		{"role":"user","parts":[
			{"functionResponse":{"id":"fc-7","name":"grade_list","response":{"output":{"status":"executed"}}}},
			{"functionResponse":{"name":"course_get","response":{"output":{"status":"executed"}}}}]}]`)

	other := f.newAdapter(t, func(c *llm.Config) { c.Model = "gemini-2.5-pro" })
	if _, err := other.Call(ctx, req); err != nil {
		t.Fatal(err)
	}
	checkContents(t, f.last(t), `[
		{"role":"user","parts":[{"text":"Why did I lose marks?"}]},
		{"role":"model","parts":[
			{"text":"Checking."},
			{"functionCall":{"name":"grade_list","args":{"assignment_id":"a3"}}},
			{"functionCall":{"name":"course_get","args":{}}}]},
		{"role":"user","parts":[
			{"functionResponse":{"name":"grade_list","response":{"output":{"status":"executed"}}}},
			{"functionResponse":{"name":"course_get","response":{"output":{"status":"executed"}}}}]}]`)
}

func checkContents(t *testing.T, r recorded, want string) {
	t.Helper()
	body := wireBody(t, r)
	got := canonical(t, mustJSON(t, body["contents"]))
	if w := canonical(t, []byte(want)); string(got) != string(w) {
		t.Errorf("contents\n--- want\n%s--- got\n%s", w, got)
	}
}

// A tool result names its tool even when the result part does not, and a
// result for a call not in the message before it goes without an id.
func TestAResultFindsItsCall(t *testing.T) {
	f := newFake(t, ok(`{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`))
	a := f.newAdapter(t)
	req := &llm.Request{
		Tools: []llm.Tool{{Name: "grade_list"}},
		Messages: []llm.Message{
			llm.UserText("Grades?"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ID: "fc-1", Name: "grade_list", Maker: testMaker, Opaque: json.RawMessage(`{"id":"fc-1"}`)}}},
			{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, CallID: "fc-1", Content: "{}"}}},
			{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ID: "call_1", Name: "course_get"}}},
			{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, CallID: "fc-1", Name: "grade_list", Content: "{}"}}},
		},
	}
	if _, err := a.Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	checkContents(t, f.last(t), `[
		{"role":"user","parts":[{"text":"Grades?"}]},
		{"role":"model","parts":[{"functionCall":{"id":"fc-1","name":"grade_list","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"fc-1","name":"grade_list","response":{"output":{}}}}]},
		{"role":"model","parts":[{"functionCall":{"name":"course_get","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"grade_list","response":{"output":{}}}}]}]`)
}

// Turns left empty are dropped, and turns by the same role in a row are
// merged: results first, then what the user wrote.
func TestTurnsAlternate(t *testing.T) {
	f := newFake(t, ok(`{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`))
	a := f.newAdapter(t)
	req := &llm.Request{
		Tools: []llm.Tool{{Name: "course_get"}},
		Messages: []llm.Message{
			llm.UserText("First"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartReasoning, Maker: "anthropic|https://api.anthropic.com|claude", Opaque: json.RawMessage(`{"thinking":"x"}`)}}},
			llm.UserText("Second"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: ""}, {Type: llm.PartToolCall, ID: "call_1", Name: "course_get", Args: json.RawMessage(`[1]`), ArgsError: "[1]"}}},
			{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, CallID: "call_1", Name: "course_get", IsError: true, Content: "the arguments are not a JSON object"}}},
			llm.UserText("Third"),
		},
	}
	if _, err := a.Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	checkContents(t, f.last(t), `[
		{"role":"user","parts":[{"text":"First"},{"text":"Second"}]},
		{"role":"model","parts":[{"functionCall":{"name":"course_get","args":{}}}]},
		{"role":"user","parts":[
			{"functionResponse":{"name":"course_get","response":{"error":"the arguments are not a JSON object"}}},
			{"text":"Third"}]}]`)
}

func TestNothingToSend(t *testing.T) {
	f := newFake(t)
	a := f.newAdapter(t)
	_, err := a.Call(context.Background(), &llm.Request{System: "Be brief.", Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{llm.Text("")}}}})
	var le *llm.Error
	if !errors.As(err, &le) || le.Kind != llm.ErrBadRequest {
		t.Fatalf("got %v, want a bad_request", err)
	}
	if _, err := a.Call(context.Background(), nil); !errors.As(err, &le) || le.Kind != llm.ErrBadRequest {
		t.Fatalf("nil request: got %v, want a bad_request", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests sent for nothing", n)
	}
}

func TestRefusals(t *testing.T) {
	quota := `{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details.","status":"RESOURCE_EXHAUSTED","details":[` +
		`{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"generativelanguage.googleapis.com/generate_content_free_tier_requests"}]},` +
		`{"@type":"type.googleapis.com/google.rpc.Help","links":[{"description":"Learn more","url":"https://ai.google.dev/gemini-api/docs/rate-limits"}]},` +
		`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"36s"}]}}`
	google := func(code int, status, msg string) string {
		return fmt.Sprintf(`{"error":{"code":%d,"message":%q,"status":%q}}`, code, msg, status)
	}
	tests := []struct {
		name       string
		status     int
		header     map[string]string
		body       string
		kind       llm.ErrorKind
		code       string
		retryAfter time.Duration
	}{
		{name: "quota, with RetryInfo", status: 429, body: quota, kind: llm.ErrRateLimited, code: "RESOURCE_EXHAUSTED", retryAfter: 36 * time.Second},
		{name: "quota, Retry-After first", status: 429, header: map[string]string{"Retry-After": "7"}, body: quota, kind: llm.ErrRateLimited, code: "RESOURCE_EXHAUSTED", retryAfter: 7 * time.Second},
		{name: "quota, a fraction of a second", status: 429, body: strings.Replace(quota, `"36s"`, `"0.25s"`, 1), kind: llm.ErrRateLimited, code: "RESOURCE_EXHAUSTED", retryAfter: 250 * time.Millisecond},
		{name: "quota, as a list", status: 429, body: "[" + quota + "]", kind: llm.ErrRateLimited, code: "RESOURCE_EXHAUSTED", retryAfter: 36 * time.Second},
		{name: "the input is too long", status: 400,
			body: google(400, "INVALID_ARGUMENT", "The input token count (1196265) exceeds the maximum number of tokens allowed (1048575)."),
			kind: llm.ErrContextOverflow, code: "INVALID_ARGUMENT"},
		{name: "the output cap is out of range", status: 400,
			body: google(400, "INVALID_ARGUMENT", "Unable to submit request because it has a maxOutputTokens value of 70000 but the supported range is from 1 (inclusive) to 65537 (exclusive). Update the value and try again."),
			kind: llm.ErrBadRequest, code: "INVALID_ARGUMENT"},
		{name: "a bad key", status: 400,
			body: `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com","metadata":{"service":"generativelanguage.googleapis.com"}}]}}`,
			kind: llm.ErrAuth, code: "INVALID_ARGUMENT"},
		{name: "an expired key, said only in words", status: 400, body: google(400, "INVALID_ARGUMENT", "API key expired. Please renew the API key."), kind: llm.ErrAuth, code: "INVALID_ARGUMENT"},
		{name: "a bad request", status: 400, body: google(400, "INVALID_ARGUMENT", "Please use a valid role: user, model."), kind: llm.ErrBadRequest, code: "INVALID_ARGUMENT"},
		{name: "a region not served", status: 400, body: google(400, "FAILED_PRECONDITION", "User location is not supported for the API use."), kind: llm.ErrBadRequest, code: "FAILED_PRECONDITION"},
		{name: "permission denied", status: 403, body: google(403, "PERMISSION_DENIED", "Permission denied: Consumer 'api_key:x' has been suspended."), kind: llm.ErrAuth, code: "PERMISSION_DENIED"},
		{name: "no such model", status: 404, body: google(404, "NOT_FOUND", "models/gemini-9 is not found for API version v1beta."), kind: llm.ErrBadRequest, code: "NOT_FOUND"},
		{name: "overloaded", status: 503, body: google(503, "UNAVAILABLE", "The model is overloaded. Please try again later."), kind: llm.ErrOverloaded, code: "UNAVAILABLE"},
		{name: "an internal error", status: 500, body: google(500, "INTERNAL", "An internal error has occurred."), kind: llm.ErrServer, code: "INTERNAL"},
		{name: "a deadline", status: 504, body: google(504, "DEADLINE_EXCEEDED", "Deadline expired before operation could complete."), kind: llm.ErrTimeout, code: "DEADLINE_EXCEEDED"},
		{name: "a proxy's page", status: 502, body: "<html><body>Bad Gateway</body></html>", kind: llm.ErrServer},
		{name: "a payload too large", status: 400, body: google(400, "INVALID_ARGUMENT", "Request payload size exceeds the limit: 104857600 bytes."), kind: llm.ErrContextOverflow, code: "INVALID_ARGUMENT"},
		{name: "a payload refused at the door", status: 413, body: "<html><body>413 Request Entity Too Large</body></html>", kind: llm.ErrContextOverflow},
		{name: "a message that echoes the key", status: 400, body: google(400, "INVALID_ARGUMENT", "Invalid value "+testKey+" for field model."), kind: llm.ErrBadRequest, code: "INVALID_ARGUMENT"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, answer{status: tc.status, header: tc.header, body: tc.body})
			a := f.newAdapter(t)
			resp, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hello")}})
			var le *llm.Error
			if !errors.As(err, &le) {
				t.Fatalf("got %v, %v; want an *llm.Error", resp, err)
			}
			if le.Kind != tc.kind || le.Code != tc.code || le.Status != tc.status || le.RetryAfter != tc.retryAfter {
				t.Fatalf("got kind %s, code %q, status %d, retry after %s; want %s, %q, %d, %s",
					le.Kind, le.Code, le.Status, le.RetryAfter, tc.kind, tc.code, tc.status, tc.retryAfter)
			}
			if le.Message == "" || len(le.Message) > 400 {
				t.Fatalf("message %q", le.Message)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Fatalf("the error shows the key: %v", err)
			}
		})
	}
}

// Each call reads its own refusal, however many run at once.
func TestConcurrentRefusalsStayApart(t *testing.T) {
	f := newFake(t)
	f.answerBy(func(body []byte) answer {
		var req struct {
			Contents []struct {
				Parts []struct{ Text string } `json:"parts"`
			} `json:"contents"`
		}
		_ = json.Unmarshal(body, &req)
		n := req.Contents[0].Parts[0].Text
		if k, _ := strconv.Atoi(n); k%2 == 0 {
			return ok(`{"candidates":[{"content":{"parts":[{"text":"` + n + `"}]},"finishReason":"STOP"}]}`)
		}
		return answer{status: 429, body: `{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"` + n + `s"}]}}`}
	})
	a := f.newAdapter(t)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 1; i <= 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText(strconv.Itoa(i))}})
			var le *llm.Error
			switch {
			case i%2 == 0 && (err != nil || resp.Text() != strconv.Itoa(i)):
				errs <- fmt.Errorf("call %d: %v, %w", i, resp, err)
			case i%2 == 1 && (!errors.As(err, &le) || le.RetryAfter != time.Duration(i)*time.Second):
				errs <- fmt.Errorf("call %d: %w, want retry after %ds", i, err, i)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestUnreachable(t *testing.T) {
	f := newFake(t)
	a := f.newAdapter(t)
	f.srv.Close()
	_, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hello")}})
	var le *llm.Error
	if !errors.As(err, &le) || le.Kind != llm.ErrNetwork || !le.Retryable() {
		t.Fatalf("got %v, want a retryable network error", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("the error shows the key: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := newFake(t)
	if _, err := g.newAdapter(t).Call(ctx, &llm.Request{Messages: []llm.Message{llm.UserText("Hello")}}); !errors.As(err, &le) || le.Kind != llm.ErrTimeout {
		t.Fatalf("cancelled: got %v, want a timeout", err)
	}
}

func TestAnswersThatDoNotDecode(t *testing.T) {
	for _, body := range []string{
		`not JSON`,
		`{"candidates":[{"content":{"parts":["a string, not a part"]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":"many"}}`,
	} {
		f := newFake(t, ok(body))
		_, err := f.newAdapter(t).Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hello")}})
		var le *llm.Error
		if !errors.As(err, &le) || le.Kind != llm.ErrServer {
			t.Errorf("%s: got %v, want a server error", body, err)
		}
	}
}

// Proto3's JSON may write counts as strings; they count the same.
func TestCountsWrittenAsStrings(t *testing.T) {
	f := newFake(t, ok(`{"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":"12","candidatesTokenCount":3,"thoughtsTokenCount":"4","cachedContentTokenCount":null}}`))
	resp, err := f.newAdapter(t).Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hello")}})
	if err != nil {
		t.Fatal(err)
	}
	if u := resp.Usage; u.Input != 12 || u.Output != 7 || u.Reasoning != 4 || u.CacheRead != 0 {
		t.Fatalf("usage %+v", u)
	}
}

func TestArguments(t *testing.T) {
	tests := []struct {
		raw, args, argsError string
	}{
		{raw: ``, args: `{}`},
		{raw: `null`, args: `{}`},
		{raw: `{ "limit" : 5 }`, args: `{"limit":5}`},
		{raw: `[1,2]`, args: `{}`, argsError: `[1,2]`},
		{raw: `"{\"limit\":5}"`, args: `{}`, argsError: `"{\"limit\":5}"`},
	}
	for _, tc := range tests {
		args, argsError := argsOf(json.RawMessage(tc.raw))
		if string(args) != tc.args || argsError != tc.argsError {
			t.Errorf("argsOf(%s) = %s, %q; want %s, %q", tc.raw, args, argsError, tc.args, tc.argsError)
		}
	}
}

func TestThinking(t *testing.T) {
	tests := []struct {
		model, effort, want string
	}{
		{"gemini-2.5-flash", "", `null`},
		{"gemini-2.5-flash", "minimal", `{"includeThoughts":false,"thinkingBudget":512}`},
		{"gemini-2.5-flash", "low", `{"includeThoughts":false,"thinkingBudget":1024}`},
		{"gemini-2.5-flash", "medium", `{"includeThoughts":false,"thinkingBudget":8192}`},
		{"gemini-2.5-flash", "high", `{"includeThoughts":false,"thinkingBudget":24576}`},
		{"gemini-2.5-pro", "minimal", `{"includeThoughts":false,"thinkingBudget":512}`},
		{"gemini-2.5-flash-lite", "minimal", `{"includeThoughts":false,"thinkingBudget":512}`},
		{"gemini-flash-latest", "low", `{"includeThoughts":false,"thinkingBudget":1024}`},
		{"gemini-exp-1206", "low", `{"includeThoughts":false,"thinkingBudget":1024}`},
		{"tunedModels/cs101-tutor", "low", `{"includeThoughts":false,"thinkingBudget":1024}`},
		{"gemini-3-pro-preview", "minimal", `{"includeThoughts":false,"thinkingLevel":"LOW"}`},
		{"gemini-3-pro-preview", "low", `{"includeThoughts":false,"thinkingLevel":"LOW"}`},
		{"gemini-3-pro-preview", "medium", `{"includeThoughts":false,"thinkingLevel":"HIGH"}`},
		{"gemini-3-pro-preview", "high", `{"includeThoughts":false,"thinkingLevel":"HIGH"}`},
		{"gemini-3-pro-image-preview", "medium", `{"includeThoughts":false,"thinkingLevel":"HIGH"}`},
		{"gemini-3-flash-preview", "medium", `{"includeThoughts":false,"thinkingLevel":"MEDIUM"}`},
		{"gemini-3.1-pro-preview", "medium", `{"includeThoughts":false,"thinkingLevel":"MEDIUM"}`},
		{"gemini-3.8-flash", "minimal", `{"includeThoughts":false,"thinkingLevel":"LOW"}`},
		{"gemini-3.1-flash", "low", `{"includeThoughts":false,"thinkingLevel":"LOW"}`},
		{"models/gemini-3-flash", "high", `{"includeThoughts":false,"thinkingLevel":"HIGH"}`},
		{"gemini-10-ultra", "low", `{"includeThoughts":false,"thinkingLevel":"LOW"}`},
		{"gemini-1000-ultra", "low", `{"includeThoughts":false,"thinkingBudget":1024}`},
		{"gemini-3x-pro", "low", `{"includeThoughts":false,"thinkingBudget":1024}`},
	}
	for _, tc := range tests {
		got, err := json.Marshal(thinking(tc.model, tc.effort))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("%s at %q: %s, want %s", tc.model, tc.effort, got, tc.want)
		}
	}
}

// TestLeastReasoning: a call asking for the least reasoning (ForceAnswer's,
// a continuation's) asks for the least thinking Google documents for the
// model, configured or not, with its allowance: 2.5 Flash's thinking off,
// with none; 2.5 Pro's least budget, 128; Gemini 3's least level; nothing
// for a model that thinks least unasked. A model not documented there is
// asked at low in place of medium or high, and nothing unasked.
func TestLeastReasoning(t *testing.T) {
	tests := []struct {
		model, effort, thinking string
		maxOut                  int
	}{
		{"gemini-2.5-flash", "", `{"includeThoughts":false,"thinkingBudget":0}`, 1500},
		{"gemini-2.5-flash", "minimal", `{"includeThoughts":false,"thinkingBudget":0}`, 1500},
		{"gemini-2.5-pro", "high", `{"includeThoughts":false,"thinkingBudget":128}`, 1628},
		{"gemini-2.5-flash-lite", "", `null`, 1500},
		{"gemini-2.5-flash-lite", "medium", `null`, 1500},
		{"gemini-3-pro-preview", "", `{"includeThoughts":false,"thinkingLevel":"LOW"}`, 1500 + defaultThinkingAllowance},
		{"gemini-3-flash-preview", "high", `{"includeThoughts":false,"thinkingLevel":"MINIMAL"}`, 1500 + defaultThinkingAllowance},
		{"gemini-3.8-flash", "", `{"includeThoughts":false,"thinkingLevel":"LOW"}`, 1500 + defaultThinkingAllowance},
		{"gemini-3.1-flash-lite", "high", `null`, 1500 + defaultThinkingAllowance},
		{"gemini-3.9-flash", "high", `{"includeThoughts":false,"thinkingLevel":"LOW"}`, 1500 + defaultThinkingAllowance},
		{"gemini-3.9-flash", "", `null`, 1500 + defaultThinkingAllowance},
		{"gemini-flash-latest", "medium", `{"includeThoughts":false,"thinkingBudget":1024}`, 2524},
	}
	for _, tc := range tests {
		a := &Adapter{model: tc.model, effort: tc.effort}
		g := a.generation(llm.Limits{MaxOutputTokens: 1500}, true)
		got, err := json.Marshal(g.ThinkingConfig)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.thinking || g.MaxOutputTokens != tc.maxOut {
			t.Errorf("%s at %q: thinking %s, maxOutputTokens %d; want %s, %d", tc.model, tc.effort, got, g.MaxOutputTokens, tc.thinking, tc.maxOut)
		}
	}
}

// TestLeastEffort holds the least thinking Google documents for each
// Gemini, by its id as Google or a router writes it, and "" where the
// model thinks least unasked; a model not documented is not known.
func TestLeastEffort(t *testing.T) {
	for _, c := range []struct {
		model, effort string
		known         bool
	}{
		{"gemini-2.5-flash", "none", true},
		{"gemini-2.5-flash-preview-09-2025", "none", true},
		{"models/gemini-2.5-pro", "minimal", true},
		{"google/gemini-2.5-pro:batch", "minimal", true},
		{"gemini-2.5-flash-lite", "", true},
		{"gemini-2.5-flash-lite-preview-06-17", "", true},
		{"gemini-3-pro-preview", "low", true},
		{"gemini-3-flash-preview", "minimal", true},
		{"gemini-3.1-pro-preview-customtools", "low", true},
		{"gemini-3.1-flash-lite", "", true},
		{"gemini-3.1-flash-lite-image", "", true},
		{"gemini-3.5-flash", "minimal", true},
		{"gemini-3.5-flash-lite", "", true},
		{"gemini-3.6-flash", "minimal", true},
		{"gemini-3.7-flash", "low", true},
		{"gemini-3.8-flash", "low", true},
		{"gemini-2.5-flash-image", "", false},
		{"gemini-3.1-flash-image-preview", "", false},
		{"gemini-3.8-pro", "", false},
		{"gemini-3.9-flash", "", false},
		{"gemini-4-flash", "", false},
		{"gemini-2.0-flash-001", "", false},
		{"gemini-flash-latest", "", false},
		{"tunedModels/cs101-tutor", "", false},
	} {
		if effort, known := LeastEffort(c.model); effort != c.effort || known != c.known {
			t.Errorf("LeastEffort(%q) = %q, %v; want %q, %v", c.model, effort, known, c.effort, c.known)
		}
	}
}

func TestVersionOf(t *testing.T) {
	tests := []struct {
		model string
		want  version
	}{
		{"gemini-2.5-flash", version{major: 2, minor: 5, known: true, variant: "-flash"}},
		{"models/gemini-2.5-flash-lite", version{major: 2, minor: 5, known: true, variant: "-flash-lite"}},
		{"gemini-3-pro-preview", version{major: 3, known: true, variant: "-pro-preview"}},
		{"gemini-3.1-pro", version{major: 3, minor: 1, known: true, variant: "-pro"}},
		{"gemini-2.0-flash-001", version{major: 2, known: true, variant: "-flash-001"}},
		{"gemini-4", version{major: 4, known: true}},
		{"gemini-flash-latest", version{}},
		{"gemini-exp-1206", version{}},
		{"gemini-2.", version{}},
		{"gemini-2.5x", version{}},
		{"gemini-1000", version{}},
		{"tunedModels/cs101-tutor", version{}},
		{"gemma-3-27b-it", version{}},
	}
	for _, tc := range tests {
		if got := versionOf(tc.model); got != tc.want {
			t.Errorf("versionOf(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

// maxOutputTokens counts thinking, so a model that thinks is given its
// allowance beside the answer's cap; one that does not is given the cap.
func TestOutputCap(t *testing.T) {
	tests := []struct {
		model, effort string
		limit, want   int
	}{
		{"gemini-2.5-flash", "", 1500, 1500 + defaultThinkingAllowance},
		{"gemini-2.5-pro", "", 1500, 1500 + defaultThinkingAllowance},
		{"gemini-2.5-flash", "minimal", 1500, 2012},
		{"gemini-2.5-flash", "low", 1500, 2524},
		{"gemini-2.5-pro", "high", 4000, 28576},
		{"gemini-2.5-flash-lite", "", 1500, 1500},
		{"gemini-2.5-flash-lite", "low", 1500, 2524},
		{"gemini-2.5-flash-image", "", 1500, 1500},
		{"gemini-2.0-flash", "", 1500, 1500},
		{"gemini-flash-latest", "", 1500, 1500},
		{"gemini-flash-latest", "medium", 1500, 9692},
		{"tunedModels/cs101-tutor", "", 1500, 1500},
		{"gemini-3-pro-preview", "", 1500, 1500 + defaultThinkingAllowance},
		{"gemini-3.5-flash", "low", 1500, 1500 + defaultThinkingAllowance},
		{"gemini-2.5-flash", "", 60000, maxOutputCeiling},
		{"gemini-2.5-flash", "", 70000, 70000},
		{"gemini-2.5-flash", "high", 0, 0},
	}
	for _, tc := range tests {
		if got := outputCap(tc.limit, thinkingAllowance(tc.model, tc.effort)); got != tc.want {
			t.Errorf("%s at %q with a cap of %d: %d, want %d", tc.model, tc.effort, tc.limit, got, tc.want)
		}
	}
}

func TestTakesArguments(t *testing.T) {
	tests := []struct {
		schema string
		want   bool
	}{
		{``, false},
		{`null`, false},
		{`{}`, false},
		{`{"type":"object"}`, false},
		{`{"type":"object","properties":{},"additionalProperties":false,"required":[]}`, false},
		{`{"type":"object","properties":{},"description":"No arguments."}`, false},
		{`{"type":"object","properties":{"limit":{"type":"integer"}}}`, true},
		{`{"type":"object","additionalProperties":{"type":"string"}}`, true},
		{`{"type":"object","properties":{},"anyOf":[{"required":["a"]}]}`, true},
		{`{"type":"object","properties":{},"required":["a"]}`, true},
		{`{"type":"string"}`, true},
		{`[1]`, true},
	}
	for _, tc := range tests {
		if got := takesArguments(json.RawMessage(tc.schema)); got != tc.want {
			t.Errorf("takesArguments(%s) = %v, want %v", tc.schema, got, tc.want)
		}
	}
}

func TestMediaType(t *testing.T) {
	tests := []struct {
		name, mime, want string
	}{
		{"syllabus.pdf", "application/pdf", "application/pdf"},
		{"syllabus.pdf", "Application/PDF", "application/pdf"},
		{"photo", "image/jpg", "image/jpeg"},
		{"notes.txt", "text/plain; charset=utf-8", "text/plain"},
		{"figure.png", "", "image/png"},
		{"figure.png", "application/octet-stream", "image/png"},
		{"export", "application/octet-stream", ""},
		{"export", "", ""},
		{"export", "not a type", ""},
	}
	for typ, want := range map[string]bool{
		"text/markdown": true, "application/json": true, "application/ld+json": true, "image/svg+xml": true,
		"application/x-yaml": true, "application/pdf": false, "image/png": false, "": false,
	} {
		if got := isText(typ); got != want {
			t.Errorf("isText(%q) = %v, want %v", typ, got, want)
		}
	}
	for _, tc := range tests {
		if got := mediaType(&llm.File{Name: tc.name, MIME: tc.mime}); got != tc.want {
			t.Errorf("mediaType(%q, %q) = %q, want %q", tc.name, tc.mime, got, tc.want)
		}
	}
}

// Files are sent in order while the request's allowance lasts; one that
// does not fit is a line, and a smaller one after it still goes.
func TestFilesWithinTheirAllowance(t *testing.T) {
	a, err := New(llm.Config{Model: "gemini-2.5-flash", APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	files := 20
	var got []string
	for _, f := range []llm.File{
		{Name: "a.pdf", MIME: "application/pdf", Data: []byte("0123456789")}, // 16 in base64
		{Name: "b.pdf", MIME: "application/pdf", Data: []byte("0123456789")},
		{Name: "c.md", MIME: "text/markdown", Data: []byte("# ok")},
		{Name: "d.md", MIME: "text/markdown", Data: []byte("# too long")},
		{Name: "e.txt", MIME: "text/plain", Data: []byte("x")},
	} {
		got = append(got, string(mustJSON(t, a.filePart(&f, &files))))
	}
	want := []string{
		`{"inlineData":{"mimeType":"application/pdf","data":"MDEyMzQ1Njc4OQ=="}}`,
		`{"text":"[file \"b.pdf\" left out: it is too large to send]"}`,
		`{"text":"[file \"c.md\"]\n# ok"}`,
		`{"text":"[file \"d.md\" left out: it is too large to send]"}`,
		`{"text":"[file \"e.txt\" left out: it is too large to send]"}`,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file %d: %s, want %s", i, got[i], want[i])
		}
	}
	if files != 0 {
		t.Errorf("%d bytes of the allowance left, want 0", files)
	}

	// Text that is not UTF-8 is not text, and Markdown is not a type
	// Gemini takes inline.
	files = maxFileBytes
	f := llm.File{Name: "f.md", MIME: "text/markdown", Data: []byte{0xff, 0xfe}}
	if got, want := string(mustJSON(t, a.filePart(&f, &files))), `{"text":"[file \"f.md\" left out: Gemini does not take files of its type]"}`; got != want {
		t.Errorf("%s, want %s", got, want)
	}
}

// Without a client of its own, the adapter uses the default transport, and
// a base URL of its own is taken as it is.
func TestTheDefaultClient(t *testing.T) {
	f := newFake(t, ok(`{"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}]}`))
	a, err := New(llm.Config{Model: "gemini-2.5-flash", BaseURL: f.srv.URL, APIKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Call(context.Background(), &llm.Request{Messages: []llm.Message{llm.UserText("Hello")}})
	if err != nil || resp.Text() != "Hi" {
		t.Fatalf("got %v, %v", resp, err)
	}
	if r := f.last(t); r.header.Get("X-Goog-Api-Key") != testKey || r.url != "" {
		t.Fatalf("key header %q, redirected from %q", r.header.Get("X-Goog-Api-Key"), r.url)
	}
}

// What the adapter kept on a part is read only when it can be: an Opaque it
// cannot read gives no signature and no id.
func TestAnUnreadableOpaqueSendsNothing(t *testing.T) {
	f := newFake(t, ok(`{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`))
	a := f.newAdapter(t)
	req := &llm.Request{
		Tools: []llm.Tool{{Name: "course_get"}},
		Messages: []llm.Message{
			llm.UserText("Course?"),
			{Role: llm.RoleAssistant, Parts: []llm.Part{
				{Type: llm.PartText, Text: "Looking.", Maker: testMaker, Opaque: json.RawMessage(`"not an object"`)},
				{Type: llm.PartToolCall, ID: "fc-1", Name: "course_get", Maker: testMaker, Opaque: json.RawMessage(`["fc-1"]`)},
				{Type: llm.PartReasoning, Maker: testMaker, Opaque: json.RawMessage(`"not a part"`)},
			}},
			{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, CallID: "fc-1", Name: "course_get", Content: "{}"}}},
		},
	}
	if _, err := a.Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	checkContents(t, f.last(t), `[
		{"role":"user","parts":[{"text":"Course?"}]},
		{"role":"model","parts":[{"text":"Looking."},{"functionCall":{"name":"course_get","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"course_get","response":{"output":{}}}}]}]`)
}

func TestReadingGooglesError(t *testing.T) {
	for _, body := range []string{``, `[]`, `[1]`, `{}`, `{"error":"plain text"}`, `<html></html>`} {
		if _, ok := parseGoogleError([]byte(body)); ok {
			t.Errorf("%q read as Google's error", body)
		}
	}
	if g, ok := parseGoogleError([]byte(`[{"error":{"code":503,"message":"busy","status":"UNAVAILABLE"}}]`)); !ok || g.Error.Status != "UNAVAILABLE" {
		t.Errorf("a listed error not read: %+v", g)
	}
}

func TestTokenLimit(t *testing.T) {
	for msg, want := range map[string]bool{
		"the input token count (1196265) exceeds the maximum number of tokens allowed (1048575).": true,
		"request had too many tokens in the prompt.":                                              true,
		"the prompt exceeds the context limit.":                                                   false,
		"max_output_tokens exceeds the limit of 65536 tokens.":                                    false,
		"unable to submit request because it has a maxoutputtokens value of 70000.":               false,
		"the thinking budget of 40000 tokens exceeds the limit of 32768.":                         false,
		"please use a valid role: user, model.":                                                   false,
	} {
		if got := tokenLimit(msg); got != want {
			t.Errorf("tokenLimit(%q) = %v, want %v", msg, got, want)
		}
	}
}
