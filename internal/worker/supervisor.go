package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
)

// Supervisor runs every configured agent this worker can lease (design
// §5.1). Each agent is leased as "agent:{id}", renewed every
// Timing.LeaseEvery and lasting Timing.LeaseTTL: two workers never run one
// agent's pollers at once, so one process's token bucket is the agent's
// whole spend, and a dead worker's agents are taken up by another within
// the TTL. A paused agent is never leased and makes no call to Core, so
// that Core's presence tells the truth.
type Supervisor struct {
	o        Options
	log      *slog.Logger
	coreHTTP *http.Client
	files    toolset.FileFetcher
	schemas  *toolschema.Cache
	// texts keeps what was read of documents' files, for every agent of
	// this worker: a file read in parts is fetched and read once.
	texts *toolset.TextCache

	kick    chan struct{}
	running atomic.Bool
	prices  atomic.Pointer[pricing.Table]

	mu      sync.Mutex
	cfg     *config.Config
	pending *config.Config
	// retry is whether the pending configuration starts again the agents
	// stopped until a reload, changed or not (Reload), or only those that
	// changed (Update).
	retry   bool
	runners map[string]*runner
	// paused are the paused agents, whose state has been written, and the
	// version of a hosted one's row it was written for.
	paused map[string]int
	// rejected are the registry's agents not run, and why, whose state has
	// been written.
	rejected map[string]rejection
	// cats are Core's catalogues, fetched once per base URL.
	cats map[string]*catEntry
	// actors are the Core actors this worker's agents run as, by base URL
	// and actor id, and the agent that runs as each.
	actors map[string]string

	// siteChatAbsent are the catalogues, by hash, that do not offer
	// me.site_chat, once said.
	siteChatAbsent sync.Map

	// stateLocks order each agent's state writes, by agent id: a write
	// holds its agent's lock from setting the state here until the store
	// has taken it, so that the store takes an agent's states in the order
	// they were set; and a write that reads the state it writes (a
	// rewrite, a detail) reads it under the same lock, never a state an
	// instant old.
	stateLocks sync.Map
}

// runner is one configured agent that is not paused, and the instance of
// it this worker runs, if any. Its fields are the supervisor's, under mu.
type runner struct {
	id  string
	cfg *config.Agent
	// holds is whether this worker holds the agent's lease.
	holds bool
	// agent is the running instance, nil when none runs.
	agent       *Agent
	stopPoll    context.CancelFunc
	stopAnswers context.CancelFunc
	done        chan struct{}
	// stopping is set when the supervisor stops the instance on purpose.
	stopping bool
	// blocked is set when Core refused the agent's token: it is not
	// started again until the configuration is reloaded.
	blocked bool
	// failures and retryAt pace starting again after a failure.
	failures int
	retryAt  time.Time
	state    string
	reason   string
	detail   string
}

// rejection is why the registry's agent is not run, as its state says it,
// and the version of its row that was rejected.
type rejection struct {
	detail, reason string
	version        int
}

// NewSupervisor checks o and makes a supervisor of o.Config's agents. It
// starts nothing until Run.
func NewSupervisor(o Options) (*Supervisor, error) {
	o, err := o.withDefaults()
	if err != nil {
		return nil, err
	}
	s := &Supervisor{
		o: o, log: o.Log.With("worker", o.WorkerID), coreHTTP: coreClient(o.HTTPClient),
		files: toolset.NewHTTPFetcher(o.HTTPClient), schemas: toolschema.NewCache(), texts: toolset.NewTextCache(0),
		kick: make(chan struct{}, 1), runners: map[string]*runner{}, paused: map[string]int{}, rejected: map[string]rejection{},
		cats: map[string]*catEntry{}, actors: map[string]string{}, pending: o.Config,
	}
	s.prices.Store(o.Prices)
	return s, nil
}

// SetPrices replaces the price table the next model calls are costed by;
// nil leaves their costs unknown.
func (s *Supervisor) SetPrices(t *pricing.Table) { s.prices.Store(t) }

// priceTable is the price table in force.
func (s *Supervisor) priceTable() *pricing.Table { return s.prices.Load() }

