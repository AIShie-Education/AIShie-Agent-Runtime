package fakecore

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// grade.submit, as Core's tools/grade.go carries it out: a draft grade for a
// student's work, which replaces any earlier draft of the same work, and
// which a proposal may carry. Its since is Core's: approving a proposal of
// it is refused when a newer draft of the work was entered while it waited
// (noNewerDraft), and so is its proposer's owner, asked whether it is
// theirs to decide. The canned course gives it what it holds: work handed
// in on its assignment, which has no rubric, and a grade tree whose two
// components are rolled up, so that a grade on either is refused as Core
// refuses one; and no uploads, so that a feedback file is refused as a
// document's are (errNoDocumentUploads). grade.post, which posts a draft,
// is not carried out here.

// decimal is a decimal number as Core's decimal.Decimal takes and gives
// it: read exactly from a JSON number or a string, written as a string
// with no exponent and no trailing zeros.
type decimal struct{ r *big.Rat }

func (d *decimal) UnmarshalJSON(b []byte) error {
	s := string(b)
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return fmt.Errorf("can't convert %s to decimal", s)
	}
	d.r = r
	return nil
}

func (d decimal) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// rat is the number, 0 for none.
func (d decimal) rat() *big.Rat {
	if d.r == nil {
		return new(big.Rat)
	}
	return d.r
}

// String writes the number out in full: a decimal's denominator is made
// of twos and fives, so as many places as the more of them say it exactly.
func (d decimal) String() string {
	r := d.rat()
	den := new(big.Int).Set(r.Denom())
	places := 0
	for _, p := range []int64{2, 5} {
		n, q, m := 0, new(big.Int), new(big.Int)
		for {
			q.QuoRem(den, big.NewInt(p), m)
			if m.Sign() != 0 {
				break
			}
			den.Set(q)
			n++
		}
		places = max(places, n)
	}
	s := r.FloatString(places)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}

func (d decimal) negative() bool { return d.rat().Sign() < 0 }

func (d decimal) cmp(o decimal) int { return d.rat().Cmp(o.rat()) }

// decimalOf is a decimal the fake keeps as a string, such as an
// assignment's points.
func decimalOf(s string) decimal {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return decimal{}
	}
	return decimal{r}
}

type feedbackFile struct {
	Title       string `json:"title"`
	UploadToken string `json:"upload_token"`
	Filename    string `json:"filename,omitempty"`
}

type breakdownItem struct {
	Criterion string  `json:"criterion"`
	Points    decimal `json:"points"`
	Max       decimal `json:"max"`
	Comment   *string `json:"comment,omitempty"`
}

// gradeContent is a grade's own part (Core's GradeContent, which
// grade.regrade takes too).
type gradeContent struct {
	Score           decimal         `json:"score"`
	Feedback        *string         `json:"feedback,omitempty"`
	Breakdown       []breakdownItem `json:"breakdown,omitempty"`
	RubricVersionID *uuid.UUID      `json:"rubric_version_id,omitempty"`
	NoRubric        bool            `json:"no_rubric,omitempty"`
	OutOf           *decimal        `json:"out_of,omitempty"`
	AllowExtra      bool            `json:"allow_extra,omitempty"`
	FeedbackFiles   []feedbackFile  `json:"feedback_files,omitempty"`
}

// check holds a grade to what it may say whatever it is a grade of (Core's
// GradeContent.check): no score or breakdown points below zero, a rubric
// version named or none, not both, and feedback files each with a title,
// none listed twice.
func (g gradeContent) check() error {
	if g.Score.negative() {
		return invalid("score cannot be negative")
	}
	for _, b := range g.Breakdown {
		if b.Points.negative() || b.Max.negative() {
			return invalid("breakdown points cannot be negative")
		}
	}
	if g.NoRubric && g.RubricVersionID != nil {
		return invalid("give rubric_version_id or no_rubric, not both")
	}
	seen := map[string]bool{}
	for _, f := range g.FeedbackFiles {
		if strings.TrimSpace(f.Title) == "" {
			return invalid("every feedback file needs a title")
		}
		if seen[f.UploadToken] {
			return invalid("the same upload is listed twice")
		}
		seen[f.UploadToken] = true
	}
	return nil
}

