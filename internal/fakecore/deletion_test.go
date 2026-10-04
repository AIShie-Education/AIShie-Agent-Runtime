package fakecore

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// An assignment deleted for good as Core deletes it (deletion.go), beyond
// what the assignment_delete fixture holds the fake to: what goes and what
// stays of the canned HW1, the feed's news of it, the seats' lists of it,
// an archived course, and proposals of the deletion approved as their
// proposer's, held to what they confirmed.

// counts is the preview's counts for c of the assignment, and its
// refusal, "" for none.
func previewOf(t *testing.T, w *fakeWorld, c *mcpClient, assignment string) (map[string]any, string) {
	t.Helper()
	a := mustCall(t, c, "assignment_delete_preview", inCourseArgs(w, "assignment_id", assignment))
	wantEnvelope(t, a, "executed", "", "")
	res, _ := a.Structured["result"].(map[string]any)
	counts, _ := res["counts"].(map[string]any)
	return counts, a.str("result", "refusal")
}

func deleteArgs(w *fakeWorld, assignment, key string, confirm map[string]any) map[string]any {
	return inCourseArgs(w, "assignment_id", assignment, "confirm", confirm, "idempotency_key", key)
}

// count is a count of the preview's, as a number.
func count(counts map[string]any, key string) int {
	n, _ := counts[key].(json.Number).Int64()
	return int(n)
}

// ids is the ids of a list's entries.
func ids(entries []any) []string {
	var out []string
	for _, e := range entries {
		m, _ := e.(map[string]any)
		id, _ := m["id"].(string)
		out = append(out, id)
	}
	return out
}

// totalsTold is the totals the student's feed tells of after since, in
// order: each grade.total_updated's payload.
func totalsTold(t *testing.T, w *fakeWorld, c *mcpClient, since int64) []any {
	t.Helper()
	var out []any
	for _, e := range list(mustCall(t, c, "event_list", inCourseArgs(w, "since_seq", since)), "events") {
		if m, _ := e.(map[string]any); m["type"] == "grade.total_updated" {
			out = append(out, m["payload"])
		}
	}
	return out
}

