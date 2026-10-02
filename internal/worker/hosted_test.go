package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// hosting is a world's registry of hosted agents: a store holding them, a
// vault sealing their tokens and keys, and the worker's options to seal
// and open them.
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

// host hosts ag as the hosted agent id by its id, as the API does: its row
// names the agent in Core and its owner, holds the owner's key sealed, and
// no token, which the worker is issued.
func (h *hosting) host(id string, ag agent, settings json.RawMessage) {
	h.w.t.Helper()
	h.hostAs(id, ag, ag.owner.ID, settings)
}

// hostAs is host, hosted by the person owner.
func (h *hosting) hostAs(id string, ag agent, owner string, settings json.RawMessage) {
	h.w.t.Helper()
	key := h.seal("ten_"+id, store.SecretModelKey, modelKey)
	_, err := h.st.CreateHostedAgent(context.Background(), store.HostedAgent{
		ID: id, CoreActorID: ag.actor.ID, OwnerActorID: owner, OwnerVerified: true, TenantID: "ten_" + id, DisplayName: "Hosted " + id,
		KeySecretID: key.ID, Settings: settings,
	}, key)
	h.w.ok(err)
}

// hostPasted hosts ag as it was hosted before hosting by id: its row holds
// the token its owner pasted, sealed, not issued to the runtime; Core's
// migration took it as the runtime's (ag.actor.Token, which AddAgent
// issued as the runtime's).
func (h *hosting) hostPasted(id string, ag agent, settings json.RawMessage) {
	h.w.t.Helper()
	tok := h.seal("ten_"+id, store.SecretCoreToken, ag.actor.Token)
	key := h.seal("ten_"+id, store.SecretModelKey, modelKey)
	_, err := h.st.CreateHostedAgent(context.Background(), store.HostedAgent{
		ID: id, CoreActorID: ag.actor.ID, OwnerActorID: ag.owner.ID, OwnerVerified: false, TenantID: "ten_" + id, DisplayName: "Hosted " + id,
		TokenSecretID: tok.ID, TokenHint: tok.Hint, KeySecretID: key.ID, Settings: settings,
	}, tok, key)
	h.w.ok(err)
}

// row is the hosted agent's row as the store holds it.
func (h *hosting) row(id string) *store.HostedAgent {
	h.w.t.Helper()
	r, err := h.st.HostedAgent(context.Background(), id)
	h.w.ok(err)
	return r
}

// build is YAML ∪ registry, as run builds it.
func (h *hosting) build(yaml *config.Config) *config.Config {
	h.w.t.Helper()
	cfg, _, err := registry.Build(context.Background(), yaml, h.st, registry.Options{CoreBaseURL: h.w.srv.URL})
	h.w.ok(err)
	return cfg
}

// start runs a worker on the registry's store, sealing the tokens it is
// issued and opening its sealed secrets.
func (h *hosting) start(cfg *config.Config, ms models) *worker {
	return h.startAs("w1", cfg, ms)
}

// startAs is start, of the worker id.
func (h *hosting) startAs(id string, cfg *config.Config, ms models) *worker {
	return h.w.start(cfg, ms, workerOpts{id: id, store: h.st, edit: h.options})
}

// options has a worker seal the tokens it is issued, and open its sealed
// secrets, in the registry's store.
func (h *hosting) options(o *Options) {
	o.Secrets = secrets.Resolver{Getenv: h.w.getenv, Sealed: vault.Opener{Vault: h.v, Store: h.st}}
	o.Sealer = h.v
}

// update puts the registry in force in wk, as the watcher does.
func (h *hosting) update(wk *worker, yaml *config.Config) { wk.sup.Update(h.build(yaml)) }

// statusOf is the agent's status as the supervisor reports it.
func (wk *worker) statusOf(id string) AgentStatus {
	for _, st := range wk.sup.Status() {
		if st.AgentID == id {
			return st
		}
	}
	return AgentStatus{}
}

