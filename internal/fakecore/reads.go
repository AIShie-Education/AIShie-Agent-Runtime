package fakecore

import (
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
)

// The reads the runtime makes itself (me_*, event_list, action_list_mine),
// with Core's semantics; and the reads a model is offered (§2.3), over a
// course's canned material, with Core's gates and scope.

type emptyIn struct{}

func meGet() *impl {
	return define(spec[emptyIn]{
		gate:    gate{self: true},
		resolve: func(*Core, *course, emptyIn) (target, error) { return target{typ: "actor"}, nil },
		query: func(c *Core, rc *readCtx, _ emptyIn) (any, error) {
			a := rc.actor
			out := struct {
				ID          string `json:"id"`
				Kind        string `json:"kind"`
				DisplayName string `json:"display_name"`
				Status      string `json:"status"`
				// OwnerActorID is the person who owns an agent, absent for
				// a person and for an agent nobody owns (Core's C1).
				OwnerActorID *string `json:"owner_actor_id,omitempty"`
				// Hosting is an agent's, runtime or mcp; absent for a
				// person.
				Hosting *string `json:"hosting,omitempty"`
			}{ID: a.id, Kind: a.kind, DisplayName: a.name, Status: a.status}
			if a.owner != nil {
				out.OwnerActorID = &a.owner.id
			}
			if a.hosting != "" {
				out.Hosting = ptr(a.hosting)
			}
			return out, nil
		},
	})
}

type membershipView struct {
	MemberID          string            `json:"member_id"`
	CourseID          string            `json:"course_id"`
	Code              string            `json:"code"`
	Section           string            `json:"section"`
	Title             string            `json:"title"`
	CourseStatus      string            `json:"course_status"`
	Role              string            `json:"role"`
	Status            string            `json:"status"`
	ExpiresAt         *time.Time        `json:"expires_at,omitempty"`
	StudentScope      string            `json:"student_scope"`
	AssignmentScope   string            `json:"assignment_scope"`
	PrincipalMemberID *string           `json:"principal_member_id,omitempty"`
	Perms             map[string]string `json:"perms"`
	AnswersCourse     bool              `json:"answers_course"`
	ceilings
}

func meMemberships() *impl {
	return define(spec[emptyIn]{
		gate:    gate{self: true},
		resolve: func(*Core, *course, emptyIn) (target, error) { return target{typ: "course_member"}, nil },
		query: func(c *Core, rc *readCtx, _ emptyIn) (any, error) {
			var seats []*member
			for _, m := range c.memberList {
				if m.actor == rc.actor && m.status != statusRemoved {
					seats = append(seats, m)
				}
			}
			sort.SliceStable(seats, func(i, j int) bool {
				x, y := seats[i], seats[j]
				if x.course.code != y.course.code {
					return x.course.code < y.course.code
				}
				if x.course.section != y.course.section {
					return x.course.section < y.course.section
				}
				return x.id < y.id
			})
			out := struct {
				Memberships []membershipView `json:"memberships"`
			}{Memberships: make([]membershipView, 0, len(seats))}
			for _, m := range seats {
				v := membershipView{MemberID: m.id, CourseID: m.course.id, Code: m.course.code, Section: m.course.section,
					Title: m.course.title, CourseStatus: m.course.status, Role: m.role, Status: m.status, ExpiresAt: m.expiresAt,
					StudentScope: m.studentScope, AssignmentScope: m.assignmentScope, Perms: m.effectivePerms(rc.now),
					AnswersCourse: m.answersOthers(), ceilings: ceilingsOf(m)}
				if m.principal != nil {
					id := m.principal.id
					v.PrincipalMemberID = &id
				}
				out.Memberships = append(out.Memberships, v)
			}
			return out, nil
		},
	})
}

