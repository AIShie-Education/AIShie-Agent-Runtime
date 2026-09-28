package core

import (
	"encoding/json"
	"strconv"
)

// Actor is me_get's result. It does not say who owns the agent (§10).
type Actor struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

// KindAgent is an agent's Actor.Kind; a person's is "human".
const KindAgent = "agent"

// Permission levels, as perms maps carry them.
const (
	LevelDenied          = "denied"
	LevelConfirmRequired = "confirm_required"
	LevelPendingReview   = "pending_review"
	LevelAutonomous      = "autonomous"
)

// Membership is one seat, from me_memberships.
type Membership struct {
	MemberID          string            `json:"member_id"`
	CourseID          string            `json:"course_id"`
	Code              string            `json:"code"`
	Section           string            `json:"section"`
	Title             string            `json:"title"`
	CourseStatus      string            `json:"course_status"`
	Role              string            `json:"role"`
	Status            string            `json:"status"`
	ExpiresAt         *string           `json:"expires_at,omitempty"`
	StudentScope      string            `json:"student_scope"`
	AssignmentScope   string            `json:"assignment_scope"`
	PrincipalMemberID *string           `json:"principal_member_id,omitempty"`
	Perms             map[string]string `json:"perms"`
	AnswersCourse     bool              `json:"answers_course"`
}

// Level is the seat's level for perm, denied when absent.
func (m Membership) Level(perm string) string {
	if l, ok := m.Perms[perm]; ok && l != "" {
		return l
	}
	return LevelDenied
}

// Answers reports whether the runtime should answer in this seat: an active
// seat of a course not archived whose conversation_answer is not denied
// (§2.3).
func (m Membership) Answers() bool {
	return m.Status == "active" && m.CourseStatus != "archived" && m.Level("conversation_answer") != LevelDenied
}