// Reload replaces the configuration, as SIGHUP does: agents added, removed,
// paused or changed are started, stopped or restarted, and the runtime's
// settings (tenants) apply to the next answer. An agent stopped because
// Core refused its token is started again, whether or not its
// configuration changed: a reload is how an owner says a new token is in
// the file its token_ref names. It returns at once; the running supervisor
// applies it.
func (s *Supervisor) Reload(cfg *config.Config) { s.put(cfg, true) }

// Update replaces the configuration as the registry of hosted agents
// changed it: as Reload, but an agent stopped until a reload starts again
// only if its configuration changed. A hosted agent's new token is a new
// secret, so its configuration changes; the agents that did not change are
// not made to call Core again at every change to another.
func (s *Supervisor) Update(cfg *config.Config) { s.put(cfg, false) }

func (s *Supervisor) put(cfg *config.Config, retry bool) {
	if cfg == nil {
		return
	}
	s.mu.Lock()
	s.pending = cfg
	s.retry = s.retry || retry
	s.mu.Unlock()
	s.poke()
}

func (s *Supervisor) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Running reports whether Run is running.
func (s *Supervisor) Running() bool { return s.running.Load() }

// WorkerID is this worker's name in leases.
func (s *Supervisor) WorkerID() string { return s.o.WorkerID }

// Run leases and runs agents until ctx is done, then stops every agent,
// giving answers in progress Env.ShutdownGrace to finish, releases their
// leases, and returns nil.
func (s *Supervisor) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("worker: the supervisor is running already")
	}
	defer s.running.Store(false)
	s.log.Info("supervisor started")
	s.apply(ctx)
	s.leaseTick(ctx)
	lease := time.NewTicker(s.o.Timing.LeaseEvery)
	defer lease.Stop()
	house := time.NewTicker(s.o.Timing.Housekeeping)
	defer house.Stop()
	s.housekeep(ctx)
	for {
		select {
		case <-ctx.Done():
			s.shutdown()
			s.log.Info("supervisor stopped")
			return nil
		case <-lease.C:
			s.leaseTick(ctx)
		case <-s.kick:
			s.apply(ctx)
			s.leaseTick(ctx)
		case <-house.C:
			s.housekeep(ctx)
		}
	}
}

// config is the configuration in force.
func (s *Supervisor) config() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// tenant is a tenant's settings in the configuration in force.
func (s *Supervisor) tenant(id string) (config.Tenant, bool) {
	cfg := s.config()
	if cfg == nil || id == "" {
		return config.Tenant{}, false
	}
	t, ok := cfg.Runtime.Tenants[id]
	return t, ok
}

// school is the school's plan in the configuration in force.
func (s *Supervisor) school() config.School {
	cfg := s.config()
	if cfg == nil {
		return config.School{}
	}
	return cfg.Runtime.School
}