type gradeSubmitIn struct {
	inCourse
	SubmissionID    *uuid.UUID `json:"submission_id,omitempty"`
	ComponentID     *uuid.UUID `json:"component_id,omitempty"`
	StudentMemberID *uuid.UUID `json:"student_member_id,omitempty"`
	ForMissing      *bool      `json:"for_missing,omitempty"`
	gradeContent
}

// gradeSubject is what a grade is for: a submission, or a component for a
// student (Core's gradeSubject).
type gradeSubject struct {
	student    *member
	submission *submission
	assignment *assignment
	component  *component
}

// pointsPossible is what the work is worth: the assignment's points. The
// canned tree's components are rolled up and worth nothing of their own,
// which checkSubject refuses before this is asked.
func (s gradeSubject) pointsPossible() decimal {
	if s.assignment != nil {
		return decimalOf(s.assignment.points)
	}
	return decimal{}
}

func (s gradeSubject) target() target {
	t := target{scope: scope{students: []string{s.student.id}}}
	if s.submission != nil {
		t.typ, t.id = "submission", &s.submission.id
		t.scope.assignments = []string{s.assignment.id}
	} else {
		t.typ, t.id = "grade_component", &s.component.id
		t.scope.spans = true
	}
	return t
}

// loadSubject finds what in grades, as Core's loadSubject does.
func (c *Core) loadSubject(co *course, in gradeSubmitIn) (gradeSubject, error) {
	var s gradeSubject
	switch {
	case (in.SubmissionID == nil) == (in.ComponentID == nil):
		return s, invalid("give exactly one of submission_id and component_id")
	case in.SubmissionID != nil:
		if in.StudentMemberID != nil {
			return s, invalid("student_member_id goes with component_id; a submission already names its student")
		}
		sub := findSubmission(co, *in.SubmissionID)
		if sub == nil {
			return s, missing("no such submission in this course")
		}
		s.student, s.submission, s.assignment = sub.student, sub, sub.assignment
	default:
		if in.StudentMemberID == nil {
			return s, invalid("student_member_id is required with component_id")
		}
		var cp *component
		for _, x := range co.components {
			if x.id == in.ComponentID.String() {
				cp = x
			}
		}
		if cp == nil {
			return s, missing("no such grade component in this course")
		}
		m := c.members[in.StudentMemberID.String()]
		if m == nil || m.course != co {
			return s, missing("no such member in this course")
		}
		s.student, s.component = m, cp
	}
	return s, nil
}

// checkSubject holds the rules about what may be graded at all (Core's
// checkSubject): work that is what the grade was given for, handed in or
// not, as forMissing says; a component that is a leaf with points of its
// own. The canned work is all handed in, and the canned components are
// all rolled up.
func checkSubject(s gradeSubject, forMissing *bool) error {
	if s.submission != nil {
		if forMissing != nil && *forMissing {
			return precondition("this grade was given for nothing handed in, and there is work here now; look at it, and grade it again")
		}
		return nil
	}
	return precondition("this component is rolled up from what is beneath it and takes no grade of its own")
}

// checkContent holds the rules about the grade itself (Core's
// checkContent): out of what the work is worth now, no more than it unless
// allow_extra, and no rubric version where there is no rubric, as the
// canned assignment has none.
func checkContent(s gradeSubject, g gradeContent) error {
	most := s.pointsPossible()
	if g.OutOf != nil && g.OutOf.cmp(most) != 0 {
		return precondition("the score was given out of %s, and the work is worth %s now; grade it again out of what it is worth", *g.OutOf, most)
	}
	if g.Score.cmp(most) > 0 && !g.AllowExtra {
		return precondition("score %s is above the %s points possible; set allow_extra to permit it", g.Score, most)
	}
	if g.RubricVersionID != nil {
		return precondition("there is no rubric here for rubric_version_id to be a version of")
	}
	return nil
}

// liveDraft reports whether g is a draft not yet replaced: entered, not
// posted, not superseded.
func (g *grade) liveDraft() bool { return g.postedAt == nil && g.supersededBy == nil }

// noNewerDraft refuses to replace a draft entered after the proposal of this
// grade was made, at proposedAt (Core's noNewerDraft). A direct call is as
// new as anything: it applies to a proposal, as it is approved (since, and
// execute) and as its proposer's owner is told whether it is theirs to
// decide.
func noNewerDraft(co *course, s gradeSubject, proposedAt time.Time) error {
	for _, g := range co.grades {
		if g.submission != nil && g.submission == s.submission && g.liveDraft() && g.createdAt.After(proposedAt) {
			return precondition("a newer draft was entered for this work after this grade was proposed; look at it, and propose again if it should still be replaced")
		}
	}
	return nil
}

