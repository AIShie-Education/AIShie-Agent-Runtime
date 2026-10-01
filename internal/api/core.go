package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// The API asks Core about an agent at CORE_BASE_URL and nowhere else, with
// the runtime's own credential (Options.Runtime, Core's agent_runtime
// service): whether the person signed in owns it and may host it here
// (check_owner), and, as its hosting ends, to revoke the token the runtime
// was issued for it (revoke_token). The API is never issued a token: the
// worker that runs the agent is (package worker).

// Deadlines of what a request asks of Core.
const (
	// askTimeout bounds asking Core about an agent: the service's
	// catalogue and check_owner.
	askTimeout = 20 * time.Second
	// revokeTimeout bounds revoking an agent's token in Core.
	revokeTimeout = 15 * time.Second
	// catalogueTTL is how long Core's catalogue is kept.
	catalogueTTL = 10 * time.Minute
)

// catalogueCache is Core's catalogue, fetched once every catalogueTTL:
// what the transcription service's client calls Core by.
type catalogueCache struct {
	mu  sync.Mutex
	cat *core.Catalogue
	at  time.Time
}

// catalogue is Core's catalogue, fetched when it is not kept or is older
// than catalogueTTL, one fetch at a time.
func (s *Server) catalogue(ctx context.Context) (*core.Catalogue, error) {
	s.cats.mu.Lock()
	defer s.cats.mu.Unlock()
	now := s.o.Now()
	if s.cats.cat != nil && now.Sub(s.cats.at) < catalogueTTL {
		return s.cats.cat, nil
	}
	cat, err := core.FetchCatalogue(ctx, s.coreHTTP, s.o.CoreBaseURL)
	if err != nil {
		return nil, err
	}
	s.cats.cat, s.cats.at = cat, now
	return cat, nil
}

// Refusals of a request about an agent in Core.
var (
	errNotYours = Error{Code: CodeNotFound, Reason: ReasonAgentNotFound,
		Message: "no such agent of yours in Core: it is not an agent, or someone else's, or nobody's"}
	errCoreUnavailable = Error{Code: CodeUnavailable, Reason: ReasonCoreUnavailable,
		Message: "Core could not be reached to ask about the agent"}
	errRuntimeMisconfigured = Error{Code: CodeUnavailable, Reason: ReasonRuntimeMisconfigured,
		Message: "the runtime has no credential of its own in Core that Core takes (CORE_SERVICE_CREDENTIAL): its operator gives it one"}
	errCoreTooOld = Error{Code: CodeFailedPrecondition, Reason: ReasonCoreTooOld,
		Message: "this Core hosts no agent by its id (it has no agent_runtime service): it must be upgraded"}
)

// hostRefusals are why Core says the runtime may not host an agent
// (core.RuntimeAgent.Reason), as the API answers them.
var hostRefusals = map[string]Error{
	core.ReasonNotRuntimeHosted: {Code: CodeFailedPrecondition, Reason: ReasonMCPAgent,
		Message: "the agent is an mcp agent: its owner's own tools reach it over MCP, and it cannot be hosted here"},
	core.ReasonAgentSuspended: {Code: CodeFailedPrecondition, Reason: ReasonAgentSuspended, Message: "the agent is suspended in Core"},
	core.ReasonOwnerSuspended: {Code: CodeFailedPrecondition, Reason: ReasonOwnerSuspended, Message: "the agent's owner is suspended in Core"},
}

// hostReason is the API's reason for Core's reason an agent may not be
// hosted (mcp_agent for not_runtime_hosted), "" for none.
func hostReason(coreReason string) string {
	if coreReason == "" {
		return ""
	}
	if e, ok := hostRefusals[coreReason]; ok {
		return e.Reason
	}
	return coreReason
}

// hostRefusal is the refusal to host view, as the API answers it.
func hostRefusal(view *core.RuntimeAgent) Error {
	if e, ok := hostRefusals[view.Reason]; ok {
		return e
	}
	return Error{Code: CodeFailedPrecondition, Reason: view.Reason, Message: "Core says the agent may not be hosted now"}
}

// agentIDOf reads the agent's id in Core a request names (agent_id, a
// UUID), in lower case as Core writes it.
func agentIDOf(id string) (string, *Error) {
	if id == "" {
		return "", &Error{Code: CodeInvalidArgument, Reason: ReasonMissingField, Message: "agent_id is the agent's id in Core",
			Details: map[string]any{"field": "/agent_id"}}
	}
	u, err := uuid.Parse(id)
	if err != nil || len(id) != 36 {
		return "", &Error{Code: CodeInvalidArgument, Reason: ReasonInvalidField, Message: "agent_id is not a UUID",
			Details: map[string]any{"field": "/agent_id"}}
	}
	return u.String(), nil
}

// ownedAgent asks Core, with the runtime's credential, whether the caller
// owns the agent agentID, and what it is: the agent as Core hosts it, or
// the refusal to answer (404 for one not theirs, as for no agent at all).
func (s *Server) ownedAgent(ctx context.Context, c *Caller, agentID string) (*core.RuntimeAgent, *Error) {
	if s.o.Runtime == nil {
		s.o.Log.Error("the API cannot ask Core about an agent: the runtime has no credential of its own (CORE_SERVICE_CREDENTIAL)")
		e := errRuntimeMisconfigured
		return nil, &e
	}
	owns, view, err := s.o.Runtime.CheckOwner(core.WithPriority(ctx, core.PriorityAnswer), c.ActorID, agentID)
	switch {
	case err != nil:
		e := s.serviceRefusal("agent_runtime.check_owner", err)
		return nil, &e
	case !owns || view == nil:
		e := errNotYours
		return nil, &e
	}
	return view, nil
}

