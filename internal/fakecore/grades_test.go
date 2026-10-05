package fakecore

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// grade_submit as Core carries it out (grades.go): drafts, what replaces
// what, who sees them, and the proposals a newer draft refuses on approval
// (Core's Since), which the grade_since fixture holds the fake to as well.

func TestDecimal(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`"87.50"`, "87.5"}, {`87.5`, "87.5"}, {`"1e2"`, "100"}, {`1E2`, "100"}, {`"100.00"`, "100"}, {`".5"`, "0.5"},
		{`"-0.0"`, "0"}, {`"-2.250"`, "-2.25"}, {`0.1`, "0.1"}, {`"12345678901234567890.0000000001"`, "12345678901234567890.0000000001"},
		{`"2.5e-3"`, "0.0025"},
	} {
		var d decimal
		if err := json.Unmarshal([]byte(c.in), &d); err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if got := d.String(); got != c.want {
			t.Errorf("%s: %s, want %s", c.in, got, c.want)
		}
		// A JSON number, as Core writes a decimal.
		if b, _ := json.Marshal(d); string(b) != c.want {
			t.Errorf("%s written %s", c.in, b)
		}
	}
	var d decimal
	if err := json.Unmarshal([]byte(`"ten"`), &d); err == nil {
		t.Error("ten read as a decimal")
	}
	if decimalOf("100").cmp(decimalOf("100.00")) != 0 || decimalOf("100.5").cmp(decimalOf("100")) <= 0 {
		t.Error("compared wrong")
	}
}

// gradeArgs is grade_submit's arguments for a submission.
func gradeArgs(w *fakeWorld, submission, key string, score any, more ...any) map[string]any {
	return inCourseArgs(w, append([]any{"submission_id", submission, "score", score, "idempotency_key", key}, more...)...)
}

// grades is what grade_list shows c of the course's grades, by id: their
// states.
func grades(t *testing.T, w *fakeWorld, c *mcpClient) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, g := range list(mustCall(t, c, "grade_list", inCourseArgs(w)), "grades") {
		g := g.(map[string]any)
		out[g["id"].(string)] = g["state"].(string)
	}
	return out
}

