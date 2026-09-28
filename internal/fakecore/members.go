package fakecore

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The course's members, as Core's tools/member.go has them: the roster
// (member.list, member.get), whom an actor id names (member.lookup_actor),
// and seating someone (member.add), which a model makes through its seat's
// perms (the runtime's docs/design.md §4) when its seat manages members.
// Changing, pausing and removing a seat are the test controls' (SetLevel,
// PauseSeat, RemoveSeat); their tools are refused as not carried out here.

var (
	gateReadMembers   = gate{perms: []string{permMemberRead}}
	gateManageMembers = gate{perms: []string{permMemberManage}}
)

// memberView is a seat as Core's MemberView shows it: the row's own
// levels, not a delegate's capped ones, which me_memberships shows.
type memberView struct {
	ID                string            `json:"id"`
	ActorID           string            `json:"actor_id"`
	DisplayName       string            `json:"display_name"`
	Kind              string            `json:"kind"`
	Role              string            `json:"role"`
	Status            string            `json:"status"`
	PresetID          *string           `json:"preset_id,omitempty"`
	ExpiresAt         *time.Time        `json:"expires_at,omitempty"`
	StudentScope      string            `json:"student_scope"`
	AssignmentScope   string            `json:"assignment_scope"`
	Perms             map[string]string `json:"perms"`
	ListedStudents    []string          `json:"listed_students,omitempty"`
	ListedAssignments []string          `json:"listed_assignments,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	PrincipalMemberID *string           `json:"principal_member_id,omitempty"`
	OwnerActorID      *string           `json:"owner_actor_id,omitempty"`
	OwnerName         *string           `json:"owner_name,omitempty"`
	AnswersCourse     bool              `json:"answers_course"`
	// SiteChat, for an agent's seat: whether people in the site may ask
	// it (me_site_chat); absent for a person's.
	SiteChat *bool `json:"site_chat,omitempty"`
	ceilings
}

func (c *Core) viewMember(m *member) memberView {
	v := memberView{ID: m.id, ActorID: m.actor.id, DisplayName: m.actor.name, Kind: m.actor.kind, Role: m.role, Status: m.status,
		ExpiresAt: m.expiresAt, StudentScope: m.studentScope, AssignmentScope: m.assignmentScope,
		Perms: make(map[string]string, len(allPerms)), CreatedAt: m.createdAt, AnswersCourse: m.answersCourse}
	for _, p := range allPerms {
		v.Perms[p] = m.perms[p].String()
	}
	if m.presetID != "" {
		v.PresetID = ptr(m.presetID)
	}
	if m.principal != nil {
		v.PrincipalMemberID = ptr(m.principal.id)
	}
	if o := m.actor.owner; o != nil {
		v.OwnerActorID, v.OwnerName = ptr(o.id), ptr(o.name)
	}
	if m.actor.kind == "agent" {
		v.SiteChat = ptr(c.takesSiteChat(m.actor))
	}
	v.ceilings = ceilingsOf(m)
	return v
}

// sortedIDs are a set's ids in order, as Core lists a scope.
func sortedIDs(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

type memberListIn struct {
	inCourse
	Role           *string `json:"role,omitempty"`
	IncludeRemoved bool    `json:"include_removed,omitempty"`
	pageIn
}

// memberList is the course's seats in the order they were made, people and
// agents alike, removed ones only when asked for.
func memberList() *impl {
	return define(spec[memberListIn]{
		gate:    gateReadMembers,
		resolve: func(*Core, *course, memberListIn) (target, error) { return target{typ: "course_member"}, nil },
		query: func(c *Core, rc *readCtx, in memberListIn) (any, error) {
			limit, after := pageLimit(in.Limit), in.after()
			out := struct {
				Members []memberView `json:"members"`
				Next    *string      `json:"next,omitempty"`
			}{Members: []memberView{}}
			for _, m := range c.memberList {
				if len(out.Members) == limit {
					break
				}
				if m.course != rc.course || m.id <= after || (in.Role != nil && m.role != *in.Role) ||
					(!in.IncludeRemoved && m.status == statusRemoved) {
					continue
				}
				out.Members = append(out.Members, c.viewMember(m))
			}
			if n := len(out.Members); n > 0 && n == limit {
				out.Next = &out.Members[n-1].ID
			}
			return out, nil
		},
	})
}

type memberIDIn struct {
	inCourse
	MemberID uuid.UUID `json:"member_id"`
}

// seatIn is the seat id names in co, removed or not.
func (c *Core) seatIn(co *course, id uuid.UUID) (*member, error) {
	m := c.members[id.String()]
	if m == nil || m.course != co {
		return nil, missing("no such member in this course")
	}
	return m, nil
}

// memberGet is one seat in full, with whom a listed scope lists.
func memberGet() *impl {
	return define(spec[memberIDIn]{
		gate: gateReadMembers,
		resolve: func(c *Core, co *course, in memberIDIn) (target, error) {
			m, err := c.seatIn(co, in.MemberID)
			if err != nil {
				return target{}, err
			}
			return target{typ: "course_member", id: &m.id}, nil
		},
		query: func(c *Core, rc *readCtx, in memberIDIn) (any, error) {
			m, err := c.seatIn(rc.course, in.MemberID)
			if err != nil {
				return nil, err
			}
			v := c.viewMember(m)
			v.ListedStudents, v.ListedAssignments = sortedIDs(m.students), sortedIDs(m.assignments)
			return v, nil
		},
	})
}

type memberLookupActorIn struct {
	inCourse
	Email   *string    `json:"email,omitempty"`
	LoginID *string    `json:"login_id,omitempty"`
	ActorID *uuid.UUID `json:"actor_id,omitempty"`
}

// memberLookupActor finds the actor an id names, for whoever seats people:
// with their seat here when they have one, and their owner when an agent
// is someone's. Nobody here has an email address or a login ID, so
// neither finds anybody.
func memberLookupActor() *impl {
	return define(spec[memberLookupActorIn]{
		gate:    gateManageMembers,
		resolve: func(*Core, *course, memberLookupActorIn) (target, error) { return target{typ: "actor"}, nil },
		query: func(c *Core, rc *readCtx, in memberLookupActorIn) (any, error) {
			given := 0
			for _, v := range []*string{in.Email, in.LoginID} {
				if v != nil && strings.TrimSpace(*v) != "" {
					given++
				}
			}
			if in.ActorID != nil {
				given++
			}
			if given != 1 {
				return nil, invalid("give one of email, login_id and actor_id")
			}
			// The fake keeps nobody's email or login ID: only an id
			// finds anyone.
			var a *actor
			if in.ActorID != nil {
				a = c.actors[in.ActorID.String()]
			}
			if a == nil || a.kind == "system" {
				return nil, missing("nobody is registered with that email, login ID or id")
			}
			out := struct {
				ActorID      string  `json:"actor_id"`
				DisplayName  string  `json:"display_name"`
				Kind         string  `json:"kind"`
				Status       string  `json:"status"`
				MemberID     *string `json:"member_id,omitempty"`
				OwnerActorID *string `json:"owner_actor_id,omitempty"`
				OwnerName    *string `json:"owner_name,omitempty"`
			}{ActorID: a.id, DisplayName: a.name, Kind: a.kind, Status: a.status}
			if m := c.seatOf(a, rc.course); m != nil {
				out.MemberID = ptr(m.id)
			}
			if a.owner != nil {
				out.OwnerActorID, out.OwnerName = ptr(a.owner.id), ptr(a.owner.name)
			}
			return out, nil
		},
	})
}

type memberAddIn struct {
	inCourse
	ActorID           uuid.UUID         `json:"actor_id"`
	Preset            *string           `json:"preset,omitempty"`
	PresetID          *uuid.UUID        `json:"preset_id,omitempty"`
	Role              *string           `json:"role,omitempty"`
	Perms             map[string]string `json:"perms,omitempty"`
	StudentScope      *string           `json:"student_scope,omitempty"`
	ListedStudents    []uuid.UUID       `json:"listed_students,omitempty"`
	AssignmentScope   *string           `json:"assignment_scope,omitempty"`
	ListedAssignments []uuid.UUID       `json:"listed_assignments,omitempty"`
	ExpiresAt         *time.Time        `json:"expires_at,omitempty"`
}

var validRoles = map[string]bool{"student": true, "instructor": true, "ta": true, "observer": true, "assistant": true}

func validScope(s string) bool { return s == scopeAll || s == scopeListed }

// presetID is the id Core gives the built-in preset name: one per fake.
func (c *Core) presetID(name string) string {
	if c.presetIDs[name] == "" {
		c.presetIDs[name] = newID()
	}
	return c.presetIDs[name]
}

// findPreset is a built-in preset by name or by id, as Core's findPreset
// finds one for a course whose department has none of its own.
func (c *Core) findPreset(name *string, id *uuid.UUID) (string, preset, error) {
	switch {
	case (name == nil) == (id == nil):
		return "", preset{}, invalid("give exactly one of preset and preset_id")
	case id != nil:
		for n, pid := range c.presetIDs {
			if pid == id.String() {
				return n, presets[n], nil
			}
		}
	default:
		if pr, ok := presets[*name]; ok {
			return *name, pr, nil
		}
	}
	return "", preset{}, missing("no such preset")
}

// applyPerms overlays named levels, refusing an unknown name or level: a
// typo must not leave a permission as it was. The first refused, by name,
// is the one reported.
func applyPerms(ps map[string]level, over map[string]string) error {
	names := make([]string, 0, len(over))
	for n := range over {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !validPerm(n) {
			return invalid("there is no permission named %q", n)
		}
		l, err := parseLevel(over[n])
		if err != nil {
			return invalid("%q is not a level; use denied, confirm_required, pending_review or autonomous", over[n])
		}
		ps[n] = l
	}
	return nil
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if s := id.String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// withinGranter refuses to hand out more than the granter g holds: no level
// above g's own, no reach outside g's listed scope, as Core's does.
func withinGranter(g *member, perms map[string]level, listsItself bool, studentScope string, students []string, assignmentScope string, assignments []string) error {
	for _, p := range allPerms {
		if perms[p] > g.perm(p) {
			return forbid("you hold %s at %s and cannot grant it at %s", p, g.perm(p), perms[p]).with("permission", p)
		}
	}
	if g.studentScope == scopeListed {
		switch {
		case studentScope != scopeListed:
			return forbid("your own student scope is a list; you cannot grant the whole class")
		case listsItself:
			return forbid("your own student scope is a list; a new student's seat reaches that student, who is not on it")
		case checkScope(g, scope{students: students}) != "":
			return forbid("you can only give a seat that reaches students who are in your own scope")
		}
	}
	if g.assignmentScope == scopeListed {
		switch {
		case assignmentScope != scopeListed:
			return forbid("your own assignment scope is a list; you cannot grant every assignment")
		case checkScope(g, scope{assignments: assignments}) != "":
			return forbid("you can only list assignments that are in your own scope")
		}
	}
	return nil
}

// memberAdd seats an actor with a preset's role, levels and scope, any of
// them overridden, within what the caller holds, as Core's member.add does.
func memberAdd() *impl {
	return define(spec[memberAddIn]{
		gate:    gateManageMembers,
		resolve: func(*Core, *course, memberAddIn) (target, error) { return target{typ: "course_member"}, nil },
		execute: func(c *Core, ec *execCtx, in memberAddIn) (any, error) {
			name, pr, err := c.findPreset(in.Preset, in.PresetID)
			if err != nil {
				return nil, err
			}
			role, studentScope, assignmentScope := pr.role, pr.studentScope, pr.assignmentScope
			if in.Role != nil {
				role = *in.Role
			}
			if in.StudentScope != nil {
				studentScope = *in.StudentScope
			}
			if in.AssignmentScope != nil {
				assignmentScope = *in.AssignmentScope
			}
			if !validRoles[role] || !validScope(studentScope) || !validScope(assignmentScope) {
				return nil, invalid("role or scope is not one of the allowed values")
			}
			perms := make(map[string]level, len(allPerms))
			for i, p := range allPerms {
				perms[p] = pr.levels[i]
			}
			if err := applyPerms(perms, in.Perms); err != nil {
				return nil, err
			}
			students, assignments := uuidStrings(in.ListedStudents), uuidStrings(in.ListedAssignments)
			listsItself := role == "student" && studentScope == scopeListed && len(in.ListedStudents) == 0
			if err := withinGranter(ec.member, perms, listsItself, studentScope, students, assignmentScope, assignments); err != nil {
				return nil, err
			}
			expiresAt := in.ExpiresAt
			if expiresAt != nil {
				t := expiresAt.UTC().Truncate(time.Microsecond)
				expiresAt = &t
			}
			if g := ec.member.expiresAt; g != nil && (expiresAt == nil || expiresAt.After(*g)) {
				return nil, forbid("your own membership ends at %s; you cannot give one that lasts longer", g.UTC().Format(time.RFC3339))
			}
			m, err := c.seatNew(ec, in.ActorID.String(), role, name, perms, expiresAt)
			if err != nil {
				return nil, err
			}
			if err := toCeilings(m.actor.kind == "agent", nil, m.perms, in.Perms); err != nil {
				return nil, err
			}
			if err := c.writeScope(m, studentScope, students, listsItself, assignmentScope, assignments); err != nil {
				return nil, err
			}
			c.members[m.id] = m
			c.memberList = append(c.memberList, m)
			ec.emit(memberEvent("member.added", m, map[string]any{"actor_id": m.actor.id, "role": m.role}))
			return struct {
				MemberID string `json:"member_id"`
			}{m.id}, nil
		},
	})
}

// errSeated refuses a second live seat.
var errSeated = conflicts("the actor already has a seat in this course; change it, or remove it and add again for a fresh start")

// seatNew is the seat Core's seat() would make for the actor, not yet in
// the course: the actor must be someone active, not the system, and not an
// agent someone owns, with no live seat here; a seat whose expiry has passed
// is removed first.
func (c *Core) seatNew(ec *execCtx, actorID, role, presetName string, perms map[string]level, expiresAt *time.Time) (*member, error) {
	a := c.actors[actorID]
	switch {
	case a == nil:
		return nil, missing("no such actor")
	case !a.active():
		return nil, precondition("the actor is suspended")
	case a.kind == "system":
		return nil, precondition("the system actor is not seated in courses")
	case a.owner != nil:
		return nil, precondition("the agent belongs to someone: its owner brings it in, with member.add_delegate")
	}
	if live := c.seatOf(a, ec.course); live != nil {
		if live.expiresAt == nil || live.expiresAt.After(ec.now) {
			return nil, errSeated
		}
		live.status = statusRemoved
		ec.emit(memberEvent("member.removed", live, map[string]any{"reason": "expired"}))
	}
	if expiresAt != nil && !expiresAt.After(ec.now) {
		return nil, invalid("expires_at is in the past")
	}
	return &member{id: newID(), course: ec.course, actor: a, status: statusActive, expiresAt: expiresAt, role: role,
		perms: perms, presetID: c.presetID(presetName), createdAt: ec.now}, nil
}

// writeScope gives a new seat its lists: whom and what a listed scope
// reaches, each of them the course's; a student listed with nobody lists
// itself.
func (c *Core) writeScope(m *member, studentScope string, students []string, listsItself bool, assignmentScope string, assignments []string) error {
	if listsItself {
		students = []string{m.id}
	}
	switch {
	case studentScope != scopeListed && len(students) > 0:
		return invalid("listed_students only makes sense with student_scope = listed")
	case assignmentScope != scopeListed && len(assignments) > 0:
		return invalid("listed_assignments only makes sense with assignment_scope = listed")
	}
	for _, id := range students {
		if s := c.members[id]; id != m.id && (s == nil || s.course != m.course || s.role != "student" || s.status == statusRemoved) {
			return precondition("listed_students must all be current students of this course")
		}
	}
	for _, id := range assignments {
		if findAssignment(m.course, uuid.MustParse(id)) == nil {
			return precondition("listed_assignments must all be assignments of this course")
		}
	}
	m.studentScope, m.assignmentScope = studentScope, assignmentScope
	m.students, m.assignments = map[string]bool{}, map[string]bool{}
	for _, id := range students {
		m.students[id] = true
	}
	for _, id := range assignments {
		m.assignments[id] = true
	}
	return nil
}