// serviceRefusal is a call of the runtime's service that failed, as the
// API answers it: the runtime's credential missing or refused, Core too
// old, or Core not reached.
func (s *Server) serviceRefusal(tool string, err error) Error {
	var ce *core.CredentialError
	switch {
	case errors.As(err, &ce), core.CredentialRefused(err):
		s.o.Log.Error("Core refused the runtime's own credential, or the runtime has none (CORE_SERVICE_CREDENTIAL)", "tool", tool, "err", err)
		return errRuntimeMisconfigured
	case core.IsReason(err, core.ReasonCoreTooOld):
		return errCoreTooOld
	}
	s.o.Log.Warn("Core could not be reached about an agent", "tool", tool, "err", err)
	return errCoreUnavailable
}

// What became of revoking an agent's token in Core as its hosting ended
// (Revocation.Outcome).
const (
	// RevocationRevoked: Core revoked the token the runtime held.
	RevocationRevoked = "revoked"
	// RevocationNone: Core held no live token of the runtime's for the
	// agent.
	RevocationNone = "none"
	// RevocationFailed: it was not revoked (Problem says why); the agent
	// is not run all the same, and its owner may pause it again, or
	// revoke the token in Core.
	RevocationFailed = "failed"
	// RevocationNotAttempted: the runtime did not ask Core (Problem says
	// why).
	RevocationNotAttempted = "not_attempted"
)

// Revocation is what became of the token the runtime held for an agent
// whose hosting ended (paused, or deleted): its outcome, and why it failed
// or was not attempted (core_unavailable, runtime_misconfigured,
// core_too_old, operator_agent), null otherwise.
type Revocation struct {
	Outcome string  `json:"outcome"`
	Problem *string `json:"problem"`
}

func revocation(outcome, problem string) Revocation {
	r := Revocation{Outcome: outcome}
	if problem != "" {
		r.Problem = &problem
	}
	return r
}

// revokeHosting revokes the token Core holds for the agent coreActorID as
// the runtime's, as its hosting ends here (agent_runtime.revoke_token); an
// agent the operator's configuration runs keeps its own, and is not asked
// about. What fails is logged: the hosting ends all the same.
func (s *Server) revokeHosting(ctx context.Context, rowID, coreActorID string) Revocation {
	if s.o.Actors != nil {
		if _, hosted, ok := s.o.Actors.ActorAgent(s.o.CoreBaseURL, coreActorID); ok && !hosted {
			return revocation(RevocationNotAttempted, ReasonOperatorAgent)
		}
	}
	if s.o.Runtime == nil {
		s.o.Log.Error("a hosted agent's token was not revoked in Core: the runtime has no credential of its own (CORE_SERVICE_CREDENTIAL)",
			"agent", rowID)
		return revocation(RevocationFailed, ReasonRuntimeMisconfigured)
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	revoked, err := s.o.Runtime.RevokeToken(core.WithPriority(rctx, core.PriorityAnswer), coreActorID)
	if err != nil {
		e := s.serviceRefusal("agent_runtime.revoke_token", err)
		s.o.Log.Warn("a hosted agent's token was not revoked in Core as its hosting ended", "agent", rowID, "reason", e.Reason)
		return revocation(RevocationFailed, e.Reason)
	}
	if len(revoked) == 0 {
		return revocation(RevocationNone, "")
	}
	return revocation(RevocationRevoked, "")
}

// coreClient is the client calls to Core go through, bounded by
// core.DefaultTimeout when it has none of its own.
func coreClient(c *http.Client) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	if c.Timeout > 0 {
		return c
	}
	cp := *c
	cp.Timeout = core.DefaultTimeout
	return &cp
}

// sameActor reports whether two actor ids are one, in any case.
func sameActor(x, y string) bool { return strings.EqualFold(x, y) }

// tenantOf is the tenant of an owner's agents and secrets.
func tenantOf(owner string) string { return "ten_" + owner }

// noteAgent puts the agent's id on the request's log line.
func noteAgent(w http.ResponseWriter, id string) {
	if rec, ok := w.(*recorder); ok {
		rec.agent = id
	}
}

// storeUnavailable answers a store that did not answer.
func (s *Server) storeUnavailable(w http.ResponseWriter, what string, err error) {
	s.o.Log.Warn("the store cannot be reached", "what", what, "err", err)
	WriteError(w, Error{Code: CodeUnavailable, Reason: ReasonStoreUnavailable, Message: "the runtime's store cannot be reached"})
}

// purge removes what the store holds of a deleted agent but its ledger;
// what it misses, or a worker writes after it, housekeeping purges.
func (s *Server) purge(ctx context.Context, id string) {
	if err := s.o.Store.PurgeAgent(ctx, id); err != nil {
		s.o.Log.Warn("a deleted agent was not purged; housekeeping purges it", "agent", id, "err", err)
	}
}

// keepsTokens reports whether the runtime can keep the tokens Core issues
// its hosted agents (it has a vault to seal them with), having answered
// when not.
func (s *Server) keepsTokens(w http.ResponseWriter) bool {
	if s.o.Vault == nil {
		s.o.Log.Error("the runtime cannot seal a hosted agent's token: it has no keyring (KMS_KEY_ID)")
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime cannot keep tokens now"})
		return false
	}
	return true
}
