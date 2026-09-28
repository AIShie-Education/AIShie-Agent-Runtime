package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/vault"
)

// hosting is a world's registry of hosted agents: a store holding them, a
// vault sealing their tokens and keys, and the worker's options to open
// them.
type hosting struct {
	w  *world
	st *memstore.Store
	v  *vault.Vault
	n  int
}

func (w *world) hosting() *hosting {
	w.t.Helper()
	return &hosting{w: w, st: memstore.New(), v: testVault(w.t)}
}

// settings are a hosted agent's settings for model: the owner's key, and
// the tests' polling.
func hostedSettings(model string) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"model":   map[string]any{"adapter": "openai_chat", "model": model, "key_source": "own", "params": map[string]any{"max_output_tokens": 500}},
		"polling": fastPolling(),
		"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 10}},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// seal seals a secret of the tenant into the store's hands, returning it
// to be given with a write.
func (h *hosting) seal(tenant, kind, plaintext string) store.Secret {
	h.w.t.Helper()
	h.n++
	s, err := h.v.Seal(context.Background(), store.Secret{ID: vault.NewSecretID(), TenantID: tenant, Kind: kind}, plaintext)
	if err != nil {
		h.w.t.Fatal(err)
	}
	return s
}

// host connects ag as the hosted agent id, with token (ag's own when "")
// and settings, as the API would: its token and the model key sealed.
func (h *hosting) host(id string, ag agent, token string, settings json.RawMessage) {
	h.w.t.Helper()
	if token == "" {
		token = ag.actor.Token
	}
	tok := h.seal("ten_"+id, store.SecretCoreToken, token)
	key := h.seal("ten_"+id, store.SecretModelKey, modelKey)
	_, err := h.st.CreateHostedAgent(context.Background(), store.HostedAgent{
		ID: id, CoreActorID: ag.actor.ID, OwnerActorID: "owner", TenantID: "ten_" + id, DisplayName: "Hosted " + id,
		TokenSecretID: tok.ID, KeySecretID: key.ID, Settings: settings,
	}, tok, key)
	h.w.ok(err)
}

// build is YAML ∪ registry, as run builds it.
func (h *hosting) build(yaml *config.Config) *config.Config {
	h.w.t.Helper()
	cfg, _, err := registry.Build(context.Background(), yaml, h.st, registry.Options{CoreBaseURL: h.w.srv.URL})
	h.w.ok(err)
	return cfg
}

// start runs a worker on the registry's store, opening its sealed secrets.
func (h *hosting) start(cfg *config.Config, ms models) *worker {
	return h.w.start(cfg, ms, workerOpts{store: h.st, edit: func(o *Options) {
		o.Secrets = secrets.Resolver{Getenv: h.w.getenv, Sealed: vault.Opener{Vault: h.v, Store: h.st}}
	}})
}

// statusOf is the agent's status as the supervisor reports it.
func (wk *worker) statusOf(id string) AgentStatus {
	for _, st := range wk.sup.Status() {
		if st.AgentID == id {
			return st
		}
	}
	return AgentStatus{}
}

