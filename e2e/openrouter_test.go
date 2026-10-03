package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/api"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// orKey is the key of the school's at OpenRouter the administrator gives
// an offer: no log, answer or row may hold it.
const orKey = "sk-or-v1-e2e-0123456789abcdefghijklmnop"

// providerOf is the provider member of a request's body, as sent, and
// whether it has one.
func providerOf(t *testing.T, req fakellm.ChatRequest) (string, bool) {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(req.Raw, &body); err != nil {
		t.Fatalf("a model request's body: %v", err)
	}
	p, ok := body["provider"]
	return string(p), ok
}

// openRouterRoutingOnTheWire is OpenRouter's upstream routing reaching its
// calls against the real Core: a YAML agent's model at OpenRouter sends
// its routing, canonical, as provider in each of its answer's calls; and
// an offer of OpenRouter's the school's administrator makes through the
// API, with routing, is kept and answered canonical, its key tried with
// none, and the hosted agent put on it sends the offer's routing in each
// of its calls, over the hosted-model client.
func openRouterRoutingOnTheWire(t *testing.T, w *world) {
	// A YAML agent: the course's tutor, its model at OpenRouter (here, the
	// scripted model, named so).
	m := newModel(t, fakellm.DefaultResponder)
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "tutor", seat: w.tutor, over: map[string]any{
		"model": map[string]any{"provider": "openrouter", "model": "e2e-or-yaml", "openrouter": map[string]any{
			"max_price": map[string]any{"prompt": "1.50"}, "only": []any{"groq", "deepinfra"}, "order": []any{"groq"}, "data_collection": "deny",
			"preferred_max_latency": map[string]any{"p50": 2},
		}},
	}}}})
	rt.waitPolling("tutor")
	conv, _ := w.ask(t, w.yuki, w.tutor.member, "Which upstream answers me?")
	if ans := w.waitAnswer(t, w.yuki, conv, w.tutor.member); !strings.HasPrefix(ans.text(), "Answer: ") {
		t.Fatalf("the YAML agent's answer: %q", ans.text())
	}
	const yamlRouting = `{"order":["groq"],"data_collection":"deny","only":["groq","deepinfra"],"preferred_max_latency":2,"max_price":{"prompt":"1.5"}}`
	calls := 0
	for _, req := range m.Requests() {
		if req.Model != "e2e-or-yaml" {
			continue
		}
		calls++
		if p, ok := providerOf(t, req); !ok || p != yamlRouting {
			t.Errorf("a call of the YAML agent's sent provider %s, want %s", p, yamlRouting)
		}
	}
	if calls == 0 {
		t.Fatal("the YAML agent made no call")
	}

	// An offer made through the API.
	audience := os.Getenv("E2E_RUNTIME_AUDIENCE")
	if audience == "" {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); ci {
			t.Fatal("E2E_RUNTIME_AUDIENCE is not set, and CI is: start Core with scripts/ci-core.sh")
		}
		t.Skip("E2E_RUNTIME_AUDIENCE is not set: this Core makes no assertion for a runtime (scripts/ci-core.sh sets it)")
	}
	st, dbURL := runtimeStore(t)
	v, kek := keyring(t)
	w.addSecret("the key that seals the OpenRouter scenario's runtime's secrets", kek)
	w.addSecret("the key of the school's at OpenRouter", orKey)
	hm := newModel(t, fakellm.DefaultResponder)
	yaml := &config.Config{Runtime: config.Runtime{
		Defaults: map[string]any{"polling": polling(), "budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 60}}},
	}}
	hosted := w.startHosted(t, hm, st, v, yaml)
	target, err := url.Parse(hm.URL())
	if err != nil {
		t.Fatal(err)
	}
	keyTrials := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(keyTrials.CloseIdleConnections)
	a := w.startAPI(t, st, audience, func(o *api.Options) {
		o.Vault, o.Actors, o.Hosting = v, hosted.sup, staticHosting{yaml: yaml}
		// The offer's key is tried at OpenRouter's own endpoint, which is
		// the scripted model here, as the worker's hosted-model client has it.
		o.ModelHTTP = modelClient(keyTrials, target)
	})
	call := func(as, method, path, body string, want int, v any, headers ...string) {
		t.Helper()
		code, _, raw := a.do(t, method, "/runtime/api/v1/"+path, w.assertion(t, as, "", audience), body, headers...)
		if code != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, code, want, raw)
		}
		if v != nil {
			if err := json.Unmarshal(raw, v); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
		}
	}
	const siteRouting = `{"order":["deepinfra/turbo"],"allow_fallbacks":false,"require_parameters":true,"data_collection":"deny",` +
		`"only":["deepinfra","groq"],"quantizations":["fp8"],"max_price":{"prompt":"0.8","completion":"0"}}`
	made, _ := json.Marshal(map[string]any{"id": "llama", "label": "School AI (Llama)", "provider": "openrouter", "model": "e2e-or-site",
		"key": orKey, "openrouter": map[string]any{"data_collection": "deny", "require_parameters": true, "allow_fallbacks": false,
			"only": []any{"deepinfra", "groq"}, "order": []any{"deepinfra/turbo"}, "quantizations": []any{"fp8"}, "ignore": []any{},
			"max_price": map[string]any{"prompt": 0.80, "completion": "0.000"}}})
	var offer api.PlanOffer
	call(w.admin.token, "POST", "admin/school-plan/offers", string(made), http.StatusCreated, &offer)
	if got := string(offer.OpenRouter.JSON()); got != siteRouting || offer.KeyStatus == nil || *offer.KeyStatus != api.KeyTested {
		t.Fatalf("the offer made: %s, %+v", got, offer)
	}
	trials := hm.Requests()
	if len(trials) != 1 || trials[0].Model != "e2e-or-site" || trials[0].Header.Get("Authorization") != "Bearer "+orKey {
		t.Fatalf("the offer's key tried: %d calls", len(trials))
	}
	if p, ok := providerOf(t, trials[0]); ok {
		t.Errorf("the key's trial sent routing: %s", p)
	}

	// Yuki hosts her agent by its id and puts it on the offer: each of its
	// calls carries the offer's routing.
	host, _ := json.Marshal(map[string]string{"agent_id": w.own.id})
	var agent api.HostedAgent
	call(w.yuki.token, "POST", "agents", string(host), http.StatusCreated, &agent)
	id := agent.ID
	call(w.yuki.token, "PATCH", "agents/"+id, `{"model":{"school":{"offer":"llama"}}}`, 200, &agent, "If-Match", `"1"`)
	eventually(t, answerWait, "the hosted agent running on the offer", func() bool {
		code, _, raw := a.do(t, "GET", "/runtime/api/v1/agents/"+id, w.assertion(t, w.yuki.token, "", audience), "")
		var got api.HostedAgent
		return code == 200 && json.Unmarshal(raw, &got) == nil && got.Status == api.StatusRunning && got.Version == 3
	})
	const q = "Does the school's Llama answer me?"
	conv2, _ := w.ask(t, w.yuki, w.own.member, q)
	if ans := w.waitAnswer(t, w.yuki, conv2, w.own.member); !strings.HasPrefix(ans.text(), "Answer: "+q) {
		t.Fatalf("the offer's answer: %q", ans.text())
	}
	answered := 0
	for _, req := range hm.Requests()[1:] {
		if req.Model != "e2e-or-site" {
			continue
		}
		answered++
		if p, ok := providerOf(t, req); !ok || p != siteRouting || req.Header.Get("Authorization") != "Bearer "+orKey {
			t.Errorf("a call of the hosted agent's sent provider %s, want %s", p, siteRouting)
		}
	}
	if answered == 0 {
		t.Fatal("the hosted agent made no call on the offer")
	}
	if strings.Contains(dumpDatabase(t, dbURL), orKey) {
		t.Error("the runtime's database holds the school's key")
	}
}
