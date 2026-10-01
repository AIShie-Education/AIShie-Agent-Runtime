package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
)

// The site's agent runtime's side of Core (AIShie-Core #52, its
// docs/agent-runtime.md §2.0): every agent is hosted one way, for good, and
// a runtime agent is run by the site's agent runtime and nothing else. The
// runtime is a site service of Core's, agent_runtime, with a credential of
// its own (aissvc_…, CORE_SERVICE_CREDENTIAL), which Core takes at the
// service's four REST routes alone. With it the runtime hosts an agent by
// the agent's id: it asks whether the person signed in to it owns the agent
// (agent_runtime.check_owner), reads the agent as it hosts it
// (agent_runtime.agent), is issued the agent's one token
// (agent_runtime.issue_token), which revokes the one before, and revokes
// it when the hosting ends (agent_runtime.revoke_token). The token is the
// agent's: the runtime runs the agent over MCP with it, as it always has.
// Nobody pastes a token, and nothing is declared: people in the site ask a
// runtime agent while the token the runtime holds for it lives.

// The agent runtime's tools, as the catalogue names them over MCP (Core
// serves none of them over MCP: they are REST's alone).
const (
	ToolRuntimeAgent       = "agent_runtime_agent"
	ToolRuntimeCheckOwner  = "agent_runtime_check_owner"
	ToolRuntimeIssueToken  = "agent_runtime_issue_token"
	ToolRuntimeRevokeToken = "agent_runtime_revoke_token" // #nosec G101 -- a tool's name, not a credential.
)

// How an agent is hosted, for good (RuntimeAgent.Hosting, Actor.Hosting).
const (
	// HostingRuntime: the site's agent runtime runs it, with the one
	// token it was issued, and people in the site ask it.
	HostingRuntime = "runtime"
	// HostingMCP: its owner's own tools reach it over MCP, with tokens of
	// the owner's, and nobody asks it in the site; the runtime never
	// hosts it.
	HostingMCP = "mcp"
)

// Why the runtime may not host an agent now: RuntimeAgent.Reason, and
// agent_runtime.issue_token's refusals (failed_precondition).
const (
	// ReasonNotRuntimeHosted: an mcp agent.
	ReasonNotRuntimeHosted = "not_runtime_hosted"
	// ReasonAgentSuspended: the agent is suspended; it may be hosted again
	// once it is reactivated.
	ReasonAgentSuspended = "agent_suspended"
	// ReasonOwnerSuspended: the agent's owner is suspended.
	ReasonOwnerSuspended = "owner_suspended"
)

// RuntimeTokenLabel is the label the runtime asks its tokens to be issued
// under: what an agent's owner sees listed among its credentials.
const RuntimeTokenLabel = "AIshie agent runtime" // #nosec G101 -- a label, not a credential.

// ServiceTokenPrefix (service.go) begins the agent runtime's credential as
// it begins the transcription service's: aissvc_.

