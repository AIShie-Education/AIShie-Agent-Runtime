package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
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
// leases (each its own store, both holding the agent's one token: two that
// shared nothing would each be issued one, revoking the other's) both
// answer the same question at once, under the same key; Core takes one,
// and nothing is posted twice (§7.4).
func TestTwoWorkersWithoutSharedLeasesPostOnce(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	cfg := w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil))
	v := testVault(t)
	st1, st2 := memstore.New(), memstore.New()
	sealing := func(st store.Store) func(*Options) {
		return func(o *Options) {
			o.Secrets = secrets.Resolver{Getenv: w.getenv, Sealed: vault.Opener{Vault: v, Store: st}}
			o.Sealer = v
		}
	}
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
	first := w.start(cfg, models{"m1": m1}, workerOpts{id: "w1", store: st1, edit: sealing(st1)})
	first.waitState("yuki-helper", store.AgentRunning)
	// The second store is given the token the first worker was issued.
	held, err := st1.AgentToken(context.Background(), "yuki-helper")
	w.ok(err)
	sealed, err := v.Seal(context.Background(), store.Secret{ID: vault.NewSecretID(), TenantID: "operator", Kind: store.SecretCoreToken},
		w.runtimeToken(own))
	w.ok(err)
	copied := *held
	copied.SecretID = sealed.ID
	w.ok(st2.PutAgentToken(context.Background(), copied, sealed, ""))
	second := w.start(cfg, models{"m1": m2}, workerOpts{id: "w2", store: st2, edit: sealing(st2)})
	second.waitState("yuki-helper", store.AgentRunning)
	issued := w.fc.RuntimeIssues(own.actor.ID)

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
	if n := w.fc.RuntimeIssues(own.actor.ID); n != issued {
		t.Errorf("issued %d tokens; %d once both ran", n, issued)
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
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{store: st})
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
	// The claim the crash cut short left no row in the ledger: the resend
	// does, and the answer counts against the day's quotas.
	eventually(t, "the answer's row in the ledger", func() bool { return len(wk.st.outcomes(conv)) == 1 })
	_, recs := wk.st.ledger()
	if r := recs[0]; r.Outcome != store.OutcomePosted || !r.Billable || r.Key != key || r.MessageID != msg {
		t.Errorf("the ledger's row %+v", r)
	}
	eventually(t, "memory noting the answer", func() bool {
		notes, _ := st.Notes(context.Background(), "yuki-helper", own.seat.ID, conv, 10)
		return len(notes) == 1 && notes[0].Kind == store.NoteAnswered && notes[0].MessageID == got[0].ID
	})
}

// leaseSwitch is a store whose agent leases can be made to belong to
// someone else, and which counts the agent leases it refuses: the lease
// ticks that find the lease lost.
type leaseSwitch struct {
	store.Store
	taken   atomic.Bool
	refused atomic.Int32
}

func (s *leaseSwitch) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if s.taken.Load() && strings.HasPrefix(name, "agent:") {
		s.refused.Add(1)
		return false, nil
	}
	return s.Store.AcquireLease(ctx, name, holder, ttl)
}

// TestLeaseLostStopsTheAgentAtOnce: when a renewal of the agent's lease
// fails, the agent stops at once and calls Core no more; when the lease is
// had again, it starts again and answers. Its calls are counted as it
// begins them, on its connection to Core, and "no more" is over ten lease
// ticks that find the lease lost, not over a time: a long poll the stop
// cancelled is logged by Core as it ends, which on a busy machine is after
// the agent has stopped, and a busy machine ticks as many times, later.
func TestLeaseLostStopsTheAgentAtOnce(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	st := &leaseSwitch{Store: memstore.New()}
	model := scripted.New(scripted.Reply("Back with the lease."))
	var began atomic.Int32
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model},
		workerOpts{store: st, edit: countCalls("yuki-helper", &began)})
	eventually(t, "the inbox polled", func() bool { return len(w.calls(own.actor.ID, "conversation_inbox")) > 0 })

	st.taken.Store(true)
	// Not running is its pollers and its answers returned: whatever it
	// calls after this, it began after.
	eventually(t, "the agent stopped", func() bool {
		s := wk.sup.Status()
		return len(s) == 1 && !s[0].Running && !s[0].Leased
	})
	n, ticks := began.Load(), st.refused.Load()
	if n == 0 {
		t.Fatal("none of the agent's calls was counted")
	}
	eventually(t, "ten lease ticks more", func() bool { return st.refused.Load() >= ticks+10 })
	if more := began.Load() - n; more != 0 {
		t.Errorf("%d calls to Core begun after the lease was lost", more)
	}

	st.taken.Store(false)
	conv, _ := w.ask(0, own, "Are you back?")
	w.waitAnswers(conv, 1)
}

// TestConversationLeasedElsewhereIsLeft: a conversation whose lease another
// worker holds is left alone, with no model call, until the lease is free.
func TestConversationLeasedElsewhereIsLeft(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	st := memstore.New()
	conv, msg := w.ask(0, own, "Who has this one?")
	if ok, err := st.AcquireLease(context.Background(), "conv:yuki-helper:"+conv, "another-worker", time.Minute); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	model := scripted.New(scripted.Reply("Mine now."))
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{store: st})
	eventually(t, "the inbox polled a few times", func() bool { return len(w.calls(own.actor.ID, "conversation_inbox")) > 3 })
	if n := len(model.Requests()); n != 0 {
		t.Fatalf("the model was called %d times for a conversation leased elsewhere", n)
	}
	if n := len(w.calls(own.actor.ID, "conversation_messages")); n != 0 {
		t.Errorf("the conversation was read %d times", n)
	}
	w.ok(st.ReleaseLease(context.Background(), "conv:yuki-helper:"+conv, "another-worker"))
	got := w.waitAnswers(conv, 1)
	if got[0].IdempotencyKey != core.AnswerKey(conv, msg, 1) {
		t.Errorf("answer %+v", got[0])
	}
}