// Hosted agents run beside the YAML ones, from their sealed secrets. One
// whose settings do not pass is shown in state error and makes no call,
// and keeps none of the others from running; pausing one stops its calls
// to Core, and resuming it starts it again.
func TestHostedAgentsRunBesideYAML(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	tu := w.tutor("cs101-tutor")
	bad := w.ownAgent("agt_bad", 1)
	h := w.hosting()
	h.host("agt_yuki", own, "", hostedSettings("m1"))
	settings := map[string]any{}
	w.ok(json.Unmarshal(hostedSettings("m1"), &settings))
	settings["colour"] = "blue"
	badSettings, err := json.Marshal(settings)
	w.ok(err)
	h.host("agt_bad", bad, "", badSettings)
	yaml := w.config(nil, w.agentDoc("cs101-tutor", "m2", nil, nil))
	cfg := h.build(yaml)
	if len(cfg.Agents) != 2 || len(cfg.Rejected) != 1 {
		t.Fatalf("agents %d, rejected %+v", len(cfg.Agents), cfg.Rejected)
	}
	wk := h.start(cfg, models{"m1": scripted.New(scripted.Reply("From the registry.")), "m2": scripted.New(scripted.Reply("From YAML."))})

	wk.waitState("agt_yuki", store.AgentRunning)
	wk.waitState("cs101-tutor", store.AgentRunning)
	failed := wk.waitState("agt_bad", store.AgentError)
	if !strings.Contains(failed.Detail, "not run: agent.colour: unknown field") {
		t.Errorf("the bad agent's detail: %q", failed.Detail)
	}
	if st := wk.statusOf("agt_bad"); st.State != store.AgentError || !st.Hosted || st.Running {
		t.Errorf("the bad agent's status: %+v", st)
	}
	if st := wk.statusOf("agt_yuki"); !st.Hosted || !st.Running {
		t.Errorf("the hosted agent's status: %+v", st)
	}
	if st := wk.statusOf("cs101-tutor"); st.Hosted {
		t.Errorf("the YAML agent's status: %+v", st)
	}
	conv, _ := w.ask(0, own, "Hosted?")
	if got := w.waitAnswers(conv, 1); got[0].Body != "From the registry." {
		t.Errorf("the hosted agent's answer: %q", got[0].Body)
	}
	tconv, _ := w.ask(1, tu, "YAML?")
	if got := w.waitAnswers(tconv, 1); got[0].Body != "From YAML." {
		t.Errorf("the YAML agent's answer: %q", got[0].Body)
	}
	if n := len(w.calls(bad.actor.ID, "")); n != 0 {
		t.Errorf("the agent that does not pass made %d calls to Core", n)
	}

	// Paused: no more calls to Core.
	_, err = h.st.SetHostedAgentPaused(context.Background(), "agt_yuki", true)
	w.ok(err)
	wk.sup.Update(h.build(yaml))
	wk.waitState("agt_yuki", store.AgentPaused)
	eventually(t, "the hosted agent stopped", func() bool { return !wk.statusOf("agt_yuki").Running })
	// A call in flight as it stopped reaches the fake Core just after.
	time.Sleep(50 * time.Millisecond)
	n := len(w.calls(own.actor.ID, ""))
	time.Sleep(200 * time.Millisecond)
	if more := len(w.calls(own.actor.ID, "")) - n; more != 0 {
		t.Errorf("%d calls to Core by the paused agent", more)
	}
	// Resumed: it runs again.
	_, err = h.st.SetHostedAgentPaused(context.Background(), "agt_yuki", false)
	w.ok(err)
	wk.sup.Update(h.build(yaml))
	wk.waitState("agt_yuki", store.AgentRunning)

	// Fixed, the bad one runs too.
	a, err := h.st.HostedAgent(context.Background(), "agt_bad")
	w.ok(err)
	a.Settings = hostedSettings("m1")
	_, err = h.st.UpdateHostedAgent(context.Background(), *a)
	w.ok(err)
	wk.sup.Update(h.build(yaml))
	wk.waitState("agt_bad", store.AgentRunning)
}

// A hosted agent Core refuses stays unauthorized through changes to the
// registry that are not its own, making no call; a new token, which is a
// new secret, starts it again.
func TestHostedTokenChangeRestartsAnUnauthorizedAgent(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	other := w.ownAgent("agt_ken", 1)
	h := w.hosting()
	h.host("agt_yuki", own, "", hostedSettings("m1"))
	h.host("agt_ken", other, "", hostedSettings("m1"))
	yaml := &config.Config{}
	wk := h.start(h.build(yaml), models{"m1": scripted.New(scripted.Reply("Back again."))})
	wk.waitState("agt_yuki", store.AgentRunning)

	w.ok(w.fc.Revoke(own.actor.Token))
	wk.waitState("agt_yuki", store.AgentUnauthorized)
	time.Sleep(50 * time.Millisecond)
	n := len(w.calls(own.actor.ID, ""))
	// Another agent's change: this one is not tried again.
	_, err := h.st.SetHostedAgentPaused(context.Background(), "agt_ken", true)
	w.ok(err)
	wk.sup.Update(h.build(yaml))
	wk.waitState("agt_ken", store.AgentPaused)
	time.Sleep(200 * time.Millisecond)
	if more := len(w.calls(own.actor.ID, "")) - n; more != 0 {
		t.Errorf("%d calls by the unauthorized agent after a change to another", more)
	}
	if st := wk.state("agt_yuki"); st.State != store.AgentUnauthorized {
		t.Errorf("the unauthorized agent is %s", st.State)
	}

	// Its owner connects a new token: a new secret, the old one destroyed.
	token, err := w.fc.IssueToken(own.actor.ID)
	w.ok(err)
	a, err := h.st.HostedAgent(context.Background(), "agt_yuki")
	w.ok(err)
	old := a.TokenSecretID
	tok := h.seal(a.TenantID, store.SecretCoreToken, token)
	a.TokenSecretID = tok.ID
	_, err = h.st.UpdateHostedAgent(context.Background(), *a, tok)
	w.ok(err)
	if _, err := h.st.Secret(context.Background(), old); err == nil {
		t.Error("the old token's secret was kept")
	}
	wk.sup.Update(h.build(yaml))
	wk.waitState("agt_yuki", store.AgentRunning)
	conv, _ := w.ask(0, own, "Are you back?")
	w.waitAnswers(conv, 1)
}