// TestDeleteHW1: the canned HW1, with Yuki's work graded and posted and a
// draft of Sato's on it, goes with its work and grades, a grader listed
// for it alone reaching no assignment after; its instructions stay in the
// course, for Sato; its earlier news goes from the feed; Yuki's totals are
// written again, the Assignments component's and then the course
// total's, and she is told it was deleted, as the course tutor is.
func TestDeleteHW1(t *testing.T) {
	w := newFakeWorld(t, Options{})
	hw := w.co.AssignmentID
	work, err := w.fc.AddWork(w.co.ID, w.seats[0].ID, "My answers.", "90")
	w.ok(err)
	sato, yuki := w.as("sato"), w.as("yuki")
	wantEnvelope(t, mustCall(t, sato, "grade_submit", gradeArgs(w, work.SubmissionID, "g1", 95)), "executed", "", "")
	grader, err := w.fc.Seat(w.fc.AddPerson("Aki").ID, w.co.ID, SeatOptions{Preset: "grader", ListedAssignments: []string{hw}})
	w.ok(err)
	before := len(w.fc.Documents(w.co.ID))

	counts, refusal := previewOf(t, w, sato, hw)
	if refusal != "" || count(counts, "submissions") != 1 || count(counts, "handed_in") != 1 || count(counts, "grades") != 2 ||
		count(counts, "posted") != 1 || count(counts, "totals") != 1 || count(counts, "files") != 0 || count(counts, "proposals") != 0 {
		t.Fatalf("preview: %v, refusal %q", counts, refusal)
	}
	if n := len(list(mustCall(t, sato, "event_list", inCourseArgs(w)), "events")); n == 0 {
		t.Fatal("no news before")
	}
	cursor := feedCursor(t, w, yuki)
	d := mustCall(t, sato, "assignment_delete", deleteArgs(w, hw, "d1", counts))
	wantEnvelope(t, d, "executed", "", "")
	res, _ := d.Structured["result"].(map[string]any)
	removed, _ := res["removed"].(map[string]any)
	if res["deleted"] != true || res["title"] != "HW1" || count(removed, "submissions") != 1 || count(removed, "grades") != 2 ||
		count(res, "snapshots") != 2 || count(res, "proposals_cancelled") != 0 || count(res, "files_queued") != 0 {
		t.Errorf("result: %v", res)
	}

	if got := ids(list(mustCall(t, sato, "assignment_list", inCourseArgs(w)), "assignments")); slices.Contains(got, hw) {
		t.Errorf("assignment_list still lists HW1: %v", got)
	}
	if n := len(list(mustCall(t, sato, "submission_list", inCourseArgs(w)), "submissions")); n != 0 {
		t.Errorf("%d submissions left", n)
	}
	if n := len(list(mustCall(t, sato, "grade_list", inCourseArgs(w)), "grades")); n != 0 {
		t.Errorf("%d grades left", n)
	}
	if len(w.fc.Documents(w.co.ID)) != before {
		t.Error("a document of the course went with it")
	}
	wantEnvelope(t, mustCall(t, sato, "document_get", inCourseArgs(w, "document_id", w.co.InstructionsID)), "executed", "", "")
	for _, e := range list(mustCall(t, sato, "event_list", inCourseArgs(w)), "events") {
		if m, _ := e.(map[string]any); m["assignment_id"] == hw {
			t.Errorf("news of it stays: %v", m)
		}
	}
	if m := w.fc.members[grader.ID]; len(m.assignments) != 0 || m.assignmentScope != scopeListed {
		t.Errorf("the grader listed for it lists %v (%s)", m.assignments, m.assignmentScope)
	}
	var types []string
	for _, e := range list(mustCall(t, yuki, "event_list", inCourseArgs(w, "since_seq", cursor)), "events") {
		m, _ := e.(map[string]any)
		types = append(types, m["type"].(string))
	}
	if !slices.Equal(types, []string{"grade.total_updated", "grade.total_updated", "assignment.deleted"}) {
		t.Errorf("Yuki is told %v", types)
	}
	// HW1 was the component's last assignment: it is left complete with
	// nothing to go on, and the course total, with nothing from it, not
	// complete.
	co := w.fc.courses[w.co.ID]
	want := []any{
		map[string]any{"component_id": co.bucket.id, "complete": true, "no_total": true},
		map[string]any{"component_id": co.rootComponent.id, "complete": false, "no_total": true},
	}
	if got := totalsTold(t, w, yuki, cursor); !reflect.DeepEqual(got, want) {
		t.Errorf("Yuki's totals are written %v, want %v", got, want)
	}
	a := mustCall(t, w.agent(), "assignment_get", inCourseArgs(w, "assignment_id", hw))
	wantEnvelope(t, a, "error", codeNotFound, deleteDeleted)
	if a.str("error", "details", "by_action_id") != d.str("action_id") {
		t.Errorf("by_action_id: %s", a.Text)
	}
}

