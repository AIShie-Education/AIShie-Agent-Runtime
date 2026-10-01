package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// Hosting by an agent's id (AIShie-Core #52; docs/design.md §5.1,
// §11.2). Every agent the runtime runs, the operator's and the hosted
// alike, is an agent hosted runtime in Core, named by its id
// (core.agent_id): the runtime takes no token from anyone. As the worker
// that holds an agent's lease starts it, it reads the agent as Core hosts
// it, with the runtime's own credential (agent_runtime.agent): an mcp
// agent is not run; a hosted agent whose owner of record is not the person
// who hosted it, or one suspended (or whose owner is), has its token
// revoked; then it runs the agent with the token the runtime holds for it,
// or is issued one by its id (agent_runtime.issue_token), seals it and
// keeps it: a hosted agent's in its row, an operator's agent's in the store
// (store.AgentTokens), where every worker finds the one token Core holds
// live for the agent. Only the worker holding the agent's lease is issued
// it, and it keeps it only while the row (or the store's token) is still
// as it read it: two workers never each issue one, revoking the other's.
// A token pasted by an owner before hosting was by id is replaced at the
// agent's next start, which revokes it in Core.

// tokenLabel is what an agent's owner sees the runtime's token listed as.
const tokenLabel = core.RuntimeTokenLabel

// operatorTenant is the tenant an operator's agent's token is sealed
// under when its configuration names none.
const operatorTenant = "operator"

// Why an agent the runtime may not host is not run: each stops it until
// its configuration changes, or a reload.
var (
	errMCPAgent = &blockedError{reason: store.ReasonMCPAgent,
		msg: "the agent is an mcp agent in Core: its owner's own tools reach it over MCP, and the runtime cannot host it (only an agent " +
			"hosted runtime is); remove it here"}
	errAgentNotFound = &blockedError{reason: store.ReasonAgentNotFound,
		msg: "Core has no agent of this id (core.agent_id): it was never one, or it was removed"}
	errNoRuntimeService = &blockedError{reason: store.ReasonCoreTooOld,
		msg: "this Core has no agent_runtime service (it is older than AIShie-Core #52), which the runtime is issued an agent's token " +
			"by: Core must be upgraded, and the runtime reloaded"}
	errNoSealer = &blockedError{reason: store.ReasonRuntimeMisconfigured,
		msg: "the runtime cannot keep the token Core issues it for a hosted agent: it has no key to seal it with (KMS_KEY_ID)"}
)

// Failures tried again after a backoff, as any is: the agent runs by
// itself once they pass.
var (
	errNoCredential = &reasonError{reason: store.ReasonRuntimeMisconfigured,
		err: errors.New("the runtime has no credential of its own in Core (CORE_SERVICE_CREDENTIAL): it cannot be issued the agent's token")}
	errCredentialRefused = &reasonError{reason: store.ReasonRuntimeMisconfigured,
		err: errors.New("the runtime's own credential was refused by Core (CORE_SERVICE_CREDENTIAL): it was revoked, has expired, or is not " +
			"the agent_runtime service's; the operator issues another (aishie-core service issue agent_runtime), and the runtime " +
			"takes it at its next try")}
	errOwnerSuspended = &reasonError{reason: store.ReasonOwnerSuspended,
		err: errors.New("the agent's owner is suspended in Core: the agent is paused while they are, and runs again by itself once " +
			"they are reactivated")}
)

// ownerChangedDetail is what a hosted agent's state says when Core names
// another owner than the person who hosted it. It names no one: the state
// is shown to whoever the registry says owns the agent.
const ownerChangedDetail = "the agent's owner in Core is not the person who hosted it here: it is not run, and its token is revoked; " +
	"its owner hosts it again"

// runtime is the agent runtime's client at baseURL, whose catalogue is
// cat: made once per Core, and nil with an error when the runtime has no
// credential, or the Core no service.
func (s *Supervisor) runtime(baseURL string, cat *core.Catalogue) (*core.RuntimeService, error) {
	if s.o.RuntimeCredential == nil {
		return nil, errNoCredential
	}
	if !core.HasRuntimeService(cat) {
		return nil, errNoRuntimeService
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rs := s.runtimes[baseURL]; rs != nil {
		return rs, nil
	}
	rs := core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: baseURL, Credential: s.o.RuntimeCredential,
		HTTPClient: s.coreHTTP, Bucket: s.o.RuntimeBucket, Retry: s.o.CoreRetry, Catalogue: cat}))
	s.runtimes[baseURL] = rs
	return rs, nil
}

