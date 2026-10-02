package fakecore

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// The writes people make that the fake carries out too, so that the test
// controls act through Core's own pipeline and rules: opening a
// conversation, asking in it, and deciding a proposal. Anyone holding a
// person's token may call them over MCP or REST as well.

var gateAsks = gate{perms: []string{permConversationAsk}}

type openIn struct {
	inCourse
	RespondentMemberID uuid.UUID      `json:"respondent_member_id"`
	Title              *string        `json:"title,omitempty"`
	Body               *string        `json:"body,omitempty"`
	Attachments        []attachmentIn `json:"attachments,omitempty"`
}

// checkOpenArgs is what conversation_open's arguments say alone (its
// check): a title and a body that fit, files only with a body, and files a
// message may carry.
func checkOpenArgs(in openIn) error {
	if _, err := optionalText("title", in.Title, 200); err != nil {
		return err
	}
	if in.Body != nil {
		if err := checkBody(*in.Body); err != nil {
			return err
		}
	} else if len(in.Attachments) > 0 {
		return errAttachmentsNeedBody
	}
	_, err := shapeFiles(in.Attachments)
	return err
}

// checkOpen is conversation_open's rule on arguments checkOpenArgs has
// taken: a respondent that is an agent (errWithAgents) the caller may
// address, and then that people in the site may ask it (notAskable).
func (c *Core) checkOpen(m, respondent *member) error {
	if respondent.actor.kind != "agent" {
		return errWithAgents
	}
	if why := refusal(m, respondent, c.now()); why != "" {
		return notAddressable("you may not address that member", why)
	}
	return c.notAskable(respondent)
}

func conversationOpen() *impl {
	return define(spec[openIn]{
		gate:  gateAsks,
		check: checkOpenArgs,
		resolve: func(c *Core, co *course, in openIn) (target, error) {
			if r := c.members[in.RespondentMemberID.String()]; r == nil || r.course != co {
				return target{}, missing("no such member in this course")
			}
			return target{typ: "conversation"}, nil
		},
		validate: func(c *Core, m *member, in openIn) error {
			if err := c.checkOpen(m, c.members[in.RespondentMemberID.String()]); err != nil {
				return err
			}
			return c.checkMessageFiles(m, nil, in.Attachments, false)
		},
		pin: func(c *Core, m *member, in openIn) error {
			return c.checkMessageFiles(m, nil, in.Attachments, true)
		},
		execute: func(c *Core, ec *execCtx, in openIn) (any, error) {
			respondent := c.members[in.RespondentMemberID.String()]
			if err := c.checkOpen(ec.member, respondent); err != nil {
				return nil, err
			}
			files, names, err := c.checkFiles(ec.member, in.Attachments, false)
			if err != nil {
				return nil, err
			}
			title, _ := optionalText("title", in.Title, 200)
			cv := &conversation{id: newID(), course: ec.course, opener: ec.member, respondent: respondent, title: title,
				status: "open", createdAt: ec.now}
			c.conversations[cv.id] = cv
			c.conversationList = append(c.conversationList, cv)
			ec.emit(&event{typ: "conversation.opened", course: cv.course, subjectType: "conversation", subjectID: &cv.id,
				payload: mustJSON(map[string]any{"conversation_id": cv.id, "opener_member_id": cv.opener.id,
					"respondent_member_id": cv.respondent.id})})
			out := struct {
				ConversationID string  `json:"conversation_id"`
				MessageID      *string `json:"message_id,omitempty"`
			}{ConversationID: cv.id}
			if in.Body != nil {
				id, err := c.post(ec, cv, nil, *in.Body, nil, files, names)
				if err != nil {
					return nil, err
				}
				out.MessageID = &id
			}
			return out, nil
		},
	})
}

type askIn struct {
	inCourse
	ConversationID uuid.UUID      `json:"conversation_id"`
	Body           string         `json:"body"`
	Attachments    []attachmentIn `json:"attachments,omitempty"`
}

