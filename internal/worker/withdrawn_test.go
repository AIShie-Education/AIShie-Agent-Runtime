package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

// A question its opener withdraws, retracting it ("stop" in the chat), is
// not answered: against a Core whose withdrawn question waits for no
// answer, as the fake and the pinned Core are, and against one before
// AIShie-Core #42, whose state still says it waits and which would post an
// answer to it
// (fakecore.Options.WithdrawnWaits).

// withdrawnCore names a test's Core by what it does with a question
// withdrawn.
func withdrawnCore(waits bool) string {
	if waits {
		return "withdrawn_waits"
	}
	return "withdrawn_waits_for_nothing"
}

// retractOnce retracts the question of conv as student i, once, from
// whichever goroutine asks first.
type retractOnce struct {
	w    *world
	i    int
	once sync.Once
}

func (r *retractOnce) retract(conv string) {
	r.once.Do(func() {
		msgs := r.w.fc.Messages(conv)
		if len(msgs) == 0 {
			r.w.t.Error("no question to retract in " + conv)
			return
		}
		if err := r.w.fc.Retract(msgs[len(msgs)-1].ID, r.w.studentSeats[r.i].ID, "never mind"); err != nil {
			r.w.t.Error(err)
		}
	})
}

// conversationOf is the conversation_id a call's arguments name.
func conversationOf(args json.RawMessage) string {
	var a struct {
		ConversationID string `json:"conversation_id"`
	}
	_ = json.Unmarshal(args, &a)
	return a.ConversationID
}

// settled waits for the agent's inbox to be polled n more times: long
// enough for anything more it meant to do about what it found there.
func (w *world) settled(ag agent, n int) {
	w.t.Helper()
	from := len(w.calls(ag.actor.ID, "conversation_inbox"))
	eventually(w.t, "more inbox polls", func() bool { return len(w.calls(ag.actor.ID, "conversation_inbox")) >= from+n })
}

// TestWithdrawnBeforeItIsRead: the opener withdraws the question as the
// claim reads the conversation, after the inbox listed it: the model is
// never asked, nothing is posted, and the ledger says dropped.
func TestWithdrawnBeforeItIsRead(t *testing.T) {
	for _, waits := range []bool{false, true} {
		t.Run(withdrawnCore(waits), func(t *testing.T) {
			w := newWorldWith(t, fakecore.Options{WithdrawnWaits: waits})
			own := w.ownAgent("yuki-helper", 0)
			model := scripted.New(scripted.Reply("Not to be asked."))
			stop := &retractOnce{w: w}
			w.fc.OnCall(func(tool string, args json.RawMessage) {
				if tool == "conversation_messages" {
					stop.retract(conversationOf(args))
				}
			})
			over := map[string]any{"polling": onSchedule(nil)}
			wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, nil)), models{"m1": model}, workerOpts{})
			conv, _ := w.ask(0, own, "When is HW3 due?")
			eventually(t, "the question dropped", func() bool { return slices.Contains(wk.st.outcomes(conv), store.OutcomeDropped) })
			w.settled(own, 5)
			if n := len(model.Requests()); n != 0 {
				t.Errorf("the model was asked %d times about a question withdrawn", n)
			}
			if as := w.answers(conv); len(as) != 0 {
				t.Errorf("answers posted: %+v", as)
			}
			if o := wk.st.outcomes(conv); !slices.Equal(o, []string{store.OutcomeDropped}) {
				t.Errorf("outcomes %v", o)
			}
			if !strings.Contains(w.logs.String(), "the question was withdrawn: it is not answered") {
				t.Error("no log line says the question was withdrawn")
			}
		})
	}
}

