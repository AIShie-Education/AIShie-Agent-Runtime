package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// confirmedTutor is Sato's tutor, whose answers wait for a person's
// approval, and the worker running it.
func confirmedTutor(t *testing.T, w *world, model *scripted.Adapter, over map[string]any) (agent, *worker) {
	t.Helper()
	tu := w.tutor("cs101-tutor")
	w.ok(w.fc.SetLevel(tu.seat.ID, "conversation_answer", "confirm_required"))
	wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", over, nil)), models{"m1": model}, workerOpts{})
	return tu, wk
}

// waitProposal waits for the proposal of an answer under key.
func (w *world) waitProposal(key string) fakecore.Proposal {
	w.t.Helper()
	var found fakecore.Proposal
	eventually(w.t, "a proposal under "+key, func() bool {
		for _, p := range w.fc.Proposals(w.co.ID) {
			if p.IdempotencyKey == key {
				found = p
				return true
			}
		}
		return false
	})
	return found
}

// waitAttempt waits for the attempt under key to be in state.
func (wk *worker) waitAttempt(agentID, key string, state store.AttemptState) *store.Attempt {
	wk.w.t.Helper()
	var at *store.Attempt
	eventually(wk.w.t, "the attempt "+key+" "+string(state), func() bool {
		var err error
		at, err = wk.st.Attempt(context.Background(), agentID, key)
		return err == nil && at.State == state
	})
	return at
}

// TestProposalApproved: at confirm_required the answer is proposed, kept
// by its action id, and followed through event_list until a person
// approves it; then it is posted, and memory notes it (§2.4).
func TestProposalApproved(t *testing.T) {
	w := newWorld(t)
	model := scripted.New(scripted.Reply("An answer an instructor approves."))
	tu, wk := confirmedTutor(t, w, model, nil)
	conv, msg := w.ask(0, tu, "May I have an approved answer?")
	key := core.AnswerKey(conv, msg, 1)
	p := w.waitProposal(key)
	at := wk.waitAttempt("cs101-tutor", key, store.AttemptProposed)
	if at.ActionID != p.ActionID {
		t.Errorf("the attempt keeps action %q, the proposal is %q", at.ActionID, p.ActionID)
	}
	if n := len(w.answers(conv)); n != 0 {
		t.Fatalf("%d answers before approval", n)
	}
	outcome, err := w.fc.Approve(p.ActionID)
	w.ok(err)
	if outcome != "executed" {
		t.Fatalf("approval: %s", outcome)
	}
	got := w.waitAnswers(conv, 1)
	at = wk.waitAttempt("cs101-tutor", key, store.AttemptExecuted)
	if at.PostedMessageID != got[0].ID {
		t.Errorf("posted message %q, the answer is %q", at.PostedMessageID, got[0].ID)
	}
	eventually(t, "memory noting the answer", func() bool {
		notes, _ := wk.st.Notes(context.Background(), "cs101-tutor", tu.seat.ID, conv, 10)
		return len(notes) == 1 && notes[0].Kind == store.NoteAnswered && notes[0].MessageID == got[0].ID
	})
	if n := len(model.Requests()); n != 1 {
		t.Errorf("the model was called %d times", n)
	}
}

// TestProposalRejectedThenClosed: a rejected answer's reason goes into the
// next attempt's prompt, under the next number; after max_attempts
// rejections the conversation is closed under close:X (§2.4).
func TestProposalRejectedThenClosed(t *testing.T) {
	w := newWorld(t)
	model := scripted.New(scripted.Reply("Try one."), scripted.Reply("Try two."), scripted.Reply("Try three."))
	tu, wk := confirmedTutor(t, w, model, map[string]any{"answer": map[string]any{"max_attempts": 3}})
	conv, msg := w.ask(1, tu, "Explain recursion.")
	reasons := []string{"Cite the syllabus.", "Too long.", "Still wrong."}
	for i, reason := range reasons {
		key := core.AnswerKey(conv, msg, i+1)
		p := w.waitProposal(key)
		w.ok(w.fc.Reject(p.ActionID, reason))
		at := wk.waitAttempt("cs101-tutor", key, store.AttemptRejected)
		if at.Reason != reason {
			t.Errorf("attempt %d: reason %q", i+1, at.Reason)
		}
	}
	eventually(t, "the conversation closed", func() bool {
		for _, c := range w.calls(tu.actor.ID, toolClose) {
			if c.Status == "executed" && c.IdempotencyKey == core.CloseKey(conv) {
				return true
			}
		}
		return false
	})
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	reqs := model.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d model calls", len(reqs))
	}
	if strings.Contains(reqs[0].System, "Cite the syllabus.") || !strings.Contains(reqs[1].System, "Cite the syllabus.") ||
		!strings.Contains(reqs[2].System, "Too long.") {
		t.Errorf("the rejections' reasons did not reach the next attempts' prompts")
	}
	eventually(t, "the close in the ledger", func() bool {
		o := wk.st.outcomes(conv)
		return len(o) > 0 && o[len(o)-1] == store.OutcomeClosed
	})
	if n := len(w.answers(conv)); n != 0 {
		t.Errorf("%d answers posted", n)
	}
}

// TestProposalExpired: a proposal cancelled as expired is noted, and the
// question answered again under the next number.
func TestProposalExpired(t *testing.T) {
	w := newWorld(t)
	model := scripted.New(scripted.Reply("Waited too long."), scripted.Reply("Second try."))
	tu, wk := confirmedTutor(t, w, model, nil)
	conv, msg := w.ask(0, tu, "Will anyone approve this?")
	p := w.waitProposal(core.AnswerKey(conv, msg, 1))
	w.ok(w.fc.Expire(p.ActionID))
	at := wk.waitAttempt("cs101-tutor", core.AnswerKey(conv, msg, 1), store.AttemptCancelled)
	if at.Reason != "proposal_expired" {
		t.Errorf("reason %q", at.Reason)
	}
	w.waitProposal(core.AnswerKey(conv, msg, 2))
	if !strings.Contains(lastRequest(t, model).System, "waited too long for approval") {
		t.Error("the second attempt's prompt does not say the first expired")
	}
}

// TestProposalDecidedWhileDown: a proposal decided while no worker ran is
// settled from action_list_mine when the seat starts again.
func TestProposalDecidedWhileDown(t *testing.T) {
	w := newWorld(t)
	st := &recordingStore{}
	model := scripted.New(scripted.Reply("Approved while the runtime was down."))
	tu := w.tutor("cs101-tutor")
	w.ok(w.fc.SetLevel(tu.seat.ID, "conversation_answer", "confirm_required"))
	cfg := w.config(nil, w.agentDoc("cs101-tutor", "m1", map[string]any{"polling": map[string]any{"events_s": 3600}}, nil))
	wk := w.start(cfg, models{"m1": model}, workerOpts{})
	st.Store = wk.st.Store
	conv, msg := w.ask(0, tu, "Approve me while it sleeps.")
	key := core.AnswerKey(conv, msg, 1)
	p := w.waitProposal(key)
	wk.waitAttempt("cs101-tutor", key, store.AttemptProposed)
	wk.stop()
	if _, err := w.fc.Approve(p.ActionID); err != nil {
		t.Fatal(err)
	}
	wk2 := w.start(cfg, models{"m1": scripted.New()}, workerOpts{store: st.Store})
	got := w.waitAnswers(conv, 1)
	at := wk2.waitAttempt("cs101-tutor", key, store.AttemptExecuted)
	if at.PostedMessageID != got[0].ID {
		t.Errorf("posted %q, answer %q", at.PostedMessageID, got[0].ID)
	}
}
