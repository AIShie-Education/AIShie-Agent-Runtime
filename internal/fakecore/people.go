package fakecore

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// The writes people make that the fake carries out too, so that the test
// controls act through Core's own pipeline and rules: opening a
// conversation, asking in it, and deciding a proposal. Anyone holding a
// person's token may call them over MCP or REST as well.

var gateAsks = gate{perms: []string{permConversationAsk}}

type openIn struct {
	inCourse
	RespondentMemberID uuid.UUID `json:"respondent_member_id"`
	Title              *string   `json:"title,omitempty"`
	Body               *string   `json:"body,omitempty"`
}

// checkOpen is conversation_open's rule: a title and a body that fit, and a
// respondent the caller may address.
func (c *Core) checkOpen(m, respondent *member, in openIn) error {
	if _, err := optionalText("title", in.Title, 200); err != nil {
		return err
	}
	if in.Body != nil {
		if err := checkBody(*in.Body); err != nil {
			return err
		}
	}
	if why := refusal(m, respondent, c.now()); why != "" {
		return notAddressable("you may not address that member", why)
	}
	return nil
}

func conversationOpen() *impl {
	return define(spec[openIn]{
		gate: gateAsks,
		resolve: func(c *Core, co *course, in openIn) (target, error) {
			if r := c.members[in.RespondentMemberID.String()]; r == nil || r.course != co {
				return target{}, missing("no such member in this course")
			}
			return target{typ: "conversation"}, nil
		},
		pin: func(c *Core, m *member, in openIn) error {
			return c.checkOpen(m, c.members[in.RespondentMemberID.String()], in)
		},
		execute: func(c *Core, ec *execCtx, in openIn) (any, error) {
			respondent := c.members[in.RespondentMemberID.String()]
			if err := c.checkOpen(ec.member, respondent, in); err != nil {
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
				id, err := c.post(ec, cv, nil, *in.Body, nil)
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
	ConversationID uuid.UUID `json:"conversation_id"`
	Body           string    `json:"body"`
}

var errNotOpener = forbid("only whoever opened a conversation asks in it; the member it is addressed to answers, with conversation.answer")

// checkAsk is conversation_ask's rule: the caller opened it, it is open, the
// body fits, and the caller may still address its respondent.
func (c *Core) checkAsk(m *member, cv *conversation, body string) error {
	if cv.opener != m {
		return errNotOpener
	}
	if cv.status != "open" {
		return errClosed
	}
	if err := checkBody(body); err != nil {
		return err
	}
	if why := refusal(m, cv.respondent, c.now()); why != "" {
		return notAddressable("the respondent is no longer available to you; start a new conversation with someone who is", why)
	}
	return nil
}

func conversationAsk() *impl {
	return define(spec[askIn]{
		gate: gateAsks,
		resolve: func(c *Core, co *course, in askIn) (target, error) {
			return conversationTarget(c, co, in.ConversationID)
		},
		pin: func(c *Core, m *member, in askIn) error {
			cv, err := c.findConversation(m.course, in.ConversationID)
			if err != nil {
				return err
			}
			return c.checkAsk(m, cv, in.Body)
		},
		execute: func(c *Core, ec *execCtx, in askIn) (any, error) {
			cv, err := c.findConversation(ec.course, in.ConversationID)
			if err != nil {
				return nil, err
			}
			if err := c.checkAsk(ec.member, cv, in.Body); err != nil {
				return nil, err
			}
			id, err := c.post(ec, cv, nil, in.Body, nil)
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
}

// sameParty reports whether two actors are one party for four eyes: the
// same actor, one the other's owner, or two agents of one owner.
func sameParty(x, y *actor) bool {
	return x == y || x.owner == y || y.owner == x || (x.owner != nil && x.owner == y.owner)
}

func actionDecide() *impl {
	return define(spec[decideIn]{
		gate: gate{perms: []string{permActionDecide}},
		resolve: func(c *Core, co *course, in decideIn) (target, error) {
			a := c.actions[in.ActionID.String()]
			if a == nil || a.course != co {
				return target{}, missing("no such action in this course")
			}
			return target{typ: "action", id: &a.id}, nil
		},
		execute: func(c *Core, ec *execCtx, in decideIn) (any, error) {
			return c.decide(ec, in)
		},
	})
}

// decide approves or rejects a proposal, as Core's pipeline.Decide does:
// the proposer authorized again, now, against the seat it proposed from,
// and the tool carried out as the proposer under the proposal's id.
func (c *Core) decide(ec *execCtx, in decideIn) (any, error) {
	if in.Decision != "approve" && in.Decision != "reject" {
		return nil, invalid("decision must be %q or %q", "approve", "reject")
	}
	prop := c.actions[in.ActionID.String()]
	if prop == nil || prop.course != ec.course {
		return nil, missing("no such action in this course")
	}
	if prop.status != actProposed {
		return nil, conflicts("the action is %s, not awaiting a decision", prop.status)
	}
	if ec.member == nil || prop.member == nil {
		return nil, forbid("only a course member decides a member's proposal")
	}
	if prop.member == ec.member || sameParty(prop.actor, ec.actor) {
		return nil, forbid("nobody decides their own proposal, nor their agent's, nor their owner's")
	}
	if c.judgesOwn(prop, ec.member, ec.actor) {
		return nil, forbid("nobody decides their own proposal, even at one remove: this one decides or reviews an action of yours")
	}
	if ttl := c.proposalTTL(); ttl > 0 && prop.createdAt.Add(ttl).Before(ec.now) {
		return c.cancelProposal(ec, prop, cancelExpired, nil), nil
	}
	if in.Decision == "reject" {
		prop.result = mustJSON(map[string]any{"decision": map[string]any{
			"decision": "reject", "reason": in.Reason, "by_action_id": ec.actionID}})
		c.finish(prop, actRejected, ec.member, ec.now)
		ec.emit(proposalEvent("action.rejected", prop, ec.actionID, nil))
		return decideOut{ActionID: prop.id, Outcome: actRejected}, nil
	}

	t := c.cat.byName[prop.actionType]
	if t == nil || t.impl == nil {
		return c.cancelProposal(ec, prop, cancelToolRemoved, nil), nil
	}
	args, err := t.decodeArgs(prop.payload)
	if err != nil {
		return c.cancelProposal(ec, prop, cancelToolRemoved, map[string]any{"detail": err.Error()}), nil
	}
	a, err := c.authorize(t, args, prop.actor, prop.member)
	if e, ok := asAPI(err); ok && e.Code == codeNotFound {
		return c.cancelProposal(ec, prop, cancelTargetGone, nil), nil
	}
	if err != nil {
		return nil, err
	}
	if !a.decision.level.allowed() {
		return c.cancelProposal(ec, prop, cancelReauthorization, map[string]any{"authz_reason": a.decision.reason}), nil
	}
	child := &execCtx{now: ec.now, actor: prop.actor, member: a.decision.member, course: prop.course, actionID: prop.id,
		createdAt: prop.createdAt}
	res, err := t.impl.execute(c, child, args)
	if err != nil {
		e, ok := asAPI(err)
		if !ok {
			return nil, err
		}
		prop.result = errorResult(e)
		c.finish(prop, actFailed, ec.member, ec.now)
		ec.emit(proposalEvent("action.approved", prop, ec.actionID, map[string]any{"outcome": actFailed, "error": e.Code}))
		return decideOut{ActionID: prop.id, Outcome: actFailed, Error: e}, nil
	}
	full, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	prop.result = full
	c.finish(prop, actExecuted, ec.member, ec.now)
	ec.emit(proposalEvent("action.approved", prop, ec.actionID, map[string]any{"outcome": actExecuted}))
	for _, e := range child.events {
		ec.emit(e)
	}
	return decideOut{ActionID: prop.id, Outcome: actExecuted, Result: full}, nil
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
		gate: gate{perms: []string{permActionDecide}},
		resolve: func(c *Core, co *course, in reviewIn) (target, error) {
			a := c.actions[in.ActionID.String()]
			if a == nil || a.course != co {
				return target{}, missing("no such action in this course")
			}
			return target{typ: "action", id: &a.id}, nil
		},
		execute: func(c *Core, ec *execCtx, in reviewIn) (any, error) {
			return c.review(ec, in)
		},
	})
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

// review records that a person looked at an action that executed pending
// review, as Core's pipeline.Review does. It undoes nothing.
func (c *Core) review(ec *execCtx, in reviewIn) (any, error) {
	if in.Outcome != reviewReviewed && in.Outcome != reviewEscalated {
		return nil, invalid("outcome must be %q or %q", reviewReviewed, reviewEscalated)
	}
	row := c.actions[in.ActionID.String()]
	if row == nil || row.course != ec.course {
		return nil, missing("no such action in this course")
	}
	from := row.reviewState
	if !canReview(from, in.Outcome) {
		if from == reviewEscalated {
			return nil, conflicts("the action is already escalated")
		}
		return nil, conflicts("the action is not awaiting review")
	}
	if ec.member == nil {
		return nil, forbid("only a course member reviews")
	}
	if row.member == ec.member || sameParty(row.actor, ec.actor) {
		return nil, forbid("nobody reviews their own action, nor their agent's, nor their owner's")
	}
	if c.judgesOwn(row, ec.member, ec.actor) {
		return nil, forbid("nobody reviews their own action, even at one remove: this one decides or reviews an action of yours")
	}
	if from == reviewEscalated && c.escalatedBy(row, ec.actor) {
		return nil, forbid("an escalation is for someone else to look at")
	}
	at := ec.now
	row.reviewState, row.reviewedBy, row.reviewedAt = in.Outcome, ec.member, &at
	typ := "action.reviewed"
	if in.Outcome == reviewEscalated {
		typ = "action.escalated"
	}
	ec.emit(proposalEvent(typ, row, ec.actionID, nil))
	return struct {
		ActionID    string `json:"action_id"`
		ReviewState string `json:"review_state"`
	}{row.id, in.Outcome}, nil
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