// wantIssued fails unless the hosted agent's row holds the token Core holds
// live for its agent as the runtime's, issued to it.
func (h *hosting) wantIssued(id string, ag agent) {
	h.w.t.Helper()
	row := h.row(id)
	live := h.w.fc.RuntimeToken(ag.actor.ID)
	if !row.TokenIssued || row.TokenSecretID == "" || row.TokenCredentialID != live.CredentialID {
		h.w.t.Fatalf("the row of %s: %+v; Core's live token %s", id, row, live.CredentialID)
	}
	tok, err := vault.Opener{Vault: h.v, Store: h.st}.OpenSecret(context.Background(), row.TokenSecretID)
	h.w.ok(err)
	if tok != live.Token {
		h.w.t.Fatalf("the token sealed in %s's row is not Core's live one", id)
	}
}

// Hosted agents run beside the YAML ones, hosted by their ids: the worker
// is issued each one's token as it starts it, and seals it in its row. One
// whose settings do not pass is shown in state error, is issued nothing
// and makes no call, and keeps none of the others from running; pausing
// one drops its token from its row (the API revokes it in Core) and stops
// its calls to Core, and resuming it has it issued another.
func TestHostedAgentsRunBesideYAML(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	tu := w.tutor("cs101-tutor")
	bad := w.ownAgent("agt_bad", 1)
	h := w.hosting()
	h.host("agt_yuki", own, hostedSettings("m1"))
	settings := map[string]any{}
	w.ok(json.Unmarshal(hostedSettings("m1"), &settings))
	settings["colour"] = "blue"
	badSettings, err := json.Marshal(settings)
	w.ok(err)
	h.host("agt_bad", bad, badSettings)
	yaml := w.config(nil, w.agentDoc("cs101-tutor", "m2", nil, nil))
	cfg := h.build(yaml)
	if len(cfg.Agents) != 2 || len(cfg.Rejected) != 1 {
		t.Fatalf("agents %d, rejected %+v", len(cfg.Agents), cfg.Rejected)
	}
	// The agents' calls are counted as they begin them, on their
	// connections to Core.
	var began, yamlBegan atomic.Int32
	wk := w.start(cfg, models{"m1": scripted.New(scripted.Reply("From the registry.")), "m2": scripted.New(scripted.Reply("From YAML."))},
		workerOpts{id: "w1", store: h.st, edit: func(o *Options) {
			h.options(o)
			countCalls("agt_yuki", &began)(o)
			countCalls("cs101-tutor", &yamlBegan)(o)
		}})

	wk.waitState("agt_yuki", store.AgentRunning)
	wk.waitState("cs101-tutor", store.AgentRunning)
	h.wantIssued("agt_yuki", own)
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
	if n := len(w.calls(bad.actor.ID, "")); n != 0 || w.fc.RuntimeIssues(bad.actor.ID) != 1 {
		t.Errorf("the agent that does not pass made %d calls to Core, and was issued %d tokens", n, w.fc.RuntimeIssues(bad.actor.ID)-1)
	}

	// Paused: its token dropped from its row and revoked (the API's), and
	// no more calls to Core.
	paused, err := h.st.SetHostedAgentPaused(context.Background(), "agt_yuki", true, 0)
	w.ok(err)
	if paused.TokenSecretID != "" {
		t.Fatalf("a paused row holds a token: %+v", paused)
	}
	_, err = w.fc.RevokeRuntimeToken(own.actor.ID)
	w.ok(err)
	h.update(wk, yaml)
	wk.waitState("agt_yuki", store.AgentPaused)
	// Not running is its pollers and its answers returned: whatever it
	// calls after this, it began after. The calls are those it begins, not
	// those the fake Core logs, which logs a poll the pause cancelled as it
	// ends, on a busy machine after the agent has stopped; and "no more" is
	// over ten calls the YAML agent, still running, begins meanwhile, not
	// over a time, in which a busy machine would poll less.
	eventually(t, "the hosted agent stopped", func() bool { return !wk.statusOf("agt_yuki").Running })
	n, other := began.Load(), yamlBegan.Load()
	if n == 0 {
		t.Fatal("none of the hosted agent's calls was counted")
	}
	eventually(t, "ten calls more by the YAML agent", func() bool { return yamlBegan.Load() >= other+10 })
	if more := began.Load() - n; more != 0 {
		t.Errorf("%d calls to Core begun by the paused agent", more)
	}
	if w.fc.SiteChat(own.actor.ID) {
		t.Error("a paused agent is asked in the site")
	}
	// Resumed: issued another, and it runs again.
	_, err = h.st.SetHostedAgentPaused(context.Background(), "agt_yuki", false, 0)
	w.ok(err)
	h.update(wk, yaml)
	wk.waitState("agt_yuki", store.AgentRunning)
	h.wantIssued("agt_yuki", own)
	if n := w.fc.RuntimeIssues(own.actor.ID); n != 3 {
		t.Errorf("the hosted agent was issued %d tokens; want AddAgent's, its first and its resumed", n)
	}

	// Fixed, the bad one runs too.
	a := h.row("agt_bad")
	a.Settings = hostedSettings("m1")
	_, err = h.st.UpdateHostedAgent(context.Background(), *a)
	w.ok(err)
	h.update(wk, yaml)
	wk.waitState("agt_bad", store.AgentRunning)
}

