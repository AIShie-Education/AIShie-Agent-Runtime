package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// tokenManager owns only agent token lifecycle. It has no conversation inbox,
// answer loop, draft writer or model adapter. A lease prevents two processes
// issuing (and thereby revoking each other's) one Core agent token.
type tokenManager struct {
	store      store.Store
	secrets    secrets.Resolver
	sealer     *vault.Vault
	credential func(context.Context) (string, error)
	http       *http.Client
	bucket     *ratelimit.Bucket
	log        *slog.Logger
	workerID   string
	now        func() time.Time
	inspect    func(context.Context, *config.Agent, string) (*core.Actor, error) // test hook; production uses me_get.

	mu      sync.RWMutex
	configs map[string]*config.Agent
	states  map[string]*tokenState
	serial  sync.Mutex
	wake    chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}
}

type tokenState struct {
	cfg         *config.Agent
	lease       bool
	nextLease   time.Time
	memoryToken string
	memoryActor string
	retryAt     time.Time
	failures    int
}

const (
	tokenLeaseEvery = 10 * time.Second
	tokenLeaseTTL   = 30 * time.Second
	tokenRetryMax   = 30 * time.Second
	tokenKeepTries  = 5
)

func newTokenManager(st store.Store, res secrets.Resolver, sealer *vault.Vault,
	credential func(context.Context) (string, error), client *http.Client, bucket *ratelimit.Bucket,
	log *slog.Logger, workerID string) *tokenManager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &tokenManager{store: st, secrets: res, sealer: sealer, credential: credential, http: client,
		bucket: bucket, log: log, workerID: workerID, now: time.Now,
		configs: map[string]*config.Agent{}, states: map[string]*tokenState{}, wake: make(chan struct{}, 1)}
}

// Sync installs the latest registry-built configuration and reconciles it now.
// Errors are returned for the caller to report and retried by Run.
func (m *tokenManager) Sync(ctx context.Context, cfg *config.Config) error {
	m.serial.Lock()
	defer m.serial.Unlock()
	m.mu.Lock()
	m.configs = map[string]*config.Agent{}
	if cfg != nil {
		for _, a := range cfg.Agents {
			if a != nil {
				m.configs[a.ID] = a
			}
		}
	}
	m.mu.Unlock()
	return m.reconcile(ctx)
}

// Run renews leases and retries failed issuance until stopped.
func (m *tokenManager) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel, m.done = cancel, make(chan struct{})
	done := m.done
	m.mu.Unlock()
	ticker := time.NewTicker(time.Second)
	defer close(done)
	defer func() {
		ticker.Stop()
		m.mu.Lock()
		cancel()
		m.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
		m.serial.Lock()
		_ = m.reconcile(ctx)
		m.serial.Unlock()
	}
}

// Close stops retries and releases leases. Ordinary shutdown does not revoke
// tokens: another lease holder may already be serving chats with them.
func (m *tokenManager) Close() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	m.serial.Lock()
	defer m.serial.Unlock()
	ctx, cancelCtx := context.WithTimeout(context.Background(), tokenLeaseEvery)
	defer cancelCtx()
	for id, st := range m.states {
		if st.lease {
			if err := m.store.ReleaseLease(ctx, "agent:"+id, m.workerID); err != nil {
				m.log.Warn("agent token lease not released", "agent", id, "err", redact.String(err.Error()))
			}
			st.lease = false
		}
	}
}

