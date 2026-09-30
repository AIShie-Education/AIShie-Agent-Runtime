package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// continued is how the runtime's word to go on with an answer the output
// cap cut off begins (prompt.Continue).
const continued = "[Your answer above stopped at the length limit of one reply."

// longAnswerContinued: Yuki's agent's model is cut off by its output cap
// part way through its answer (finish_reason length). The runtime does not
// ask it again from the start: it gives it the answer so far as its own,
// then its word to go on from where it stops, with no tools, and posts the
// two pieces as one answer, the model's. Where Core takes drafts, Yuki,
// watching the conversation, sees the answer's text grow through the
// continuation, each a prefix of the next, never starting again.
func longAnswerContinued(t *testing.T, w *world) {
	first := "Here is how to go about the lab, step by step. First, read the handout; second, write the loop so that"
	rest := " it ends when the list is empty; third, try it on an empty list and on a long one."
	m := newModel(t, func(req fakellm.ChatRequest) fakellm.ChatResponse {
		if last := req.Messages[len(req.Messages)-1]; last.Role == "user" && strings.HasPrefix(last.Text(), continued) {
			return fakellm.Reply(rest)
		}
		r := fakellm.Reply(first)
		r.Choices[0].FinishReason = "length"
		return r
	}).StreamEvery(200 * time.Millisecond)
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "yuki-helper", seat: w.own}}})
	rt.waitPolling("yuki-helper")

	conv, _ := w.ask(t, w.yuki, w.own.member, "How should I go about the lab this week?")
	drafts := w.api.takesDrafts(t)
	var got watched
	if drafts {
		got = w.watch(t, w.yuki, conv, w.own.member)
	} else {
		got.answer = w.waitAnswer(t, w.yuki, conv, w.own.member)
	}
	if want := first + rest; got.answer.text() != want {
		t.Errorf("Yuki's answer %q, want %q", got.answer.text(), want)
	}

	reqs := m.Requests()
	if len(reqs) != 2 {
		t.Fatalf("the model was asked %d times; want the answer, then its continuation", len(reqs))
	}
	msgs := reqs[1].Messages
	if n := len(msgs); n < 2 || msgs[n-2].Role != "assistant" || msgs[n-2].Text() != first || !strings.HasPrefix(msgs[n-1].Text(), continued) {
		t.Errorf("the continuation's request ends with %+v", msgs[max(len(msgs)-2, 0):])
	}
	// A server not known to take tool_choice none, as this one, is offered
	// no tools instead (llm.ToolNone).
	if tc := strings.Trim(string(reqs[1].ToolChoice), `"`); len(reqs[1].Tools) > 0 && tc != "none" {
		t.Errorf("the continuation is offered %d tools, and tool_choice %q", len(reqs[1].Tools), tc)
	}
	eventually(t, answerWait, "the answer recorded posted", func() bool {
		return rt.metric("answers_total", map[string]string{"outcome": "posted"}) == 1
	})
	if n := rt.metric("budget_exhausted_total", map[string]string{"budget": "truncated"}); n != 0 {
		t.Errorf("budget_exhausted_total{truncated} = %v", n)
	}

	if !drafts {
		return
	}
	var texts []string
	for _, d := range got.drafts {
		if d.Text != nil && *d.Text != "" {
			texts = append(texts, *d.Text)
		}
	}
	grew := false
	for i, text := range texts {
		if !strings.HasPrefix(first+rest, text) || (i > 0 && !strings.HasPrefix(text, texts[i-1])) {
			t.Errorf("text %d %q is not the answer so far, after %q", i, text, texts[max(i-1, 0)])
		}
		grew = grew || len(text) > len(first)
	}
	t.Logf("Yuki saw %d drafts, %d with text", len(got.drafts), len(texts))
	if !grew {
		t.Errorf("no draft showed the answer growing through its continuation: %q", texts)
	}
}