// agentID is the agent's id in Core, in lower case as Core writes it.
func agentID(cfg *config.Agent) string { return strings.ToLower(cfg.Core.AgentID) }

// serviceError is a call of the service that failed, as the reason the
// agent stops for: the credential refused, Core too old, the agent not
// there, refused for its hosting or its standing; else err itself, a
// failure tried again.
func serviceError(err error) error {
	var ce *core.CredentialError
	switch {
	case errors.Is(err, errNoCredential), errors.Is(err, errNoRuntimeService):
		return err
	case errors.As(err, &ce):
		return &reasonError{reason: store.ReasonRuntimeMisconfigured, err: err}
	case core.CredentialRefused(err):
		return errCredentialRefused
	case core.IsReason(err, core.ReasonCoreTooOld):
		return errNoRuntimeService
	case core.IsNotFound(err):
		return errAgentNotFound
	case core.IsReason(err, core.ReasonNotRuntimeHosted):
		return errMCPAgent
	case core.IsReason(err, core.ReasonAgentSuspended):
		return errSuspended
	case core.IsReason(err, core.ReasonOwnerSuspended):
		return errOwnerSuspended
	}
	return err
}

// host reads the agent as Core hosts it, and holds it to being one the
// runtime may run now: an agent hosted runtime (errMCPAgent), a hosted
// agent's owner of record still the person who hosted it (an
// *OwnerProblem), it and its owner active (errSuspended,
// errOwnerSuspended). A hosted agent whose owner is not the one who hosted
// it, or one suspended, ends its hosting: its token is revoked, and
// forgotten. One whose owner is suspended keeps its token: Core pauses the
// agent while its owner is, and it runs again once they are reactivated.
// A hosted agent's row is marked as its owner checked.
func (a *Agent) host(ctx context.Context, rs *core.RuntimeService) (*core.RuntimeAgent, error) {
	ctx = core.WithPriority(ctx, core.PriorityBackground)
	view, err := rs.Agent(ctx, agentID(a.cfg))
	if err != nil {
		return nil, serviceError(fmt.Errorf("agent_runtime.agent: %w", err))
	}
	var stop error
	switch h := a.cfg.Hosted; {
	case view.Hosting != core.HostingRuntime:
		return nil, errMCPAgent
	case h != nil && !strings.EqualFold(view.OwnerActorID, h.OwnerActorID):
		stop = &OwnerProblem{State: store.AgentOwnerChanged, Reason: store.ReasonOwnerChanged, Detail: ownerChangedDetail}
	case view.Status != core.StatusActive:
		stop = errSuspended
	case view.OwnerStatus != "" && view.OwnerStatus != core.StatusActive:
		return nil, errOwnerSuspended
	}
	if stop != nil {
		a.endHosting(ctx, rs, view)
		return nil, stop
	}
	a.s.markOwnerVerified(ctx, a.cfg)
	return view, nil
}

// endHosting revokes the runtime's token of the agent in Core, when Core
// shows it holds one, and forgets the one the runtime keeps, for an agent
// it stops hosting. What fails is logged: the agent is not run either way.
func (a *Agent) endHosting(ctx context.Context, rs *core.RuntimeService, view *core.RuntimeAgent) {
	if view.RuntimeToken != nil {
		revoked, err := rs.RevokeToken(ctx, agentID(a.cfg))
		if err != nil {
			a.log.Warn("the agent's token was not revoked in Core", "err", err)
		} else {
			a.log.Info("the agent's token was revoked in Core: the runtime no longer hosts it", "revoked", len(revoked))
		}
	}
	a.forgetToken(ctx, "")
}

// token is the token the agent runs with: the one the runtime keeps for it,
// or one issued now by its id. A hosted agent's row holding one its owner
// pasted, from before hosting was by id, is issued one in its place, which
// revokes the pasted one.
func (a *Agent) token(ctx context.Context, rs *core.RuntimeService) (string, error) {
	if h := a.cfg.Hosted; h != nil {
		switch {
		case h.TokenSecretID != "" && h.TokenIssued:
			a.heldSecret = h.TokenSecretID
			return a.s.o.Secrets.Resolve(ctx, secrets.SchemeSealed+h.TokenSecretID, "")
		case h.TokenSecretID != "":
			a.log.Info("the agent's token was pasted before hosting was by its id: the runtime is issued one in its place, which revokes it")
		}
		t, err := a.issue(ctx, rs)
		a.heldSecret = a.cfg.Hosted.TokenSecretID
		return t, err
	}
	t, secret, err := a.s.operatorToken(ctx, a.cfg)
	if err != nil || t != "" {
		a.heldSecret = secret
		return t, err
	}
	return a.issue(ctx, rs)
}