// apply puts a pending configuration in force.
func (s *Supervisor) apply(ctx context.Context) {
	s.mu.Lock()
	cfg, retry := s.pending, s.retry
	s.pending, s.retry = nil, false
	if cfg == nil {
		s.mu.Unlock()
		return
	}
	s.cfg = cfg
	s.cats = map[string]*catEntry{}
	want := map[string]*config.Agent{}
	for _, a := range cfg.Agents {
		want[a.ID] = a
	}
	type change struct {
		r      *runner
		remove bool
		why    string
	}
	var changes []change
	// rewrite are agents whose hosted row moved on to a version that runs
	// them as they run: their state is written again for it.
	var rewrite []*runner
	for id, r := range s.runners {
		a, ok := want[id]
		switch {
		case !ok:
			changes = append(changes, change{r, true, "removed from the configuration"})
		case a.Paused:
			changes = append(changes, change{r, true, ""})
		case !sameRun(r.cfg, a):
			changes = append(changes, change{r: r})
			r.cfg, r.blocked, r.failures, r.retryAt = a, false, 0, time.Time{}
		case retry && (r.blocked || r.state == store.AgentError):
			r.cfg, r.blocked, r.failures, r.retryAt = a, false, 0, time.Time{}
		default:
			if hostedVersion(r.cfg) != hostedVersion(a) {
				rewrite = append(rewrite, r)
			}
			r.cfg = a
		}
	}
	// A rejected agent's state is error, written below, not stopped.
	rejected := map[string]rejection{}
	for _, rj := range cfg.Rejected {
		if _, running := want[rj.AgentID]; rj.AgentID != "" && !running {
			reason := rj.Reason
			if reason == "" {
				reason = store.ReasonSettingsRejected
			}
			rejected[rj.AgentID] = rejection{detail: rj.Detail(), reason: reason, version: rj.Version}
		}
	}
	for i, c := range changes {
		if _, ok := rejected[c.r.id]; ok && c.remove {
			changes[i].why = ""
		}
	}
	for id, a := range want {
		if _, ok := s.runners[id]; !ok && !a.Paused {
			s.runners[id] = &runner{id: id, cfg: a}
		}
	}
	var nowPaused []string
	for id, a := range want {
		if !a.Paused {
			continue
		}
		if v, ok := s.paused[id]; !ok || v != hostedVersion(a) {
			s.paused[id] = hostedVersion(a)
			nowPaused = append(nowPaused, id)
		}
	}
	for id := range s.paused {
		if a, ok := want[id]; !ok || !a.Paused {
			delete(s.paused, id)
		}
	}
	var nowRejected []string
	for id, rj := range rejected {
		if s.rejected[id] != rj {
			s.rejected[id] = rj
			nowRejected = append(nowRejected, id)
		}
	}
	for id := range s.rejected {
		if _, ok := rejected[id]; !ok {
			delete(s.rejected, id)
		}
	}
	s.mu.Unlock()

	for _, c := range changes {
		s.stopRunner(c.r, 0)
		if !c.remove {
			continue
		}
		s.mu.Lock()
		holds := c.r.holds
		s.mu.Unlock()
		// The state is written before the lease goes, so that a worker
		// taking the agent up reads a handover, not a lapse; and before
		// the runner goes, so that it names the version of the row the
		// agent ran, over whose state the store writes no older one.
		if c.why != "" && holds {
			s.writeState(ctx, c.r.id, store.AgentStopped, "", c.why)
		}
		s.mu.Lock()
		delete(s.runners, c.r.id)
		c.r.holds = false
		s.mu.Unlock()
		if holds {
			s.releaseLease(c.r.id)
		}
		s.o.Metrics.Forget(c.r.id)
	}
	slices.Sort(nowPaused)
	for _, id := range nowPaused {
		s.o.Metrics.Forget(id)
		s.writeState(ctx, id, store.AgentPaused, "", "paused in the configuration: no call is made to Core for it")
	}
	slices.Sort(nowRejected)
	for _, id := range nowRejected {
		s.o.Metrics.Forget(id)
		s.log.Warn("a hosted agent is not run: its configuration does not pass", "agent", id, "reason", rejected[id].reason)
		s.writeState(ctx, id, store.AgentError, rejected[id].reason, "not run: "+rejected[id].detail)
	}
	slices.SortFunc(rewrite, func(x, y *runner) int { return strings.Compare(x.id, y.id) })
	for _, r := range rewrite {
		s.rewriteState(ctx, r)
	}
	s.updateGauge()
}

// rewriteState writes r's state again, for the version of its hosted row
// now in force. Written by the worker that holds the agent, which has put
// the new version in force: the state it last wrote, as it was.
func (s *Supervisor) rewriteState(ctx context.Context, r *runner) {
	l := s.stateLock(r.id)
	l.Lock()
	defer l.Unlock()
	s.mu.Lock()
	holds, state, reason, detail := r.holds, r.state, r.reason, r.detail
	s.mu.Unlock()
	if holds && state != "" {
		s.writeStateLocked(ctx, r.id, state, reason, detail)
	}
}

// hostedVersion is the version of a hosted agent's row a configuration was
// built from; 0 for a YAML agent.
func hostedVersion(a *config.Agent) int {
	if a == nil || a.Hosted == nil {
		return 0
	}
	return a.Hosted.Version
}