// TestMovedOnNamingNoMessage: the opener withdraws the question while its
// answer is being sent, and Core refuses the answer as moved_on naming no
// message, as a Core whose withdrawn question waits for nothing does. The
// conversation is read again, the question found withdrawn, and nothing
// more is tried: no second attempt, no loop. Against the pinned Core, whose
// state still says the question waits (and which would have posted the
// answer, refused here as the newer Core refuses it), the same.
func TestMovedOnNamingNoMessage(t *testing.T) {
	for _, waits := range []bool{false, true} {
		t.Run(withdrawnCore(waits), func(t *testing.T) {
			w := newWorldWith(t, fakecore.Options{WithdrawnWaits: waits})
			own := w.ownAgent("yuki-helper", 0)
			model := scripted.New(scripted.Reply("HW3 is due on Friday."), scripted.Reply("Not to be asked."))
			stop := &retractOnce{w: w}
			var sends atomic.Int32
			refuse := wrapCaller(func(next core.Caller) core.Caller {
				return callerFunc(func(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
					if tool != toolAnswer {
						return next.Call(ctx, tool, args)
					}
					sends.Add(1)
					stop.retract(conversationOf(args))
					if !waits {
						return next.Call(ctx, tool, args)
					}
					return &core.Envelope{Status: core.StatusFailed, Error: &core.Error{Code: core.CodeConflict,
						Message: "the question was withdrawn: nothing waits for an answer now", Details: map[string]any{"reason": core.ReasonMovedOn}}}, nil
				})
			})
			over := map[string]any{"polling": onSchedule(nil)}
			wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, nil)), models{"m1": model}, workerOpts{edit: refuse})
			conv, msg := w.ask(0, own, "When is HW3 due?")
			eventually(t, "the answer refused", func() bool { return len(wk.st.outcomes(conv)) > 0 })
			w.settled(own, 5)
			if o := wk.st.outcomes(conv); !slices.Equal(o, []string{store.OutcomeDropped}) {
				t.Errorf("outcomes %v", o)
			}
			if n := sends.Load(); n != 1 {
				t.Errorf("the answer was sent %d times", n)
			}
			if n := len(model.Requests()); n != 1 {
				t.Errorf("the model was asked %d times", n)
			}
			if as := w.answers(conv); len(as) != 0 {
				t.Errorf("answers posted: %+v", as)
			}
			at, err := wk.st.Attempt(context.Background(), "yuki-helper", core.AnswerKey(conv, msg, 1))
			if err != nil || at.State != store.AttemptFailed || at.Reason != core.ReasonMovedOn {
				t.Errorf("the attempt %+v, %v", at, err)
			}
			if !strings.Contains(w.logs.String(), "the question was withdrawn: it is not answered again") {
				t.Error("no log line says the question was withdrawn")
			}
			// Its draft went with the question: its end is not sent.
			noDraftEnd(t, w, own)
		})
	}
}

// noDraftEnd fails t if ag ended a draft with done.
func noDraftEnd(t *testing.T, w *world, ag agent) {
	t.Helper()
	for _, c := range w.calls(ag.actor.ID, core.ToolDraft) {
		var args core.DraftArgs
		if err := json.Unmarshal(c.Args, &args); err != nil {
			t.Fatal(err)
		}
		if args.Done {
			t.Errorf("a draft was ended with done: %s", c.Args)
		}
	}
}

// stalling is a model's call that streams the first words of its answer
// and then waits for its caller to give up, as a provider that stops mid
// answer does: it says on started that it has begun, and on gaveUp why its
// call ended.
func stalling(started chan<- struct{}, gaveUp chan<- error) scripted.Step {
	return func(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
		scripted.TextOf(ctx)("HW3 is due")
		started <- struct{}{}
		<-ctx.Done()
		gaveUp <- context.Cause(ctx)
		return nil, ctx.Err()
	}
}

