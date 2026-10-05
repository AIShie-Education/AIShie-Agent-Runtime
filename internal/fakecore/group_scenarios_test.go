package fakecore

import (
	"testing"
)

// groupSpec is a group a world's group set is made with: its name, and the
// students placed in it (0 Yuki, 1 Ken).
type groupSpec struct {
	name     string
	students []int
}

// groupWork is a group assignment as an agent meets it (the runtime's part
// of the group assignments design): the course's group sets as the course
// tutor, a student's own agent and an instructor's assistant read them, and
// the group assignment as a member's own agent and the course tutor read
// it; a group's work handed in by one member, read by the assistant, by the
// member's own agent and by a tutor listed for the other member alone;
// where the class stands on it; the work graded once, a member adjusted,
// and graded again with the adjustment carried; what grade.submit refuses
// of a group's work and of a student's own; a proposed group grade that a
// newer draft refuses on approval; the members' grades posted and read by
// a member, who is shown their adjustment but not who made it; and the
// peer form, its results and an agent's peer evaluation, which Core
// refuses (people_only).
var groupWork = scenario{name: "group_work",
	about: "group_set_list and group_set_get as the course tutor, a student's own agent and an instructor's assistant " +
		"(members named to a reader of the member list and to a member, work and history to the assistant), and of no set; " +
		"assignment_get of the group assignment (my_group to a member's own agent); a group's work as submission_get, " +
		"submission_list and submission_roster give it to the assistant, a member's own agent and a tutor listed for one " +
		"member; grade_submit on it with an adjustment, again with the adjustment carried, the feed of it, and refused for a " +
		"non-member's adjustment, one without a reason, adjustments on a student's own work and members named wrongly; " +
		"grade_list of the drafts; a proposed group grade that a newer draft refuses on approval; the members' grades posted " +
		"and read by a member (no by_member_id); peer_form_get as a member's own agent, the assistant and the course tutor, " +
		"and of a student's own work; peer_review_results (its shape) and refused to the course tutor; and peer_review_submit " +
		"refused to an agent, without submission_write (denied) and with it (people_only)",
	run: func(t *testing.T, w world, s *steps) {
		set, _ := w.groupSet("Project groups", groupSpec{"Group A", []int{0, 1}}, groupSpec{"Group B", nil})
		project := w.groupAssignment("Project", set)
		sub := w.groupHandIn(0, project)
		hw := w.handIn(0, w.assignment())
		seat, asst := w.assistant(map[string]string{permMemberRead: "autonomous", permGradeSubmit: "autonomous",
			permGradePost: "autonomous"})
		own := w.ownAgent()
		_, lab := w.listedTutor(1)
		sato, mori, yuki, ken := w.as("sato"), w.as("mori"), w.as("yuki"), w.as("ken")
		yukiSeat, kenSeat := w.studentSeat(0), w.studentSeat(1)

		// The sets, as each reads them.
		callAs(t, w.agent(), s, "sets_as_the_course_tutor", "group_set_list", inCourseArgs(w))
		callAs(t, own, s, "sets_as_a_members_own_agent", "group_set_list", inCourseArgs(w))
		callAs(t, asst, s, "sets_as_an_assistant", "group_set_list", inCourseArgs(w))
		callAs(t, w.agent(), s, "set_as_the_course_tutor", "group_set_get", inCourseArgs(w, "set_id", set))
		callAs(t, own, s, "set_as_a_members_own_agent", "group_set_get", inCourseArgs(w, "set_id", set))
		callAs(t, asst, s, "set_as_an_assistant", "group_set_get", inCourseArgs(w, "set_id", set, "include_history", true))
		callAs(t, asst, s, "no_such_set", "group_set_get", inCourseArgs(w, "set_id", "0192f3c1-0000-7000-8000-00000000abcd"))
		callAs(t, own, s, "assignment_as_a_members_own_agent", "assignment_get", inCourseArgs(w, "assignment_id", project))
		callAs(t, w.agent(), s, "assignment_as_the_course_tutor", "assignment_get", inCourseArgs(w, "assignment_id", project))

		// The group's work, as each reads it.
		callAs(t, asst, s, "work_as_an_assistant", "submission_get", inCourseArgs(w, "submission_id", sub))
		callAs(t, own, s, "work_as_a_members_own_agent", "submission_get", inCourseArgs(w, "submission_id", sub))
		callAs(t, lab, s, "work_as_a_tutor_listed_for_one_member", "submission_get", inCourseArgs(w, "submission_id", sub))
		callAs(t, asst, s, "work_listed", "submission_list", inCourseArgs(w, "assignment_id", project))
		callAs(t, own, s, "work_listed_for_a_members_own_agent", "submission_list", inCourseArgs(w, "assignment_id", project))
		callAs(t, asst, s, "roster", "submission_roster", inCourseArgs(w, "assignment_id", project))
		callAs(t, lab, s, "roster_for_a_tutor_listed_for_one_member", "submission_roster", inCourseArgs(w, "assignment_id", project))

		// Graded once, Ken adjusted; and again, his adjustment carried.
		before := feedCursor(t, w, asst)
		grade := func(name, key string, more ...any) toolAnswer {
			t.Helper()
			args := inCourseArgs(w, append([]any{"submission_id", sub, "idempotency_key", key}, more...)...)
			return callAs(t, asst, s, name, "grade_submit", args)
		}
		wantStatus(t, grade("graded", "tool:g:1", "score", "16", "feedback", "Clear and well argued.",
			"adjustments", []any{map[string]any{"student_member_id": kenSeat, "kind": "delta", "points": "-2",
				"reason": "Missed two of the group's meetings."}}), "executed")
		wantStatus(t, grade("graded_again", "tool:g:2", "score", "18"), "executed")
		news(t, w, s, "news_of_the_grades", asst, before)
		grade("adjusting_a_non_member", "tool:g:3", "score", "18", "adjustments", []any{map[string]any{
			"student_member_id": w.tutorSeat(), "kind": "replace", "points": "10", "reason": "Not in the group."}})
		grade("adjustment_without_a_reason", "tool:g:4", "score", "18", "adjustments", []any{map[string]any{
			"student_member_id": yukiSeat, "kind": "delta", "points": "1"}})
		grade("members_named_wrongly", "tool:g:5", "score", "18", "members", []any{yukiSeat})
		callAs(t, asst, s, "adjusting_a_students_own_work", "grade_submit", inCourseArgs(w, "submission_id", hw, "score", "90",
			"adjustments", []any{map[string]any{"student_member_id": yukiSeat, "kind": "delta", "points": "1", "reason": "Extra."}},
			"idempotency_key", "tool:g:6"))
		callAs(t, asst, s, "drafts", "grade_list", inCourseArgs(w, "assignment_id", project))

		// A proposed group grade, and a newer draft entered while it
		// waits: approving it is refused.
		w.setLevel(seat, permGradeSubmit, "confirm_required")
		prop := grade("proposed", "tool:g:7", "score", "19", "adjustments", []any{map[string]any{"student_member_id": kenSeat,
			"kind": "none"}})
		wantStatus(t, prop, "proposed")
		id := prop.str("action_id")
		newer := callAs(t, sato, s, "newer_draft_by_a_person", "grade_submit", inCourseArgs(w, "submission_id", sub, "score", "17",
			"idempotency_key", "grade:"+sub+":1"))
		wantStatus(t, newer, "executed")
		callAs(t, mori, s, "approved_after_a_newer_draft", "action_decide", inCourseArgs(w, "action_id", id, "decision", "approve",
			"idempotency_key", "decide:"+id))
		callAs(t, asst, s, "mine", "action_list_mine", inCourseArgs(w))

		// Posted, and read by the members.
		var posted []string
		list, _ := resultOf(newer, "member_grades").([]any)
		for _, g := range list {
			m, _ := g.(map[string]any)
			gid, _ := m["grade_id"].(string)
			posted = append(posted, gid)
		}
		if len(posted) != 2 {
			t.Fatalf("member_grades: %v", resultOf(newer, "member_grades"))
		}
		w.post(posted...)
		callAs(t, yuki, s, "grades_as_a_member", "grade_list", inCourseArgs(w, "assignment_id", project))
		callAs(t, ken, s, "adjusted_grade_as_its_member", "grade_get", inCourseArgs(w, "grade_id", posted[1]))
		callAs(t, own, s, "grades_as_a_members_own_agent", "grade_list", inCourseArgs(w, "assignment_id", project))
		callAs(t, asst, s, "adjusted_grade_as_an_assistant", "grade_get", inCourseArgs(w, "grade_id", posted[1]))

		// Peer evaluation.
		w.peerForm(project)
		callAs(t, own, s, "peer_form_as_a_members_own_agent", "peer_form_get", inCourseArgs(w, "assignment_id", project))
		callAs(t, asst, s, "peer_form_as_an_assistant", "peer_form_get", inCourseArgs(w, "assignment_id", project))
		callAs(t, w.agent(), s, "peer_form_as_the_course_tutor", "peer_form_get", inCourseArgs(w, "assignment_id", project))
		callAs(t, asst, s, "peer_form_of_a_students_own_work", "peer_form_get", inCourseArgs(w, "assignment_id", w.assignment()))
		callShape(t, asst, s, "peer_results", "peer_review_results", inCourseArgs(w, "assignment_id", project))
		callAs(t, w.agent(), s, "peer_results_as_the_course_tutor", "peer_review_results", inCourseArgs(w, "assignment_id", project))
		sheet := inCourseArgs(w, "assignment_id", project, "entries", []any{map[string]any{"student_member_id": kenSeat, "share": 100}},
			"idempotency_key", "tool:p:1")
		callAs(t, own, s, "peer_review_by_an_agent_without_submission_write", "peer_review_submit", sheet)
		w.setLevel(w.ownSeat(), permSubmissionWrite, "confirm_required")
		sheet["idempotency_key"] = "tool:p:2"
		callAs(t, own, s, "peer_review_by_an_agent", "peer_review_submit", sheet)
	}}
