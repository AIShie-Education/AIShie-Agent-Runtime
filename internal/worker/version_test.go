package worker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/netguard"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/vault"
)

// waitVersion waits for the agent's state to be state at the row's
// version.
func (wk *worker) waitVersion(id, state string, version int) store.AgentState {
	wk.w.t.Helper()
	eventually(wk.w.t, "agent "+id+" "+state+" at its version", func() bool {
		st := wk.state(id)
		return st.State == state && st.ConfigVersion == version
	})
	return wk.state(id)
}

// Every state a hosted agent's worker writes names the version of its row
// in force: at its start; when a write moves the row on and changes
// nothing in how it runs (its owner recorded verified), which restarts
// nothing and writes the state again; paused, and paused at a new
// version; resumed; and rejected, at a new version with the same problem.
// A YAML agent's is 0.
func TestStatesNameTheVersionInForce(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	bad := w.ownAgent("agt_bad", 1)
	tu := w.tutor("cs101-tutor")
	h := w.hosting()
	h.host("agt_yuki", own, "", hostedSettings("m1"))
	h.host("agt_bad", bad, "", []byte(`{"colour":"blue"}`))
	yaml := w.config(nil, w.agentDoc("cs101-tutor", "m2", nil, nil))
	wk := h.start(h.build(yaml), models{"m1": scripted.New(scripted.Reply("Hosted.")), "m2": scripted.New(scripted.Reply("YAML."))})
	ctx := context.Background()

	if st := wk.waitVersion("agt_yuki", store.AgentRunning, 1); st.Reason != "" {
		t.Errorf("running with a reason: %+v", st)
	}
	if st := wk.waitState("cs101-tutor", store.AgentRunning); st.ConfigVersion != 0 {
		t.Errorf("the YAML agent's state names version %d", st.ConfigVersion)
	}
	wk.waitVersion("agt_bad", store.AgentError, 1)
	// Its start recorded its owner verified: version 2, which changes
	// nothing in how it runs.
	eventually(t, "the owner recorded verified", func() bool {
		row, err := h.st.HostedAgent(ctx, "agt_yuki")
		return err == nil && row.Version == 2
	})
	wk.sup.Update(h.build(yaml))
	st := wk.waitVersion("agt_yuki", store.AgentRunning, 2)
	if n := len(w.calls(own.actor.ID, "me_get")); n != 1 {
		t.Errorf("a version-only change restarted the agent: %d me_get", n)
	}
	if st.Reason != "" {
		t.Errorf("after the version-only change: %+v", st)
	}

	// Paused (3), then written again while paused (4): paused, at each.
	_, err := h.st.SetHostedAgentPaused(ctx, "agt_yuki", true, 0)
	w.ok(err)
	wk.sup.Update(h.build(yaml))
	wk.waitVersion("agt_yuki", store.AgentPaused, 3)
	row, err := h.st.HostedAgent(ctx, "agt_yuki")
	w.ok(err)
	_, err = h.st.UpdateHostedAgent(ctx, *row)
	w.ok(err)
	wk.sup.Update(h.build(yaml))
	wk.waitVersion("agt_yuki", store.AgentPaused, 4)
	// Resumed (5): running at it.
	_, err = h.st.SetHostedAgentPaused(ctx, "agt_yuki", false, 0)
	w.ok(err)
	wk.sup.Update(h.build(yaml))
	wk.waitVersion("agt_yuki", store.AgentRunning, 5)

	// Rejected, then written again with the same problem: at each version.
	row, err = h.st.HostedAgent(ctx, "agt_bad")
	w.ok(err)
	_, err = h.st.UpdateHostedAgent(ctx, *row)
	w.ok(err)
	wk.sup.Update(h.build(yaml))
	if st := wk.waitVersion("agt_bad", store.AgentError, 2); st.Reason != store.ReasonSettingsRejected ||
		!strings.Contains(st.Detail, "agent.colour") {
		t.Errorf("rejected again: %+v", st)
	}
	conv, _ := w.ask(1, tu, "Still there?")
	w.waitAnswers(conv, 1)
}

