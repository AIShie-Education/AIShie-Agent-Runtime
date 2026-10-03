package probe

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/providers"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
)

const key = "sk-proj-TestKeyThatMustNotLeak0123456789"

// TryModel calls once, for one output token, and names what the provider
// said by the contract's results, keeping a code only of a safe shape and
// nothing of the provider's words.
func TestTryModel(t *testing.T) {
	var mu sync.Mutex
	var status int
	var body string
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Header.Get("Authorization")+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	ok := `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`
	refusal := func(code string) string {
		return fmt.Sprintf(`{"error":{"code":%q,"message":"Incorrect API key provided: sk-proj-****0123. See the docs."}}`, code)
	}
	m := config.Model{Adapter: llm.AdapterOpenAIChat, Model: "m", BaseURL: srv.URL, Params: config.ModelParams{MaxOutputTokens: 2000}}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		result string
		code   string
	}{
		{"answered", 200, ok, ResultOK, ""},
		{"401", 401, refusal("invalid_api_key"), ResultKeyRefused, "invalid_api_key"},
		{"403", 403, refusal("forbidden"), ResultKeyRefused, "forbidden"},
		{"404", 404, refusal("model_not_found"), ResultModelNotFound, "model_not_found"},
		{"no such model, said with a 400", 400, refusal("model_not_exist"), ResultModelNotFound, "model_not_exist"},
		{"429", 429, refusal("rate_limit_exceeded"), ResultKeyAccepted, "rate_limit_exceeded"},
		{"500", 500, refusal("server_error"), ResultKeyAccepted, "server_error"},
		{"a code of no safe shape", 400, refusal("bad code <script>"), ResultKeyAccepted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			status, body = tc.status, tc.body
			mu.Unlock()
			tr, err := TryModel(t.Context(), m, key, srv.Client(), nil)
			if err != nil || tr.Result != tc.result || tr.ProviderCode != tc.code || tr.Err != nil {
				t.Fatalf("%+v %v", tr, err)
			}
			if tc.status != 200 && tr.HTTPStatus != tc.status {
				t.Errorf("HTTP %d", tr.HTTPStatus)
			}
			if s := fmt.Sprintf("%+v", tr); strings.Contains(s, "Incorrect") || strings.Contains(s, "sk-") {
				t.Errorf("the trial holds the provider's words: %s", s)
			}
		})
	}
	mu.Lock()
	for _, s := range seen {
		if !strings.HasPrefix(s, "Bearer "+key+" ") || !strings.Contains(s, `"max_tokens":1`) && !strings.Contains(s, `"max_completion_tokens":1`) {
			t.Errorf("the call: %.200s", strings.ReplaceAll(s, key, "<key>"))
		}
	}
	mu.Unlock()

	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	m.BaseURL = dead.URL
	if tr, err := TryModel(t.Context(), m, key, nil, nil); err != nil || tr.Result != ResultUnreachable || tr.Kind != llm.ErrNetwork {
		t.Errorf("no provider there: %+v %v", tr, err)
	}
	if _, err := TryModel(t.Context(), config.Model{Adapter: "nope"}, key, nil, nil); err == nil || strings.Contains(err.Error(), key) {
		t.Errorf("an adapter that cannot be built: %v", err)
	}
}

// A trial sends no upstream routing, even of a model at OpenRouter that
// has one: a trial refused for the routing would read as a key refused or
// a model not found. The model it was given keeps its routing.
func TestTryModelSendsNoRouting(t *testing.T) {
	var cfgs []llm.Config
	deny := "deny"
	m := config.Model{Adapter: llm.AdapterOpenAIChat, Model: "meta-llama/llama-3.3-70b-instruct", BaseURL: "https://openrouter.ai/api/v1",
		Params: config.ModelParams{MaxOutputTokens: 100}, OpenRouter: &openrouter.Routing{DataCollection: &deny, Only: []string{"groq"}}}
	var sent string
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Request: r,
			Body: io.NopCloser(strings.NewReader(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"length"}]}`))}, nil
	})}
	newAdapter := func(c llm.Config) (llm.Adapter, error) {
		cfgs = append(cfgs, c)
		return providers.New(c)
	}
	tr, err := TryModel(t.Context(), m, key, client, newAdapter)
	if err != nil || tr.Result != ResultOK {
		t.Fatalf("%+v %v", tr, err)
	}
	if len(cfgs) != 1 || cfgs[0].OpenRouter != nil || strings.Contains(sent, `"provider"`) || !strings.Contains(sent, `"model":"meta-llama/llama-3.3-70b-instruct"`) {
		t.Errorf("the trial's routing: %+v; sent %s", cfgs, sent)
	}
	if m.OpenRouter == nil || len(m.OpenRouter.Only) != 1 {
		t.Error("the model lost its routing")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
