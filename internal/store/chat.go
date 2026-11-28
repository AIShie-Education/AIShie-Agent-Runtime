package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Chat conversation and message states are the browser-facing states in the
// AR chat amendment. Chat is owned by AR; none of these rows is a Core
// conversation or message.
const (
	ChatQueued               = "queued"
	ChatAnswering            = "answering"
	ChatAnswered             = "answered"
	ChatReplyPendingApproval = "reply_pending_approval"
	ChatFailed               = "failed"
	ChatClosed               = "closed"

	ChatRoleUser      = "user"
	ChatRoleAssistant = "assistant"

	ChatPosted    = "posted"
	ChatPending   = "pending"
	ChatRetracted = "retracted"
	ChatMsgFailed = "failed"
)

var (
	// ErrChatNotAwaiting means a conversation is closed or its latest user
	// question is already being answered. Asks are serialized per conversation.
	ErrChatNotAwaiting = errors.New("store: chat conversation is not awaiting a question")
	// ErrChatIdempotency means a key was reused for different request bytes.
	ErrChatIdempotency = errors.New("store: chat idempotency key names another request")
	// ErrChatAttachments means an attachment cannot be bound: it is already
	// bound, duplicated, or exceeds the per-message image/text limits.
	ErrChatAttachments = errors.New("store: invalid chat attachment binding")
	// ErrChatWithdraw means the named message is not the latest, live user
	// question of this conversation.
	ErrChatWithdraw = errors.New("store: chat message cannot be withdrawn")
	// ErrChatApproval means an approval is stale, already decided, withdrawn,
	// closed, superseded, or not of the requested operation kind.
	ErrChatApproval = errors.New("store: chat approval is stale or cannot be decided")
)

// ChatConversation is the canonical browser DTO plus AR-internal ownership
// and close fields. AgentID is the Core agent actor id.
type ChatConversation struct {
	ID             string     `json:"id"`
	CourseID       string     `json:"course_id"`
	AgentID        string     `json:"agent_id"`
	Title          string     `json:"title"`
	State          string     `json:"state"`
	Revision       int64      `json:"revision"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	LastActivityAt time.Time  `json:"last_activity_at"`
	CloseReason    string     `json:"close_reason,omitempty"`
	RuntimeAgentID string     `json:"-"`
	OpenerActorID  string     `json:"-"`
	ClosedAt       *time.Time `json:"-"`
}

// ChatSource identifies course material the Runner actually read.
type ChatSource struct {
	DocumentID string `json:"document_id"`
	VersionID  string `json:"version_id,omitempty"`
	FileID     string `json:"file_id,omitempty"`
	Page       int    `json:"page,omitempty"`
	Slide      int    `json:"slide,omitempty"`
	Part       int    `json:"part,omitempty"`
}

// ChatUsage is one answer's token usage.
type ChatUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// ChatAttachment is safe attachment metadata. Bytes and binding/ownership
// fields never appear in browser JSON.
type ChatAttachment struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	MediaType string    `json:"media_type"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at,omitempty"`

	CourseID           string `json:"-"`
	OwnerActorID       string `json:"-"`
	BoundMessage       string `json:"-"`
	Position           int    `json:"-"`
	Bytes              []byte `json:"-"`
	IdempotencyKey     string `json:"-"`
	RequestFingerprint string `json:"-"`
}

// ChatMessageError is deliberately only a stable kind and retry flag. A
// provider's error string is never persisted or returned.
type ChatMessageError struct {
	Kind      string `json:"kind"`
	Retryable bool   `json:"retryable"`
}

// ChatMessage is the canonical browser message DTO.
type ChatMessage struct {
	ID              string            `json:"id"`
	ConversationID  string            `json:"conversation_id"`
	Seq             int64             `json:"seq"`
	Role            string            `json:"role"`
	Status          string            `json:"status"`
	Body            string            `json:"body"`
	CreatedAt       time.Time         `json:"created_at"`
	Attachments     []ChatAttachment  `json:"attachments,omitempty"`
	Sources         []ChatSource      `json:"sources,omitempty"`
	SourcesStated   *bool             `json:"sources_stated,omitempty"`
	Usage           *ChatUsage        `json:"usage,omitempty"`
	FinishReason    string            `json:"finish_reason,omitempty"`
	Error           *ChatMessageError `json:"error,omitempty"`
	RetractedAt     *time.Time        `json:"retracted_at,omitempty"`
	RetractedReason string            `json:"retracted_reason,omitempty"`
}

