package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/netguard"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/probe"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// fakeHosting is the configuration in force: the operator's YAML and the
// price table.
type fakeHosting struct {
	mu     sync.Mutex
	yaml   *config.Config
	prices *pricing.Table
}

func (f *fakeHosting) YAML() *config.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.yaml
}

func (f *fakeHosting) Prices() *pricing.Table {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prices
}

// provider is a model provider the hosted-model client reaches instead of
// the internet: it answers each call with the status set, and keeps what
// it was sent.
type provider struct {
	mu     sync.Mutex
	status int
	fail   error
	seen   []*http.Request
	bodies []string
}

func (p *provider) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	p.seen, p.bodies = append(p.seen, r), append(p.bodies, string(b))
	if p.fail != nil {
		return nil, p.fail
	}
	body := `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"O"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`
	h := http.Header{"Content-Type": {"application/json"}}
	switch {
	case p.status == http.StatusFound:
		h.Set("Location", "http://169.254.169.254/latest/meta-data/")
		body = ""
	case p.status >= 400:
		body = `{"error":{"code":"code_` + http.StatusText(p.status)[:3] + `","message":"Incorrect API key provided: sk-proj-****wxyz, see the docs"}}`
		if p.status == http.StatusNotFound {
			body = `{"error":{"code":"model_not_found","message":"The model does not exist"}}`
		}
	}
	return &http.Response{StatusCode: p.status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func (p *provider) set(status int, fail error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status, p.fail = status, fail
}

const table = `
version: "t1"
prices:
  - {provider: openai, model: gpt-4.1-mini, from: 2025-04-14, usd_per_mtok: {input: 0.4, output: 1.6}}
  - {provider: openai, model: gpt-4.1-preview, from: 2025-04-14, usd_per_mtok: {input: 0.4, output: 1.6}}
  - {provider: openai, model: gpt-5, from: 2027-01-01, usd_per_mtok: {input: 1, output: 8}}
  - {provider: openai, model: "o*", from: 2025-01-01, usd_per_mtok: {input: 1, output: 1}}
  - {provider: deepseek, model: deepseek-chat, from: 2025-09-29, usd_per_mtok: {input: 0.28, output: 0.42}}
`

// modelWorld is a host world with the configuration in force and a
// provider behind the hosted-model client.
func newModelWorld(t *testing.T, rt config.Runtime) (*hostWorld, *provider, *fakeHosting) {
	t.Helper()
	prices, err := pricing.Parse([]byte(table))
	if err != nil {
		t.Fatal(err)
	}
	fh := &fakeHosting{yaml: &config.Config{Runtime: rt}, prices: prices}
	p := &provider{status: http.StatusOK}
	h := newHostWorld(t, fakecore.Options{}, func(o *Options) {
		o.Hosting = fh
		o.ModelHTTP = netguard.NoRedirects(&http.Client{Transport: p})
	})
	h.s.keyTest.reset(Rate{PerMinute: 60000, Burst: 10000})
	return h, p, fh
}

var denyPreviews = config.Runtime{DeniedModels: []string{"*:*:*-preview"}}

// GET /models lists every provider offered, each endpoint an official one
// for each of its adapters, with the models the price table prices by
// name today and the school does not deny; the school's key is not
// offered.
func TestModels(t *testing.T) {
	h, _, _ := newModelWorld(t, denyPreviews)
	a := h.call("GET", "models", h.yuki, "")
	wantSecured(t, a, "no-store")
	var m Models
	a.decode(t, &m)
	if a.code != 200 || !m.OwnKey.Offered || m.SchoolKey.Offered || m.SchoolKey.Offers == nil || len(m.OwnKey.Providers) != len(registry.Offers()) {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if !strings.Contains(a.body, `"school_key":{"offered":false,"offers":[]}`) {
		t.Errorf("the school's key: %s", a.body)
	}
	for _, p := range m.OwnKey.Providers {
		var urls []string
		switch p.Endpoint.Kind {
		case registry.EndpointFixed:
			urls = append(urls, p.Endpoint.BaseURL)
		case registry.EndpointChoice:
			for _, c := range p.Endpoint.Choices {
				urls = append(urls, c.BaseURL)
			}
		case registry.EndpointAzureResource:
			urls = append(urls, "https://"+p.Endpoint.Example+".openai.azure.com/openai/v1/")
		case registry.EndpointBedrockRegion:
			for _, r := range p.Endpoint.Suggested {
				urls = append(urls, "https://bedrock-runtime."+r+".amazonaws.com")
			}
		}
		if len(urls) == 0 || len(p.Adapters) == 0 {
			t.Errorf("%s: %+v", p.Provider, p)
		}
		for _, raw := range urls {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			for _, ad := range p.Adapters {
				if !registry.Official(ad, u) {
					t.Errorf("%s: %s is not official for %s", p.Provider, raw, ad)
				}
			}
		}
		switch p.Provider {
		case "openai":
			if len(p.SuggestedModels) != 1 || p.SuggestedModels[0] != (SuggestedModel{Model: "gpt-4.1-mini", Priced: true}) || *p.KeyPrefix != "sk-" {
				t.Errorf("openai: %+v", p)
			}
		case "azure":
			if p.KeyPrefix != nil || p.Endpoint.Pattern == "" || len(p.SuggestedModels) != 0 {
				t.Errorf("azure: %+v", p)
			}
		}
	}
}

// keysTest is a keys/test body.
func keysTest(provider, model, key string, more ...string) string {
	m := map[string]any{"provider": provider, "model": model, "key": key}
	for i := 0; i+1 < len(more); i += 2 {
		m[more[i]] = more[i+1]
	}
	b, _ := json.Marshal(m)
	return string(b)
}

const ownKey = "sk-proj-OwnKeyThatMustNotLeakAnywhere-0123wxyz"

// keys/test tries the key at the provider's official endpoint, once, for
// one token, over the hosted-model client, and names what came back; the
// key is never given back, logged or audited, its hint aside.
func TestKeyTest(t *testing.T) {
	h, p, _ := newModelWorld(t, denyPreviews)
	for _, tc := range []struct {
		name   string
		status int
		fail   error
		result string
		http   int
	}{
		{"answered", 200, nil, probe.ResultOK, 0},
		{"401", 401, nil, probe.ResultKeyRefused, 401},
		{"403", 403, nil, probe.ResultKeyRefused, 403},
		{"404", 404, nil, probe.ResultModelNotFound, 404},
		{"429", 429, nil, probe.ResultKeyAccepted, 429},
		{"500", 500, nil, probe.ResultKeyAccepted, 500},
		{"the dial refused", 0, netguard.ErrBlocked, probe.ResultUnreachable, 0},
		{"a redirect", http.StatusFound, nil, probe.ResultUnreachable, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.set(tc.status, tc.fail)
			a := h.call("POST", "keys/test", h.yuki, keysTest("openai", "gpt-4.1-mini", ownKey))
			wantSecured(t, a, "no-store")
			var kt KeyTest
			a.decode(t, &kt)
			status := 0
			if kt.HTTPStatus != nil {
				status = *kt.HTTPStatus
			}
			if a.code != 200 || kt.Result != tc.result || status != tc.http {
				t.Errorf("%d %s", a.code, a.body)
			}
			if strings.Contains(a.body, "Incorrect") || strings.Contains(a.body, "sk-") {
				t.Errorf("the answer holds the provider's words: %s", a.body)
			}
		})
	}
	p.mu.Lock()
	for i, r := range p.seen {
		if r.URL.Host != "api.openai.com" || r.Header.Get("Authorization") != "Bearer "+ownKey || !strings.Contains(p.bodies[i], `"max_completion_tokens":1`) && !strings.Contains(p.bodies[i], `"max_tokens":1`) {
			t.Errorf("call %d: %s %s", i, r.URL, strings.ReplaceAll(p.bodies[i], ownKey, "<key>"))
		}
		if r.URL.Host == "169.254.169.254" {
			t.Error("the redirect was followed")
		}
	}
	p.mu.Unlock()
	ev := h.events("key.test")
	if len(ev) != 8 || ev[0].Outcome != "ok" || ev[0].TargetID != "openai" ||
		!strings.Contains(string(ev[0].Detail), `"key_hint":"sk-proj-…wxyz"`) || !strings.Contains(string(ev[0].Detail), `"result":"ok"`) {
		t.Errorf("the audit: %+v", ev)
	}
	body := h.log.String()
	for _, e := range ev {
		body += string(e.Detail)
	}
	if strings.Contains(body, ownKey) || strings.Contains(body, "OwnKeyThatMustNotLeak") {
		t.Error("the key is logged or audited")
	}
}

// keys/test refuses a request it cannot try, each with its reason and
// field, before any call: and asks the provider nothing.
func TestKeyTestRefuses(t *testing.T) {
	h, p, _ := newModelWorld(t, denyPreviews)
	for _, tc := range []struct {
		name, body    string
		code          int
		reason, field string
	}{
		{"no provider", keysTest("", "m", ownKey), 400, ReasonMissingField, "/provider"},
		{"no model", keysTest("openai", "", ownKey), 400, ReasonMissingField, "/model"},
		{"a model of no model's shape", keysTest("openai", "gpt 4", ownKey), 400, ReasonInvalidField, "/model"},
		{"a Core token as the key", keysTest("openai", "gpt-4.1-mini", "ais_"+strings.Repeat("a", 12)+"_"+strings.Repeat("B", 43)), 400, ReasonKeyMalformed, "/key"},
		{"an invitation as the key", keysTest("openai", "gpt-4.1-mini", "aisinv_abcdefgh"), 400, ReasonKeyMalformed, "/key"},
		{"a short key", keysTest("openai", "gpt-4.1-mini", "sk-1"), 400, ReasonKeyMalformed, "/key"},
		{"a key with a space", keysTest("openai", "gpt-4.1-mini", "sk-proj abc defgh"), 400, ReasonKeyMalformed, "/key"},
		{"a key of more than ASCII", keysTest("openai", "gpt-4.1-mini", "sk-proj-ключключ"), 400, ReasonKeyMalformed, "/key"},
		{"an unknown provider", keysTest("ollama", "llama3", ownKey), 400, ReasonUnknownProvider, "/provider"},
		{"an adapter not offered", keysTest("anthropic", "claude-x", ownKey, "adapter", "openai_chat"), 400, ReasonAdapterNotOffered, "/adapter"},
		{"an endpoint not offered", keysTest("qwen", "qwen-plus", ownKey, "endpoint", "evil"), 400, ReasonUnknownEndpoint, "/endpoint"},
		{"an endpoint of a fixed provider", keysTest("openai", "gpt-4.1-mini", ownKey, "endpoint", "global"), 400, ReasonInvalidField, "/endpoint"},
		{"no Azure resource", keysTest("azure", "my-deployment", ownKey), 400, ReasonMissingField, "/resource"},
		{"an Azure resource with a dot", keysTest("azure", "my-deployment", ownKey, "resource", "evil.com"), 400, ReasonInvalidField, "/resource"},
		{"a region of no region's shape", keysTest("bedrock", "anthropic.claude", ownKey, "region", "us-east-1.evil"), 400, ReasonInvalidField, "/region"},
		{"a URL", `{"provider":"openai","model":"m","key":"` + ownKey + `","base_url":"https://evil.example"}`, 400, ReasonUnknownField, "/base_url"},
		{"too many output tokens", `{"provider":"openai","model":"m","key":"` + ownKey + `","max_output_tokens":99999}`, 400, ReasonInvalidField, "/max_output_tokens"},
		{"an effort of none", keysTest("openai", "gpt-4.1-mini", ownKey, "reasoning_effort", "extreme"), 400, ReasonInvalidField, "/reasoning_effort"},
		{"a model the school denies", keysTest("openai", "gpt-4.1-preview", ownKey), 422, ReasonModelDenied, "/model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.call("POST", "keys/test", h.yuki, tc.body)
			e := wantRefused(t, a, tc.code, map[int]string{400: CodeInvalidArgument, 422: CodeFailedPrecondition}[tc.code], tc.reason)
			if e.Details["field"] != tc.field {
				t.Errorf("field %v, want %s", e.Details["field"], tc.field)
			}
			if strings.Contains(a.body, ownKey) {
				t.Error("the answer holds the key")
			}
		})
	}
	p.mu.Lock()
	if len(p.seen) != 0 {
		t.Errorf("the provider was asked %d times", len(p.seen))
	}
	p.mu.Unlock()

	// The allowance: its bucket, then its day.
	h.s.keyTest.reset(Rate{PerMinute: 60, Burst: 1})
	h.call("POST", "keys/test", h.yuki, keysTest("openai", "gpt-4.1-mini", ownKey))
	wantRefused(t, h.call("POST", "keys/test", h.yuki, keysTest("openai", "gpt-4.1-mini", ownKey)), 429, CodeRateLimited, ReasonRateLimited)
	h.s.keyTest.reset(Rate{PerMinute: 60000, Burst: 10000})
	h.s.keyDay = newDailyLimiter(2)
	for range 2 {
		if a := h.call("POST", "keys/test", h.yuki, keysTest("openai", "gpt-4.1-mini", ownKey)); a.code != 200 {
			t.Fatalf("%d %s", a.code, a.body)
		}
	}
	a := h.call("POST", "keys/test", h.yuki, keysTest("openai", "gpt-4.1-mini", ownKey))
	e := wantRefused(t, a, 429, CodeRateLimited, ReasonRateLimited)
	if secs, _ := e.Details["retry_after_seconds"].(float64); secs != 14*3600 {
		t.Errorf("until the next UTC day: %v", e.Details)
	}
	if a := h.call("POST", "keys/test", h.ken, keysTest("openai", "gpt-4.1-mini", ownKey)); a.code != 200 {
		t.Errorf("another person's day: %d", a.code)
	}
	h.add(14 * time.Hour)
	if a := h.call("POST", "keys/test", h.yuki, keysTest("openai", "gpt-4.1-mini", ownKey)); a.code != 200 {
		t.Errorf("the next day: %d %s", a.code, a.body)
	}
}

