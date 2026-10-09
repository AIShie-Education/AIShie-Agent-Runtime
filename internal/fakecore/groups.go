package fakecore

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Group assignments, as AIShie-Core b5d6b43 has them (its docs/schema.md
// §2.5a and §2.5b; tools/group.go, submission_group.go, grade_group.go,
// peer.go and peer_review.go): a course's group sets and their groups,
// read under Core's gates and naming rules (group_set_list, group_set_get);
// a group's work, one submission handed in for its members, frozen then,
// which submission_get, submission_list and submission_roster show as Core
// does and reach when the caller reaches any of its members; grade_submit
// on it as Core carries it out (grades.go: a group grade, and a draft for
// each member from it, adjusted or not, adjustments carried); grades that
// say so (group); a group assignment's peer form, read with the student's
// own task (peer_form_get), and its results, for those who grade
// (peer_review_results, canned: no sheet is ever written here); and a peer
// evaluation refused to an agent (peer_review_submit, people_only).
//
// The tests set groups up with controls (AddGroupSet, AddGroupAssignment,
// HandInGroupWork, SetPeerForm, PostGrades), as a teacher and a student
// would through the front end. Forming groups, signing up, drafts of a
// group's work, correcting its members, adjusting one member's grade,
// writing a peer form or a sheet, and counting peer evaluation are not
// carried out: those tools are refused not_implemented, as every tool the
// fake does not carry out is.

// groupSet is a course's set of groups (Core's group_set).
type groupSet struct {
	id, name   string
	course     *course
	signupOpen bool
	closesAt   *time.Time
	createdAt  time.Time
	updatedAt  time.Time
	groups     []*group
	// stays are every stay in a group of the set, in the order begun; the
	// fake's never end.
	stays []*stay
}

// group is one group of a set (Core's course_group).
type group struct {
	id, name  string
	set       *groupSet
	capacity  *int
	createdAt time.Time
}

// stay is a student's stay in a group (Core's group_membership).
type stay struct {
	id        string
	group     *group
	member    *member
	joinedAt  time.Time
	joinedBy  *member
	joinedHow string
}

// groupGrade is what a group was given for its work, once (Core's
// group_grade): each member's grade is given from it.
type groupGrade struct {
	id         string
	submission *submission
	score      decimal
	allowExtra bool
	createdAt  time.Time
}

// adjustment is a member's adjustment of a grade given from a group grade:
// replace (a score of their own) or delta (plus or minus the group's), why,
// and who made it.
type adjustment struct {
	kind   string
	points decimal
	reason string
	by     *member
}

// peerForm is a group assignment's peer form (Core's peer_form): how its
// groups' members evaluate each other, and when.
type peerForm struct {
	kind      string
	opens     string
	opensAt   *time.Time
	closesAt  time.Time
	weight    int
	selfEval  bool
	share     string
	enabled   bool
	version   int
	updatedAt time.Time
}

// liveMembers are the group's members now: stays not ended of seats that
// are students', not removed, and not past their expiry (Core's
// live_group_members), in the order they joined.
func (g *group) liveMembers(now time.Time) []*member {
	var out []*member
	for _, st := range g.set.stays {
		m := st.member
		if st.group == g && m.role == "student" && m.status != statusRemoved && (m.expiresAt == nil || m.expiresAt.After(now)) {
			out = append(out, m)
		}
	}
	return out
}

// groupOf is the group of set m is in now, or nil.
func (set *groupSet) groupOf(m *member) *group {
	for _, st := range set.stays {
		if st.member == m {
			return st.group
		}
	}
	return nil
}

// GroupSpec is a group a control makes: its name, its capacity (nil for
// none), and the seats placed in it.
type GroupSpec struct {
	Name     string
	Capacity *int
	Members  []string
}

// GroupSet is a set a control made: its id, and its groups' ids in order.
type GroupSet struct {
	ID       string
	GroupIDs []string
}

// PeerForm is a peer form a control sets: kind share or rating (the fake
// keeps no criteria), opens on_hand_in or at OpensAt, closing at
// ClosesAt, counting at Weight percent.
type PeerForm struct {
	Kind           string
	Opens          string
	OpensAt        *time.Time
	ClosesAt       time.Time
	Weight         int
	SelfEvaluation bool
}

// teacherOf is the first seat of the course that writes assignments, who
// a control's groups are made and placed by.
func (c *Core) teacherOf(co *course) *member {
	for _, m := range c.memberList {
		if m.course == co && m.status == statusActive && m.perm(permAssignmentWrite).allowed() {
			return m
		}
	}
	return nil
}

// AddGroupSet makes a group set of the course with the groups given, in
// order, and places the seats each names in it, as a teacher's
// group_set.create, group.create and group.set_members do: each a current
// student of the course, in one group of the set.
func (c *Core) AddGroupSet(courseID, name string, groups ...GroupSpec) (GroupSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	co := c.courses[courseID]
	if co == nil {
		return GroupSet{}, fmt.Errorf("fakecore: AddGroupSet: no course %s", courseID)
	}
	teacher := c.teacherOf(co)
	if teacher == nil {
		return GroupSet{}, errors.New("fakecore: AddGroupSet: nobody in the course writes assignments")
	}
	now := c.now()
	set := &groupSet{id: newID(), name: name, course: co, createdAt: now, updatedAt: now}
	out := GroupSet{ID: set.id}
	placed := map[*member]bool{}
	for _, spec := range groups {
		g := &group{id: newID(), name: spec.Name, set: set, capacity: spec.Capacity, createdAt: now}
		set.groups = append(set.groups, g)
		out.GroupIDs = append(out.GroupIDs, g.id)
		for _, id := range spec.Members {
			m := c.members[id]
			if m == nil || m.course != co || m.role != "student" || m.status == statusRemoved {
				return GroupSet{}, fmt.Errorf("fakecore: AddGroupSet: %s is not a current student of the course", id)
			}
			if placed[m] {
				return GroupSet{}, fmt.Errorf("fakecore: AddGroupSet: %s is placed twice", id)
			}
			placed[m] = true
			set.stays = append(set.stays, &stay{id: newID(), group: g, member: m, joinedAt: now, joinedBy: teacher, joinedHow: "assigned"})
		}
	}
	co.groupSets = append(co.groupSets, set)
	return out, nil
}

func (c *Core) findSet(co *course, id string) *groupSet {
	for _, s := range co.groupSets {
		if s.id == id {
			return s
		}
	}
	return nil
}

// AddGroupAssignment adds a published group assignment worth points that
// uses the set, in the component the course's HW1 counts toward, as a
// teacher's assignment.create (group_set_id) and assignment.publish make
// one. It returns its id.
func (c *Core) AddGroupAssignment(courseID, title, points, setID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	co := c.courses[courseID]
	if co == nil {
		return "", fmt.Errorf("fakecore: AddGroupAssignment: no course %s", courseID)
	}
	set := c.findSet(co, setID)
	if set == nil {
		return "", fmt.Errorf("fakecore: AddGroupAssignment: no group set %s in the course", setID)
	}
	now := c.now()
	a := &assignment{id: newID(), title: title, component: co.bucket, points: points, publishedAt: &now, groupSet: set}
	co.assignments = append(co.assignments, a)
	return a.id, nil
}

// HandInGroupWork is a student starting their group's work on the group
// assignment and handing it in, as submission.create and
// submission.submit do: one submission of the group's, whose members are
// the group's live members then, less any whom another group's work for
// the assignment names. It returns the submission's id.
func (c *Core) HandInGroupWork(courseID, studentID, assignmentID, body string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	co := c.courses[courseID]
	if co == nil {
		return "", fmt.Errorf("fakecore: HandInGroupWork: no course %s", courseID)
	}
	student := c.members[studentID]
	if student == nil || student.course != co {
		return "", fmt.Errorf("fakecore: HandInGroupWork: no seat %s in the course", studentID)
	}
	var a *assignment
	for _, x := range co.assignments {
		if x.id == assignmentID {
			a = x
		}
	}
	if a == nil || a.groupSet == nil {
		return "", fmt.Errorf("fakecore: HandInGroupWork: no group assignment %s in the course", assignmentID)
	}
	g := a.groupSet.groupOf(student)
	if g == nil {
		return "", fmt.Errorf("fakecore: HandInGroupWork: %s is in no group of the assignment's set (no_group)", studentID)
	}
	for _, s := range co.submissions {
		if s.assignment == a && s.group == g {
			return "", errors.New("fakecore: HandInGroupWork: the group has handed work in already")
		}
	}
	now := c.now()
	s := &submission{id: newID(), assignment: a, group: g, submittedBy: student, body: body, createdAt: now, submittedAt: now}
	for _, m := range g.liveMembers(now) {
		if workOf(co, a, m) == nil {
			s.members = append(s.members, m)
		}
	}
	co.submissions = append(co.submissions, s)
	return s.id, nil
}

// workOf is the work for assignment a that m is part of, or nil.
func workOf(co *course, a *assignment, m *member) *submission {
	for _, s := range co.submissions {
		if s.assignment == a && s.has(m) {
			return s
		}
	}
	return nil
}

// SetPeerForm gives the group assignment a peer form, or replaces it, as a
// teacher's peer_form.set does: enabled, sharing nothing with students.
func (c *Core) SetPeerForm(assignmentID string, f PeerForm) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, co := range c.courses {
		for _, a := range co.assignments {
			if a.id != assignmentID {
				continue
			}
			if a.groupSet == nil {
				return errors.New("fakecore: SetPeerForm: not a group assignment (not_a_group_assignment)")
			}
			version := 1
			if a.peer != nil {
				version = a.peer.version + 1
			}
			a.peer = &peerForm{kind: f.Kind, opens: f.Opens, opensAt: f.OpensAt, closesAt: f.ClosesAt.UTC().Truncate(time.Microsecond),
				weight: f.Weight, selfEval: f.SelfEvaluation, share: "none", enabled: true, version: version, updatedAt: c.now()}
			return nil
		}
	}
	return fmt.Errorf("fakecore: SetPeerForm: no assignment %s", assignmentID)
}