// issue is the agent's token issued to the runtime now, kept where every
// worker finds it, and returned. Core refusing it says why the agent is
// not run (serviceError).
func (a *Agent) issue(ctx context.Context, rs *core.RuntimeService) (string, error) {
	if a.cfg.Hosted != nil && a.s.o.Sealer == nil {
		return "", errNoSealer
	}
	asked := a.now()
	it, err := rs.IssueToken(core.WithPriority(ctx, core.PriorityBackground), agentID(a.cfg), tokenLabel)
	if err != nil {
		return "", serviceError(fmt.Errorf("agent_runtime.issue_token: %w", err))
	}
	a.s.o.Metrics.TokensIssued.Inc()
	a.log.Info("the runtime was issued the agent's token by its id", "credential", it.CredentialID, "replaced", len(it.Replaced))
	if a.cfg.Hosted != nil {
		return a.keepHosted(ctx, rs, it)
	}
	t, secret, err := a.s.keepOperatorToken(ctx, a.cfg, it, asked)
	a.heldSecret = secret
	return t, err
}

// keepAttempts bounds keeping an issued token while its row changes under
// the write.
const keepAttempts = 5

// errHostingEnded stops an agent whose token was issued while its hosting
// ended (its row paused, or deleted): the token is revoked, and the
// configuration in force stops it, or restarts it as it now is.
var errHostingEnded = errors.New("the agent's hosting ended while its token was issued: it is revoked")

// keepHosted seals the token Core issued for the hosted agent in its row,
// written at the version read, and puts the row as written in force for
// the agent and its runner (adopt), so that the registry's rebuild, which
// the write sets off, restarts nothing; it returns the token the agent
// runs with. A row written meanwhile is read again: a token another put in
// since that this one replaced is written over, one put in after is kept,
// and run with, and this one dropped (Core revoked it); a row paused or
// deleted meanwhile is no longer hosted, and the token is revoked.
func (a *Agent) keepHosted(ctx context.Context, rs *core.RuntimeService, it *core.IssuedToken) (string, error) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*storeTimeout)
	defer cancel()
	for range keepAttempts {
		row, err := a.store().HostedAgent(sctx, a.id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			a.revokeIssued(ctx, rs, "its row was deleted")
			return "", errHostingEnded
		case err != nil:
			a.revokeIssued(ctx, rs, "its row could not be read")
			return "", fmt.Errorf("the agent's row: %w", err)
		case row.Paused:
			a.revokeIssued(ctx, rs, "its row was paused")
			return "", errHostingEnded
		case !strings.EqualFold(row.CoreActorID, agentID(a.cfg)):
			a.revokeIssued(ctx, rs, "its row names another agent")
			return "", errHostingEnded
		case row.TokenIssued && row.TokenSecretID != "" && row.TokenSecretID != a.cfg.Hosted.TokenSecretID &&
			!slices.Contains(it.Replaced, row.TokenCredentialID):
			// Another worker was issued it after this one: its token is
			// the live one, and this one's is revoked.
			a.adopt(row)
			return a.s.o.Secrets.Resolve(sctx, secrets.SchemeSealed+row.TokenSecretID, "")
		}
		sealed, err := a.seal(sctx, row.TenantID, it)
		if err != nil {
			a.revokeIssued(ctx, rs, "it could not be sealed")
			return "", err
		}
		next := *row
		next.TokenSecretID, next.TokenHint, next.TokenIssued, next.TokenCredentialID = sealed.ID, sealed.Hint, true, it.CredentialID
		updated, err := a.store().UpdateHostedAgent(sctx, next, sealed)
		switch {
		case errors.Is(err, store.ErrConflict):
			continue
		case err != nil:
			a.revokeIssued(ctx, rs, "it could not be kept")
			return "", fmt.Errorf("the agent's token kept: %w", err)
		}
		a.adopt(updated)
		return it.Token, nil
	}
	a.revokeIssued(ctx, rs, "its row kept changing")
	return "", fmt.Errorf("the agent's token was not kept: its row changed %d times while it was written", keepAttempts)
}