// One token per agent, whichever worker runs it: the worker holding the
// agent's lease is issued it once and seals it in the row, and the worker
// that takes the agent up after it runs it with the same token, issued
// nothing. The registry's rebuild after the token is written restarts
// nothing.
func TestHostedOneTokenAcrossWorkers(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	h := w.hosting()
	h.host("agt_yuki", own, hostedSettings("m1"))
	yaml := &config.Config{}
	ms := models{"m1": scripted.New(scripted.Reply("Hello."), scripted.Reply("Hello again."))}
	w1 := h.startAs("w1", h.build(yaml), ms)
	w1.waitState("agt_yuki", store.AgentRunning)
	w2 := h.startAs("w2", h.build(yaml), ms)
	h.wantIssued("agt_yuki", own)
	issued := h.row("agt_yuki")
	// The rebuild the write set off, in both workers.
	h.update(w1, yaml)
	h.update(w2, yaml)
	time.Sleep(200 * time.Millisecond)
	if n := w.fc.RuntimeIssues(own.actor.ID); n != 2 {
		t.Fatalf("issued %d tokens; want AddAgent's and the runtime's", n)
	}
	if n := len(w.calls(own.actor.ID, "me_get")); n != 1 {
		t.Errorf("me_get %d times: the rebuild restarted the agent", n)
	}
	conv, _ := w.ask(0, own, "Who answers?")
	w.waitAnswers(conv, 1)

	w1.stop()
	w2.waitState("agt_yuki", store.AgentRunning)
	eventually(t, "the second worker running the agent", func() bool { return w2.statusOf("agt_yuki").Running })
	if n := w.fc.RuntimeIssues(own.actor.ID); n != 2 || h.row("agt_yuki").TokenSecretID != issued.TokenSecretID {
		t.Errorf("after the takeover: %d tokens issued, the row's %q", n, h.row("agt_yuki").TokenSecretID)
	}
	conv2, _ := w.ask(0, own, "And now?")
	w.waitAnswers(conv2, 1)
}

// Upgrading: a hosted agent whose token its owner pasted, before hosting
// was by id, is issued one in its place at its first start, which revokes
// the pasted one (Core's migration took it as the runtime's), and its row
// says so; the service's calls are paced by the worker's bucket. A row of
// an mcp agent is not run, says why, and is issued nothing.
func TestHostedUpgradeReissuesPastedTokens(t *testing.T) {
	w := newWorld(t)
	h := w.hosting()
	var pasted []agent
	for i, id := range []string{"agt_a", "agt_b", "agt_c"} {
		ag := w.ownAgent(id, i%2)
		h.hostPasted(id, ag, hostedSettings("m1"))
		pasted = append(pasted, ag)
	}
	tools, err := w.fc.AddMCPAgent("Ken's tools", w.students[1].ID)
	w.ok(err)
	h.hostPasted("agt_mcp", agent{id: "agt_mcp", actor: tools, owner: w.students[1]}, hostedSettings("m1"))
	bucket := ratelimit.New(6000, 1)
	wk := w.start(h.build(&config.Config{}), models{"m1": scripted.New(scripted.Reply("Upgraded."))}, workerOpts{store: h.st,
		edit: func(o *Options) {
			o.Secrets = secrets.Resolver{Getenv: w.getenv, Sealed: vault.Opener{Vault: h.v, Store: h.st}}
			o.Sealer, o.RuntimeBucket = h.v, bucket
		}})
	for i, ag := range pasted {
		id := []string{"agt_a", "agt_b", "agt_c"}[i]
		wk.waitState(id, store.AgentRunning)
		h.wantIssued(id, ag)
		if row := h.row(id); !row.OwnerVerified {
			t.Errorf("%s's owner, checked by its id, is not recorded verified", id)
		}
		if a, err := newCaller(t, w, ag.actor.Token).Me(context.Background()); err == nil {
			t.Errorf("the token %s's owner pasted still works: %+v", id, a)
		}
		conv, _ := w.ask(i%2, ag, "Upgraded?")
		w.waitAnswers(conv, 1)
	}
	st := wk.waitState("agt_mcp", store.AgentError)
	if st.Reason != store.ReasonMCPAgent || !strings.Contains(st.Detail, "mcp agent") {
		t.Errorf("the mcp agent's state: %+v", st)
	}
	if n := w.fc.RuntimeIssues(tools.ID); n != 0 || len(w.calls(tools.ID, "")) != 0 {
		t.Errorf("the mcp agent was issued %d runtime tokens and made %d calls", n, len(w.calls(tools.ID, "")))
	}
	if stats := bucket.Stats(); stats.Granted < 7 || stats.Waited == 0 {
		t.Errorf("the service's calls were not paced by the bucket: %+v", stats)
	}
}

