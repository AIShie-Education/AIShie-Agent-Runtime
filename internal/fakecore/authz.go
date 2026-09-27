package fakecore

import "time"

// Why authorization denied a call: Core's authz.Reason, which goes in the
// error's details.reason.
const (
	reasonActorNotActive     = "actor_not_active"
	reasonCourseArchived     = "course_archived"
	reasonNotAMember         = "not_a_member"
	reasonMemberNotLive      = "membership_not_active"
	reasonPrincipalNotActive = "principal_not_active"
	reasonPermDenied         = "permission_denied"
	reasonStudentScope       = "student_out_of_scope"
	reasonAssignmentScope    = "assignment_out_of_scope"
)

// perm is the seat's level for p. A delegate's is the lower of its own and
// its principal's, with conversation_answer capped by the principal's
// conversation_ask, and member_manage and agent_delegate never held
// (Core's domain.Member.Perm).
func (m *member) perm(p string) level {
	own := m.perms[p]
	if m.principal == nil {
		return own
	}
	return min(own, delegateCap(m.principal, p))
}

func delegateCap(principal *member, p string) level {
	switch p {
	case permMemberManage, permAgentDelegate:
		return denied
	case permConversationAnswer:
		return principal.perm(permConversationAsk)
	}
	return principal.perm(p)
}

// answersOthers reports whether a delegate may be addressed by anyone but its
// principal: it was seated to answer the course, and its principal still
// manages the course's members.
func (m *member) answersOthers() bool {
	return m.principal != nil && m.answersCourse && m.principal.perm(permMemberManage).allowed()
}

// live reports whether the seat itself counts at now.
func (m *member) live(now time.Time) bool {
	return m.status == statusActive && (m.expiresAt == nil || m.expiresAt.After(now))
}

// seatValid says the seat is what its actor's ownership says it must be: no
// principal for an actor nobody owns, the active owner's seat for one
// somebody does.
func (m *member) seatValid() bool {
	if m.principal == nil {
		return m.actor.owner == nil
	}
	return m.actor.owner == m.principal.actor && m.principal.actor.active()
}

// principalLive reports whether what the seat depends on counts at now.
func (m *member) principalLive(now time.Time) bool {
	return m.seatValid() && (m.principal == nil || m.principal.live(now))
}

// counts reports whether a seat takes part now: live, its principal's too,
// held by an active actor.
func (m *member) counts(now time.Time) bool {
	return m.live(now) && m.principalLive(now) && m.actor.active()
}

// effectivePerms is every permission's level as me_memberships and the views
// show it: all denied while the seat does not count.
func (m *member) effectivePerms(now time.Time) map[string]string {
	usable := m.live(now) && m.principalLive(now)
	out := make(map[string]string, len(allPerms))
	for _, p := range allPerms {
		l := denied
		if usable {
			l = m.perm(p)
		}
		out[p] = l.String()
	}
	return out
}

// decision is authorization's verdict: a level, why it is denied, and the
// seat it was reached under (set whenever one was found).
type decision struct {
	level  level
	reason string
	member *member
}

func deny(reason string, m *member) decision {
	return decision{level: denied, reason: reason, member: m}
}

// evaluate is Core's authorize() steps 1 to 3 with everything loaded. A tool
// gated by several permissions runs at the lowest of their levels.
func evaluate(act *actor, co *course, m *member, perms []string, write bool, now time.Time) decision {
	switch {
	case !act.active():
		return deny(reasonActorNotActive, m)
	case write && co.status == statusArchived:
		return deny(reasonCourseArchived, m)
	case m == nil:
		return deny(reasonNotAMember, nil)
	case !m.live(now):
		return deny(reasonMemberNotLive, m)
	case !m.principalLive(now):
		return deny(reasonPrincipalNotActive, m)
	}
	l := denied
	for i, p := range perms {
		if i == 0 || m.perm(p) < l {
			l = m.perm(p)
		}
	}
	if !l.allowed() {
		return deny(reasonPermDenied, m)
	}
	return decision{level: l, member: m}
}