// leaseTick takes or renews the lease of every agent that is not paused,
// starts those it holds that are not running, and stops at once any whose
// lease it lost.
func (s *Supervisor) leaseTick(ctx context.Context) {
	s.mu.Lock()
	rs := make([]*runner, 0, len(s.runners))
	for _, r := range s.runners {
		rs = append(rs, r)
	}
	s.mu.Unlock()
	slices.SortFunc(rs, func(x, y *runner) int {
		switch {
		case x.id < y.id:
			return -1
		case x.id > y.id:
			return 1
		}
		return 0
	})
	for _, r := range rs {
		if ctx.Err() != nil {
			return
		}
		ok, err := s.acquire(ctx, r.id)
		s.mu.Lock()
		held := r.holds
		running := r.agent != nil
		start := !running && !r.blocked && !s.o.Now().Before(r.retryAt)
		if err != nil || !ok {
			r.holds = false
		} else {
			r.holds = true
		}
		s.mu.Unlock()
		if err != nil || !ok {
			if held {
				s.log.Warn("agent lease lost; stopping the agent", "agent", r.id, "err", err)
				s.o.Metrics.Forget(r.id)
			}
			if running {
				s.stopRunner(r, 0)
			}
			continue
		}
		if !held {
			s.checkTakeover(ctx, r.id)
		}
		if start && ctx.Err() == nil {
			s.startRunner(ctx, r)
		}
	}
	s.updateGauge()
}

func leaseName(agentID string) string { return "agent:" + agentID }

// acquire takes or renews the agent's lease. A store that does not answer
// within storeTimeout, or a third of the lease's life if that is less, has
// failed to renew it: the agent is stopped rather than run on while its
// lease may be lapsing, and the ticks of the other agents are not held up.
func (s *Supervisor) acquire(ctx context.Context, id string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, min(storeTimeout, s.o.Timing.LeaseTTL/3))
	defer cancel()
	return s.o.Store.AcquireLease(ctx, leaseName(id), s.o.WorkerID, s.o.Timing.LeaseTTL)
}

// checkTakeover counts a takeover when the agent's last recorded state
// names another worker that did not stop it on purpose: its lease lapsed.
func (s *Supervisor) checkTakeover(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	states, err := s.o.Store.AgentStates(ctx)
	if err != nil {
		return
	}
	for _, st := range states {
		if st.AgentID != id {
			continue
		}
		if st.Worker != "" && st.Worker != s.o.WorkerID && st.State != store.AgentStopped && st.State != store.AgentPaused {
			s.o.Metrics.LeaseTakeovers.Inc()
			s.log.Info("agent taken over from another worker", "agent", id, "from", st.Worker, "its_state", st.State)
		}
		return
	}
}

// startRunner starts an instance of r's agent. Its pollers end with ctx;
// its answers in progress end only when the supervisor says.
func (s *Supervisor) startRunner(ctx context.Context, r *runner) {
	pollCtx, stopPoll := context.WithCancel(ctx)
	answerCtx, stopAnswers := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	ag := newAgent(s, r.cfg)
	done := make(chan struct{})
	r.agent, r.stopPoll, r.stopAnswers, r.done, r.stopping = ag, stopPoll, stopAnswers, done, false
	// A start tried again after a failure keeps the failure's state, and
	// its reason, until it runs or fails again: its owner reads why it
	// does not run, not that it starts, at each try.
	retrying := r.failures > 0
	if !retrying {
		r.state, r.reason, r.detail = store.AgentStarting, "", ""
	}
	s.mu.Unlock()
	if !retrying {
		s.writeState(ctx, r.id, store.AgentStarting, "", "")
	}
	go func() {
		defer close(done)
		err := ag.run(pollCtx, answerCtx)
		stopPoll()
		stopAnswers()
		s.agentEnded(r, ag, err)
	}()
}

// stopRunner stops r's instance, if one runs, and waits for it: its
// pollers at once, its answers in progress after grace.
func (s *Supervisor) stopRunner(r *runner, grace time.Duration) {
	s.mu.Lock()
	if r.agent == nil {
		s.mu.Unlock()
		return
	}
	r.stopping = true
	stopPoll, stopAnswers, done := r.stopPoll, r.stopAnswers, r.done
	s.mu.Unlock()
	stopPoll()
	if grace <= 0 {
		stopAnswers()
	} else {
		t := time.AfterFunc(grace, stopAnswers)
		defer t.Stop()
	}
	<-done
}