// PostGrades posts draft grades, as grade.post does, from a seat that
// posts grades: each student's totals are written down. It writes no
// event.
func (c *Core) PostGrades(gradeIDs ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, id := range gradeIDs {
		var found *grade
		var in *course
		for _, co := range c.courses {
			for _, g := range co.grades {
				if g.id == id {
					found, in = g, co
				}
			}
		}
		switch {
		case found == nil:
			return fmt.Errorf("fakecore: PostGrades: no grade %s", id)
		case !found.liveDraft():
			return fmt.Errorf("fakecore: PostGrades: grade %s is not a live draft", id)
		}
		found.postedAt = &now
		in.wroteTotals(found.student)
	}
	return nil
}

// selfOf is whom a seat acts as for its own work: its principal, for a
// delegate (a student's own agent acts for the student), and itself
// otherwise (Core's selfOf).
func selfOf(m *member) *member {
	if m.principal != nil {
		return m.principal
	}
	return m
}

// reaches says whether reader's student scope, and a delegate's
// principal's, reaches the student.
func reaches(reader *member, student string) bool {
	return checkScope(reader, scope{students: []string{student}}) == ""
}

// reachesEvery says whether reader, and a delegate's principal, reach every
// student of the course.
func reachesEvery(reader *member) bool {
	return reader.studentScope == scopeAll && (reader.principal == nil || reader.principal.studentScope == scopeAll)
}

