package fakecore

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// drafts is conversation.draft and the draft the conversation's views show
// (draft.go), recorded from Core like the rest: what the respondent writes,
// what is kept of it and what passed over, who reads its text, a reader
// waiting for the next version, done, a new attempt, the answer in its
// place, and the refusals, over MCP and REST.
var drafts = scenario{name: "drafts", about: "conversation_draft: the respondent's writes kept and passed over, text and steps kept when left out, " +
	"the opener reading the text at autonomous and only the steps at confirm_required where a decider and the agent's owner read it, " +
	"a reader waiting for the next version (seen_draft_version) and one without it not woken, done and a new attempt, the answer in " +
	"its place, and the refusals: another agent, a person, steps and ids Core refuses, the conversation no longer waiting; over REST too",
	run: func(t *testing.T, w world, s *steps) {
		conv, q1 := w.ask(0, "Q1: when is the lab report due?")
		d := func(more ...any) map[string]any {
			return inCourseArgs(w, append([]any{"conversation_id", conv}, more...)...)
		}
		step := func(kind, state string, target ...string) map[string]any {
			m := map[string]any{"kind": kind, "state": state}
			if len(target) > 0 {
				m["target"] = target[0]
			}
			return m
		}
		get := inCourseArgs(w, "conversation_id", conv)
		yuki := w.as("yuki")

		wantStatus(t, call(t, w, s, "first", "conversation_draft", d("attempt", "a1", "version", 1,
			"steps", []any{step("thinking", "running")})), "executed")
		callAs(t, yuki, s, "the_opener_reads_it", "conversation_get", get)
		call(t, w, s, "text", "conversation_draft", d("attempt", "a1", "version", 2, "text", "The lab report is due",
			"steps", []any{step("thinking", "done"), step("reading_document", "done", "  Syllabus "), step("writing", "running")}))
		call(t, w, s, "a_late_write_passed_over", "conversation_draft", d("attempt", "a1", "version", 1, "text", "late"))
		call(t, w, s, "the_same_version_passed_over", "conversation_draft", d("attempt", "a1", "version", 2, "text", "again"))
		call(t, w, s, "text_alone_keeps_the_steps", "conversation_draft", d("attempt", "a1", "version", 3, "text", "The lab report is due on Friday."))
		callAs(t, yuki, s, "the_opener_reads_the_text", "conversation_messages", get)
		callAs(t, w.as("ken"), s, "another_student_reads_nothing", "conversation_get", get)
		call(t, w, s, "the_respondent_reads_it", "conversation_get", get)

		// At confirm_required the opener sees the steps; whoever would
		// decide the answer, and the agent's owner, the text.
		w.setTutorLevel("confirm_required")
		callAs(t, yuki, s, "the_opener_sees_the_steps_alone", "conversation_get", get)
		callAs(t, w.as("mori"), s, "a_decider_reads_the_text", "conversation_get", get)
		callAs(t, w.as("sato"), s, "the_owner_reads_the_text", "conversation_get", get)
		call(t, w, s, "written_at_confirm_required", "conversation_draft", d("attempt", "a1", "version", 4, "text", "The lab report is due on Friday"))
		w.setTutorLevel("autonomous")

		// Refused.
		callAs(t, w.ownAgent(), s, "another_agent", "conversation_draft", d("attempt", "x", "version", 1))
		callAs(t, yuki, s, "a_person", "conversation_draft", d("attempt", "x", "version", 1))
		call(t, w, s, "a_step_of_no_kind", "conversation_draft", d("attempt", "a1", "version", 9, "steps", []any{step("dreaming", "running")}))
		call(t, w, s, "a_step_of_no_state", "conversation_draft", d("attempt", "a1", "version", 9, "steps", []any{step("thinking", "paused")}))
		call(t, w, s, "a_target_on_two_lines", "conversation_draft", d("attempt", "a1", "version", 9,
			"steps", []any{step("reading_document", "done", "HW1\nnotes")}))
		call(t, w, s, "version_zero", "conversation_draft", d("attempt", "a1", "version", 0))
		call(t, w, s, "an_attempt_too_long", "conversation_draft", d("attempt", strings.Repeat("a", 65), "version", 1))
		call(t, w, s, "an_idempotency_key", "conversation_draft", d("attempt", "a1", "version", 9, "idempotency_key", "k"))
		call(t, w, s, "no_such_conversation", "conversation_draft", inCourseArgs(w, "conversation_id", "0192f3c1-0000-7000-8000-000000000000",
			"attempt", "a1", "version", 9))

		// A reader waits for the draft's next version; without
		// seen_draft_version, a draft wakes nothing.
		read := callAs(t, yuki, s, "messages", "conversation_messages", get)
		seq := lastSeq(t, read)
		waitingCall(t, yuki, s, "a_reader_waits_for_the_next_version", "conversation_messages",
			d("after_seq", seq, "wait_s", 5, "seen_draft_version", 4), newsAfter, func() {
				w.agent().call(context.Background(), "conversation_draft", d("attempt", "a1", "version", 5, "text", "The lab report is due on Friday at noon."))
			})
		waitingCall(t, yuki, s, "a_version_not_seen_answers_at_once", "conversation_messages",
			d("after_seq", seq, "wait_s", 5, "seen_draft_version", 4), 0, nil)
		waitingCall(t, yuki, s, "without_seen_draft_version_a_draft_wakes_nothing", "conversation_messages",
			d("after_seq", seq, "wait_s", 2), newsAfter, func() {
				w.agent().call(context.Background(), "conversation_draft", d("attempt", "a1", "version", 6))
			})
		call(t, w, s, "seen_draft_version_below_zero", "conversation_messages", d("after_seq", seq, "wait_s", 1, "seen_draft_version", -1))

		// Done: the end of another attempt is passed over; its own, with
		// what else it says ignored, clears the draft, and a write of it
		// after is passed over; a new attempt starts another.
		call(t, w, s, "the_end_of_another_attempt", "conversation_draft", d("attempt", "a0", "version", 9, "done", true))
		call(t, w, s, "done", "conversation_draft", d("attempt", "a1", "version", 6, "done", true, "text", "ignored"))
		call(t, w, s, "a_write_after_its_end", "conversation_draft", d("attempt", "a1", "version", 7, "text", "late"))
		callAs(t, yuki, s, "no_draft_after_its_end", "conversation_get", get)
		call(t, w, s, "a_new_attempt", "conversation_draft", d("attempt", "a2", "version", 1, "steps", []any{step("thinking", "running")}))
		callAs(t, yuki, s, "the_new_attempt", "conversation_get", get)

		// The answer takes its place.
		wantStatus(t, call(t, w, s, "answer", "conversation_answer", answer(w, conv, q1, "On Friday at noon.", 1)), "executed")
		callAs(t, yuki, s, "the_answer_in_its_place", "conversation_messages", get)
		call(t, w, s, "after_the_answer", "conversation_draft", d("attempt", "a2", "version", 2, "text", "On Friday"))

		// Over REST: kept, passed over, and refused once the conversation
		// is closed.
		conv2, _ := w.ask(1, "K1: is there a lab this week?")
		path := fmt.Sprintf("/v1/courses/%s/conversations/%s/draft", w.course(), conv2)
		for _, c := range []struct {
			name string
			body map[string]any
		}{
			{"rest", map[string]any{"attempt": "r1", "version": 1, "text": "Yes", "steps": []any{step("writing", "running")}}},
			{"rest_passed_over", map[string]any{"attempt": "r1", "version": 1, "text": "No"}},
		} {
			a, err := w.rest().do(context.Background(), http.MethodPost, path, c.body, "")
			if err != nil {
				t.Fatal(err)
			}
			s.rest(c.name, http.MethodPost, path, c.body, a)
		}
		w.closeAsOpener(conv2, "Thanks!")
		body := map[string]any{"attempt": "r1", "version": 2, "text": "Yes, on Tuesday."}
		a, err := w.rest().do(context.Background(), http.MethodPost, path, body, "")
		if err != nil {
			t.Fatal(err)
		}
		s.rest("rest_closed", http.MethodPost, path, body, a)
	}}