// ChatStep is one safe, ordered generation step.
type ChatStep struct {
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"`
	State  string `json:"state"`
}

// ChatDraft is the one current draft of a conversation.
type ChatDraft struct {
	Version   int64      `json:"version"`
	Text      string     `json:"text"`
	Steps     []ChatStep `json:"steps"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// ChatSnapshot is returned by reads, creates, asks, withdrawals and SSE.
type ChatSnapshot struct {
	Conversation ChatConversation `json:"conversation"`
	Messages     []ChatMessage    `json:"messages"`
	More         bool             `json:"more"`
	Draft        *ChatDraft       `json:"draft"`
	Submission   *ChatSubmission  `json:"submission,omitempty"`
}

// ChatSubmission reports successful accepted execution or a successful
// proposal. ReviewState is deliberately nullable on the wire.
type ChatSubmission struct {
	Status      string  `json:"status"`
	ApprovalID  string  `json:"approval_id,omitempty"`
	ReviewState *string `json:"review_state"`
}

// ChatApproval is the canonical approval DTO. Mode and GenerationID are
// AR-internal scheduling/review metadata and never appear in browser JSON.
type ChatApproval struct {
	ID             string     `json:"id"`
	CourseID       string     `json:"course_id"`
	ConversationID string     `json:"conversation_id"`
	Subject        string     `json:"subject"`
	MessageID      string     `json:"message_id"`
	State          string     `json:"state"`
	Reason         string     `json:"reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	Mode           string     `json:"-"`
	GenerationID   string     `json:"-"`
}

const (
	ChatSubmissionExecuted   = "executed"
	ChatSubmissionProposed   = "proposed"
	ChatSubjectQuestion      = "question"
	ChatSubjectAnswer        = "answer"
	ChatApprovalPending      = "pending"
	ChatApprovalApproved     = "approved"
	ChatApprovalRejected     = "rejected"
	ChatApprovalChanges      = "changes_requested"
	ChatApprovalReviewed     = "reviewed"
	ChatApprovalCancelled    = "cancelled"
	ChatApprovalModeReview   = "review"
	ChatApprovalModeDecision = "decision"
)

// ChatCreateParams atomically creates a conversation, its first message and
// its generation outbox row, and binds owned attachments.
type ChatCreateParams struct {
	Conversation       ChatConversation
	Message            ChatMessage
	GenerationID       string
	AssistantMsgID     string
	AttachmentIDs      []string
	IdempotencyKey     string
	RequestFingerprint string
	Submission         ChatSubmission
	Approval           *ChatApproval
	AnswerLevel        string
}

// ChatAppendParams atomically appends a user question and generation row.
type ChatAppendParams struct {
	CourseID           string
	ConversationID     string
	ActorID            string
	Message            ChatMessage
	GenerationID       string
	AssistantMsgID     string
	AttachmentIDs      []string
	IdempotencyKey     string
	RequestFingerprint string
	Submission         ChatSubmission
	Approval           *ChatApproval
	AnswerLevel        string
}

// ChatResult is written ahead before an assistant message is finalized.
type ChatResult struct {
	Body          string
	Sources       []ChatSource
	SourcesStated *bool
	Usage         *ChatUsage
	FinishReason  string
	TenantID      string
	AgentID       string
	MemberID      string
	KeySource     string
	Adapter       string
	Provider      string
	Model         string
	Turns         int
	ToolCalls     int
	Writes        WriteCounts
}