// seal seals the issued token under tenant.
func (a *Agent) seal(ctx context.Context, tenant string, it *core.IssuedToken) (store.Secret, error) {
	sealed, err := a.s.o.Sealer.Seal(ctx, store.Secret{ID: vault.NewSecretID(), TenantID: tenant, Kind: store.SecretCoreToken}, it.Token)
	if err != nil {
		return store.Secret{}, fmt.Errorf("the agent's token could not be sealed: %w", err)
	}
	return sealed, nil
}

// revokeIssued revokes the token just issued, which the runtime will not
// keep, saying why.
func (a *Agent) revokeIssued(ctx context.Context, rs *core.RuntimeService, why string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, err := rs.RevokeToken(rctx, agentID(a.cfg)); err != nil {
		a.log.Warn("a token the runtime was issued and does not keep was not revoked in Core", "why", why, "err", err)
		return
	}
	a.log.Info("a token the runtime was issued and does not keep was revoked in Core", "why", why)
}

// adopt puts the hosted agent's row as the worker wrote it (or read it)
// in force for the agent, and for its runner while the runner runs it as
// it does: the token in the row and its version, which the registry's
// rebuild then brings, so that it restarts nothing (sameRun), and the
// states written after name the new version.
func (a *Agent) adopt(row *store.HostedAgent) {
	s := a.s
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := *a.cfg
	h := *cfg.Hosted
	h.TokenSecretID, h.TokenIssued, h.OwnerVerified, h.Version = row.TokenSecretID, row.TokenIssued, row.OwnerVerified, row.Version
	cfg.Hosted = &h
	if r := s.runners[a.id]; r != nil && r.agent == a && sameRun(r.cfg, a.cfg) {
		r.cfg = &cfg
	}
	a.cfg = &cfg
}

// forgetToken forgets the token the runtime keeps for the agent, when it
// is still the secret secretID ("" for any): a hosted agent's row holds
// none, at the version read (adopt), and an operator's agent's is
// forgotten. Its next start is issued another.
func (a *Agent) forgetToken(ctx context.Context, secretID string) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if a.cfg.Hosted == nil {
		a.s.forgetOperatorToken(sctx, a.cfg, secretID)
		return
	}
	row, err := a.store().HostedAgent(sctx, a.id)
	if err != nil || row.TokenSecretID == "" || (secretID != "" && row.TokenSecretID != secretID) {
		return
	}
	next := *row
	next.TokenSecretID, next.TokenHint, next.TokenIssued, next.TokenCredentialID = "", "", false, ""
	updated, err := a.store().UpdateHostedAgent(sctx, next)
	if err != nil {
		a.log.Warn("the agent's token was not forgotten in its row", "err", err)
		return
	}
	a.adopt(updated)
}

// operatorToken is the token the runtime keeps for an operator's agent,
// and its secret in the store, "" for none: sealed in the store
// (store.AgentTokens), else in this worker's memory (no secret). One kept
// for another agent in Core (core.agent_id changed in the configuration)
// is revoked for that agent, and forgotten.
func (s *Supervisor) operatorToken(ctx context.Context, cfg *config.Agent) (token, secretID string, err error) {
	if s.o.Sealer == nil {
		s.mu.Lock()
		t := s.memTokens[cfg.ID]
		s.mu.Unlock()
		switch {
		case t.token == "":
		case strings.EqualFold(t.coreActorID, agentID(cfg)):
			return t.token, "", nil
		default:
			s.endOther(ctx, cfg, t.coreActorID, "")
		}
		return "", "", nil
	}
	sctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	t, err := s.o.Store.AgentToken(sctx, cfg.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", "", nil
	case err != nil:
		return "", "", fmt.Errorf("the agent's token: %w", err)
	case !strings.EqualFold(t.CoreActorID, agentID(cfg)):
		s.endOther(ctx, cfg, t.CoreActorID, t.SecretID)
		return "", "", nil
	}
	token, err = s.o.Secrets.Resolve(ctx, secrets.SchemeSealed+t.SecretID, "")
	return token, t.SecretID, err
}

// endOther revokes the token the runtime kept for an operator's agent that
// was the agent other in Core, and is now another (core.agent_id changed),
// and forgets it (the secret secretID; "" in memory).
func (s *Supervisor) endOther(ctx context.Context, cfg *config.Agent, other, secretID string) {
	log := s.log.With("agent", cfg.ID)
	cat, err := s.catalogue(ctx, cfg.Core.BaseURL)
	if err == nil {
		var rs *core.RuntimeService
		if rs, err = s.runtime(cfg.Core.BaseURL, cat); err == nil {
			_, err = rs.RevokeToken(ctx, other)
		}
	}
	if err != nil {
		log.Warn("the token the runtime kept for the agent this one was in Core was not revoked there", "err", err)
	} else {
		log.Info("the agent is another in Core (core.agent_id): the token kept for the one before was revoked")
	}
	s.forgetOperatorToken(ctx, cfg, secretID)
}