// One Core actor is one agent here, and the operator's wins: a hosted
// agent on a YAML agent's actor is shown in state error and does not poll,
// whichever started first, and only the YAML agent answers. The YAML
// agent may name the same Core as CORE_BASE_URL in another spelling: with
// a / at the end, or its host in capitals.
func TestYAMLWinsACoreActor(t *testing.T) {
	for _, first := range []string{"both at once", "the hosted one first", "a / at the end of the YAML's Core", "the YAML's Core in capitals"} {
		t.Run(first, func(t *testing.T) {
			w := newWorld(t)
			own := w.ownAgent("yuki-helper", 0)
			h := w.hosting()
			// agt_ sorts before yuki-: the hosted agent is started first.
			h.host("agt_dup", own, "", hostedSettings("m1"))
			var over map[string]any
			switch first {
			case "a / at the end of the YAML's Core":
				over = map[string]any{"core": map[string]any{"base_url": w.srv.URL + "/"}}
			case "the YAML's Core in capitals":
				over = map[string]any{"core": map[string]any{"base_url": strings.Replace(w.srv.URL, "http://", "HTTP://", 1)}}
			}
			yamlDoc := w.agentDoc("yuki-helper", "m2", over, nil)
			yaml := w.config(nil, yamlDoc)
			ms := models{"m1": scripted.New(scripted.Reply("From the registry.")), "m2": scripted.New(scripted.Reply("From YAML."))}
			var wk *worker
			if first != "the hosted one first" {
				wk = h.start(h.build(yaml), ms)
			} else {
				wk = h.start(h.build(&config.Config{}), ms)
				wk.waitState("agt_dup", store.AgentRunning)
				wk.sup.Reload(h.build(yaml))
			}
			wk.waitState("yuki-helper", store.AgentRunning)
			st := wk.waitState("agt_dup", store.AgentError)
			if !strings.Contains(st.Detail, `runs here as agent "yuki-helper", of the operator's configuration`) {
				t.Errorf("the hosted agent's detail: %q", st.Detail)
			}
			eventually(t, "the hosted agent stopped", func() bool { return !wk.statusOf("agt_dup").Running })
			conv, _ := w.ask(0, own, "Who answers?")
			w.waitAnswers(conv, 1)
			time.Sleep(200 * time.Millisecond)
			if got := w.answers(conv); len(got) != 1 || got[0].Body != "From YAML." {
				t.Errorf("answers: %+v", got)
			}
		})
	}
}

// A hosted agent whose token is another Core actor's than its row names is
// not run: it would answer as someone else.
func TestHostedTokenOfAnotherActor(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	other := w.ownAgent("agt_ken", 1)
	h := w.hosting()
	h.host("agt_yuki", own, other.actor.Token, hostedSettings("m1"))
	wk := h.start(h.build(&config.Config{}), models{"m1": scripted.New()})
	st := wk.waitState("agt_yuki", store.AgentError)
	if !strings.Contains(st.Detail, "its token is another Core actor's") {
		t.Errorf("detail %q", st.Detail)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(w.calls(other.actor.ID, "conversation_inbox")); n != 0 {
		t.Errorf("%d inbox polls under the other actor's token", n)
	}
	if n := len(w.calls(other.actor.ID, "me_get")); n != 1 {
		t.Errorf("me_get was called %d times; the agent is not tried again until it changes", n)
	}
}

// A hosted agent whose row names a person, with the person's own token, is
// not run: the runtime would act in Core as the person, with every seat of
// theirs. The API refuses such a token as it connects one; the worker
// refuses it too.
func TestHostedTokenOfAPerson(t *testing.T) {
	w := newWorld(t)
	person := w.fc.AddPerson("Mallory")
	h := w.hosting()
	h.host("agt_person", agent{actor: person}, "", hostedSettings("m1"))
	wk := h.start(h.build(&config.Config{}), models{"m1": scripted.New()})
	st := wk.waitState("agt_person", store.AgentError)
	if !strings.Contains(st.Detail, "its token is not an agent's in Core") {
		t.Errorf("detail %q", st.Detail)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(w.calls(person.ID, "")); n != 1 {
		t.Errorf("%d calls to Core under the person's token; want its one me_get", n)
	}
}

// One Core is one Core in the actor claim, however its base URL is
// written; another host, port or path is another.
func TestActorKey(t *testing.T) {
	same := []string{"https://lms.example.edu", "https://lms.example.edu/", "HTTPS://LMS.Example.edu", "https://lms.example.edu:443/"}
	for _, u := range same {
		if actorKey(u, "a1") != actorKey(same[0], "a1") {
			t.Errorf("%s is another Core than %s", u, same[0])
		}
	}
	for _, u := range []string{"https://lms.example.edu:8443", "https://other.example.edu", "https://lms.example.edu/core", "http://lms.example.edu"} {
		if actorKey(u, "a1") == actorKey(same[0], "a1") {
			t.Errorf("%s is the same Core as %s", u, same[0])
		}
	}
	if actorKey(same[0], "a1") == actorKey(same[0], "a2") {
		t.Error("two actors are one")
	}
}