// patch is a PATCH of the agent at version, with its body.
func (h *hostWorld) patch(id string, version, body string) answer {
	h.t.Helper()
	headers := []string{}
	if version != "" {
		headers = append(headers, "If-Match", version)
	}
	return h.call("PATCH", "agents/"+id, h.yuki, body, headers...)
}

const ownOpenAI = `{"model":{"own":{"provider":"openai","model":"gpt-4.1-mini"}},"own_key":{"value":"` + ownKey + `"}}`

// PATCH sets the owner's model and key: the model written as §5.9 says,
// its endpoint made from the offer, the key sealed; the agent starts at
// its new version; the row passes what the registry holds a row to.
func TestPatch(t *testing.T) {
	h, _, _ := newModelWorld(t, config.Runtime{})
	v := h.connect(h.yuki, h.helper.Token)
	ctx := context.Background()
	a := h.patch(v.ID, `"1"`, ownOpenAI)
	wantSecured(t, a, "no-store")
	var got HostedAgent
	a.decode(t, &got)
	if a.code != 200 || got.Version != 2 || got.Status != StatusStarting || a.header.Get("ETag") != `"2"` || got.OwnKey == nil ||
		got.OwnKey.Hint != "sk-proj-…wxyz" || got.OwnKey.Provider == nil || *got.OwnKey.Provider != "openai" {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if own := got.Model.Own; own == nil || *own != (OwnModel{Provider: "openai", Adapter: "openai_chat", Model: "gpt-4.1-mini", PriceKnown: true}) {
		t.Errorf("the model: %+v", got.Model.Own)
	}
	row, err := h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	if string(row.Settings) != `{"model":{"adapter":"openai_chat","model":"gpt-4.1-mini","provider":"openai","key_source":"own"}}` ||
		row.KeyProvider != "openai" {
		t.Errorf("the row: %s %q", row.Settings, row.KeyProvider)
	}
	firstKey := row.KeySecretID
	// The row passes what the registry holds a row to.
	cfg, _, err := registry.Build(ctx, &config.Config{}, h.st, registry.Options{CoreBaseURL: h.srv.URL})
	h.ok(err)
	if len(cfg.Agents) != 1 || len(cfg.Rejected) != 0 || cfg.Agents[0].Model.Model != "gpt-4.1-mini" || cfg.Agents[0].Hosted.Version != 2 {
		t.Errorf("the build: %+v %+v", cfg.Agents, cfg.Rejected)
	}
	ev := h.events("agent.update")
	if len(ev) != 1 || !strings.Contains(string(ev[0].Detail), `"changed":["model.own","own_key"]`) ||
		!strings.Contains(string(ev[0].Detail), `"key_hint":"sk-proj-…wxyz"`) || !strings.Contains(string(ev[0].Detail), `"version":2`) {
		t.Errorf("the audit: %+v", ev)
	}
	// Running once the worker has put version 2 in force.
	h.ok(h.st.SetAgentState(ctx, store.AgentState{AgentID: v.ID, State: store.AgentRunning, ConfigVersion: 2}))
	h.call("GET", "agents/"+v.ID, h.yuki, "").decode(t, &got)
	if got.Status != StatusRunning {
		t.Errorf("after the worker: %s", got.Status)
	}

	// A no-op writes nothing.
	for _, body := range []string{`{}`, `{"model":{"own":{"provider":"openai","model":"gpt-4.1-mini"}}}`, `{"model":{}}`} {
		a = h.patch(v.ID, `"2"`, body)
		a.decode(t, &got)
		if a.code != 200 || got.Version != 2 {
			t.Errorf("no-op %s: %d %s", body, a.code, a.body)
		}
	}
	// Azure, by resource, with the stored key: another provider's key is
	// not sent to it.
	azure := `{"model":{"own":{"provider":"azure","model":"my-gpt","resource":"my-res1","max_output_tokens":1000,"reasoning_effort":"low"}}}`
	e := wantRefused(t, h.patch(v.ID, `"2"`, azure), 422, CodeFailedPrecondition, ReasonOwnKeyProviderMismatch)
	if e.Details["field"] != "/own_key" {
		t.Errorf("mismatch: %+v", e)
	}
	azureWithKey := strings.TrimSuffix(azure, "}") + `,"own_key":{"value":"azure-key-0123456789abcdef"}}`
	a = h.patch(v.ID, `"2"`, azureWithKey)
	a.decode(t, &got)
	if a.code != 200 || got.Version != 3 || got.Model.Own == nil || got.Model.Own.Resource == nil || *got.Model.Own.Resource != "my-res1" ||
		*got.Model.Own.MaxOutputTokens != 1000 || *got.Model.Own.ReasoningEffort != "low" || got.Model.Own.Adapter != "openai_responses" {
		t.Fatalf("azure: %d %s", a.code, a.body)
	}
	row, err = h.st.HostedAgent(ctx, v.ID)
	h.ok(err)
	if !strings.Contains(string(row.Settings), `"base_url":"https://my-res1.openai.azure.com/openai/v1/"`) || row.KeyProvider != "azure" {
		t.Errorf("the azure row: %s", row.Settings)
	}
	// The key replaced destroyed the one before.
	if _, err := h.st.Secret(ctx, firstKey); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old key's secret: %v", err)
	}
	// The model cleared: back to needs_model; the key is kept.
	a = h.patch(v.ID, `"3"`, `{"model":{"own":null}}`)
	a.decode(t, &got)
	if a.code != 200 || got.Status != StatusNeedsModel || got.Model.Own != nil || got.OwnKey == nil {
		t.Errorf("cleared: %d %s", a.code, a.body)
	}
	if row, _ := h.st.HostedAgent(ctx, v.ID); string(row.Settings) != `{}` {
		t.Errorf("the settings: %s", row.Settings)
	}
	// The key cleared: with no model, it may be.
	a = h.patch(v.ID, `"4"`, `{"own_key":null}`)
	a.decode(t, &got)
	if a.code != 200 || got.OwnKey != nil || got.Version != 5 {
		t.Errorf("key cleared: %d %s", a.code, a.body)
	}
	h.noSecrets(a)
	if strings.Contains(h.log.String(), ownKey) {
		t.Error("the log holds the key")
	}
}