// OperatorToken is chatwire's optional in-memory YAML token hook. It retains
// no token in Wiring and remains subject to me_get actor validation there.
func (m *tokenManager) OperatorToken(_ context.Context, cfg *config.Agent) (string, bool, error) {
	if cfg == nil || cfg.Hosted != nil {
		return "", false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.states[cfg.ID]
	if st == nil || st.memoryToken == "" || !strings.EqualFold(st.memoryActor, cfg.Core.AgentID) {
		return "", false, nil
	}
	return st.memoryToken, true, nil
}

func (m *tokenManager) reconcile(ctx context.Context) error {
	m.mu.RLock()
	agents := make([]*config.Agent, 0, len(m.configs))
	for _, a := range m.configs {
		agents = append(agents, a)
	}
	m.mu.RUnlock()
	slices.SortFunc(agents, func(a, b *config.Agent) int { return strings.Compare(a.ID, b.ID) })

	current := make(map[string]bool, len(agents))
	var errs []error
	for _, a := range agents {
		current[a.ID] = true
		if a.Paused {
			if err := m.endAgent(ctx, a, "paused"); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := m.ensureAgent(ctx, a); err != nil && ctx.Err() == nil {
			m.log.Warn("agent token is not ready; it will be retried", "agent", a.ID, "err", redact.String(err.Error()))
			errs = append(errs, fmt.Errorf("%s: %w", a.ID, err))
		}
	}
	m.mu.RLock()
	var removed []*config.Agent
	for id, st := range m.states {
		if current[id] {
			continue
		}
		if st.cfg != nil {
			removed = append(removed, st.cfg)
		}
	}
	m.mu.RUnlock()
	for _, a := range removed {
		if err := m.endAgent(ctx, a, "removed from configuration"); err != nil {
			errs = append(errs, err)
		}
		m.mu.Lock()
		delete(m.states, a.ID)
		m.mu.Unlock()
	}
	return errors.Join(errs...)
}

// state gets or creates one tokenState.
func (m *tokenManager) state(id string) *tokenState {
	st := m.states[id]
	if st == nil {
		st = &tokenState{}
		m.states[id] = st
	}
	return st
}

func (m *tokenManager) ensureAgent(ctx context.Context, a *config.Agent) error {
	now := m.now()
	m.mu.Lock()
	st := m.state(a.ID)
	st.cfg = a
	if now.Before(st.retryAt) {
		m.mu.Unlock()
		return nil
	}
	if !st.lease || !now.Before(st.nextLease) {
		ok, err := m.store.AcquireLease(ctx, "agent:"+a.ID, m.workerID, tokenLeaseTTL)
		if err != nil {
			m.mu.Unlock()
			return m.failed(a.ID, err)
		}
		st.lease = ok
		if ok {
			st.nextLease = now.Add(tokenLeaseEvery)
		}
	}
	if !st.lease {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	if err := m.checkExisting(ctx, a); err == nil {
		m.succeeded(a.ID)
		return nil
	} else if !errors.Is(err, errTokenNeedsIssue) && !errors.Is(err, errTokenActor) && !errors.Is(err, core.ErrUnauthenticated) {
		return m.failed(a.ID, err)
	}
	if err := m.issueAgent(ctx, a); err != nil {
		return m.failed(a.ID, err)
	}
	m.succeeded(a.ID)
	return nil
}

var (
	errTokenNeedsIssue = errors.New("agent token needs to be issued")
	errTokenActor      = errors.New("core token actor does not match the configured agent")
)

func (m *tokenManager) failed(id string, err error) error {
	m.mu.Lock()
	st := m.state(id)
	st.failures++
	wait := min(time.Second<<min(st.failures-1, 5), tokenRetryMax)
	st.retryAt = m.now().Add(wait)
	m.mu.Unlock()
	return err
}

func (m *tokenManager) succeeded(id string) {
	m.mu.Lock()
	st := m.state(id)
	st.failures, st.retryAt = 0, time.Time{}
	m.mu.Unlock()
}

// checkExisting returns nil only for a token which resolves and whose me_get
// names exactly the configured runtime agent (and its hosted owner).
func (m *tokenManager) checkExisting(ctx context.Context, a *config.Agent) error {
	var token string
	if h := a.Hosted; h != nil {
		if h.TokenSecretID == "" || !h.TokenIssued {
			return errTokenNeedsIssue
		}
		var err error
		token, err = m.secrets.Resolve(ctx, secrets.SchemeSealed+h.TokenSecretID, "")
		if err != nil || token == "" {
			return fmt.Errorf("hosted token cannot be opened: %w", err)
		}
	} else {
		m.mu.RLock()
		st := m.states[a.ID]
		if st != nil && st.memoryToken != "" && strings.EqualFold(st.memoryActor, a.Core.AgentID) {
			token = st.memoryToken
		}
		m.mu.RUnlock()
		if token == "" && m.sealer != nil {
			held, err := m.store.AgentToken(ctx, a.ID)
			switch {
			case errors.Is(err, store.ErrNotFound):
				return errTokenNeedsIssue
			case err != nil:
				return err
			case held == nil || held.SecretID == "":
				return errTokenNeedsIssue
			case !strings.EqualFold(held.CoreActorID, a.Core.AgentID):
				if _, err := m.runtime(a).RevokeToken(ctx, held.CoreActorID); err != nil {
					m.log.Warn("the previous Core actor's token was not revoked", "agent", a.ID, "err", redact.String(err.Error()))
				}
				_ = m.store.DeleteAgentToken(ctx, a.ID, held.SecretID)
				return errTokenNeedsIssue
			}
			token, err = m.secrets.Resolve(ctx, secrets.SchemeSealed+held.SecretID, "")
			if err != nil || token == "" {
				return fmt.Errorf("operator token cannot be opened: %w", err)
			}
		}
		if token == "" {
			return errTokenNeedsIssue
		}
	}
	return m.validateToken(ctx, a, token)
}

func (m *tokenManager) validateToken(ctx context.Context, a *config.Agent, token string) error {
	inspect := m.inspect
	if inspect == nil {
		inspect = func(ctx context.Context, a *config.Agent, token string) (*core.Actor, error) {
			caller := core.NewMCPCaller(core.MCPOptions{BaseURL: a.Core.BaseURL, Token: token,
				Protocol: a.Core.MCPProtocol, HTTPClient: m.http})
			return core.NewClient(caller).Me(core.WithPriority(ctx, core.PriorityBackground))
		}
	}
	actor, err := inspect(ctx, a, token)
	if err != nil {
		return err
	}
	if actor == nil || !strings.EqualFold(actor.ID, a.Core.AgentID) || actor.Kind != "" && actor.Kind != core.KindAgent ||
		actor.Status != "" && actor.Status != core.StatusActive || actor.Hosting != "" && actor.Hosting != core.HostingRuntime {
		return errTokenActor
	}
	if a.Hosted != nil && a.Hosted.OwnerActorID != "" && !strings.EqualFold(actor.OwnerActorID, a.Hosted.OwnerActorID) {
		return errTokenActor
	}
	return nil
}

func (m *tokenManager) issueAgent(ctx context.Context, a *config.Agent) error {
	if a.Hosted != nil && m.sealer == nil {
		return errors.New("a hosted agent's token cannot be kept without KMS_KEY_ID")
	}
	rs := m.runtime(a)
	view, err := rs.Agent(core.WithPriority(ctx, core.PriorityBackground), strings.ToLower(a.Core.AgentID))
	if err != nil {
		return err
	}
	if view.Hosting != core.HostingRuntime || !view.Hostable {
		return fmt.Errorf("core says the agent is not hostable: %s", view.Reason)
	}
	if view.Status != core.StatusActive || view.OwnerStatus != "" && view.OwnerStatus != core.StatusActive {
		return errors.New("core says the agent or its owner is suspended")
	}
	if a.Hosted != nil && !strings.EqualFold(view.OwnerActorID, a.Hosted.OwnerActorID) {
		m.revoke(ctx, a)
		return errors.New("the hosted agent's owner changed in Core")
	}
	it, err := rs.IssueToken(core.WithPriority(ctx, core.PriorityBackground), strings.ToLower(a.Core.AgentID), core.RuntimeTokenLabel)
	if err != nil {
		return err
	}
	if it == nil || it.Token == "" {
		return errors.New("core issued no token")
	}
	if err := m.validateToken(ctx, a, it.Token); err != nil {
		m.revoke(ctx, a)
		return err
	}
	if a.Hosted != nil {
		return m.keepHosted(ctx, a, it)
	}
	return m.keepOperator(ctx, a, it)
}

func (m *tokenManager) runtime(a *config.Agent) *core.RuntimeService {
	return core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: a.Core.BaseURL,
		Credential: m.credential, HTTPClient: m.http, Bucket: m.bucket}))
}

