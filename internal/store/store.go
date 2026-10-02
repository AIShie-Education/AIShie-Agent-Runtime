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
// PurgeAgent removes all an agent has but its ledger;
// SeatGone of a seat never seen is ErrNotFound; and a ledger row is keyed on
// (agent_id, ID), so every LLMCall and AnswerRecord needs an ID of its own,
// and recording one again counts nothing twice.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
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
	Secrets
	Registry
	Reports
	Audit
	OCRTexts
	SearchIndex
	Site
	SitePrices
	Transcription
	AgentTokens
	Close() error
}

// ErrNotFound is a lookup that found nothing.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is a write refused because what it would change has changed
// since the caller read it: a secret rewrapped by another, or a hosted
// agent whose version is no longer the one the caller names (If-Match).
var ErrConflict = errors.New("store: changed since it was read")

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
	// AttemptChangesRequested: a proposal a person sent back for changes;
	// Reason is what they asked to change. The next attempt at the message
	// names its ActionID as the proposal it revises.
	AttemptChangesRequested AttemptState = "changes_requested"
)

// Known reports whether s is one of the states above. A store refuses any
// other.
func (s AttemptState) Known() bool {
	switch s {
	case AttemptSending, AttemptExecuted, AttemptProposed, AttemptFailed,
		AttemptDenied, AttemptError, AttemptRejected, AttemptCancelled, AttemptChangesRequested:
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
// message under one attempt number; or, written by an earlier version,
// closing a conversation, which the runtime does no longer.
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
	// (on_budget_text), refusal (on_refusal_text); close for an earlier
	// version's close.
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

// ProposalsWaiting counts one seat's answers waiting for a person's
// approval: its attempts in state proposed.
func ProposalsWaiting(ctx context.Context, a Attempts, agentID, memberID string) (int, error) {
	atts, err := a.Unsettled(ctx, agentID, memberID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, at := range atts {
		if at.State == AttemptProposed {
			n++
		}
	}
	return n, nil
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
	// NoteChangesRequested: a person sent an answer back for changes; Text
	// is what they asked to change.
	NoteChangesRequested = "changes_requested"
	// NoteCancelled: a proposed answer expired or was cancelled.
	NoteCancelled = "cancelled"
	// NoteRetractedOwn: an answer of the agent's was retracted; do not
	// repeat it.
	NoteRetractedOwn = "retracted_own"
	// NoteAnswered: an answer was posted; Text is a short record, never the
	// question.
	NoteAnswered = "answered"
	// NoteWrote: the model made a write that Core executed or proposed;
	// Text is the tool, its status, the action and the ids it made, never
	// its arguments.
	NoteWrote = "wrote"
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
	// PurgeAgent removes everything the store holds of an agent but its
	// ledger (its model calls and answers, kept by D11): the notes,
	// attempts and cursors of every seat of its, its seats, its state,
	// and its leases, agent:{id} and conv:{id}:*. An agent it holds
	// nothing of is nothing.
	PurgeAgent(ctx context.Context, agentID string) error
}

// SeatRef is one seat the runtime has seen: its ids, and, as
// me_memberships last showed it, what the seat is (a snapshot, so that the
// API can show an agent's seats without its token).
type SeatRef struct {
	AgentID  string `json:"agent_id"`
	MemberID string `json:"member_id"`
	CourseID string `json:"course_id"`
	// CourseCode, CourseTitle and Section name the course.
	CourseCode  string `json:"course_code"`
	CourseTitle string `json:"course_title"`
	Section     string `json:"section"`
	// Status is the seat's: active, paused, …
	Status string `json:"status"`
	// CourseStatus is its course's: active, archived, …; "" for a seat
	// last seen by a release that did not keep it.
	CourseStatus string `json:"course_status"`
	// AnswersCourse is true for a course tutor's seat.
	AnswersCourse bool `json:"answers_course"`
	// PrincipalMemberID is whom a person's own agent answers, "" for none.
	PrincipalMemberID string `json:"principal_member_id,omitempty"`
	// Perms are the seat's levels by permission.
	Perms  map[string]string `json:"perms,omitempty"`
	SeenAt time.Time         `json:"seen_at"`
	GoneAt *time.Time        `json:"gone_at,omitempty"`
}

// Seats track which member_ids are current, so that memory is purged a
// while after a seat leaves me_memberships (retention_days_after_removal),
// and what each seat is.
type Seats interface {
	// SeatSeen records the seat as current, as s says it is, clearing any
	// GoneAt; s.SeenAt is when (zero, the store's now), and s.GoneAt is
	// not read.
	SeatSeen(ctx context.Context, s SeatRef) error
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
	ID string `json:"id"`
	// Kind is what the call was for: an agent's answer (CallAnswer, or
	// ""), or the transcriber's (CallTranscription), which is of no agent,
	// tenant, course or asker.
	Kind           string          `json:"kind,omitempty"`
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
	OutcomeClosed   = "closed" // a close an earlier version wrote ahead, sent again, went through
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
	Billable  bool `json:"billable"`
	Turns     int  `json:"turns"`
	ToolCalls int  `json:"tool_calls"`
	// Writes are the tool calls among ToolCalls that were writes sent to
	// Core, and what Core said of them.
	Writes       WriteCounts `json:"writes"`
	InputTokens  int64       `json:"input_tokens"`
	OutputTokens int64       `json:"output_tokens"`
	CostPUSD     int64       `json:"cost_pusd"`
	KeySource    string      `json:"key_source"`
	PromptHash   string      `json:"prompt_hash"`
	LatencyMS    int64       `json:"latency_ms"`
}

// WriteCounts count the writes models made through their seats' perms
// (docs/design.md §4): those sent to Core, and how many of them Core
// executed, proposed (waiting for a person), denied and failed, a replay
// counted as its stored status. A write sent that Core did not answer, or
// answered with an error, is in Sent alone.
type WriteCounts struct {
	Sent     int `json:"sent"`
	Executed int `json:"executed"`
	Proposed int `json:"proposed"`
	Denied   int `json:"denied"`
	Failed   int `json:"failed"`
}

// Add adds o to w.
func (w *WriteCounts) Add(o WriteCounts) {
	w.Sent += o.Sent
	w.Executed += o.Executed
	w.Proposed += o.Proposed
	w.Denied += o.Denied
	w.Failed += o.Failed
}

// SpendScope filters Spend. Empty fields do not filter; at least one must
// be set. The transcriber's calls (CallTranscription) are of no agent,
// tenant, course or asker, so that only a scope of the key source alone
// counts them: the plan's ceiling across the school's key.
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
	AgentStarting = "starting"
	AgentRunning  = "running"
	AgentPaused   = "paused"
	// AgentUnauthorized: Core said 401 to the token the runtime holds for
	// the agent, which was revoked there (by its owner, an administrator,
	// or a migration): it is not run until it is hosted again (a hosted
	// agent's owner asks for a new token; the operator reloads).
	AgentUnauthorized = "unauthorized"
	// AgentOwnerChanged is a hosted agent whose owner in Core is not the
	// person who hosted it here: it is not run, and its token is revoked.
	AgentOwnerChanged = "owner_changed"
	AgentError        = "error"
	AgentStopped      = "stopped"
)

// Why an agent is in its state, as the API's problem.reason names it (the
// M2 API contract, §6.1): "" when nothing is wrong.
const (
	ReasonTokenRefused         = "token_refused"
	ReasonSettingsRejected     = "settings_rejected"
	ReasonRuntimeMisconfigured = "runtime_misconfigured"
	ReasonOperatorAgent        = "operator_agent"
	ReasonActorInUse           = "actor_in_use"
	ReasonTokenOtherAgent      = "token_other_agent"
	ReasonOwnerChanged         = "owner_changed"
	ReasonCoreTooOld           = "core_too_old"
	ReasonAgentSuspended       = "agent_suspended"
	ReasonFailing              = "failing"
	// ReasonOwnerSuspended: the agent's owner is suspended in Core, and
	// the agent is not hosted until they are reactivated.
	ReasonOwnerSuspended = "owner_suspended"
	// ReasonMCPAgent: the agent is an mcp agent in Core, its owner's own
	// tools', which the runtime cannot host.
	ReasonMCPAgent = "mcp_agent"
	// ReasonAgentNotFound: Core has no agent of the agent's id.
	ReasonAgentNotFound = "agent_not_found"
	// ReasonOfferWithdrawn: the agent is on an offer of the school's plan
	// the school no longer offers (removed, or turned off), and no model
	// of its owner's stands behind it.
	ReasonOfferWithdrawn = "offer_withdrawn"
)

// AgentState is what the owner's page shows about one agent.
type AgentState struct {
	AgentID string `json:"agent_id"`
	State   string `json:"state"`
	// Reason is why, for a state that is a problem: one of the reasons
	// above; "" otherwise.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	Worker string `json:"worker,omitempty"`
	// ConfigVersion is the version of a hosted agent's row the worker had
	// put in force when it wrote the state; 0 for a YAML agent's.
	ConfigVersion int       `json:"config_version"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Status is each agent's state.
type Status interface {
	// SetAgentState records s as the agent's state, in place of the one
	// before, unless that one names a later ConfigVersion: a worker that
	// put an older version of a hosted agent's row in force (a slower
	// watcher, a late write) never writes over the state of a newer one.
	// That is not an error: the newer state stands.
	SetAgentState(ctx context.Context, s AgentState) error
	// AgentState is one agent's state, or ErrNotFound when none is
	// recorded.
	AgentState(ctx context.Context, agentID string) (*AgentState, error)
	AgentStates(ctx context.Context) ([]AgentState, error)
}

// Secret kinds.
const (
	// SecretCoreToken is an agent's Core token.
	SecretCoreToken = "core_token"
	// SecretModelKey is a model provider's key.
	SecretModelKey = "model_key"
)

// secretIDRe is the shape of a secret's id: sec_ and up to 60 letters,
// digits, '_' and '-'. A sealed:// reference names one.
var secretIDRe = regexp.MustCompile(`^sec_[A-Za-z0-9_-]{1,60}$`)

// IsSecretID reports whether id has the shape of a secret's id.
func IsSecretID(id string) bool { return secretIDRe.MatchString(id) }

// Secret is one secret the vault sealed (package vault): its ciphertext,
// under a data key (DEK) of its own, and that key wrapped by the
// key-encryption key KEKID names. Both are bound to the secret's id, tenant
// and kind, so that neither can be moved to another row. Nothing here is
// plaintext: Hint is all of it that may be shown.
type Secret struct {
	// ID is sec_…, see IsSecretID.
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	// Kind is SecretCoreToken or SecretModelKey.
	Kind       string `json:"kind"`
	KEKID      string `json:"kek_id"`
	WrappedDEK []byte `json:"-"`
	Nonce      []byte `json:"-"`
	Ciphertext []byte `json:"-"`
	// Hint is what may be shown of the secret: ais_ and the token's public
	// prefix, or a key's provider prefix and its last four characters.
	Hint string `json:"hint"`
	// CreatedBy is the Core actor who gave the secret, "" when none did.
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// Secrets keep sealed secrets. They are opened only where the worker
// resolves a sealed:// reference, and by `aishie-runtime keys`.
type Secrets interface {
	// PutSecret stores s. An id already taken is ErrExists, and the secret
	// there is left as it is.
	PutSecret(ctx context.Context, s Secret) error
	// Secret is the secret id, or ErrNotFound.
	Secret(ctx context.Context, id string) (*Secret, error)
	// ListSecrets lists up to limit secrets whose ids sort after afterID
	// (bytewise), in that order: every secret, a page at a time.
	ListSecrets(ctx context.Context, afterID string, limit int) ([]Secret, error)
	// RewrapSecret replaces the secret's wrapped data key and kek_id, if
	// the key that wraps it is still fromKEKID: ErrNotFound when the secret
	// is gone, ErrConflict when it was rewrapped since it was read.
	RewrapSecret(ctx context.Context, id, fromKEKID, kekID string, wrapped []byte) error
	// DeleteSecret destroys the secret; one that is not there is nothing.
	// A secret a hosted agent or an offer of the school's plan still
	// refers to is refused with ErrInUse: the agent's secrets go with it
	// (DeleteHostedAgent), or when it is given new ones
	// (UpdateHostedAgent), and an offer's key likewise.
	DeleteSecret(ctx context.Context, id string) error
}

// ErrInUse is a secret that cannot be deleted because a hosted agent, or
// an offer of the school's plan, refers to it.
var ErrInUse = errors.New("store: a hosted agent or an offer refers to it")

// CheckSecret refuses a secret a store must not keep: without its id (in
// the shape IsSecretID gives), tenant, key id or sealed bytes, or of a kind
// not named above.
func CheckSecret(s Secret) error {
	var bad []string
	if !IsSecretID(s.ID) {
		bad = append(bad, "id (sec_…)")
	}
	if s.TenantID == "" {
		bad = append(bad, "tenant_id")
	}
	if s.Kind != SecretCoreToken && s.Kind != SecretModelKey {
		bad = append(bad, "kind ("+SecretCoreToken+" or "+SecretModelKey+")")
	}
	if s.KEKID == "" {
		bad = append(bad, "kek_id")
	}
	if len(s.WrappedDEK) == 0 || len(s.Nonce) == 0 || len(s.Ciphertext) == 0 {
		bad = append(bad, "the sealed bytes")
	}
	if len(bad) > 0 {
		return fmt.Errorf("store: secret: %s required", strings.Join(bad, ", "))
	}
	return nil
}

// Person is someone who has used the runtime's API, as Core's assertion
// named them: kept for the admin's view and the audit, never for
// authorization, which reads the assertion itself.
type Person struct {
	CoreActorID string `json:"core_actor_id"`
	DisplayName string `json:"display_name"`
	// PlatformRole is root, admin or "", as Core has it.
	PlatformRole string    `json:"platform_role"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

// hostedIDRe is the shape of a hosted agent's id: agt_ and a UUID, or up to
// 60 letters, digits, '_' and '-'.
var hostedIDRe = regexp.MustCompile(`^agt_[A-Za-z0-9_-]{1,60}$`)

// IsHostedAgentID reports whether id has the shape of a hosted agent's id.
func IsHostedAgentID(id string) bool { return hostedIDRe.MatchString(id) }

// HostedAgent is one agent a person hosts on the runtime, rather than an
// operator in YAML (docs/design.md §11.2). The registry turns it, with its
// courses, into the same agent document a YAML file holds.
type HostedAgent struct {
	// ID is agt_<uuid>.
	ID string `json:"id"`
	// CoreActorID is the agent's actor in Core: one hosted agent each.
	CoreActorID string `json:"core_actor_id"`
	// OwnerActorID is who owns the agent in Core, "" when Core names no
	// one (an agent an administrator registered).
	OwnerActorID string `json:"owner_actor_id"`
	// OwnerVerified is whether Core said who the owner is (its
	// agent_runtime service, by the agent's id), rather than holding a
	// pasted token being the proof, as it was before hosting by id.
	OwnerVerified bool `json:"owner_verified"`
	// TenantID is ten_<owner>: its quotas, and every secret's binding.
	TenantID    string `json:"tenant_id"`
	DisplayName string `json:"display_name"`
	// TokenSecretID is the agent's Core token, sealed: a secret of kind
	// core_token of its tenant, "" while it holds none (hosted by its id
	// before its model is chosen, paused, or its owner asked for a new
	// one). TokenHint is what may be shown of it.
	TokenSecretID string `json:"token_secret_id,omitempty"`
	TokenHint     string `json:"token_hint,omitempty"`
	// TokenIssued is whether Core issued the token to the runtime, by the
	// agent's id (agent_runtime.issue_token), rather than its owner having
	// pasted it before hosting was by id; TokenCredentialID is Core's id
	// of an issued one. Both are false and "" without a token.
	TokenIssued       bool   `json:"token_issued,omitempty"`
	TokenCredentialID string `json:"token_credential_id,omitempty"`
	// KeySecretID is the owner's own model key, sealed, "" when none is
	// stored: a secret of kind model_key of its tenant, which the registry
	// gives every model section on the owner's key. KeyHint is what may be
	// shown of it, and KeyProvider the provider it was given for, "" when
	// none was named (a key stored by hand): a key is never sent to
	// another provider's host.
	KeySecretID string `json:"key_secret_id,omitempty"`
	KeyHint     string `json:"key_hint,omitempty"`
	KeyProvider string `json:"key_provider,omitempty"`
	// Paused stops every call to Core for the agent.
	Paused bool `json:"paused"`
	// Settings are the rest of the agent document (Core's
	// docs/agent-runtime.md §4) as a JSON object: never its id, name,
	// tenant, pause, Core, or any reference (*_ref), which the registry
	// sets itself. Empty is {}.
	Settings json.RawMessage `json:"settings"`
	// Version moves on with every write: UpdateHostedAgent takes the
	// version the caller read (If-Match).
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HostedCourse is a hosted agent's settings for one course, as a YAML
// agent's courses[course_id] holds them.
type HostedCourse struct {
	AgentID  string `json:"agent_id"`
	CourseID string `json:"course_id"`
	// Settings are courses[course_id]: the agent's settings, less those
	// that are the agent's alone, with enabled and prompt_append_text.
	// Empty is {}.
	Settings json.RawMessage `json:"settings"`
	// UpdatedBy is the Core actor who wrote them.
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Registry is the hosted agents, their courses and the people who use the
// API. Every write to an agent or a course moves the registry's revision
// on (RegistryRev), and pgstore tells every listener (LISTEN
// aishie_registry), so that each worker reloads.
type Registry interface {
	// PutPerson records p, replacing what was known of them. A zero
	// LastSeenAt is the store's now.
	PutPerson(ctx context.Context, p Person) error
	// Person is the person, or ErrNotFound.
	Person(ctx context.Context, coreActorID string) (*Person, error)

	// CreateHostedAgent stores a at version 1 with the secrets given,
	// which must be those it refers to, in one transaction, and returns it
	// as stored. An id or a Core actor already hosted is ErrExists.
	CreateHostedAgent(ctx context.Context, a HostedAgent, secrets ...Secret) (*HostedAgent, error)
	// HostedAgent is the agent id, or ErrNotFound.
	HostedAgent(ctx context.Context, id string) (*HostedAgent, error)
	// HostedAgentByActor is the agent of a Core actor, or ErrNotFound.
	HostedAgentByActor(ctx context.Context, coreActorID string) (*HostedAgent, error)
	// HostedAgentsOwnedBy lists an owner's agents, by id.
	HostedAgentsOwnedBy(ctx context.Context, ownerActorID string) ([]HostedAgent, error)
	// HostedAgents lists every hosted agent, by id.
	HostedAgents(ctx context.Context) ([]HostedAgent, error)
	// UpdateHostedAgent writes a over the agent of its id, if a.Version is
	// still the agent's version, and returns it at the next version:
	// ErrNotFound when it is gone, ErrConflict when it has been written
	// since (If-Match). Its Core actor and tenant do not change. The
	// secrets given are stored in the same transaction, and a secret the
	// agent referred to and no longer does (a token or a key replaced) is
	// destroyed in it.
	UpdateHostedAgent(ctx context.Context, a HostedAgent, secrets ...Secret) (*HostedAgent, error)
	// SetHostedAgentPaused pauses or resumes the agent and returns it at
	// the next version: whatever its version when version is 0, and
	// otherwise only if it is still at version (If-Match), ErrConflict
	// when it has been written since. ErrNotFound when it is gone. Paused,
	// it holds no token: the token's secret is destroyed in the same
	// transaction (the API revokes it in Core), and the agent is issued
	// another when it is resumed.
	SetHostedAgentPaused(ctx context.Context, id string, paused bool, version int) (*HostedAgent, error)
	// DeleteHostedAgent destroys the agent, its courses and its secrets, in
	// one transaction, if it still is as cond says: ErrConflict when it is
	// not, ErrNotFound when it is not there.
	DeleteHostedAgent(ctx context.Context, id string, cond DeleteIf) error

	// PutHostedCourse writes an agent's settings for a course, replacing
	// those it had; ErrNotFound when the agent is not there. A zero
	// UpdatedAt is the store's now.
	PutHostedCourse(ctx context.Context, c HostedCourse) error
	// HostedCourses lists one agent's courses, by course id.
	HostedCourses(ctx context.Context, agentID string) ([]HostedCourse, error)
	// ListHostedCourses lists every hosted agent's courses, by agent, then
	// course.
	ListHostedCourses(ctx context.Context) ([]HostedCourse, error)
	// DeleteHostedCourse removes an agent's settings for a course; ones
	// not there are nothing.
	DeleteHostedCourse(ctx context.Context, agentID, courseID string) error

	// RegistryRev is the registry's revision: it moves on with every write
	// to a hosted agent or course, and with nothing else.
	RegistryRev(ctx context.Context) (int64, error)
}

// AgentToken is the token Core issued the runtime for an agent of the
// operator's configuration (YAML), by its id in Core, sealed: kept so that
// every worker runs the agent with the one token Core holds live for it,
// which only the worker holding the agent's lease is issued.
type AgentToken struct {
	// AgentID is the agent's id in the configuration.
	AgentID string `json:"agent_id"`
	// CoreActorID is the agent in Core (core.agent_id).
	CoreActorID string `json:"core_actor_id"`
	// SecretID is the token, sealed: a secret of kind core_token.
	SecretID string `json:"secret_id"`
	// CredentialID is Core's id of the token, and Hint what may be shown of
	// it.
	CredentialID string `json:"credential_id"`
	Hint         string `json:"hint"`
	// IssuedAt is when, and IssuedBy the worker it was issued to.
	IssuedAt time.Time `json:"issued_at"`
	IssuedBy string    `json:"issued_by"`
}

// AgentTokens keep the tokens of the operator's agents (AgentToken); a
// hosted agent's is its row's (HostedAgent.TokenSecretID).
type AgentTokens interface {
	// AgentToken is the agent's token, or ErrNotFound.
	AgentToken(ctx context.Context, agentID string) (*AgentToken, error)
	// PutAgentToken stores t, with its sealed secret, in place of the
	// agent's token, only while the agent's token is still the secret
	// prev ("" for none): ErrConflict otherwise, and nothing is written.
	// The secret replaced is destroyed in the same transaction.
	PutAgentToken(ctx context.Context, t AgentToken, secret Secret, prev string) error
	// DeleteAgentToken forgets the agent's token, and destroys its
	// secret, while it is still the secret secretID ("" for any): one
	// not there, or another since, is left as it is, which is no error.
	DeleteAgentToken(ctx context.Context, agentID, secretID string) error
}

// CheckAgentToken refuses a token a store must not keep: without its
// agent, its agent in Core, or its secret, a core_token of a secret's
// shape.
func CheckAgentToken(t AgentToken, secret Secret) error {
	var bad []string
	if t.AgentID == "" {
		bad = append(bad, "agent_id")
	}
	if t.CoreActorID == "" {
		bad = append(bad, "core_actor_id")
	}
	if !IsSecretID(t.SecretID) || secret.ID != t.SecretID || secret.Kind != SecretCoreToken {
		bad = append(bad, "its secret, a core_token")
	}
	if len(bad) > 0 {
		return fmt.Errorf("store: agent token: %s required", strings.Join(bad, ", "))
	}
	return CheckSecret(secret)
}

// DeleteIf is what a hosted agent must still be for DeleteHostedAgent to
// delete it; its zero value deletes it as it is. A caller that acted on
// the agent as it read it (revoked its token in Core) deletes it only as
// it read it, and not a row written since in its place.
type DeleteIf struct {
	// TokenSecretID, when set, is the token the agent must still hold: a
	// new token put in since is not deleted unrevoked.
	TokenSecretID string
	// Version, when not 0, is the version it must still be at (If-Match).
	Version int
}

// CheckHostedAgent refuses an agent a store must not keep: without its id
// (agt_…), Core actor or tenant, with a token not of a secret's shape, said
// to be issued with none, or whose settings are not a JSON object. It
// returns the agent with its settings as stored ({} for none).
func CheckHostedAgent(a HostedAgent) (HostedAgent, error) {
	var bad []string
	if !IsHostedAgentID(a.ID) {
		bad = append(bad, "id (agt_…)")
	}
	if a.CoreActorID == "" {
		bad = append(bad, "core_actor_id")
	}
	if a.TenantID == "" {
		bad = append(bad, "tenant_id")
	}
	if a.TokenSecretID != "" && !IsSecretID(a.TokenSecretID) {
		bad = append(bad, "a token_secret_id of a secret's shape")
	}
	if a.TokenSecretID == "" && (a.TokenIssued || a.TokenCredentialID != "" || a.TokenHint != "") {
		bad = append(bad, "a token for token_issued, token_credential_id or token_hint")
	}
	if a.KeySecretID != "" && !IsSecretID(a.KeySecretID) {
		bad = append(bad, "a key_secret_id of a secret's shape")
	}
	settings, err := jsonObject(a.Settings)
	if err != nil {
		bad = append(bad, "settings that are a JSON object")
	}
	if len(bad) > 0 {
		return a, fmt.Errorf("store: hosted agent: %s required", strings.Join(bad, ", "))
	}
	a.Settings = settings
	return a, nil
}

// CheckHostedCourse refuses a course's settings a store must not keep, and
// returns them as stored.
func CheckHostedCourse(c HostedCourse) (HostedCourse, error) {
	if c.AgentID == "" || c.CourseID == "" {
		return c, errors.New("store: hosted course: agent_id and course_id required")
	}
	settings, err := jsonObject(c.Settings)
	if err != nil {
		return c, fmt.Errorf("store: hosted course %s: settings must be a JSON object", c.CourseID)
	}
	c.Settings = settings
	return c, nil
}

// jsonObject is raw if it is a JSON object, compacted, and {} if it is
// empty.
func jsonObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, errors.New("not a JSON object")
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// CheckAgentSecrets holds a hosted agent's secrets to it: the token a
// core_token and the key a model_key, both of its tenant; and every new
// secret given one of those two. have finds a secret already stored.
func CheckAgentSecrets(a HostedAgent, secrets []Secret, have func(id string) (*Secret, error)) error {
	given := map[string]Secret{}
	for _, s := range secrets {
		if s.ID != a.TokenSecretID && (a.KeySecretID == "" || s.ID != a.KeySecretID) {
			return fmt.Errorf("store: hosted agent %s: secret %s is neither its token nor its key", a.ID, s.ID)
		}
		given[s.ID] = s
	}
	for _, ref := range []struct{ id, kind string }{{a.TokenSecretID, SecretCoreToken}, {a.KeySecretID, SecretModelKey}} {
		if ref.id == "" {
			continue
		}
		s, ok := given[ref.id]
		if !ok {
			stored, err := have(ref.id)
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("store: hosted agent %s: its secret %s is not stored", a.ID, ref.id)
			}
			if err != nil {
				return err
			}
			s = *stored
		}
		if s.Kind != ref.kind || s.TenantID != a.TenantID {
			return fmt.Errorf("store: hosted agent %s: secret %s must be a %s of tenant %s", a.ID, ref.id, ref.kind, a.TenantID)
		}
	}
	return nil
}

// AuditEvent is one thing done through the runtime's API, or refused, as
// the audit keeps it (docs/design.md §11.4): ids, hints, providers,
// models and results; never a secret, a name, an email, or text anyone
// wrote.
type AuditEvent struct {
	// ID is the store's, given when the event is recorded.
	ID int64 `json:"id"`
	// At is when; zero is the store's now.
	At time.Time `json:"at"`
	// ActorID is the person, "" for none (a refusal before one was known).
	ActorID string `json:"actor_id"`
	// SessionID is the Core credential the person's assertion names.
	SessionID string `json:"session_id"`
	// IP is the client's address.
	IP string `json:"ip"`
	// Action is what was done, such as agent.connect.
	Action string `json:"action"`
	// TargetType and TargetID are what it was done to, such as
	// hosted_agent and its id.
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	// Outcome is ok, or the reason it was refused.
	Outcome string `json:"outcome"`
	// Detail is a JSON object; empty is {}.
	Detail json.RawMessage `json:"detail"`
}

// Audit is the audit of the runtime's API.
type Audit interface {
	// RecordAudit records e, and returns its id.
	RecordAudit(ctx context.Context, e AuditEvent) (int64, error)
	// AuditEvents lists up to limit events recorded at or after since,
	// oldest first (by time, then id).
	AuditEvents(ctx context.Context, since time.Time, limit int) ([]AuditEvent, error)
	// PruneAudit destroys the events recorded before before, and says how
	// many it destroyed.
	PruneAudit(ctx context.Context, before time.Time) (int64, error)
}

// CheckAuditEvent refuses an event a store must not keep, without its
// action and outcome, or whose detail is not a JSON object, and returns it
// with its detail as stored ({} for none).
func CheckAuditEvent(e AuditEvent) (AuditEvent, error) {
	if e.Action == "" || e.Outcome == "" {
		return e, errors.New("store: audit event: action and outcome required")
	}
	detail, err := jsonObject(e.Detail)
	if err != nil {
		return e, errors.New("store: audit event: detail must be a JSON object")
	}
	e.Detail = detail
	return e, nil
}

// OCRText is what the runtime's OCR recognized of one file (docs/design.md
// §4, Files): the text of a scanned PDF, or of one whose fonts map to
// nothing, or of an image, which recognizing again would cost minutes of
// CPU. It is kept by the file's checksum, which the runtime computes from
// the bytes it fetched, never Core's word for it: one file's text is every
// copy's, in whatever course, and no other file's.
type OCRText struct {
	// Sum is the file's sha256, as "sha256:<hex>".
	Sum string
	// Status is OCRDone or OCRFailed.
	Status string
	// Kind is OCRPDF or OCRImage.
	Kind string
	// Text is what was recognized, each page of a PDF under its heading;
	// "" for a failure, or a file OCR found no text in.
	Text string
	// Pages is the pages recognized, of PagesOf the file has.
	Pages, PagesOf int
	// Sections are where each page's heading begins in Text.
	Sections []OCRSection
	// Notes say what the text leaves out: the pages past the limit, those
	// OCR found nothing on, those it could not read.
	Notes []string
	// Reason is a failure's cause, as a code: timeout, failed, too_large,
	// malformed.
	Reason string
	// Engine names what recognized it: the program, its version, the
	// languages and the resolution.
	Engine string
	// DurationMS is how long recognizing it took.
	DurationMS int64
	// CreatedAt is when it was kept; a zero time is the store's now.
	CreatedAt time.Time
}

// OCRSection is where one page of an OCRText begins: its number, from 1,
// and the byte offset of its heading.
type OCRSection struct {
	N      int `json:"n"`
	Offset int `json:"offset"`
}

// What an OCRText is, and how it ended.
const (
	OCRDone   = "done"
	OCRFailed = "failed"
	OCRPDF    = "pdf"
	OCRImage  = "image"
)

// MaxOCRText bounds an OCRText's text: a page of dense Chinese is a few
// kilobytes, so this is far past the pages OCR reads of a file.
const MaxOCRText = 4 << 20

var ocrSum = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// CheckOCRText refuses what a store must not keep as OCR text: a sum that
// is not a sha256, a status or kind of no known value, a text past
// MaxOCRText or not valid UTF-8, or holding NUL, and sections out of the
// text or out of order.
func CheckOCRText(t OCRText) error {
	switch {
	case !ocrSum.MatchString(t.Sum):
		return errors.New("store: OCR text: the sum is not sha256:<hex>")
	case t.Status != OCRDone && t.Status != OCRFailed:
		return fmt.Errorf("store: OCR text: status %q is not done or failed", t.Status)
	case t.Kind != OCRPDF && t.Kind != OCRImage:
		return fmt.Errorf("store: OCR text: kind %q is not pdf or image", t.Kind)
	case len(t.Text) > MaxOCRText:
		return errors.New("store: OCR text: the text is past MaxOCRText")
	case !utf8.ValidString(t.Text) || strings.ContainsRune(t.Text, 0):
		return errors.New("store: OCR text: the text is not valid UTF-8 without NUL")
	case t.Pages < 0 || t.PagesOf < 0:
		return errors.New("store: OCR text: pages are negative")
	}
	last := 0
	for _, s := range t.Sections {
		if s.Offset < last || s.Offset > len(t.Text) || s.N < 1 {
			return errors.New("store: OCR text: a section is out of the text, or out of order")
		}
		last = s.Offset
	}
	return nil
}

// OCRTexts keep what the runtime's OCR recognized, by the file's checksum,
// for every worker: a file is recognized once, and read again from here.
type OCRTexts interface {
	// OCRText is the text kept under sum; ErrNotFound when there is none.
	OCRText(ctx context.Context, sum string) (OCRText, error)
	// PutOCRText keeps t under t.Sum, in place of what was there.
	PutOCRText(ctx context.Context, t OCRText) error
	// PurgeOCRTexts destroys the texts kept before doneBefore, and the
	// failures kept before failedBefore, so that a file is recognized
	// again when next asked; it says how many it destroyed.
	PurgeOCRTexts(ctx context.Context, doneBefore, failedBefore time.Time) (int64, error)
}

// UsageRow is what an agent used in one course on one UTC day: ids and
// numbers, never text.
type UsageRow struct {
	// Day is the UTC day's start.
	Day      time.Time `json:"day"`
	CourseID string    `json:"course_id"`
	// Answers are the billable answers, those a model wrote; Outcomes are
	// every answer recorded, by its outcome.
	Answers  int            `json:"answers"`
	Outcomes map[string]int `json:"outcomes"`
	// Writes are the writes the answers' models made.
	Writes WriteCounts `json:"writes"`
	// ModelCalls, the tokens and the cost are every model call's.
	ModelCalls       int   `json:"model_calls"`
	InputTokens      int64 `json:"input_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CostPUSD         int64 `json:"cost_pusd"`
}

// AskerUsage is what one asker (a conversation's opener) had of an agent in
// a course: counts, never what anyone wrote.
type AskerUsage struct {
	OpenerMemberID string `json:"opener_member_id"`
	// Answers are the billable answers; Outcomes every answer, by outcome.
	Answers      int            `json:"answers"`
	Outcomes     map[string]int `json:"outcomes"`
	ModelCalls   int            `json:"model_calls"`
	InputTokens  int64          `json:"input_tokens"`
	OutputTokens int64          `json:"output_tokens"`
	CostPUSD     int64          `json:"cost_pusd"`
}

// TenantUsage is what one tenant (a hosted agent's owner, ten_<actor>)
// had on one key source: counts, never what anyone wrote.
type TenantUsage struct {
	TenantID string `json:"tenant_id"`
	// Answers are the billable answers; ModelCalls and CostPUSD every
	// model call's.
	Answers    int   `json:"answers"`
	ModelCalls int   `json:"model_calls"`
	CostPUSD   int64 `json:"cost_pusd"`
}

// How CostReport groups the model calls.
const (
	CostByTotal     = "total"
	CostByDay       = "day"
	CostByTenant    = "tenant"
	CostByAgent     = "agent"
	CostByModel     = "model"
	CostByKeySource = "key_source"
)

// CostQuery is what CostReport sums: the model calls recorded in [Since,
// Until), on KeySource alone when it is not "", grouped by Group, the
// groups whose key sorts after After (bytewise), at most Limit of them.
type CostQuery struct {
	Since, Until time.Time
	Group        string
	KeySource    string
	After        string
	Limit        int
}

// MaxCostRows bounds CostQuery.Limit.
const MaxCostRows = 1000

// CostRow is one group's model calls: its key (the tenant's or the
// agent's id, key_source/provider/model, the key source, the day as
// YYYY-MM-DD, or "" for the total), what it is grouped by, and the
// answers' calls, those no price held (their cost unknown, recorded as
// nothing), the tokens and the cost; and the transcriber's calls apart
// (Transcription), which are grouped under the key CostKeyTranscription
// by agent and CostKeySite by tenant. Ids and numbers, never text.
type CostRow struct {
	Key string `json:"key"`
	// Day is the UTC day's start, by day.
	Day time.Time `json:"day"`
	// TenantID is by tenant, and the agent's by agent.
	TenantID string `json:"tenant_id"`
	AgentID  string `json:"agent_id"`
	// KeySource is by key source and by model; Provider and Model by
	// model.
	KeySource        string `json:"key_source"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	ModelCalls       int    `json:"model_calls"`
	Unpriced         int    `json:"unpriced"`
	InputTokens      int64  `json:"input_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CostPUSD         int64  `json:"cost_pusd"`
	// Transcription are the group's calls of the transcriber's.
	Transcription CostCalls `json:"transcription"`
}

// CostCalls are some model calls summed: how many, those no price held,
// their tokens and their cost.
type CostCalls struct {
	Calls            int   `json:"calls"`
	Unpriced         int   `json:"unpriced"`
	InputTokens      int64 `json:"input_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CostPUSD         int64 `json:"cost_pusd"`
}

// The keys the transcriber's calls are grouped under, by agent and by
// tenant: they are of neither.
const (
	CostKeyTranscription = "transcription"
	CostKeySite          = "site"
)

// CheckCostQuery refuses a cost report's query whose span is empty or
// backwards, whose group is not one of CostReport's, or whose limit is not
// from 1 to MaxCostRows.
func CheckCostQuery(q CostQuery) error {
	switch q.Group {
	case CostByTotal, CostByDay, CostByTenant, CostByAgent, CostByModel, CostByKeySource:
	default:
		return fmt.Errorf("store: a cost report by %q", q.Group)
	}
	if !q.Until.After(q.Since) {
		return errors.New("store: a report's span must end after it begins")
	}
	if q.Limit < 1 || q.Limit > MaxCostRows {
		return fmt.Errorf("store: a cost report of 1 to %d rows", MaxCostRows)
	}
	return nil
}

// Reports sum the ledger for people to read: an agent's use by day and
// course, a course's by asker, and a key's by tenant.
type Reports interface {
	// Usage is agentID's use in [since, until): a row per UTC day and
	// course in which anything was recorded, by day, then course.
	Usage(ctx context.Context, agentID string, since, until time.Time) ([]UsageRow, error)
	// AskerUsage is agentID's use in one course in [since, until): a row
	// per asker, by member id.
	AskerUsage(ctx context.Context, agentID, courseID string, since, until time.Time) ([]AskerUsage, error)
	// TenantUsage is the use on keySource (the school's key, for the
	// school's plan) in [since, until): a row per tenant that recorded
	// anything on it, by tenant id. The transcriber's calls are no
	// tenant's, and are not in it.
	TenantUsage(ctx context.Context, keySource string, since, until time.Time) ([]TenantUsage, error)
	// CostReport sums the model calls as q says, a row per group, by key.
	CostReport(ctx context.Context, q CostQuery) ([]CostRow, error)
}

// CheckKeySpan refuses a report of a key's use whose span is empty or
// backwards, or whose key source is not named.
func CheckKeySpan(keySource string, since, until time.Time) error {
	if keySource == "" {
		return errors.New("store: a report of a key's use needs its key source")
	}
	if !until.After(since) {
		return errors.New("store: a report's span must end after it begins")
	}
	return nil
}

// CheckSpan refuses a report's span that is empty or backwards, or an
// agent not named.
func CheckSpan(agentID string, since, until time.Time) error {
	if agentID == "" {
		return errors.New("store: a report needs its agent")
	}
	if !until.After(since) {
		return errors.New("store: a report's span must end after it begins")
	}
	return nil
}

// UTCDay is the start of t's UTC day, as reports group by.
func UTCDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