// visibility says who sees each type of event: holding any one of the
// listed permissions is enough (Core's tools/event.go). A type not listed is
// seen only by the member whose action caused it; a conversation's news is
// its two participants' and nobody else's.
var visibility = map[string][]string{
	"action.proposed": {permActionDecide}, "action.approved": {permActionDecide},
	"action.rejected": {permActionDecide}, "action.cancelled": {permActionDecide},
	"action.reviewed": {permActionDecide}, "action.escalated": {permActionDecide},
	"member.added": {permMemberRead}, "member.updated": {permMemberRead},
	"member.paused": {permMemberRead}, "member.resumed": {permMemberRead},
	"member.removed": {permMemberRead}, "member.rescoped": {permMemberRead},
	"course.created": {permDocumentRead}, "course.updated": {permDocumentRead},
	"course.activated": {permDocumentRead}, "course.archived": {permDocumentRead},
	"document.text_updated": {permDocumentRead}, "document.rubric_text_updated": {permRubricRead},
	"document.draft_text_updated": {permDocumentReadDraft},
}

func (c *Core) visible(e *event, m *member) bool {
	if e.subjectType == "conversation" {
		cv := c.conversations[*e.subjectID]
		return cv != nil && (cv.opener == m || cv.respondent == m)
	}
	for _, p := range visibility[e.typ] {
		if m.perm(p).allowed() {
			return true
		}
	}
	if e.actionID != nil {
		if a := c.actions[*e.actionID]; a != nil && a.member == m {
			return true
		}
	}
	return false
}

type eventListIn struct {
	inCourse
	SinceSeq int64 `json:"since_seq,omitempty"`
	Limit    int   `json:"limit,omitempty"`
	canWait
}

type eventView struct {
	Seq         int64           `json:"seq"`
	Type        string          `json:"type"`
	ActionID    *string         `json:"action_id,omitempty"`
	SubjectType string          `json:"subject_type"`
	SubjectID   *string         `json:"subject_id,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	OccurredAt  time.Time       `json:"occurred_at"`
}

// eventList is the feed from a cursor. The fake's events name no student or
// assignment, so Core's scope filters on those have nothing to filter here.
// next_seq is the last event returned, or since_seq when none is: Core's
// query leaves out what the caller may not see before it pages.
func eventList() *impl {
	return define(spec[eventListIn]{
		gate:    gateConverses,
		resolve: func(*Core, *course, eventListIn) (target, error) { return target{typ: "event"}, nil },
		query: func(c *Core, rc *readCtx, in eventListIn) (any, error) {
			limit := 100
			if in.Limit > 0 {
				limit = min(in.Limit, 500)
			}
			out := struct {
				Events  []eventView `json:"events"`
				NextSeq int64       `json:"next_seq"`
				More    bool        `json:"more"`
			}{Events: []eventView{}, NextSeq: in.SinceSeq}
			for _, e := range rc.course.events {
				if len(out.Events) == limit {
					break
				}
				if e.seq <= in.SinceSeq || !c.visible(e, rc.member) {
					continue
				}
				out.Events = append(out.Events, eventView{Seq: e.seq, Type: e.typ, ActionID: e.actionID, SubjectType: e.subjectType,
					SubjectID: e.subjectID, Payload: e.payload, OccurredAt: e.occurredAt})
				out.NextSeq = e.seq
			}
			out.More = len(out.Events) == limit
			return out, nil
		},
	})
}

type actionView struct {
	ID                 string          `json:"id"`
	ActorID            string          `json:"actor_id"`
	MemberID           *string         `json:"member_id,omitempty"`
	ActionType         string          `json:"action_type"`
	TargetType         string          `json:"target_type"`
	TargetID           *string         `json:"target_id,omitempty"`
	Payload            json.RawMessage `json:"payload"`
	AuthzResult        string          `json:"authz_result"`
	Status             string          `json:"status"`
	DecidedByMemberID  *string         `json:"decided_by_member_id,omitempty"`
	DecidedAt          *time.Time      `json:"decided_at,omitempty"`
	ReviewState        string          `json:"review_state"`
	ReviewedByMemberID *string         `json:"reviewed_by_member_id,omitempty"`
	ReviewedAt         *time.Time      `json:"reviewed_at,omitempty"`
	ExecutedAt         *time.Time      `json:"executed_at,omitempty"`
	Result             json.RawMessage `json:"result,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
}

