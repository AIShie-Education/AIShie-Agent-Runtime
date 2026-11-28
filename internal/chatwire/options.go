// Package chatwire connects AR-owned chat to the effective agent
// configuration, Core identity/seat state, sealed credentials, the TypeScript
// Runner, and the existing pricing/quota ledger. It deliberately contains no
// inbox, answer, or conversation loop.
//
// Plaintext Core tokens and provider keys exist only in local RunRequest
// values. They are never retained by Wiring, written to the store, or logged.
package chatwire

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/runner"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Store is the small existing-store surface used by chat wiring. Implementations
// of store.Store, including memstore and pgstore, satisfy it directly.
type Store interface {
	AgentToken(ctx context.Context, agentID string) (*store.AgentToken, error)
	KnownSeats(ctx context.Context, agentID string) ([]store.SeatRef, error)
	HostedAgents(ctx context.Context) ([]store.HostedAgent, error)
	ChatAttachment(ctx context.Context, courseID, attachmentID, actorID string) (store.ChatAttachment, error)
	Spend(ctx context.Context, scope store.SpendScope, since time.Time) (store.Spend, error)
	RecentAnswerCosts(ctx context.Context, agentID string, n int) ([]int64, error)
	RecordLLMCall(ctx context.Context, call store.LLMCall) error
}

// SecretResolver is secrets.Resolver's contract. Sealed references should be
// backed by vault.Opener over the same Store.
type SecretResolver interface {
	Resolve(ctx context.Context, ref, baseDir string) (string, error)
}

// CoreIdentity is the result of authenticating an agent Core token. Actor is
// required. When HasMemberships is false, Prepare falls back to the store's
// KnownSeats snapshot; otherwise Memberships is authoritative, including when
// it is empty.
type CoreIdentity struct {
	Actor          core.Actor
	Memberships    []core.Membership
	HasMemberships bool
}

// CoreInspector authenticates a token and reads the agent's current seats.
// Implementations must never include token in an error or log.
type CoreInspector interface {
	Inspect(ctx context.Context, cfg config.Core, token string) (*CoreIdentity, error)
}

// CoreInspectorFunc adapts a function to CoreInspector.
type CoreInspectorFunc func(context.Context, config.Core, string) (*CoreIdentity, error)

func (f CoreInspectorFunc) Inspect(ctx context.Context, cfg config.Core, token string) (*CoreIdentity, error) {
	return f(ctx, cfg, token)
}

// CoreTokenFunc reads an operator-configured agent's currently held token.
// found=false falls back to the store's sealed AgentToken. The reader must
// bind the token to agent.Core.AgentID; Prepare verifies that actor with Core
// again regardless. Wiring never retains the returned plaintext.
type CoreTokenFunc func(ctx context.Context, agent *config.Agent) (token string, found bool, err error)

// Options are the process facilities root must connect. Config should return
// the newest registry-built configuration (a watcher/current-config closure),
// not a one-time YAML snapshot. Prices is the file table; Runtime.Site is
// applied for every lookup so current site rows win.
type Options struct {
	Config  func() *config.Config
	Store   Store
	Secrets SecretResolver

	// Core defaults to MCPInspector. HTTPClient is used by that default for
	// me_get and me_memberships token/seat validation.
	Core       CoreInspector
	HTTPClient *http.Client
	// OperatorToken optionally reads the in-memory token of a non-hosted
	// (YAML/operator) agent when no KMS/sealed store is available. Hosted agents
	// deliberately never use it: their row's token secret is authoritative.
	OperatorToken CoreTokenFunc

	// PriceTable, when set, returns the complete table currently in force and
	// takes precedence over Prices. Use it for a hosting/supervisor closure that
	// follows file reloads and site changes. Otherwise Prices is the file table
	// and the current config's Runtime.Site rows are applied per call.
	PriceTable func() *pricing.Table
	Prices     *pricing.Table
	Location   *time.Location // UTC when nil; Core actor.time_zone takes precedence for quota days.
	Now        func() time.Time
	NewID      func() string
	Log        *slog.Logger

	MaxResultBytes int           // runner.DefaultMaxResultBytes when zero; otherwise Runner's 1024…4 MiB bounds.
	LedgerTimeout  time.Duration // five seconds when <= 0.
}

// Wiring is one stateless facade for chat's three direct-model boundaries.
type Wiring struct {
	opts Options
	core CoreInspector
}

var (
	_ chat.Preparer      = (*Wiring)(nil)
	_ chat.Candidates    = (*Wiring)(nil)
	_ chat.UsageRecorder = (*Wiring)(nil)
)

// New validates process wiring without resolving or retaining any secret.
func New(o Options) (*Wiring, error) {
	if o.Config == nil {
		return nil, errors.New("chatwire: Config current-configuration function required")
	}
	if o.Config() == nil {
		return nil, errors.New("chatwire: current configuration is nil")
	}
	if o.Store == nil {
		return nil, errors.New("chatwire: Store required")
	}
	if o.Secrets == nil {
		return nil, errors.New("chatwire: Secrets resolver required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = uuid.NewString
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.MaxResultBytes < 0 || o.MaxResultBytes > 0 && (o.MaxResultBytes < 1024 || o.MaxResultBytes > 4<<20) {
		return nil, errors.New("chatwire: MaxResultBytes must be between 1024 and 4194304")
	}
	if o.MaxResultBytes == 0 {
		o.MaxResultBytes = runner.DefaultMaxResultBytes
	}
	if o.LedgerTimeout <= 0 {
		o.LedgerTimeout = 5 * time.Second
	}
	w := &Wiring{opts: o, core: o.Core}
	if w.core == nil {
		w.core = MCPInspector{HTTPClient: o.HTTPClient}
	}
	return w, nil
}

func (w *Wiring) now() time.Time { return w.opts.Now() }

func runError(kind string, retryable bool) error {
	return &chat.RunError{Kind: kind, Retryable: retryable}
}

var (
	errGrant        = runError("authorization_grant_incomplete", false)
	errAgent        = runError("agent_configuration_unavailable", false)
	errTokenActor   = runError("core_token_actor_mismatch", false)
	errCore         = runError("core_unavailable", true)
	errStore        = runError("store_unavailable", true)
	errSecret       = runError("secret_unavailable", false)
	errQuota        = runError("quota_exhausted", false)
	errAttachment   = runError("invalid_attachment", false)
	errConversation = runError("invalid_generation_input", false)
)
