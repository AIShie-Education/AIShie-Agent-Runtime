package fakecore

import (
	"encoding/json"
	"fmt"
	"time"
)

// level is one rung of Core's ladder, which is both permission and autonomy.
type level int

const (
	denied level = iota
	confirmRequired
	pendingReview
	autonomous
)

var levelNames = [...]string{"denied", "confirm_required", "pending_review", "autonomous"}

func (l level) String() string {
	if l < denied || l > autonomous {
		return fmt.Sprintf("level(%d)", int(l))
	}
	return levelNames[l]
}

func (l level) allowed() bool { return l > denied }

func parseLevel(s string) (level, error) {
	for i, n := range levelNames {
		if n == s {
			return level(i), nil
		}
	}
	return denied, fmt.Errorf("fakecore: %q is not a level; use denied, confirm_required, pending_review or autonomous", s)
}

// The permissions, in Core's column order (domain.AllPerms).
const (
	permDocumentRead       = "document_read"
	permDocumentReadDraft  = "document_read_draft"
	permDocumentWrite      = "document_write"
	permRubricRead         = "rubric_read"
	permAssignmentWrite    = "assignment_write"
	permSubmissionRead     = "submission_read"
	permSubmissionWrite    = "submission_write"
	permGradeRead          = "grade_read"
	permGradeSubmit        = "grade_submit"
	permGradePost          = "grade_post"
	permMemberRead         = "member_read"
	permMemberManage       = "member_manage"
	permActionDecide       = "action_decide"
	permAgentDelegate      = "agent_delegate"
	permConversationAsk    = "conversation_ask"
	permConversationAnswer = "conversation_answer"
	permMemberInvite       = "member_invite"
)

var allPerms = []string{
	permDocumentRead, permDocumentReadDraft, permDocumentWrite, permRubricRead,
	permAssignmentWrite, permSubmissionRead, permSubmissionWrite, permGradeRead,
	permGradeSubmit, permGradePost, permMemberRead, permMemberManage, permActionDecide,
	permAgentDelegate, permConversationAsk, permConversationAnswer, permMemberInvite,
}

func validPerm(p string) bool {
	for _, q := range allPerms {
		if p == q {
			return true
		}
	}
	return false
}

// preset is one of Core's built-in permission presets (src/seed/presets.sql).
type preset struct {
	role            string
	studentScope    string
	assignmentScope string
	levels          [17]level // in allPerms order
}

// ladder reads a preset's levels written one letter each, in allPerms order:
// d denied, c confirm_required, p pending_review, a autonomous.
func ladder(s string) [17]level {
	var out [17]level
	for i := range out {
		switch s[i] {
		case 'c':
			out[i] = confirmRequired
		case 'p':
			out[i] = pendingReview
		case 'a':
			out[i] = autonomous
		}
	}
	return out
}

// presets are Core's built-ins, level for level: member_invite, the last,
// an instructor's alone; conversation_answer, the one before, no person's.
var presets = map[string]preset{
	"student":      {"student", scopeListed, scopeAll, ladder("addddaaadddddcadd")},
	"observer":     {"observer", scopeAll, scopeAll, ladder("adddddddddadddddd")},
	"ta":           {"ta", scopeAll, scopeAll, ladder("aadadadaadaddcadd")},
	"instructor":   {"instructor", scopeAll, scopeAll, ladder("aaaaaaaaaaaaaaada")},
	"tutor":        {"assistant", scopeListed, scopeAll, ladder("addddadadddddddad")},
	"grader":       {"assistant", scopeAll, scopeListed, ladder("addadaddcdddddddd")},
	"delegate":     {"assistant", scopeListed, scopeAll, ladder("addddadadddddddad")},
	"course_tutor": {"assistant", scopeListed, scopeAll, ladder("addddddddddddddad")},
}

const (
	scopeAll    = "all"
	scopeListed = "listed"

	statusActive    = "active"
	statusPaused    = "paused"
	statusRemoved   = "removed"
	statusSuspended = "suspended"
	statusArchived  = "archived"
)

// actor is a person, an agent or the system.
type actor struct {
	id     string
	kind   string // human, agent or system
	name   string
	status string // active or suspended
	// owner is the person who owns an agent; an owned agent acts only as its
	// owner's delegate.
	owner *actor
}