func viewAction(a *action) actionView {
	v := actionView{ID: a.id, ActorID: a.actor.id, ActionType: a.actionType, TargetType: a.targetType, TargetID: a.targetID,
		Payload: a.payload, AuthzResult: a.authz.String(), Status: a.status, DecidedAt: a.decidedAt, ReviewState: a.reviewState,
		ReviewedAt: a.reviewedAt, ExecutedAt: a.executedAt, Result: a.result, CreatedAt: a.createdAt}
	if a.member != nil {
		id := a.member.id
		v.MemberID = &id
	}
	if a.decidedBy != nil {
		id := a.decidedBy.id
		v.DecidedByMemberID = &id
	}
	if a.reviewedBy != nil {
		id := a.reviewedBy.id
		v.ReviewedByMemberID = &id
	}
	return v
}

type pageIn struct {
	After *uuid.UUID `json:"after,omitempty"`
	Limit int        `json:"limit,omitempty"`
}

func (p pageIn) after() string {
	if p.After == nil {
		return ""
	}
	return p.After.String()
}

type actionListMineIn struct {
	inCourse
	ExcludeTypes []string `json:"exclude_types,omitempty"`
	pageIn
}

// actionListMine is the caller's own actions in the course, oldest first:
// how an agent reads what became of its proposals, and a rejection's reason.
func actionListMine() *impl {
	return define(spec[actionListMineIn]{
		gate:    gateConverses,
		resolve: func(*Core, *course, actionListMineIn) (target, error) { return target{typ: "action"}, nil },
		query: func(c *Core, rc *readCtx, in actionListMineIn) (any, error) {
			limit, after := pageLimit(in.Limit), in.after()
			out := struct {
				Actions []actionView `json:"actions"`
				Next    *string      `json:"next,omitempty"`
			}{Actions: []actionView{}}
			for _, a := range c.actionList {
				if len(out.Actions) == limit {
					break
				}
				if a.course != rc.course || a.member != rc.member || a.id <= after || slices.Contains(in.ExcludeTypes, a.actionType) {
					continue
				}
				out.Actions = append(out.Actions, viewAction(a))
			}
			if n := len(out.Actions); n > 0 && n == limit {
				id := out.Actions[n-1].ID
				out.Next = &id
			}
			return out, nil
		},
	})
}

// payloadString is a string field of an action's stored arguments.
func payloadString(payload json.RawMessage, field string) string {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	s, _ := m[field].(string)
	return s
}

// ---------------------------------------------------------------------------
// The model's reads, over canned material
// ---------------------------------------------------------------------------

var (
	gateDocumentRead = gate{perms: []string{permDocumentRead}}
	gateSubmissions  = gate{perms: []string{permSubmissionRead}}
	gateGrades       = gate{perms: []string{permGradeRead}}
)

type courseIn struct{ inCourse }

func courseGet() *impl {
	return define(spec[courseIn]{
		gate:    gateDocumentRead,
		resolve: func(_ *Core, co *course, _ courseIn) (target, error) { return target{typ: "course", id: &co.id}, nil },
		query: func(_ *Core, rc *readCtx, _ courseIn) (any, error) {
			co := rc.course
			return struct {
				ID        string    `json:"id"`
				DeptID    string    `json:"dept_id"`
				TermID    string    `json:"term_id"`
				Code      string    `json:"code"`
				Section   string    `json:"section"`
				Title     string    `json:"title"`
				Status    string    `json:"status"`
				CreatedAt time.Time `json:"created_at"`
			}{co.id, co.deptID, co.termID, co.code, co.section, co.title, co.status, co.createdAt}, nil
		},
	})
}

// Document kinds, and the permission that reads each.
const (
	kindMaterial     = "material"
	kindInstructions = "instructions"
	kindRubric       = "rubric"
	kindSubmission   = "submission"
	kindFeedback     = "feedback"
)

func readPerm(kind string) string {
	switch kind {
	case kindRubric:
		return permRubricRead
	case kindSubmission:
		return permSubmissionRead
	case kindFeedback:
		return permGradeRead
	}
	return permDocumentRead
}

func courseLevel(kind string) bool {
	return kind == kindMaterial || kind == kindInstructions || kind == kindRubric
}