var errNotOpener = forbid("only whoever opened a conversation asks in it; the member it is addressed to answers, with conversation.answer")

// checkAsk is conversation_ask's rule on arguments checkMessage has taken:
// the caller opened it, it is open, its respondent is an agent
// (errWithAgents) the caller may still address, and people in the site may
// still ask it (notAskable).
func (c *Core) checkAsk(m *member, cv *conversation) error {
	if cv.opener != m {
		return errNotOpener
	}
	if cv.status != "open" {
		return errClosed
	}
	if cv.respondent.actor.kind != "agent" {
		return errWithAgents
	}
	if why := refusal(m, cv.respondent, c.now()); why != "" {
		return notAddressable("the respondent is no longer available to you; start a new conversation with someone who is", why)
	}
	return c.notAskable(cv.respondent)
}

func conversationAsk() *impl {
	return define(spec[askIn]{
		gate:  gateAsks,
		check: func(in askIn) error { return checkMessage(in.Body, in.Attachments) },
		resolve: func(c *Core, co *course, in askIn) (target, error) {
			return conversationTarget(c, co, in.ConversationID)
		},
		validate: func(c *Core, m *member, in askIn) error {
			cv, err := c.findConversation(m.course, in.ConversationID)
			if err != nil {
				return err
			}
			if err := c.checkAsk(m, cv); err != nil {
				return err
			}
			return c.checkMessageFiles(m, cv, in.Attachments, false)
		},
		pin: func(c *Core, m *member, in askIn) error {
			cv, err := c.findConversation(m.course, in.ConversationID)
			if err != nil {
				return err
			}
			return c.checkMessageFiles(m, cv, in.Attachments, true)
		},
		execute: func(c *Core, ec *execCtx, in askIn) (any, error) {
			cv, err := c.findConversation(ec.course, in.ConversationID)
			if err != nil {
				return nil, err
			}
			if err := c.checkAsk(ec.member, cv); err != nil {
				return nil, err
			}
			files, names, err := c.checkFiles(ec.member, in.Attachments, false)
			if err != nil {
				return nil, err
			}
			id, err := c.post(ec, cv, nil, in.Body, nil, files, names)
			if err != nil {
				return nil, err
			}
			return map[string]string{"message_id": id}, nil
		},
	})
}

// Why a proposal was cancelled (Core's pipeline.Cancel*).
const (
	cancelExpired         = "proposal_expired"
	cancelReauthorization = "reauthorization_failed"
	cancelTargetGone      = "target_gone"
	cancelMemberRemoved   = "member_removed"
	cancelToolRemoved     = "tool_removed"
)

type decideIn struct {
	inCourse
	ActionID uuid.UUID `json:"action_id"`
	Decision string    `json:"decision"`
	Reason   *string   `json:"reason,omitempty"`
}