func (m *tokenManager) keepHosted(ctx context.Context, a *config.Agent, it *core.IssuedToken) error {
	for range tokenKeepTries {
		row, err := m.store.HostedAgent(ctx, a.ID)
		switch {
		case errors.Is(err, store.ErrNotFound), err == nil && row.Paused:
			m.revoke(ctx, a)
			return errors.New("the hosted agent was removed or paused while its token was issued")
		case err != nil:
			m.revoke(ctx, a)
			return err
		case !strings.EqualFold(row.CoreActorID, a.Core.AgentID):
			m.revoke(ctx, a)
			return errTokenActor
		case row.TokenIssued && row.TokenSecretID != "" && !slices.Contains(it.Replaced, row.TokenCredentialID):
			// A newer lease holder's token wins; this one has been revoked.
			token, err := m.secrets.Resolve(ctx, secrets.SchemeSealed+row.TokenSecretID, "")
			if err != nil {
				return err
			}
			if err := m.validateToken(ctx, a, token); err != nil {
				return err
			}
			m.adoptHosted(a, row)
			return nil
		}
		sealed, err := m.sealer.Seal(ctx, store.Secret{ID: vault.NewSecretID(), TenantID: row.TenantID, Kind: store.SecretCoreToken}, it.Token)
		if err != nil {
			m.revoke(ctx, a)
			return err
		}
		next := *row
		next.TokenSecretID, next.TokenHint, next.TokenIssued, next.TokenCredentialID = sealed.ID, sealed.Hint, true, it.CredentialID
		next.OwnerVerified = true
		updated, err := m.store.UpdateHostedAgent(ctx, next, sealed)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			m.revoke(ctx, a)
			return err
		}
		m.adoptHosted(a, updated)
		m.log.Info("agent token issued and sealed", "agent", a.ID, "credential", it.CredentialID, "replaced", len(it.Replaced))
		return nil
	}
	m.revoke(ctx, a)
	return errors.New("the hosted row changed repeatedly while its token was kept")
}