// A hosted agent is run only while Core names as its owner the person who
// hosted it: one Core names another owner for is stopped in state
// owner_changed, its token revoked in Core and dropped from its row, with
// a state that names no one and holds no secret, having made no call as
// the agent; one whose owner of record is written in another case runs,
// and is recorded verified, which restarts nothing.
func TestHostedOwnerCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner func(w *world, ag agent) string
		state string
	}{
		{"Core names the person who hosted it", func(_ *world, ag agent) string { return ag.owner.ID }, store.AgentRunning},
		{"the row in capitals", func(_ *world, ag agent) string { return strings.ToUpper(ag.owner.ID) }, store.AgentRunning},
		{"Core names another owner", func(w *world, _ agent) string { return w.students[1].ID }, store.AgentOwnerChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			h := w.hosting()
			own := w.ownAgent("agt_yuki", 0)
			hostedBy := tc.owner(w, own)
			h.hostAs("agt_yuki", own, hostedBy, hostedSettings("m1"))
			row := h.row("agt_yuki")
			row.OwnerVerified = false
			_, err := h.st.UpdateHostedAgent(context.Background(), *row)
			w.ok(err)
			tu := w.tutor("cs101-tutor")
			yaml := w.config(nil, w.agentDoc("cs101-tutor", "m2", nil, nil))
			wk := h.start(h.build(yaml), models{"m1": scripted.New(scripted.Reply("Hosted.")), "m2": scripted.New(scripted.Reply("From YAML."))})

			st := wk.waitState("agt_yuki", tc.state)
			wk.waitState("cs101-tutor", store.AgentRunning)
			conv, _ := w.ask(1, tu, "Does the YAML agent answer?")
			w.waitAnswers(conv, 1)
			if tc.state == store.AgentRunning {
				eventually(t, "the owner recorded verified", func() bool { return h.row("agt_yuki").OwnerVerified })
				h.update(wk, yaml)
				time.Sleep(200 * time.Millisecond)
				if n := len(w.calls(own.actor.ID, "me_get")); n != 1 || !wk.statusOf("agt_yuki").Running {
					t.Errorf("after its owner was recorded verified: %d me_get, running %v", n, wk.statusOf("agt_yuki").Running)
				}
				return
			}
			if st.Detail != ownerChangedDetail || st.Reason != store.ReasonOwnerChanged {
				t.Errorf("the state says %q (%s)", st.Detail, st.Reason)
			}
			for what, s := range map[string]string{"its token": own.actor.Token, "the model key": modelKey, "its actor": own.actor.ID,
				"who hosted it": hostedBy, "its owner": own.owner.ID, "a token's prefix": "ais_"} {
				if strings.Contains(st.Detail, s) {
					t.Errorf("the state holds %s: %q", what, st.Detail)
				}
			}
			if w.fc.SiteChat(own.actor.ID) || w.fc.RuntimeToken(own.actor.ID).Token != "" {
				t.Error("the agent whose owner changed is still asked in the site: its token was not revoked")
			}
			if row := h.row("agt_yuki"); row.OwnerVerified || row.TokenSecretID != "" {
				t.Errorf("its row: %+v", row)
			}
			if s := wk.statusOf("agt_yuki"); !s.Hosted || s.Running || s.State != tc.state {
				t.Errorf("its status: %+v", s)
			}
			if n := len(w.calls(own.actor.ID, "")); n != 0 {
				t.Errorf("%d calls to Core as the agent", n)
			}
		})
	}
}

