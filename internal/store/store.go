// Package store is the runtime's own state: leases, answers written ahead,
// cursors, memory, the seats it has seen, the ledger, and each agent's
// state. It is never Core's database (Core's docs/agent-runtime.md §8.3).
//
// Two implementations keep the same contract, and storetest checks both:
// memstore, for one worker and for tests, and pgstore, for a cluster, whose
// leases are what let several workers share agents.
//
// Every method is scoped to one agent (agent_id) or names it: no call reads
// across agents except SeatsGoneBefore and AgentStates, which the runtime
// uses for housekeeping and the status page.
//
// Where the interfaces leave a choice, storetest's package comment says
// what both stores do. What a caller must know: a write without the ids it
// is keyed on is refused (an attempt needs its seat's member_id too); a
// zero time is the store's now; FinishAttempt keeps a known action id and
// posted message id when the outcome has none; PurgeMember removes the
// seat's notes, attempts and cursors, but not its seat row or the ledger;
// SeatGone of a seat never seen is ErrNotFound; and a ledger row is keyed on
// (agent_id, ID), so every LLMCall and AnswerRecord needs an ID of its own,
// and recording one again counts nothing twice.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Store is all of it.
type Store interface {
	Leases
	Attempts
	Cursors
	Memory
	Seats
	Ledger
	Status
	Close() error
}

// ErrNotFound is a lookup that found nothing.
var ErrNotFound = errors.New("store: not found")

// Leases are named, held by one holder at a time, until they expire.
// Names in use: "agent:{agent_id}", held by the worker that runs the agent,
// and "conv:{agent_id}:{conversation_id}", held while a conversation is
// answered.
type Leases interface {
	// AcquireLease takes name for holder until ttl from now, when it is free,
	// expired, or holder's already (which renews it). It reports whether
	// holder now has it.
	AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error)
	// ReleaseLease frees name if holder has it.
	ReleaseLease(ctx context.Context, name, holder string) error
}

// AttemptState is where an attempt at an answer, or a close, stands.
type AttemptState string

const (
	// AttemptSending is written ahead, before the call. Found so after a
	// crash, its exact bytes are sent again under the same key.
	AttemptSending AttemptState = "sending"
	// AttemptExecuted: posted (review_state may be pending).
	AttemptExecuted AttemptState = "executed"
	// AttemptProposed: waits for a person; event_list says what became of it.
	AttemptProposed AttemptState = "proposed"
	// AttemptFailed, AttemptDenied, AttemptError: Core refused it; nothing
	// was posted. ErrorCode and Reason say why.
	AttemptFailed AttemptState = "failed"
	AttemptDenied AttemptState = "denied"
	AttemptError  AttemptState = "error"
	// AttemptRejected, AttemptCancelled: a proposal a person rejected, or
	// that expired.
	AttemptRejected  AttemptState = "rejected"
	AttemptCancelled AttemptState = "cancelled"
)

// Known reports whether s is one of the states above. A store refuses any
// other.
func (s AttemptState) Known() bool {
	switch s {
	case AttemptSending, AttemptExecuted, AttemptProposed, AttemptFailed,
		AttemptDenied, AttemptError, AttemptRejected, AttemptCancelled:
		return true
	}
	return false
}

// Posted reports whether the attempt put a message in the conversation.
func (s AttemptState) Posted() bool { return s == AttemptExecuted }

// Settled reports whether nothing more will become of the attempt: it
// posted, or it posted nothing and never will.
func (s AttemptState) Settled() bool { return s != AttemptSending && s != AttemptProposed }