func gradeSubmit() *impl {
	return define(spec[gradeSubmitIn]{
		gate: gate{perms: []string{permGradeSubmit}},
		check: func(in gradeSubmitIn) error {
			if in.ForMissing != nil && in.SubmissionID == nil {
				return invalid("for_missing is for a grade on a submission")
			}
			return in.check()
		},
		resolve: func(c *Core, co *course, in gradeSubmitIn) (target, error) {
			s, err := c.loadSubject(co, in)
			if err != nil {
				return target{}, err
			}
			return s.target(), nil
		},
		validate: func(c *Core, m *member, in gradeSubmitIn) error {
			s, err := c.loadSubject(m.course, in)
			if err != nil {
				return err
			}
			// The feedback files first, as Core claims their uploads
			// first: the fake hands out none.
			if len(in.FeedbackFiles) > 0 {
				return errNoDocumentUploads
			}
			if err := checkSubject(s, in.ForMissing); err != nil {
				return err
			}
			return checkContent(s, in.gradeContent)
		},
		// A draft entered while the proposal waited has been in front of
		// nobody who asked for this one to replace it.
		since: func(c *Core, proposedAt time.Time, in gradeSubmitIn) error {
			co := c.courses[in.CourseID.String()]
			s, err := c.loadSubject(co, in)
			if err != nil {
				return err
			}
			return noNewerDraft(co, s, proposedAt)
		},
		// A proposal records what the grade is against as it stands when
		// it is made: the work handed in, out of what it is worth, and no
		// rubric, there being none (Core's Pin and pinContent).
		pin: func(c *Core, m *member, in gradeSubmitIn) (gradeSubmitIn, error) {
			s, err := c.loadSubject(m.course, in)
			if err != nil {
				return in, err
			}
			if s.submission != nil {
				handedIn := false
				in.ForMissing = &handedIn
			}
			if in.OutOf == nil {
				most := s.pointsPossible()
				in.OutOf = &most
			}
			if in.RubricVersionID != nil || s.assignment == nil {
				return in, nil
			}
			if in.NoRubric {
				return in, checkContent(s, in.gradeContent)
			}
			in.NoRubric = true
			return in, nil
		},
		execute: func(c *Core, ec *execCtx, in gradeSubmitIn) (any, error) {
			s, err := c.loadSubject(ec.course, in)
			if err != nil {
				return nil, err
			}
			if s.submission == nil {
				// Refused by validate, which runs first, call or approval.
				return nil, checkSubject(s, in.ForMissing)
			}
			if err := checkContent(s, in.gradeContent); err != nil {
				return nil, err
			}
			var breakdown json.RawMessage
			if len(in.Breakdown) > 0 {
				if breakdown, err = json.Marshal(in.Breakdown); err != nil {
					return nil, err
				}
			}
			// A new draft replaces earlier ones, but it is as old as the
			// call that made it: a proposal approved on Wednesday does not
			// replace a draft somebody entered on Tuesday.
			if ec.approved {
				if err := noNewerDraft(ec.course, s, ec.createdAt); err != nil {
					return nil, err
				}
			}
			id := newID()
			for _, g := range ec.course.grades {
				if g.submission == s.submission && g.liveDraft() {
					g.supersededBy = &id
				}
			}
			// Dated when the call was made, not when it was approved: the
			// next proposal measures itself against this draft.
			g := &grade{id: id, student: s.student, assignment: s.assignment, submission: s.submission, grader: ec.member,
				actionID: ec.actionID, score: in.Score.String(), feedback: in.Feedback, breakdown: breakdown, createdAt: ec.createdAt}
			ec.course.grades = append(ec.course.grades, g)
			ec.emit(&event{typ: "grade.created", course: ec.course, subjectType: "grade", subjectID: &g.id, student: s.student.id,
				assignment: s.assignment.id})
			return struct {
				GradeID string `json:"grade_id"`
			}{id}, nil
		},
	})
}

// seesDrafts: drafts and what they replaced are for those who grade;
// everyone else sees posted grades that stand, and nothing else (Core's
// seesDrafts).
func seesDrafts(m *member) bool {
	return m.perm(permGradeSubmit).allowed() || m.perm(permGradePost).allowed()
}