func (m *tokenManager) adoptHosted(a *config.Agent, row *store.HostedAgent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *a
	h := *a.Hosted
	h.TokenSecretID, h.TokenIssued, h.OwnerVerified, h.Version = row.TokenSecretID, row.TokenIssued, row.OwnerVerified, row.Version
	cp.Hosted = &h
	if m.states[a.ID] != nil {
		m.states[a.ID].cfg = &cp
	}
	m.configs[a.ID] = &cp
}

func (m *tokenManager) keepOperator(ctx context.Context, a *config.Agent, it *core.IssuedToken) error {
	m.mu.Lock()
	if m.sealer == nil {
		st := m.state(a.ID)
		st.memoryToken, st.memoryActor = it.Token, strings.ToLower(a.Core.AgentID)
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	tenant := a.TenantID
	if tenant == "" {
		tenant = "operator"
	}
	sealed, err := m.sealer.Seal(ctx, store.Secret{ID: vault.NewSecretID(), TenantID: tenant, Kind: store.SecretCoreToken}, it.Token)
	if err != nil {
		m.revoke(ctx, a)
		return err
	}
	record := store.AgentToken{AgentID: a.ID, CoreActorID: strings.ToLower(a.Core.AgentID), SecretID: sealed.ID,
		CredentialID: it.CredentialID, Hint: sealed.Hint, IssuedAt: m.now().UTC(), IssuedBy: m.workerID}
	for range tokenKeepTries {
		previous := ""
		held, err := m.store.AgentToken(ctx, a.ID)
		if err == nil && held != nil {
			if strings.EqualFold(held.CoreActorID, record.CoreActorID) && held.IssuedAt.After(record.IssuedAt) && !slices.Contains(it.Replaced, held.CredentialID) {
				return nil // another lease holder's newer token wins
			}
			previous = held.SecretID
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			m.revoke(ctx, a)
			return err
		}
		if err := m.store.PutAgentToken(ctx, record, sealed, previous); err == nil {
			m.log.Info("agent token issued and sealed", "agent", a.ID, "credential", it.CredentialID, "replaced", len(it.Replaced))
			return nil
		} else if !errors.Is(err, store.ErrConflict) {
			m.revoke(ctx, a)
			return err
		}
	}
	m.revoke(ctx, a)
	return errors.New("the operator token changed repeatedly while it was kept")
}

// endAgent revokes and forgets a removed or paused agent's token only when
// this manager holds its lease, then releases the lease.
func (m *tokenManager) endAgent(ctx context.Context, a *config.Agent, why string) error {
	m.mu.Lock()
	st := m.state(a.ID)
	held := st.lease
	m.mu.Unlock()
	if !held {
		return nil
	}
	var errs []error
	if _, err := m.runtime(a).RevokeToken(ctx, strings.ToLower(a.Core.AgentID)); err != nil && ctx.Err() == nil {
		errs = append(errs, err)
	}
	if a.Hosted != nil {
		row, err := m.store.HostedAgent(ctx, a.ID)
		if err == nil && row.TokenSecretID != "" {
			next := *row
			next.TokenSecretID, next.TokenHint, next.TokenIssued, next.TokenCredentialID = "", "", false, ""
			if _, err := m.store.UpdateHostedAgent(ctx, next); err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
				errs = append(errs, err)
			}
		}
	} else {
		if err := m.store.DeleteAgentToken(ctx, a.ID, ""); err != nil {
			errs = append(errs, err)
		}
	}
	m.mu.Lock()
	st.memoryToken, st.memoryActor = "", ""
	st.lease = false
	m.mu.Unlock()
	if err := m.store.ReleaseLease(ctx, "agent:"+a.ID, m.workerID); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		m.log.Warn("agent token hosting did not end cleanly", "agent", a.ID, "why", why)
	}
	return errors.Join(errs...)
}

func (m *tokenManager) revoke(parent context.Context, a *config.Agent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), tokenLeaseEvery)
	defer cancel()
	if _, err := m.runtime(a).RevokeToken(ctx, strings.ToLower(a.Core.AgentID)); err != nil {
		m.log.Warn("an unkept agent token was not revoked", "agent", a.ID, "err", redact.String(err.Error()))
	}
}