type documentSummary struct {
	ID                 string    `json:"id"`
	Kind               string    `json:"kind"`
	Title              string    `json:"title"`
	PublishedVersionID *string   `json:"published_version_id,omitempty"`
	SortOrder          int       `json:"sort_order"`
	Status             string    `json:"status"`
	CreatedAt          time.Time `json:"created_at"`
}

func summarize(doc *document) documentSummary {
	s := documentSummary{ID: doc.id, Kind: doc.kind, Title: doc.title, SortOrder: doc.sortOrder, Status: "active", CreatedAt: doc.createdAt}
	if !doc.draft {
		v := doc.versionID
		s.PublishedVersionID = &v
	}
	return s
}

// hiddenDraft reports whether doc is a draft m cannot see: a course's
// document not yet published, to anyone who cannot read drafts.
func hiddenDraft(doc *document, m *member) bool {
	return doc.draft && courseLevel(doc.kind) && !m.perm(permDocumentReadDraft).allowed()
}

type documentListIn struct {
	inCourse
	Kind            *string `json:"kind,omitempty"`
	IncludeArchived bool    `json:"include_archived,omitempty"`
	pageIn
}

// withheld reports whether a course's instructions or rubric are out of
// m's reach: they follow their assignment, so to anyone who does not write
// assignments they are there only while a published assignment in their
// scope refers to them.
func withheld(doc *document, m *member) bool {
	if doc.kind == kindMaterial || !courseLevel(doc.kind) || m.perm(permAssignmentWrite).allowed() {
		return false
	}
	for _, a := range doc.course.assignments {
		if a.instructions == doc && a.publishedAt != nil && inScope(m, "", a.id) {
			return false
		}
	}
	return true
}

func documentList() *impl {
	return define(spec[documentListIn]{
		gate: gate{any: true, perms: []string{permDocumentRead, permRubricRead}},
		// Core's gate takes either permission, and then the target names the
		// one that governs: document_read, or the named kind's.
		resolve: func(_ *Core, _ *course, in documentListIn) (target, error) {
			t := target{typ: "document", perms: []string{permDocumentRead}}
			if in.Kind != nil {
				if !courseLevel(*in.Kind) {
					return t, invalid("kind must be material, instructions or rubric")
				}
				t.perms = []string{readPerm(*in.Kind)}
			}
			return t, nil
		},
		query: func(_ *Core, rc *readCtx, in documentListIn) (any, error) {
			limit, after := pageLimit(in.Limit), in.after()
			out := struct {
				Documents []documentSummary `json:"documents"`
				Next      *string           `json:"next,omitempty"`
			}{Documents: []documentSummary{}}
			for _, doc := range rc.course.documents {
				if len(out.Documents) == limit {
					break
				}
				if !courseLevel(doc.kind) || !rc.member.perm(readPerm(doc.kind)).allowed() || doc.id <= after ||
					(in.Kind != nil && *in.Kind != doc.kind) || withheld(doc, rc.member) || hiddenDraft(doc, rc.member) {
					continue
				}
				out.Documents = append(out.Documents, summarize(doc))
			}
			if n := len(out.Documents); n > 0 && n == limit {
				id := out.Documents[n-1].ID
				out.Next = &id
			}
			return out, nil
		},
	})
}

type documentGetIn struct {
	inCourse
	DocumentID uuid.UUID  `json:"document_id"`
	VersionID  *uuid.UUID `json:"version_id,omitempty"`
}

type versionView struct {
	ID     string  `json:"id"`
	Seq    int     `json:"seq"`
	BodyMD *string `json:"body_md,omitempty"`
	// DownloadURL, ContentType, ByteSize, Checksum and Text are the first
	// file's, as Core keeps them for the runtimes of the release before.
	DownloadURL *string `json:"download_url,omitempty"`
	ContentType *string `json:"content_type,omitempty"`
	ByteSize    *int64  `json:"byte_size,omitempty"`
	Checksum    *string `json:"checksum,omitempty"`
	// Files are the version's files, in order; none from a Core before
	// several files to a version (Options.WithoutFiles).
	Files          *[]fileView `json:"files,omitempty"`
	AuthorMemberID string      `json:"author_member_id"`
	CreatedAt      time.Time   `json:"created_at"`
	Published      bool        `json:"published"`
	Text           *textView   `json:"text,omitempty"`
}

