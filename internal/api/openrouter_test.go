package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
)

// openRouterStub stands in for OpenRouter: it answers its three lists from
// testdata/openrouter (trimmed copies of its answers for Llama 3.3 70B,
// novita/bf16 given what that model's list does not show: no quantization,
// a price a request and an image, a long-context price, a price that does
// not read, its latency, a link not https and datacenters not codes), 404
// for any other model, and what a test sets in their place; and it keeps
// every request.
type openRouterStub struct {
	t        *testing.T
	mu       sync.Mutex
	status   map[string]int    // path → a status to answer instead
	body     map[string]string // path → a body to answer instead
	seen     []*http.Request
	fixtures map[string][]byte
}

const (
	llamaPath     = "/api/v1/models/meta-llama/llama-3.3-70b-instruct/endpoints"
	providersPath = "/api/v1/providers"
	zdrPath       = "/api/v1/endpoints/zdr"
)

func newOpenRouterStub(t *testing.T) (*openRouterStub, *httptest.Server) {
	st := &openRouterStub{t: t, status: map[string]int{}, body: map[string]string{}, fixtures: map[string][]byte{}}
	for path, file := range map[string]string{llamaPath: "endpoints.json", providersPath: "providers.json", zdrPath: "zdr.json"} {
		b, err := os.ReadFile(filepath.Join("testdata", "openrouter", file))
		if err != nil {
			t.Fatal(err)
		}
		st.fixtures[path] = b
	}
	srv := httptest.NewServer(st)
	t.Cleanup(srv.Close)
	return st, srv
}

func (st *openRouterStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	st.seen = append(st.seen, r.Clone(r.Context()))
	status, body := st.status[r.URL.Path], st.body[r.URL.Path]
	fixture, ok := st.fixtures[r.URL.Path]
	st.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case status != 0:
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	case body != "":
		_, _ = w.Write([]byte(body))
	case !ok:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"Not Found","code":404}}`))
	default:
		_, _ = w.Write(fixture)
	}
}

// serve has path answered with body, as a model's list of its own.
func (st *openRouterStub) serve(path, body string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.fixtures[path] = []byte(body)
}

func (st *openRouterStub) set(path string, status int, body string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.status[path], st.body[path] = status, body
}

// calls are how many requests reached path.
func (st *openRouterStub) calls(path string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for _, r := range st.seen {
		if r.URL.Path == path {
			n++
		}
	}
	return n
}

// The price file of these tests: OpenRouter's Llama models by a glob.
const openRouterTable = `
version: "or1"
prices:
  - {provider: openrouter, model: "meta-llama/*", from: 2025-01-01, id: llama, usd_per_mtok: {input: 0.72, output: "0.720000"}}