// RuntimeToken is the live token the runtime holds for an agent, as Core
// shows it: never the secret.
type RuntimeToken struct {
	CredentialID string     `json:"credential_id"`
	TokenPrefix  string     `json:"token_prefix,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

// RuntimeAgent is an agent as the runtime needs it to host it
// (agent_runtime.agent, and check_owner's agent): how it is hosted, its
// standing and its owner's, how many seats of its count now, whether the
// runtime may host it now (Hostable, and Reason when not), the token the
// runtime holds for it, and whether people in the site may ask it now.
type RuntimeAgent struct {
	AgentID      string        `json:"agent_id"`
	DisplayName  string        `json:"display_name"`
	Hosting      string        `json:"hosting"`
	Status       string        `json:"status"`
	OwnerActorID string        `json:"owner_actor_id,omitempty"`
	OwnerName    string        `json:"owner_name,omitempty"`
	OwnerStatus  string        `json:"owner_status,omitempty"`
	LiveSeats    int           `json:"live_seats"`
	Hostable     bool          `json:"hostable"`
	Reason       string        `json:"reason,omitempty"`
	RuntimeToken *RuntimeToken `json:"runtime_token,omitempty"`
	SiteChat     bool          `json:"site_chat"`
}

// IssuedToken is agent_runtime.issue_token's result: the agent's token,
// which Core shows once, and the credentials it revoked.
type IssuedToken struct {
	AgentID      string   `json:"agent_id"`
	CredentialID string   `json:"credential_id"`
	Token        string   `json:"token"`
	TokenPrefix  string   `json:"token_prefix"`
	Replaced     []string `json:"replaced,omitempty"`
}

// HasRuntimeService reports whether cat offers the agent runtime's
// service: a Core since AIShie-Core #52.
func HasRuntimeService(cat *Catalogue) bool {
	if cat == nil {
		return false
	}
	for _, name := range []string{ToolRuntimeAgent, ToolRuntimeCheckOwner, ToolRuntimeIssueToken, ToolRuntimeRevokeToken} {
		if _, ok := cat.Tool(name); !ok {
			return false
		}
	}
	return true
}

// RuntimeService is the agent runtime's typed client: its four calls, over
// a Caller that carries the service's credential (RuntimeCaller, a
// RESTCaller: the service's tools are REST's alone).
type RuntimeService struct {
	c Caller
}

// NewRuntimeService is the agent runtime's client over c.
func NewRuntimeService(c Caller) *RuntimeService { return &RuntimeService{c: c} }

// Agent is the agent agentID as the runtime hosts it; a *ServiceError of
// code not_found (IsNotFound) for an id that is no agent's.
func (s *RuntimeService) Agent(ctx context.Context, agentID string) (*RuntimeAgent, error) {
	var a RuntimeAgent
	err := serviceCall(ctx, s.c, ToolRuntimeAgent, map[string]any{"agent_id": agentID}, &a)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CheckOwner says whether the person actorID (an assertion's sub) owns the
// agent agentID, and, when they do, the agent as Agent gives it. Of
// someone else's agent, nobody's, an id that is no agent's or nobody's, it
// is false, and nil.
func (s *RuntimeService) CheckOwner(ctx context.Context, actorID, agentID string) (bool, *RuntimeAgent, error) {
	var r struct {
		Owns  bool          `json:"owns"`
		Agent *RuntimeAgent `json:"agent"`
	}
	if err := serviceCall(ctx, s.c, ToolRuntimeCheckOwner, map[string]any{"actor_id": actorID, "agent_id": agentID}, &r); err != nil {
		return false, nil, err
	}
	if !r.Owns || r.Agent == nil {
		return false, nil, nil
	}
	return true, r.Agent, nil
}

// issueAttempts bounds IssueToken's calls when Core answers one with the
// replay of an earlier, which carries no token.
const issueAttempts = 3

// errNoToken is an issue_token Core answered without its token: the replay
// of a call it carried out before, whose token it showed once to a call
// that did not come back.
var errNoToken = errors.New("core: agent_runtime_issue_token: the answer carries no token: it replays one the runtime never received")

// IssueToken is the agent agentID's one token issued to the runtime,
// labelled label (RuntimeTokenLabel when ""), which revokes the one
// before. Every call is under a key of its own, never the key of a call
// that went before: Core shows the token once, and a replay of the same
// key comes back without it, so a call that timed out is made again under
// a new key, which revokes the token never received (and a Retrying
// beneath it that sent the same key again is answered the replay, which is
// made again here the same way). A refusal is a *ServiceError: not_found,
// or failed_precondition with ReasonNotRuntimeHosted, ReasonAgentSuspended
// or ReasonOwnerSuspended.
func (s *RuntimeService) IssueToken(ctx context.Context, agentID, label string) (*IssuedToken, error) {
	if label == "" {
		label = RuntimeTokenLabel
	}
	for range issueAttempts {
		var t IssuedToken
		args := map[string]any{"agent_id": agentID, "label": label, idempotencyKeyArg: "issue:" + agentID + ":" + uuid.NewString()}
		if err := serviceCall(ctx, s.c, ToolRuntimeIssueToken, args, &t); err != nil {
			return nil, err
		}
		if t.Token != "" {
			return &t, nil
		}
	}
	return nil, errNoToken
}

// RevokeToken revokes the token the runtime holds for the agent agentID,
// and returns the credentials Core revoked: none, when it held none, which
// is no error. People in the site then ask the agent nothing until the
// runtime is issued another.
func (s *RuntimeService) RevokeToken(ctx context.Context, agentID string) ([]string, error) {
	var r struct {
		Revoked []string `json:"revoked"`
	}
	args := map[string]any{"agent_id": agentID, idempotencyKeyArg: "revoke:" + agentID + ":" + uuid.NewString()}
	if err := serviceCall(ctx, s.c, ToolRuntimeRevokeToken, args, &r); err != nil {
		return nil, err
	}
	return r.Revoked, nil
}

// CredentialRefused reports whether err is Core refusing the runtime's own
// credential: a 401 (missing, revoked, expired), or a credential that is
// not the agent runtime's (service_only: an agent's or a person's token;
// not_for_services: another service's). The runtime's operator replaces it.
func CredentialRefused(err error) bool {
	return errors.Is(err, ErrUnauthenticated) || IsReason(err, ReasonServiceOnly) || IsReason(err, ReasonNotForServices)
}

// RuntimeOptions configure RuntimeCaller.
type RuntimeOptions struct {
	// BaseURL is Core's base, such as https://lms.example.edu.
	BaseURL string
	// Credential is the agent runtime's credential (aissvc_…), resolved
	// at each call: a new one put where CORE_SERVICE_CREDENTIAL points is
	// taken at the next call, with no restart. Required.
	Credential func(ctx context.Context) (string, error)
	// HTTPClient carries the calls; one with DefaultTimeout when nil.
	HTTPClient *http.Client
	// Bucket paces every call made through it: the service's calls count
	// against Core's per-actor limit, every worker's together (600 a
	// minute, bursts of 100, by default), so that hosting every agent at
	// once (the first start after an upgrade) never runs past it. Nil
	// paces nothing.
	Bucket *ratelimit.Bucket
	// Retry is how a call Core did not answer, or answered 429, is sent
	// again (Retrying); its zero value is the handout's.
	Retry RetryOptions
	// Once sends each call once, never again: the API's, which a person
	// waits for, and which they make again themselves.
	Once bool
	// Catalogue is Core's catalogue, whose routes the calls go to; when
	// nil it is fetched at the first call, kept, and fetched again after a
	// failure.
	Catalogue *Catalogue
}

// RuntimeCaller is a Caller of the agent runtime's tools at o.BaseURL: each
// call resolves the credential, waits for a token of o.Bucket, and goes to
// its REST route, sent again as Retrying does.
func RuntimeCaller(o RuntimeOptions) Caller {
	return &runtimeCaller{o: o}
}

type runtimeCaller struct {
	o RuntimeOptions

	mu  sync.Mutex
	cat *Catalogue
}

// catalogue is Core's catalogue: the one given, or fetched once.
func (r *runtimeCaller) catalogue(ctx context.Context) (*Catalogue, error) {
	if r.o.Catalogue != nil {
		return r.o.Catalogue, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cat != nil {
		return r.cat, nil
	}
	hc := r.o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	cat, err := FetchCatalogue(ctx, hc, r.o.BaseURL)
	if err != nil {
		return nil, err
	}
	r.cat = cat
	return cat, nil
}

func (r *runtimeCaller) Call(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error) {
	if r.o.Credential == nil {
		return nil, &CredentialError{Tool: tool, Err: errors.New("none is configured")}
	}
	cat, err := r.catalogue(ctx)
	if err != nil {
		return nil, fmt.Errorf("core: %s: Core's catalogue: %w", tool, err)
	}
	if !HasRuntimeService(cat) {
		return nil, &ServiceError{Tool: tool, Status: StatusError, Code: CodeNotFound, Reason: ReasonCoreTooOld,
			Message: "Core has no agent_runtime service: it is older than AIShie-Core #52"}
	}
	token, err := r.o.Credential(ctx)
	if err != nil {
		return nil, &CredentialError{Tool: tool, Err: err}
	}
	if !strings.HasPrefix(token, ServiceTokenPrefix) {
		return nil, &CredentialError{Tool: tool, Err: errors.New("it is not a site service's credential (aissvc_…)")}
	}
	var c Caller = NewRESTCaller(RESTOptions{BaseURL: r.o.BaseURL, Token: token, HTTPClient: r.o.HTTPClient, Catalogue: cat})
	c = Limited(c, r.o.Bucket)
	if !r.o.Once {
		c = NewRetrying(c, r.o.Retry)
	}
	return c.Call(ctx, tool, args)
}

// CredentialError is a call of the agent runtime's tools not made for want
// of the runtime's own credential: none configured, its secret not read,
// or not a site service's token. It never holds the credential.
type CredentialError struct {
	Tool string
	Err  error
}

func (e *CredentialError) Error() string {
	return "core: " + e.Tool + ": the agent runtime's credential (CORE_SERVICE_CREDENTIAL): " + e.Err.Error()
}

func (e *CredentialError) Unwrap() error { return e.Err }

// ReasonCoreTooOld is what a call of the agent runtime's tools says of a
// Core that has none (RuntimeCaller): one from before AIShie-Core #52.
const ReasonCoreTooOld = "core_too_old"

// serviceCall makes one of a site service's calls with c: executed decodes
// into out, and anything else is a *ServiceError (a 401,
// ErrUnauthenticated; a 429, a *RateLimitedError; a 5xx or the network, a
// *TransientError).
func serviceCall(ctx context.Context, c Caller, tool string, args, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("core: %s: %w", tool, err)
	}
	env, err := c.Call(ctx, tool, raw)
	if err != nil {
		return err
	}
	if env.Status != StatusExecuted {
		se := &ServiceError{Tool: tool, Status: env.Status, Code: env.Code(), Reason: env.Reason()}
		if env.Error != nil {
			se.Message = env.Error.Message
		}
		return se
	}
	if err := env.Decode(out); err != nil {
		return &ProtocolError{Message: fmt.Sprintf("%s: the result does not decode: %v", tool, err)}
	}
	return nil
}