// Attempt is one write the runtime sent, or is about to: an answer to one
// message under one attempt number, or closing a conversation.
type Attempt struct {
	// Key is the idempotency key, unique per agent: answer:{c}:{m}:{n} or
	// close:{c}.
	Key            string `json:"key"`
	AgentID        string `json:"agent_id"`
	MemberID       string `json:"member_id"`
	CourseID       string `json:"course_id"`
	ConversationID string `json:"conversation_id"`
	// MessageID is the message answered; empty for a close.
	MessageID string `json:"message_id"`
	// No is the attempt number, from 1; 0 for a close.
	No   int    `json:"no"`
	Tool string `json:"tool"`
	// Args are the exact bytes sent, idempotency_key included.
	Args []byte `json:"args"`
	// Kind is what wrote the body: model, quota (the canned notice), budget
	// (on_budget_text), refusal (on_refusal_text), close.
	Kind  string       `json:"kind"`
	State AttemptState `json:"state"`
	// ActionID is Core's action, once known.
	ActionID string `json:"action_id,omitempty"`
	// PostedMessageID is the message an executed answer made.
	PostedMessageID string    `json:"posted_message_id,omitempty"`
	ErrorCode       string    `json:"error_code,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Outcome is what became of an attempt, for FinishAttempt.
type Outcome struct {
	State           AttemptState
	ActionID        string
	PostedMessageID string
	ErrorCode       string
	Reason          string
}

// Attempts are the answers written ahead (§2.2).
type Attempts interface {
	// PutAttempt writes a's row before a is sent. A key already there is
	// left as it is and ErrExists is returned with the row that is there,
	// whose bytes are what must be sent under that key.
	PutAttempt(ctx context.Context, a Attempt) (*Attempt, error)
	// FinishAttempt records what became of the attempt under key.
	FinishAttempt(ctx context.Context, agentID, key string, o Outcome) error
	// Attempt is the attempt under key, or ErrNotFound.
	Attempt(ctx context.Context, agentID, key string) (*Attempt, error)
	// AttemptsFor lists the attempts at answering one message, by number.
	AttemptsFor(ctx context.Context, agentID, conversationID, messageID string) ([]Attempt, error)
	// AttemptByAction is the attempt Core recorded as actionID, or
	// ErrNotFound.
	AttemptByAction(ctx context.Context, agentID, actionID string) (*Attempt, error)
	// Unsettled lists one seat's attempts still sending or proposed, oldest
	// first.
	Unsettled(ctx context.Context, agentID, memberID string) ([]Attempt, error)
}

// ErrExists is PutAttempt finding the key taken.
var ErrExists = errors.New("store: the key is taken")

// Cursor kinds.
const (
	// CursorEvents is event_list's next_seq, as a decimal string.
	CursorEvents = "events"
	// CursorActions is action_list_mine's after: the last action id seen.
	CursorActions = "actions"
)

// Cursors are the places the runtime has read up to, one per seat and kind.
type Cursors interface {
	// Cursor is the cursor, or "" when there is none yet.
	Cursor(ctx context.Context, agentID, memberID, kind string) (string, error)
	SetCursor(ctx context.Context, agentID, memberID, kind, value string) error
}

// Note kinds.
const (
	// NoteRejected: a person rejected an answer; Text is their reason.
	NoteRejected = "rejected"
	// NoteCancelled: a proposed answer expired or was cancelled.
	NoteCancelled = "cancelled"
	// NoteRetractedOwn: an answer of the agent's was retracted; do not
	// repeat it.
	NoteRetractedOwn = "retracted_own"
	// NoteAnswered: an answer was posted; Text is a short record, never the
	// question.
	NoteAnswered = "answered"
)

// Note is one thing remembered about one conversation. Memory is keyed on
// the seat's member_id, then on conversation_id (§2.5): nothing one person
// wrote is ever read into another's conversation.
type Note struct {
	AgentID        string `json:"agent_id"`
	MemberID       string `json:"member_id"`
	ConversationID string `json:"conversation_id"`
	Kind           string `json:"kind"`
	Text           string `json:"text"`
	// MessageID is the message the note is about, so a retraction can take
	// it out.
	MessageID string    `json:"message_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Memory is per (agent, member, conversation).
type Memory interface {
	AddNote(ctx context.Context, n Note) error
	// Notes are the conversation's newest limit notes, oldest first.
	Notes(ctx context.Context, agentID, memberID, conversationID string, limit int) ([]Note, error)
	// ForgetMessage removes the notes about messageID.
	ForgetMessage(ctx context.Context, agentID, memberID, conversationID, messageID string) error
	// PurgeMember removes everything the store holds in the seat: its notes,
	// its attempts (whose bytes hold the answers' bodies) and its cursors.
	// The seat's row goes with ForgetSeat; the ledger stays.
	PurgeMember(ctx context.Context, agentID, memberID string) error
}

// SeatRef names one seat the runtime has seen.
type SeatRef struct {
	AgentID  string     `json:"agent_id"`
	MemberID string     `json:"member_id"`
	CourseID string     `json:"course_id"`
	SeenAt   time.Time  `json:"seen_at"`
	GoneAt   *time.Time `json:"gone_at,omitempty"`
}

// Seats track which member_ids are current, so that memory is purged a
// while after a seat leaves me_memberships (retention_days_after_removal).
type Seats interface {
	// SeatSeen records the seat as current, clearing any GoneAt.
	SeatSeen(ctx context.Context, agentID, memberID, courseID string, at time.Time) error
	// SeatGone records when the seat was first missed; a later call keeps
	// the first time.
	SeatGone(ctx context.Context, agentID, memberID string, at time.Time) error
	// KnownSeats lists the agent's seats, current and gone.
	KnownSeats(ctx context.Context, agentID string) ([]SeatRef, error)
	// SeatsGoneBefore lists seats of any agent gone before t.
	SeatsGoneBefore(ctx context.Context, t time.Time) ([]SeatRef, error)
	// ForgetSeat removes the seat's row, once its memory is purged.
	ForgetSeat(ctx context.Context, agentID, memberID string) error
}

// LLMCall is one model call, for the ledger (§5.3). It holds ids and
// numbers, never text.
type LLMCall struct {
	ID             string          `json:"id"`
	At             time.Time       `json:"at"`
	TenantID       string          `json:"tenant_id"`
	AgentID        string          `json:"agent_id"`
	CourseID       string          `json:"course_id"`
	MemberID       string          `json:"member_id"`
	ConversationID string          `json:"conversation_id"`
	MessageID      string          `json:"message_id"`
	OpenerMemberID string          `json:"opener_member_id"`
	Adapter        string          `json:"adapter"`
	Provider       string          `json:"provider"`
	Model          string          `json:"model"`
	Stop           string          `json:"stop"`
	RawStop        string          `json:"raw_stop"`
	Input          int64           `json:"input"`
	CacheRead      int64           `json:"cache_read"`
	CacheWrite     int64           `json:"cache_write"`
	Output         int64           `json:"output"`
	Reasoning      int64           `json:"reasoning"`
	Estimated      bool            `json:"estimated"`
	RawUsage       json.RawMessage `json:"raw_usage,omitempty"`
	// PriceVersion names the price table row used; "" when none matched and
	// the cost is unknown.
	PriceVersion string `json:"price_version"`
	// CostPUSD is the cost in pico-dollars (1e-12 USD).
	CostPUSD  int64  `json:"cost_pusd"`
	KeySource string `json:"key_source"`
	LatencyMS int64  `json:"latency_ms"`
}

// Answer outcomes.
const (
	OutcomePosted   = "posted"   // executed
	OutcomeProposed = "proposed" // waits for a person
	OutcomeQuota    = "quota"    // the canned notice, no model call
	OutcomeBudget   = "budget"   // on_budget_text
	OutcomeRefusal  = "refusal"  // on_refusal_text
	OutcomeDropped  = "dropped"  // closed, not found, not addressable, answered already
	OutcomeDenied   = "denied"
	OutcomeFailed   = "failed" // refused by Core for a reason the runtime could not fix
	OutcomeClosed   = "closed" // attempts exhausted; the conversation was closed
	OutcomeSkipped  = "skipped"
	OutcomeError    = "error" // the provider or Core could not be reached; retried later
)

// AnswerRecord is one claimed question's end, for the ledger: a row per
// answer (§5.3). Ids and numbers only.
type AnswerRecord struct {
	ID             string    `json:"id"`
	At             time.Time `json:"at"`
	TenantID       string    `json:"tenant_id"`
	AgentID        string    `json:"agent_id"`
	CourseID       string    `json:"course_id"`
	MemberID       string    `json:"member_id"`
	ConversationID string    `json:"conversation_id"`
	MessageID      string    `json:"message_id"`
	OpenerMemberID string    `json:"opener_member_id"`
	Key            string    `json:"key"`
	Outcome        string    `json:"outcome"`
	// Billable answers count against answer quotas: those a model wrote.
	Billable     bool   `json:"billable"`
	Turns        int    `json:"turns"`
	ToolCalls    int    `json:"tool_calls"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CostPUSD     int64  `json:"cost_pusd"`
	KeySource    string `json:"key_source"`
	PromptHash   string `json:"prompt_hash"`
	LatencyMS    int64  `json:"latency_ms"`
}

// SpendScope filters Spend. Empty fields do not filter; at least one must
// be set.
type SpendScope struct {
	AgentID        string
	TenantID       string
	CourseID       string
	OpenerMemberID string
	KeySource      string
}

// Spend is what a scope has used since a time: the billable answers, and
// the cost of every model call.
type Spend struct {
	Answers  int   `json:"answers"`
	CostPUSD int64 `json:"cost_pusd"`
}

// Ledger is the record of model calls and answers, and the sums quotas are
// checked against.
type Ledger interface {
	RecordLLMCall(ctx context.Context, c LLMCall) error
	RecordAnswer(ctx context.Context, a AnswerRecord) error
	Spend(ctx context.Context, scope SpendScope, since time.Time) (Spend, error)
	// RecentAnswerCosts are the costs of the agent's newest n billable
	// answers, newest first, for the p95 a quota check predicts with.
	RecentAnswerCosts(ctx context.Context, agentID string, n int) ([]int64, error)
}

// Agent states.
const (
	AgentStarting     = "starting"
	AgentRunning      = "running"
	AgentPaused       = "paused"
	AgentUnauthorized = "unauthorized" // Core said 401: the owner must issue a new token
	AgentError        = "error"
	AgentStopped      = "stopped"
)

// AgentState is what the owner's page shows about one agent.
type AgentState struct {
	AgentID   string    `json:"agent_id"`
	State     string    `json:"state"`
	Detail    string    `json:"detail,omitempty"`
	Worker    string    `json:"worker,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Status is each agent's state.
type Status interface {
	SetAgentState(ctx context.Context, s AgentState) error
	AgentStates(ctx context.Context) ([]AgentState, error)
}