// agentEnded records what became of an instance that returned: Core
// refused its token (unauthorized, not started again until a reload), a
// hosted agent's owner is not the one who connected it (owner_changed,
// likewise), it failed (error, started again after a backoff, with why:
// agent_suspended for an agent Core suspended, failing for the rest), or
// it was stopped. An instance of a configuration since replaced (apply
// put a new token or new settings in force while it wound down) records
// nothing: how it ended says nothing of the configuration in force, which
// the next lease tick starts, and whose state is its own to write. Its
// 401 is most often that of the token the new one revoked.
func (s *Supervisor) agentEnded(r *runner, ag *Agent, err error) {
	s.mu.Lock()
	if r.agent != ag {
		s.mu.Unlock()
		return
	}
	r.agent = nil
	stopping := r.stopping
	r.stopping = false
	if !sameRun(ag.cfg, r.cfg) {
		s.mu.Unlock()
		return
	}
	var state, reason, detail string
	var blocked *blockedError
	var owner *OwnerProblem
	switch {
	case isUnauthenticated(err) && r.cfg.Hosted != nil:
		// Its owner gave the token, and gives the next one: no file of
		// the operator's holds it.
		r.blocked = true
		state, reason, detail = store.AgentUnauthorized, store.ReasonTokenRefused,
			"Core refused the agent's token (401): connect the agent again with a new token"
	case isUnauthenticated(err):
		r.blocked = true
		state, reason, detail = store.AgentUnauthorized, store.ReasonTokenRefused,
			"Core refused the agent's token (401): issue a new token for it in Core, put it where core.token_ref points, and reload"
	case errors.As(err, &owner):
		// Stopped until its row changes (its owner connecting it again)
		// or a reload, as for a refused token: started again as it is,
		// it would meet the same owner.
		r.blocked = true
		state, reason, detail = owner.State, owner.Reason, owner.Detail
	case errors.As(err, &blocked):
		r.blocked = true
		state, reason, detail = store.AgentError, blocked.reason, blocked.Error()
	case stopping:
	case err != nil:
		wait := Backoff(r.failures, s.o.Timing.Restart, s.o.Timing.RestartMax, 1)
		r.failures++
		r.retryAt = s.o.Now().Add(wait)
		state, reason, detail = store.AgentError, reasonOf(err), redact.String(err.Error())
	}
	s.mu.Unlock()
	if state == "" {
		return
	}
	switch {
	case state == store.AgentUnauthorized:
		s.log.Warn("agent stopped: Core refused its token", "agent", r.id)
	case owner != nil:
		s.log.Warn("hosted agent stopped: Core does not name as its owner the person who connected it", "agent", r.id, "state", state)
	default:
		s.log.Error("agent failed", "agent", r.id, "err", err)
	}
	s.writeState(context.Background(), r.id, state, reason, detail)
}

