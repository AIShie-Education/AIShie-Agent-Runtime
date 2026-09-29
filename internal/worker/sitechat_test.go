package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
)

// siteChatCalls are the actor's me.site_chat calls, their arguments read.
func (w *world) siteChatCalls(actor string) []map[string]any {
	var out []map[string]any
	for _, c := range w.calls(actor, "me_site_chat") {
		var m map[string]any
		w.ok(json.Unmarshal(c.Args, &m))
		m["status"] = c.Status
		out = append(out, m)
	}
	return out
}

// TestSiteChatDeclared: each time the runtime starts running an agent with
// an owner, it declares with the agent's token, once, that the agent takes
// conversations in the site, under a key of its own; it never takes that
// back, stopping included.
func TestSiteChatDeclared(t *testing.T) {
	w := newWorld(t)
	own, tu := w.ownAgent("yuki-helper", 0), w.tutor("tutor")
	cfg := w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil), w.agentDoc("tutor", "m2", nil, nil))
	wk := w.start(cfg, models{"m1": scripted.New(scripted.Reply("Hello.")), "m2": scripted.New()}, workerOpts{})
	for _, ag := range []agent{own, tu} {
		eventually(t, "the declaration of "+ag.id, func() bool { return w.fc.SiteChat(ag.actor.ID) })
	}
	conv, _ := w.ask(0, own, "Are you there?")
	w.waitAnswers(conv, 1)
	wk.stop()
	keys := map[string]bool{}
	for _, ag := range []agent{own, tu} {
		calls := w.siteChatCalls(ag.actor.ID)
		if len(calls) != 1 || calls[0]["on"] != true || calls[0]["status"] != "executed" ||
			!strings.HasPrefix(calls[0]["idempotency_key"].(string), "site-chat:") {
			t.Fatalf("%s declared %+v; want once, on", ag.id, calls)
		}
		keys[calls[0]["idempotency_key"].(string)] = true
	}
	if !w.fc.SiteChat(own.actor.ID) || !w.fc.SiteChat(tu.actor.ID) {
		t.Error("stopping took the declaration back")
	}
	// Started again: declared again, under a new key.
	w.start(cfg, models{"m1": scripted.New(), "m2": scripted.New()}, workerOpts{id: "w2"})
	eventually(t, "the declaration of the second start", func() bool { return len(w.siteChatCalls(own.actor.ID)) == 2 })
	calls := w.siteChatCalls(own.actor.ID)
	if key := calls[1]["idempotency_key"].(string); keys[key] || calls[1]["on"] != true {
		t.Errorf("the second start declared %+v", calls[1])
	}
}

// TestSiteChatUnowned: an agent nobody owns declares once a seat of its
// answers, and not before.
func TestSiteChatUnowned(t *testing.T) {
	w := newWorld(t)
	a, err := w.fc.AddAgent("The department's helper", w.sato.ID)
	w.ok(err)
	w.ok(w.fc.SetOwner(a.ID, ""))
	token, err := w.fc.IssueToken(a.ID)
	w.ok(err)
	w.env.Store(tokenVar("helper"), token)
	seat := w.must(w.fc.Seat(a.ID, w.co.ID, fakecore.SeatOptions{Perms: map[string]string{"document_read": "autonomous", "conversation_answer": "denied"}}))
	wk := w.start(w.config(nil, w.agentDoc("helper", "m1", nil, nil)), models{"m1": scripted.New()}, workerOpts{})
	polls := func() float64 {
		return counter(t, wk.reg, "core_calls_total", map[string]string{"tool": "me_memberships"})
	}
	eventually(t, "three reads of its seats", func() bool { return polls() >= 3 })
	if n := len(w.siteChatCalls(a.ID)); n != 0 {
		t.Fatalf("an agent nobody owns, answering nowhere, declared %d times", n)
	}
	w.ok(w.fc.SetLevel(seat.ID, "conversation_answer", "autonomous"))
	eventually(t, "the declaration once a seat answers", func() bool { return w.fc.SiteChat(a.ID) })
	from := polls()
	eventually(t, "more reads of its seats", func() bool { return polls() >= from+3 })
	if n := len(w.siteChatCalls(a.ID)); n != 1 {
		t.Errorf("declared %d times; want once", n)
	}
}

// TestSiteChatUnknownToCore: a Core that does not offer me.site_chat, as
// one from before it, is sent none, and the runtime says so once per
// catalogue; its agents answer as ever.
func TestSiteChatUnknownToCore(t *testing.T) {
	w := newWorldWith(t, fakecore.Options{WithoutSiteChat: true})
	own, tu := w.ownAgent("yuki-helper", 0), w.tutor("tutor")
	cfg := w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil), w.agentDoc("tutor", "m2", nil, nil))
	w.start(cfg, models{"m1": scripted.New(scripted.Reply("Hello.")), "m2": scripted.New()}, workerOpts{})
	conv, _ := w.ask(0, own, "Are you there?")
	w.waitAnswers(conv, 1)
	eventually(t, "the tutor running", func() bool { return len(w.calls(tu.actor.ID, "conversation_inbox")) > 0 })
	if n := len(w.calls("", "me_site_chat")) + len(w.calls(own.actor.ID, "me_site_chat")) + len(w.calls(tu.actor.ID, "me_site_chat")); n != 0 {
		t.Errorf("me.site_chat was sent %d times to a Core that does not offer it", n)
	}
	if n := strings.Count(w.logs.String(), "does not offer me.site_chat"); n != 1 {
		t.Errorf("said %d times that Core does not offer me.site_chat; want once", n)
	}
}