func (c *Core) findDocument(co *course, id uuid.UUID) *document {
	for _, doc := range co.documents {
		if doc.id == id.String() {
			return doc
		}
	}
	return nil
}

// ownerScope is whose an owned document is: a submission's file is its
// student's work on its assignment.
func ownerScope(doc *document) scope {
	switch {
	case doc.submission != nil:
		return scope{students: []string{doc.submission.student.id}, assignments: []string{doc.submission.assignment.id}}
	case doc.grade != nil:
		return scope{students: []string{doc.grade.student.id}, assignments: []string{doc.grade.assignment.id}}
	}
	return scope{}
}

// documentGet reads a document's one published version. A file's
// download_url points at the fake itself, and lasts as long as it runs.
func documentGet() *impl {
	return define(spec[documentGetIn]{
		gate: gate{any: true, perms: []string{permDocumentRead, permRubricRead, permSubmissionRead, permGradeRead}},
		resolve: func(c *Core, co *course, in documentGetIn) (target, error) {
			doc := c.findDocument(co, in.DocumentID)
			if doc == nil {
				return target{}, missing("no such document in this course")
			}
			return target{typ: "document", id: &doc.id, scope: ownerScope(doc), perms: []string{readPerm(doc.kind)}}, nil
		},
		query: func(c *Core, rc *readCtx, in documentGetIn) (any, error) {
			doc := c.findDocument(rc.course, in.DocumentID)
			if hiddenDraft(doc, rc.member) {
				return nil, missing("no such document in this course")
			}
			if withheld(doc, rc.member) {
				if in.VersionID == nil {
					return nil, missing("no such document in this course")
				}
				return nil, missing("no such version of this document")
			}
			if in.VersionID != nil && in.VersionID.String() != doc.versionID {
				return nil, missing("no such version of this document")
			}
			var v *versionView
			if doc.versionID != "" {
				v = &versionView{ID: doc.versionID, Seq: 1, BodyMD: doc.bodyMD, AuthorMemberID: doc.authorMemberID,
					CreatedAt: doc.versionCreatedAt, Published: !doc.draft}
				files := filesOf(doc, rc.base, true)
				if !c.opts.WithoutFiles {
					v.Files = &files
				}
				if len(files) > 0 {
					f := files[0]
					v.DownloadURL, v.ContentType, v.ByteSize, v.Checksum, v.Text = f.DownloadURL, &f.ContentType, &f.ByteSize, f.Checksum, f.Text
					if c.opts.WithoutFiles {
						// The version's one file's text, whatever its size
						// beside the others', as before.
						v.Checksum, v.Text = nil, doc.files[0].text.viewIf(true)
					}
				}
			}
			out := struct {
				documentSummary
				SubmissionID *string      `json:"submission_id,omitempty"`
				GradeID      *string      `json:"grade_id,omitempty"`
				Version      *versionView `json:"version,omitempty"`
			}{documentSummary: summarize(doc), Version: v}
			if doc.submission != nil {
				out.SubmissionID = &doc.submission.id
			}
			if doc.grade != nil {
				out.GradeID = &doc.grade.id
			}
			return out, nil
		},
	})
}

type assignmentView struct {
	ID                     string     `json:"id"`
	ComponentID            *string    `json:"component_id,omitempty"`
	Title                  string     `json:"title"`
	InstructionsDocumentID *string    `json:"instructions_document_id,omitempty"`
	PointsPossible         string     `json:"points_possible"`
	DueAt                  *time.Time `json:"due_at,omitempty"`
	PublishedAt            *time.Time `json:"published_at,omitempty"`
}

func viewAssignment(a *assignment) assignmentView {
	v := assignmentView{ID: a.id, Title: a.title, PointsPossible: a.points, DueAt: a.dueAt, PublishedAt: a.publishedAt}
	if a.component != nil {
		v.ComponentID = &a.component.id
	}
	if a.instructions != nil {
		v.InstructionsDocumentID = &a.instructions.id
	}
	return v
}