// A hosted agent suspended in Core while it runs is stopped, its token
// revoked, and not asked in the site; reactivated, it is issued another
// and answers again, by itself.
func TestHostedSuspendedAndBack(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	h := w.hosting()
	h.host("agt_yuki", own, hostedSettings("m1"))
	wk := h.start(h.build(&config.Config{}), models{"m1": scripted.New(scripted.Reply("Back."))})
	wk.waitState("agt_yuki", store.AgentRunning)
	w.ok(w.fc.SuspendActor(own.actor.ID))
	eventually(t, "the agent stopped for its suspension", func() bool {
		st := wk.state("agt_yuki")
		return st.State == store.AgentError && st.Reason == store.ReasonAgentSuspended
	})
	// Its state says so once a call of its own is refused; its next start
	// ends its hosting: its token revoked in Core, then forgotten in its
	// row.
	eventually(t, "its token revoked, and forgotten", func() bool {
		return w.fc.RuntimeToken(own.actor.ID).Token == "" && h.row("agt_yuki").TokenSecretID == ""
	})
	if w.fc.SiteChat(own.actor.ID) {
		t.Error("a suspended agent is still asked in the site")
	}
	w.ok(w.fc.ReactivateActor(own.actor.ID))
	wk.waitState("agt_yuki", store.AgentRunning)
	h.wantIssued("agt_yuki", own)
	conv, _ := w.ask(0, own, "Are you back?")
	w.waitAnswers(conv, 1)
}

// A hosted agent whose owner is suspended in Core is not started: it is in
// state error, reason owner_suspended, and makes no call as the agent; its
// token is kept (Core pauses the agent while its owner is). Its owner
// reactivated, it starts by itself with the token it holds, issued nothing.
func TestHostedOwnerSuspended(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	h := w.hosting()
	h.host("agt_yuki", own, hostedSettings("m1"))
	yaml := &config.Config{}
	wk := h.start(h.build(yaml), models{"m1": scripted.New(scripted.Reply("Back."))})
	wk.waitState("agt_yuki", store.AgentRunning)
	h.wantIssued("agt_yuki", own)
	issued := w.fc.RuntimeIssues(own.actor.ID)
	wk.stop()

	w.ok(w.fc.SuspendActor(own.owner.ID))
	wk = h.startAs("w2", h.build(yaml), models{"m1": scripted.New(scripted.Reply("Back."))})
	st := wk.waitState("agt_yuki", store.AgentError)
	if st.Reason != store.ReasonOwnerSuspended || !strings.Contains(st.Detail, "owner is suspended") {
		t.Errorf("its state: %q (%s)", st.Detail, st.Reason)
	}
	since := time.Now()
	time.Sleep(100 * time.Millisecond)
	for _, c := range w.fc.Calls() {
		if c.ActorID == own.actor.ID && c.At.After(since) {
			t.Fatalf("a call as the agent whose owner is suspended: %s", c.Tool)
		}
	}
	h.wantIssued("agt_yuki", own)
	w.ok(w.fc.ReactivateActor(own.owner.ID))
	wk.waitState("agt_yuki", store.AgentRunning)
	if n := w.fc.RuntimeIssues(own.actor.ID); n != issued {
		t.Errorf("issued %d tokens, %d before its owner's suspension", n, issued)
	}
	conv, _ := w.ask(0, own, "Are you back?")
	w.waitAnswers(conv, 1)
}

