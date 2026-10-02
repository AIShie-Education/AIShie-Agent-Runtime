package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// changesRequested is AIShie-Core #68 for an agent the runtime hosts: the
// tutor at confirm_required, Mori, the second instructor, sends its answer
// back for changes with a note. The runtime settles the attempt as sent
// back, with the note, which the next attempt's system prompt states
// plainly, and proposes that attempt (key :2) naming the first, as Core
// keeps it (revises_action_id). Mori sends the revision back in turn: the
// third names the second, is told the second note and remembers the first;
// Mori approves it, and Yuki reads it. The first attempt's bytes sent
// again replay what Mori did, 409 changes_requested. Over MCP, the
// runtime's default, and over REST, where the revision names the proposal
// in the Revises header.
func changesRequested(t *testing.T, w *world) {
	w.setAnswerLevel(t, "confirm_required")
	notes := []string{"Say which chapters the exam covers.", "And say whether notes are allowed."}
	// Each answer says how many of the notes its prompt holds, so that what
	// is read in the end shows it was written for them.
	m := newModel(t, func(req fakellm.ChatRequest) fakellm.ChatResponse {
		n := 0
		for _, note := range notes {
			if strings.Contains(system(req), note) {
				n++
			}
		}
		return fakellm.Reply(fmt.Sprintf("Answer %d: %s", n+1, question(req)))
	})
	told := func(note string) string {
		return fmt.Sprintf("sent it back for changes, asking: %q. Write the answer again, making the changes they asked for.", note)
	}
	// revisions are what each of the tutor's answers in the course
	// revises, as Core keeps it, by action.
	revisions := func(t *testing.T, rt *instance) map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, a := range w.actionsMine(t, rt, "tutor") {
			if a.ActionType == "conversation.answer" {
				out[a.ID] = a.RevisesActionID
			}
		}
		return out
	}

	t.Run("over MCP", func(t *testing.T) {
		rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "tutor", seat: w.tutor}}})
		rt.waitPolling("tutor")
		const q = "What does the final exam cover?"
		conv, msg := w.ask(t, w.yuki, w.tutor.member, q)
		var proposals []string
		for i, note := range notes {
			prev := ""
			if i > 0 {
				prev = proposals[i-1]
			}
			proposed := w.waitPending(t, w.yuki, conv, prev)
			proposals = append(proposals, proposed)
			key := answerKey(conv, msg, i+1)
			eventually(t, answerWait, "the runtime recording the proposal "+key, func() bool {
				at := rt.attempt("tutor", key)
				return at != nil && at.State == store.AttemptProposed && at.ActionID == proposed
			})
			if out := w.decide(t, proposed, "request_changes", note); out != "changes_requested" {
				t.Fatalf("Mori sent the answer back, and it came to %s", out)
			}
			eventually(t, answerWait, "the runtime recording "+key+" sent back, with Mori's note", func() bool {
				at := rt.attempt("tutor", key)
				return at != nil && at.State == store.AttemptChangesRequested && at.Reason == note && at.ActionID == proposed
			})
		}
		last := w.waitPending(t, w.yuki, conv, proposals[len(proposals)-1])
		proposals = append(proposals, last)
		key3 := answerKey(conv, msg, 3)
		eventually(t, answerWait, "the runtime recording the third proposal", func() bool {
			at := rt.attempt("tutor", key3)
			return at != nil && at.State == store.AttemptProposed && at.ActionID == last
		})
		revises := revisions(t, rt)
		if revises[proposals[0]] != "" || revises[proposals[1]] != proposals[0] || revises[proposals[2]] != proposals[1] {
			t.Errorf("the answers %v revise %v in Core; want each the one before", proposals, revises)
		}

		// The second attempt is told what Mori asked first; the third what
		// he asked of the second, remembering the first.
		var second, third bool
		for _, req := range m.Requests() {
			if question(req) != q {
				continue
			}
			s := system(req)
			second = second || strings.Contains(s, told(notes[0])) && !strings.Contains(s, notes[1])
			third = third || strings.Contains(s, told(notes[1])) && strings.Contains(s, fmt.Sprintf("asking: %q. Take it into account.", notes[0]))
		}
		if !second || !third {
			t.Errorf("the revisions' prompts: told the first note %v, the second with the first remembered %v; want both", second, third)
		}

		if out := w.decide(t, last, "approve", ""); out != "executed" {
			t.Fatalf("Mori approved the revision, and it came to %s", out)
		}
		answer := w.waitAnswer(t, w.yuki, conv, w.tutor.member)
		if answer.replyTo() != msg || answer.text() != "Answer 3: "+q {
			t.Errorf("Yuki reads %q in reply to %s; want the third answer, written for both notes, in reply to %s", answer.text(), answer.replyTo(), msg)
		}
		eventually(t, answerWait, "the runtime recording the third answer as posted", func() bool {
			at := rt.attempt("tutor", key3)
			return at != nil && at.State == store.AttemptExecuted && at.PostedMessageID == answer.ID
		})

		// Core holds the first under answer:{x}:{m}:1: the runtime's stored
		// bytes, sent again, replay Mori's request for changes.
		key1 := answerKey(conv, msg, 1)
		r := w.answerAs(t, rt, "tutor", conv, key1)
		if r.HTTP != http.StatusConflict || r.Status != "changes_requested" || !r.Replayed || r.ActionID != proposals[0] {
			t.Errorf("the first attempt sent again under its key: %s; want the request for changes replayed", r)
		}
		rt.settle("tutor", 3)
		if n := len(w.answers(t, w.yuki, conv, w.tutor.member)); n != 1 {
			t.Errorf("%d answers in the conversation; want 1", n)
		}
	})

	t.Run("over REST", func(t *testing.T) {
		rest := map[string]any{"core": map[string]any{"transport": "rest"}}
		rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "tutor", seat: w.tutor, over: rest}}})
		rt.waitPolling("tutor")
		const q = "Which chapters does the resit cover?"
		conv, msg := w.ask(t, w.ken, w.tutor.member, q)
		first := w.waitPending(t, w.ken, conv, "")
		if out := w.decide(t, first, "request_changes", notes[0]); out != "changes_requested" {
			t.Fatalf("Mori sent the answer back, and it came to %s", out)
		}
		second := w.waitPending(t, w.ken, conv, first)
		key1, key2 := answerKey(conv, msg, 1), answerKey(conv, msg, 2)
		eventually(t, answerWait, "the runtime recording the revision", func() bool {
			at := rt.attempt("tutor", key2)
			return at != nil && at.State == store.AttemptProposed && at.ActionID == second
		})
		if at := rt.attempt("tutor", key1); at == nil || at.State != store.AttemptChangesRequested || at.Reason != notes[0] {
			t.Errorf("the first attempt is recorded as %s; want it sent back, with Mori's note", attemptState(at))
		}
		if revises := revisions(t, rt); revises[second] != first {
			t.Errorf("the revision %s revises %q in Core; want %s", second, revises[second], first)
		}
		if out := w.decide(t, second, "approve", ""); out != "executed" {
			t.Fatalf("Mori approved the revision, and it came to %s", out)
		}
		if answer := w.waitAnswer(t, w.ken, conv, w.tutor.member); answer.replyTo() != msg || answer.text() != "Answer 2: "+q {
			t.Errorf("Ken reads %q in reply to %s; want the revision, written for the note, in reply to %s", answer.text(), answer.replyTo(), msg)
		}
	})
}
