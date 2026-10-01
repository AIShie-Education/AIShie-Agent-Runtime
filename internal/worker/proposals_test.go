package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
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

// TestProposalRejectedThenSkipped: a rejected answer's reason goes into the
// next attempt's prompt, under the next number; after max_attempts
// rejections the question is skipped until the next day, and its
// conversation left open: the runtime closes none (§2.4 would close it).
func TestProposalRejectedThenSkipped(t *testing.T) {
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
	eventually(t, "the question skipped", func() bool {
		o := wk.st.outcomes(conv)
		return len(o) > 0 && o[len(o)-1] == store.OutcomeSkipped
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
	if n := len(w.calls(tu.actor.ID, toolClose)); n != 0 {
		t.Errorf("conversation_close was called %d times", n)
	}
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

// TestActionsCursorWaitsForProposals: the seat's cursor into
// action_list_mine moves over the agent's settled actions, and stops before
// a proposal still waiting, so that its fate is found however late it
// comes.
func TestActionsCursorWaitsForProposals(t *testing.T) {
	w := newWorld(t)
	model := scripted.New(scripted.Reply("Autonomous one."), scripted.Reply("Autonomous two."),
		scripted.Reply("Waits for a person."), scripted.Reply("Waits too."))
	tu := w.tutor("cs101-tutor")
	wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", nil, nil)), models{"m1": model}, workerOpts{})
	c1, _ := w.ask(0, tu, "One?")
	w.waitAnswers(c1, 1)
	c2, _ := w.ask(1, tu, "Two?")
	w.waitAnswers(c2, 1)
	w.ok(w.fc.SetLevel(tu.seat.ID, "conversation_answer", "confirm_required"))
	eventually(t, "the seat at confirm_required", func() bool {
		st := wk.sup.Status()
		return len(st) == 1 && len(st[0].Seats) == 1 && st[0].Seats[0].Level == "confirm_required"
	})
	c3, m3 := w.ask(0, tu, "Three?")
	p3 := w.waitProposal(core.AnswerKey(c3, m3, 1))
	c4, m4 := w.ask(1, tu, "Four?")
	p4 := w.waitProposal(core.AnswerKey(c4, m4, 1))

	// The later proposal is decided first: its fate is found, and the
	// cursor stays before the earlier one, still waiting.
	w.ok(w.fc.Reject(p4.ActionID, "Not yet."))
	wk.waitAttempt("cs101-tutor", core.AnswerKey(c4, m4, 1), store.AttemptRejected)
	cursor := func() string {
		c, err := wk.st.Cursor(context.Background(), "cs101-tutor", tu.seat.ID, store.CursorActions)
		w.ok(err)
		return c
	}
	// The cursor is saved when the round of events that read it ends.
	eventually(t, "the actions cursor saved", func() bool { return cursor() != "" })
	if c := cursor(); c >= p3.ActionID {
		t.Errorf("the cursor %q is not before the proposal waiting, %s", c, p3.ActionID)
	}
	_, err := w.fc.Approve(p3.ActionID)
	w.ok(err)
	wk.waitAttempt("cs101-tutor", core.AnswerKey(c3, m3, 1), store.AttemptExecuted)
}

// TestAgentFailsThenStarts: an agent that cannot start (the runtime's own
// credential in Core is missing, then refused) is recorded in error,
// saying why and never showing a credential, and started again after a
// backoff until it can: once the operator gives the runtime its credential,
// it is issued the agent's token and runs.
func TestAgentFailsThenStarts(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	w.env.Delete(credentialVar)
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": scripted.New(scripted.Reply("Started at last."))}, workerOpts{})
	st := wk.waitState("yuki-helper", store.AgentError)
	if !strings.Contains(st.Detail, credentialVar) || st.Reason != store.ReasonRuntimeMisconfigured {
		t.Errorf("no credential: %q (%s)", st.Detail, st.Reason)
	}
	revoked := w.fc.IssueRuntimeServiceToken("revoked")
	w.ok(w.fc.RevokeServiceToken(revoked.CredentialID))
	w.env.Store(credentialVar, revoked.Token)
	eventually(t, "the refused credential said", func() bool {
		return strings.Contains(wk.state("yuki-helper").Detail, "the runtime's own credential was refused by Core")
	})
	if st := wk.state("yuki-helper"); strings.Contains(st.Detail, "aissvc_") || st.Reason != store.ReasonRuntimeMisconfigured {
		t.Errorf("a refused credential: %q (%s)", st.Detail, st.Reason)
	}
	if tok := w.fc.RuntimeToken(own.actor.ID); tok.Token != own.actor.Token {
		t.Errorf("the agent was issued a token without the runtime's credential")
	}
	w.env.Store(credentialVar, w.svc.Token)
	wk.waitState("yuki-helper", store.AgentRunning)
	conv, _ := w.ask(0, own, "Are you up?")
	w.waitAnswers(conv, 1)
}

// TestRejectedWhileLeftSending: an answer Core took as a proposal, whose
// attempt a crash left sending, is rejected while no worker runs. Sent
// again at the seat's start, it replays as rejected; the rejection's
// reason is kept on the attempt and reaches the next attempt's prompt, as
// when the events poller reads the decision.
func TestRejectedWhileLeftSending(t *testing.T) {
	w := newWorld(t)
	tu := w.tutor("cs101-tutor")
	w.ok(w.fc.SetLevel(tu.seat.ID, "conversation_answer", "confirm_required"))
	conv, msg := w.ask(0, tu, "Proposed before the crash.")
	key := core.AnswerKey(conv, msg, 1)
	args, err := json.Marshal(core.AnswerArgs{CourseID: w.co.ID, ConversationID: conv, InReplyToMessageID: msg,
		Body: "Written before the crash.", IdempotencyKey: key})
	w.ok(err)
	caller := core.NewMCPCaller(core.MCPOptions{BaseURL: w.srv.URL, Token: tu.actor.Token, HTTPClient: &http.Client{Timeout: 5 * time.Second}})
	env, err := caller.Call(context.Background(), toolAnswer, args)
	if err != nil || env.Status != core.StatusProposed {
		t.Fatalf("the answer sent before the crash: %+v, %v", env, err)
	}
	w.ok(w.fc.Reject(env.ActionID, "Cite the syllabus."))
	st := memstore.New()
	if _, err := st.PutAttempt(context.Background(), store.Attempt{Key: key, AgentID: "cs101-tutor", MemberID: tu.seat.ID, CourseID: w.co.ID,
		ConversationID: conv, MessageID: msg, No: 1, Tool: toolAnswer, Args: args, Kind: kindModel, State: store.AttemptSending}); err != nil {
		t.Fatal(err)
	}

	model := scripted.New(scripted.Reply("Second try, citing the syllabus."))
	wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", nil, nil)), models{"m1": model}, workerOpts{store: st})
	at := wk.waitAttempt("cs101-tutor", key, store.AttemptRejected)
	if at.Reason != "Cite the syllabus." || at.ActionID != env.ActionID {
		t.Errorf("the attempt settled as %+v", at)
	}
	w.waitProposal(core.AnswerKey(conv, msg, 2))
	if !strings.Contains(lastRequest(t, model).System, "Cite the syllabus.") {
		t.Errorf("the second attempt's prompt lacks the rejection's reason:\n%s", lastRequest(t, model).System)
	}
}

// TestCloseLeftSendingFromBefore: an earlier version, a message's attempts
// spent, closed a conversation, and stopped before it recorded what came of
// it: the close is left sending in the store. This version, which closes
// none, sends it again at the seat's start, the stored bytes under its key,
// as any attempt left sending: Core replays it, and the attempt and the
// ledger say it went through, so that an upgrade part way loses nothing.
func TestCloseLeftSendingFromBefore(t *testing.T) {
	w := newWorld(t)
	tu := w.tutor("cs101-tutor")
	conv, _ := w.ask(0, tu, "Closed before the upgrade.")
	key := core.CloseKey(conv)
	args, err := json.Marshal(core.CloseArgs{CourseID: w.co.ID, ConversationID: conv, Reason: "Closed after three tries.", IdempotencyKey: key})
	w.ok(err)
	caller := core.NewMCPCaller(core.MCPOptions{BaseURL: w.srv.URL, Token: tu.actor.Token, HTTPClient: &http.Client{Timeout: 5 * time.Second}})
	if env, err := caller.Call(context.Background(), toolClose, args); err != nil || env.Status != core.StatusExecuted {
		t.Fatalf("the close sent before the upgrade: %+v, %v", env, err)
	}
	st := memstore.New()
	if _, err := st.PutAttempt(context.Background(), store.Attempt{Key: key, AgentID: "cs101-tutor", MemberID: tu.seat.ID, CourseID: w.co.ID,
		ConversationID: conv, Tool: toolClose, Args: args, Kind: "close", State: store.AttemptSending}); err != nil {
		t.Fatal(err)
	}

	model := scripted.New()
	wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", nil, nil)), models{"m1": model}, workerOpts{store: st})
	wk.waitAttempt("cs101-tutor", key, store.AttemptExecuted)
	eventually(t, "the close in the ledger", func() bool {
		o := wk.st.outcomes(conv)
		return len(o) == 1 && o[0] == store.OutcomeClosed
	})
	calls := w.calls(tu.actor.ID, toolClose)
	if len(calls) != 2 || calls[1].IdempotencyKey != key || calls[1].Status != "executed" {
		t.Errorf("conversation_close calls: %+v", calls)
	}
	if err := model.Err(); err != nil || len(model.Requests()) != 0 {
		t.Errorf("the model was asked about a closed conversation: %v", err)
	}
}

// flakyActions is a store whose AttemptByAction fails once for one action.
type flakyActions struct {
	store.Store
	action atomic.Value // string
	failed atomic.Bool
}

func (s *flakyActions) AttemptByAction(ctx context.Context, agentID, actionID string) (*store.Attempt, error) {
	if id, _ := s.action.Load().(string); id == actionID && s.failed.CompareAndSwap(false, true) {
		return nil, errors.New("the store blinked")
	}
	return s.Store.AttemptByAction(ctx, agentID, actionID)
}

// TestEventReadAgainAfterAStoreFailure: when the store fails while an
// event is acted on, the events cursor stays before it, and the next read
// settles the proposal.
func TestEventReadAgainAfterAStoreFailure(t *testing.T) {
	w := newWorld(t)
	tu := w.tutor("cs101-tutor")
	w.ok(w.fc.SetLevel(tu.seat.ID, "conversation_answer", "confirm_required"))
	st := &flakyActions{Store: memstore.New()}
	model := scripted.New(scripted.Reply("An answer approved at the second reading."))
	wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", nil, nil)), models{"m1": model}, workerOpts{store: st})
	conv, msg := w.ask(0, tu, "Will the approval be found?")
	key := core.AnswerKey(conv, msg, 1)
	p := w.waitProposal(key)
	wk.waitAttempt("cs101-tutor", key, store.AttemptProposed)
	st.action.Store(p.ActionID)
	if _, err := w.fc.Approve(p.ActionID); err != nil {
		t.Fatal(err)
	}
	wk.waitAttempt("cs101-tutor", key, store.AttemptExecuted)
	if !st.failed.Load() {
		t.Error("the store never failed: the test tested nothing")
	}
	w.waitAnswers(conv, 1)
}

// slowSettle is a store that is slow to record an attempt as proposed:
// settling says the write has begun, and it ends when release is closed.
type slowSettle struct {
	store.Store
	once              sync.Once
	settling, release chan struct{}
}

func (s *slowSettle) FinishAttempt(ctx context.Context, agentID, key string, o store.Outcome) error {
	if o.State == store.AttemptProposed {
		s.once.Do(func() { close(s.settling) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.FinishAttempt(ctx, agentID, key, o)
}

// TestDecisionBeforeTheSendIsSettled: a person rejects a proposal before
// the store has recorded it, and events are read in between. The
// rejection, read while the send is under way, is read again after it, and
// settles the attempt; the next attempt follows.
func TestDecisionBeforeTheSendIsSettled(t *testing.T) {
	w := newWorld(t)
	tu := w.tutor("cs101-tutor")
	w.ok(w.fc.SetLevel(tu.seat.ID, "conversation_answer", "confirm_required"))
	st := &slowSettle{Store: memstore.New(), settling: make(chan struct{}), release: make(chan struct{})}
	model := scripted.New(scripted.Reply("Decided before it was stored."), scripted.Reply("The second try."))
	wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", nil, nil)), models{"m1": model}, workerOpts{store: st})
	conv, msg := w.ask(0, tu, "Can a decision come first?")
	key := core.AnswerKey(conv, msg, 1)
	p := w.waitProposal(key)
	select {
	case <-st.settling:
	case <-time.After(15 * time.Second):
		t.Fatal("the proposal was never recorded")
	}
	w.ok(w.fc.Reject(p.ActionID, "Too soon."))
	// Reads of events are one after another: the second to begin after
	// the rejection read it, and the third began once it was acted on.
	n := len(w.calls(tu.actor.ID, "event_list"))
	eventually(t, "events read after the rejection", func() bool { return len(w.calls(tu.actor.ID, "event_list")) >= n+3 })
	close(st.release)
	at := wk.waitAttempt("cs101-tutor", key, store.AttemptRejected)
	if at.Reason != "Too soon." {
		t.Errorf("reason = %q", at.Reason)
	}
	w.waitProposal(core.AnswerKey(conv, msg, 2))
}

// TestAttemptsExhaustedSkip: a question whose attempts are spent is held
// back until the next UTC day, and its conversation is not closed: by
// default, with on_attempts_exhausted skip, and with close, which a
// configuration written before the runtime stopped closing conversations
// may still say, taken as skip and logged as deprecated.
func TestAttemptsExhaustedSkip(t *testing.T) {
	for _, c := range []struct {
		name string
		over map[string]any
	}{
		{"by default", nil},
		{"skip", map[string]any{"on_attempts_exhausted": "skip"}},
		{"close, deprecated", map[string]any{"on_attempts_exhausted": "close"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			model := scripted.New(scripted.Reply("The one try."))
			over := map[string]any{"answer": mergeMaps(map[string]any{"max_attempts": 1}, c.over)}
			if c.over != nil && c.over["on_attempts_exhausted"] == "close" {
				over["prompt"] = map[string]any{"close_reason_text": "Closed after the one try."}
			}
			tu, wk := confirmedTutor(t, w, model, over)
			conv, msg := w.ask(0, tu, "Only one try?")
			p := w.waitProposal(core.AnswerKey(conv, msg, 1))
			w.ok(w.fc.Reject(p.ActionID, "No."))
			eventually(t, "the question skipped", func() bool {
				o := wk.st.outcomes(conv)
				return len(o) > 0 && o[len(o)-1] == store.OutcomeSkipped
			})
			eventually(t, "the conversation held back", func() bool {
				st := wk.sup.Status()
				return len(st) == 1 && len(st[0].Seats) == 1 && st[0].Seats[0].HeldBack == 1
			})
			if n := len(w.calls(tu.actor.ID, toolClose)); n != 0 {
				t.Errorf("conversation_close was called %d times", n)
			}
			if n := len(model.Requests()); n != 1 {
				t.Errorf("the model was called %d times", n)
			}
			logs := w.logs.String()
			deprecated := strings.Contains(logs, `"path":"agent.answer.on_attempts_exhausted"`) &&
				strings.Contains(logs, `"path":"agent.prompt.close_reason_text"`)
			if want := c.name == "close, deprecated"; deprecated != want || strings.Contains(logs, "Closed after the one try.") {
				t.Errorf("the deprecated settings logged: %v, want %v", deprecated, want)
			}
		})
	}
}