// A hosted agent whose token is revoked in Core by someone else than the
// runtime (its owner, an administrator) stops, unauthorized, and stays
// stopped through changes to the registry that are not its own, issued
// nothing; its owner asking for a new token (its row's dropped) has it
// issued another, and it runs again.
func TestHostedRevokedElsewhere(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	other := w.ownAgent("agt_ken", 1)
	h := w.hosting()
	h.host("agt_yuki", own, hostedSettings("m1"))
	h.host("agt_ken", other, hostedSettings("m1"))
	yaml := &config.Config{}
	wk := h.start(h.build(yaml), models{"m1": scripted.New(scripted.Reply("Back again."))})
	wk.waitState("agt_yuki", store.AgentRunning)
	wk.waitState("agt_ken", store.AgentRunning)

	_, err := w.fc.RevokeRuntimeToken(own.actor.ID)
	w.ok(err)
	st := wk.waitState("agt_yuki", store.AgentUnauthorized)
	if st.Reason != store.ReasonTokenRefused || !strings.Contains(st.Detail, "revoked in Core") || strings.Contains(st.Detail, "token_ref") {
		t.Errorf("the unauthorized hosted agent's state: %q (%s)", st.Detail, st.Reason)
	}
	time.Sleep(50 * time.Millisecond)
	n, issued := len(w.calls(own.actor.ID, "")), w.fc.RuntimeIssues(own.actor.ID)
	// Another agent's change: this one is not tried again.
	_, err = h.st.SetHostedAgentPaused(context.Background(), "agt_ken", true, 0)
	w.ok(err)
	h.update(wk, yaml)
	wk.waitState("agt_ken", store.AgentPaused)
	time.Sleep(200 * time.Millisecond)
	if more := len(w.calls(own.actor.ID, "")) - n; more != 0 || w.fc.RuntimeIssues(own.actor.ID) != issued {
		t.Errorf("%d calls, and %d tokens issued, for the unauthorized agent after a change to another", more,
			w.fc.RuntimeIssues(own.actor.ID)-issued)
	}

	// Its owner asks for a new token (POST …/token): the row's dropped.
	row := h.row("agt_yuki")
	old := row.TokenSecretID
	row.TokenSecretID, row.TokenHint, row.TokenIssued, row.TokenCredentialID = "", "", false, ""
	_, err = h.st.UpdateHostedAgent(context.Background(), *row)
	w.ok(err)
	if _, err := h.st.Secret(context.Background(), old); err == nil {
		t.Error("the old token's secret was kept")
	}
	h.update(wk, yaml)
	wk.waitState("agt_yuki", store.AgentRunning)
	h.wantIssued("agt_yuki", own)
	conv, _ := w.ask(0, own, "Are you back?")
	w.waitAnswers(conv, 1)
}

// One Core actor is one agent here, and the operator's wins: a hosted
// agent on a YAML agent's actor is not run (the registry refuses it), and
// is issued nothing, so that it never revokes the YAML agent's token; only
// the YAML agent answers. The YAML agent may name the same Core as
// CORE_BASE_URL in another spelling.
func TestYAMLWinsACoreActor(t *testing.T) {
	for _, spelling := range []string{"the same", "a / at the end of the YAML's Core", "the YAML's Core in capitals"} {
		t.Run(spelling, func(t *testing.T) {
			w := newWorld(t)
			own := w.ownAgent("yuki-helper", 0)
			h := w.hosting()
			h.host("agt_dup", own, hostedSettings("m1"))
			var over map[string]any
			switch spelling {
			case "a / at the end of the YAML's Core":
				over = map[string]any{"core": map[string]any{"base_url": w.srv.URL + "/"}}
			case "the YAML's Core in capitals":
				over = map[string]any{"core": map[string]any{"base_url": strings.Replace(w.srv.URL, "http://", "HTTP://", 1)}}
			}
			yaml := w.config(nil, w.agentDoc("yuki-helper", "m2", over, nil))
			ms := models{"m1": scripted.New(scripted.Reply("From the registry.")), "m2": scripted.New(scripted.Reply("From YAML."))}
			wk := h.start(h.build(yaml), ms)
			wk.waitState("yuki-helper", store.AgentRunning)
			st := wk.waitState("agt_dup", store.AgentError)
			if !strings.Contains(st.Detail, `YAML agent "yuki-helper" is this agent in Core`) || st.Reason != store.ReasonOperatorAgent {
				t.Errorf("the hosted agent's detail: %q (%s)", st.Detail, st.Reason)
			}
			conv, _ := w.ask(0, own, "Who answers?")
			w.waitAnswers(conv, 1)
			time.Sleep(200 * time.Millisecond)
			if got := w.answers(conv); len(got) != 1 || got[0].Body != "From YAML." {
				t.Errorf("answers: %+v", got)
			}
			if n := w.fc.RuntimeIssues(own.actor.ID); n != 2 {
				t.Errorf("%d tokens issued; want AddAgent's and the YAML agent's", n)
			}
		})
	}
}