// byName orders seats as Core names them: by display name, then by id.
func byName(ms []*member) []*member {
	out := slices.Clone(ms)
	slices.SortFunc(out, func(a, b *member) int {
		if c := strings.Compare(a.actor.name, b.actor.name); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	return out
}

// byID orders seats by id.
func byID(ms []*member) []*member {
	out := slices.Clone(ms)
	slices.SortFunc(out, func(a, b *member) int { return strings.Compare(a.id, b.id) })
	return out
}

// workMember is one of a group's work's students, as a reader is shown
// them (Core's WorkMember).
type workMember struct {
	MemberID    string  `json:"member_id"`
	DisplayName *string `json:"display_name,omitempty"`
}

// workMembers are s's students as reader is shown them: by name to the
// work's own members (or a member's own agent) and to readers of the
// member list, in order of name; by id alone to anyone else, in order of
// id (Core's workMembers).
func workMembers(s *submission, reader *member) []workMember {
	students := s.students()
	named := reader.perm(permMemberRead).allowed() || slices.Contains(students, selfOf(reader))
	out := make([]workMember, 0, len(students))
	if !named {
		for _, m := range byID(students) {
			out = append(out, workMember{MemberID: m.id})
		}
		return out
	}
	for _, m := range byName(students) {
		name := m.actor.name
		out = append(out, workMember{MemberID: m.id, DisplayName: &name})
	}
	return out
}

// matchesWork says whether s is what submission.list's filters ask for:
// of the assignment, the work the student is part of, of the group.
func (in workListIn) matchesWork(s *submission) bool {
	return (in.AssignmentID == nil || in.AssignmentID.String() == s.assignment.id) &&
		(in.StudentMemberID == nil || slices.Contains(s.studentIDs(), in.StudentMemberID.String())) &&
		(in.GroupID == nil || (s.group != nil && in.GroupID.String() == s.group.id))
}

// readsWork says whether reader's student scope reaches any of s's
// students, as a list of a group's work filters it.
func readsWork(reader *member, s *submission) bool {
	return checkScope(reader, scope{anyStudents: true, anyOf: s.studentIDs()}) == ""
}

// ---------------------------------------------------------------------------
// group_set_list, group_set_get
// ---------------------------------------------------------------------------

type signupView struct {
	Open     bool       `json:"open"`
	ClosesAt *time.Time `json:"closes_at,omitempty"`
	Joinable bool       `json:"joinable"`
	Reason   *string    `json:"reason,omitempty"`
}

type groupMemberView struct {
	MemberID    string     `json:"member_id"`
	DisplayName *string    `json:"display_name,omitempty"`
	JoinedAt    *time.Time `json:"joined_at,omitempty"`
	JoinedHow   *string    `json:"joined_how,omitempty"`
}

type groupWorkView struct {
	AssignmentID string `json:"assignment_id"`
	Title        string `json:"title"`
	SubmissionID string `json:"submission_id"`
	Attempt      int    `json:"attempt"`
	State        string `json:"state"`
}

type groupView struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Capacity  *int              `json:"capacity,omitempty"`
	Size      int               `json:"size"`
	Full      bool              `json:"full"`
	CreatedAt time.Time         `json:"created_at"`
	Members   []groupMemberView `json:"members,omitempty"`
	Work      []groupWorkView   `json:"work,omitempty"`
}

type setAssignmentView struct {
	AssignmentID string `json:"assignment_id"`
	Title        string `json:"title"`
	Published    bool   `json:"published"`
}

type stayView struct {
	ID               string    `json:"id"`
	GroupID          string    `json:"group_id"`
	MemberID         string    `json:"member_id"`
	DisplayName      string    `json:"display_name"`
	JoinedAt         time.Time `json:"joined_at"`
	JoinedByMemberID string    `json:"joined_by_member_id"`
	JoinedHow        string    `json:"joined_how"`
}

type groupSetView struct {
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	Signup          signupView          `json:"signup"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	Groups          []groupView         `json:"groups"`
	Assignments     []setAssignmentView `json:"assignments"`
	MyGroupID       *string             `json:"my_group_id,omitempty"`
	UnassignedCount *int                `json:"unassigned_count,omitempty"`
	Unassigned      []groupMemberView   `json:"unassigned,omitempty"`
	History         []stayView          `json:"history,omitempty"`
}

// signupState says whether a student may sign up to the set now, and why
// not (Core's signupState).
func signupState(set *groupSet, now time.Time) signupView {
	v := signupView{Open: set.signupOpen, ClosesAt: set.closesAt}
	reason := ""
	switch {
	case set.course.status == statusArchived:
		reason = reasonCourseArchived
	case !set.signupOpen || (set.closesAt != nil && !now.Before(*set.closesAt)):
		reason = "signup_closed"
	}
	if reason == "" {
		v.Joinable = true
	} else {
		v.Reason = &reason
	}
	return v
}

// viewSet is set as reader may see it (Core's setViewer.views): members by
// name to a reader of the member list, of the students their scope
// reaches, and to anyone else their own group's; detail adds
// group_set_get's students in no group, and each group's latest work to a
// reader of submissions or of the member list; history every stay.
func viewSet(c *Core, set *groupSet, reader *member, now time.Time, detail, history bool) groupSetView {
	members := reader.perm(permMemberRead).allowed()
	work := members || reader.perm(permSubmissionRead).allowed()
	every := reachesEvery(reader)
	v := groupSetView{ID: set.id, Name: set.name, Signup: signupState(set, now), CreatedAt: set.createdAt, UpdatedAt: set.updatedAt,
		Groups: []groupView{}, Assignments: []setAssignmentView{}}
	visible := map[*assignment]bool{}
	for _, a := range set.course.assignments {
		if a.groupSet != set || (a.publishedAt == nil && !reader.perm(permAssignmentWrite).allowed()) ||
			checkScope(reader, scope{assignments: []string{a.id}}) != "" {
			continue
		}
		visible[a] = true
		v.Assignments = append(v.Assignments, setAssignmentView{AssignmentID: a.id, Title: a.title, Published: a.publishedAt != nil})
	}
	mine := set.groupOf(selfOf(reader))
	if mine != nil {
		v.MyGroupID = &mine.id
	}
	for _, g := range set.groups {
		live := g.liveMembers(now)
		gv := groupView{ID: g.id, Name: g.name, Capacity: g.capacity, Size: len(live), CreatedAt: g.createdAt,
			Full: g.capacity != nil && len(live) >= *g.capacity}
		reached := false
		switch {
		case members:
			for _, m := range byName(live) {
				if !reaches(reader, m.id) {
					continue
				}
				name, at, how := m.actor.name, set.stayOf(m).joinedAt, set.stayOf(m).joinedHow
				gv.Members = append(gv.Members, groupMemberView{MemberID: m.id, DisplayName: &name, JoinedAt: &at, JoinedHow: &how})
			}
			reached = len(gv.Members) > 0
		case g == mine:
			// A student is shown their own group's members, by name:
			// people who hand work in together know each other's names.
			for _, m := range byName(live) {
				name := m.actor.name
				gv.Members = append(gv.Members, groupMemberView{MemberID: m.id, DisplayName: &name})
			}
			reached = true
		}
		if detail && work && (every || reached) {
			for _, a := range set.course.assignments {
				if !visible[a] {
					continue
				}
				// The fake's work is handed in once: its latest attempt is
				// its first.
				for _, s := range set.course.submissions {
					if s.assignment == a && s.group == g && readsWork(reader, s) {
						gv.Work = append(gv.Work, groupWorkView{AssignmentID: a.id, Title: a.title, SubmissionID: s.id, Attempt: 1,
							State: "submitted"})
					}
				}
			}
		}
		v.Groups = append(v.Groups, gv)
	}
	if !members {
		return v
	}
	var unassigned []*member
	for _, m := range c.memberList {
		if m.course == set.course && m.role == "student" && m.status != statusRemoved && (m.expiresAt == nil || m.expiresAt.After(now)) &&
			set.groupOf(m) == nil && reaches(reader, m.id) {
			unassigned = append(unassigned, m)
		}
	}
	n := len(unassigned)
	v.UnassignedCount = &n
	if detail {
		for _, m := range byName(unassigned) {
			name := m.actor.name
			v.Unassigned = append(v.Unassigned, groupMemberView{MemberID: m.id, DisplayName: &name})
		}
	}
	if history {
		// The newest first; the fake's stays never end.
		for i := len(set.stays) - 1; i >= 0; i-- {
			st := set.stays[i]
			if reaches(reader, st.member.id) {
				v.History = append(v.History, stayView{ID: st.id, GroupID: st.group.id, MemberID: st.member.id, DisplayName: st.member.actor.name,
					JoinedAt: st.joinedAt, JoinedByMemberID: st.joinedBy.id, JoinedHow: st.joinedHow})
			}
		}
	}
	return v
}

// stayOf is m's stay in a group of the set now.
func (set *groupSet) stayOf(m *member) *stay {
	for _, st := range set.stays {
		if st.member == m {
			return st
		}
	}
	return nil
}

type groupSetListIn struct {
	inCourse
	IncludeArchived bool `json:"include_archived,omitempty"`
}

func groupSetList() *impl {
	return define(spec[groupSetListIn]{
		gate:    gateDocumentRead,
		resolve: func(*Core, *course, groupSetListIn) (target, error) { return target{typ: "group_set"}, nil },
		query: func(c *Core, rc *readCtx, _ groupSetListIn) (any, error) {
			out := struct {
				Sets []groupSetView `json:"sets"`
			}{Sets: []groupSetView{}}
			for _, set := range rc.course.groupSets {
				out.Sets = append(out.Sets, viewSet(c, set, rc.member, rc.now, false, false))
			}
			return out, nil
		},
	})
}

type groupSetGetIn struct {
	inCourse
	SetID          uuid.UUID `json:"set_id"`
	IncludeHistory bool      `json:"include_history,omitempty"`
}

func groupSetGet() *impl {
	return define(spec[groupSetGetIn]{
		gate: gateDocumentRead,
		resolve: func(c *Core, co *course, in groupSetGetIn) (target, error) {
			set := c.findSet(co, in.SetID.String())
			if set == nil {
				return target{}, missing("no such group set in this course")
			}
			return target{typ: "group_set", id: &set.id}, nil
		},
		query: func(c *Core, rc *readCtx, in groupSetGetIn) (any, error) {
			return viewSet(c, c.findSet(rc.course, in.SetID.String()), rc.member, rc.now, true, in.IncludeHistory), nil
		},
	})
}

// ---------------------------------------------------------------------------
// A member's grade from a group grade, as grade_get and grade_list show it
// ---------------------------------------------------------------------------

type adjustmentView struct {
	Kind       string      `json:"kind"`
	Points     json.Number `json:"points"`
	Reason     *string     `json:"reason,omitempty"`
	ByMemberID *string     `json:"by_member_id,omitempty"`
}

type gradeGroupView struct {
	GroupGradeID string          `json:"group_grade_id"`
	GroupID      *string         `json:"group_id,omitempty"`
	GroupName    *string         `json:"group_name,omitempty"`
	Score        json.Number     `json:"score"`
	Adjustment   *adjustmentView `json:"adjustment,omitempty"`
}

// view is the adjustment as a grade shows it: who made it only to those
// who grade (Core's forReader).
func (a *adjustment) view(graders bool) *adjustmentView {
	if a == nil {
		return nil
	}
	v := &adjustmentView{Kind: a.kind, Points: json.Number(a.points.String())}
	if a.reason != "" {
		r := a.reason
		v.Reason = &r
	}
	if a.by != nil && graders {
		v.ByMemberID = &a.by.id
	}
	return v
}

// viewGradeGroup is what a member's grade from a group grade says of it,
// nil for any other grade.
func viewGradeGroup(g *grade, graders bool) *gradeGroupView {
	if g.groupGrade == nil {
		return nil
	}
	s := g.groupGrade.submission
	return &gradeGroupView{GroupGradeID: g.groupGrade.id, GroupID: &s.group.id, GroupName: &s.group.name,
		Score: json.Number(g.groupGrade.score.String()), Adjustment: g.adjust.view(graders)}
}

// ---------------------------------------------------------------------------
// peer_form_get, peer_review_results, peer_review_submit
// ---------------------------------------------------------------------------

const (
	windowNotOpen = "not_open"
	windowOpen    = "open"
	windowClosed  = "closed"
)

type peerFormView struct {
	AssignmentID      string     `json:"assignment_id"`
	Enabled           bool       `json:"enabled"`
	Kind              string     `json:"kind"`
	SelfEvaluation    bool       `json:"self_evaluation"`
	Opens             string     `json:"opens"`
	OpensAt           *time.Time `json:"opens_at,omitempty"`
	ClosesAt          time.Time  `json:"closes_at"`
	Weight            int        `json:"weight"`
	ShareWithStudents string     `json:"share_with_students"`
	Version           int        `json:"version"`
	InUse             *bool      `json:"in_use,omitempty"`
	VisibleTo         []string   `json:"visible_to"`
	StudentsSee       []string   `json:"students_see"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// counts says whether the form moves grades: enabled, with a weight.
func (f *peerForm) counts() bool { return f != nil && f.enabled && f.weight > 0 }

// view is a's form as reader sees it: whether a sheet has been written
// (never, here) only to those who write assignments or grade, since on a
// form that opens on hand-in it would tell one group that another has
// handed in (Core's seesWhetherInUse).
func (f *peerForm) view(a *assignment, reader *member) peerFormView {
	v := peerFormView{AssignmentID: a.id, Enabled: f.enabled, Kind: f.kind, SelfEvaluation: f.selfEval, Opens: f.opens, OpensAt: f.opensAt,
		ClosesAt: f.closesAt, Weight: f.weight, ShareWithStudents: f.share, Version: f.version, UpdatedAt: f.updatedAt,
		VisibleTo: []string{"graders", "action_record"}, StudentsSee: []string{"own_sheet"}}
	if reader.perm(permAssignmentWrite).allowed() || seesDrafts(reader) {
		v.InUse = ptr(false)
	}
	if f.share == "own_average" {
		v.StudentsSee = append(v.StudentsSee, "own_average")
	}
	if f.counts() {
		v.StudentsSee = append(v.StudentsSee, "own_adjustment")
	}
	return v
}

// window is the form's window for a group: open once it opens (at
// opens_at, or once the group has handed in) until closes_at.
func (f *peerForm) window(handedIn bool, now time.Time) string {
	switch {
	case !now.Before(f.closesAt):
		return windowClosed
	case f.opens == "at" && f.opensAt != nil && now.Before(*f.opensAt):
		return windowNotOpen
	case f.opens == "on_hand_in" && !handedIn:
		return windowNotOpen
	}
	return windowOpen
}

// circle is a group's circle for an assignment: who evaluates whom, the
// members of its work handed in, or, with none, its members now less those
// another group's work names; and its work.
type circle struct {
	group   *group
	members []*member
	work    *submission
}

func circleOf(co *course, a *assignment, g *group, now time.Time) circle {
	c := circle{group: g}
	for _, s := range co.submissions {
		if s.assignment == a && s.group == g {
			c.work = s
		}
	}
	if c.work != nil {
		c.members = c.work.students()
	} else {
		for _, m := range g.liveMembers(now) {
			if workOf(co, a, m) == nil {
				c.members = append(c.members, m)
			}
		}
	}
	c.members = byID(c.members)
	return c
}

// circleFor is the circle student evaluates in for a, and whether it holds
// them: the one of the group whose work names them, or else of their group
// now (Core's circleFor).
func circleFor(co *course, a *assignment, student *member, now time.Time) (*circle, bool) {
	if a.groupSet == nil {
		return nil, false
	}
	if s := workOf(co, a, student); s != nil && s.group != nil {
		c := circleOf(co, a, s.group, now)
		return &c, true
	}
	g := a.groupSet.groupOf(student)
	if g == nil {
		return nil, false
	}
	c := circleOf(co, a, g, now)
	return &c, slices.Contains(c.members, student)
}

type circleMemberView struct {
	MemberID    string `json:"member_id"`
	DisplayName string `json:"display_name"`
}

type peerWindowView struct {
	State    string     `json:"state"`
	Opens    string     `json:"opens"`
	OpensAt  *time.Time `json:"opens_at,omitempty"`
	ClosesAt time.Time  `json:"closes_at"`
}

type peerTaskView struct {
	GroupID    string             `json:"group_id"`
	GroupName  string             `json:"group_name"`
	Circle     []circleMemberView `json:"circle"`
	ToEvaluate []string           `json:"to_evaluate"`
	Window     peerWindowView     `json:"window"`
}

func (f *peerForm) windowView(handedIn bool, now time.Time) peerWindowView {
	return peerWindowView{State: f.window(handedIn, now), Opens: f.opens, OpensAt: f.opensAt, ClosesAt: f.closesAt}
}

// peerAssignmentIn is the input of the tools about one assignment's peer
// evaluation.
type peerAssignmentIn struct {
	inCourse
	AssignmentID uuid.UUID `json:"assignment_id"`
}

func assignmentTarget(co *course, id uuid.UUID) (target, error) {
	a, err := findOrGone(co, id)
	if err != nil {
		return target{}, err
	}
	return target{typ: "assignment", id: &a.id, scope: scope{assignments: []string{a.id}}}, nil
}

// peerFormGet is Core's peer_form.get: the form, if the assignment has
// one, and, for a student in a group's circle or their own agent, their
// task. Nobody here has written a sheet.
func peerFormGet() *impl {
	return define(spec[peerAssignmentIn]{
		gate: gateDocumentRead,
		resolve: func(_ *Core, co *course, in peerAssignmentIn) (target, error) {
			return assignmentTarget(co, in.AssignmentID)
		},
		query: func(_ *Core, rc *readCtx, in peerAssignmentIn) (any, error) {
			a := findAssignment(rc.course, in.AssignmentID)
			if a.publishedAt == nil && !rc.member.perm(permAssignmentWrite).allowed() {
				return nil, missing("no such assignment in this course")
			}
			out := struct {
				Form *peerFormView `json:"form"`
				Task *peerTaskView `json:"task,omitempty"`
			}{}
			f := a.peer
			if f == nil {
				return out, nil
			}
			v := f.view(a, rc.member)
			out.Form = &v
			self := selfOf(rc.member)
			c, inCircle := circleFor(rc.course, a, self, rc.now)
			if c == nil || !inCircle {
				return out, nil
			}
			t := &peerTaskView{GroupID: c.group.id, GroupName: c.group.name, Circle: []circleMemberView{}, ToEvaluate: []string{},
				Window: f.windowView(c.work != nil, rc.now)}
			for _, m := range c.members {
				if m != self || f.selfEval {
					t.ToEvaluate = append(t.ToEvaluate, m.id)
				}
			}
			for _, m := range byName(c.members) {
				t.Circle = append(t.Circle, circleMemberView{MemberID: m.id, DisplayName: m.actor.name})
			}
			out.Task = t
			return out, nil
		},
	})
}

type peerResultsIn struct {
	inCourse
	AssignmentID uuid.UUID  `json:"assignment_id"`
	GroupID      *uuid.UUID `json:"group_id,omitempty"`
}

type peerMemberGradeView struct {
	GradeID        string      `json:"grade_id"`
	Score          json.Number `json:"score"`
	State          string      `json:"state"`
	AdjustmentKind *string     `json:"adjustment_kind,omitempty"`
}

type peerMemberResultView struct {
	MemberID    string               `json:"member_id"`
	DisplayName string               `json:"display_name"`
	Submitted   bool                 `json:"submitted"`
	RatedBy     []string             `json:"rated_by"`
	Factor      json.Number          `json:"factor"`
	Score       *json.Number         `json:"score,omitempty"`
	Grade       *peerMemberGradeView `json:"grade,omitempty"`
	Flags       []string             `json:"flags"`
}

type peerGroupResultView struct {
	GroupID        string                 `json:"group_id"`
	Name           string                 `json:"name"`
	SubmissionID   *string                `json:"submission_id,omitempty"`
	Window         peerWindowView         `json:"window"`
	GroupScore     *json.Number           `json:"group_score,omitempty"`
	PointsPossible json.Number            `json:"points_possible"`
	Flags          []string               `json:"flags"`
	Members        []peerMemberResultView `json:"members"`
	Sheets         []any                  `json:"sheets"`
}

// liveGroupGrade is the group grade the work's live member grades were
// given from, the newest, or nil.
func liveGroupGrade(co *course, s *submission) *groupGrade {
	var out *groupGrade
	for _, g := range co.grades {
		if g.submission == s && g.supersededBy == nil && g.groupGrade != nil {
			out = g.groupGrade
		}
	}
	return out
}

// liveGradeOf is m's live grade on s, their draft before a posted one.
func liveGradeOf(co *course, s *submission, m *member) *grade {
	var posted *grade
	for _, g := range co.grades {
		if g.submission != s || g.student != m || g.supersededBy != nil {
			continue
		}
		if g.postedAt == nil {
			return g
		}
		posted = g
	}
	return posted
}

// peerReviewResults is Core's peer_review.results for those who grade, of
// each group whose circle lies wholly within the reader's student scope,
// canned: nobody here writes a sheet, so every member is flagged missing,
// rated by nobody, at a factor of 1.
func peerReviewResults() *impl {
	return define(spec[peerResultsIn]{
		gate: gate{any: true, perms: []string{permGradeSubmit, permGradePost}},
		resolve: func(c *Core, co *course, in peerResultsIn) (target, error) {
			t, err := assignmentTarget(co, in.AssignmentID)
			if err != nil || in.GroupID == nil {
				return t, err
			}
			a := findAssignment(co, in.AssignmentID)
			g := a.groupOf(in.GroupID.String())
			if g == nil {
				for _, set := range co.groupSets {
					if slices.ContainsFunc(set.groups, func(x *group) bool { return x.id == in.GroupID.String() }) {
						return target{}, missing("no such group in this assignment's group set")
					}
				}
				return target{}, missing("no such group in this course")
			}
			for _, m := range circleOf(co, a, g, c.now()).members {
				t.scope.students = append(t.scope.students, m.id)
			}
			return t, nil
		},
		query: func(_ *Core, rc *readCtx, in peerResultsIn) (any, error) {
			a := findAssignment(rc.course, in.AssignmentID)
			f := a.peer
			if f == nil {
				return nil, precondition("this assignment has no peer form").with("reason", "no_peer_form")
			}
			out := struct {
				Form   peerFormView          `json:"form"`
				Groups []peerGroupResultView `json:"groups"`
			}{Form: f.view(a, rc.member), Groups: []peerGroupResultView{}}
			for _, g := range a.groupSet.groups {
				if in.GroupID != nil && in.GroupID.String() != g.id {
					continue
				}
				c := circleOf(rc.course, a, g, rc.now)
				if len(c.members) == 0 || slices.ContainsFunc(c.members, func(m *member) bool { return !reaches(rc.member, m.id) }) {
					continue
				}
				r := peerGroupResultView{GroupID: g.id, Name: g.name, Window: f.windowView(c.work != nil, rc.now),
					PointsPossible: json.Number(decimalOf(a.points).String()), Flags: []string{}, Members: []peerMemberResultView{},
					Sheets: []any{}}
				if len(c.members) == 2 && !f.selfEval {
					r.Flags = append(r.Flags, "pair_without_self_evaluation")
				}
				var gg *groupGrade
				if c.work != nil {
					r.SubmissionID = &c.work.id
					if gg = liveGroupGrade(rc.course, c.work); gg != nil {
						score := json.Number(gg.score.String())
						r.GroupScore = &score
					}
				}
				for _, m := range byName(c.members) {
					mr := peerMemberResultView{MemberID: m.id, DisplayName: m.actor.name, RatedBy: []string{}, Factor: "1",
						Flags: []string{"missing"}}
					if gg != nil {
						score := json.Number(gg.score.String())
						mr.Score = &score
					}
					if c.work != nil {
						if g := liveGradeOf(rc.course, c.work, m); g != nil {
							mr.Grade = &peerMemberGradeView{GradeID: g.id, Score: json.Number(decimalOf(g.score).String()), State: g.state()}
							if g.adjust != nil {
								mr.Grade.AdjustmentKind = &g.adjust.kind
							}
						}
					}
					r.Members = append(r.Members, mr)
				}
				out.Groups = append(out.Groups, r)
			}
			return out, nil
		},
	})
}

// groupOf is the group of a's set with id, or nil.
func (a *assignment) groupOf(id string) *group {
	if a.groupSet == nil {
		return nil
	}
	for _, g := range a.groupSet.groups {
		if g.id == id {
			return g
		}
	}
	return nil
}

type peerEntryIn struct {
	StudentMemberID uuid.UUID      `json:"student_member_id"`
	Ratings         map[string]int `json:"ratings,omitempty"`
	Share           *int           `json:"share,omitempty"`
	Comment         *string        `json:"comment,omitempty"`
}

type peerReviewSubmitIn struct {
	inCourse
	AssignmentID uuid.UUID     `json:"assignment_id"`
	Entries      []peerEntryIn `json:"entries"`
	Comment      *string       `json:"comment,omitempty"`
}

// errPeerPeopleOnly refuses an agent's peer evaluation: a person's
// judgment of their classmates, which no agent writes, its owner's or any
// other's, whatever it holds.
var errPeerPeopleOnly = forbid("a peer evaluation is a person's judgment of their classmates; an agent writes none").
	with("reason", "people_only")

// peerReviewSubmit is Core's peer_review.submit as far as an agent meets
// it: gated on submission_write, the sheet held to what it says alone, and
// refused to any agent (people_only) before it is carried out or proposed.
// A person's sheet is not carried out here (not_implemented).
func peerReviewSubmit() *impl {
	return define(spec[peerReviewSubmitIn]{
		gate: gate{perms: []string{permSubmissionWrite}},
		check: func(in peerReviewSubmitIn) error {
			if len(in.Entries) == 0 || len(in.Entries) > maxGroupSize {
				return invalid("a sheet has 1 to %d entries, one for each member evaluated", maxGroupSize).with("reason", "sheet_incomplete")
			}
			seen := map[uuid.UUID]bool{}
			shares, total := 0, 0
			for _, e := range in.Entries {
				if seen[e.StudentMemberID] {
					return invalid("member %s is evaluated twice", e.StudentMemberID).with("reason", "sheet_incomplete")
				}
				seen[e.StudentMemberID] = true
				switch {
				case (e.Share == nil) == (len(e.Ratings) == 0):
					return invalid("an entry gives ratings or a share, one of them").with("reason", "bad_rating")
				case e.Share != nil:
					if *e.Share < 0 || *e.Share > 100 {
						return invalid("a share is 0 to %d", 100).with("reason", "bad_share_total")
					}
					shares++
					total += *e.Share
				}
			}
			if shares > 0 && shares != len(in.Entries) {
				return invalid("a sheet gives ratings or shares, not both").with("reason", "bad_rating")
			}
			if shares > 0 && total != 100 {
				return invalid("the shares add up to %d, not %d", total, 100).with("reason", "bad_share_total").with("total", total)
			}
			return nil
		},
		resolve: func(_ *Core, co *course, in peerReviewSubmitIn) (target, error) {
			return assignmentTarget(co, in.AssignmentID)
		},
		validate: func(_ *Core, m *member, _ peerReviewSubmitIn) error {
			if m.actor.kind == "agent" || m.principal != nil {
				return errPeerPeopleOnly
			}
			return forbid("peer_review.submit by a person is not something this fake Core carries out").with("reason", "not_implemented")
		},
		execute: func(*Core, *execCtx, peerReviewSubmitIn) (any, error) {
			return nil, forbid("peer_review.submit is not something this fake Core carries out").with("reason", "not_implemented")
		},
	})
}

// maxGroupSize is the most members a group, or a sheet, has (Core's).
const maxGroupSize = 500

// ---------------------------------------------------------------------------
// grade_submit on a group's work (Core's grade_group.go)
// ---------------------------------------------------------------------------

const adjustReplace, adjustDelta, adjustNone = "replace", "delta", "none"

// maxAdjustReason is the longest reason for an adjustment, in characters.
const maxAdjustReason = 500

// adjustmentIn is one member's adjustment, as a call names it.
type adjustmentIn struct {
	StudentMemberID uuid.UUID `json:"student_member_id"`
	Kind            string    `json:"kind"`
	Points          *decimal  `json:"points,omitempty"`
	Reason          *string   `json:"reason,omitempty"`
}

var (
	errNotAGroupAssignment = precondition("this assignment is not a group assignment: work is each student's own").
				with("reason", "not_a_group_assignment")
	errMembersChanged = conflicts("the group's members have changed since this was proposed; look again, and propose it again").
				with("reason", "members_changed")
)

func errBadAdjustment(format string, args ...any) *apiError {
	return invalid(format, args...).with("reason", "bad_adjustment")
}

// checkAdjustment holds an adjustment to what it may say alone: a kind,
// with points and a reason for replace and delta, and neither for none; a
// replaced score not below zero.
func checkAdjustment(a adjustmentIn) error {
	switch a.Kind {
	case adjustNone:
		if a.Points != nil || a.Reason != nil {
			return errBadAdjustment("an adjustment of kind none takes no points and no reason")
		}
		return nil
	case adjustReplace, adjustDelta:
	default:
		return errBadAdjustment("an adjustment is replace, delta or none")
	}
	if a.Points == nil {
		return errBadAdjustment("a %s adjustment says by how much: points", a.Kind)
	}
	if a.Kind == adjustReplace && a.Points.negative() {
		return errBadAdjustment("a replaced score cannot be negative")
	}
	if a.Reason == nil || strings.TrimSpace(*a.Reason) == "" || utf8.RuneCountInString(strings.TrimSpace(*a.Reason)) > maxAdjustReason {
		return errBadAdjustment("an adjustment says why, in 1 to %d characters: reason", maxAdjustReason)
	}
	return nil
}

func checkAdjustments(adjs []adjustmentIn) error {
	seen := map[uuid.UUID]bool{}
	for _, a := range adjs {
		if seen[a.StudentMemberID] {
			return errBadAdjustment("member %s is adjusted twice", a.StudentMemberID)
		}
		seen[a.StudentMemberID] = true
		if err := checkAdjustment(a); err != nil {
			return err
		}
	}
	return nil
}

// same: the same adjustment, whoever made it; nil is none.
func (a *adjustment) same(b *adjustment) bool {
	switch {
	case a == nil || b == nil:
		return a == nil && b == nil
	case a.kind != b.kind:
		return false
	}
	return a.points.cmp(b.points) == 0 && a.reason == b.reason
}

// named is the adjustment a call names, made by by; nil for kind none.
func named(a adjustmentIn, by *member) *adjustment {
	if a.Kind == adjustNone {
		return nil
	}
	return &adjustment{kind: a.Kind, points: *a.Points, reason: strings.TrimSpace(*a.Reason), by: by}
}

// memberScore is a member's score from group score g: g, a replaced score,
// or g with a delta, never below zero, and above most only where the grade
// allows extra credit.
func memberScore(g decimal, a *adjustment, most decimal, allowExtra bool) (decimal, error) {
	score := g
	if a != nil {
		switch a.kind {
		case adjustReplace:
			score = a.points
		case adjustDelta:
			score = decimal{new(big.Rat).Add(g.rat(), a.points.rat())}
		}
	}
	if score.negative() {
		return score, precondition("the adjustment would give a score of %s, below zero", score).with("reason", "adjusted_below_zero")
	}
	if a != nil && score.cmp(most) > 0 && !allowExtra {
		return score, precondition("the adjustment would give a score of %s, above the %s points possible; set allow_extra to permit it",
			score, most).with("reason", "adjusted_above_points")
	}
	return score, nil
}

// memberAdjustments is each member's adjustment a grade of s, as in asks,
// is written with, made by by: refused on a student's own work, once a
// grade on the work is posted, for a member not of the work, for a score
// out of bounds, and, where in names the members, when they are not the
// work's (Core's memberAdjustments). Each is the one the call names, or
// else the one carried from the member's live grade on the work (their
// draft, or their posted grade); one named the same as the one carried
// keeps who made it.
func (in gradeSubmitIn) memberAdjustments(co *course, s gradeSubject, by *member) (map[*member]*adjustment, error) {
	if !s.group() {
		if len(in.Adjustments) > 0 || in.Members != nil {
			return nil, errNotAGroupAssignment
		}
		return nil, nil
	}
	if in.Members != nil && !sameMembers(in.Members, s.members) {
		return nil, errMembersChanged
	}
	carried := map[*member]*adjustment{}
	for _, m := range s.members {
		g := liveGradeOf(co, s.submission, m)
		if g == nil {
			continue
		}
		if g.postedAt != nil {
			return nil, precondition("the group's grade on this work is posted: regrade it with grade.regrade, which gives a member "+
				"added to the work since a grade from it as well, or change one member's with grade.adjust").
				with("reason", "group_grade_posted").with("grade_id", g.id)
		}
		carried[m] = g.adjust
	}
	out := map[*member]*adjustment{}
	for _, m := range s.members {
		out[m] = carried[m]
	}
	for _, a := range in.Adjustments {
		i := slices.IndexFunc(s.members, func(m *member) bool { return m.id == a.StudentMemberID.String() })
		if i < 0 {
			return nil, precondition("%s is not a member of this work", a.StudentMemberID).with("reason", "not_a_member_of_work").
				with("member_id", a.StudentMemberID.String())
		}
		m := s.members[i]
		if n := named(a, by); !n.same(carried[m]) {
			out[m] = n
		}
	}
	for _, a := range out {
		if _, err := memberScore(in.Score, a, s.pointsPossible(), in.AllowExtra); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// sameMembers says whether ids name exactly ms.
func sameMembers(ids []uuid.UUID, ms []*member) bool {
	a := make([]string, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(a, id.String()) {
			a = append(a, id.String())
		}
	}
	b := make([]string, 0, len(ms))
	for _, m := range ms {
		b = append(b, m.id)
	}
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// pinnedMembers names a work's members in order of id, and every member's
// adjustment as it will be written: what a proposal stores.
func pinnedMembers(members []*member, adjs map[*member]*adjustment) ([]uuid.UUID, []adjustmentIn) {
	ids := make([]uuid.UUID, 0, len(members))
	out := make([]adjustmentIn, 0, len(members))
	for _, m := range byID(members) {
		id := uuid.MustParse(m.id)
		ids = append(ids, id)
		in := adjustmentIn{StudentMemberID: id, Kind: adjustNone}
		if a := adjs[m]; a != nil {
			points, reason := a.points, a.reason
			in.Kind, in.Points, in.Reason = a.kind, &points, &reason
		}
		out = append(out, in)
	}
	return ids, out
}

// memberGradeOut is one member's grade given from a group grade.
type memberGradeOut struct {
	StudentMemberID string          `json:"student_member_id"`
	GradeID         string          `json:"grade_id"`
	Score           decimal         `json:"score"`
	Adjustment      *adjustmentView `json:"adjustment,omitempty"`
}

// peerState is how peer evaluation counts in a group's grades written now:
// not at all (""), not yet (window_open), or counted. Nobody here writes a
// sheet, so a member counted is rated by nobody and given no peer
// adjustment, as Core gives none to a member nobody rated.
func peerState(a *assignment, now time.Time) string {
	switch f := a.peer; {
	case !f.counts():
		return ""
	case now.Before(f.closesAt):
		return "window_open"
	}
	return "counted"
}

// gradeGroupWork is grade.submit on a group's work: its group grade, and a
// draft for each member of the work from it, in order of their ids, each
// replacing the member's earlier draft and told to them by an event of
// their own.
func (in gradeSubmitIn) gradeGroupWork(_ *Core, ec *execCtx, s gradeSubject) (any, error) {
	adjs, err := in.memberAdjustments(ec.course, s, ec.member)
	if err != nil {
		return nil, err
	}
	var breakdown json.RawMessage
	if len(in.Breakdown) > 0 {
		if breakdown, err = json.Marshal(in.Breakdown); err != nil {
			return nil, err
		}
	}
	gg := &groupGrade{id: newID(), submission: s.submission, score: in.Score, allowExtra: in.AllowExtra, createdAt: ec.createdAt}
	out := struct {
		GroupGradeID string           `json:"group_grade_id"`
		MemberGrades []memberGradeOut `json:"member_grades"`
		Peer         string           `json:"peer,omitempty"`
	}{GroupGradeID: gg.id, Peer: peerState(s.assignment, ec.now)}
	for _, m := range byID(s.members) {
		a := adjs[m]
		score, err := memberScore(in.Score, a, s.pointsPossible(), in.AllowExtra)
		if err != nil {
			return nil, err
		}
		id := newID()
		for _, g := range ec.course.grades {
			if g.submission == s.submission && g.student == m && g.liveDraft() {
				g.supersededBy = &id
			}
		}
		// Dated when the call was made, not when it was approved, as a
		// student's own draft is.
		g := &grade{id: id, student: m, assignment: s.assignment, submission: s.submission, grader: ec.member, actionID: ec.actionID,
			score: score.String(), feedback: in.Feedback, breakdown: breakdown, createdAt: ec.createdAt, groupGrade: gg, adjust: a}
		ec.course.grades = append(ec.course.grades, g)
		ec.emit(&event{typ: "grade.created", course: ec.course, subjectType: "grade", subjectID: &g.id, student: m.id,
			assignment: s.assignment.id, payload: mustJSON(map[string]any{"group_grade_id": gg.id})})
		out.MemberGrades = append(out.MemberGrades, memberGradeOut{StudentMemberID: m.id, GradeID: id, Score: score,
			Adjustment: a.view(true)})
	}
	return out, nil
}
