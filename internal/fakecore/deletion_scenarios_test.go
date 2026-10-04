package fakecore

import (
	"context"
	"encoding/json"
	"testing"
)

// assignmentDeletion is an assignment deleted for good (AIShie-Core #73):
// HW1, which nobody has started on, deleted first, leaving the quiz Yuki
// was graded on the last assignment of its component; what the quiz's
// preview counts, and what it says the deletion would be refused, for
// Sato, for his assistant (an agent, which deletes no work), for an agent
// of his that reaches no student, the course tutor and a student; the
// deletion refused for an agent, for one reaching nobody, for a
// confirmation missing, below zero and stale; carried out by Sato,
// replayed, and every call naming the assignment or its work after told
// it was deleted, the assistant's proposal about it among them, which was
// cancelled and emptied; what the feed tells a student, the course tutor
// and the assistant, Yuki's totals among it: the component's, left with
// nothing to go on, and then the course total's, which that leaves
// incomplete, in the order Core writes them, held there by the course's
// components being read before the feed names them; an empty assignment
// its assistant deletes at once; and its proposal to delete another,
// which work reached while it waited.
var assignmentDeletion = scenario{name: "assignment_delete", about: "component_tree, naming the components before the feed does; " +
	"an assignment nobody has started on deleted, leaving a graded one the last of its component; assignment_delete_preview " +
	"by an instructor, his agents (one reaching every student, one reaching none), the course tutor and a student, and of no " +
	"assignment; assignment_delete refused for an agent (people_only), out of scope, without confirm, below zero and stale " +
	"(confirm_stale); carried out, replayed, refused after as deleted, as are reads and writes naming it, and a key of an " +
	"action it emptied (target_deleted); the proposal about it cancelled and emptied (action_list_mine); the feed as a " +
	"student, the course tutor and the agent, with the student's totals written again, the component's (no_total) and then " +
	"the course total's (no_total, incomplete); an unpublished one with nothing in it deleted by the agent; and an agent's " +
	"proposal to delete one that work reached while it waited, which its owner may not decide and which fails when approved",
	run: func(t *testing.T, w world, s *steps) {
		quiz := w.quiz("Quiz 1", true)
		sub := w.handIn(0, quiz)
		w.gradeAndPost(sub, "8")
		seat, asst := w.assistant(map[string]string{permAssignmentWrite: "autonomous", permGradeSubmit: "confirm_required"})
		_, nobody := w.ownerAgent(map[string]string{permAssignmentWrite: "autonomous"})
		sato, mori, yuki := w.as("sato"), w.as("mori"), w.as("yuki")
		of := func(id string, more ...any) map[string]any {
			return inCourseArgs(w, append([]any{"assignment_id", id}, more...)...)
		}
		del := func(id, key string, confirm any) map[string]any {
			return of(id, "confirm", confirm, "idempotency_key", key)
		}
		preview := func(c *mcpClient, name, id string) toolAnswer {
			t.Helper()
			return callAs(t, c, s, name, "assignment_delete_preview", of(id))
		}

		// The course's components, the course total and then the
		// Assignments component, named here before the feed names them:
		// the order Yuki is told her totals in is then held to Core's.
		callAs(t, sato, s, "components", "component_tree", inCourseArgs(w))
		// HW1 goes first, which nobody has started on: Yuki's totals are
		// then the quiz's alone, written again whole, and the quiz is the
		// last assignment of its component.
		hw := w.assignment()
		ph := preview(sato, "hw1_preview", hw)
		wantStatus(t, callAs(t, sato, s, "hw1_deleted", "assignment_delete", del(hw, "delete:"+hw+":1", resultOf(ph, "counts"))),
			"executed")

		p := preview(sato, "preview", quiz)
		wantStatus(t, p, "executed")
		counts, _ := resultOf(p, "counts").(map[string]any)
		preview(asst, "preview_by_an_agent", quiz)
		preview(nobody, "preview_by_an_agent_reaching_nobody", quiz)
		preview(w.agent(), "preview_by_the_course_tutor", quiz)
		preview(yuki, "preview_by_a_student", quiz)
		preview(sato, "preview_of_no_assignment", "0192f3c1-0000-7000-8000-00000000abcd")

		grade := inCourseArgs(w, "submission_id", sub, "score", 9, "idempotency_key", "tool:x:g:1")
		wantStatus(t, callAs(t, asst, s, "grade_proposed", "grade_submit", grade), "proposed")

		callAs(t, asst, s, "deleted_by_an_agent", "assignment_delete", del(quiz, "tool:x:d:1", counts))
		callAs(t, nobody, s, "deleted_by_an_agent_reaching_nobody", "assignment_delete", del(quiz, "tool:x:d:1", counts))
		callAs(t, sato, s, "without_confirm", "assignment_delete", of(quiz, "idempotency_key", "delete:"+quiz+":1"))
		below := make(map[string]any, len(counts))
		for k, v := range counts {
			below[k] = v
		}
		below["files"] = -1
		callAs(t, sato, s, "confirm_below_zero", "assignment_delete", del(quiz, "delete:"+quiz+":2", below))
		// The proposal about it came after the preview: more would go.
		callAs(t, sato, s, "confirm_stale", "assignment_delete", del(quiz, "delete:"+quiz+":3", counts))
		again := preview(sato, "preview_again", quiz)

		cursors := map[*mcpClient]int64{}
		for _, c := range []*mcpClient{yuki, w.agent(), asst} {
			cursors[c] = feedCursor(t, w, c)
		}
		d := callAs(t, sato, s, "deleted", "assignment_delete", del(quiz, "delete:"+quiz+":4", resultOf(again, "counts")))
		wantStatus(t, d, "executed")
		callAs(t, sato, s, "replayed", "assignment_delete", del(quiz, "delete:"+quiz+":4", resultOf(again, "counts")))
		callAs(t, sato, s, "deleted_again", "assignment_delete", del(quiz, "delete:"+quiz+":5", resultOf(again, "counts")))
		callAs(t, sato, s, "get_after", "assignment_get", of(quiz))
		preview(sato, "preview_after", quiz)
		callAs(t, sato, s, "roster_after", "submission_roster", of(quiz))
		callAs(t, sato, s, "graded_after", "grade_submit", inCourseArgs(w, "submission_id", sub, "score", 9,
			"idempotency_key", "grade:"+sub+":1"))
		callAs(t, asst, s, "grade_replayed_after", "grade_submit", grade)
		callAs(t, asst, s, "mine", "action_list_mine", inCourseArgs(w))
		news(t, w, s, "news_for_a_student", yuki, cursors[yuki])
		news(t, w, s, "news_for_the_course_tutor", w.agent(), cursors[w.agent()])
		news(t, w, s, "news_for_the_agent", asst, cursors[asst])

		// An assignment nobody could see, with nothing in it: the agent
		// deletes it at once, and those who write assignments are told.
		empty := w.quiz("Quiz 2", false)
		pe := preview(asst, "empty_preview", empty)
		satoAt := feedCursor(t, w, sato)
		wantStatus(t, callAs(t, asst, s, "empty_deleted_by_an_agent", "assignment_delete",
			del(empty, "tool:x:d:2", resultOf(pe, "counts"))), "executed")
		news(t, w, s, "news_of_the_empty_one", sato, satoAt)

		// The agent's proposal to delete one, which a student's work
		// reaches while it waits: its owner may not decide it, and Mori's
		// approval fails, as approving runs it as the agent's.
		w.setLevel(seat, permAssignmentWrite, "confirm_required")
		third := w.quiz("Quiz 3", true)
		p3 := preview(asst, "third_preview", third)
		prop := callAs(t, asst, s, "proposed_by_an_agent", "assignment_delete", del(third, "tool:x:d:3", resultOf(p3, "counts")))
		wantStatus(t, prop, "proposed")
		id := prop.str("action_id")
		w.handIn(1, third)
		decide := func(key string) map[string]any {
			return inCourseArgs(w, "action_id", id, "decision", "approve", "idempotency_key", key)
		}
		callAs(t, sato, s, "its_owner_approves_after_work_came", "action_decide", decide("decide:"+id))
		callAs(t, mori, s, "approved_after_work_came", "action_decide", decide("decide:"+id+":mori"))
	}}