// decideOut reports what became of the proposal, which is not what became
// of the decision: approving a proposal whose proposer may no longer make it
// succeeds as a decision and cancels the proposal.
type decideOut struct {
	ActionID string          `json:"action_id"`
	Outcome  string          `json:"outcome"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *apiError       `json:"error,omitempty"`
	// ByOwner: the proposer's owner decided it.
	ByOwner bool `json:"by_owner,omitempty"`
}

// sameParty reports whether two actors are one party for four eyes: the
// same actor, one the other's owner, or two agents of one owner.
func sameParty(x, y *actor) bool {
	return x == y || x.owner == y || y.owner == x || (x.owner != nil && x.owner == y.owner)
}

// ownAgentsJudge is action_decide's and action_review's Gate.OwnAgents: an
// agent's owner decides or reviews their own agent's action at autonomous
// where they could have done it themselves just now without anyone's
// confirmation (ownerJudges), whatever their own action_decide.
func ownAgentsJudge(c *Core, caller *actor, seat *member, tgt target) level {
	if tgt.id == nil {
		return denied
	}
	a := c.actions[*tgt.id]
	if a == nil {
		return denied
	}
	if _, may, _ := c.ownerJudges(caller, seat, a); may {
		return autonomous
	}
	return denied
}

// ownerJudges says whether caller, from seat, is the owner of the agent
// that did a (owner), and whether they could have done a themselves just
// now without anyone's confirmation (may): their own level for it
// autonomous (for an answer, their action_decide: gate.ownerJudgedBy), its
// target within their reach, and, for a proposal, approving it now not
// refused for what it asks (refusal), which is then refused (Core's
// pipeline.ownerJudges).
func (c *Core) ownerJudges(caller *actor, seat *member, a *action) (owner, may bool, refused *apiError) {
	if a.member == nil || a.member == seat || a.actor == caller || a.actor.owner != caller {
		return false, false, nil
	}
	t := c.cat.byName[a.actionType]
	if t == nil || t.impl == nil {
		return true, false, nil
	}
	tool := t
	if by := t.impl.gate.ownerJudgedBy; len(by) > 0 {
		// A permission no person holds, conversation_answer: the owner is
		// measured by what judging it is instead.
		im, def := *t.impl, *t
		im.gate.perms, im.gate.any, im.gate.ownAgents = by, false, nil
		def.impl = &im
		t = &def
	}
	args, err := t.parseArgs(a.payload)
	if err != nil {
		return true, false, nil
	}
	got, err := c.authorize(t, args, caller, seat)
	if err != nil || got.decision.level != autonomous {
		return true, false, nil
	}
	if a.status == actProposed {
		if refused := c.refusal(tool, a, args); refused != nil {
			return true, false, refused
		}
	}
	return true, true, nil
}

// refusal is what approving proposal a, of tool t with arguments args,
// would be refused with now for what it asks, before anything is carried
// out: the tool's check, and its validate as the proposer's (Core's
// pipeline.refusal); nil when neither refuses it. It changes nothing.
func (c *Core) refusal(t *toolDef, a *action, args any) *apiError {
	asked := func(err error) *apiError {
		if e, ok := asAPI(err); ok {
			return e
		}
		return &apiError{Code: codeInternal, Message: "something went wrong on our side; retry with the same idempotency_key"}
	}
	if t.impl.check != nil {
		if err := t.impl.check(args); err != nil {
			return asked(err)
		}
	}
	if t.impl.validate != nil {
		if err := t.impl.validate(c, a.member, args); err != nil {
			return asked(err)
		}
	}
	return nil
}

// errOwnerNotAutonomous refuses an agent's owner who could not have done
// what their agent did without someone's confirmation.
func errOwnerNotAutonomous(what string) *apiError {
	return forbid("you %s what your agent did only where you would do it yourself without anyone's confirmation; "+
		"here your own level for it is lower, or it is beyond your reach, so someone else %ss it", what, what).
		with("reason", "owner_not_autonomous")
}

// errOwnerWouldBeRefused refuses an agent's owner a decision about its
// proposal that, approved now, would be refused as refused says (AIShie-Core
// #60): the owner may well hold the action at autonomous.
func errOwnerWouldBeRefused(refused *apiError) *apiError {
	return forbid("you decide what your agent proposed only where you would do it yourself without anyone's confirmation, "+
		"and approved now it would be refused (%s); take it back with action.withdraw, or someone else rejects it", refused.Message).
		with("reason", "owner_would_be_refused").with("refusal", refused)
}

// The decisions about a proposal. Whoever may reject one may request
// changes to it instead, under the same rules: it ends as a rejection does,
// nothing of it carried out, in changes_requested, with a note of what to
// change, for the proposer to propose again naming it (revises).
const (
	decisionApprove        = "approve"
	decisionReject         = "reject"
	decisionRequestChanges = "request_changes"
)

// maxChangesNoteChars bounds the note a request for changes carries, its
// reason, in characters.
const maxChangesNoteChars = 2000

// checkDecision is action.decide's check (Core's CheckDecision): a decision
// is to approve, to reject, or to request changes, which says what to
// change (changesNote).
func checkDecision(in decideIn) error {
	switch in.Decision {
	case decisionApprove, decisionReject:
		return nil
	case decisionRequestChanges:
		_, err := changesNote(in.Reason)
		return err
	}
	return invalid("decision must be %q, %q or %q", decisionApprove, decisionReject, decisionRequestChanges)
}

// changesNote is what a request for changes asks of its proposer: its
// reason, trimmed, 1 to maxChangesNoteChars characters (Core's changesNote).
func changesNote(reason *string) (string, error) {
	if reason == nil || strings.TrimSpace(*reason) == "" {
		return "", invalid("a request for changes says what to change, in reason").with("reason", "note_required")
	}
	note := strings.TrimSpace(*reason)
	if n := utf8.RuneCountInString(note); n > maxChangesNoteChars {
		return "", invalid("the note is %d characters long; the most is %d", n, maxChangesNoteChars).with("reason", "note_too_long")
	}
	return note, nil
}

func actionDecide() *impl {
	return define(spec[decideIn]{
		gate:  gate{perms: []string{permActionDecide}, ownAgents: ownAgentsJudge},
		check: checkDecision,
		resolve: func(c *Core, co *course, in decideIn) (target, error) {
			a := c.actions[in.ActionID.String()]
			if a == nil || a.course != co {
				return target{}, missing("no such action in this course")
			}
			return target{typ: "action", id: &a.id}, nil
		},
		// What deciding would refuse is refused before anything is decided
		// or proposed: a party agent's decision is never proposed for a
		// person to confirm and fail (Core's ValidateDecision).
		validate: func(c *Core, m *member, in decideIn) error {
			prop := c.actions[in.ActionID.String()]
			if prop == nil || prop.course == nil || prop.course.id != in.CourseID.String() {
				return missing("no such action in this course")
			}
			_, err := c.refuseDecision(m, prop)
			return err
		},
		execute: func(c *Core, ec *execCtx, in decideIn) (any, error) {
			return c.decide(ec, in)
		},
	})
}

// refuseDecision says why seat m may not decide proposal prop now, or nil
// when it may; and, when it may, whether it decides it as the owner of the
// agent that proposed it (byOwner), as Core's refuseDecision does. It
// changes nothing.
func (c *Core) refuseDecision(m *member, prop *action) (byOwner bool, err error) {
	if prop.status != actProposed {
		return false, conflicts("the action is %s, not awaiting a decision", prop.status)
	}
	if m == nil || prop.member == nil {
		return false, forbid("only a course member decides a member's proposal")
	}
	if prop.member == m || sameParty(prop.actor, m.actor) {
		// An agent decides nothing its owner proposed, nor another of the
		// owner's agents anything it did. Its owner decides what it
		// proposed only where they could have done it themselves without
		// anyone's confirmation, and approving it now would not be refused.
		owner, may, refused := c.ownerJudges(m.actor, m, prop)
		switch {
		case owner && refused != nil:
			return false, errOwnerWouldBeRefused(refused)
		case owner && !may:
			return false, errOwnerNotAutonomous("decide")
		case !may:
			return false, forbid("nobody decides their own proposal, nor their owner's, nor another agent's of their owner")
		}
		byOwner = true
	}
	if c.judgesOwn(prop, m, m.actor) {
		return false, forbid("nobody decides their own proposal, even at one remove: this one decides or reviews an action of yours")
	}
	return byOwner, nil
}

// decide approves, rejects or sends back a proposal, as Core's
// pipeline.Decide does:
// the proposer authorized again, now, against the seat it proposed from,
// what it asks checked again as the proposer's, and the tool carried out
// as the proposer under the proposal's id.
func (c *Core) decide(ec *execCtx, in decideIn) (any, error) {
	prop := c.actions[in.ActionID.String()]
	if prop == nil || prop.course != ec.course {
		return nil, missing("no such action in this course")
	}
	byOwner, err := c.refuseDecision(ec.member, prop)
	if err != nil {
		return nil, err
	}
	// said is what an event, and the record of a rejection, say of the
	// decision: that the proposer's owner made it, when they did.
	said := func(m map[string]any) map[string]any {
		if byOwner {
			if m == nil {
				m = map[string]any{}
			}
			m["by_owner"] = true
		}
		return m
	}
	cancel := func(code string, details map[string]any) decideOut {
		o := c.cancelProposal(ec, prop, code, details)
		o.ByOwner = byOwner
		return o
	}
	if ttl := c.proposalTTL(); ttl > 0 && prop.createdAt.Add(ttl).Before(ec.now) {
		return cancel(cancelExpired, nil), nil
	}
	if in.Decision == decisionReject {
		prop.result = mustJSON(map[string]any{"decision": said(map[string]any{
			"decision": decisionReject, "reason": in.Reason, "by_action_id": ec.actionID})})
		c.finish(prop, actRejected, ec.member, ec.now)
		ec.emit(proposalEvent("action.rejected", prop, ec.actionID, said(nil)))
		return decideOut{ActionID: prop.id, Outcome: actRejected, ByOwner: byOwner}, nil
	}
	if in.Decision == decisionRequestChanges {
		// As a rejection, with the note it carries as its reason: a
		// proposer reads both the same way. The event carries no note, as
		// no event carries what was written.
		note, err := changesNote(in.Reason)
		if err != nil {
			return nil, err
		}
		prop.result = mustJSON(map[string]any{"decision": said(map[string]any{
			"decision": decisionRequestChanges, "reason": note, "by_action_id": ec.actionID})})
		c.finish(prop, actChangesRequested, ec.member, ec.now)
		ec.emit(proposalEvent("action.changes_requested", prop, ec.actionID, said(nil)))
		return decideOut{ActionID: prop.id, Outcome: actChangesRequested, ByOwner: byOwner}, nil
	}

	t := c.cat.byName[prop.actionType]
	if t == nil || t.impl == nil {
		return cancel(cancelToolRemoved, nil), nil
	}
	// Read back by the schema alone: what its check refuses fails below.
	args, err := t.parseArgs(prop.payload)
	if err != nil {
		return cancel(cancelToolRemoved, map[string]any{"detail": err.Error()}), nil
	}
	a, err := c.authorize(t, args, prop.actor, prop.member)
	if e, ok := asAPI(err); ok && e.Code == codeNotFound {
		return cancel(cancelTargetGone, nil), nil
	}
	if err != nil {
		return nil, err
	}
	if !a.decision.level.allowed() {
		return cancel(cancelReauthorization, map[string]any{"authz_reason": a.decision.reason}), nil
	}
	fail := func(err error) (any, error) {
		e, ok := asAPI(err)
		if !ok {
			return nil, err
		}
		prop.result = errorResult(e)
		c.finish(prop, actFailed, ec.member, ec.now)
		ec.emit(proposalEvent("action.approved", prop, ec.actionID, said(map[string]any{"outcome": actFailed, "error": e.Code})))
		return decideOut{ActionID: prop.id, Outcome: actFailed, Error: e, ByOwner: byOwner}, nil
	}
	if t.impl.check != nil {
		if err := t.impl.check(args); err != nil {
			return fail(err)
		}
	}
	if t.impl.validate != nil {
		if err := t.impl.validate(c, a.decision.member, args); err != nil {
			return fail(err)
		}
	}
	child := &execCtx{now: ec.now, actor: prop.actor, member: a.decision.member, course: prop.course, actionID: prop.id,
		createdAt: prop.createdAt}
	res, err := t.impl.execute(c, child, args)
	if err != nil {
		return fail(err)
	}
	full, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	prop.result = full
	c.finish(prop, actExecuted, ec.member, ec.now)
	ec.emit(proposalEvent("action.approved", prop, ec.actionID, said(map[string]any{"outcome": actExecuted})))
	for _, e := range child.events {
		ec.emit(e)
	}
	return decideOut{ActionID: prop.id, Outcome: actExecuted, Result: full, ByOwner: byOwner}, nil
}

// judgesOwn reports whether a is a decision or a review about an action of
// m's, or of act's party, at any remove: approving it would carry out what
// they proposed, with four eyes that are their own two. The chain runs back
// in time, so it ends.
func (c *Core) judgesOwn(a *action, m *member, act *actor) bool {
	for (a.actionType == toolActionDecide || a.actionType == toolActionReview) && a.targetID != nil {
		about := c.actions[*a.targetID]
		if about == nil {
			return false
		}
		if about.member == m || sameParty(about.actor, act) {
			return true
		}
		a = about
	}
	return false
}

const (
	toolActionDecide = "action.decide"
	toolActionReview = "action.review"
)

type reviewIn struct {
	inCourse
	ActionID uuid.UUID `json:"action_id"`
	Outcome  string    `json:"outcome"`
	Note     *string   `json:"note,omitempty"`
}

func actionReview() *impl {
	return define(spec[reviewIn]{
		gate:  gate{perms: []string{permActionDecide}, ownAgents: ownAgentsJudge},
		check: checkReview,
		resolve: func(c *Core, co *course, in reviewIn) (target, error) {
			a := c.actions[in.ActionID.String()]
			if a == nil || a.course != co {
				return target{}, missing("no such action in this course")
			}
			return target{typ: "action", id: &a.id}, nil
		},
		// As action.decide's (Core's ValidateReview).
		validate: func(c *Core, m *member, in reviewIn) error {
			row := c.actions[in.ActionID.String()]
			if row == nil || row.course == nil || row.course.id != in.CourseID.String() {
				return missing("no such action in this course")
			}
			_, err := c.refuseReview(m, row, in.Outcome)
			return err
		},
		execute: func(c *Core, ec *execCtx, in reviewIn) (any, error) {
			return c.review(ec, in)
		},
	})
}

// checkReview is action.review's check (Core's CheckReview).
func checkReview(in reviewIn) error {
	if in.Outcome != reviewReviewed && in.Outcome != reviewEscalated {
		return invalid("outcome must be %q or %q", reviewReviewed, reviewEscalated)
	}
	return nil
}

// canReview reports whether a review may move from one state to another:
// pending to reviewed or escalated, escalated to reviewed.
func canReview(from, to string) bool {
	switch from {
	case reviewPending:
		return to == reviewReviewed || to == reviewEscalated
	case reviewEscalated:
		return to == reviewReviewed
	}
	return false
}

// refuseReview says why seat m may not review action row as outcome says
// now, or nil when it may; and, when it may, whether it reviews it as the
// owner of the agent that did it (byOwner), as Core's refuseReview does.
// It changes nothing.
func (c *Core) refuseReview(m *member, row *action, outcome string) (byOwner bool, err error) {
	from := row.reviewState
	if !canReview(from, outcome) {
		if from == reviewEscalated {
			return false, conflicts("the action is already escalated")
		}
		return false, conflicts("the action is not awaiting review")
	}
	if m == nil {
		return false, forbid("only a course member reviews")
	}
	if row.member == m || sameParty(row.actor, m.actor) {
		// An agent does not review its owner's action, nor a sibling's;
		// its owner reviews what it did only where they could have done
		// it themselves without anyone's confirmation.
		owner, may, _ := c.ownerJudges(m.actor, m, row)
		switch {
		case owner && !may:
			return false, errOwnerNotAutonomous("review")
		case !may:
			return false, forbid("nobody reviews their own action, nor their owner's, nor another agent's of their owner")
		}
		byOwner = true
	}
	if c.judgesOwn(row, m, m.actor) {
		return false, forbid("nobody reviews their own action, even at one remove: this one decides or reviews an action of yours")
	}
	if from == reviewEscalated && c.escalatedBy(row, m.actor) {
		return false, forbid("an escalation is for someone else to look at")
	}
	return byOwner, nil
}

// review records that a person looked at an action that executed pending
// review, as Core's pipeline.Review does. It undoes nothing.
func (c *Core) review(ec *execCtx, in reviewIn) (any, error) {
	row := c.actions[in.ActionID.String()]
	if row == nil || row.course != ec.course {
		return nil, missing("no such action in this course")
	}
	byOwner, err := c.refuseReview(ec.member, row, in.Outcome)
	if err != nil {
		return nil, err
	}
	at := ec.now
	row.reviewState, row.reviewedBy, row.reviewedAt = in.Outcome, ec.member, &at
	typ := "action.reviewed"
	if in.Outcome == reviewEscalated {
		typ = "action.escalated"
	}
	var extra map[string]any
	if byOwner {
		extra = map[string]any{"by_owner": true}
	}
	ec.emit(proposalEvent(typ, row, ec.actionID, extra))
	return struct {
		ActionID    string `json:"action_id"`
		ReviewState string `json:"review_state"`
		ByOwner     bool   `json:"by_owner,omitempty"`
	}{row.id, in.Outcome, byOwner}, nil
}

// escalatedBy reports whether act, or anyone of its party, escalated a: an
// executed review of it with outcome escalated. (Core also counts whoever
// approved such a review when it was a proposal; the fake's reviews of the
// tests' own making are not proposed.)
func (c *Core) escalatedBy(a *action, act *actor) bool {
	for _, r := range c.actionList {
		if r.actionType == toolActionReview && r.status == actExecuted && r.targetID != nil && *r.targetID == a.id &&
			payloadString(r.payload, "outcome") == reviewEscalated && sameParty(r.actor, act) {
			return true
		}
	}
	return false
}

// finish moves a proposal to its end state.
func (c *Core) finish(prop *action, status string, decidedBy *member, now time.Time) {
	prop.status = status
	if decidedBy != nil {
		prop.decidedBy = decidedBy
		t := now
		prop.decidedAt = &t
	}
	if status == actExecuted {
		t := now
		prop.executedAt = &t
	}
}

// cancelProposal ends a proposal without executing it and without blaming
// anyone: it was fine when it was made and is not any more.
func (c *Core) cancelProposal(ec *execCtx, prop *action, code string, details map[string]any) decideOut {
	e := cancellation(code, details)
	prop.result = errorResult(e)
	c.finish(prop, actCancelled, nil, ec.now)
	ec.emit(proposalEvent("action.cancelled", prop, ec.actionID, map[string]any{"reason": code}))
	return decideOut{ActionID: prop.id, Outcome: actCancelled, Error: e}
}

// cancellation is the error a cancelled proposal carries, stored as its
// result so that a replay reads the same whoever cancelled it.
func cancellation(code string, details map[string]any) *apiError {
	e := precondition("the proposal can no longer be carried out").with("reason", code)
	for k, v := range details {
		e = e.with(k, v)
	}
	return e
}

// proposalEvent is filed under the proposal's id, so that its proposer finds
// it in its own feed.
func proposalEvent(typ string, prop *action, byAction string, extra map[string]any) *event {
	id := prop.id
	payload := map[string]any{"action_type": prop.actionType, "by_action_id": byAction}
	for k, v := range extra {
		payload[k] = v
	}
	return &event{typ: typ, course: prop.course, actionID: &id, subjectType: "action", subjectID: &id, payload: mustJSON(payload)}
}