// keepOperatorToken keeps the token Core issued for an operator's agent,
// asked for at asked, and returns the token the agent runs with: sealed in
// the store in place of the one this worker read, else in memory. Another
// worker's that this one replaced is written over; one it was issued after
// this one was asked for, which this one did not replace, is kept, and run
// with, and this one dropped (Core revoked it).
func (s *Supervisor) keepOperatorToken(ctx context.Context, cfg *config.Agent, it *core.IssuedToken, asked time.Time) (token, secretID string,
	err error) {
	if s.o.Sealer == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.memTokens[cfg.ID] = memToken{token: it.Token, coreActorID: agentID(cfg)}
		return it.Token, "", nil
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*storeTimeout)
	defer cancel()
	tenant := cfg.TenantID
	if tenant == "" {
		tenant = operatorTenant
	}
	sealed, err := s.o.Sealer.Seal(sctx, store.Secret{ID: vault.NewSecretID(), TenantID: tenant, Kind: store.SecretCoreToken}, it.Token)
	if err != nil {
		return "", "", fmt.Errorf("the agent's token could not be sealed: %w", err)
	}
	t := store.AgentToken{AgentID: cfg.ID, CoreActorID: agentID(cfg), SecretID: sealed.ID, CredentialID: it.CredentialID, Hint: sealed.Hint,
		IssuedAt: s.o.Now().UTC(), IssuedBy: s.o.WorkerID}
	for range keepAttempts {
		prev := ""
		held, err := s.o.Store.AgentToken(sctx, cfg.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return "", "", fmt.Errorf("the agent's token: %w", err)
		case strings.EqualFold(held.CoreActorID, t.CoreActorID) && held.IssuedAt.After(asked) && !slices.Contains(it.Replaced, held.CredentialID):
			// Another worker was issued it after this one.
			token, err := s.o.Secrets.Resolve(sctx, secrets.SchemeSealed+held.SecretID, "")
			return token, held.SecretID, err
		default:
			prev = held.SecretID
		}
		err = s.o.Store.PutAgentToken(sctx, t, sealed, prev)
		switch {
		case err == nil:
			return it.Token, sealed.ID, nil
		case !errors.Is(err, store.ErrConflict):
			return "", "", fmt.Errorf("the agent's token kept: %w", err)
		}
	}
	return "", "", fmt.Errorf("the agent's token was not kept: it changed %d times while it was written", keepAttempts)
}

// forgetOperatorToken forgets the token kept for an operator's agent,
// while it is still the secret secretID ("" for any).
func (s *Supervisor) forgetOperatorToken(ctx context.Context, cfg *config.Agent, secretID string) {
	if s.o.Sealer == nil {
		s.mu.Lock()
		delete(s.memTokens, cfg.ID)
		s.mu.Unlock()
		return
	}
	if err := s.o.Store.DeleteAgentToken(ctx, cfg.ID, secretID); err != nil {
		s.log.Warn("the agent's token was not forgotten", "agent", cfg.ID, "err", err)
	}
}

// memToken is an operator's agent's token kept in memory, without a key to
// seal it with: the token, and the agent in Core it was issued for.
type memToken struct {
	token, coreActorID string
}

// endOperatorHosting revokes the token of an operator's agent the
// configuration no longer runs (removed, or paused), and forgets it: the
// worker holding its lease does, before it lets the lease go, so that no
// worker taking the agent up after is revoked the token it is issued.
func (s *Supervisor) endOperatorHosting(cfg *config.Agent, why string) {
	if cfg == nil || cfg.Hosted != nil || cfg.Core.AgentID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log := s.log.With("agent", cfg.ID)
	cat, err := s.catalogue(ctx, cfg.Core.BaseURL)
	if err == nil {
		var rs *core.RuntimeService
		if rs, err = s.runtime(cfg.Core.BaseURL, cat); err == nil {
			_, err = rs.RevokeToken(ctx, agentID(cfg))
		}
	}
	if err != nil {
		log.Warn("the agent's token was not revoked in Core as its hosting ended", "why", why, "err", err)
	} else {
		log.Info("the agent's hosting ended: its token was revoked in Core", "why", why)
	}
	s.forgetOperatorToken(ctx, cfg, "")
}