// TestDeleteTotals: a student's totals worked out again as Core's
// snapshot writes them, nearest first, each only when its working
// changed. A quiz Yuki was graded on goes, then HW1, which nobody started
// on, the last assignment of the Assignments component. The quiz's
// deletion leaves the component, and so the course total, with nothing to
// go on, HW1 ungraded: both are written again, neither complete. HW1's
// leaves the component complete with nothing in it, and the course total
// as it was, with nothing to go on: only the component's is written.
func TestDeleteTotals(t *testing.T) {
	w := newFakeWorld(t, Options{})
	co := w.fc.courses[w.co.ID]
	quiz, err := w.fc.AddAssignment(w.co.ID, "Quiz", "10", true)
	w.ok(err)
	sub, err := w.fc.HandIn(w.co.ID, w.seats[0].ID, quiz, "Mine.")
	w.ok(err)
	_, err = w.fc.PostGrade(sub, "8")
	w.ok(err)
	sato, yuki := w.as("sato"), w.as("yuki")
	for _, c := range []struct {
		assignment string
		want       []any
	}{
		{quiz, []any{
			map[string]any{"component_id": co.bucket.id, "complete": false, "no_total": true},
			map[string]any{"component_id": co.rootComponent.id, "complete": false, "no_total": true},
		}},
		{w.co.AssignmentID, []any{
			map[string]any{"component_id": co.bucket.id, "complete": true, "no_total": true},
		}},
	} {
		cursor := feedCursor(t, w, yuki)
		counts, _ := previewOf(t, w, sato, c.assignment)
		d := mustCall(t, sato, "assignment_delete", deleteArgs(w, c.assignment, "d:"+c.assignment, counts))
		wantEnvelope(t, d, "executed", "", "")
		res, _ := d.Structured["result"].(map[string]any)
		if n := count(res, "snapshots"); n != len(c.want) {
			t.Errorf("%s: %d snapshots, want %d", res["title"], n, len(c.want))
		}
		if got := totalsTold(t, w, yuki, cursor); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Yuki's totals are written %v, want %v", res["title"], got, c.want)
		}
	}
}

// TestDeleteArchived: an archived course's assignment is not deleted, and
// its preview says so.
func TestDeleteArchived(t *testing.T) {
	w := newFakeWorld(t, Options{})
	sato := w.as("sato")
	counts, _ := previewOf(t, w, sato, w.co.AssignmentID)
	w.ok(w.fc.ArchiveCourse(w.co.ID))
	if _, refusal := previewOf(t, w, sato, w.co.AssignmentID); refusal != reasonCourseArchived {
		t.Errorf("refusal %q", refusal)
	}
	wantEnvelope(t, mustCall(t, sato, "assignment_delete", deleteArgs(w, w.co.AssignmentID, "d1", counts)), actDenied, codeForbidden,
		reasonCourseArchived)
}

// TestDeleteUnreleased: one nobody could see is news for those who write
// assignments, not for a student, nor for the course tutor; and its
// deletion works no totals out again.
func TestDeleteUnreleased(t *testing.T) {
	w := newFakeWorld(t, Options{})
	quiz, err := w.fc.AddAssignment(w.co.ID, "Quiz", "10", false)
	w.ok(err)
	_, err = w.fc.AddWork(w.co.ID, w.seats[0].ID, "My answers.", "90")
	w.ok(err)
	sato, mori, yuki := w.as("sato"), w.as("mori"), w.as("yuki")
	cursors := map[*mcpClient]int64{}
	for _, c := range []*mcpClient{mori, yuki, w.agent()} {
		cursors[c] = feedCursor(t, w, c)
	}
	counts, _ := previewOf(t, w, sato, quiz)
	if count(counts, "totals") != 0 {
		t.Errorf("an unpublished one rewrites totals: %v", counts)
	}
	wantEnvelope(t, mustCall(t, sato, "assignment_delete", deleteArgs(w, quiz, "d1", counts)), "executed", "", "")
	for c, want := range map[*mcpClient]int{mori: 1, yuki: 0, w.agent(): 0} {
		evs := list(mustCall(t, c, "event_list", inCourseArgs(w, "since_seq", cursors[c])), "events")
		if len(evs) != want {
			t.Errorf("told %v, want %d", evs, want)
		}
	}
}