// ChatGeneration is recoverable generation/outbox state. Result is non-nil
// once model output has been written ahead; a recovery pass finalizes it
// without making another model call.
type ChatGeneration struct {
	ID                     string
	ConversationID         string
	UserMessageID          string
	AssistantMsgID         string
	State                  string
	Attempt                int
	Holder                 string
	Fence                  int64
	AnswerLevel            string
	RevisionReason         string
	RevisionBody           string
	RevisionOfGenerationID string
	LeaseExpiresAt         time.Time
	Result                 *ChatResult
	Error                  *ChatMessageError
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

const (
	ChatGenerationQueued    = "queued"
	ChatGenerationRunning   = "running"
	ChatGenerationProposed  = "proposed"
	ChatGenerationDone      = "done"
	ChatGenerationFailed    = "failed"
	ChatGenerationCancelled = "cancelled"
)

// ChatDecisionParams atomically decides or reviews an approval. New IDs are
// used only for answer changes_requested, which starts a fresh generation.
type ChatDecisionParams struct {
	ActorID          string
	ApprovalID       string
	Decision         string
	Reason           string
	IdempotencyKey   string
	Fingerprint      string
	Operation        string // decide or review
	ReviewerLevel    string
	ReviewApprovalID string
	NewGenerationID  string
	NewAssistantID   string
}

// ChatStore is AR's own conversation persistence. Every browser-facing
// lookup is scoped by course and opener actor, making unknown and unowned ids
// the same ErrNotFound. Implementations must be safe for concurrent use.
type ChatStore interface {
	CreateChatConversation(context.Context, ChatCreateParams) (ChatSnapshot, bool, error)
	AppendChatMessage(context.Context, ChatAppendParams) (ChatSnapshot, bool, error)
	ChatConversation(ctx context.Context, courseID, conversationID, actorID string) (ChatConversation, error)
	ListChatConversations(ctx context.Context, courseID, actorID, after string, limit int) ([]ChatConversation, string, error)
	ChatSnapshot(ctx context.Context, courseID, conversationID, actorID string, limit int, beforeSeq int64) (ChatSnapshot, error)
	StageChatAttachment(ctx context.Context, a ChatAttachment) (ChatAttachment, error)
	StageChatAttachmentIdempotent(ctx context.Context, a ChatAttachment, key, fingerprint string) (ChatAttachment, bool, error)
	ChatAttachment(ctx context.Context, courseID, attachmentID, actorID string) (ChatAttachment, error)
	WithdrawChatMessage(ctx context.Context, courseID, conversationID, actorID, messageID, reason string) (ChatSnapshot, error)
	CloseChatConversation(ctx context.Context, courseID, conversationID, actorID, reason string) (ChatConversation, error)

	// ChatGeneration reads one outbox row. RecoverChatGenerations lists
	// queued/interrupted work, oldest first. BeginChatGeneration moves one
	// row to running, increments its Runner attempt and moves its
	// conversation to answering atomically.
	ChatGeneration(ctx context.Context, generationID string) (ChatGeneration, error)
	RecoverChatGenerations(ctx context.Context, limit int) ([]ChatGeneration, error)
	BeginChatGeneration(ctx context.Context, generationID, holder string, ttl time.Duration) (ChatGeneration, ChatConversation, bool, error)
	HeartbeatChatGeneration(ctx context.Context, generationID, holder string, fence int64, ttl time.Duration) (bool, error)
	PutChatDraft(ctx context.Context, generationID, holder string, fence int64, text string, steps []ChatStep) (ChatConversation, ChatDraft, bool, error)
	WriteChatResult(ctx context.Context, generationID, holder string, fence int64, result ChatResult) (bool, error)
	FinalizeChatGeneration(ctx context.Context, generationID, holder string, fence int64) (ChatSnapshot, bool, error)
	FinalizeReviewedChatGeneration(ctx context.Context, generationID, holder string, fence int64, approval ChatApproval) (ChatSnapshot, bool, error)
	ProposeChatGeneration(ctx context.Context, generationID, holder string, fence int64, approval ChatApproval) (ChatSnapshot, bool, error)
	FailChatGeneration(ctx context.Context, generationID, holder string, fence int64, kind string, retryable bool) (ChatConversation, bool, error)

	ChatApprovalRecord(ctx context.Context, approvalID string) (ChatApproval, ChatConversation, error)
	ListChatApprovals(ctx context.Context, courseID, state, after string, limit int) ([]ChatApproval, string, error)
	DecideChatApproval(ctx context.Context, p ChatDecisionParams) (ChatApproval, ChatSnapshot, bool, error)
	ReviewChatApproval(ctx context.Context, p ChatDecisionParams) (ChatApproval, ChatSnapshot, error)
	ChatApprovalAttachment(ctx context.Context, approvalID, attachmentID string) (ChatAttachment, error)
}

// EncodeChatCursor makes list pagination stable even when later writes move
// other conversations' activity timestamps.
func EncodeChatCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "\n" + id))
}

