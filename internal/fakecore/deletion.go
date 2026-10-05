package fakecore

import (
	"encoding/json"
	"math/big"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Deleting an assignment for good, as AIShie-Core #73 has it (Core's
// tools/assignment_delete.go): assignment.delete_preview counts what would
// go with it and says what the deletion would be refused for its caller,
// and assignment.delete takes it, given back the counts the preview
// showed. What goes is the assignment's work: its submissions, the grades
// given on them, and the files of both (a submission's and a feedback
// file's documents); its instructions are left in the course as they are.
// The proposals about it waiting are cancelled (target_deleted), every
// action about it is emptied (redacted), its events and the seats' lists
// of it go, and the totals it counted in are worked out again without it.
// Afterwards every call naming it is told it was deleted, and a call whose
// action was emptied is told that, whatever it sends.
//
// The fake keeps no totals of its own: a student has them once a grade of
// theirs was posted (AddWork, PostGrade), and working them out again
// writes again those of the course's two components whose working changes,
// in the order Core's snapshot writes them, from the assignment's
// component up (gradecalc.Ancestors, nearest first): the Assignments
// component's, which loses the assignment's line, and then the course
// total's, when that changes what the component gives it: what it comes
// to, or whether it is complete and comes to anything.

const (
	toolAssignmentDelete = "assignment.delete"

	// Why an assignment is not deleted, in error.details.reason.
	deletePeopleOnly   = "people_only"
	deleteConfirmStale = "confirm_stale"
	deleteDeleted      = "deleted"
	// cancelTargetDeleted is why a proposal about it was cancelled, and
	// what a call whose action was emptied is told.
	cancelTargetDeleted = "target_deleted"
)

// deletion is the record of an assignment deleted for good: when, and by
// which action.
type deletion struct {
	at       time.Time
	actionID string
}

// wroteTotals records that the student's totals were written down, as
// posting a grade writes them.
func (co *course) wroteTotals(student *member) {
	if co.totals == nil {
		co.totals = map[string]bool{}
	}
	co.totals[student.id] = true
}

// assignmentGone is what a call naming an assignment the course does not
// have is told: that it was deleted, when and by which action, if it was
// deleted for good; not found, otherwise.
func assignmentGone(co *course, id string) *apiError {
	if d := co.deletions[id]; d != nil {
		return missing("the assignment was deleted for good").with("reason", deleteDeleted).
			with("deleted_at", d.at).with("by_action_id", d.actionID)
	}
	return missing("no such assignment in this course")
}

// findOrGone is the course's assignment id, or what a call naming it is
// told.
func findOrGone(co *course, id uuid.UUID) (*assignment, error) {
	if a := findAssignment(co, id); a != nil {
		return a, nil
	}
	return nil, assignmentGone(co, id.String())
}

// deletionCounts is what goes with an assignment, counted (Core's
// DeletionCounts).
type deletionCounts struct {
	Submissions int `json:"submissions"`
	HandedIn    int `json:"handed_in"`
	Drafts      int `json:"drafts"`
	Missing     int `json:"missing"`
	Grades      int `json:"grades"`
	Posted      int `json:"posted"`
	Files       int `json:"files"`
	Proposals   int `json:"proposals"`
	Totals      int `json:"totals"`
}

// above says whether more would go, as c has it, than was shown in was.
func (c deletionCounts) above(was deletionCounts) bool {
	return c.Submissions > was.Submissions || c.HandedIn > was.HandedIn || c.Drafts > was.Drafts ||
		c.Missing > was.Missing || c.Grades > was.Grades || c.Posted > was.Posted || c.Files > was.Files ||
		c.Proposals > was.Proposals || c.Totals > was.Totals
}

func (c deletionCounts) check() error {
	if min(c.Submissions, c.HandedIn, c.Drafts, c.Missing, c.Grades, c.Posted, c.Files, c.Proposals, c.Totals) < 0 {
		return invalid("confirm's counts are none of them below zero: send back the counts assignment.delete_preview gave")
	}
	return nil
}

// doomed is what deleting an assignment takes with it, as the course
// stands.
type doomed struct {
	a      *assignment
	counts deletionCounts
	// work are its submissions, grades the grades given on them,
	// superseded ones included, and owned the documents of both.
	work   []*submission
	grades []*grade
	owned  []*document
	// students have work on it; totals have totals worked out again.
	students, totals []string
	// keys are the files of owned and their PDFs, as Core queues them.
	keys int
	// about are the course's actions about it (Core's
	// ListActionsAboutAssignment).
	about []*action
}

// rewritesTotals says whether the totals are worked out again without it:
// it counted toward a component, which only a published assignment does.
func (d doomed) rewritesTotals() bool { return d.a.publishedAt != nil && d.a.component != nil }

// scope is what deleting it reaches: the assignment, every student whose
// work goes with it, and, when the totals are worked out again, every
// student who has them, over the whole course.
func (d doomed) scope() scope {
	students := slices.Clone(d.students)
	for _, s := range d.totals {
		if !slices.Contains(students, s) {
			students = append(students, s)
		}
	}
	return scope{assignments: []string{d.a.id}, students: students, spans: d.rewritesTotals()}
}

// hasWork says whether anyone has started on it: a submission or a grade.
func (d doomed) hasWork() bool { return d.counts.Submissions > 0 || d.counts.Grades > 0 }

// refusal is what deleting it is refused for, by an agent or not, who was
// shown confirm (nil for the preview): nil when nothing refuses it. The
// fake always has somewhere to keep files, so no_file_storage never is.
func (d doomed) refusal(agent bool, confirm *deletionCounts) *apiError {
	switch {
	case agent && d.hasWork():
		return forbid("an agent does not delete an assignment that has work or grades; a person deletes it").
			with("reason", deletePeopleOnly)
	case confirm != nil && d.counts.above(*confirm):
		return conflicts("more would go with the assignment than you were shown; read what goes with it again "+
			"(assignment.delete_preview) and confirm that").with("reason", deleteConfirmStale).with("current", d.counts)
	}
	return nil
}

// readDeletion reads what deleting a takes with it.
func (c *Core) readDeletion(co *course, a *assignment) doomed {
	d := doomed{a: a}
	ids := map[string]bool{a.id: true}
	for _, s := range co.submissions {
		if s.assignment != a {
			continue
		}
		d.work = append(d.work, s)
		ids[s.id] = true
		d.counts.Submissions++
		// The fake's work is handed in: none is a draft or missing.
		d.counts.HandedIn++
		for _, st := range s.studentIDs() {
			if !slices.Contains(d.students, st) {
				d.students = append(d.students, st)
			}
		}
	}
	for _, g := range co.grades {
		if g.submission == nil || g.submission.assignment != a {
			continue
		}
		d.grades = append(d.grades, g)
		ids[g.id] = true
		if g.groupGrade != nil {
			ids[g.groupGrade.id] = true
		}
		if g.supersededBy == nil {
			d.counts.Grades++
			if g.postedAt != nil {
				d.counts.Posted++
			}
		}
	}
	for _, doc := range co.documents {
		if (doc.kind == kindSubmission && doc.submission != nil && ids[doc.submission.id]) ||
			(doc.kind == kindFeedback && doc.grade != nil && ids[doc.grade.id]) {
			d.owned = append(d.owned, doc)
			ids[doc.id] = true
			d.counts.Files += len(doc.files)
			d.keys += len(doc.files)
			for _, f := range doc.files {
				if f.rend != nil && f.rend.pdf != nil {
					d.keys++
				}
			}
		}
	}
	d.about = c.actionsAbout(co, ids)
	for _, act := range d.about {
		if act.status == actProposed {
			d.counts.Proposals++
		}
	}
	if d.rewritesTotals() {
		for s := range co.totals {
			d.totals = append(d.totals, s)
		}
		sort.Strings(d.totals)
		d.counts.Totals = len(d.totals)
	}
	return d
}

// actionsAbout are the course's actions about what ids names, other than
// deletions, that no deletion has emptied yet: those whose target is one
// of them; whose arguments name one as assignment_id, submission_id,
// grade_id or document_id, or whose result does as id, submission_id,
// grade_id or document_id; and, at any remove, the decisions, reviews,
// withdrawals and expiries of those.
func (c *Core) actionsAbout(co *course, ids map[string]bool) []*action {
	names := func(raw json.RawMessage, fields ...string) bool {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return false
		}
		for _, f := range fields {
			if s, ok := m[f].(string); ok && ids[s] {
				return true
			}
		}
		return false
	}
	in := map[string]bool{}
	for _, a := range c.actionList {
		if a.course != co || a.actionType == toolAssignmentDelete {
			continue
		}
		if (a.targetID != nil && ids[*a.targetID]) || names(a.payload, "assignment_id", "submission_id", "grade_id", "document_id") ||
			names(a.result, "id", "submission_id", "grade_id", "document_id", "group_grade_id") {
			in[a.id] = true
		}
	}
	for grew := true; grew; {
		grew = false
		for _, a := range c.actionList {
			if a.course == co && !in[a.id] && a.targetType == "action" && a.targetID != nil && in[*a.targetID] {
				in[a.id], grew = true, true
			}
		}
	}
	var out []*action
	for _, a := range c.actionList {
		if in[a.id] && a.redactedBy == "" {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

var gateWritesAssignments = gate{perms: []string{permAssignmentWrite}}

func assignmentDeletePreview() *impl {
	return define(spec[assignmentGetIn]{
		gate: gateWritesAssignments,
		resolve: func(_ *Core, co *course, in assignmentGetIn) (target, error) {
			a, err := findOrGone(co, in.AssignmentID)
			if err != nil {
				return target{}, err
			}
			return target{typ: "assignment", id: &a.id, scope: scope{assignments: []string{a.id}}}, nil
		},
		query: func(c *Core, rc *readCtx, in assignmentGetIn) (any, error) {
			a, err := findOrGone(rc.course, in.AssignmentID)
			if err != nil {
				return nil, err
			}
			d := c.readDeletion(rc.course, a)
			out := struct {
				AssignmentID string         `json:"assignment_id"`
				Title        string         `json:"title"`
				Published    bool           `json:"published"`
				InGrade      bool           `json:"in_grade"`
				Counts       deletionCounts `json:"counts"`
				Refusal      *string        `json:"refusal"`
			}{AssignmentID: a.id, Title: a.title, Published: a.publishedAt != nil, InGrade: a.component != nil, Counts: d.counts}
			// As assignment.delete would be refused, in its order: the
			// course, the reach, then what its validate asks.
			reason := ""
			if rc.course.status == statusArchived {
				reason = reasonCourseArchived
			} else if reason = checkScope(rc.member, d.scope()); reason == "" {
				if e := d.refusal(rc.actor.kind == "agent", nil); e != nil {
					reason, _ = e.Details["reason"].(string)
				}
			}
			if reason != "" {
				out.Refusal = &reason
			}
			return out, nil
		},
	})
}

type assignmentDeleteIn struct {
	inCourse
	AssignmentID uuid.UUID      `json:"assignment_id"`
	Confirm      deletionCounts `json:"confirm"`
}

func assignmentDelete() *impl {
	return define(spec[assignmentDeleteIn]{
		gate:  gateWritesAssignments,
		check: func(in assignmentDeleteIn) error { return in.Confirm.check() },
		resolve: func(c *Core, co *course, in assignmentDeleteIn) (target, error) {
			a, err := findOrGone(co, in.AssignmentID)
			if err != nil {
				return target{}, err
			}
			return target{typ: "assignment", id: &a.id, scope: c.readDeletion(co, a).scope()}, nil
		},
		// Asked before a call is carried out or a proposal queued, again
		// as the proposer when it is approved, and for its proposer's
		// owner; execute asks it again.
		validate: func(c *Core, m *member, in assignmentDeleteIn) error {
			a, err := findOrGone(m.course, in.AssignmentID)
			if err != nil {
				return err
			}
			if e := c.readDeletion(m.course, a).refusal(m.actor.kind == "agent", &in.Confirm); e != nil {
				return e
			}
			return nil
		},
		execute: func(c *Core, ec *execCtx, in assignmentDeleteIn) (any, error) {
			return c.deleteAssignment(ec, in)
		},
	})
}

type deletionRemoved struct {
	Submissions int `json:"submissions"`
	Grades      int `json:"grades"`
	Files       int `json:"files"`
}

type assignmentDeleteOut struct {
	Deleted            bool            `json:"deleted"`
	AssignmentID       string          `json:"assignment_id"`
	Title              string          `json:"title"`
	Removed            deletionRemoved `json:"removed"`
	ProposalsCancelled int             `json:"proposals_cancelled"`
	Snapshots          int             `json:"snapshots"`
	FilesQueued        int             `json:"files_queued"`
}

// deleteAssignment carries assignment.delete out, as Core's does in one
// transaction: what goes is read again and refused again, the reach
// checked over it as it is now, and then the proposals about it are
// cancelled, every action about it emptied, its work and the files of it,
// the seats' lists of it, its events and it deleted, the totals worked out
// again, and the feed told.
func (c *Core) deleteAssignment(ec *execCtx, in assignmentDeleteIn) (any, error) {
	co := ec.course
	a, err := findOrGone(co, in.AssignmentID)
	if err != nil {
		return nil, err
	}
	d := c.readDeletion(co, a)
	if e := d.refusal(ec.actor.kind == "agent", &in.Confirm); e != nil {
		return nil, e
	}
	if r := checkScope(ec.member, d.scope()); r != "" {
		return nil, forbid("deleting it changes the work or the totals of students outside your scope").with("reason", r)
	}
	// What the totals came to with it, to be worked out again without it
	// (none, unless it counts toward a component).
	before := map[string][2]working{}
	for _, s := range d.totals {
		before[s] = workTotals(co, s)
	}
	if co.deletions == nil {
		co.deletions = map[string]*deletion{}
	}
	co.deletions[a.id] = &deletion{at: ec.now, actionID: ec.actionID}
	out := assignmentDeleteOut{Deleted: true, AssignmentID: a.id, Title: a.title, FilesQueued: d.keys,
		Removed: deletionRemoved{Submissions: d.counts.Submissions, Grades: d.counts.Grades, Files: d.counts.Files}}

	// What was about it: the proposals waiting are cancelled, each
	// telling its proposer, and then all of it is emptied.
	for _, act := range d.about {
		if act.status == actProposed {
			act.status, act.result = actCancelled, errorResult(cancellation(cancelTargetDeleted, map[string]any{"by_action_id": ec.actionID}))
			out.ProposalsCancelled++
			id := act.id
			ec.emit(&event{typ: "action.cancelled", course: act.course, actionID: &id, subjectType: "action", subjectID: &id,
				payload: mustJSON(map[string]any{"action_type": act.actionType, "reason": cancelTargetDeleted, "by_action_id": ec.actionID})})
		}
		act.payload, act.result, act.redactedBy = json.RawMessage("{}"), nil, ec.actionID
		act.hash = payloadHash(act.actionType, act.payload)
	}

	// Its work, the files of it, and the seats' lists and the feed's
	// events of it.
	owned := map[*document]bool{}
	for _, doc := range d.owned {
		owned[doc] = true
	}
	for token, f := range c.blobs {
		if owned[f.doc] {
			delete(c.blobs, token)
		}
	}
	for id, r := range c.rends {
		if r.file != nil && owned[r.file.doc] {
			delete(c.rends, id)
		}
	}
	co.documents = slices.DeleteFunc(co.documents, func(doc *document) bool { return owned[doc] })
	co.grades = slices.DeleteFunc(co.grades, func(g *grade) bool { return slices.Contains(d.grades, g) })
	co.submissions = slices.DeleteFunc(co.submissions, func(s *submission) bool { return s.assignment == a })
	for _, m := range c.memberList {
		if m.course == co {
			delete(m.assignments, a.id)
		}
	}
	co.events = slices.DeleteFunc(co.events, func(e *event) bool { return e.assignment == a.id })
	co.assignments = slices.DeleteFunc(co.assignments, func(x *assignment) bool { return x == a })

	// The totals it counts in, worked out again without it: each written
	// again whose working changed, the component's before the course
	// total's, as Core walks up from it.
	for _, s := range d.totals {
		after := workTotals(co, s)
		for i, cp := range []*component{co.bucket, co.rootComponent} {
			if before[s][i].same(after[i]) {
				continue
			}
			out.Snapshots++
			payload := map[string]any{"component_id": cp.id, "complete": after[i].complete}
			if after[i].fraction == nil {
				payload["no_total"] = true
			}
			id := newID()
			ec.emit(&event{typ: "grade.total_updated", course: co, subjectType: "grade", subjectID: &id, student: s, payload: mustJSON(payload)})
		}
	}

	typ := "assignment.deleted"
	if a.publishedAt == nil {
		typ = "assignment.deleted_unreleased"
	}
	ec.emit(&event{typ: typ, course: co, subjectType: "assignment", subjectID: &a.id, payload: mustJSON(map[string]any{"title": a.title})})
	return out, nil
}

// working is a component's total for a student as far as the fake works
// one out: what it comes to (nil for nothing to go on), whether every line
// beneath it was graded, and its lines.
type working struct {
	fraction *big.Rat
	complete bool
	lines    []string
}

func (w working) same(o working) bool {
	if w.complete != o.complete || (w.fraction == nil) != (o.fraction == nil) || !slices.Equal(w.lines, o.lines) {
		return false
	}
	return w.fraction == nil || w.fraction.Cmp(o.fraction) == 0
}

// workTotals is the student's totals of the Assignments component, which
// every assignment of the fake's counts toward, and of the course total,
// in that order: the posted grades of its published assignments, over the
// points they are worth. The course total is complete only when the
// component is and comes to something, as Core's gradecalc.parent has it:
// a component with nothing to go on, such as one left with no published
// assignment, is a gap in the total above it.
func workTotals(co *course, student string) [2]working {
	var bucket working
	bucket.complete = true
	got, possible := new(big.Rat), new(big.Rat)
	graded := false
	for _, a := range co.assignments {
		if a.publishedAt == nil || a.component != co.bucket {
			continue
		}
		line := a.id + ":"
		var score *big.Rat
		for _, g := range co.grades {
			if g.student.id == student && g.assignment == a && g.standing() {
				score = decimalOf(g.score).rat()
			}
		}
		if score == nil {
			bucket.complete = false
		} else {
			graded = true
			got.Add(got, score)
			possible.Add(possible, decimalOf(a.points).rat())
			line += score.RatString()
		}
		bucket.lines = append(bucket.lines, line)
	}
	if graded && possible.Sign() > 0 {
		bucket.fraction = new(big.Rat).Quo(got, possible)
	}
	root := working{fraction: bucket.fraction, complete: bucket.complete && bucket.fraction != nil, lines: []string{co.bucket.id}}
	if bucket.fraction != nil {
		root.lines[0] += ":" + bucket.fraction.RatString()
	}
	return [2]working{bucket, root}
}
