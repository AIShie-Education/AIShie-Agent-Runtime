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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
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
	Secrets
	Registry
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
	// A secret a hosted agent still refers to is refused with ErrInUse:
	// the agent's secrets go with it (DeleteHostedAgent), or when it is
	// given new ones (UpdateHostedAgent).
	DeleteSecret(ctx context.Context, id string) error
}

// ErrInUse is a secret that cannot be deleted because a hosted agent
// refers to it.
var ErrInUse = errors.New("store: a hosted agent refers to it")

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
	// OwnerVerified is whether Core said who the owner is (me_get's
	// owner_actor_id), rather than holding the token being the proof.
	OwnerVerified bool `json:"owner_verified"`
	// TenantID is ten_<owner>: its quotas, and every secret's binding.
	TenantID    string `json:"tenant_id"`
	DisplayName string `json:"display_name"`
	// TokenSecretID is the agent's Core token, sealed: a secret of kind
	// core_token of its tenant. TokenHint is what may be shown of it.
	TokenSecretID string `json:"token_secret_id"`
	TokenHint     string `json:"token_hint"`
	// KeySecretID is the owner's own model key, sealed, "" when none is
	// stored: a secret of kind model_key of its tenant, which the registry
	// gives every model section on the owner's key. KeyHint is what may be
	// shown of it.
	KeySecretID string `json:"key_secret_id,omitempty"`
	KeyHint     string `json:"key_hint,omitempty"`
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
	// SetHostedAgentPaused pauses or resumes the agent, whatever its
	// version, and returns it at the next version.
	SetHostedAgentPaused(ctx context.Context, id string, paused bool) (*HostedAgent, error)
	// DeleteHostedAgent destroys the agent, its courses and its secrets, in
	// one transaction; ErrNotFound when it is not there.
	DeleteHostedAgent(ctx context.Context, id string) error

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

// CheckHostedAgent refuses an agent a store must not keep: without its id
// (agt_…), Core actor, tenant or token, or whose settings are not a JSON
// object. It returns the agent with its settings as stored ({} for none).
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
	if !IsSecretID(a.TokenSecretID) {
		bad = append(bad, "token_secret_id")
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
