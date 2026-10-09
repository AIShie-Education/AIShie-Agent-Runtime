package fakecore

import (
	"slices"
	"testing"
	"time"
)

// Group assignments as Core b5d6b43 has them (groups.go), beyond what the
// group_work fixture holds the fake to: the bounds of a member's adjusted
// score, an adjustment taken away, a group grade refused once one is
// posted, who reaches a group's work and who grades it, a student in no
// group, the peer form's window and how it counts, the refusals of peer
// evaluation, and deleting a group assignment.

// groupWorld is a world with a group set, Group A of Yuki and Ken and
// Group B of Aiko, a third student; Ren, a fourth, in no group; the group
// assignment Project worth 20 points; and Group A's work on it, handed in
// by Yuki.
type groupWorld struct {
	*fakeWorld
	set, groupA, groupB, project, work string
	aiko, ren                          Member
	aikoC                              *mcpClient
}

func newGroupWorld(t *testing.T, o Options) *groupWorld {
	t.Helper()
	w := &groupWorld{fakeWorld: newFakeWorld(t, o)}
	seat := func(name string) (Member, *mcpClient) {
		t.Helper()
		p := w.fc.AddPerson(name)
		m, err := w.fc.Seat(p.ID, w.co.ID, SeatOptions{Preset: "student"})
		w.ok(err)
		return m, w.client(p.Token)
	}
	w.aiko, w.aikoC = seat("Aiko")
	w.ren, _ = seat("Ren")
	set, err := w.fc.AddGroupSet(w.co.ID, "Project groups", GroupSpec{Name: "Group A", Members: []string{w.seats[0].ID, w.seats[1].ID}},
		GroupSpec{Name: "Group B", Members: []string{w.aiko.ID}})
	w.ok(err)
	w.set, w.groupA, w.groupB = set.ID, set.GroupIDs[0], set.GroupIDs[1]
	w.project, err = w.fc.AddGroupAssignment(w.co.ID, "Project", "20", w.set)
	w.ok(err)
	w.work, err = w.fc.HandInGroupWork(w.co.ID, w.seats[0].ID, w.project, "Our project.")
	w.ok(err)
	return w
}

// adjust is an adjustment as grade_submit takes one.
func adjust(member, kind string, points any, reason string) map[string]any {
	a := map[string]any{"student_member_id": member, "kind": kind}
	if points != nil {
		a["points"] = points
	}
	if reason != "" {
		a["reason"] = reason
	}
	return a
}

// memberScores are a group grade's member grades' scores, by member.
func memberScores(a toolAnswer) map[string]string {
	out := map[string]string{}
	for i := range list(a, "member_grades") {
		n := []string{"result", "member_grades", string(rune('0' + i))}
		out[a.str(append(n, "student_member_id")...)] = a.num(append(n, "score")...)
	}
	return out
}

