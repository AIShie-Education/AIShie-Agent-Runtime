package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// stopCancelsTheAnswer: Yuki asks her agent, whose model streams the first
// words of its answer and then stalls, as a slow model may; she withdraws
// the question ("stop" in the chat: conversation.retract of her message).
// The runtime cancels the model's request within seconds, and no answer is
// ever posted: against the pinned Core, whose withdrawn question still
// waits for an answer, and one would be posted, and against a Core whose
// withdrawn question waits for nothing (AIShie-Core #42).
func stopCancelsTheAnswer(t *testing.T, w *world) {
	m := newModel(t, func(fakellm.ChatRequest) fakellm.ChatResponse {
		r := fakellm.Reply("Recursion is a function calling itself on a smaller input, until it reaches a case it answers at once.")
		r.Stall = 5 * time.Minute
		return r
	})
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "yuki-helper", seat: w.own}}})
	rt.waitPolling("yuki-helper")

	conv, q := w.ask(t, w.yuki, w.own.member, "What is recursion, and when does it stop?")
	eventually(t, answerWait, "the model asked", func() bool { return len(m.Requests()) > 0 })
	if w.api.takesDrafts(t) {
		eventually(t, answerWait, "Yuki reading the answer's first words", func() bool {
			d := w.conversationDraft(t, w.yuki, conv)
			return d != nil && d.Text != nil && *d.Text != ""
		})
	}

	withdrawn := time.Now()
	if r := w.api.call(t, http.StatusOK, w.yuki.token, "POST", w.path("/conversation-messages/"+q+"/retract"),
		map[string]any{"reason": "Never mind"}); r.Status != "executed" {
		t.Fatalf("Yuki withdrawing her question: %s", r)
	}
	eventually(t, 10*time.Second, "the model's request cancelled", func() bool { return len(m.GaveUp()) > 0 })
	took := m.GaveUp()[0].Sub(withdrawn)
	t.Logf("the model's request was cancelled %s after the question was withdrawn", took.Round(time.Millisecond))
	if took > 5*time.Second {
		t.Errorf("the model's request was cancelled %s after the question was withdrawn; want within 5 s", took)
	}

	// Nothing is posted, then or after: the runtime records the answer
	// dropped, and polls on without trying it again.
	eventually(t, answerWait, "the answer recorded dropped", func() bool {
		return rt.metric("answers_total", map[string]string{"outcome": "dropped"}) >= 1
	})
	rt.settle("yuki-helper", 3)
	if as := w.answers(t, w.yuki, conv, w.own.member); len(as) != 0 {
		t.Errorf("answers were posted to a question withdrawn: %+v", as)
	}
	if n := rt.metric("answers_total", nil); n != 1 {
		t.Errorf("answers_total = %v; want the one dropped", n)
	}
	if n := len(m.Requests()); n != 1 {
		t.Errorf("the model was asked %d times", n)
	}
}
