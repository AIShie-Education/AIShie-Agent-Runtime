package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// TestOwnAgentAnswersEndToEnd is the handout's §2.6: Yuki asks her own
// agent, and its answer arrives under answer:X:M1:1, with the model's
// words, a ledger row per model call and one per answer, and a note in
// the conversation's memory.
func TestOwnAgentAnswersEndToEnd(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Reply("You lost marks for not showing your working."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{})
	wk.waitState("yuki-helper", store.AgentRunning)

	conv, msg := w.ask(0, own, "Why did I lose marks on HW3?")
	got := w.waitAnswers(conv, 1)
	if got[0].IdempotencyKey != core.AnswerKey(conv, msg, 1) || got[0].InReplyTo != msg {
		t.Errorf("answer key %q in reply to %q, want %q to %s", got[0].IdempotencyKey, got[0].InReplyTo, core.AnswerKey(conv, msg, 1), msg)
	}
	if got[0].Body != "You lost marks for not showing your working." {
		t.Errorf("body %q", got[0].Body)
	}
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	req := lastRequest(t, model)
	if n := len(req.Messages); n != 1 || req.Messages[0].Role != llm.RoleUser || req.Messages[0].Parts[0].Text != "Why did I lose marks on HW3?" {
		t.Errorf("the model was given %+v", req.Messages)
	}
	if !strings.Contains(req.System, "Yuki") || !strings.Contains(req.System, "CS101") {
		t.Errorf("the system prompt does not name the asker and the course:\n%s", req.System)
	}

	eventually(t, "the ledger's answer row", func() bool { return len(wk.st.outcomes(conv)) == 1 })
	calls, recs := wk.st.ledger()
	if len(calls) != 1 || calls[0].ConversationID != conv || calls[0].MessageID != msg || calls[0].Input == 0 || calls[0].ID == "" {
		t.Errorf("model calls in the ledger: %+v", calls)
	}
	if r := recs[0]; r.Outcome != store.OutcomePosted || !r.Billable || r.Key != core.AnswerKey(conv, msg, 1) || r.Turns != 1 || r.PromptHash == "" {
		t.Errorf("answer row %+v", r)
	}
	at, err := wk.st.Attempt(context.Background(), "yuki-helper", core.AnswerKey(conv, msg, 1))
	if err != nil || at.State != store.AttemptExecuted || at.PostedMessageID != got[0].ID || at.Kind != kindModel {
		t.Errorf("attempt %+v, %v", at, err)
	}
	notes, err := wk.st.Notes(context.Background(), "yuki-helper", own.seat.ID, conv, 10)
	if err != nil || len(notes) != 1 || notes[0].Kind != store.NoteAnswered || notes[0].MessageID != got[0].ID {
		t.Errorf("memory %+v, %v", notes, err)
	}
}
