package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
)

// TestTwoWorkersShareAnAgentByLease: two workers on one database; only the
// one holding the agent's lease runs it, so one answer is written, and
// when it stops the other takes the agent up.
func TestTwoWorkersShareAnAgentByLease(t *testing.T) {
	dbURL := pgDatabase(t)
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	cfg := w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil))
	m1 := scripted.New(scripted.Reply("From the first worker."), scripted.Reply("From the first worker, again."))
	m2 := scripted.New(scripted.Reply("From the second worker."), scripted.Reply("From the second worker, again."))
	w1 := w.start(cfg, models{"m1": m1}, workerOpts{id: "w1", store: pgStore(t, dbURL)})
	w2 := w.start(cfg, models{"m1": m2}, workerOpts{id: "w2", store: pgStore(t, dbURL)})
	st := w1.waitState("yuki-helper", store.AgentRunning)

	conv, _ := w.ask(0, own, "Which of you answers?")
	w.waitAnswers(conv, 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(w.answers(conv)); n != 1 {
		t.Fatalf("%d answers", n)
	}
	if calls := len(m1.Requests()) + len(m2.Requests()); calls != 1 {
		t.Errorf("the models were called %d times", calls)
	}

	// The holder stops; the other takes the agent up and answers.
	holder, other, otherModel := w1, w2, m2
	if st.Worker == "w2" {
		holder, other, otherModel = w2, w1, m1
	}
	holder.stop()
	eventually(t, "the other worker running the agent", func() bool {
		s := other.state("yuki-helper")
		return s.State == store.AgentRunning && s.Worker == other.sup.WorkerID()
	})
	before := len(otherModel.Requests())
	conv2, _ := w.ask(0, own, "And now?")
	w.waitAnswers(conv2, 1)
	if len(otherModel.Requests()) != before+1 {
		t.Error("the second question was not answered by the worker that took the agent up")
	}
}

// TestLeaseTakeoverIsCounted: an agent whose last state names another
// worker that did not stop it on purpose was taken over.
func TestLeaseTakeoverIsCounted(t *testing.T) {
	w := newWorld(t)
	w.ownAgent("yuki-helper", 0)
	st := memstore.New()
	if err := st.SetAgentState(context.Background(), store.AgentState{AgentID: "yuki-helper", State: store.AgentRunning, Worker: "dead-worker"}); err != nil {
		t.Fatal(err)
	}
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": scripted.New()}, workerOpts{store: st})
	eventually(t, "the agent running here", func() bool {
		s := wk.state("yuki-helper")
		return s.State == store.AgentRunning && s.Worker == "w1"
	})
	if got := counter(t, wk.reg, "lease_takeovers_total", nil); got != 1 {
		t.Errorf("lease_takeovers_total = %v", got)
	}
}

// TestTwoWorkersWithoutSharedLeasesPostOnce: two workers that do not share
// a store both answer the same question at once, under the same key; Core
// takes one, and nothing is posted twice (§7.4).
func TestTwoWorkersWithoutSharedLeasesPostOnce(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	cfg := w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil))
	var arrived atomic.Int32
	both := func(text string) scripted.Step {
		return func(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
			arrived.Add(1)
			deadline := time.Now().Add(5 * time.Second)
			for arrived.Load() < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			return scripted.Reply(text)(ctx, nil)
		}
	}
	m1 := scripted.New(both("From the first worker."))
	m2 := scripted.New(both("From the second worker."))
	w.start(cfg, models{"m1": m1}, workerOpts{id: "w1"})
	w.start(cfg, models{"m1": m2}, workerOpts{id: "w2"})

	conv, msg := w.ask(0, own, "Which of you answers?")
	w.waitAnswers(conv, 1)
	eventually(t, "both workers to post", func() bool { return len(w.calls(own.actor.ID, toolAnswer)) >= 2 })
	time.Sleep(100 * time.Millisecond)
	got := w.answers(conv)
	if len(got) != 1 || got[0].IdempotencyKey != core.AnswerKey(conv, msg, 1) {
		t.Fatalf("answers %+v", got)
	}
	codes := map[string]int{}
	for _, c := range w.calls(own.actor.ID, toolAnswer) {
		codes[c.Status+"/"+c.Code]++
	}
	if codes["executed/"] != 1 || codes["error/idempotency_conflict"] != 1 {
		t.Errorf("conversation_answer came back %v", codes)
	}
}

// TestWriteAheadResendsAfterATimeout: Core carries out the answer but its
// reply comes too late; the same bytes go again under the same key, Core
// replays the outcome, and one answer is posted (§2.2).
func TestWriteAheadResendsAfterATimeout(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	var first atomic.Bool
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == toolAnswer && first.CompareAndSwap(false, true) {
			return &fakecore.Injection{DelayAfter: time.Second}
		}
		return nil
	})
	model := scripted.New(scripted.Reply("Posted once."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{
		edit: func(o *Options) { o.HTTPClient = &http.Client{Timeout: 300 * time.Millisecond} },
	})
	conv, msg := w.ask(0, own, "Will this be posted twice?")
	w.waitAnswers(conv, 1)
	key := core.AnswerKey(conv, msg, 1)
	eventually(t, "the answer settled", func() bool {
		at, err := wk.st.Attempt(context.Background(), "yuki-helper", key)
		return err == nil && at.State == store.AttemptExecuted
	})
	calls := w.calls(own.actor.ID, toolAnswer)
	if len(calls) != 2 || string(calls[0].Args) != string(calls[1].Args) || calls[0].IdempotencyKey != key || calls[1].IdempotencyKey != key || !calls[1].Replayed {
		t.Errorf("conversation_answer calls: %+v", calls)
	}
	if n := len(w.answers(conv)); n != 1 {
		t.Errorf("%d answers", n)
	}
}

// TestWriteAheadResendsWhatACrashLeft: an attempt found sending when a seat
// starts is sent again with its stored bytes, before anything else, and
// the model is not called.
func TestWriteAheadResendsWhatACrashLeft(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	conv, msg := w.ask(0, own, "Asked before the crash.")
	key := core.AnswerKey(conv, msg, 1)
	args, _ := json.Marshal(core.AnswerArgs{CourseID: w.co.ID, ConversationID: conv, InReplyToMessageID: msg, Body: "Written before the crash.", IdempotencyKey: key})
	st := memstore.New()
	if _, err := st.PutAttempt(context.Background(), store.Attempt{Key: key, AgentID: "yuki-helper", MemberID: own.seat.ID, CourseID: w.co.ID,
		ConversationID: conv, MessageID: msg, No: 1, Tool: toolAnswer, Args: args, Kind: kindModel}); err != nil {
		t.Fatal(err)
	}
	model := scripted.New()
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{store: st})
	got := w.waitAnswers(conv, 1)
	if got[0].Body != "Written before the crash." || got[0].IdempotencyKey != key {
		t.Errorf("answer %+v", got[0])
	}
	if n := len(model.Requests()); n != 0 {
		t.Errorf("the model was called %d times", n)
	}
	eventually(t, "the attempt settled", func() bool {
		at, err := st.Attempt(context.Background(), "yuki-helper", key)
		return err == nil && at.State == store.AttemptExecuted
	})
}