// streaming is a model's call that streams its answer a word every 20 ms,
// for as long as it is let: until its caller gives up, which it says on
// gaveUp, or, after words words when words is above zero, when it answers.
func streaming(started chan<- struct{}, gaveUp chan<- error, words int) scripted.Step {
	return func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
		text := scripted.TextOf(ctx)
		started <- struct{}{}
		for i := 0; words <= 0 || i < words; i++ {
			text("word ")
			select {
			case <-ctx.Done():
				gaveUp <- context.Cause(ctx)
				return nil, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
		return scripted.Reply(strings.Repeat("word ", words))(ctx, req)
	}
}

// waitFor waits for ch, failing t after a while.
func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

// TestWithdrawnBeforeWritingBegins: a retraction read just before the
// answer to that message begins to be written, by a claim that read the
// conversation before it, stops the answer as it begins; one read a minute
// before, or of another message, does not, nor one read after the answer
// was written.
func TestWithdrawnBeforeWritingBegins(t *testing.T) {
	now := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	var clock atomic.Pointer[time.Time]
	clock.Store(&now)
	sup, err := NewSupervisor(Options{Config: &config.Config{}, Store: memstore.New(), WorkerID: "w", Now: func() time.Time { return *clock.Load() }})
	if err != nil {
		t.Fatal(err)
	}
	a := newAgent(sup, &config.Agent{ID: "a"})
	a.withdraw("c1", "m1")
	u, ctx := a.writing(context.Background(), "course", "c1", "m1", nil)
	if cause := context.Cause(ctx); !errors.Is(cause, errWithdrawn) {
		t.Errorf("the answer to a message retracted begins, %v", cause)
	}
	if stopped, seen := a.written(u); !stopped || seen != "event" {
		t.Errorf("written: withdrawn %v, seen %q", stopped, seen)
	}

	u, ctx = a.writing(context.Background(), "course", "c1", "m2", nil)
	a.withdraw("c1", "m1")
	if ctx.Err() != nil {
		t.Error("another message's retraction stopped the answer")
	}
	if stopped, _ := a.written(u); stopped {
		t.Error("written: withdrawn")
	}
	a.withdraw("c1", "m2")

	later := now.Add(retractedFor + time.Second)
	clock.Store(&later)
	u, ctx = a.writing(context.Background(), "course", "c1", "m1", nil)
	if ctx.Err() != nil {
		t.Error("a retraction read long before stopped the answer")
	}
	a.written(u)
}

// TestWithdrawnStopsTheAnswer: the opener withdraws the question while the
// model is part way through its answer. The model's call is cancelled
// within moments, for the question withdrawn; nothing is posted; nothing
// more of the draft is written, not even its end; the ledger says dropped;
// and the question is not tried again. The seat's events are read on a
// schedule of 30 s: the retraction is seen by their long poll while the
// answer is being written, or, where Core refuses a draft once the question
// is withdrawn, by that too.
func TestWithdrawnStopsTheAnswer(t *testing.T) {
	for _, c := range []struct {
		name string
		o    fakecore.Options
	}{
		{withdrawnCore(false), fakecore.Options{}},
		{withdrawnCore(false) + "_without_drafts", fakecore.Options{WithoutDraft: true}},
		{withdrawnCore(true), fakecore.Options{WithdrawnWaits: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorldWith(t, c.o)
			own := w.ownAgent("yuki-helper", 0)
			started, gaveUp := make(chan struct{}, 1), make(chan error, 1)
			model := scripted.New(stalling(started, gaveUp))
			over := map[string]any{"polling": map[string]any{"events_s": 30}}
			wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, nil)), models{"m1": model}, draftsEvery(10*time.Millisecond))
			conv, msg := w.ask(0, own, "When is HW3 due?")
			waitFor(t, started, "the model's answer begun")
			if !c.o.WithoutDraft {
				eventually(t, "the draft's text", func() bool {
					d, ok := w.fc.Draft(conv)
					return ok && d.Text != nil
				})
			}

			w.ok(w.fc.Retract(msg, w.studentSeats[0].ID, "never mind"))
			withdrawn := time.Now()
			if cause := waitFor(t, gaveUp, "the model's call cancelled"); !errors.Is(cause, errWithdrawn) {
				t.Errorf("the model's call ended for %v", cause)
			}
			if took := time.Since(withdrawn); took > 2*time.Second {
				t.Errorf("the model's call was cancelled %s after the question was withdrawn", took)
			}
			eventually(t, "the answer dropped", func() bool { return len(wk.st.outcomes(conv)) > 0 })
			w.settled(own, 2)
			if o := wk.st.outcomes(conv); !slices.Equal(o, []string{store.OutcomeDropped}) {
				t.Errorf("outcomes %v", o)
			}
			if err := model.Err(); err != nil {
				t.Errorf("the question was tried again: %v", err)
			}
			if as := w.answers(conv); len(as) != 0 {
				t.Errorf("answers posted: %+v", as)
			}
			if n := len(w.calls(own.actor.ID, toolAnswer)); n != 0 {
				t.Errorf("%d answers sent", n)
			}
			noDraftEnd(t, w, own)
			if !strings.Contains(w.logs.String(), "the question was withdrawn: the answer being written to it stops") {
				t.Error("no log line says the answer stopped")
			}
		})
	}
}

