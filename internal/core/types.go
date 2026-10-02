package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// Actor is me_get's result.
type Actor struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	// OwnerActorID is, for an agent a person owns, that person's actor id
	// (§2.3, §10.1): given when the agent is registered, and never changed.
	// It is "" for a person, and for an agent nobody owns.
	OwnerActorID string `json:"owner_actor_id,omitempty"`
	// Hosting is how an agent is hosted, for good: HostingRuntime or
	// HostingMCP; "" for a person, and on a Core from before it said so.
	Hosting string `json:"hosting,omitempty"`
}

// KindAgent is an agent's Actor.Kind; a person's is "human".
const KindAgent = "agent"

// StatusActive is an actor's Status while it may act; a suspended one's is
// "suspended".
const StatusActive = "active"

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

// Message is one message of a conversation. A retracted one has no Body,
// and lists no Attachments: Core withholds its files with its text.
type Message struct {
	ID                 string      `json:"id"`
	Seq                int64       `json:"seq"`
	AuthorMemberID     string      `json:"author_member_id"`
	InReplyToMessageID *string     `json:"in_reply_to_message_id,omitempty"`
	Body               *string     `json:"body,omitempty"`
	CreatedAt          string      `json:"created_at"`
	Retracted          *Retraction `json:"retracted,omitempty"`
	// Attachments are the files the message carries, in order
	// (attachments.go).
	Attachments []Attachment `json:"attachments,omitempty"`
	// Sources are what an answer said it relied on, as the reader may
	// read them now (§2.10): empty for an answer that relied on none, nil
	// for one that did not say.
	Sources []SourceView `json:"sources,omitzero"`
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
	EventActionChangesRequested       = "action.changes_requested" // sent back for changes, over as a rejection is
	EventActionCancelled              = "action.cancelled"
	EventConversationOpened           = "conversation.opened"
	EventConversationMessagePosted    = "conversation.message_posted"
	EventConversationClosed           = "conversation.closed"
	EventConversationMessageRetracted = "conversation.message_retracted"
	// EventDocumentPurged and EventDocumentPurgedUnreleased say a
	// document (its subject), or one version of it (its payload's
	// version_id), was purged: its text and files are gone for good. A
	// seat that reads drafts sees the first, one that writes assignments
	// the second, of instructions or a rubric not yet released.
	EventDocumentPurged           = "document.purged"
	EventDocumentPurgedUnreleased = "document.purged_unreleased"
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
	RevisesActionID    *string         `json:"revises_action_id,omitempty"`
}

// DecisionReason is a rejected proposal's reason, or what a proposal sent
// back for changes asks to change, from its result.decision.reason
// (§2.3), or "".
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
	// Sources are the course materials the answer relied on, in the order
	// it read them (§2.10): empty, and sent as [], when it relied on none;
	// nil, and left out, when the runtime cannot say.
	Sources        []Source `json:"sources,omitzero"`
	IdempotencyKey string   `json:"idempotency_key"`
	// Revises is, for an answer that proposes again one a person sent
	// back for changes, that proposal's action id; left out otherwise,
	// so that an answer revising nothing is the bytes it always was.
	// Over REST it goes in the Revises header.
	Revises string `json:"revises,omitempty"`
}

// MaxSources is the most sources one answer names, as Core takes them.
const MaxSources = 20

// Source is one course material an answer relied on, as
// conversation_answer takes it (§2.10): a version of a material,
// instructions or a rubric, and, where the answer relied on one file of
// it, that file, with a page or a slide of it and the part of its text
// version as Core numbers them, each from 1, where known.
type Source struct {
	DocumentID string `json:"document_id"`
	VersionID  string `json:"version_id"`
	FileID     string `json:"file_id,omitempty"`
	Page       int    `json:"page,omitempty"`
	Slide      int    `json:"slide,omitempty"`
	Part       int    `json:"part,omitempty"`
}

// SourceView is a source of an answer as one reader of
// conversation_messages is shown it: whole where they may open its
// version; Restricted, and nothing else, where they may not open its
// document; OtherVersion, with its document alone, where they may open the
// document but not that version.
type SourceView struct {
	Restricted   bool    `json:"restricted,omitempty"`
	OtherVersion bool    `json:"other_version,omitempty"`
	DocumentID   *string `json:"document_id,omitempty"`
	Kind         *string `json:"kind,omitempty"`
	Title        *string `json:"title,omitempty"`
	VersionID    *string `json:"version_id,omitempty"`
	Seq          *int    `json:"seq,omitempty"`
	Published    *bool   `json:"published,omitempty"`
	FileID       *string `json:"file_id,omitempty"`
	Filename     *string `json:"filename,omitempty"`
	Page         *int    `json:"page,omitempty"`
	Slide        *int    `json:"slide,omitempty"`
	Part         *int    `json:"part,omitempty"`
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

// MaxKeyChars is the most characters Core takes in an idempotency key.
const MaxKeyChars = 200

// ToolKey is the idempotency key of the nth write (from 1) the model made
// in attempt at answering message messageID in conversationID:
// tool:{conversation}:{message}:{attempt}:{n}. The same attempt tried
// again keys its writes the same, so that Core replays what it did then;
// a new attempt's are new. A key that would be longer than Core takes is
// tool: and the sha256 of what it would have been, in hex.
func ToolKey(conversationID, messageID string, attempt, n int) string {
	key := "tool:" + conversationID + ":" + messageID + ":" + strconv.Itoa(attempt) + ":" + strconv.Itoa(n)
	if utf8.RuneCountInString(key) <= MaxKeyChars {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "tool:" + hex.EncodeToString(sum[:])
}

// CloseKey is the key of closing a conversation.
func CloseKey(conversationID string) string { return "close:" + conversationID }