// releaseWatch is a store that notes the agent's state each time an agent
// lease is released.
type releaseWatch struct {
	store.Store
	mu     sync.Mutex
	states []string
}

func (s *releaseWatch) ReleaseLease(ctx context.Context, name, holder string) error {
	if strings.HasPrefix(name, "agent:") {
		sts, _ := s.AgentStates(ctx)
		for _, st := range sts {
			if "agent:"+st.AgentID == name {
				s.mu.Lock()
				s.states = append(s.states, st.State)
				s.mu.Unlock()
			}
		}
	}
	return s.Store.ReleaseLease(ctx, name, holder)
}

// TestHandoverIsNotATakeover: a worker that stops, or stops running an
// agent removed from its configuration, records the agent stopped before
// it lets the lease go, so that a worker taking the agent up at once reads
// a handover and counts no takeover.
func TestHandoverIsNotATakeover(t *testing.T) {
	w := newWorld(t)
	w.ownAgent("yuki-helper", 0)
	w.tutor("cs101-tutor")
	st := &releaseWatch{Store: memstore.New()}
	cfg := w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil), w.agentDoc("cs101-tutor", "m1", nil, nil))
	wk := w.start(cfg, models{"m1": scripted.New()}, workerOpts{store: st})
	wk.waitState("yuki-helper", store.AgentRunning)
	wk.waitState("cs101-tutor", store.AgentRunning)
	wk.sup.Reload(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)))
	wk.waitState("cs101-tutor", store.AgentStopped)
	wk.stop()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.states) != 2 || st.states[0] != store.AgentStopped || st.states[1] != store.AgentStopped {
		t.Errorf("the agents' states when their leases went: %v", st.states)
	}
}

// hangingLeases is a store whose agent leases, once hung, are never
// answered until the caller gives up; it notes when each began, how long
// the caller gave each, by its context's deadline (-1 for none), and
// whether running said the agent was running when each began.
type hangingLeases struct {
	store.Store
	hung    atomic.Bool
	running func() bool // set before hung
	mu      sync.Mutex
	began   []time.Time
	given   []time.Duration
	ran     []bool
}

func (s *hangingLeases) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if s.hung.Load() && strings.HasPrefix(name, "agent:") {
		given := time.Duration(-1)
		if dl, ok := ctx.Deadline(); ok {
			given = time.Until(dl)
		}
		ran := s.running()
		s.mu.Lock()
		s.began = append(s.began, time.Now())
		s.given = append(s.given, given)
		s.ran = append(s.ran, ran)
		s.mu.Unlock()
		<-ctx.Done()
		return false, ctx.Err()
	}
	return s.Store.AcquireLease(ctx, name, holder, ttl)
}

// TestLeaseRenewalThatHangsStopsTheAgent: a store that stops answering
// the renewal of an agent's lease has failed to renew it; the agent stops
// at the first renewal that fails, before the lease would lapse, not when
// the store answers, which it never does. The lease lasts six seconds
// here, so that the bound is the lease's and not how fast a busy machine
// is: a renewal waits two of them at most, read off the deadline it gives
// the store, which is what the supervisor decides; and the agent has
// stopped, its last call ended, before the lease's six seconds from the
// first renewal that hung are out, as the next renewal begins, which
// happens only once the stop is over. That it stops at the first renewal
// that fails, not at a later one, is read off whether it was still
// running when the next began: a busy machine makes the ticks later,
// never fewer.
func TestLeaseRenewalThatHangsStopsTheAgent(t *testing.T) {
	const ttl = 6 * time.Second
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	st := &hangingLeases{Store: memstore.New()}
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": scripted.New()}, workerOpts{
		store: st, edit: func(o *Options) { o.Timing.LeaseTTL = ttl },
	})
	eventually(t, "the inbox polled", func() bool { return len(w.calls(own.actor.ID, "conversation_inbox")) > 0 })
	st.running = func() bool {
		s := wk.sup.Status()
		return len(s) == 1 && s[0].Running
	}
	st.hung.Store(true)
	within(t, 2*ttl, "two renewals hung", func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.began) >= 2
	})
	st.mu.Lock()
	began, given, ran := slices.Clone(st.began), slices.Clone(st.given), slices.Clone(st.ran)
	st.mu.Unlock()
	for i, g := range given {
		if g < 0 || g > ttl/3 {
			t.Errorf("hung renewal %d was given %v (-1: no deadline); want a third of the lease's %v at most (all: %v)", i+1, g, ttl, given)
		}
	}
	if !ran[0] {
		t.Error("the agent was not running when its lease's renewal first hung")
	}
	if i := slices.Index(ran[1:], true); i >= 0 {
		t.Errorf("the agent still ran when renewal %d of its lease began to hang, %d after the first: %v", i+2, i+1, ran)
	}
	if d := began[1].Sub(began[0]); d >= ttl {
		t.Errorf("the agent stopped %v after its lease's renewal first hung, past the lease's %v", d.Round(time.Millisecond), ttl)
	}
}
