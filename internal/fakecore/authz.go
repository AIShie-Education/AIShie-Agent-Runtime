package fakecore

import (
	"fmt"
	"time"
)

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
// its ceiling (delegateCap), which authorization applies on every call
// (Core's domain.Member.Perm).
func (m *member) perm(p string) level {
	own := m.perms[p]
	if m.principal == nil {
		return own
	}
	return min(own, delegateCap(m.principal, p))
}

// delegateCap is the most a delegate of principal may hold of p: its
// ceiling (Core's domain.DelegateCap).
func delegateCap(principal *member, p string) level {
	l, _ := ceiling(true, principal, p)
	return l
}

// Why a ceiling is below autonomous (Core's domain.CeilingReason): the
// codes a refusal to go above it gives, and the views' perm_ceiling_reasons.
const (
	ceilingAgentNever             = "agent_never"
	ceilingAgentDecidesByProposal = "agent_decides_by_proposal"
	ceilingStudentAgentByProposal = "student_agent_by_proposal"
	ceilingPrincipalLevel         = "principal_level"
	// A person answers no conversation: conversations are between a person
	// and an agent. It is also why a person is refused as a respondent,
	// and refused answering (errWithAgents).
	ceilingConversationsAreWithAgents = "conversations_are_with_agents"
)

// delegatePresetLevels is what the built-in delegate preset gives, every
// other permission denied (Core's domain.DelegatePresetLevels).
var delegatePresetLevels = map[string]level{
	permDocumentRead: autonomous, permSubmissionRead: autonomous, permGradeRead: autonomous, permConversationAnswer: autonomous,
}

// ceiling is the most a seat may hold of p at all, whoever grants it, and
// why when that is below autonomous (Core's domain.Ceiling, the one rule):
// a person answers no conversation; an agent decides and reviews only by
// proposal; a delegate never brings agents of its own, holds no more than
// its principal (conversation_answer no more than the principal's
// conversation_ask), and, for a principal who does not manage the course's
// members, does only by proposal what the delegate preset does not give,
// member_manage and member_invite left to the principal's own level.
// principal is a delegate's principal's seat; a delegate is always an
// agent.
func ceiling(agent bool, principal *member, p string) (level, string) {
	lvl, why := autonomous, ""
	lower := func(l level, r string) {
		if l < lvl {
			lvl, why = l, r
		}
	}
	if principal != nil {
		agent = true
		if p == permAgentDelegate {
			lower(denied, ceilingAgentNever)
		}
	}
	if agent && p == permActionDecide {
		lower(confirmRequired, ceilingAgentDecidesByProposal)
	}
	if !agent && p == permConversationAnswer {
		lower(denied, ceilingConversationsAreWithAgents)
	}
	if principal == nil {
		return lvl, why
	}
	if p != permMemberManage && p != permMemberInvite && !principal.perm(permMemberManage).allowed() {
		lower(max(delegatePresetLevels[p], confirmRequired), ceilingStudentAgentByProposal)
	}
	if p == permConversationAnswer {
		lower(principal.perm(permConversationAsk), ceilingPrincipalLevel)
	} else {
		lower(principal.perm(p), ceilingPrincipalLevel)
	}
	return lvl, why
}

// ceilings is what the views say of a seat's ceilings (Core's Ceilings).
type ceilings struct {
	PermCeilings       map[string]string `json:"perm_ceilings"`
	PermCeilingReasons map[string]string `json:"perm_ceiling_reasons,omitempty"`
}

// ceilingsOf is m's ceilings, its principal's as it stands.
func ceilingsOf(m *member) ceilings {
	out := ceilings{PermCeilings: make(map[string]string, len(allPerms))}
	for _, p := range allPerms {
		l, why := ceiling(m.actor.kind == "agent", m.principal, p)
		out.PermCeilings[p] = l.String()
		if why != "" {
			if out.PermCeilingReasons == nil {
				out.PermCeilingReasons = map[string]string{}
			}
			out.PermCeilingReasons[p] = why
		}
	}
	return out
}

// errAboveCeiling refuses a level above what a seat may hold at all, as
// Core's does.
func errAboveCeiling(p string, asked, limit level, why string) *apiError {
	var msg string
	switch why {
	case ceilingAgentNever:
		msg = fmt.Sprintf("a delegate never holds %s: it brings no agents of its own", p)
	case ceilingAgentDecidesByProposal:
		msg = fmt.Sprintf("an agent holds %s at %s at most: it decides and reviews only by proposal, which a person confirms", p, limit)
	case ceilingConversationsAreWithAgents:
		msg = fmt.Sprintf("a person holds %s at %s: conversations are between a person and an agent, and a person answers "+
			"none; people talk to people elsewhere", p, limit)
	case ceilingStudentAgentByProposal:
		msg = fmt.Sprintf("the agent of someone who does not manage the course's members holds %s at %s at most: "+
			"beyond what the delegate preset gives, it acts only by proposal", p, limit)
	default:
		msg = fmt.Sprintf("the delegate's principal holds %s at %s, so the delegate cannot hold it at %s", p, limit, asked)
	}
	return forbid("%s", msg).with("permission", p).with("reason", why).with("ceiling", limit.String())
}

// toCeilings cuts the levels a seat is being given down to its ceilings, a
// level named in the call refused instead (Core's toCeilings).
func toCeilings(agent bool, principal *member, perms map[string]level, named map[string]string) error {
	for _, p := range allPerms {
		limit, why := ceiling(agent, principal, p)
		if perms[p] <= limit {
			continue
		}
		if _, ok := named[p]; ok {
			return errAboveCeiling(p, perms[p], limit, why)
		}
		perms[p] = limit
	}
	return nil
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