// shutdown stops every agent, giving answers in progress the shutdown
// grace, and releases the leases this worker holds.
func (s *Supervisor) shutdown() {
	s.mu.Lock()
	rs := make([]*runner, 0, len(s.runners))
	for _, r := range s.runners {
		rs = append(rs, r)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range rs {
		wg.Go(func() {
			s.stopRunner(r, s.o.Env.ShutdownGrace)
			s.mu.Lock()
			holds := r.holds
			r.holds = false
			s.mu.Unlock()
			if !holds {
				return
			}
			// Written before the lease goes: see apply.
			s.writeState(context.Background(), r.id, store.AgentStopped, "", "the worker stopped")
			s.releaseLease(r.id)
			s.o.Metrics.Forget(r.id)
		})
	}
	wg.Wait()
	s.updateGauge()
}

func (s *Supervisor) releaseLease(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := s.o.Store.ReleaseLease(ctx, leaseName(id), s.o.WorkerID); err != nil {
		s.log.Warn("agent lease not released", "agent", id, "err", err)
	}
}

// storeTimeout bounds the bookkeeping writes made after a call's own
// context may have ended: a state, a lease released, an attempt settled.
const storeTimeout = 5 * time.Second

// writeState records an agent's state for the owner's page, and in the
// status the supervisor reports: the state, why (reason, "" when nothing
// is wrong) and its detail, with the version of a hosted agent's row in
// force here, so that the API tells a change not yet applied from one
// that is.
func (s *Supervisor) writeState(ctx context.Context, id, state, reason, detail string) {
	l := s.stateLock(id)
	l.Lock()
	defer l.Unlock()
	s.writeStateLocked(ctx, id, state, reason, detail)
}

// stateLock is agent id's state lock (stateLocks). It is taken before mu,
// never while mu is held.
func (s *Supervisor) stateLock(id string) *sync.Mutex {
	l, _ := s.stateLocks.LoadOrStore(id, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// writeStateLocked is writeState with id's state lock held.
func (s *Supervisor) writeStateLocked(ctx context.Context, id, state, reason, detail string) {
	detail = redact.String(detail)
	s.mu.Lock()
	if r := s.runners[id]; r != nil {
		r.state, r.reason, r.detail = state, reason, detail
	}
	version := s.configVersion(id)
	s.mu.Unlock()
	s.updateGauge()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	err := s.o.Store.SetAgentState(ctx, store.AgentState{AgentID: id, State: state, Reason: reason, Detail: detail,
		Worker: s.o.WorkerID, ConfigVersion: version, UpdatedAt: s.o.Now()})
	if err != nil {
		s.log.Warn("agent state not recorded", "agent", id, "state", state, "err", err)
	}
}

// configVersion is the version of id's hosted row in force here: its
// runner's, else its as paused, else as rejected; 0 for a YAML agent and
// one this worker does not know. Called with mu held.
func (s *Supervisor) configVersion(id string) int {
	if r := s.runners[id]; r != nil {
		return hostedVersion(r.cfg)
	}
	if v, ok := s.paused[id]; ok {
		return v
	}
	return s.rejected[id].version
}

// setDetail records a running agent's detail, when it changed.
func (s *Supervisor) setDetail(id, detail string) {
	detail = redact.String(detail)
	l := s.stateLock(id)
	l.Lock()
	defer l.Unlock()
	s.mu.Lock()
	r := s.runners[id]
	same := r == nil || r.state == store.AgentRunning && r.detail == detail
	s.mu.Unlock()
	if !same {
		s.writeStateLocked(context.Background(), id, store.AgentRunning, "", detail)
	}
}

// states are those Metrics.AgentStates reports.
var states = []string{store.AgentStarting, store.AgentRunning, store.AgentUnauthorized, store.AgentOwnerChanged, store.AgentError}

// updateGauge sets Metrics.AgentStates: the agents this worker runs, by
// state.
func (s *Supervisor) updateGauge() {
	counts := map[string]int{}
	s.mu.Lock()
	for _, r := range s.runners {
		if r.holds && r.state != "" {
			counts[r.state]++
		}
	}
	s.mu.Unlock()
	for _, st := range states {
		s.o.Metrics.AgentStates.WithLabelValues(st).Set(float64(counts[st]))
	}
}

// claimActor records that agent id runs as the Core actor actorID at
// baseURL, and returns ""; or, when another of this worker's agents runs
// as that actor already (two agents configured with one token), that
// agent's id, and records nothing. The operator's configuration wins over
// the registry's: when id is a YAML agent's and the other is a hosted one,
// id takes the actor, and the hosted agent is returned to be stopped.
func (s *Supervisor) claimActor(baseURL, actorID, id string) (string, *Agent) {
	key := actorKey(baseURL, actorID)
	s.mu.Lock()
	defer s.mu.Unlock()
	other, ok := s.actors[key]
	if !ok || other == id {
		s.actors[key] = id
		return "", nil
	}
	mine, theirs := s.runners[id], s.runners[other]
	if mine != nil && mine.cfg.Hosted == nil && theirs != nil && theirs.cfg.Hosted != nil {
		s.actors[key] = id
		return "", theirs.agent
	}
	return other, nil
}

// actorKey names a Core actor at a Core, whichever way its base URL is
// written: the scheme and host in any case, the scheme's own port given or
// not, and a / at the end or none are one Core, as the Core client calls
// them (it drops the /). Two agents on one actor must meet here however
// their configurations spell its Core, CORE_BASE_URL's hosted agents and
// the operator's YAML among them.
func actorKey(baseURL, actorID string) string {
	return coreOrigin(baseURL) + "\x00" + actorID
}

// coreOrigin is baseURL as actorKey compares it; as written, when it does
// not parse.
func coreOrigin(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return baseURL
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/")
}

// hostedAgent reports whether id is an agent of the registry's.
func (s *Supervisor) hostedAgent(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runners[id]
	return r != nil && r.cfg.Hosted != nil
}

// blockedError stops an agent until its configuration changes, or a
// reload says to try again: what it needs is not in its hands. reason is
// why, as the API names it.
type blockedError struct{ reason, msg string }

func (e *blockedError) Error() string { return e.msg }

// reasonError is a failure that says why, as the API names it: the agent
// is started again after a backoff, as for any failure.
type reasonError struct {
	reason string
	err    error
}

func (e *reasonError) Error() string { return e.err.Error() }

func (e *reasonError) Unwrap() error { return e.err }

// reasonOf is why err stopped an agent: its reasonError's reason, else
// failing.
func reasonOf(err error) string {
	var re *reasonError
	if errors.As(err, &re) {
		return re.reason
	}
	return store.ReasonFailing
}

// ActorAgent says which of this worker's agents runs as the Core actor
// actorID at baseURL, and whether it is a hosted one: ok is false when
// none runs as it here. It knows only the agents this worker has started,
// so a YAML agent another worker runs is not seen: the API asks it as a
// best effort, the operator's configuration winning at the agent's start
// whatever it says.
func (s *Supervisor) ActorAgent(baseURL, actorID string) (agentID string, hosted, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.actors[actorKey(baseURL, strings.ToLower(actorID))]
	if !ok {
		return "", false, false
	}
	r := s.runners[id]
	return id, r != nil && r.cfg.Hosted != nil, true
}

// releaseActor undoes claimActor, when id holds the actor.
func (s *Supervisor) releaseActor(baseURL, actorID, id string) {
	key := actorKey(baseURL, actorID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.actors[key] == id {
		delete(s.actors, key)
	}
}

// catEntry is one base URL's catalogue: fetched by the first agent that
// asks, while the others wait for it.
type catEntry struct {
	done chan struct{}
	cat  *core.Catalogue
	err  error
}

// catalogue is Core's catalogue at baseURL, fetched once per base URL and
// held to the gates the toolset keeps by hand. Its hash is logged, with a
// warning when it is not the one this runtime was built against. A fetch
// that fails is tried again by the next agent that asks.
func (s *Supervisor) catalogue(ctx context.Context, baseURL string) (*core.Catalogue, error) {
	s.mu.Lock()
	e, ok := s.cats[baseURL]
	if !ok {
		e = &catEntry{done: make(chan struct{})}
		s.cats[baseURL] = e
	}
	s.mu.Unlock()
	if ok {
		select {
		case <-e.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if e.err != nil {
			return nil, e.err
		}
		return e.cat, nil
	}
	e.cat, e.err = s.fetchCatalogue(ctx, baseURL)
	if e.err != nil {
		s.mu.Lock()
		if s.cats[baseURL] == e {
			delete(s.cats, baseURL)
		}
		s.mu.Unlock()
	}
	close(e.done)
	return e.cat, e.err
}

func (s *Supervisor) fetchCatalogue(ctx context.Context, baseURL string) (*core.Catalogue, error) {
	cat, err := core.FetchCatalogue(ctx, s.coreHTTP, baseURL)
	if err != nil {
		return nil, err
	}
	if err := toolset.CheckCatalogue(cat); err != nil {
		return nil, fmt.Errorf("worker: the catalogue of Core at %s lacks what the runtime relies on: %w", baseURL, err)
	}
	if cat.Hash() == SnapshotCatalogueHash {
		s.log.Info("Core's catalogue", "core", baseURL, "hash", cat.Hash(), "tools", cat.Len())
	} else {
		s.log.Warn("Core's catalogue is not the one this runtime was built against; its tools still pass the checks",
			"core", baseURL, "hash", cat.Hash(), "snapshot", SnapshotCatalogueHash, "tools", cat.Len())
	}
	return cat, nil
}

// newCaller is the agent's connection to Core: Options.NewCaller's, or the
// default one, which slows the agent's polling on a 429.
func (s *Supervisor) newCaller(a *config.Agent, token string, cat *core.Catalogue) (core.Caller, error) {
	if s.o.NewCaller != nil {
		return s.o.NewCaller(a, token, cat)
	}
	return DefaultCaller(a, token, cat, s.coreHTTP, s.o.Metrics, s.o.CoreRetry, func() { s.slowDown(a.ID) })
}

// slowDown halves the polling of the agent id runs now, for Slowdown.
func (s *Supervisor) slowDown(id string) {
	s.mu.Lock()
	var ag *Agent
	if r := s.runners[id]; r != nil {
		ag = r.agent
	}
	s.mu.Unlock()
	if ag != nil {
		ag.slowDown()
	}
}