// inScope reports whether m reaches the student and the assignment, as a
// list query's scope filter decides it; "" matches anything.
func inScope(m *member, student, assignmentID string) bool {
	t := scope{}
	if student != "" {
		t.students = []string{student}
	}
	if assignmentID != "" {
		t.assignments = []string{assignmentID}
	}
	return checkScope(m, t) == ""
}

type listIn struct {
	inCourse
	pageIn
}

func assignmentList() *impl {
	return define(spec[listIn]{
		gate:    gateDocumentRead,
		resolve: func(*Core, *course, listIn) (target, error) { return target{typ: "assignment"}, nil },
		query: func(_ *Core, rc *readCtx, in listIn) (any, error) {
			limit, after := pageLimit(in.Limit), in.after()
			out := struct {
				Assignments []assignmentView `json:"assignments"`
				Next        *string          `json:"next,omitempty"`
			}{Assignments: []assignmentView{}}
			for _, a := range rc.course.assignments {
				if len(out.Assignments) < limit && a.id > after && inScope(rc.member, "", a.id) {
					out.Assignments = append(out.Assignments, viewAssignment(a))
				}
			}
			if n := len(out.Assignments); n > 0 && n == limit {
				out.Next = &out.Assignments[n-1].ID
			}
			return out, nil
		},
	})
}

type assignmentGetIn struct {
	inCourse
	AssignmentID uuid.UUID `json:"assignment_id"`
}

func findAssignment(co *course, id uuid.UUID) *assignment {
	for _, a := range co.assignments {
		if a.id == id.String() {
			return a
		}
	}
	return nil
}

func assignmentGet() *impl {
	return define(spec[assignmentGetIn]{
		gate: gateDocumentRead,
		resolve: func(_ *Core, co *course, in assignmentGetIn) (target, error) {
			a := findAssignment(co, in.AssignmentID)
			if a == nil {
				return target{}, missing("no such assignment in this course")
			}
			return target{typ: "assignment", id: &a.id, scope: scope{assignments: []string{a.id}}}, nil
		},
		query: func(_ *Core, rc *readCtx, in assignmentGetIn) (any, error) {
			return viewAssignment(findAssignment(rc.course, in.AssignmentID)), nil
		},
	})
}

type submissionView struct {
	ID              string     `json:"id"`
	AssignmentID    string     `json:"assignment_id"`
	StudentMemberID string     `json:"student_member_id"`
	Attempt         int        `json:"attempt"`
	Body            *string    `json:"body,omitempty"`
	State           string     `json:"state"`
	SubmittedAt     *time.Time `json:"submitted_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// viewSubmission is a submission as submission_get shows it, body and all;
// submission_list leaves the body out, as Core's does.
func viewSubmission(s *submission, withBody bool) submissionView {
	at := s.submittedAt
	v := submissionView{ID: s.id, AssignmentID: s.assignment.id, StudentMemberID: s.student.id, Attempt: 1,
		State: "submitted", SubmittedAt: &at, CreatedAt: s.createdAt}
	if withBody {
		body := s.body
		v.Body = &body
	}
	return v
}

type workListIn struct {
	inCourse
	AssignmentID    *uuid.UUID `json:"assignment_id,omitempty"`
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty"`
	pageIn
}

func (in workListIn) matches(student, assignmentID string) bool {
	return (in.StudentMemberID == nil || in.StudentMemberID.String() == student) &&
		(in.AssignmentID == nil || in.AssignmentID.String() == assignmentID)
}

func submissionList() *impl {
	return define(spec[workListIn]{
		gate:    gateSubmissions,
		resolve: func(*Core, *course, workListIn) (target, error) { return target{typ: "submission"}, nil },
		query: func(_ *Core, rc *readCtx, in workListIn) (any, error) {
			limit, after := pageLimit(in.Limit), in.after()
			out := struct {
				Submissions []submissionView `json:"submissions"`
				Next        *string          `json:"next,omitempty"`
			}{Submissions: []submissionView{}}
			for _, s := range rc.course.submissions {
				if len(out.Submissions) < limit && s.id > after && in.matches(s.student.id, s.assignment.id) &&
					inScope(rc.member, s.student.id, s.assignment.id) {
					out.Submissions = append(out.Submissions, viewSubmission(s, false))
				}
			}
			if n := len(out.Submissions); n > 0 && n == limit {
				out.Next = &out.Submissions[n-1].ID
			}
			return out, nil
		},
	})
}

