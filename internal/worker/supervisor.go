package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolset"
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

	kick    chan struct{}
	running atomic.Bool
	prices  atomic.Pointer[pricing.Table]

	mu      sync.Mutex
	cfg     *config.Config
	pending *config.Config
	runners map[string]*runner
	// paused are the paused agents, whose state has been written.
	paused map[string]bool
	// cats are Core's catalogues, fetched once per base URL.
	cats map[string]*catEntry
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
	detail   string
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
		files: toolset.NewHTTPFetcher(o.HTTPClient), schemas: toolschema.NewCache(),
		kick: make(chan struct{}, 1), runners: map[string]*runner{}, paused: map[string]bool{},
		cats: map[string]*catEntry{}, pending: o.Config,
	}
	s.prices.Store(o.Prices)
	return s, nil
}

// SetPrices replaces the price table the next model calls are costed by;
// nil leaves their costs unknown.
func (s *Supervisor) SetPrices(t *pricing.Table) { s.prices.Store(t) }

// priceTable is the price table in force.
func (s *Supervisor) priceTable() *pricing.Table { return s.prices.Load() }

// Reload replaces the configuration: agents added, removed, paused or
// changed are started, stopped or restarted, and the runtime's settings
// (tenants) apply to the next answer. An agent stopped because Core refused
// its token is started again, whether or not its configuration changed:
// a reload is how an owner says a new token is in place. It returns at
// once; the running supervisor applies it.
func (s *Supervisor) Reload(cfg *config.Config) {
	if cfg == nil {
		return
	}
	s.mu.Lock()
	s.pending = cfg
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

// apply puts a pending configuration in force.
func (s *Supervisor) apply(ctx context.Context) {
	s.mu.Lock()
	cfg := s.pending
	s.pending = nil
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
	for id, r := range s.runners {
		a, ok := want[id]
		switch {
		case !ok:
			changes = append(changes, change{r, true, "removed from the configuration"})
		case a.Paused:
			changes = append(changes, change{r, true, ""})
		case !reflect.DeepEqual(r.cfg, a):
			changes = append(changes, change{r: r})
			r.cfg, r.blocked, r.failures, r.retryAt = a, false, 0, time.Time{}
		case r.blocked || r.state == store.AgentError:
			r.blocked, r.failures, r.retryAt = false, 0, time.Time{}
		}
	}
	for id, a := range want {
		if _, ok := s.runners[id]; !ok && !a.Paused {
			s.runners[id] = &runner{id: id, cfg: a}
		}
	}
	var nowPaused []string
	for id, a := range want {
		if a.Paused && !s.paused[id] {
			s.paused[id] = true
			nowPaused = append(nowPaused, id)
		}
	}
	for id := range s.paused {
		if a, ok := want[id]; !ok || !a.Paused {
			delete(s.paused, id)
		}
	}
	s.mu.Unlock()

	for _, c := range changes {
		s.stopRunner(c.r, 0)
		if !c.remove {
			continue
		}
		s.mu.Lock()
		delete(s.runners, c.r.id)
		holds := c.r.holds
		c.r.holds = false
		s.mu.Unlock()
		if holds {
			s.releaseLease(c.r.id)
		}
		if c.why != "" && holds {
			s.writeState(ctx, c.r.id, store.AgentStopped, c.why)
		}
		s.o.Metrics.Forget(c.r.id)
	}
	slices.Sort(nowPaused)
	for _, id := range nowPaused {
		s.o.Metrics.Forget(id)
		s.writeState(ctx, id, store.AgentPaused, "paused in the configuration: no call is made to Core for it")
	}
	s.updateGauge()
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
		ok, err := s.o.Store.AcquireLease(ctx, leaseName(r.id), s.o.WorkerID, s.o.Timing.LeaseTTL)
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

// checkTakeover counts a takeover when the agent's last recorded state
// names another worker that did not stop it on purpose: its lease lapsed.
func (s *Supervisor) checkTakeover(ctx context.Context, id string) {
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
	r.state, r.detail = store.AgentStarting, ""
	s.mu.Unlock()
	s.writeState(ctx, r.id, store.AgentStarting, "")
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
// refused its token (unauthorized, not started again until a reload), it
// failed (error, started again after a backoff), or it was stopped.
func (s *Supervisor) agentEnded(r *runner, ag *Agent, err error) {
	s.mu.Lock()
	if r.agent != ag {
		s.mu.Unlock()
		return
	}
	r.agent = nil
	stopping := r.stopping
	r.stopping = false
	var state, detail string
	switch {
	case isUnauthenticated(err):
		r.blocked = true
		state, detail = store.AgentUnauthorized,
			"Core refused the agent's token (401): issue a new token for it in Core, put it where core.token_ref points, and reload"
	case stopping:
	case err != nil:
		wait := Backoff(r.failures, s.o.Timing.Restart, s.o.Timing.RestartMax, 1)
		r.failures++
		r.retryAt = s.o.Now().Add(wait)
		state, detail = store.AgentError, redact.String(err.Error())
	}
	s.mu.Unlock()
	if state == "" {
		return
	}
	if state == store.AgentUnauthorized {
		s.log.Warn("agent stopped: Core refused its token", "agent", r.id)
	} else {
		s.log.Error("agent failed", "agent", r.id, "err", err)
	}
	s.writeState(context.Background(), r.id, state, detail)
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
			s.releaseLease(r.id)
			s.o.Metrics.Forget(r.id)
			s.writeState(context.Background(), r.id, store.AgentStopped, "the worker stopped")
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
// status the supervisor reports.
func (s *Supervisor) writeState(ctx context.Context, id, state, detail string) {
	detail = redact.String(detail)
	s.mu.Lock()
	if r := s.runners[id]; r != nil {
		r.state, r.detail = state, detail
	}
	s.mu.Unlock()
	s.updateGauge()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	err := s.o.Store.SetAgentState(ctx, store.AgentState{AgentID: id, State: state, Detail: detail, Worker: s.o.WorkerID, UpdatedAt: s.o.Now()})
	if err != nil {
		s.log.Warn("agent state not recorded", "agent", id, "state", state, "err", err)
	}
}

// setDetail records a running agent's detail, when it changed.
func (s *Supervisor) setDetail(id, detail string) {
	detail = redact.String(detail)
	s.mu.Lock()
	r := s.runners[id]
	same := r == nil || r.state == store.AgentRunning && r.detail == detail
	s.mu.Unlock()
	if !same {
		s.writeState(context.Background(), id, store.AgentRunning, detail)
	}
}

// states are those Metrics.AgentStates reports.
var states = []string{store.AgentStarting, store.AgentRunning, store.AgentUnauthorized, store.AgentError}

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
