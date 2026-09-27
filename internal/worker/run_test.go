package worker

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// TestNoSecretOrTextInTheLogs runs agents through answers, tool calls, a
// proposal, a 429, a retraction and a 401, with every log line kept as
// written (no redacting handler), and finds no token, no key, and none of
// what anyone wrote in them.
func TestNoSecretOrTextInTheLogs(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	tu := w.tutor("cs101-tutor")
	w.ok(w.fc.SetLevel(tu.seat.ID, "conversation_answer", "confirm_required"))
	var limited atomic.Bool
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == "course_get" && limited.CompareAndSwap(false, true) {
			return &fakecore.Injection{Status: http.StatusTooManyRequests, RetryAfter: time.Second}
		}
		return nil
	})
	texts := []string{"Secret question about my grade?", "Private follow-up text.", "The model's private answer.",
		"The tutor's private answer.", "Another private question."}
	ownModel := scripted.New(
		scripted.CallTool("course_get", `{}`),
		scripted.CallTool("document_get", `{"document_id":"`+w.co.SlidesID+`"}`),
		scripted.Reply(texts[2]),
	)
	tutorModel := scripted.New(scripted.Reply(texts[3]))
	cfg := w.config(nil, w.agentDoc("yuki-helper", "own", nil, nil), w.agentDoc("cs101-tutor", "tutor", nil, nil))
	wk := w.start(cfg, models{"own": ownModel, "tutor": tutorModel}, workerOpts{edit: func(o *Options) {
		o.CoreRetry.Sleep = func(ctx context.Context, _ time.Duration) error { time.Sleep(time.Millisecond); return ctx.Err() }
	}})
	conv, _ := w.ask(0, own, texts[0])
	got := w.waitAnswers(conv, 1)
	tconv, tmsg := w.ask(1, tu, texts[4])
	p := w.waitProposal("answer:" + tconv + ":" + tmsg + ":1")
	w.ok(w.fc.Reject(p.ActionID, "Private rejection reason."))
	w.ok(w.fc.Retract(got[0].ID, w.satoSeat.ID, "private retraction reason"))
	eventually(t, "the retraction noted", func() bool { return strings.Contains(wk.state("yuki-helper").Detail, "retracted") })
	w.ok(w.fc.Revoke(own.actor.Token))
	wk.waitState("yuki-helper", store.AgentUnauthorized)
	wk.stop()

	logs := w.logs.String()
	if !strings.Contains(logs, `"msg":"answer"`) {
		t.Fatal("the run logged no answer")
	}
	secrets := []string{modelKey, own.actor.Token, tu.actor.Token, w.sato.Token, w.students[0].Token, "ais_"}
	for _, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("a log line holds a secret (%.8s…)", s)
		}
	}
	for _, s := range append(texts, "Private rejection reason.", "private retraction reason") {
		if strings.Contains(logs, s) {
			t.Errorf("a log line holds what someone wrote: %q", s)
		}
	}
}

// TestPresence: an idle agent's calls to Core are never more than
// inbox_max_s (and its jitter) apart, whether or not it has a seat to poll,
// so that Core shows it present (§2.5); a paused one makes none at all.
func TestPresence(t *testing.T) {
	polling := map[string]any{"polling": map[string]any{"inbox_idle_s": 0.04, "inbox_max_s": 0.08, "memberships_s": 5, "events_s": 5}}
	bound := time.Duration(0.08*1.25*float64(time.Second)) + 60*time.Millisecond
	gaps := func(w *world, actor string, since time.Time) time.Duration {
		var at []time.Time
		for _, c := range w.fc.Calls() {
			if c.ActorID == actor && c.At.After(since) {
				at = append(at, c.At)
			}
		}
		slices.SortFunc(at, func(a, b time.Time) int { return a.Compare(b) })
		var most time.Duration
		for i := 1; i < len(at); i++ {
			most = max(most, at[i].Sub(at[i-1]))
		}
		if len(at) < 5 {
			t.Fatalf("only %d calls", len(at))
		}
		return most
	}
	t.Run("polling a seat", func(t *testing.T) {
		w := newWorld(t)
		own := w.ownAgent("yuki-helper", 0)
		wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", polling, nil)), models{"m1": scripted.New()}, workerOpts{})
		wk.waitState("yuki-helper", store.AgentRunning)
		since := time.Now().Add(200 * time.Millisecond)
		time.Sleep(time.Second + 200*time.Millisecond)
		if most := gaps(w, own.actor.ID, since); most > bound {
			t.Errorf("calls %s apart, more than %s", most, bound)
		}
	})
	t.Run("with no seat to poll", func(t *testing.T) {
		w := newWorld(t)
		own := w.ownAgent("yuki-helper", 0)
		courses := map[string]any{w.co.ID: map[string]any{"enabled": false}}
		wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", polling, courses)), models{"m1": scripted.New()}, workerOpts{})
		wk.waitState("yuki-helper", store.AgentRunning)
		since := time.Now().Add(200 * time.Millisecond)
		time.Sleep(time.Second + 200*time.Millisecond)
		if most := gaps(w, own.actor.ID, since); most > bound {
			t.Errorf("calls %s apart, more than %s", most, bound)
		}
		if n := len(w.calls(own.actor.ID, "conversation_inbox")); n != 0 {
			t.Errorf("the disabled course's inbox was polled %d times", n)
		}
	})
	t.Run("paused", func(t *testing.T) {
		w := newWorld(t)
		own := w.ownAgent("yuki-helper", 0)
		wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", mergeMaps(polling, map[string]any{"paused": true}), nil)),
			models{"m1": scripted.New()}, workerOpts{})
		wk.waitState("yuki-helper", store.AgentPaused)
		time.Sleep(200 * time.Millisecond)
		if n := len(w.calls(own.actor.ID, "")); n != 0 {
			t.Errorf("a paused agent made %d calls", n)
		}
	})
}