// Two agents of the worker on one Core actor, the operator's and a hosted
// one put in force before the registry knew of the YAML one: the YAML
// agent stops the hosted one before it is issued its token.
func TestYAMLPreemptsAHostedAgentOnItsActor(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	h := w.hosting()
	h.host("agt_dup", own, hostedSettings("m1"))
	ms := models{"m1": scripted.New(scripted.Reply("From the registry.")), "m2": scripted.New(scripted.Reply("From YAML."))}
	wk := h.start(h.build(&config.Config{}), ms)
	wk.waitState("agt_dup", store.AgentRunning)
	cfg := h.build(&config.Config{})
	cfg.Agents = append(cfg.Agents, w.config(nil, w.agentDoc("yuki-helper", "m2", nil, nil)).Agents...)
	wk.sup.Reload(cfg)
	wk.waitState("yuki-helper", store.AgentRunning)
	st := wk.waitState("agt_dup", store.AgentError)
	if !strings.Contains(st.Detail, `runs here as agent "yuki-helper", of the operator's configuration`) || st.Reason != store.ReasonOperatorAgent {
		t.Errorf("the hosted agent's detail: %q (%s)", st.Detail, st.Reason)
	}
	conv, _ := w.ask(0, own, "Who answers?")
	if got := w.waitAnswers(conv, 1); got[0].Body != "From YAML." {
		t.Errorf("answers: %+v", got)
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

// A hosted agent's model is its owner's choice, of any text: its calls are
// counted under the name the price table gives it, and under "other" when
// the table does not price it, so that no owner's text becomes a metric's
// label. A YAML agent's model is counted under the name its operator
// wrote.
func TestHostedModelsLabels(t *testing.T) {
	w := newWorld(t)
	yuki, ken, tu := w.ownAgent("agt_yuki", 0), w.ownAgent("agt_ken", 1), w.tutor("cs101-tutor")
	h := w.hosting()
	h.host("agt_yuki", yuki, hostedSettings("m1"))
	h.host("agt_ken", ken, hostedSettings("m2"))
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
			o.Sealer = h.v
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

// A new token asked for while the instance on the old one winds down: the
// row's token dropped is a change, which stops the old instance, waiting
// for its answer in progress, and the new one is issued another once it
// has ended, which revokes the old. How the old one ended is not the new
// token's: nothing is written for it, nothing is blocked, and the agent
// runs on its new token, its state at the row's version.
func TestNewTokenWhileTheOldWindsDown(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	h := w.hosting()
	h.host("agt_yuki", own, hostedSettings("m1"))
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
	w.ask(0, own, "Take your time.")
	<-started

	// POST …/token: the row's token dropped.
	old := w.runtimeToken(own)
	row := h.row("agt_yuki")
	row.TokenSecretID, row.TokenHint, row.TokenIssued, row.TokenCredentialID = "", "", false, ""
	dropped, err := h.st.UpdateHostedAgent(context.Background(), *row)
	w.ok(err)
	h.update(wk, yaml)
	eventually(t, "the new row put in force, the old instance stopping", func() bool {
		wk.sup.mu.Lock()
		defer wk.sup.mu.Unlock()
		r := wk.sup.runners["agt_yuki"]
		return r != nil && r.stopping && hostedVersion(r.cfg) == dropped.Version
	})
	if n := w.fc.RuntimeIssues(own.actor.ID); n != 2 {
		t.Errorf("issued %d tokens before the old instance ended", n)
	}
	close(release)
	st := wk.waitVersion("agt_yuki", store.AgentRunning, dropped.Version+1)
	if st.Reason != "" {
		t.Errorf("running on the new token, with a reason: %+v", st)
	}
	h.wantIssued("agt_yuki", own)
	if _, err := newCaller(t, w, old).Me(context.Background()); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("the old token, after the new one was issued: %v", err)
	}
	conv, _ := w.ask(0, own, "Are you there, on the new token?")
	if got := w.waitAnswers(conv, 1); got[0].Body != "On the new token." {
		t.Errorf("the answer: %q", got[0].Body)
	}
}