// Party is a conversation's opener.
type Party struct {
	MemberID    string `json:"member_id"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
}

// Respondent is a conversation's respondent: the agent, here.
type Respondent struct {
	MemberID           string  `json:"member_id"`
	DisplayName        string  `json:"display_name"`
	Kind               string  `json:"kind"`
	Role               string  `json:"role"`
	SeatStatus         string  `json:"seat_status"`
	IsDelegateOfOpener bool    `json:"is_delegate_of_opener"`
	OwnerName          *string `json:"owner_name,omitempty"`
	LastSeenAt         *string `json:"last_seen_at,omitempty"`
	AnswerLevel        string  `json:"answer_level"`
}

// Conversation states.
const (
	StateAwaitingAnswer       = "awaiting_answer"
	StateReplyPendingApproval = "reply_pending_approval"
	StateAnswered             = "answered"
	StateClosed               = "closed"
)

// Conversation is the view conversation_get, conversation_messages and
// conversation_inbox return.
type Conversation struct {
	ID                    string     `json:"id"`
	Title                 *string    `json:"title,omitempty"`
	Status                string     `json:"status"`
	ClosedReason          *string    `json:"closed_reason,omitempty"`
	State                 string     `json:"state"`
	PendingReplyActionID  *string    `json:"pending_reply_action_id,omitempty"`
	Opener                Party      `json:"opener"`
	Respondent            Respondent `json:"respondent"`
	CreatedAt             string     `json:"created_at"`
	LastMessageAt         *string    `json:"last_message_at,omitempty"`
	LastAuthorMemberID    *string    `json:"last_author_member_id,omitempty"`
	LatestOpenerMessageID *string    `json:"latest_opener_message_id,omitempty"`
	LastRetractedAt       *string    `json:"last_retracted_at,omitempty"`
	VisibleTo             []string   `json:"visible_to,omitempty"`
}

// Retraction is what replaced a retracted message's body.
type Retraction struct {
	At         string  `json:"at"`
	ByMemberID *string `json:"by_member_id,omitempty"`
	Reason     *string `json:"reason,omitempty"`
}

// Message is one message of a conversation. A retracted one has no Body.
type Message struct {
	ID                 string      `json:"id"`
	Seq                int64       `json:"seq"`
	AuthorMemberID     string      `json:"author_member_id"`
	InReplyToMessageID *string     `json:"in_reply_to_message_id,omitempty"`
	Body               *string     `json:"body,omitempty"`
	CreatedAt          string      `json:"created_at"`
	Retracted          *Retraction `json:"retracted,omitempty"`
}

// Inbox is conversation_inbox's result: longest waiting first.
type Inbox struct {
	Conversations []Conversation `json:"conversations"`
}

// Messages is conversation_messages' result, oldest first.
type Messages struct {
	Conversation Conversation `json:"conversation"`
	Messages     []Message    `json:"messages"`
	More         bool         `json:"more"`
}

// Event is one entry of event_list. Events carry ids, never text.
type Event struct {
	Seq             int64           `json:"seq"`
	Type            string          `json:"type"`
	ActionID        *string         `json:"action_id,omitempty"`
	SubjectType     string          `json:"subject_type"`
	SubjectID       *string         `json:"subject_id,omitempty"`
	StudentMemberID *string         `json:"student_member_id,omitempty"`
	AssignmentID    *string         `json:"assignment_id,omitempty"`
	Payload         json.RawMessage `json:"payload"`
	OccurredAt      string          `json:"occurred_at"`
}

// Event types the runtime follows.
const (
	EventActionApproved               = "action.approved"
	EventActionRejected               = "action.rejected"
	EventActionCancelled              = "action.cancelled"
	EventConversationOpened           = "conversation.opened"
	EventConversationMessagePosted    = "conversation.message_posted"
	EventConversationClosed           = "conversation.closed"
	EventConversationMessageRetracted = "conversation.message_retracted"
)

// Events is event_list's result. NextSeq moves on over events the caller
// may not see; keep it as the cursor.
type Events struct {
	Events  []Event `json:"events"`
	NextSeq int64   `json:"next_seq"`
	More    bool    `json:"more"`
}

// Action is one of the agent's own actions, from action_list_mine.
type Action struct {
	ID                 string          `json:"id"`
	ActorID            string          `json:"actor_id"`
	MemberID           *string         `json:"member_id,omitempty"`
	ActionType         string          `json:"action_type"`
	TargetType         string          `json:"target_type"`
	TargetID           *string         `json:"target_id,omitempty"`
	Payload            json.RawMessage `json:"payload,omitempty"`
	AuthzResult        string          `json:"authz_result"`
	Status             string          `json:"status"`
	DecidedByMemberID  *string         `json:"decided_by_member_id,omitempty"`
	DecidedAt          *string         `json:"decided_at,omitempty"`
	ReviewState        string          `json:"review_state"`
	ReviewedByMemberID *string         `json:"reviewed_by_member_id,omitempty"`
	ReviewedAt         *string         `json:"reviewed_at,omitempty"`
	ExecutedAt         *string         `json:"executed_at,omitempty"`
	Result             json.RawMessage `json:"result,omitempty"`
	CreatedAt          string          `json:"created_at"`
}

// DecisionReason is a rejected proposal's reason, from its
// result.decision.reason (§2.3), or "".
func (a Action) DecisionReason() string {
	var r struct {
		Decision struct {
			Reason string `json:"reason"`
		} `json:"decision"`
	}
	if len(a.Result) == 0 || json.Unmarshal(a.Result, &r) != nil {
		return ""
	}
	return r.Decision.Reason
}

// Actions is action_list_mine's result, oldest first.
type Actions struct {
	Actions []Action `json:"actions"`
}

// AnswerArgs are conversation_answer's arguments. Marshalled, they are the
// exact bytes written ahead and sent again after a timeout (§2.2): the field
// order is fixed, so the same values always make the same bytes.
type AnswerArgs struct {
	CourseID           string `json:"course_id"`
	ConversationID     string `json:"conversation_id"`
	InReplyToMessageID string `json:"in_reply_to_message_id"`
	Body               string `json:"body"`
	IdempotencyKey     string `json:"idempotency_key"`
}

// CloseArgs are conversation_close's arguments.
type CloseArgs struct {
	CourseID       string `json:"course_id"`
	ConversationID string `json:"conversation_id"`
	Reason         string `json:"reason,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}

// AnswerKey is an answer's idempotency key (§2.2): Core's MCP instructions
// give this form.
func AnswerKey(conversationID, messageID string, attempt int) string {
	return "answer:" + conversationID + ":" + messageID + ":" + strconv.Itoa(attempt)
}

// CloseKey is the key of closing a conversation.
func CloseKey(conversationID string) string { return "close:" + conversationID }