// TestGroupGradeAdjustments: a member's score from the group's, replaced
// or moved by a delta, held to zero and to the points possible unless the
// grade allows extra; the call's own refusals of what an adjustment says;
// an adjustment taken away (kind none); and a new group grade refused once
// a member's grade from the last is posted.
func TestGroupGradeAdjustments(t *testing.T) {
	w := newGroupWorld(t, Options{})
	sato := w.as("sato")
	yuki, ken := w.seats[0].ID, w.seats[1].ID
	n := 0
	grade := func(score any, more ...any) toolAnswer {
		t.Helper()
		n++
		return mustCall(t, sato, "grade_submit", gradeArgs(w.fakeWorld, w.work, "g"+string(rune('a'+n)), score, more...))
	}

	g := grade("15", "adjustments", []any{adjust(yuki, "replace", "18", "Wrote the analysis alone."), adjust(ken, "delta", -1, "Late.")})
	wantEnvelope(t, g, "executed", "", "")
	if got := memberScores(g); got[yuki] != "18" || got[ken] != "14" || g.str("result", "grade_id") != "" {
		t.Fatalf("member grades %v: %s", got, g.Text)
	}
	if g.str("result", "member_grades", "0", "adjustment", "by_member_id") != w.sato.ID {
		t.Errorf("the adjustment does not say who made it to its maker: %s", g.Text)
	}
	wantEnvelope(t, grade("15", "adjustments", []any{adjust(ken, "delta", "-16", "Absent.")}), "failed", codeFailedPrecondition,
		"adjusted_below_zero")
	wantEnvelope(t, grade("15", "adjustments", []any{adjust(ken, "delta", "6", "Extra.")}), "failed", codeFailedPrecondition,
		"adjusted_above_points")
	wantEnvelope(t, grade("15", "adjustments", []any{adjust(ken, "delta", "6", "Extra.")}, "allow_extra", true), "executed", "", "")
	for _, bad := range [][]any{
		{adjust(ken, "none", "1", "")},
		{adjust(ken, "replace", "-1", "Never.")},
		{adjust(ken, "half", "1", "Why.")},
		{adjust(ken, "delta", "1", "Twice."), adjust(ken, "delta", "2", "Twice.")},
	} {
		wantEnvelope(t, grade("15", "adjustments", bad), "error", codeInvalidArgument, "bad_adjustment")
	}
	// Carried: Yuki's replace, and Ken's delta of the last call, which
	// takes him above the points possible again; then taken away.
	wantEnvelope(t, grade("16"), "failed", codeFailedPrecondition, "adjusted_above_points")
	carried := grade("16", "allow_extra", true)
	if got := memberScores(carried); got[yuki] != "18" || got[ken] != "22" {
		t.Errorf("carried: %v", got)
	}
	none := grade("16", "adjustments", []any{adjust(yuki, "none", nil, ""), adjust(ken, "none", nil, "")})
	if got := memberScores(none); got[yuki] != "16" || got[ken] != "16" || none.str("result", "member_grades", "0", "adjustment", "kind") != "" {
		t.Errorf("taken away: %v %s", got, none.Text)
	}
	var drafts []string
	for _, x := range list(none, "member_grades") {
		drafts = append(drafts, x.(map[string]any)["grade_id"].(string))
	}
	w.ok(w.fc.PostGrades(drafts...))
	posted := grade("17")
	wantEnvelope(t, posted, "failed", codeFailedPrecondition, "group_grade_posted")
	if id := posted.str("error", "details", "grade_id"); !slices.Contains(drafts, id) {
		t.Errorf("group_grade_posted names %q, not a posted grade", id)
	}
	// On a student's own work, no group's adjustments nor members.
	own, err := w.fc.HandIn(w.co.ID, yuki, w.co.AssignmentID, "Mine.")
	w.ok(err)
	wantEnvelope(t, mustCall(t, sato, "grade_submit", gradeArgs(w.fakeWorld, own, "own", "90", "members", []any{yuki})), "failed",
		codeFailedPrecondition, "not_a_group_assignment")
	if _, err := w.fc.HandIn(w.co.ID, yuki, w.project, "Mine."); err == nil {
		t.Error("a student's own work was handed in to a group assignment")
	}
}

