package worker

import (
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// serviceCalls counts the calls of the agent runtime's tool made to Core.
func (w *world) serviceCalls(tool string) int {
	n := 0
	for _, c := range w.fc.Calls() {
		if c.Tool == tool {
			n++
		}
	}
	return n
}

// An operator's agent is named by its id in Core (core.agent_id): the
// worker is issued its token as it starts it, under the runtime's label,
// and keeps it (in memory, with no key to seal it with) for every start
// after; the configuration naming another agent in Core revokes the one
// before's token, and is issued the new one's; paused in the configuration,
// then removed from it, its token is revoked each time.
func TestOperatorAgentsByID(t *testing.T) {
	w := newWorld(t)
	tu := w.tutor("cs101-tutor")
	other := w.tutor("cs101-other")
	doc := func(over map[string]any) map[string]any { return w.agentDoc("cs101-tutor", "m1", over, nil) }
	model := scripted.New(scripted.Reply("Asked by its id."), scripted.Reply("Still the same token."), scripted.Reply("Another agent."))
	wk := w.start(w.config(nil, doc(nil)), models{"m1": model}, workerOpts{})
	wk.waitState("cs101-tutor", store.AgentRunning)
	first := w.fc.RuntimeToken(tu.actor.ID)
	if n := w.fc.RuntimeIssues(tu.actor.ID); n != 2 {
		t.Fatalf("the agent's token was issued %d times; want 2 (its creation's, and the worker's)", n)
	}
	if creds := w.fc.Credentials(tu.actor.ID); creds[0].Label != core.RuntimeTokenLabel {
		t.Errorf("the token's label: %q", creds[0].Label)
	}
	conv, _ := w.ask(0, tu, "Who are you?")
	w.waitAnswers(conv, 1)

	// Restarted by a change of its configuration: the token kept.
	wk.sup.Reload(w.config(nil, doc(map[string]any{"display_name": "The tutor"})))
	eventually(t, "the agent restarted", func() bool { return len(w.calls(tu.actor.ID, "me_get")) == 2 })
	wk.waitState("cs101-tutor", store.AgentRunning)
	if now := w.fc.RuntimeToken(tu.actor.ID); now.CredentialID != first.CredentialID || w.fc.RuntimeIssues(tu.actor.ID) != 2 {
		t.Errorf("a restart was issued another token")
	}
	conv, _ = w.ask(1, tu, "Still you?")
	w.waitAnswers(conv, 1)

	// Another agent in Core.
	wk.sup.Reload(w.config(nil, doc(map[string]any{"core": map[string]any{"agent_id": other.actor.ID}})))
	eventually(t, "the other agent issued its token", func() bool { return w.fc.RuntimeIssues(other.actor.ID) == 2 })
	wk.waitState("cs101-tutor", store.AgentRunning)
	eventually(t, "the agent before's token revoked", func() bool { return w.fc.RuntimeToken(tu.actor.ID).Token == "" })
	if w.fc.SiteChat(tu.actor.ID) || !w.fc.SiteChat(other.actor.ID) {
		t.Error("the site asks the agent before, or not the one now")
	}
	conv, _ = w.ask(0, other, "And you?")
	w.waitAnswers(conv, 1)

	// Paused, resumed, then removed.
	wk.sup.Reload(w.config(nil, doc(map[string]any{"core": map[string]any{"agent_id": other.actor.ID}, "paused": true})))
	wk.waitState("cs101-tutor", store.AgentPaused)
	if w.fc.RuntimeToken(other.actor.ID).Token != "" {
		t.Error("the paused agent's token was not revoked")
	}
	wk.sup.Reload(w.config(nil, doc(map[string]any{"core": map[string]any{"agent_id": other.actor.ID}})))
	wk.waitState("cs101-tutor", store.AgentRunning)
	if n := w.fc.RuntimeIssues(other.actor.ID); n != 3 {
		t.Errorf("resumed, the agent's token was issued %d times; want 3", n)
	}
	wk.sup.Reload(&config.Config{})
	wk.waitState("cs101-tutor", store.AgentStopped)
	eventually(t, "the removed agent's token revoked", func() bool { return w.fc.RuntimeToken(other.actor.ID).Token == "" })
}

// An operator's agent the runtime may not host is not run, saying why,
// and not tried again until its configuration changes: an mcp agent
// (mcp_agent), an id that is no agent in Core (agent_not_found); a Core
// from before hosting by id has no service to be issued a token by
// (core_too_old). None is issued a token, nor calls Core as the agent.
func TestOperatorAgentsRefused(t *testing.T) {
	t.Run("an mcp agent, and a person", func(t *testing.T) {
		w := newWorld(t)
		tools, err := w.fc.AddMCPAgent("Ken's tools", w.students[1].ID)
		w.ok(err)
		w.inCore("kens-tools", tools.ID)
		w.inCore("a-person", w.students[0].ID)
		wk := w.start(w.config(nil, w.agentDoc("kens-tools", "m1", nil, nil), w.agentDoc("a-person", "m1", nil, nil)),
			models{"m1": scripted.New()}, workerOpts{})
		for id, reason := range map[string]string{"kens-tools": store.ReasonMCPAgent, "a-person": store.ReasonAgentNotFound} {
			st := wk.waitState(id, store.AgentError)
			if st.Reason != reason {
				t.Errorf("%s: %q (%s)", id, st.Detail, st.Reason)
			}
		}
		reads := w.serviceCalls(core.ToolRuntimeAgent)
		time.Sleep(200 * time.Millisecond) // restarts twenty times over
		if again := w.serviceCalls(core.ToolRuntimeAgent); again != reads {
			t.Errorf("tried again: %d reads, %d before", again, reads)
		}
		if n := w.serviceCalls(core.ToolRuntimeIssueToken); n != 0 {
			t.Errorf("%d tokens issued", n)
		}
		if n := len(w.calls(tools.ID, "")) + len(w.calls(w.students[0].ID, "")); n != 0 {
			t.Errorf("%d calls as the agents", n)
		}
		if w.fc.Hosting(tools.ID) != "mcp" || w.fc.SiteChat(tools.ID) {
			t.Error("the mcp agent changed")
		}
	})
	t.Run("a Core too old", func(t *testing.T) {
		w := newWorldWith(t, fakecore.Options{WithoutHosting: true})
		tu := w.tutor("cs101-tutor")
		wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", nil, nil)), models{"m1": scripted.New()}, workerOpts{})
		st := wk.waitState("cs101-tutor", store.AgentError)
		if st.Reason != store.ReasonCoreTooOld {
			t.Errorf("its state: %q (%s)", st.Detail, st.Reason)
		}
		if n := len(w.calls(tu.actor.ID, "")); n != 0 {
			t.Errorf("%d calls as the agent", n)
		}
	})
}