// ParseChatCursor decodes a cursor made by EncodeChatCursor.
func ParseChatCursor(cursor string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", errors.New("store: invalid chat cursor")
	}
	at, id, ok := strings.Cut(string(b), "\n")
	if !ok || id == "" {
		return time.Time{}, "", errors.New("store: invalid chat cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil || len(id) > 128 || strings.ContainsAny(id, "\r\n") {
		return time.Time{}, "", errors.New("store: invalid chat cursor")
	}
	return t, id, nil
}

// CheckChatAttachment enforces the amendment's media and byte limits.
func CheckChatAttachment(a ChatAttachment) error {
	if a.ID == "" || a.CourseID == "" || a.OwnerActorID == "" || a.Filename == "" {
		return fmt.Errorf("store: chat attachment id, course, owner and filename required")
	}
	if a.Size != int64(len(a.Bytes)) || a.Size < 0 {
		return fmt.Errorf("store: chat attachment size does not match its bytes")
	}
	switch {
	case strings.HasPrefix(a.MediaType, "image/"):
		if a.Size > 5<<20 {
			return fmt.Errorf("store: chat image exceeds 5 MiB")
		}
	case strings.HasPrefix(a.MediaType, "text/"):
		if a.Size > 32<<10 {
			return fmt.Errorf("store: chat text attachment exceeds 32 KiB")
		}
	default:
		return fmt.Errorf("store: chat attachment media type must be image/* or text/*")
	}
	return nil
}

// ChatResultJSON is the stable JSON representation used by PostgreSQL for a
// write-ahead result. Secrets and provider errors have no representation.
type ChatResultJSON struct {
	Body          string       `json:"body"`
	Sources       []ChatSource `json:"sources,omitempty"`
	SourcesStated *bool        `json:"sources_stated,omitempty"`
	Usage         *ChatUsage   `json:"usage,omitempty"`
	FinishReason  string       `json:"finish_reason,omitempty"`
	TenantID      string       `json:"tenant_id,omitempty"`
	AgentID       string       `json:"agent_id,omitempty"`
	MemberID      string       `json:"member_id,omitempty"`
	KeySource     string       `json:"key_source,omitempty"`
	Adapter       string       `json:"adapter,omitempty"`
	Provider      string       `json:"provider,omitempty"`
	Model         string       `json:"model,omitempty"`
	Turns         int          `json:"turns,omitempty"`
	ToolCalls     int          `json:"tool_calls,omitempty"`
	Writes        WriteCounts  `json:"writes,omitempty"`
}

func (r ChatResult) JSON() json.RawMessage {
	sources := r.Sources
	if r.SourcesStated != nil && *r.SourcesStated && sources == nil {
		sources = []ChatSource{}
	}
	b, _ := json.Marshal(ChatResultJSON{Body: r.Body, Sources: sources, SourcesStated: r.SourcesStated,
		Usage: r.Usage, FinishReason: r.FinishReason, TenantID: r.TenantID, AgentID: r.AgentID, MemberID: r.MemberID,
		KeySource: r.KeySource, Adapter: r.Adapter, Provider: r.Provider, Model: r.Model, Turns: r.Turns, ToolCalls: r.ToolCalls, Writes: r.Writes})
	return b
}

func ParseChatResult(raw []byte) (*ChatResult, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v ChatResultJSON
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if v.SourcesStated != nil && *v.SourcesStated && v.Sources == nil {
		v.Sources = []ChatSource{}
	}
	return &ChatResult{Body: v.Body, Sources: v.Sources, SourcesStated: v.SourcesStated, Usage: v.Usage, FinishReason: v.FinishReason,
		TenantID: v.TenantID, AgentID: v.AgentID, MemberID: v.MemberID, KeySource: v.KeySource, Adapter: v.Adapter, Provider: v.Provider,
		Model: v.Model, Turns: v.Turns, ToolCalls: v.ToolCalls, Writes: v.Writes}, nil
}
