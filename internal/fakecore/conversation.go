package fakecore

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// The conversation tools, with Core's semantics (Core's
// internal/tools/conversation.go, docs/schema.md §2.8): who may address
// whom, what an answer is checked against and in which order, and what the
// views show.

const (
	maxMessageChars = 20000
	maxReasonChars  = 500
	// seatRemoved is what closing a removed seat's conversations says,
	// and closedWithAPerson what closing the conversations people were
	// asked in did (Core's ClosedWithAPerson).
	seatRemoved       = "seat_removed"
	closedWithAPerson = ceilingConversationsAreWithAgents
)

// The states a conversation is in, as the views say.
const (
	stateAwaitingAnswer       = "awaiting_answer"
	stateReplyPendingApproval = "reply_pending_approval"
	stateAnswered             = "answered"
	stateClosed               = "closed"
)

var (
	gateAnswers   = gate{perms: []string{permConversationAnswer}, refusal: personAnswersNothing}
	gateConverses = gate{perms: []string{permDocumentRead}}
)

// errWithAgents is Core's refusal of a person as a conversation's
// respondent, and of a person answering: conversations are between a person
// and an agent.
var errWithAgents = forbid("conversations are between a person and an agent: a person answers none, and is asked "+
	"nothing here; people talk to people elsewhere").with("reason", ceilingConversationsAreWithAgents)

// personAnswersNothing is gateAnswers' refusal of a person (Core's).
func personAnswersNothing(caller *actor, _ *member) *apiError {
	if caller.kind == "agent" {
		return nil
	}
	return errWithAgents
}

// Refusals Core words the same everywhere.
var (
	errNoConversation = missing("no such conversation in this course")
	errClosed         = conflicts("the conversation is closed; start a new one").with("reason", "closed")
	errNotRespondent  = forbid("only the member a conversation is addressed to answers in it")
)

func notAddressable(prefix, why string) *apiError {
	return forbid("%s: %s", prefix, why).with("reason", "not_addressable")
}

func (c *Core) findConversation(co *course, id uuid.UUID) (*conversation, error) {
	cv := c.conversations[id.String()]
	if cv == nil || cv.course != co {
		return nil, errNoConversation
	}
	return cv, nil
}

func conversationTarget(c *Core, co *course, id uuid.UUID) (target, error) {
	if _, err := c.findConversation(co, id); err != nil {
		return target{}, err
	}
	s := id.String()
	return target{typ: "conversation", id: &s}, nil
}

// checkBody holds a message to some text of at most 20,000 characters,
// counted as Unicode code points.
func checkBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return invalid("the message is empty")
	}
	if n := utf8.RuneCountInString(body); n > maxMessageChars {
		return invalid("the message is %d characters long; the most is %d", n, maxMessageChars)
	}
	return nil
}