// TestRetractingAnotherMessageStopsNothing: while the answer to the
// opener's second question is being written, the first question is
// retracted, and so is the agent's answer to it; neither stops the answer
// being written, which is posted.
func TestRetractingAnotherMessageStopsNothing(t *testing.T) {
	for _, waits := range []bool{false, true} {
		t.Run(withdrawnCore(waits), func(t *testing.T) {
			w := newWorldWith(t, fakecore.Options{WithdrawnWaits: waits})
			own := w.ownAgent("yuki-helper", 0)
			started, release := make(chan struct{}, 1), make(chan struct{})
			var cancelled atomic.Bool
			model := scripted.New(scripted.Reply("HW3 is due on Friday."), func(ctx context.Context, req *llm.Request) (*llm.Response, error) {
				scripted.TextOf(ctx)("HW4 is")
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					cancelled.Store(true)
					return nil, ctx.Err()
				}
				return scripted.Reply("HW4 is due on Monday.")(ctx, req)
			})
			wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, draftsEvery(10*time.Millisecond))
			conv, q1 := w.ask(0, own, "When is HW3 due?")
			a1 := w.waitAnswers(conv, 1)[0]
			q2, err := w.fc.FollowUp(conv, "And HW4?")
			w.ok(err)
			waitFor(t, started, "the second answer begun")

			w.ok(w.fc.Retract(q1, w.studentSeats[0].ID, "one at a time"))
			w.ok(w.fc.Retract(a1.ID, w.satoSeat.ID, "inaccurate"))
			eventually(t, "the retractions read", func() bool { return strings.Contains(wk.state("yuki-helper").Detail, "retracted") })
			close(release)
			got := w.waitAnswers(conv, 2)[1]
			if cancelled.Load() {
				t.Error("the answer being written was stopped")
			}
			if got.Body != "HW4 is due on Monday." || got.InReplyTo != q2.ID {
				t.Errorf("the second answer %+v", got)
			}
			eventually(t, "both answers recorded", func() bool { return len(wk.st.outcomes(conv)) == 2 })
			if o := wk.st.outcomes(conv); !slices.Equal(o, []string{store.OutcomePosted, store.OutcomePosted}) {
				t.Errorf("outcomes %v", o)
			}
		})
	}
}

