package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
)

// check --live tries a hosted agent's model as run calls it, over the
// hosted-model client: a model at an address that is not a public one, or
// a redirect to one, never gets the owner's key. A YAML agent's model is
// its operator's, called over the egress client wherever it is.
func TestCheckLiveGuardsHostedModels(t *testing.T) {
	const key = "sk-ant-owner-key-0123456789abcdef"
	var mu sync.Mutex
	var seen []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Api-Key"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],`+
			`"model":"claude-x","stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer model.Close()
	// localhost, another host than the model's 127.0.0.1, as a redirect
	// elsewhere would be.
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost"+strings.TrimPrefix(model.URL, "http://127.0.0.1")+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	env, err := config.FromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	clients, err := newLiveClients(env)
	if err != nil {
		t.Fatal(err)
	}
	res := secrets.Resolver{Getenv: func(string) string { return key }}
	var out strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&out, f+"\n", a...) }
	try := func(a *config.Agent, base string) bool {
		m := config.Model{Adapter: "anthropic", Model: "claude-x", BaseURL: base, KeyRef: "env://KEY", KeySource: config.KeyOwn,
			Params: config.ModelParams{MaxOutputTokens: 100}}
		return tryModel(context.Background(), p, a, m, res, clients.model(a, m))
	}
	hosted := &config.Agent{ID: "agt_x", Hosted: &config.Hosted{CoreActorID: "x"}}
	for _, base := range []string{model.URL, redirect.URL} {
		if try(hosted, base) {
			t.Errorf("a hosted agent's model at %s passed:\n%s", base, out.String())
		}
	}
	mu.Lock()
	if len(seen) != 0 {
		t.Errorf("a hosted agent's model was called at a loopback address, key %v", seen)
	}
	mu.Unlock()
	if !try(&config.Agent{ID: "yaml-agent"}, model.URL) {
		t.Errorf("a YAML agent's model at its operator's address:\n%s", out.String())
	}
	// An offer of the school's plan is the operator's, at the address
	// the operator chose, whatever agent is on it.
	offer := config.Model{Adapter: "anthropic", Model: "claude-x", BaseURL: model.URL, KeyRef: "env://KEY", KeySource: config.KeySchool,
		Offer: "standard", Params: config.ModelParams{MaxOutputTokens: 100}}
	if !tryModel(context.Background(), p, hosted, offer, res, clients.model(hosted, offer)) {
		t.Errorf("the school's offer for a hosted agent:\n%s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != key {
		t.Errorf("the operator's models were called %d times", len(seen))
	}
}