// TestGradeDrafts: a draft replaces the work's earlier draft, not its
// posted grade; drafts and what they replaced are for those who grade, a
// student seeing posted grades alone, and the totals count posted grades
// alone; and news of a draft is for those who grade, within their scope.
func TestGradeDrafts(t *testing.T) {
	w := newFakeWorld(t, Options{})
	work, err := w.fc.AddWork(w.co.ID, w.seats[0].ID, "My answers.", "90")
	w.ok(err)
	sato, mori, yuki := w.as("sato"), w.as("mori"), w.as("yuki")
	first := mustCall(t, sato, "grade_submit", gradeArgs(w, work.SubmissionID, "g1", "85", "feedback", "Good.",
		"breakdown", []map[string]any{{"criterion": "Working", "points": 40, "max": "50"}}))
	wantEnvelope(t, first, "executed", "", "")
	g1 := first.str("result", "grade_id")
	if got := grades(t, w, sato); len(got) != 2 || got[work.GradeID] != "posted" || got[g1] != "draft" {
		t.Errorf("Sato's grade_list: %v", got)
	}
	// Mori's over REST, as the front end enters one.
	moriREST := &restClient{base: w.srv.URL, token: w.moriA.Token, hc: w.srv.Client()}
	h, err := moriREST.do(context.Background(), http.MethodPost, "/v1/courses/"+w.co.ID+"/grades",
		map[string]any{"submission_id": work.SubmissionID, "score": 88}, "g2")
	w.ok(err)
	var second struct {
		Status string `json:"status"`
		Result struct {
			GradeID string `json:"grade_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(h.Body, &second); err != nil || h.Status != http.StatusOK || second.Status != actExecuted {
		t.Fatalf("Mori's draft over REST: %d %s", h.Status, h.Body)
	}
	g2 := second.Result.GradeID
	if got := grades(t, w, sato); len(got) != 3 || got[work.GradeID] != "posted" || got[g1] != "superseded" || got[g2] != "draft" {
		t.Errorf("Sato's grade_list after Mori's draft: %v", got)
	}
	g := mustCall(t, sato, "grade_get", inCourseArgs(w, "grade_id", g1))
	if g.str("result", "superseded_by") != g2 || g.str("result", "feedback") != "Good." || g.num("result", "score") != "85" ||
		g.num("result", "breakdown", "0", "points") != "40" || g.str("result", "posted_at") != "" {
		t.Errorf("grade_get of the first draft: %s", g.Text)
	}
	if got := grades(t, w, yuki); len(got) != 1 || got[work.GradeID] != "posted" {
		t.Errorf("Yuki's grade_list: %v", got)
	}
	wantEnvelope(t, mustCall(t, yuki, "grade_get", inCourseArgs(w, "grade_id", g2)), "error", codeNotFound, "")
	book := mustCall(t, sato, "gradebook_get", inCourseArgs(w, "student_member_id", w.seats[0].ID))
	if f := book.str("result", "components", "1", "items", "0", "fraction"); f != "0.9" {
		t.Errorf("HW1 counts %s in the gradebook, want the posted 90's 0.9: %s", f, book.Text)
	}

	news := func(c *mcpClient) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, e := range list(mustCall(t, c, "event_list", inCourseArgs(w, "since_seq", 0)), "events") {
			if e := e.(map[string]any); e["type"] == "grade.created" {
				out = append(out, e)
			}
		}
		return out
	}
	if got := news(mori); len(got) != 2 || got[0]["subject_id"] != g1 || got[0]["student_member_id"] != w.seats[0].ID ||
		got[0]["assignment_id"] != w.co.AssignmentID {
		t.Errorf("Mori's news of the drafts: %v", got)
	}
	if got := news(yuki); len(got) != 0 {
		t.Errorf("Yuki is told of drafts: %v", got)
	}
	tutor := func(student int) *mcpClient {
		t.Helper()
		seat, c := w.listedTutor(student)
		w.ok(w.fc.SetLevel(seat, permGradeSubmit, "confirm_required"))
		return c
	}
	if got := news(tutor(0)); len(got) != 2 {
		t.Errorf("Yuki's listed tutor is told of %d drafts of her work, want 2", len(got))
	}
	if got := news(tutor(1)); len(got) != 0 {
		t.Errorf("Ken's listed tutor is told of drafts of Yuki's work: %v", got)
	}
}

// TestGradeSince: a proposal is pinned as Core pins it; approving it is
// refused, its owner told so, for a draft entered after it was proposed,
// and not for one entered before or at once; and the draft it makes is
// dated when it was proposed, replacing the one before.
func TestGradeSince(t *testing.T) {
	clk := newClock()
	w := newFakeWorld(t, Options{Now: clk.now})
	work, err := w.fc.AddWork(w.co.ID, w.seats[0].ID, "My answers.", "90")
	w.ok(err)
	seat, lab := w.listedTutor(0)
	w.ok(w.fc.SetLevel(seat, permGradeSubmit, "confirm_required"))
	sato, mori := w.as("sato"), w.as("mori")
	propose := func(key string, score any) string {
		t.Helper()
		p := mustCall(t, lab, "grade_submit", gradeArgs(w, work.SubmissionID, key, score))
		wantEnvelope(t, p, "proposed", "", "")
		return p.str("action_id")
	}
	draft := func(key string, score any) string {
		t.Helper()
		g := mustCall(t, sato, "grade_submit", gradeArgs(w, work.SubmissionID, key, score))
		wantEnvelope(t, g, "executed", "", "")
		return g.str("result", "grade_id")
	}
	decide := func(c *mcpClient, id, key string) toolAnswer {
		t.Helper()
		return mustCall(t, c, "action_decide", inCourseArgs(w, "action_id", id, "decision", "approve", "idempotency_key", key))
	}

	// Entered before the proposal, and at the same moment: neither is
	// newer than it, and approving it replaces the one at the same moment.
	draft("before", 70)
	clk.add(time.Second)
	first := propose("p1", "80")
	atOnce := draft("at_once", 75)
	for _, p := range w.fc.Proposals(w.co.ID) {
		var args map[string]any
		w.ok(json.Unmarshal(p.Args, &args))
		// Pinned as Core pins it, its decimals JSON numbers.
		if p.ActionID == first && (args["for_missing"] != false || args["out_of"] != 100.0 || args["no_rubric"] != true || args["score"] != 80.0) {
			t.Errorf("the proposal is kept as %s", p.Args)
		}
	}
	if d := decide(mori, first, "d1"); d.str("result", "outcome") != actExecuted {
		t.Fatalf("Mori approves a proposal no newer draft was entered after: %s", d.Text)
	}
	if got := grades(t, w, sato)[atOnce]; got != "superseded" {
		t.Errorf("the draft entered as the proposal was made is %s once it is approved", got)
	}

	// Entered after it: approving it is refused.
	clk.add(time.Second)
	second := propose("p2", 82)
	clk.add(time.Second)
	newer := draft("newer", 85)
	o := decide(sato, second, "d2")
	wantEnvelope(t, o, "failed", codeForbidden, "owner_would_be_refused")
	if o.str("error", "details", "refusal", "code") != codeFailedPrecondition {
		t.Errorf("Sato's refusal: %s", o.Text)
	}
	if d := decide(mori, second, "d3"); d.str("result", "outcome") != actFailed || d.str("result", "error", "code") != codeFailedPrecondition {
		t.Errorf("Mori's approval: %s", d.Text)
	}

	// Proposed after that draft, and approved long after: its draft is
	// dated when it was proposed, and replaces the one before it.
	clk.add(time.Second)
	third := propose("p3", 83)
	at := clk.now()
	clk.add(time.Hour)
	d := decide(sato, third, "d4")
	if res, _ := d.Structured["result"].(map[string]any); d.str("result", "outcome") != actExecuted || res["by_owner"] != true {
		t.Fatalf("Sato approves the third: %s", d.Text)
	}
	g := mustCall(t, sato, "grade_get", inCourseArgs(w, "grade_id", d.str("result", "result", "grade_id")))
	if g.str("result", "state") != "draft" || g.str("result", "created_at") != at.Format(time.RFC3339) || g.str("result", "grader_member_id") != seat {
		t.Errorf("the draft approved: %s", g.Text)
	}
	if got := grades(t, w, sato)[newer]; got != "superseded" {
		t.Errorf("the newer draft is %s once the proposal made after it is approved", got)
	}
}

// TestGradeSubmitRefusals: what the canned course refuses as Core would:
// a feedback file, there being no uploads; a grade on a component, the
// canned ones being rolled up; and a component or a student not there.
func TestGradeSubmitRefusals(t *testing.T) {
	w := newFakeWorld(t, Options{})
	work, err := w.fc.AddWork(w.co.ID, w.seats[0].ID, "My answers.", "90")
	w.ok(err)
	sato := w.as("sato")
	f := mustCall(t, sato, "grade_submit", gradeArgs(w, work.SubmissionID, "k1", 1,
		"feedback_files", []map[string]any{{"title": "Marked", "upload_token": "u"}}))
	wantEnvelope(t, f, "failed", codeInvalidArgument, "")
	if f.str("error", "message") != errNoDocumentUploads.Message {
		t.Errorf("a feedback file: %s", f.Text)
	}
	wantEnvelope(t, mustCall(t, sato, "grade_submit", gradeArgs(w, work.SubmissionID, "k2", 1,
		"feedback_files", []map[string]any{{"title": " ", "upload_token": "u"}})), "error", codeInvalidArgument, "")
	component := func(key, component, student string) toolAnswer {
		return mustCall(t, sato, "grade_submit", inCourseArgs(w, "component_id", component, "student_member_id", student, "score", 1,
			"idempotency_key", key))
	}
	for _, id := range []string{w.co.ComponentID, w.fc.courses[w.co.ID].rootComponent.id} {
		c := component("k3"+id, id, w.seats[0].ID)
		wantEnvelope(t, c, "failed", codeFailedPrecondition, "")
		if c.str("error", "message") != "this component is rolled up from what is beneath it and takes no grade of its own" {
			t.Errorf("a grade on a component: %s", c.Text)
		}
	}
	wantEnvelope(t, component("k4", "0192f3c1-0000-7000-8000-00000000abcd", w.seats[0].ID), "error", codeNotFound, "")
	wantEnvelope(t, component("k5", w.co.ComponentID, "0192f3c1-0000-7000-8000-00000000abcd"), "error", codeNotFound, "")
	wantEnvelope(t, mustCall(t, sato, "grade_submit", inCourseArgs(w, "component_id", w.co.ComponentID, "score", 1, "idempotency_key", "k6")),
		"error", codeInvalidArgument, "")
	wantEnvelope(t, mustCall(t, sato, "grade_submit", gradeArgs(w, work.SubmissionID, "k7", 1, "student_member_id", w.seats[0].ID)),
		"error", codeInvalidArgument, "")
	// Yuki grades nothing, and her tutor reaches her work alone.
	wantEnvelope(t, mustCall(t, w.as("yuki"), "grade_submit", gradeArgs(w, work.SubmissionID, "k8", 1)), "denied", codeForbidden, reasonPermDenied)
	kens, err := w.fc.AddWork(w.co.ID, w.seats[1].ID, "Mine.", "80")
	w.ok(err)
	seat, lab := w.listedTutor(0)
	w.ok(w.fc.SetLevel(seat, permGradeSubmit, "autonomous"))
	wantEnvelope(t, mustCall(t, lab, "grade_submit", gradeArgs(w, kens.SubmissionID, "k9", 1)), "denied", codeForbidden, reasonStudentScope)
	wantEnvelope(t, mustCall(t, lab, "grade_submit", gradeArgs(w, work.SubmissionID, "k10", 1)), "executed", "", "")
}