// The registry's rejections say why: a YAML agent's id (operator_agent),
// no CORE_BASE_URL (runtime_misconfigured), settings that do not pass
// (settings_rejected); each with its row's version.
func TestRejectionsSayWhy(t *testing.T) {
	w := newWorld(t)
	h := w.hosting()
	own := w.ownAgent("agt_yuki", 0)
	h.host("agt_yuki", own, "", hostedSettings("m1"))
	h.host("agt_dup", w.ownAgent("agt_dup", 1), "", hostedSettings("m1"))
	yaml := w.config(nil, w.agentDoc("agt_dup", "m1", nil, nil))
	cfg, _, err := registry.Build(context.Background(), yaml, h.st, registry.Options{CoreBaseURL: w.srv.URL})
	w.ok(err)
	if len(cfg.Rejected) != 1 || cfg.Rejected[0].Reason != store.ReasonOperatorAgent || cfg.Rejected[0].Version != 1 {
		t.Errorf("a YAML agent's id: %+v", cfg.Rejected)
	}
	cfg, _, err = registry.Build(context.Background(), &config.Config{}, h.st, registry.Options{})
	w.ok(err)
	for _, r := range cfg.Rejected {
		if r.Reason != store.ReasonRuntimeMisconfigured || r.Version != 1 {
			t.Errorf("no CORE_BASE_URL: %+v", r)
		}
	}
	a, err := h.st.HostedAgent(context.Background(), "agt_yuki")
	w.ok(err)
	a.Settings = []byte(`{"model":{"adapter":"openai_chat","model":"m1","base_url":"http://10.0.0.1/v1"}}`)
	_, err = h.st.UpdateHostedAgent(context.Background(), *a)
	w.ok(err)
	cfg, _, err = registry.Build(context.Background(), &config.Config{}, h.st, registry.Options{CoreBaseURL: w.srv.URL})
	w.ok(err)
	var found bool
	for _, r := range cfg.Rejected {
		if r.AgentID == "agt_yuki" {
			found = r.Reason == store.ReasonSettingsRejected && r.Version == 2
		}
	}
	if !found {
		t.Errorf("settings that do not pass: %+v", cfg.Rejected)
	}
}

// An agent Core suspends while it runs stops, in state error with reason
// agent_suspended, and is tried again with a backoff, each start refused
// at me_get; reactivated, it runs again by itself.
func TestSuspendedAgentRunsAgainOnceReactivated(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	h := w.hosting()
	h.host("agt_yuki", own, "", hostedSettings("m1"))
	wk := h.start(h.build(&config.Config{}), models{"m1": scripted.New(scripted.Reply("Back."))})
	wk.waitState("agt_yuki", store.AgentRunning)
	w.ok(w.fc.SuspendActor(own.actor.ID))
	// The next read of its seats is denied: it stops.
	st := wk.waitState("agt_yuki", store.AgentError)
	if st.Reason != store.ReasonAgentSuspended || !strings.Contains(st.Detail, "suspended in Core") {
		t.Errorf("suspended: %+v", st)
	}
	n := len(w.calls(own.actor.ID, "me_get"))
	eventually(t, "a start tried again", func() bool { return len(w.calls(own.actor.ID, "me_get")) > n+1 })
	if st := wk.state("agt_yuki"); st.State != store.AgentError || st.Reason != store.ReasonAgentSuspended {
		t.Errorf("tried again, suspended still: %+v", st)
	}
	w.ok(w.fc.ReactivateActor(own.actor.ID))
	wk.waitState("agt_yuki", store.AgentRunning)
	conv, _ := w.ask(0, own, "Are you back?")
	w.waitAnswers(conv, 1)
}

// A hosted agent that is gone, in no configuration and not in the
// registry, has all the store held of it purged once its last state is
// OrphanAge old, its ledger kept; one that is newer, or still in the
// registry, or not a hosted agent's, is not.
func TestHousekeepingPurgesAgentsGone(t *testing.T) {
	w := newWorld(t)
	h := w.hosting()
	own := w.ownAgent("agt_live", 0)
	h.host("agt_live", own, "", hostedSettings("m1"))
	now := time.Now()
	ctx := context.Background()
	for id, age := range map[string]time.Duration{"agt_gone": time.Hour, "agt_recent": time.Minute, "agt_live": time.Hour, "yaml-old": time.Hour} {
		w.ok(h.st.SetAgentState(ctx, store.AgentState{AgentID: id, State: store.AgentStopped, UpdatedAt: now.Add(-age)}))
		w.ok(h.st.SeatSeen(ctx, store.SeatRef{AgentID: id, MemberID: "m1", CourseID: "c1", SeenAt: now.Add(-age)}))
		w.ok(h.st.RecordAnswer(ctx, store.AnswerRecord{ID: "r-" + id, AgentID: id, Outcome: store.OutcomePosted, Billable: true, At: now.Add(-age)}))
	}
	cfg := h.build(&config.Config{})
	cfg.Agents = cfg.Agents[:0] // this worker runs none: the registry alone keeps agt_live
	wk := h.start(cfg, models{})
	eventually(t, "agt_gone purged", func() bool {
		_, err := h.st.AgentState(ctx, "agt_gone")
		return errors.Is(err, store.ErrNotFound)
	})
	if seats, err := h.st.KnownSeats(ctx, "agt_gone"); err != nil || len(seats) != 0 {
		t.Errorf("agt_gone's seats: %+v %v", seats, err)
	}
	if sp, err := h.st.Spend(ctx, store.SpendScope{AgentID: "agt_gone"}, now.Add(-24*time.Hour)); err != nil || sp.Answers != 1 {
		t.Errorf("agt_gone's ledger: %+v %v", sp, err)
	}
	for _, id := range []string{"agt_recent", "agt_live", "yaml-old"} {
		if _, err := h.st.AgentState(ctx, id); err != nil {
			t.Errorf("%s purged: %v", id, err)
		}
	}
	wk.stop()
}