// TestDraftRefusedStopsAWithdrawnAnswer: the seat's events cannot be read,
// so that the retraction's event never comes; the question is withdrawn
// while the model streams its answer, and Core refuses the next draft, as
// the conversation waits for no answer. The conversation is read, the
// question found withdrawn, and the answer stops.
func TestDraftRefusedStopsAWithdrawnAnswer(t *testing.T) {
	w := newDraftWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == "event_list" {
			return &fakecore.Injection{Status: http.StatusServiceUnavailable}
		}
		return nil
	})
	started, gaveUp := make(chan struct{}, 1), make(chan error, 1)
	model := scripted.New(streaming(started, gaveUp, 0))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, draftsEvery(10*time.Millisecond))
	conv, msg := w.ask(0, own, "When is HW3 due?")
	waitFor(t, started, "the model's answer begun")
	eventually(t, "the draft's text", func() bool {
		d, ok := w.fc.Draft(conv)
		return ok && d.Text != nil
	})

	w.ok(w.fc.Retract(msg, w.studentSeats[0].ID, "never mind"))
	if cause := waitFor(t, gaveUp, "the model's call cancelled"); !errors.Is(cause, errWithdrawn) {
		t.Errorf("the model's call ended for %v", cause)
	}
	eventually(t, "the answer dropped", func() bool { return len(wk.st.outcomes(conv)) > 0 })
	if o := wk.st.outcomes(conv); !slices.Equal(o, []string{store.OutcomeDropped}) {
		t.Errorf("outcomes %v", o)
	}
	if as := w.answers(conv); len(as) != 0 {
		t.Errorf("answers posted: %+v", as)
	}
	if !strings.Contains(w.logs.String(), `"seen":"draft"`) {
		t.Error("the stop was not seen by the draft refused")
	}
	noDraftEnd(t, w, own)
}

// TestDraftRefusedForAClosedConversationStopsNothing: the opener closes the
// conversation while the model streams its answer, and Core refuses the
// next draft; the conversation is read, the question is not withdrawn, and
// the answer goes on, to be refused as closed, as before.
func TestDraftRefusedForAClosedConversationStopsNothing(t *testing.T) {
	w := newDraftWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	started, gaveUp := make(chan struct{}, 1), make(chan error, 1)
	model := scripted.New(streaming(started, gaveUp, 25))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, draftsEvery(10*time.Millisecond))
	conv, _ := w.ask(0, own, "When is HW3 due?")
	waitFor(t, started, "the model's answer begun")
	eventually(t, "the draft's text", func() bool {
		d, ok := w.fc.Draft(conv)
		return ok && d.Text != nil
	})
	reads := len(w.calls(own.actor.ID, "conversation_messages"))

	w.ok(w.fc.Close(conv, w.studentSeats[0].ID, "found it"))
	eventually(t, "the answer refused", func() bool { return len(wk.st.outcomes(conv)) > 0 })
	select {
	case cause := <-gaveUp:
		t.Errorf("the model's call was cancelled: %v", cause)
	default:
	}
	if o := wk.st.outcomes(conv); !slices.Equal(o, []string{store.OutcomeDropped}) {
		t.Errorf("outcomes %v", o)
	}
	sent := w.calls(own.actor.ID, toolAnswer)
	if len(sent) != 1 || sent[0].Code != "conflict" {
		t.Errorf("the answers sent: %+v", sent)
	}
	if n := len(w.calls(own.actor.ID, "conversation_messages")); n <= reads {
		t.Error("the draft refused did not have the conversation read")
	}
	if strings.Contains(w.logs.String(), "the question was withdrawn") {
		t.Error("a question closed was taken for one withdrawn")
	}
}

// TestWithdrawnAsItIsSent: the question is withdrawn while its answer is
// being sent. The pinned Core, which orders the two, posts it: nothing
// stops a send under way.
func TestWithdrawnAsItIsSent(t *testing.T) {
	w := newWorldWith(t, fakecore.Options{WithdrawnWaits: true})
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Reply("HW3 is due on Friday."))
	stop := &retractOnce{w: w}
	w.fc.OnCall(func(tool string, args json.RawMessage) {
		if tool == toolAnswer {
			stop.retract(conversationOf(args))
		}
	})
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{})
	conv, _ := w.ask(0, own, "When is HW3 due?")
	w.waitAnswers(conv, 1)
	eventually(t, "the answer recorded", func() bool { return len(wk.st.outcomes(conv)) > 0 })
	if o := wk.st.outcomes(conv); !slices.Equal(o, []string{store.OutcomePosted}) {
		t.Errorf("outcomes %v", o)
	}
	if msgs := w.fc.Messages(conv); len(msgs) != 2 || !msgs[0].Retracted {
		t.Errorf("the conversation %+v", msgs)
	}
}