// TestReload: agents added, paused, changed and removed are started,
// stopped and restarted.
func TestReload(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	tu := w.tutor("cs101-tutor")
	ms := models{"m1": scripted.New(scripted.Reply("One.")), "m2": scripted.New(scripted.Reply("Two.")), "m3": scripted.New(scripted.Reply("Three."))}
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), ms, workerOpts{})
	wk.waitState("yuki-helper", store.AgentRunning)

	// The tutor is added, and Yuki's agent changes model.
	wk.sup.Reload(w.config(nil, w.agentDoc("yuki-helper", "m2", nil, nil), w.agentDoc("cs101-tutor", "m3", nil, nil)))
	wk.waitState("cs101-tutor", store.AgentRunning)
	eventually(t, "the changed agent running again", func() bool {
		for _, s := range wk.sup.Status() {
			if s.AgentID == "yuki-helper" && s.Running && s.State == store.AgentRunning {
				return true
			}
		}
		return false
	})
	conv, _ := w.ask(0, own, "Which model?")
	if got := w.waitAnswers(conv, 1); got[0].Body != "Two." {
		t.Errorf("answered %q", got[0].Body)
	}

	// Yuki's agent paused; the tutor removed.
	wk.sup.Reload(w.config(nil, w.agentDoc("yuki-helper", "m2", map[string]any{"paused": true}, nil)))
	wk.waitState("yuki-helper", store.AgentPaused)
	wk.waitState("cs101-tutor", store.AgentStopped)
	time.Sleep(50 * time.Millisecond)
	n := len(w.calls(own.actor.ID, "")) + len(w.calls(tu.actor.ID, ""))
	time.Sleep(200 * time.Millisecond)
	if more := len(w.calls(own.actor.ID, "")) + len(w.calls(tu.actor.ID, "")) - n; more != 0 {
		t.Errorf("%d calls after pausing and removing", more)
	}
	if st := wk.sup.Status(); len(st) != 1 || !st[0].Paused {
		t.Errorf("status %+v", st)
	}
}

// TestShutdownGivesAnswersTheirGrace: an answer in progress when the worker
// stops is given the shutdown grace to finish, and the agent's lease is
// released and its state recorded as stopped.
func TestShutdownGivesAnswersTheirGrace(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	started := make(chan struct{})
	slow := func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		return scripted.Reply("Finished in the grace.")(ctx, req)
	}
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": scripted.New(slow)}, workerOpts{
		edit: func(o *Options) { o.Env = config.Env{ShutdownGrace: 5 * time.Second} },
	})
	conv, _ := w.ask(0, own, "Answer before you go.")
	<-started
	wk.stop()
	if got := w.answers(conv); len(got) != 1 || got[0].Body != "Finished in the grace." {
		t.Errorf("answers %+v", got)
	}
	if st := wk.state("yuki-helper"); st.State != store.AgentStopped {
		t.Errorf("state %+v", st)
	}
	ok, err := wk.st.AcquireLease(context.Background(), "agent:yuki-helper", "someone-else", time.Second)
	if err != nil || !ok {
		t.Errorf("the lease was not released: %v %v", ok, err)
	}
}
