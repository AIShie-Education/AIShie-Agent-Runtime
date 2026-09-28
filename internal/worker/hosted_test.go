package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
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
// and settings, as the API would: its token and the model key sealed, and
// its owner in Core recorded as the person who connected it.
func (h *hosting) host(id string, ag agent, token string, settings json.RawMessage) {
	h.w.t.Helper()
	h.hostAs(id, ag, ag.owner.ID, token, settings)
}

// hostAs is host, connected by the person owner.
func (h *hosting) hostAs(id string, ag agent, owner, token string, settings json.RawMessage) {
	h.w.t.Helper()
	if token == "" {
		token = ag.actor.Token
	}
	tok := h.seal("ten_"+id, store.SecretCoreToken, token)
	key := h.seal("ten_"+id, store.SecretModelKey, modelKey)
	_, err := h.st.CreateHostedAgent(context.Background(), store.HostedAgent{
		ID: id, CoreActorID: ag.actor.ID, OwnerActorID: owner, TenantID: "ten_" + id, DisplayName: "Hosted " + id,
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
	if !strings.Contains(failed.Detail, "not run: agent.colour: unknown field") || failed.Reason != store.ReasonSettingsRejected || failed.ConfigVersion != 1 {
		t.Errorf("the bad agent's detail: %q (%s, version %d)", failed.Detail, failed.Reason, failed.ConfigVersion)
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
	_, err = h.st.SetHostedAgentPaused(context.Background(), "agt_yuki", true, 0)
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
	_, err = h.st.SetHostedAgentPaused(context.Background(), "agt_yuki", false, 0)
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
	// What its owner reads says what the owner does: no file of theirs
	// holds the token.
	if st := wk.waitState("agt_yuki", store.AgentUnauthorized); !strings.Contains(st.Detail, "connect the agent again with a new token") ||
		strings.Contains(st.Detail, "token_ref") || st.Reason != store.ReasonTokenRefused {
		t.Errorf("the unauthorized hosted agent's detail: %q (%s)", st.Detail, st.Reason)
	}
	time.Sleep(50 * time.Millisecond)
	n := len(w.calls(own.actor.ID, ""))
	// Another agent's change: this one is not tried again.
	_, err := h.st.SetHostedAgentPaused(context.Background(), "agt_ken", true, 0)
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
			if !strings.Contains(st.Detail, `runs here as agent "yuki-helper", of the operator's configuration`) || st.Reason != store.ReasonOperatorAgent {
				t.Errorf("the hosted agent's detail: %q (%s)", st.Detail, st.Reason)
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
	if !strings.Contains(st.Detail, "its token is another Core actor's") || st.Reason != store.ReasonTokenOtherAgent {
		t.Errorf("detail %q (%s)", st.Detail, st.Reason)
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
	if !strings.Contains(st.Detail, "its token is not an agent's in Core") || st.Reason != store.ReasonTokenNotAgent {
		t.Errorf("detail %q (%s)", st.Detail, st.Reason)
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

// unownedAgent is an agent of Yuki's whose owner Core has since taken
// away, with the token issued to it after: me_get names no owner. It holds
// no seat, as Core takes an owner away only from an agent seated nowhere.
func (w *world) unownedAgent(id string) agent {
	w.t.Helper()
	a, err := w.fc.AddAgent("Yuki's old helper", w.students[0].ID)
	w.ok(err)
	w.ok(w.fc.SetOwner(a.ID, ""))
	a.Token, err = w.fc.IssueToken(a.ID)
	w.ok(err)
	w.env.Store(tokenVar(id), a.Token)
	return agent{id: id, actor: a, owner: w.students[0]}
}

// A hosted agent runs only while Core names as its owner the person who
// connected it. One Core names another owner for, or none, stops in state
// owner_changed, and one on a Core that does not say who owns an agent in
// state error; each having called me_get once and nothing else, with a
// state that names no one and holds no secret, and its row not marked
// verified. The YAML agent beside it runs whatever Core says of owners.
// One whose owner passes is marked verified in the registry, which
// restarts nothing.
func TestHostedOwnerCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before bool // a Core from before C1
		// connect hosts the agent and returns it, with who connected it.
		connect func(w *world, h *hosting) (agent, string)
		state   string
		reason  string
		detail  string
	}{
		{name: "Core names the person who connected it", connect: func(w *world, h *hosting) (agent, string) {
			own := w.ownAgent("agt_yuki", 0)
			h.host("agt_yuki", own, "", hostedSettings("m1"))
			return own, own.owner.ID
		}, state: store.AgentRunning},
		{name: "Core names the person who connected it, the row in capitals", connect: func(w *world, h *hosting) (agent, string) {
			own := w.ownAgent("agt_yuki", 0)
			h.hostAs("agt_yuki", own, strings.ToUpper(own.owner.ID), "", hostedSettings("m1"))
			return own, own.owner.ID
		}, state: store.AgentRunning},
		{name: "Core names another owner", connect: func(w *world, h *hosting) (agent, string) {
			own := w.ownAgent("agt_yuki", 0)
			h.hostAs("agt_yuki", own, w.students[1].ID, "", hostedSettings("m1"))
			return own, w.students[1].ID
		}, state: store.AgentOwnerChanged, reason: store.ReasonOwnerChanged, detail: ownerChangedDetail},
		{name: "Core names no owner", connect: func(w *world, h *hosting) (agent, string) {
			old := w.unownedAgent("agt_yuki")
			h.host("agt_yuki", old, "", hostedSettings("m1"))
			return old, old.owner.ID
		}, state: store.AgentOwnerChanged, reason: store.ReasonOwnerChanged, detail: ownerGoneDetail},
		{name: "a Core that does not say who owns an agent", before: true, connect: func(w *world, h *hosting) (agent, string) {
			own := w.ownAgent("agt_yuki", 0)
			h.host("agt_yuki", own, "", hostedSettings("m1"))
			return own, own.owner.ID
		}, state: store.AgentError, reason: store.ReasonCoreTooOld, detail: ownerUnknownDetail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorldWith(t, fakecore.Options{BeforeOwners: tc.before})
			h := w.hosting()
			ag, connectedBy := tc.connect(w, h)
			tu := w.tutor("cs101-tutor")
			yaml := w.config(nil, w.agentDoc("cs101-tutor", "m2", nil, nil))
			wk := h.start(h.build(yaml), models{"m1": scripted.New(scripted.Reply("Hosted.")), "m2": scripted.New(scripted.Reply("From YAML."))})

			st := wk.waitState("agt_yuki", tc.state)
			wk.waitState("cs101-tutor", store.AgentRunning)
			conv, _ := w.ask(1, tu, "Does the YAML agent answer?")
			w.waitAnswers(conv, 1)
			row, err := h.st.HostedAgent(context.Background(), "agt_yuki")
			w.ok(err)
			if tc.state == store.AgentRunning {
				if !row.OwnerVerified || row.Version != 2 {
					t.Errorf("the row of an agent whose owner passes: verified %v at version %d", row.OwnerVerified, row.Version)
				}
				// The registry's change, rebuilt, restarts nothing.
				wk.sup.Update(h.build(yaml))
				time.Sleep(200 * time.Millisecond)
				if n := len(w.calls(ag.actor.ID, "me_get")); n != 1 || !wk.statusOf("agt_yuki").Running {
					t.Errorf("after its owner was recorded verified: %d me_get, running %v", n, wk.statusOf("agt_yuki").Running)
				}
				return
			}
			if st.Detail != tc.detail || st.Reason != tc.reason {
				t.Errorf("the state says %q (%s), want %q (%s)", st.Detail, st.Reason, tc.detail, tc.reason)
			}
			for what, s := range map[string]string{"its token": ag.actor.Token, "the model key": modelKey, "its actor": ag.actor.ID,
				"who connected it": connectedBy, "its owner": ag.owner.ID, "the other student": w.students[1].ID, "a token's prefix": "ais_"} {
				if strings.Contains(st.Detail, s) {
					t.Errorf("the state holds %s: %q", what, st.Detail)
				}
			}
			if row.OwnerVerified {
				t.Error("the row of an agent whose owner does not pass is marked verified")
			}
			if s := wk.statusOf("agt_yuki"); !s.Hosted || s.Running || s.State != tc.state {
				t.Errorf("its status: %+v", s)
			}
			time.Sleep(100 * time.Millisecond)
			if n := len(w.calls(ag.actor.ID, "")); n != 1 || len(w.calls(ag.actor.ID, "me_get")) != 1 {
				t.Errorf("%d calls to Core with its token; want its one me_get", n)
			}
		})
	}
}

// A hosted agent stopped as owner_changed stays stopped through changes to
// the registry that are not its own, making no call, as an unauthorized
// one does; its owner connecting it again, a change of its own, starts it.
func TestHostedOwnerChangedWaitsForItsRow(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	other := w.ownAgent("agt_ken", 1)
	h := w.hosting()
	// Connected by Ken, who held its token without owning it.
	h.hostAs("agt_yuki", own, other.owner.ID, "", hostedSettings("m1"))
	h.host("agt_ken", other, "", hostedSettings("m1"))
	yaml := &config.Config{}
	wk := h.start(h.build(yaml), models{"m1": scripted.New(scripted.Reply("Back again."))})
	wk.waitState("agt_yuki", store.AgentOwnerChanged)
	wk.waitState("agt_ken", store.AgentRunning)

	_, err := h.st.SetHostedAgentPaused(context.Background(), "agt_ken", true, 0)
	w.ok(err)
	wk.sup.Update(h.build(yaml))
	wk.waitState("agt_ken", store.AgentPaused)
	time.Sleep(200 * time.Millisecond)
	if n := len(w.calls(own.actor.ID, "")); n != 1 {
		t.Errorf("%d calls with its token after a change to another agent; want its first me_get alone", n)
	}
	if st := wk.state("agt_yuki"); st.State != store.AgentOwnerChanged {
		t.Errorf("the agent is %s", st.State)
	}

	// Yuki, its owner, connects it again: her token, and her as its owner.
	token, err := w.fc.IssueToken(own.actor.ID)
	w.ok(err)
	a, err := h.st.HostedAgent(context.Background(), "agt_yuki")
	w.ok(err)
	tok := h.seal(a.TenantID, store.SecretCoreToken, token)
	a.TokenSecretID, a.OwnerActorID = tok.ID, own.owner.ID
	_, err = h.st.UpdateHostedAgent(context.Background(), *a, tok)
	w.ok(err)
	wk.sup.Update(h.build(yaml))
	wk.waitState("agt_yuki", store.AgentRunning)
	conv, _ := w.ask(0, own, "Are you back?")
	w.waitAnswers(conv, 1)
}

// A hosted agent's model is its owner's choice, of any text: its calls are
// counted under the name the price table gives it, and under "other" when
// the table does not price it, so that no owner's text becomes a metric's
// label. A YAML agent's model is counted under the name its operator
// wrote.
func TestHostedModelsLabels(t *testing.T) {
	w := newWorld(t)
	yuki, ken, tu := w.ownAgent("agt_yuki", 0), w.ownAgent("agt_ken", 1), w.tutor("cs101-tutor")
	h := w.hosting()
	h.host("agt_yuki", yuki, "", hostedSettings("m1"))
	h.host("agt_ken", ken, "", hostedSettings("m2"))
	prices, err := pricing.Parse([]byte(`version: "test"
prices:
  - {provider: openai, model: gpt-4.1-mini, from: 2020-01-01, usd_per_mtok: {input: 1, output: 1}}
  - {provider: openai, model: "o*", from: 2020-01-01, usd_per_mtok: {input: 1, output: 1}}
`))
	w.ok(err)
	const owners = `Ken's model {with} "quotes", and anything`
	ms := models{
		"m1": scripted.New(scripted.Reply("Priced.")).WithProvider("openai").WithModel("gpt-4.1-mini"),
		"m2": scripted.New(scripted.Reply("Not priced.")).WithProvider("openai").WithModel(owners),
		"m3": scripted.New(scripted.Reply("The operator's.")).WithProvider("vllm").WithModel("operator-model"),
	}
	wk := w.start(h.build(w.config(nil, w.agentDoc("cs101-tutor", "m3", nil, nil))), ms, workerOpts{store: h.st, prices: prices,
		edit: func(o *Options) {
			o.Secrets = secrets.Resolver{Getenv: w.getenv, Sealed: vault.Opener{Vault: h.v, Store: h.st}}
		}})
	for _, id := range []string{"agt_yuki", "agt_ken", "cs101-tutor"} {
		wk.waitState(id, store.AgentRunning)
	}
	c1, _ := w.ask(0, yuki, "A question for my helper.")
	c2, _ := w.ask(1, ken, "A question for mine.")
	c3, _ := w.ask(0, tu, "A question for the tutor.")
	for _, c := range []string{c1, c2, c3} {
		w.waitAnswers(c, 1)
	}
	for _, model := range []string{"gpt-4.1-mini", otherModel, "operator-model"} {
		if n := counter(t, wk.reg, "llm_calls_total", map[string]string{"model": model}); n < 1 {
			t.Errorf("no call counted under %q", model)
		}
	}
	for _, name := range []string{"llm_calls_total", "llm_tokens_total"} {
		if n := counter(t, wk.reg, name, map[string]string{"model": owners}); n != 0 {
			t.Errorf("%s is labelled with the owner's text", name)
		}
	}
}

// A new token put in force while the instance on the old one winds down:
// that instance meets Core's 401 (the new token revoked the old), and
// ends only after apply has put the new configuration in force. How it
// ended is not the new token's: nothing is written for it, nothing is
// blocked, and the agent runs on its new token, its state at the row's
// new version.
func TestNewTokenWhileTheOldWindsDown(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	h := w.hosting()
	h.host("agt_yuki", own, "", hostedSettings("m1"))
	yaml := &config.Config{}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow := func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		once.Do(func() { close(started) })
		<-release // an answer in progress, which the old instance waits for
		return scripted.Reply("Late, but here.")(ctx, req)
	}
	again := scripted.Reply("On the new token.")
	wk := h.start(h.build(yaml), models{"m1": scripted.New(slow, again, again, again)})
	wk.waitState("agt_yuki", store.AgentRunning)
	eventually(t, "the owner recorded as verified", func() bool {
		row, err := h.st.HostedAgent(context.Background(), "agt_yuki")
		return err == nil && row.Version == 2
	})
	w.ask(0, own, "Take your time.")
	<-started

	// PUT /token: the new token written, then the old one revoked with
	// it; the old instance's pollers meet the 401.
	token, err := w.fc.IssueToken(own.actor.ID)
	w.ok(err)
	row, err := h.st.HostedAgent(context.Background(), "agt_yuki")
	w.ok(err)
	tok := h.seal(row.TenantID, store.SecretCoreToken, token)
	row.TokenSecretID = tok.ID
	updated, err := h.st.UpdateHostedAgent(context.Background(), *row, tok)
	w.ok(err)
	n := len(w.fc.Calls())
	w.ok(w.fc.Revoke(own.actor.Token))
	eventually(t, "the old token refused", func() bool {
		for _, c := range w.fc.Calls()[n:] {
			if c.ActorID == own.actor.ID && c.HTTPStatus == http.StatusUnauthorized {
				return true
			}
		}
		return false
	})
	// The worker puts the new row in force while the old instance waits
	// for its answer.
	wk.sup.Update(h.build(yaml))
	eventually(t, "the new row put in force, the old instance stopping", func() bool {
		wk.sup.mu.Lock()
		defer wk.sup.mu.Unlock()
		r := wk.sup.runners["agt_yuki"]
		return r != nil && r.stopping && hostedVersion(r.cfg) == updated.Version
	})
	close(release)
	st := wk.waitVersion("agt_yuki", store.AgentRunning, updated.Version)
	if st.Reason != "" {
		t.Errorf("running on the new token, with a reason: %+v", st)
	}
	conv, _ := w.ask(0, own, "Are you there, on the new token?")
	if got := w.waitAnswers(conv, 1); got[0].Body != "On the new token." {
		t.Errorf("the answer: %q", got[0].Body)
	}
}