// TestDeleteProposed: a person's deletion at confirm_required is a
// proposal, approved as theirs: refused when more would go than they
// confirmed, carried out under the proposal when nothing was added; and
// an agent's proposal to delete one nobody has started on, its owner
// approves.
func TestDeleteProposed(t *testing.T) {
	w := newFakeWorld(t, Options{})
	w.ok(w.fc.SetLevel(w.sato.ID, permAssignmentWrite, "confirm_required"))
	sato := w.as("sato")
	quiz, err := w.fc.AddAssignment(w.co.ID, "Quiz", "10", true)
	w.ok(err)
	counts, _ := previewOf(t, w, sato, quiz)
	p := mustCall(t, sato, "assignment_delete", deleteArgs(w, quiz, "d1", counts))
	wantEnvelope(t, p, actProposed, "", "")
	_, err = w.fc.HandIn(w.co.ID, w.seats[1].ID, quiz, "Mine.")
	w.ok(err)
	if out, err := w.fc.Approve(p.str("action_id")); err != nil || out != actFailed {
		t.Fatalf("approved: %s %v", out, err)
	}
	if e := storedError(w.fc.actions[p.str("action_id")].result); e == nil || e.Code != codeConflict || e.Details["reason"] != deleteConfirmStale {
		t.Errorf("failed with %v", e)
	}

	counts, _ = previewOf(t, w, sato, quiz)
	p = mustCall(t, sato, "assignment_delete", deleteArgs(w, quiz, "d2", counts))
	wantEnvelope(t, p, actProposed, "", "")
	if out, err := w.fc.Approve(p.str("action_id")); err != nil || out != actExecuted {
		t.Fatalf("approved: %s %v", out, err)
	}
	a := mustCall(t, sato, "assignment_get", inCourseArgs(w, "assignment_id", quiz))
	wantEnvelope(t, a, "error", codeNotFound, deleteDeleted)
	if a.str("error", "details", "by_action_id") != p.str("action_id") {
		t.Errorf("deleted by %s, not by the proposal", a.Text)
	}

	_, asst := w.assistant(map[string]string{permAssignmentWrite: "confirm_required"})
	empty, err := w.fc.AddAssignment(w.co.ID, "Empty", "10", false)
	w.ok(err)
	counts, refusal := previewOf(t, w, asst, empty)
	if refusal != "" {
		t.Fatalf("refusal %q", refusal)
	}
	p = mustCall(t, asst, "assignment_delete", deleteArgs(w, empty, "tool:x:d:1", counts))
	wantEnvelope(t, p, actProposed, "", "")
	w.ok(w.fc.SetLevel(w.sato.ID, permAssignmentWrite, "autonomous"))
	decided := mustCall(t, sato, "action_decide", inCourseArgs(w, "action_id", p.str("action_id"), "decision", "approve",
		"idempotency_key", "decide:1"))
	if decided.str("result", "outcome") != actExecuted || decided.Structured["result"].(map[string]any)["by_owner"] != true {
		t.Errorf("its owner approved: %s", decided.Text)
	}
}

// TestDeleteReach: deleting a published assignment reaches every student
// whose totals it works out again, not only those whose work goes: a seat
// listed for Ken, who handed in work on it, may delete it until Yuki has
// totals, and not after.
func TestDeleteReach(t *testing.T) {
	w := newFakeWorld(t, Options{})
	quiz, err := w.fc.AddAssignment(w.co.ID, "Quiz", "10", true)
	w.ok(err)
	_, err = w.fc.HandIn(w.co.ID, w.seats[1].ID, quiz, "Mine.")
	w.ok(err)
	aki := w.fc.AddPerson("Aki")
	_, err = w.fc.Seat(aki.ID, w.co.ID, SeatOptions{Preset: "ta", StudentScope: scopeListed, ListedStudents: []string{w.seats[1].ID},
		Perms: map[string]string{permAssignmentWrite: "autonomous"}})
	w.ok(err)
	c := w.client(aki.Token)
	if _, refusal := previewOf(t, w, c, quiz); refusal != "" {
		t.Fatalf("refusal %q with Ken's work alone", refusal)
	}
	_, err = w.fc.AddWork(w.co.ID, w.seats[0].ID, "My answers.", "90")
	w.ok(err)
	counts, refusal := previewOf(t, w, c, quiz)
	if refusal != reasonStudentScope || count(counts, "totals") != 1 {
		t.Fatalf("refusal %q, counts %v, once Yuki has totals", refusal, counts)
	}
	wantEnvelope(t, mustCall(t, c, "assignment_delete", deleteArgs(w, quiz, "d1", counts)), actDenied, codeForbidden, reasonStudentScope)
}
