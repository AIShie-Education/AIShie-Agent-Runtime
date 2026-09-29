package worker

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// A question its opener withdraws, retracting it ("stop" in the chat), is
// not answered: against a Core whose withdrawn question waits for no
// answer, as the fake is, and against the pinned one, whose state still
// says it waits and which would post an answer to it
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
		})
	}
}
