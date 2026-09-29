package fakecore

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestDraft: conversation.draft as Core carries it out:
// the respondent writes, a write newer than the draft kept replaces it,
// text and steps left out keep the attempt's, done ends it, a new attempt
// replaces another, and posting the answer clears it; nobody else writes
// one, nor anyone once the conversation no longer waits for its answer.
func TestDraft(t *testing.T) {
	clk := newClock()
	w := newFakeWorld(t, Options{Now: clk.now})
	conv, msg := w.ask(0, "When is HW1 due?")
	draft := func(args map[string]any) toolAnswer {
		t.Helper()
		args["course_id"], args["conversation_id"] = w.co.ID, conv
		return mustCall(t, w.agentC, "conversation_draft", args)
	}
	stored := func(a toolAnswer, want bool, version float64) {
		t.Helper()
		res, _ := a.Structured["result"].(map[string]any)
		if a.status() != "executed" || res["stored"] != want || numberOf(res["version"]) != version {
			t.Fatalf("stored %v version %v, want %v %v: %s", res["stored"], res["version"], want, version, a.Text)
		}
	}
	stored(draft(map[string]any{"attempt": "a1", "version": 1, "steps": []any{map[string]any{"kind": "thinking", "state": "running"}}}), true, 1)
	stored(draft(map[string]any{"attempt": "a1", "version": 1, "text": "late"}), false, 1)
	stored(draft(map[string]any{"attempt": "a1", "version": 3, "text": "HW1 is due"}), true, 3)
	stored(draft(map[string]any{"attempt": "a1", "version": 2, "text": "older"}), false, 3)
	d, ok := w.fc.Draft(conv)
	if !ok || d.Attempt != "a1" || d.Version != 3 || *d.Text != "HW1 is due" || len(d.Steps) != 1 || d.Steps[0].Kind != "thinking" {
		t.Fatalf("the draft kept: %+v %v", d, ok)
	}
	target := "  HW1 instructions "
	stored(draft(map[string]any{"attempt": "a1", "version": 4, "steps": []any{
		map[string]any{"kind": "reading_document", "target": target, "state": "done"}}}), true, 4)
	if d, _ = w.fc.Draft(conv); *d.Text != "HW1 is due" || *d.Steps[0].Target != "HW1 instructions" {
		t.Errorf("text kept, steps replaced and trimmed: %+v", d)
	}

	// A new attempt replaces the draft whole; the end of the old one is
	// passed over; its own end clears it.
	stored(draft(map[string]any{"attempt": "a2", "version": 1}), true, 1)
	if d, _ = w.fc.Draft(conv); d.Text != nil || d.Steps != nil {
		t.Errorf("a new attempt kept another's text or steps: %+v", d)
	}
	stored(draft(map[string]any{"attempt": "a1", "version": 9, "done": true}), false, 1)
	// The version a reader finds after the end: none.
	stored(draft(map[string]any{"attempt": "a2", "version": 1, "done": true}), true, 0)
	if _, ok := w.fc.Draft(conv); ok {
		t.Error("a draft ended with done is still there")
	}
	stored(draft(map[string]any{"attempt": "a2", "version": 5, "text": "after the end"}), false, 0)

	// Refused: a bad step, an agent that is not the respondent.
	wantEnvelope(t, draft(map[string]any{"attempt": "a3", "version": 1, "steps": []any{map[string]any{"kind": "dreaming", "state": "running"}}}),
		"error", "invalid_argument", "")
	own := w.ownAgent()
	a, err := own.call(context.Background(), "conversation_draft", map[string]any{"course_id": w.co.ID, "conversation_id": conv, "attempt": "x", "version": 1})
	if err != nil {
		t.Fatal(err)
	}
	wantEnvelope(t, a, "error", "forbidden", "not_the_respondent")

	// Ten writes a second per conversation.
	for i := range 10 {
		if i == 0 {
			clk.add(2 * time.Second)
		}
		stored(draft(map[string]any{"attempt": "a4", "version": i + 1}), true, float64(i+1))
	}
	tooSoon := draft(map[string]any{"attempt": "a4", "version": 11})
	wantEnvelope(t, tooSoon, "error", "rate_limited", "draft_rate")
	clk.add(time.Second)
	stored(draft(map[string]any{"attempt": "a4", "version": 11, "text": "HW1 is due on Friday."}), true, 11)

	// The answer posted takes the draft's place; a draft after it is
	// refused.
	ans := mustCall(t, w.agentC, "conversation_answer", map[string]any{"course_id": w.co.ID, "conversation_id": conv,
		"in_reply_to_message_id": msg, "body": "HW1 is due on Friday.", "idempotency_key": "k1"})
	if ans.status() != "executed" {
		t.Fatalf("the answer: %s", ans.Text)
	}
	if _, ok := w.fc.Draft(conv); ok {
		t.Error("the draft is still there once the answer is posted")
	}
	wantEnvelope(t, draft(map[string]any{"attempt": "a4", "version": 12}), "error", "conflict", "conversation_not_awaiting")

	// Nothing of it is an action.
	for _, c := range w.fc.Calls() {
		if c.Tool == "conversation_draft" && c.ActionID != "" {
			t.Errorf("a draft was recorded as an action: %+v", c)
		}
	}
}