func (a *actor) active() bool { return a.status == statusActive }

// credential is an API token, as Core's credential row keeps one: its id,
// the public prefix it is listed by, who issued it and for what, and when
// it was made, last used, and revoked.
type credential struct {
	id        string
	token     string
	prefix    string
	actor     *actor
	issuer    *actor
	label     string
	createdAt time.Time
	revokedAt *time.Time
	lastUsed  *time.Time
}

// revoked reports whether the credential has been revoked.
func (cr *credential) revoked() bool { return cr.revokedAt != nil }

// course is a course and its canned material.
type course struct {
	id, code, section, title, status string
	deptID, termID                   string
	createdAt                        time.Time

	rootComponent, bucket *component
	components            []*component
	assignments           []*assignment
	documents             []*document
	submissions           []*submission
	grades                []*grade
	// events are the course's feed, in the order of their seq.
	events []*event
}

type component struct {
	id, name  string
	parent    *component
	weight    string
	sortOrder int
}

type assignment struct {
	id, title    string
	component    *component
	instructions *document
	points       string
	dueAt        *time.Time
	publishedAt  *time.Time
}

// document is a document with one version, published unless it is a
// draft: text, or a file. A document made by document.create starts as a
// draft, as Core's material, instructions and rubrics do, and a draft
// without text has no version.
type document struct {
	id, kind, title  string
	course           *course
	sortOrder        int
	createdAt        time.Time
	draft            bool
	versionID        string
	authorMemberID   string
	bodyMD           *string
	file             []byte
	contentType      *string
	fileToken        string
	submission       *submission
	grade            *grade
	versionCreatedAt time.Time
}

type submission struct {
	id          string
	assignment  *assignment
	student     *member
	body        string
	createdAt   time.Time
	submittedAt time.Time
}

type grade struct {
	id         string
	student    *member
	assignment *assignment
	submission *submission
	grader     *member
	actionID   string
	score      string
	feedback   string
	createdAt  time.Time
	postedAt   time.Time
}

// member is one actor's seat in one course.
type member struct {
	id              string
	course          *course
	actor           *actor
	status          string // active, paused or removed
	expiresAt       *time.Time
	role            string
	studentScope    string
	assignmentScope string
	students        map[string]bool // listed students, by member id
	assignments     map[string]bool // listed assignments, by id
	perms           map[string]level
	principal       *member
	answersCourse   bool
	// presetID is the preset it was seated from, as Core keeps it; ""
	// for none. createdAt is when it was seated.
	presetID  string
	createdAt time.Time
}

type conversation struct {
	id           string
	course       *course
	opener       *member
	respondent   *member
	title        *string
	status       string // open or closed
	closedReason *string
	createdAt    time.Time
	lastAt       *time.Time
	lastAuthor   *member
	messages     []*message
	// answers are the conversation.answer actions aimed at it, in order.
	answers []*action
	// draft is its answer's draft, as conversation.draft last kept it, and
	// draftTimes when it was written in the last second (draft.go).
	draft      *draft
	draftTimes []time.Time
}

type message struct {
	id         string
	conv       *conversation
	seq        int32
	author     *member
	inReplyTo  *string
	body       string
	createdAt  time.Time
	actionID   string
	retraction *retraction
}

type retraction struct {
	at     time.Time
	by     *member
	reason *string
}

// action is one row of Core's action log: every write that was attempted.
type action struct {
	id          string
	actor       *actor
	member      *member
	course      *course
	actionType  string // the registry name: conversation.answer
	targetType  string
	targetID    *string
	payload     json.RawMessage
	hash        string
	key         string
	authz       level
	status      string
	result      json.RawMessage
	decidedBy   *member
	decidedAt   *time.Time
	reviewState string
	reviewedBy  *member
	reviewedAt  *time.Time
	executedAt  *time.Time
	createdAt   time.Time
}

// event is one entry of a course's feed.
type event struct {
	seq         int64
	typ         string
	course      *course
	actionID    *string
	subjectType string
	subjectID   *string
	payload     json.RawMessage
	occurredAt  time.Time
}

// Action statuses.
const (
	actExecuted  = "executed"
	actProposed  = "proposed"
	actDenied    = "denied"
	actFailed    = "failed"
	actRejected  = "rejected"
	actCancelled = "cancelled"
	actApproved  = "approved"
)