// feedCursor is where c's feed stands: the next_seq of event_list read
// to its end, not recorded.
func feedCursor(t *testing.T, w world, c *mcpClient) int64 {
	t.Helper()
	var since int64
	for {
		a, err := c.call(context.Background(), "event_list", inCourseArgs(w, "since_seq", since, "limit", 500))
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(t, a, "executed")
		var page struct {
			NextSeq int64 `json:"next_seq"`
			More    bool  `json:"more"`
		}
		raw, _ := json.Marshal(a.Structured["result"])
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		if since = page.NextSeq; !page.More {
			return since
		}
	}
}

// news records what event_list tells c after since: the events, without
// their seq, or the cursor, which number another world's feed, a real
// Core's or the fake's, from other beginnings.
func news(t *testing.T, w world, s *steps, name string, c *mcpClient, since int64) {
	t.Helper()
	a, err := c.call(context.Background(), "event_list", inCourseArgs(w, "since_seq", since, "limit", 500))
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, a, "executed")
	res, _ := a.Structured["result"].(map[string]any)
	evs, _ := res["events"].([]any)
	out := make([]any, 0, len(evs))
	for _, e := range evs {
		ev, _ := e.(map[string]any)
		delete(ev, "seq")
		out = append(out, ev)
	}
	s.list = append(s.list, map[string]any{"step": name, "tool": "event_list", "status": a.status(), "events": out, "more": res["more"]})
}