// PATCH refuses, each with its reason: If-Match missing, *, of another
// version, or not a strong tag; the school's key; a model with no key; a
// model the school denies; settings the runtime's defaults break; a Core
// token as the key; a member it does not take, at its pointer.
func TestPatchRefuses(t *testing.T) {
	breaking := config.Runtime{DeniedModels: []string{"*:*:*-preview"},
		Defaults: map[string]any{"model": map[string]any{"params": map[string]any{"temperature": 5.0}}}}
	h, _, fh := newModelWorld(t, config.Runtime{DeniedModels: []string{"*:*:*-preview"}})
	v := h.connect(h.yuki, h.helper.Token)
	for _, tc := range []struct {
		name, version, body string
		status              int
		code, reason, field string
	}{
		{"no If-Match", "", ownOpenAI, 428, CodeVersionRequired, ReasonVersionRequired, ""},
		{"If-Match *", "*", ownOpenAI, 428, CodeVersionRequired, ReasonVersionRequired, ""},
		{"a weak tag", `W/"1"`, ownOpenAI, 400, CodeInvalidArgument, ReasonBadIfMatch, ""},
		{"not a tag", `1`, ownOpenAI, 400, CodeInvalidArgument, ReasonBadIfMatch, ""},
		{"another version", `"9"`, ownOpenAI, 412, CodeVersionMismatch, ReasonVersionMismatch, ""},
		{"the school's key", `"1"`, `{"model":{"school":{"offer_id":"x"}}}`, 422, CodeFailedPrecondition, ReasonSchoolKeyNotOffered, "/model/school"},
		{"no key", `"1"`, `{"model":{"own":{"provider":"openai","model":"gpt-4.1-mini"}}}`, 422, CodeFailedPrecondition, ReasonOwnKeyRequired, "/own_key"},
		{"a denied model", `"1"`, `{"model":{"own":{"provider":"openai","model":"gpt-5-preview"}},"own_key":{"value":"` + ownKey + `"}}`, 422,
			CodeFailedPrecondition, ReasonModelDenied, "/model/own/model"},
		{"a Core token as the key", `"1"`, `{"own_key":{"value":"` + h.helper.Token + `"}}`, 400, CodeInvalidArgument, ReasonKeyMalformed, "/own_key/value"},
		{"no key's value", `"1"`, `{"own_key":{}}`, 400, CodeInvalidArgument, ReasonMissingField, "/own_key/value"},
		{"a member of own it does not take", `"1"`, `{"model":{"own":{"provider":"openai","model":"m","base_url":"https://x"}}}`, 400,
			CodeInvalidArgument, ReasonUnknownField, "/model/own/base_url"},
		{"a member of model it does not take", `"1"`, `{"model":{"fallback":null}}`, 400, CodeInvalidArgument, ReasonUnknownField, "/model/fallback"},
		{"a member it does not take", `"1"`, `{"settings":{}}`, 400, CodeInvalidArgument, ReasonUnknownField, "/settings"},
		{"model as null", `"1"`, `{"model":null}`, 400, CodeInvalidArgument, ReasonInvalidField, "/model"},
		{"an unknown provider", `"1"`, `{"model":{"own":{"provider":"vllm","model":"m"}}}`, 400, CodeInvalidArgument, ReasonUnknownProvider, "/model/own/provider"},
		{"no Bedrock region", `"1"`, `{"model":{"own":{"provider":"bedrock","model":"m"}}}`, 400, CodeInvalidArgument, ReasonMissingField, "/model/own/region"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.patch(v.ID, tc.version, tc.body)
			e := wantRefused(t, a, tc.status, tc.code, tc.reason)
			if tc.field != "" && e.Details["field"] != tc.field {
				t.Errorf("field %v, want %s", e.Details["field"], tc.field)
			}
			if tc.reason == ReasonVersionMismatch && e.Details["current_version"] != 1.0 {
				t.Errorf("current_version %v", e.Details["current_version"])
			}
		})
	}
	wantRefused(t, h.call("PATCH", "agents/"+v.ID, h.ken, ownOpenAI, "If-Match", `"1"`), 404, CodeNotFound, ReasonAgentNotFound)

	// The runtime's defaults break it: settings_rejected, with why.
	fh.mu.Lock()
	fh.yaml = &config.Config{Runtime: breaking}
	fh.mu.Unlock()
	a := h.patch(v.ID, `"1"`, ownOpenAI)
	e := wantRefused(t, a, 422, CodeFailedPrecondition, ReasonSettingsRejected)
	problems, _ := e.Details["problems"].([]any)
	if len(problems) == 0 || !strings.Contains(problems[0].(string), "temperature") {
		t.Errorf("problems: %v", e.Details)
	}
	if row, _ := h.st.HostedAgent(context.Background(), v.ID); row.Version != 1 || row.KeySecretID != "" {
		t.Errorf("a refused patch wrote: %+v", row)
	}
	if secrets, _ := h.st.ListSecrets(context.Background(), "", 100); len(secrets) != 1 {
		t.Errorf("a refused patch stored a key: %d secrets", len(secrets))
	}
	h.noSecrets(a)
}