// TestGroupWorkReach: a group's work is read by whoever reaches any of
// its members, and graded only by whoever reaches every one; a student of
// another group reads none of it, and is told nothing of its members; a
// student in no group stands no_group on the roster.
func TestGroupWorkReach(t *testing.T) {
	w := newGroupWorld(t, Options{})
	seat, lab := w.listedTutor(1)
	w.ok(w.fc.SetLevel(seat, permGradeSubmit, "confirm_required"))
	got := mustCall(t, lab, "submission_get", inCourseArgs(w.fakeWorld, "submission_id", w.work))
	wantEnvelope(t, got, "executed", "", "")
	if got.str("result", "members", "0", "display_name") != "" || got.str("result", "student_member_id") != "" {
		t.Errorf("a tutor listed for Ken, reading the work: %s", got.Text)
	}
	if l := list(mustCall(t, lab, "submission_list", inCourseArgs(w.fakeWorld, "assignment_id", w.project)), "submissions"); len(l) != 1 {
		t.Errorf("a tutor listed for Ken lists %d works", len(l))
	}
	wantEnvelope(t, mustCall(t, lab, "grade_submit", gradeArgs(w.fakeWorld, w.work, "lab", "10")), "denied", codeForbidden,
		reasonStudentScope)

	wantEnvelope(t, mustCall(t, w.aikoC, "submission_get", inCourseArgs(w.fakeWorld, "submission_id", w.work)), "denied", codeForbidden,
		reasonStudentScope)
	if l := list(mustCall(t, w.aikoC, "submission_list", inCourseArgs(w.fakeWorld)), "submissions"); len(l) != 0 {
		t.Errorf("Aiko lists Group A's work: %v", l)
	}
	sets := mustCall(t, w.aikoC, "group_set_list", inCourseArgs(w.fakeWorld))
	if sets.str("result", "sets", "0", "my_group_id") != w.groupB || sets.str("result", "sets", "0", "groups", "0", "members", "0", "member_id") != "" ||
		sets.str("result", "sets", "0", "groups", "1", "members", "0", "display_name") != "Aiko" {
		t.Errorf("Aiko's group sets: %s", sets.Text)
	}
	set := mustCall(t, w.aikoC, "group_set_get", inCourseArgs(w.fakeWorld, "set_id", w.set))
	if set.str("result", "groups", "0", "work", "0", "submission_id") != "" || set.num("result", "unassigned_count") != "" {
		t.Errorf("Aiko is shown Group A's work, or the unassigned: %s", set.Text)
	}

	roster := mustCall(t, w.as("sato"), "submission_roster", inCourseArgs(w.fakeWorld, "assignment_id", w.project))
	states := map[string]string{}
	for _, e := range list(roster, "students") {
		e := e.(map[string]any)
		states[e["student_member_id"].(string)] = e["state"].(string)
	}
	if states[w.seats[0].ID] != "submitted" || states[w.seats[1].ID] != "submitted" || states[w.aiko.ID] != "not_started" ||
		states[w.ren.ID] != "no_group" {
		t.Errorf("the roster's states: %v", states)
	}
	if groups := list(roster, "groups"); len(groups) != 2 {
		t.Errorf("the roster's groups: %v", groups)
	}
	sato := mustCall(t, w.as("sato"), "group_set_get", inCourseArgs(w.fakeWorld, "set_id", w.set))
	if sato.num("result", "unassigned_count") != "1" || sato.str("result", "unassigned", "0", "display_name") != "Ren" {
		t.Errorf("Sato's set: %s", sato.Text)
	}
}