type submissionGetIn struct {
	inCourse
	SubmissionID uuid.UUID `json:"submission_id"`
}

func findSubmission(co *course, id uuid.UUID) *submission {
	for _, s := range co.submissions {
		if s.id == id.String() {
			return s
		}
	}
	return nil
}

func submissionGet() *impl {
	return define(spec[submissionGetIn]{
		gate: gateSubmissions,
		resolve: func(_ *Core, co *course, in submissionGetIn) (target, error) {
			s := findSubmission(co, in.SubmissionID)
			if s == nil {
				return target{}, missing("no such submission in this course")
			}
			return target{typ: "submission", id: &s.id, scope: scope{students: []string{s.student.id}, assignments: []string{s.assignment.id}}}, nil
		},
		query: func(_ *Core, rc *readCtx, in submissionGetIn) (any, error) {
			return viewSubmission(findSubmission(rc.course, in.SubmissionID), true), nil
		},
	})
}

type gradeView struct {
	ID                string     `json:"id"`
	StudentMemberID   string     `json:"student_member_id"`
	SubmissionID      *string    `json:"submission_id,omitempty"`
	AssignmentID      *string    `json:"assignment_id,omitempty"`
	Origin            string     `json:"origin"`
	Score             string     `json:"score"`
	Feedback          *string    `json:"feedback,omitempty"`
	GraderMemberID    string     `json:"grader_member_id"`
	CreatedByActionID string     `json:"created_by_action_id"`
	State             string     `json:"state"`
	PostedAt          *time.Time `json:"posted_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

func viewGrade(g *grade) gradeView {
	at := g.postedAt
	v := gradeView{ID: g.id, StudentMemberID: g.student.id, AssignmentID: &g.assignment.id, Origin: "entered", Score: g.score,
		GraderMemberID: g.grader.id, CreatedByActionID: g.actionID, State: "posted", PostedAt: &at, CreatedAt: g.createdAt}
	if g.submission != nil {
		v.SubmissionID = &g.submission.id
	}
	if g.feedback != "" {
		fb := g.feedback
		v.Feedback = &fb
	}
	return v
}

func gradeList() *impl {
	return define(spec[workListIn]{
		gate:    gateGrades,
		resolve: func(*Core, *course, workListIn) (target, error) { return target{typ: "grade"}, nil },
		query: func(_ *Core, rc *readCtx, in workListIn) (any, error) {
			limit, after := pageLimit(in.Limit), in.after()
			out := struct {
				Grades []gradeView `json:"grades"`
				Next   *string     `json:"next,omitempty"`
			}{Grades: []gradeView{}}
			for _, g := range rc.course.grades {
				if len(out.Grades) < limit && g.id > after && in.matches(g.student.id, g.assignment.id) &&
					inScope(rc.member, g.student.id, g.assignment.id) {
					out.Grades = append(out.Grades, viewGrade(g))
				}
			}
			if n := len(out.Grades); n > 0 && n == limit {
				out.Next = &out.Grades[n-1].ID
			}
			return out, nil
		},
	})
}

type gradeGetIn struct {
	inCourse
	GradeID uuid.UUID `json:"grade_id"`
}

func findGrade(co *course, id uuid.UUID) *grade {
	for _, g := range co.grades {
		if g.id == id.String() {
			return g
		}
	}
	return nil
}

func gradeGet() *impl {
	return define(spec[gradeGetIn]{
		gate: gateGrades,
		resolve: func(_ *Core, co *course, in gradeGetIn) (target, error) {
			g := findGrade(co, in.GradeID)
			if g == nil {
				return target{}, missing("no such grade in this course")
			}
			return target{typ: "grade", id: &g.id, scope: scope{students: []string{g.student.id}, assignments: []string{g.assignment.id}}}, nil
		},
		query: func(_ *Core, rc *readCtx, in gradeGetIn) (any, error) {
			return viewGrade(findGrade(rc.course, in.GradeID)), nil
		},
	})
}

type componentView struct {
	ID         string  `json:"id"`
	ParentID   *string `json:"parent_id,omitempty"`
	Name       string  `json:"name"`
	Weight     string  `json:"weight"`
	DropLowest int     `json:"drop_lowest"`
	SortOrder  int     `json:"sort_order"`
}

func componentTree() *impl {
	return define(spec[courseIn]{
		gate:    gateGrades,
		resolve: func(*Core, *course, courseIn) (target, error) { return target{typ: "component"}, nil },
		query: func(_ *Core, rc *readCtx, _ courseIn) (any, error) {
			out := struct {
				Components []componentView `json:"components"`
			}{Components: []componentView{}}
			for _, cp := range rc.course.components {
				v := componentView{ID: cp.id, Name: cp.name, Weight: cp.weight, SortOrder: cp.sortOrder}
				if cp.parent != nil {
					v.ParentID = &cp.parent.id
				}
				out.Components = append(out.Components, v)
			}
			return out, nil
		},
	})
}

type gradebookIn struct {
	inCourse
	StudentMemberID     uuid.UUID `json:"student_member_id"`
	TreatUngradedAsZero bool      `json:"treat_ungraded_as_zero,omitempty"`
}

type gradebookItem struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	Fraction *string `json:"fraction"`
	Weight   string  `json:"weight"`
	Dropped  bool    `json:"dropped"`
}

type gradebookLine struct {
	ComponentID string          `json:"component_id"`
	Name        string          `json:"name"`
	Percent     *string         `json:"percent"`
	Fraction    *string         `json:"fraction"`
	Complete    bool            `json:"complete"`
	Items       []gradebookItem `json:"items"`
}

// gradebookGet rolls one student's posted grades up the canned tree: the
// bucket holds the course's assignments, the root holds the bucket. The
// arithmetic is simple on purpose; the fake's business is who may read it.
func gradebookGet() *impl {
	return define(spec[gradebookIn]{
		gate: gateGrades,
		resolve: func(c *Core, co *course, in gradebookIn) (target, error) {
			m := c.members[in.StudentMemberID.String()]
			if m == nil || m.course != co {
				return target{}, missing("no such member in this course")
			}
			return target{typ: "gradebook", id: &m.id, scope: scope{students: []string{m.id}, spans: true}}, nil
		},
		query: func(_ *Core, rc *readCtx, in gradebookIn) (any, error) {
			co, student := rc.course, in.StudentMemberID.String()
			bucket := gradebookLine{ComponentID: co.bucket.id, Name: co.bucket.name, Items: []gradebookItem{}}
			complete := true
			var got, possible float64
			for _, a := range co.assignments {
				item := gradebookItem{ID: a.id, Kind: "assignment", Weight: a.points}
				for _, g := range co.grades {
					if g.student.id == student && g.assignment == a {
						f := ratio(g.score, a.points)
						item.Fraction = &f
						got, possible = got+parseDecimal(g.score), possible+parseDecimal(a.points)
					}
				}
				if item.Fraction == nil {
					complete = false
					if in.TreatUngradedAsZero {
						zero := "0"
						item.Fraction, possible = &zero, possible+parseDecimal(a.points)
					}
				}
				bucket.Items = append(bucket.Items, item)
			}
			bucket.Complete = complete
			if possible > 0 {
				f, p := formatDecimal(got/possible), formatDecimal(100*got/possible)
				bucket.Fraction, bucket.Percent = &f, &p
			}
			root := gradebookLine{ComponentID: co.rootComponent.id, Name: co.rootComponent.name, Fraction: bucket.Fraction,
				Percent: bucket.Percent, Complete: complete,
				Items: []gradebookItem{{ID: co.bucket.id, Kind: "component", Fraction: bucket.Fraction, Weight: co.bucket.weight}}}
			return struct {
				StudentMemberID string          `json:"student_member_id"`
				Components      []gradebookLine `json:"components"`
			}{student, []gradebookLine{root, bucket}}, nil
		},
	})
}