// TestDraftClearedByProposalAndClose: an answer proposed for approval, and
// the conversation closed, each take the draft away.
func TestDraftClearedByProposalAndClose(t *testing.T) {
	w := newFakeWorld(t, Options{})
	w.ok(w.fc.SetLevel(w.tutorM.ID, "conversation_answer", "confirm_required"))
	conv, msg := w.ask(0, "When is HW1 due?")
	args := map[string]any{"course_id": w.co.ID, "conversation_id": conv, "attempt": "a1", "version": 1, "text": "Friday"}
	mustCall(t, w.agentC, "conversation_draft", args)
	if _, ok := w.fc.Draft(conv); !ok {
		t.Fatal("no draft at confirm_required")
	}
	ans := mustCall(t, w.agentC, "conversation_answer", map[string]any{"course_id": w.co.ID, "conversation_id": conv,
		"in_reply_to_message_id": msg, "body": "Friday.", "idempotency_key": "k1"})
	if ans.status() != "proposed" {
		t.Fatalf("the answer: %s", ans.Text)
	}
	if _, ok := w.fc.Draft(conv); ok {
		t.Error("the draft is still there once the answer is proposed")
	}

	conv2, _ := w.ask(1, "And HW2?")
	args["conversation_id"] = conv2
	mustCall(t, w.agentC, "conversation_draft", args)
	w.ok(w.fc.Close(conv2, w.seats[1].ID, "never mind"))
	if _, ok := w.fc.Draft(conv2); ok {
		t.Error("the draft is still there once the conversation is closed")
	}
}

// TestDraftCostsNoRate: a draft carried out is given back to the caller's
// rate limit; a refused one counts as any call does.
func TestDraftCostsNoRate(t *testing.T) {
	clk := newClock()
	w := newFakeWorld(t, Options{Now: clk.now, RatePerMinute: 1, RateBurst: 5})
	conv, _ := w.ask(0, "When is HW1 due?")
	for i := range 8 {
		clk.add(150 * time.Millisecond)
		a, err := w.agentC.call(context.Background(), "conversation_draft",
			map[string]any{"course_id": w.co.ID, "conversation_id": conv, "attempt": "a1", "version": i + 1})
		if err != nil || a.Status != http.StatusOK || a.status() != "executed" {
			t.Fatalf("draft %d: %v HTTP %d %s", i+1, err, a.Status, a.Body)
		}
	}
	rest := &restClient{base: w.srv.URL, token: w.tutorA.Token}
	r, err := rest.do(context.Background(), http.MethodPost, "/v1/courses/"+w.co.ID+"/conversations/"+conv+"/draft",
		map[string]any{"attempt": "a1", "version": 9, "text": "Friday"}, "")
	if err != nil || r.Status != http.StatusOK || !strings.Contains(string(r.Body), `"stored":true`) {
		t.Fatalf("a draft over REST: %v HTTP %d %s", err, r.Status, r.Body)
	}
}

// numberOf is a JSON number decoded with UseNumber, as a float.
func numberOf(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	}
	return -1
}

// TestWithdrawn: a question its opener withdraws, retracting their latest
// message, waits for no answer: its draft goes, the view says answered, a
// draft is refused, and so is an answer proposed or posted to it, as
// moved_on naming no message. With Options.WithdrawnWaits, as the pinned
// Core, it waits as before: the draft is kept and written, the view says
// awaiting_answer, and the answer is proposed, and posted once approved.
func TestWithdrawn(t *testing.T) {
	for _, waits := range []bool{false, true} {
		w := newFakeWorld(t, Options{WithdrawnWaits: waits})
		w.ok(w.fc.SetLevel(w.tutorM.ID, "conversation_answer", "confirm_required"))
		conv, msg := w.ask(0, "When is HW1 due?")
		draft := func(version int) toolAnswer {
			return mustCall(t, w.agentC, "conversation_draft", map[string]any{"course_id": w.co.ID, "conversation_id": conv,
				"attempt": "a1", "version": version, "text": "Friday"})
		}
		get := func() toolAnswer {
			return mustCall(t, w.agentC, "conversation_get", map[string]any{"course_id": w.co.ID, "conversation_id": conv})
		}
		answer := func(key string) toolAnswer {
			return mustCall(t, w.agentC, "conversation_answer", map[string]any{"course_id": w.co.ID, "conversation_id": conv,
				"in_reply_to_message_id": msg, "body": "Friday.", "idempotency_key": key})
		}
		draft(1)
		w.retract(msg, "never mind")
		_, kept := w.fc.Draft(conv)
		state := get().str("result", "state")
		again := draft(2)
		proposed := answer("k1")
		if !waits {
			if kept || state != "answered" {
				t.Errorf("withdrawn: draft kept %v, state %s", kept, state)
			}
			wantEnvelope(t, again, "error", "conflict", "conversation_not_awaiting")
			wantEnvelope(t, proposed, "failed", "conflict", "moved_on")
			if id := proposed.str("error", "details", "latest_opener_message_id"); id != "" {
				t.Errorf("the refusal names a message to answer: %s", id)
			}
			w.ok(w.fc.SetLevel(w.tutorM.ID, "conversation_answer", "autonomous"))
			wantEnvelope(t, answer("k2"), "failed", "conflict", "moved_on")
			continue
		}
		if !kept || state != "awaiting_answer" || again.status() != "executed" {
			t.Errorf("WithdrawnWaits: draft kept %v, state %s, a draft after: %s", kept, state, again.Text)
		}
		if proposed.status() != "proposed" {
			t.Fatalf("WithdrawnWaits: the answer %s", proposed.Text)
		}
		if g := get(); g.str("result", "state") != "reply_pending_approval" || g.str("result", "pending_reply_action_id") != proposed.str("action_id") {
			t.Errorf("WithdrawnWaits: the answer waiting: %s", g.Text)
		}
		if out, err := w.fc.Approve(proposed.str("action_id")); err != nil || out != "executed" {
			t.Errorf("WithdrawnWaits: approved, %s %v", out, err)
		}
	}
}