// ActorAgent names the agent this worker runs as a Core actor, and
// whether it is hosted; a seat's snapshot keeps its course's status.
func TestActorAgent(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	tu := w.tutor("cs101-tutor")
	h := w.hosting()
	h.host("agt_yuki", own, "", hostedSettings("m1"))
	yaml := w.config(nil, w.agentDoc("cs101-tutor", "m2", nil, nil))
	wk := h.start(h.build(yaml), models{"m1": scripted.New(), "m2": scripted.New()})
	wk.waitState("agt_yuki", store.AgentRunning)
	wk.waitState("cs101-tutor", store.AgentRunning)
	if id, hosted, ok := wk.sup.ActorAgent(w.srv.URL+"/", strings.ToUpper(own.actor.ID)); id != "agt_yuki" || !hosted || !ok {
		t.Errorf("the hosted agent's actor: %q %v %v", id, hosted, ok)
	}
	if id, hosted, ok := wk.sup.ActorAgent(w.srv.URL, tu.actor.ID); id != "cs101-tutor" || hosted || !ok {
		t.Errorf("the YAML agent's actor: %q %v %v", id, hosted, ok)
	}
	if _, _, ok := wk.sup.ActorAgent(w.srv.URL, w.students[1].ID); ok {
		t.Error("an actor no agent runs as")
	}
	eventually(t, "the hosted agent's seat recorded", func() bool {
		seats, err := h.st.KnownSeats(context.Background(), "agt_yuki")
		return err == nil && len(seats) == 1 && seats[0].CourseStatus == "active"
	})
}

// A hosted agent's model is called over the hosted-model client, which
// follows no redirect and connects to public addresses alone; a YAML
// agent's over the egress client.
func TestHostedModelsUseTheGuardedClient(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("agt_yuki", 0)
	h := w.hosting()
	h.host("agt_yuki", own, "", hostedSettings("m1"))
	w.tutor("cs101-tutor")
	yaml := w.config(nil, w.agentDoc("cs101-tutor", "m2", nil, nil))
	var mu sync.Mutex
	clients := map[string]*http.Client{}
	ms := models{"m1": scripted.New(), "m2": scripted.New()}
	egress := &http.Client{Timeout: 5 * time.Second}
	wk := h.w.start(h.build(yaml), ms, workerOpts{store: h.st, edit: func(o *Options) {
		o.Secrets = secrets.Resolver{Getenv: w.getenv, Sealed: vault.Opener{Vault: h.v, Store: h.st}}
		o.HTTPClient = egress
		o.NewAdapter = func(c llm.Config) (llm.Adapter, error) {
			mu.Lock()
			clients[c.Model] = c.HTTPClient
			mu.Unlock()
			return ms[c.Model], nil
		}
	}})
	wk.waitState("agt_yuki", store.AgentRunning)
	wk.waitState("cs101-tutor", store.AgentRunning)
	mu.Lock()
	defer mu.Unlock()
	if clients["m2"] != egress {
		t.Error("the YAML agent's model is not called over the egress client")
	}
	hosted := clients["m1"]
	if hosted == nil || hosted == egress || hosted.CheckRedirect == nil {
		t.Fatalf("the hosted agent's model client: %+v", hosted)
	}
	if err := hosted.CheckRedirect(nil, nil); !errors.Is(err, netguard.ErrRedirect) {
		t.Errorf("a redirect: %v", err)
	}
	resp, err := hosted.Get("http://127.0.0.1:1/")
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Errorf("loopback: %v", err)
	}
	if _, err := NewSupervisor(Options{Config: &config.Config{}, Store: h.st, HTTPClient: &http.Client{Transport: roundTrip(nil)}}); err == nil {
		t.Error("a hosted-model client made from a transport it cannot guard")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