// optionalText trims a title or a reason, nil for none, and refuses one
// longer than most characters.
func optionalText(what string, s *string, most int) (*string, error) {
	if s == nil {
		return nil, nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(t) > most {
		return nil, invalid("the %s is longer than %d characters", what, most)
	}
	return &t, nil
}

// latestOpenerMessage is the opener's newest message, retracted or not.
func (cv *conversation) latestOpenerMessage() *message {
	for i := len(cv.messages) - 1; i >= 0; i-- {
		if cv.messages[i].author == cv.opener {
			return cv.messages[i]
		}
	}
	return nil
}

// errWithdrawn refuses an answer once the opener's latest message is
// retracted: nothing waits for an answer, so it names no message to answer.
var errWithdrawn = conflicts("the question was withdrawn: nothing waits for an answer now").with("reason", "moved_on")

// withdrawn reports whether cv's opener has withdrawn what they asked last,
// retracting their latest message: then nothing waits for an answer in it,
// but with Options.WithdrawnWaits.
func (c *Core) withdrawn(cv *conversation) bool {
	latest := cv.latestOpenerMessage()
	return latest != nil && latest.retraction != nil && !c.opts.WithdrawnWaits
}

// newerQuestion refuses an answer to anything but the opener's latest
// message, and to that message once it is answered or retracted: whoever
// answers answers what was last asked, once, while it is still asked.
func (c *Core) newerQuestion(cv *conversation, answered string) error {
	latest := cv.latestOpenerMessage()
	if latest == nil {
		return invalid("in_reply_to_message_id must name a message of the opener's in this conversation")
	}
	if c.withdrawn(cv) {
		return errWithdrawn
	}
	if latest.id != answered {
		return conflicts("the conversation moved on; answer the latest message").
			with("reason", "moved_on").with("latest_opener_message_id", latest.id)
	}
	for _, m := range cv.messages {
		if m.author == cv.respondent && m.seq > latest.seq {
			return conflicts("the latest message is answered already").with("reason", "already_answered")
		}
	}
	return nil
}

// pendingAnswer is the newest answer of m's to message msgID that waits for
// a decision, or nil.
func (c *Core) pendingAnswer(cv *conversation, m *member, msgID string) *action {
	for i := len(cv.answers) - 1; i >= 0; i-- {
		a := cv.answers[i]
		if a.status == actProposed && a.member == m && payloadString(a.payload, "in_reply_to_message_id") == msgID {
			return a
		}
	}
	return nil
}

const toolConversationAnswer = "conversation.answer"

type answerIn struct {
	inCourse
	ConversationID     uuid.UUID      `json:"conversation_id"`
	InReplyToMessageID uuid.UUID      `json:"in_reply_to_message_id"`
	Body               string         `json:"body"`
	Attachments        []attachmentIn `json:"attachments,omitempty"`
}

// checkMessage is what a message's arguments say alone (Core's
// checkMessageArgs), conversation_ask's and conversation_answer's check:
// some text, within its length, and files a message may carry, each named
// as a file may be.
func checkMessage(body string, files []attachmentIn) error {
	if err := checkBody(body); err != nil {
		return err
	}
	_, err := shapeFiles(files)
	return err
}

// checkAnswer is conversation_answer's rule, all but newerQuestion and
// what its arguments say alone: the caller is the respondent, the
// conversation is open, the opener may still address the caller, and
// in_reply_to is the opener's.
func (c *Core) checkAnswer(m *member, cv *conversation, in answerIn) error {
	if cv.respondent != m {
		return errNotRespondent
	}
	if cv.status != "open" {
		return errClosed
	}
	if why := refusal(cv.opener, m, c.now()); why != "" {
		return notAddressable("you may no longer answer in this conversation", why)
	}
	asked := c.messages[in.InReplyToMessageID.String()]
	if asked == nil || asked.conv.course != cv.course || asked.conv != cv || asked.author != cv.opener {
		return invalid("in_reply_to_message_id must name a message of the opener's in this conversation")
	}
	return nil
}

func conversationAnswer() *impl {
	answers := gateAnswers
	// No person holds conversation_answer: an agent's owner decides its
	// answer as far as they decide actions.
	answers.ownerJudgedBy = []string{permActionDecide}
	return define(spec[answerIn]{
		gate:  answers,
		check: func(in answerIn) error { return checkMessage(in.Body, in.Attachments) },
		resolve: func(c *Core, co *course, in answerIn) (target, error) {
			return conversationTarget(c, co, in.ConversationID)
		},
		// Checked before it is carried out or proposed, so that nobody is
		// asked to approve what could never run, and again when it is
		// approved; and whether the question it answers is still the one
		// waiting, so that its owner is not asked to approve it once the
		// opener has written again, or withdrawn it.
		validate: func(c *Core, m *member, in answerIn) error {
			cv, err := c.findConversation(m.course, in.ConversationID)
			if err != nil {
				return err
			}
			if err := c.checkAnswer(m, cv, in); err != nil {
				return err
			}
			if err := c.newerQuestion(cv, in.InReplyToMessageID.String()); err != nil {
				return err
			}
			return c.checkMessageFiles(m, cv, in.Attachments, false)
		},
		pin: func(c *Core, m *member, in answerIn) error {
			cv, err := c.findConversation(m.course, in.ConversationID)
			if err != nil {
				return err
			}
			if c.pendingAnswer(cv, m, in.InReplyToMessageID.String()) != nil {
				return conflicts("an answer of yours to that message already waits for a decision").with("reason", "answer_pending")
			}
			if err := c.checkMessageFiles(m, cv, in.Attachments, true); err != nil {
				return err
			}
			// Proposed, the answer takes its draft's place.
			cv.clearDraft()
			return nil
		},
		execute: func(c *Core, ec *execCtx, in answerIn) (any, error) {
			cv, err := c.findConversation(ec.course, in.ConversationID)
			if err != nil {
				return nil, err
			}
			if cv.respondent != ec.member {
				return nil, errNotRespondent
			}
			if err := c.checkAnswer(ec.member, cv, in); err != nil {
				return nil, err
			}
			files, names, err := c.checkFiles(ec.member, in.Attachments, false)
			if err != nil {
				return nil, err
			}
			answered := in.InReplyToMessageID.String()
			id, err := c.post(ec, cv, &answered, in.Body, func() error { return c.newerQuestion(cv, answered) }, files, names)
			if err != nil {
				return nil, err
			}
			// Posted, the answer takes its draft's place.
			cv.clearDraft()
			return map[string]string{"message_id": id}, nil
		},
	})
}

// post writes a message in cv as ec's member, with the files it carries
// (checked already: checkFiles) under their names, after check, if given,
// has passed: Core asks it under the conversation's lock, just before
// writing, and then whether the conversation has room for the files.
func (c *Core) post(ec *execCtx, cv *conversation, inReplyTo *string, body string, check func() error, files []*upload, names []string) (string, error) {
	if cv.status != "open" {
		return "", errClosed
	}
	if check != nil {
		if err := check(); err != nil {
			return "", err
		}
	}
	if err := roomFor(cv, sizeOfUploads(files)); err != nil {
		return "", err
	}
	msg := &message{id: newID(), conv: cv, seq: int32(len(cv.messages) + 1), author: ec.member, inReplyTo: inReplyTo,
		body: body, createdAt: ec.now, actionID: ec.actionID}
	cv.messages = append(cv.messages, msg)
	c.messages[msg.id] = msg
	news := c.attach(msg, files, names)
	at := ec.now
	cv.lastAt, cv.lastAuthor = &at, ec.member
	payload := map[string]any{"conversation_id": cv.id, "message_id": msg.id, "author_member_id": ec.member.id,
		"opener_member_id": cv.opener.id, "respondent_member_id": cv.respondent.id}
	if len(news) > 0 {
		payload["attachments"] = news
	}
	ec.emit(&event{typ: "conversation.message_posted", course: cv.course, subjectType: "conversation", subjectID: &cv.id,
		payload: mustJSON(payload)})
	return msg.id, nil
}

type closeIn struct {
	inCourse
	ConversationID uuid.UUID `json:"conversation_id"`
	Reason         *string   `json:"reason,omitempty"`
}

// checkClose is conversation_close's check: a reason that fits, and none
// of the two the system writes, which would read as its doing.
func checkClose(in closeIn) error {
	reason, err := optionalText("reason", in.Reason, maxReasonChars)
	if err != nil {
		return err
	}
	if reason != nil && strings.EqualFold(*reason, seatRemoved) {
		return invalid("%q is what closing a removed seat's conversations says; give another reason", *reason)
	}
	if reason != nil && strings.EqualFold(*reason, closedWithAPerson) {
		return invalid("%q is what closing the conversations people were asked in says; give another reason", *reason)
	}
	return nil
}

var errClosedAlready = conflicts("the conversation is closed already")

func conversationClose() *impl {
	return define(spec[closeIn]{
		gate:  gateConverses,
		check: checkClose,
		resolve: func(c *Core, co *course, in closeIn) (target, error) {
			return conversationTarget(c, co, in.ConversationID)
		},
		validate: func(c *Core, m *member, in closeIn) error {
			cv, err := c.findConversation(m.course, in.ConversationID)
			switch {
			case err != nil:
				return err
			case m != cv.opener && m != cv.respondent:
				return errNotParticipant
			case cv.status != "open":
				return errClosedAlready
			}
			return nil
		},
		execute: func(c *Core, ec *execCtx, in closeIn) (any, error) {
			cv, err := c.findConversation(ec.course, in.ConversationID)
			if err != nil {
				return nil, err
			}
			return c.closeConversation(ec, cv, in.Reason)
		},
	})
}

var errNotParticipant = forbid("only the two who take part in a conversation close it")

// closeConversation is conversation_close's execution, on a reason
// checkClose has taken.
func (c *Core) closeConversation(ec *execCtx, cv *conversation, why *string) (any, error) {
	if ec.member != cv.opener && ec.member != cv.respondent {
		return nil, errNotParticipant
	}
	reason, _ := optionalText("reason", why, maxReasonChars)
	if cv.status != "open" {
		return nil, errClosedAlready
	}
	cv.status, cv.closedReason = "closed", reason
	cv.clearDraft()
	ec.emit(&event{typ: "conversation.closed", course: cv.course, subjectType: "conversation", subjectID: &cv.id,
		payload: mustJSON(map[string]any{"conversation_id": cv.id, "reason": "closed", "by_member_id": ec.member.id})})
	return map[string]bool{"ok": true}, nil
}

type retractIn struct {
	inCourse
	MessageID uuid.UUID `json:"message_id"`
	Reason    *string   `json:"reason,omitempty"`
}

func conversationRetract() *impl {
	return define(spec[retractIn]{
		gate: gateConverses,
		check: func(in retractIn) error {
			_, err := optionalText("reason", in.Reason, maxReasonChars)
			return err
		},
		resolve: func(c *Core, co *course, in retractIn) (target, error) {
			msg := c.messages[in.MessageID.String()]
			if msg == nil || msg.conv.course != co {
				return target{}, missing("no such message in this course")
			}
			id := msg.id
			return target{typ: "conversation_message", id: &id}, nil
		},
		validate: func(c *Core, m *member, in retractIn) error {
			msg := c.messages[in.MessageID.String()]
			switch {
			case msg == nil || msg.conv.course != m.course:
				return missing("no such message in this course")
			case msg.author != m && !oversees(m, msg.conv.opener):
				return errNotYoursToRetract
			case msg.retraction != nil:
				return errRetractedAlready
			}
			return nil
		},
		execute: func(c *Core, ec *execCtx, in retractIn) (any, error) {
			return c.retractMessage(ec, c.messages[in.MessageID.String()], in.Reason)
		},
	})
}

// conversation_retract's refusals of a message that is not the caller's
// to retract, or is retracted already.
var (
	errNotYoursToRetract = forbid("only its author, or someone who decides actions for the conversation's opener, retracts a message")
	errRetractedAlready  = conflicts("the message is retracted already")
)

// retractMessage is conversation_retract's execution: its author may, and so
// may whoever oversees the conversation's opener. Retracting the opener's
// latest message withdraws the question: nothing waits for an answer in the
// conversation (withdrawn), and the answer's draft goes with it; an older
// message retracted leaves the draft as it is.
func (c *Core) retractMessage(ec *execCtx, msg *message, why *string) (any, error) {
	if msg.author != ec.member && !oversees(ec.member, msg.conv.opener) {
		return nil, errNotYoursToRetract
	}
	reason, _ := optionalText("reason", why, maxReasonChars)
	if msg.retraction != nil {
		return nil, errRetractedAlready
	}
	msg.retraction = &retraction{at: ec.now, by: ec.member, reason: reason}
	cv := msg.conv
	if c.withdrawn(cv) && cv.latestOpenerMessage() == msg {
		cv.clearDraft()
	}
	ec.emit(&event{typ: "conversation.message_retracted", course: cv.course, subjectType: "conversation", subjectID: &cv.id,
		payload: mustJSON(map[string]any{"conversation_id": cv.id, "message_id": msg.id, "by_member_id": ec.member.id})})
	return map[string]bool{"ok": true}, nil
}

// party is a conversation's opener as the views show it.
type party struct {
	MemberID    string `json:"member_id"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
}

type respondentView struct {
	party
	Role               string     `json:"role"`
	SeatStatus         string     `json:"seat_status"`
	IsDelegateOfOpener bool       `json:"is_delegate_of_opener"`
	OwnerName          *string    `json:"owner_name,omitempty"`
	LastSeenAt         *time.Time `json:"last_seen_at,omitempty"`
	AnswerLevel        string     `json:"answer_level"`
}

// conversationView is a conversation as the read tools show it: never what
// was written.
type conversationView struct {
	ID                    string         `json:"id"`
	Title                 *string        `json:"title,omitempty"`
	Status                string         `json:"status"`
	ClosedReason          *string        `json:"closed_reason,omitempty"`
	State                 string         `json:"state"`
	PendingReplyActionID  *string        `json:"pending_reply_action_id,omitempty"`
	Opener                party          `json:"opener"`
	Respondent            respondentView `json:"respondent"`
	CreatedAt             time.Time      `json:"created_at"`
	LastMessageAt         *time.Time     `json:"last_message_at,omitempty"`
	LastAuthorMemberID    *string        `json:"last_author_member_id,omitempty"`
	LatestOpenerMessageID *string        `json:"latest_opener_message_id,omitempty"`
	LastRetractedAt       *time.Time     `json:"last_retracted_at,omitempty"`
	// Unread is conversation_get's, for the caller who takes part (unread).
	Unread *bool `json:"unread,omitempty"`
}

func partyOf(m *member) party {
	return party{MemberID: m.id, DisplayName: m.actor.name, Kind: m.actor.kind}
}

func seatStatus(m *member, now time.Time) string {
	if m.status != statusRemoved && m.expiresAt != nil && !m.expiresAt.After(now) {
		return "expired"
	}
	return m.status
}

// lastSeen is when an agent last used a token that still works.
func (c *Core) lastSeen(a *actor) *time.Time {
	if a.kind != "agent" {
		return nil
	}
	var seen *time.Time
	for _, cr := range c.tokens {
		if cr.actor == a && !cr.revoked() && cr.lastUsed != nil && (seen == nil || cr.lastUsed.After(*seen)) {
			t := *cr.lastUsed
			seen = &t
		}
	}
	return seen
}

func (c *Core) view(cv *conversation) conversationView {
	now := c.now()
	r := cv.respondent
	v := conversationView{ID: cv.id, Title: cv.title, Status: cv.status, ClosedReason: cv.closedReason,
		Opener: partyOf(cv.opener), CreatedAt: cv.createdAt, LastMessageAt: cv.lastAt,
		Respondent: respondentView{party: partyOf(r), Role: r.role, SeatStatus: seatStatus(r, now),
			IsDelegateOfOpener: r.principal != nil && r.principal == cv.opener, LastSeenAt: c.lastSeen(r.actor),
			AnswerLevel: r.effectivePerms(now)[permConversationAnswer]}}
	if o := r.actor.owner; o != nil {
		name := o.name
		v.Respondent.OwnerName = &name
	}
	if cv.lastAuthor != nil {
		id := cv.lastAuthor.id
		v.LastAuthorMemberID = &id
	}
	latest := cv.latestOpenerMessage()
	withdrawn := c.withdrawn(cv)
	if latest != nil {
		id := latest.id
		v.LatestOpenerMessageID = &id
		// A reply waits only if it answers the opener's newest message,
		// and that message is not withdrawn: approving one to an older
		// message, or to one retracted, can only fail.
		if p := c.pendingAnswer(cv, r, latest.id); p != nil && !withdrawn {
			pid := p.id
			v.PendingReplyActionID = &pid
		}
	}
	for _, m := range cv.messages {
		if m.retraction != nil && (v.LastRetractedAt == nil || !m.retraction.at.Before(*v.LastRetractedAt)) {
			at := m.retraction.at
			v.LastRetractedAt = &at
		}
	}
	switch {
	case cv.status == stateClosed:
		v.State = stateClosed
	case withdrawn:
		// The question is withdrawn, and nothing waits for an answer.
		v.State = stateAnswered
	case v.PendingReplyActionID != nil:
		v.State = stateReplyPendingApproval
	case cv.lastAuthor != nil && cv.lastAuthor == cv.opener:
		v.State = stateAwaitingAnswer
	default:
		v.State = stateAnswered
	}
	return v
}

// unread says, when m takes part in cv, whether the other has written, and
// not retracted, anything since m last marked it read; nil when m does not
// take part (Core's withUnread). Nothing marks a conversation read in the
// fake, which has no conversation_mark_read: m has read nothing.
func unread(cv *conversation, m *member) *bool {
	if m != cv.opener && m != cv.respondent {
		return nil
	}
	u := slices.ContainsFunc(cv.messages, func(msg *message) bool { return msg.author != m && msg.retraction == nil })
	return &u
}

// Who can read what is written in a conversation, as codes: and, said
// last of every conversation, the site's administrators and those of the
// course's department, who may export it for audit (AIShie-Core #51's
// audit_export).
func visibleTo(v conversationView) []string {
	out := []string{"participants", "overseers", "action_record"}
	if !v.Respondent.IsDelegateOfOpener {
		out = append(out, "respondent_answers_others")
	}
	return append(out, "audit_export")
}

type conversationIDIn struct {
	inCourse
	ConversationID uuid.UUID `json:"conversation_id"`
}

// readable finds a conversation the caller may read, and answers one it may
// not as one that does not exist.
func (c *Core) readable(rc *readCtx, id uuid.UUID) (*conversation, error) {
	cv, err := c.findConversation(rc.course, id)
	if err != nil {
		return nil, err
	}
	if !mayRead(rc.member, cv, rc.now) {
		return nil, errNoConversation
	}
	return cv, nil
}

func conversationGet() *impl {
	return define(spec[conversationIDIn]{
		gate: gateConverses,
		resolve: func(c *Core, co *course, in conversationIDIn) (target, error) {
			return conversationTarget(c, co, in.ConversationID)
		},
		query: func(c *Core, rc *readCtx, in conversationIDIn) (any, error) {
			cv, err := c.readable(rc, in.ConversationID)
			if err != nil {
				return nil, err
			}
			v := c.view(cv)
			v.Unread = unread(cv, rc.member)
			return struct {
				conversationView
				VisibleTo []string        `json:"visible_to"`
				Draft     json.RawMessage `json:"draft,omitempty"`
			}{v, visibleTo(v), c.draftFor(rc, cv)}, nil
		},
	})
}

type messagesIn struct {
	inCourse
	ConversationID uuid.UUID `json:"conversation_id"`
	AfterSeq       *int32    `json:"after_seq,omitempty"`
	BeforeSeq      *int32    `json:"before_seq,omitempty"`
	Limit          int       `json:"limit,omitempty"`
	// wait_s waits for a message after after_seq, or a change of the
	// conversation's standing (wait.go): not with before_seq.
	canWait
	SeenState *string `json:"seen_state,omitempty"`
	// SeenDraftVersion, with wait_s: the draft's version as the caller
	// last read it, 0 for none (draft.go).
	SeenDraftVersion *int64 `json:"seen_draft_version,omitempty"`
}

type retractionView struct {
	At         time.Time `json:"at"`
	ByMemberID *string   `json:"by_member_id,omitempty"`
	Reason     *string   `json:"reason,omitempty"`
}

type messageView struct {
	ID                 string          `json:"id"`
	Seq                int32           `json:"seq"`
	AuthorMemberID     string          `json:"author_member_id"`
	InReplyToMessageID *string         `json:"in_reply_to_message_id,omitempty"`
	Body               *string         `json:"body,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	Retracted          *retractionView `json:"retracted,omitempty"`
	// Attachments are withheld with the body once the message is
	// retracted.
	Attachments []attachmentView `json:"attachments,omitempty"`
}

// pageLimit is Core's page size: 50 by default, at most 200.
func pageLimit(limit int) int {
	switch {
	case limit <= 0:
		return 50
	case limit > 200:
		return 200
	}
	return limit
}

func conversationMessages() *impl {
	return define(spec[messagesIn]{
		gate: gateConverses,
		resolve: func(c *Core, co *course, in messagesIn) (target, error) {
			return conversationTarget(c, co, in.ConversationID)
		},
		query: func(c *Core, rc *readCtx, in messagesIn) (any, error) {
			if in.AfterSeq != nil && in.BeforeSeq != nil {
				return nil, invalid("give after_seq or before_seq, not both")
			}
			if in.WaitS > 0 && in.BeforeSeq != nil {
				return nil, invalid("wait_s waits for what comes after after_seq; not with before_seq")
			}
			if in.SeenState != nil && !slices.Contains(conversationStates, *in.SeenState) {
				return nil, invalid("seen_state is one of %s", strings.Join(conversationStates, ", "))
			}
			if in.SeenDraftVersion != nil && *in.SeenDraftVersion < 0 {
				return nil, invalid("seen_draft_version is 0 or more")
			}
			cv, err := c.readable(rc, in.ConversationID)
			if err != nil {
				return nil, err
			}
			limit := pageLimit(in.Limit)
			var rows []*message
			if in.AfterSeq != nil {
				for _, m := range cv.messages {
					if m.seq > *in.AfterSeq && len(rows) < limit {
						rows = append(rows, m)
					}
				}
			} else {
				before := int32(1<<31 - 1)
				if in.BeforeSeq != nil {
					before = *in.BeforeSeq
				}
				for i := len(cv.messages) - 1; i >= 0 && len(rows) < limit; i-- {
					if m := cv.messages[i]; m.seq < before {
						rows = append([]*message{m}, rows...)
					}
				}
			}
			out := struct {
				Conversation conversationView `json:"conversation"`
				Messages     []messageView    `json:"messages"`
				More         bool             `json:"more"`
				Draft        json.RawMessage  `json:"draft,omitempty"`
			}{Messages: make([]messageView, 0, len(rows)), More: len(rows) == limit}
			for _, m := range rows {
				out.Messages = append(out.Messages, viewMessage(m))
			}
			out.Conversation = c.view(cv)
			out.Draft = c.draftFor(rc, cv)
			return out, nil
		},
	})
}

func viewMessage(m *message) messageView {
	v := messageView{ID: m.id, Seq: m.seq, AuthorMemberID: m.author.id, InReplyToMessageID: m.inReplyTo, CreatedAt: m.createdAt}
	if r := m.retraction; r != nil {
		v.Retracted = &retractionView{At: r.at, Reason: r.reason}
		if r.by != nil {
			id := r.by.id
			v.Retracted.ByMemberID = &id
		}
		return v
	}
	body := m.body
	v.Body = &body
	for _, a := range m.attachments {
		v.Attachments = append(v.Attachments, viewAttachment(a))
	}
	return v
}

type inboxIn struct {
	inCourse
	Limit int `json:"limit,omitempty"`
	canWait
}

// waiting reports whether cv waits for m's answer, all but whether its
// opener may still address m: open, addressed to m, the opener wrote last
// and has not retracted it, the opener's seat (and its principal's) live and
// its actor active, and no answer of m's to it waiting for a decision.
func (c *Core) waiting(cv *conversation, m *member, now time.Time) bool {
	o := cv.opener
	if cv.respondent != m || cv.status != "open" || cv.lastAuthor != o || !o.actor.active() || !o.live(now) {
		return false
	}
	if o.principal != nil && !o.principal.live(now) {
		return false
	}
	latest := cv.latestOpenerMessage()
	if latest == nil || latest.retraction != nil {
		return false
	}
	return c.pendingAnswer(cv, m, latest.id) == nil
}

func conversationInbox() *impl {
	return define(spec[inboxIn]{
		gate: gateAnswers,
		resolve: func(_ *Core, _ *course, _ inboxIn) (target, error) {
			return target{typ: "conversation"}, nil
		},
		query: func(c *Core, rc *readCtx, in inboxIn) (any, error) {
			limit := 20
			if in.Limit > 0 {
				limit = min(in.Limit, 100)
			}
			var list []*conversation
			for _, cv := range c.conversationList {
				if cv.course == rc.course && c.waiting(cv, rc.member, rc.now) && refusal(cv.opener, rc.member, rc.now) == "" {
					list = append(list, cv)
				}
			}
			sortWaiting(list)
			out := struct {
				Conversations []conversationView `json:"conversations"`
			}{Conversations: []conversationView{}}
			for _, cv := range list {
				if len(out.Conversations) == limit {
					break
				}
				out.Conversations = append(out.Conversations, c.view(cv))
			}
			return out, nil
		},
	})
}

// sortWaiting orders conversations longest waiting first: by when the
// opener last wrote, then by id, as Core's inbox query orders them.
func sortWaiting(list []*conversation) {
	slices.SortFunc(list, func(x, y *conversation) int {
		if c := x.lastAt.Compare(*y.lastAt); c != 0 {
			return c
		}
		return strings.Compare(x.id, y.id)
	})
}