// TestGroupPeer: the peer form's window, open to a group once it has
// handed in and closed after closes_at; how it counts in a group grade
// (window_open, then counted, nobody rated and nobody moved); its results'
// refusals; and peer evaluation refused to an agent (people_only) and not
// carried out for a person.
func TestGroupPeer(t *testing.T) {
	clk := newClock()
	w := newGroupWorld(t, Options{Now: clk.now})
	sato := w.as("sato")
	w.ok(w.fc.SetPeerForm(w.project, PeerForm{Kind: "share", Opens: "on_hand_in", ClosesAt: clk.now().Add(time.Hour), Weight: 20}))
	window := func(c *mcpClient) string {
		t.Helper()
		return mustCall(t, c, "peer_form_get", inCourseArgs(w.fakeWorld, "assignment_id", w.project)).str("result", "task", "window", "state")
	}
	if got := window(w.as("yuki")); got != windowOpen {
		t.Errorf("Group A's window, handed in: %s", got)
	}
	if got := window(w.aikoC); got != windowNotOpen {
		t.Errorf("Group B's window, not handed in: %s", got)
	}
	if got := window(w.as("sato")); got != "" {
		t.Errorf("Sato, in no group, has a task: %s", got)
	}
	g := mustCall(t, sato, "grade_submit", gradeArgs(w.fakeWorld, w.work, "g1", "16"))
	if g.str("result", "peer") != "window_open" {
		t.Errorf("graded while the window is open: %s", g.Text)
	}
	clk.add(2 * time.Hour)
	if got := window(w.as("yuki")); got != windowClosed {
		t.Errorf("Group A's window, past closes_at: %s", got)
	}
	g = mustCall(t, sato, "grade_submit", gradeArgs(w.fakeWorld, w.work, "g2", "16"))
	if g.str("result", "peer") != "counted" || g.str("result", "member_grades", "0", "adjustment", "kind") != "" {
		t.Errorf("graded once the window has closed: %s", g.Text)
	}

	results := mustCall(t, sato, "peer_review_results", inCourseArgs(w.fakeWorld, "assignment_id", w.project))
	if groups := list(results, "groups"); len(groups) != 2 || results.str("result", "groups", "0", "flags", "0") != "pair_without_self_evaluation" {
		t.Errorf("peer results: %s", results.Text)
	}
	wantEnvelope(t, mustCall(t, sato, "peer_review_results", inCourseArgs(w.fakeWorld, "assignment_id", w.co.AssignmentID)), "error",
		codeFailedPrecondition, "no_peer_form")
	wantEnvelope(t, mustCall(t, sato, "peer_review_results", inCourseArgs(w.fakeWorld, "assignment_id", w.project,
		"group_id", w.co.ComponentID)), "error", codeNotFound, "")

	sheet := func(key string, entries ...map[string]any) map[string]any {
		return inCourseArgs(w.fakeWorld, "assignment_id", w.project, "entries", entries, "idempotency_key", key)
	}
	ken := map[string]any{"student_member_id": w.seats[1].ID, "share": 100}
	wantEnvelope(t, mustCall(t, w.as("yuki"), "peer_review_submit", sheet("p0", map[string]any{"student_member_id": w.seats[1].ID,
		"share": 90})), "error", codeInvalidArgument, "bad_share_total")
	wantEnvelope(t, mustCall(t, w.as("yuki"), "peer_review_submit", sheet("p1", ken)), "failed", codeForbidden, "not_implemented")
	w.ok(w.fc.SetLevel(w.ownSeat(), permSubmissionWrite, "confirm_required"))
	wantEnvelope(t, mustCall(t, w.ownAgent(), "peer_review_submit", sheet("p2", ken)), "failed", codeForbidden, "people_only")
	if p := w.fc.Proposals(w.co.ID); len(p) != 0 {
		t.Errorf("an agent's peer evaluation was proposed: %+v", p)
	}
}

// TestGroupDeletion: deleting a group assignment counts its group's work
// once and every member's grade, reaches every member (a seat that
// reaches one is refused), and takes the work and the grades with it.
func TestGroupDeletion(t *testing.T) {
	w := newGroupWorld(t, Options{})
	sato := w.as("sato")
	wantEnvelope(t, mustCall(t, sato, "grade_submit", gradeArgs(w.fakeWorld, w.work, "g1", "16")), "executed", "", "")
	p := mustCall(t, sato, "assignment_delete_preview", inCourseArgs(w.fakeWorld, "assignment_id", w.project))
	if p.num("result", "counts", "submissions") != "1" || p.num("result", "counts", "grades") != "2" {
		t.Fatalf("the preview: %s", p.Text)
	}
	// A seat that reaches Ken alone does not reach Yuki's part in the
	// work, which goes with it.
	seat, lab := w.listedTutor(1)
	w.ok(w.fc.SetLevel(seat, permAssignmentWrite, "autonomous"))
	if r := mustCall(t, lab, "assignment_delete_preview", inCourseArgs(w.fakeWorld, "assignment_id", w.project)); r.str("result", "refusal") != reasonStudentScope {
		t.Errorf("the refusal of a seat that reaches one member: %s", r.Text)
	}
	counts, _ := p.Structured["result"].(map[string]any)["counts"]
	d := mustCall(t, sato, "assignment_delete", inCourseArgs(w.fakeWorld, "assignment_id", w.project, "confirm", counts,
		"idempotency_key", "delete:1"))
	wantEnvelope(t, d, "executed", "", "")
	if l := list(mustCall(t, sato, "submission_list", inCourseArgs(w.fakeWorld)), "submissions"); len(l) != 0 {
		t.Errorf("the group's work is left: %v", l)
	}
	if l := list(mustCall(t, sato, "grade_list", inCourseArgs(w.fakeWorld)), "grades"); len(l) != 0 {
		t.Errorf("the members' grades are left: %v", l)
	}
}