// scope is what a target belongs to, for steps 4 and 5.
type scope struct {
	students    []string
	assignments []string
	// spans marks a target of a student's that belongs to no single
	// assignment: a member limited to listed assignments may not touch it.
	spans bool
}

// checkScope is steps 4 and 5; a delegate reaches only what it and its
// principal both reach.
func checkScope(m *member, t scope) string {
	if r := checkOwnScope(m, t); r != "" || m.principal == nil {
		return r
	}
	return checkOwnScope(m.principal, t)
}

func checkOwnScope(m *member, t scope) string {
	if m.studentScope != scopeAll {
		for _, s := range t.students {
			if !m.students[s] {
				return reasonStudentScope
			}
		}
	}
	if t.spans && m.assignmentScope != scopeAll {
		return reasonAssignmentScope
	}
	if m.assignmentScope != scopeAll {
		for _, id := range t.assignments {
			if !m.assignments[id] {
				return reasonAssignmentScope
			}
		}
	}
	return ""
}

// reach is what a seat reaches of students, or of assignments: everything,
// or those listed. Listed with nothing is nobody.
type reach struct {
	all bool
	ids map[string]bool
}

func (r reach) meet(o reach) reach {
	switch {
	case r.all:
		return o
	case o.all:
		return r
	}
	out := reach{ids: map[string]bool{}}
	for id := range r.ids {
		if o.ids[id] {
			out.ids[id] = true
		}
	}
	return out
}

func (r reach) within(o reach) bool {
	if o.all {
		return true
	}
	if r.all {
		return false
	}
	for id := range r.ids {
		if !o.ids[id] {
			return false
		}
	}
	return true
}

func (m *member) ownReach(ofStudents bool) reach {
	kind, ids := m.assignmentScope, m.assignments
	if ofStudents {
		kind, ids = m.studentScope, m.students
	}
	if kind == scopeAll {
		return reach{all: true}
	}
	return reach{ids: ids}
}

func (m *member) reachOf(ofStudents bool) reach {
	r := m.ownReach(ofStudents)
	if m.principal != nil {
		r = r.meet(m.principal.ownReach(ofStudents))
	}
	return r
}

// within says whether r can see and do nothing o cannot: no level above o's
// on any permission but conversation_answer, and a reach inside o's.
func within(r, o *member) bool {
	for _, p := range allPerms {
		if p != permConversationAnswer && r.perm(p) > o.perm(p) {
			return false
		}
	}
	for _, ofStudents := range []bool{true, false} {
		if !r.reachOf(ofStudents).within(o.reachOf(ofStudents)) {
			return false
		}
	}
	return true
}

// refusal says why o may not address r, or "" if it may: Core's one rule for
// who may address whom, measured now, on every call.
func refusal(o, r *member, now time.Time) string {
	switch {
	case o == r:
		return "nobody addresses themselves"
	case !o.counts(now):
		return "the one asking is not an active member here"
	case !r.counts(now):
		return "the respondent is not an active member here"
	case !r.perm(permConversationAnswer).allowed():
		return "the respondent does not answer questions here"
	case r.principal != nil && r.principal == o:
		return ""
	case r.principal != nil && !r.answersOthers():
		return "the respondent is someone else's own agent, and answers only them"
	case !within(r, o):
		return "the respondent can see or do what you cannot"
	}
	return ""
}

// oversees says whether m oversees conversations opened from opener: it
// decides actions here, and its student scope reaches the opener.
func oversees(m, opener *member) bool {
	return m.perm(permActionDecide).allowed() && checkScope(m, scope{students: []string{opener.id}}) == ""
}

// mayRead is the read rule of conversation_get and conversation_messages: the
// opener always; the respondent while the opener may still address it; and
// whoever oversees the opener.
func mayRead(m *member, cv *conversation, now time.Time) bool {
	switch m {
	case cv.opener:
		return true
	case cv.respondent:
		return refusal(cv.opener, m, now) == ""
	}
	return oversees(m, cv.opener)
}