`

// newOpenRouterWorld is a host world whose hosted-model client reaches the
// stub, as OpenRouter's base.
func newOpenRouterWorld(t *testing.T) (*hostWorld, *openRouterStub) {
	t.Helper()
	st, srv := newOpenRouterStub(t)
	prices, err := pricing.Parse([]byte(openRouterTable))
	if err != nil {
		t.Fatal(err)
	}
	h := newHostWorld(t, fakecore.Options{}, func(o *Options) {
		o.Hosting = &fakeHosting{yaml: &config.Config{}, prices: prices}
		o.ModelHTTP, o.OpenRouterBaseURL = srv.Client(), srv.URL+"/api/v1"
	})
	h.s.general.reset(Rate{PerMinute: 60000, Burst: 10000})
	return h, st
}

// orAdmin asks the route as an administrator, with an assertion made now
// on the API's clock, which the tests move on.
func (h *hostWorld) orAdmin(query string) answer {
	h.t.Helper()
	now := h.clock()
	token := h.core.assert(h.t, claims(ken, "admin", func(c map[string]any) {
		c["iat"], c["nbf"], c["exp"], c["jti"] = now.Unix(), now.Unix(), now.Add(5*time.Minute).Unix(), "j-"+now.String()
	}))
	return h.send(req{method: "GET", path: Prefix + "admin/openrouter/endpoints" + query, token: token})
}

const llamaQuery = "?model=meta-llama/llama-3.3-70b-instruct"

// The route lists the model's upstream endpoints in OpenRouter's order,
// each as the page reads it: its prices per million tokens from
// OpenRouter's per token, exactly, and a request's and an image's; its
// tools from supported_parameters; whether OpenRouter lists it as ZDR (of
// this model alone); its provider by its slug's base or by its name; no
// quantization as unknown; https links alone, and countries' codes
// alone; the first long-context price's bound; latency passed through,
// and null where OpenRouter gives none or not all four. The price is the
// price table's, by its glob, as GET /admin/prices writes it. OpenRouter
// is asked without a key, with what the runtime is.
func TestOpenRouterEndpoints(t *testing.T) {
	h, st := newOpenRouterWorld(t)
	a := h.orAdmin(llamaQuery)
	wantSecured(t, a, "no-store")
	var got OpenRouterEndpoints
	a.decode(t, &got)
	if a.code != 200 || got.Model != "meta-llama/llama-3.3-70b-instruct" || got.Name != "Meta: Llama 3.3 70B Instruct" || got.Stale ||
		!got.FetchedAt.Equal(at.Truncate(time.Second)) || got.Price == nil || got.Price.Version != "or1/llama" ||
		got.Price.USDPerMTok != (PriceRates{Input: "0.72", CacheRead: "0.72", CacheWrite: "0.72", Output: "0.72"}) {
		t.Fatalf("%d %s", a.code, a.body)
	}
	var slugs []string
	for _, e := range got.Endpoints {
		slugs = append(slugs, e.Slug)
	}
	if strings.Join(slugs, " ") != "deepinfra/turbo novita/bf16 cloudflare/fp8 sambanova-turbo groq google-vertex/us-central1 google-vertex" {
		t.Errorf("the endpoints, in OpenRouter's order: %v", slugs)
	}
	raw := map[string]json.RawMessage{}
	var list struct {
		Endpoints []json.RawMessage `json:"endpoints"`
	}
	a.decode(t, &list)
	for i, e := range list.Endpoints {
		raw[slugs[i]] = e
	}
	for slug, want := range map[string]string{
		"deepinfra/turbo": `{"slug":"deepinfra/turbo","provider":"deepinfra","provider_name":"DeepInfra","quantization":"fp8",` +
			`"usd_per_mtok":{"input":"0.1","output":"0.32","cache_read":null,"cache_write":null},"usd_per_request":null,"usd_per_image":null,` +
			`"discount":0,"higher_above_tokens":null,"context_length":131072,"max_output_tokens":16384,"max_prompt_tokens":null,` +
			`"tools":true,"tool_choice":true,"reasoning":false,"zdr":true,"status":0,"uptime_30m":98.59366707942685,"uptime_1d":98.48357814367765,` +
			`"latency":null,"throughput":null,"headquarters":"US","datacenters":[],"privacy_policy_url":"https://deepinfra.com/privacy",` +
			`"terms_of_service_url":"https://deepinfra.com/terms","status_page_url":"https://status.deepinfra.com/"}`,
		"novita/bf16": `{"slug":"novita/bf16","provider":"novita","provider_name":"Novita","quantization":"unknown",` +
			`"usd_per_mtok":{"input":"0.135","output":"0.4","cache_read":null,"cache_write":null},"usd_per_request":"0.0005","usd_per_image":"0.000003",` +
			`"discount":0,"higher_above_tokens":128000,"context_length":12288,"max_output_tokens":11059,"max_prompt_tokens":null,` +
			`"tools":true,"tool_choice":true,"reasoning":false,"zdr":true,"status":-5,"uptime_30m":53.55908776779545,"uptime_1d":88.8729302507773,` +
			`"latency":{"p50":420.5,"p75":610,"p90":900,"p99":2400},"throughput":null,"headquarters":"US","datacenters":["US"],` +
			`"privacy_policy_url":"https://novita.ai/legal/privacy-policy","terms_of_service_url":"https://novita.ai/legal/terms-of-service","status_page_url":null}`,
	} {
		var w, g any
		if err := json.Unmarshal([]byte(want), &w); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw[slug], &g); err != nil {
			t.Fatal(err)
		}
		wb, _ := json.Marshal(w)
		gb, _ := json.Marshal(g)
		if string(wb) != string(gb) {
			t.Errorf("%s:\n%s\nwant\n%s", slug, gb, wb)
		}
	}
	by := map[string]OpenRouterEndpoint{}
	for _, e := range got.Endpoints {
		by[e.Slug] = e
	}
	if e := by["sambanova-turbo"]; e.Provider == nil || *e.Provider != "sambanova" || e.Discount != 0.25 || e.Tools || e.Quantization != "unknown" {
		t.Errorf("by the provider's name: %+v", e)
	}
	if e := by["google-vertex/us-central1"]; e.Provider == nil || *e.Provider != "google-vertex" || e.ZDR == nil || !*e.ZDR || e.Uptime30m != nil {
		t.Errorf("by its base slug: %+v", e)
	}
	if e := by["cloudflare/fp8"]; e.ZDR == nil || *e.ZDR || e.Tools {
		t.Errorf("ZDR for another model alone: %+v", e)
	}
	if e := by["groq"]; e.USDPerMTok.CacheRead == nil || *e.USDPerMTok.CacheRead != "0.295" {
		t.Errorf("a cache price: %+v", e.USDPerMTok)
	}
	st.mu.Lock()
	seen := st.seen
	st.mu.Unlock()
	for _, r := range seen {
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Accept") != "application/json" ||
			r.Header.Get("User-Agent") != "aishie-runtime/dev" {
			t.Errorf("%s %s: %v", r.Method, r.URL.Path, r.Header)
		}
	}
	if st.calls(llamaPath) != 1 || st.calls(providersPath) != 1 || st.calls(zdrPath) != 1 {
		t.Errorf("calls: %d %d %d", st.calls(llamaPath), st.calls(providersPath), st.calls(zdrPath))
	}
	if ev, _ := h.st.AuditEvents(t.Context(), at.AddDate(-1, 0, 0), 100); len(ev) != 0 {
		t.Errorf("a read was audited: %+v", ev)
	}

	// A variant is its model's list, asked as it is written; its price is
	// the table's for the model as the query gives it, which no row may
	// price.
	var nitro OpenRouterEndpoints
	h.orAdmin("?model=meta-llama/llama-3.3-70b-instruct:nitro").decode(t, &nitro)
	if st.calls("/api/v1/models/meta-llama/llama-3.3-70b-instruct:nitro/endpoints") != 1 {
		t.Errorf("the variant was not asked as written")
	}
	st.serve("/api/v1/models/qwen/qwen3-max/endpoints", `{"data":{"id":"qwen/qwen3-max","name":"Qwen3 Max","endpoints":[]}}`)
	none := h.orAdmin("?model=qwen/qwen3-max")
	if none.code != 200 || !strings.Contains(none.body, `"price":null`) || !strings.Contains(none.body, `"endpoints":[]`) {
		t.Errorf("no price and no endpoint: %d %s", none.code, none.body)
	}
}

// A list is kept ten minutes, and asked again after; while OpenRouter
// cannot be reached the last list read is answered as stale for an hour,
// then not at all. /providers and /endpoints/zdr not read leave their
// fields null.
func TestOpenRouterEndpointsCache(t *testing.T) {
	h, st := newOpenRouterWorld(t)
	first := h.orAdmin(llamaQuery)
	if first.code != 200 {
		t.Fatalf("%d %s", first.code, first.body)
	}
	h.add(9 * time.Minute)
	if again := h.orAdmin(llamaQuery); again.code != 200 || st.calls(llamaPath) != 1 || st.calls(providersPath) != 1 || st.calls(zdrPath) != 1 {
		t.Fatalf("within ten minutes: %d, %d calls", again.code, st.calls(llamaPath))
	}
	h.add(time.Minute)
	var got OpenRouterEndpoints
	h.orAdmin(llamaQuery).decode(t, &got)
	if st.calls(llamaPath) != 2 || st.calls(providersPath) != 2 || got.Stale || !got.FetchedAt.Equal(at.Add(10*time.Minute)) {
		t.Fatalf("after ten minutes: %d calls, %+v", st.calls(llamaPath), got)
	}

	// OpenRouter down: the list read ten minutes ago, as stale, and its
	// lists as read.
	for _, p := range []string{llamaPath, providersPath, zdrPath} {
		st.set(p, http.StatusInternalServerError, "")
	}
	h.add(10 * time.Minute)
	got = OpenRouterEndpoints{}
	h.orAdmin(llamaQuery).decode(t, &got)
	if !got.Stale || !got.FetchedAt.Equal(at.Add(10*time.Minute)) || len(got.Endpoints) != 7 || got.Endpoints[0].ZDR == nil ||
		got.Endpoints[0].Provider == nil {
		t.Fatalf("stale: %+v", got)
	}
	h.add(50*time.Minute + time.Second)
	e := wantRefused(t, h.orAdmin(llamaQuery), 503, CodeUnavailable, ReasonOpenRouterUnavailable)
	if e.Details["http_status"] != float64(500) {
		t.Errorf("past the hour: %+v", e.Details)
	}

	// The model's list read, its lists not: their fields null.
	st.set(llamaPath, 0, "")
	got = OpenRouterEndpoints{}
	h.orAdmin(llamaQuery).decode(t, &got)
	if got.Stale || len(got.Endpoints) != 7 {
		t.Fatalf("without the lists: %+v", got)
	}
	for _, e := range got.Endpoints {
		if e.Provider != nil || e.ZDR != nil || e.Headquarters != nil || e.Datacenters == nil || len(e.Datacenters) != 0 ||
			e.PrivacyPolicyURL != nil || e.TermsOfServiceURL != nil || e.StatusPageURL != nil {
			t.Errorf("%s without the lists: %+v", e.Slug, e)
		}
	}
}

// A model OpenRouter does not have is 404 openrouter_model_not_found,
// never kept; OpenRouter failing, too slow to answer or answering what is
// not its list is 503 openrouter_unavailable, with its status.
func TestOpenRouterEndpointsErrors(t *testing.T) {
	h, st := newOpenRouterWorld(t)
	e := wantRefused(t, h.orAdmin("?model=meta-llama/no-such-model"), 404, CodeNotFound, ReasonOpenRouterModelNotFound)
	if e.Details["field"] != "model" {
		t.Errorf("%+v", e.Details)
	}
	wantRefused(t, h.orAdmin("?model=meta-llama/no-such-model"), 404, CodeNotFound, ReasonOpenRouterModelNotFound)
	if st.calls("/api/v1/models/meta-llama/no-such-model/endpoints") != 2 {
		t.Errorf("a model not found was kept")
	}
	for _, tc := range []struct {
		name, body string
		status     int
		http       any
	}{
		{"a 500", "", http.StatusInternalServerError, float64(500)},
		{"a 429", "", http.StatusTooManyRequests, float64(429)},
		{"a 302", "", http.StatusFound, float64(302)},
		{"not its shape", `{"data":[]}`, 0, float64(200)},
		{"no data", `{"error":"none"}`, 0, float64(200)},
		{"not JSON", `<html>`, 0, float64(200)},
		{"a body past the cap", `{"data":{"id":"x","name":"` + strings.Repeat("x", 8<<20) + `"}}`, 0, float64(200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st.set(llamaPath, tc.status, tc.body)
			e := wantRefused(t, h.orAdmin(llamaQuery), 503, CodeUnavailable, ReasonOpenRouterUnavailable)
			if e.Details["http_status"] != tc.http {
				t.Errorf("http_status %v, want %v", e.Details["http_status"], tc.http)
			}
		})
	}
}

// An administrator alone reads the route; its query is the model alone,
// OpenRouter's id of it, author/slug.
func TestOpenRouterEndpointsRefuses(t *testing.T) {
	h, st := newOpenRouterWorld(t)
	for _, role := range []string{"", "instructor"} {
		a := h.adminCall(t, role, "GET", "admin/openrouter/endpoints"+llamaQuery, "")
		wantRefused(t, a, 403, CodeForbidden, ReasonNotAdmin)
	}
	for _, tc := range []struct{ query, reason, field string }{
		{"", ReasonMissingField, "model"},
		{"?model=", ReasonMissingField, "model"},
		{"?model=llama-3.3", ReasonInvalidField, "model"},
		{"?model=a/b/c", ReasonInvalidField, "model"},
		{"?model=/b", ReasonInvalidField, "model"},
		{"?model=a%20b/c", ReasonInvalidField, "model"},
		{"?model=a/b&model=a/c", ReasonInvalidField, "model"},
		{llamaQuery + "&key=x", ReasonUnknownParameter, "key"},
	} {
		e := wantRefused(t, h.orAdmin(tc.query), 400, CodeInvalidArgument, tc.reason)
		if e.Details["field"] != tc.field {
			t.Errorf("%s: field %v", tc.query, e.Details["field"])
		}
		if tc.reason == ReasonInvalidField && tc.query != "?model=a/b&model=a/c" && e.Message != "model is OpenRouter's model ID, author/slug" {
			t.Errorf("%s: %s", tc.query, e.Message)
		}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.seen) != 0 {
		t.Errorf("a refused request asked OpenRouter: %d calls", len(st.seen))
	}
}
