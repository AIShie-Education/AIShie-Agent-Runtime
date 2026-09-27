package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/providers"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// Agent is one hosted agent as this worker runs it (design §5.1): its
// connection to Core, its model, and a Seat for each course it answers in.
// It reads me_memberships every memberships_s, and at once when a seat
// meets a refusal that may mean its seat changed. Core refusing its token
// anywhere stops it.
type Agent struct {
	s   *Supervisor
	cfg *config.Agent
	id  string
	log *slog.Logger

	// Set by start, before any goroutine of the agent's reads them.
	client  *core.Client
	cat     *core.Catalogue
	me      *core.Actor
	primary *model
	sched   *scheduler

	fail          context.CancelCauseFunc
	answerCtx     context.Context
	cancelAnswers context.CancelCauseFunc
	answers       sync.WaitGroup
	reseat        chan struct{}
	detailMu      sync.Mutex

	mu        sync.Mutex
	seats     map[string]*Seat
	slowUntil time.Time
	pairs     []modelPair
	lastRead  time.Time
	notice    string
}

// model is an adapter and what the loop needs to know beside it.
type model struct {
	ad        llm.Adapter
	keySource string
	maxOut    int
}

// modelPair is the adapters built for one model section.
type modelPair struct {
	cfg               config.Model
	primary, fallback *model
}

func newAgent(s *Supervisor, cfg *config.Agent) *Agent {
	return &Agent{s: s, cfg: cfg, id: cfg.ID, log: s.log.With("agent", cfg.ID), seats: map[string]*Seat{}, reseat: make(chan struct{}, 1)}
}

func (a *Agent) now() time.Time { return a.s.o.Now() }

func (a *Agent) rand() float64 { return a.s.o.Rand() }

func (a *Agent) store() store.Store { return a.s.o.Store }

// run starts the agent and runs it until pollCtx is done or the agent
// fails, then stops its seats and waits for its answers in progress, which
// end with answerCtx. It returns why the agent failed, core.ErrUnauthenticated
// among them, or nil when it was stopped.
func (a *Agent) run(pollCtx, answerCtx context.Context) error {
	ctx, fail := context.WithCancelCause(pollCtx)
	a.fail = fail
	a.answerCtx, a.cancelAnswers = context.WithCancelCause(answerCtx)
	err := a.start(ctx)
	if err == nil {
		a.s.writeState(ctx, a.id, store.AgentRunning, "")
		a.loop(ctx)
	}
	a.stopSeats()
	a.answers.Wait()
	a.cancelAnswers(nil)
	fail(nil)
	if err == nil {
		a.s.releaseActor(a.cfg.Core.BaseURL, a.me.ID, a.id)
	}
	if err != nil {
		return err
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

// stop stops the agent for err: its pollers and its answers, at once.
func (a *Agent) stop(err error) {
	a.fail(err)
	a.cancelAnswers(err)
}

// start resolves the agent's token, fetches Core's catalogue, connects,
// checks the token with me_get, and builds the model's adapters.
func (a *Agent) start(ctx context.Context) error {
	res := a.s.o.Secrets
	res.BaseDir = a.cfg.Dir
	token, err := res.Resolve(ctx, a.cfg.Core.TokenRef)
	if err != nil {
		return fmt.Errorf("the Core token: %w", err)
	}
	cat, err := a.s.catalogue(ctx, a.cfg.Core.BaseURL)
	if err != nil {
		return err
	}
	caller, err := a.s.newCaller(a.cfg, token, cat)
	if err != nil {
		return err
	}
	client := core.NewClient(authChecked{next: caller})
	me, err := client.Me(core.WithPriority(ctx, core.PriorityBackground))
	if isUnauthenticated(err) {
		return core.ErrUnauthenticated
	}
	if err != nil {
		return fmt.Errorf("me_get: %w", err)
	}
	// One actor in Core is one agent here: two agents on one token would
	// answer every question twice over, and spend its rate limit twice.
	if other := a.s.claimActor(a.cfg.Core.BaseURL, me.ID, a.id); other != "" {
		return fmt.Errorf("its token is agent %q's too: one agent in Core is one agent here, with a token of its own", other)
	}
	primary, _, err := a.models(ctx, a.cfg.Model)
	if err != nil {
		a.s.releaseActor(a.cfg.Core.BaseURL, me.ID, a.id)
		return err
	}
	// Status reads these from another goroutine; the agent's own
	// goroutines are started after.
	a.mu.Lock()
	a.cat, a.client, a.me, a.primary = cat, client, me, primary
	a.sched = newScheduler(a.cfg.Answer.MaxConcurrent)
	a.mu.Unlock()
	a.log.Info("agent started", "actor", me.ID, "catalogue", cat.Hash(), "transport", a.cfg.Core.Transport,
		"adapter", a.primary.ad.Name(), "provider", a.primary.ad.Provider(), "model", a.primary.ad.Model())
	return nil
}

// name is what the agent is called in its prompts: its name in Core.
func (a *Agent) name() string {
	if a.me != nil && a.me.DisplayName != "" {
		return a.me.DisplayName
	}
	return a.cfg.DisplayName
}

// models are the adapters for a model section, and its fallback's, built
// once per agent and section.
func (a *Agent) models(ctx context.Context, m config.Model) (*model, *model, error) {
	a.mu.Lock()
	for _, p := range a.pairs {
		if reflect.DeepEqual(p.cfg, m) {
			a.mu.Unlock()
			return p.primary, p.fallback, nil
		}
	}
	a.mu.Unlock()
	primary, err := a.buildModel(ctx, m)
	if err != nil {
		return nil, nil, fmt.Errorf("the model: %w", err)
	}
	var fallback *model
	if m.Fallback != nil {
		if fallback, err = a.buildModel(ctx, *m.Fallback); err != nil {
			return nil, nil, fmt.Errorf("the fallback model: %w", err)
		}
	}
	a.mu.Lock()
	a.pairs = append(a.pairs, modelPair{cfg: m, primary: primary, fallback: fallback})
	a.mu.Unlock()
	return primary, fallback, nil
}

// buildModel resolves a model's key and builds its adapter.
func (a *Agent) buildModel(ctx context.Context, m config.Model) (*model, error) {
	var key string
	if m.KeyRef != "" {
		res := a.s.o.Secrets
		res.BaseDir = a.cfg.Dir
		var err error
		if key, err = res.Resolve(ctx, m.KeyRef); err != nil {
			return nil, err
		}
	}
	ad, err := a.s.o.NewAdapter(providers.Config(m, key, a.s.o.HTTPClient))
	if err != nil {
		return nil, err
	}
	return &model{ad: ad, keySource: m.KeySource, maxOut: m.Params.MaxOutputTokens}, nil
}

// loop reads me_memberships until ctx is done, and keeps a Seat running for
// each seat the agent answers in.
func (a *Agent) loop(ctx context.Context) {
	for {
		a.readMemberships(ctx)
		if ctx.Err() != nil {
			return
		}
		t := time.NewTimer(a.membershipsWait())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-a.reseat:
			t.Stop()
			// Refusals come in bursts; one read answers them all.
			if !sleep(ctx, a.lastRead.Add(a.minReseat()).Sub(a.now())) {
				return
			}
		}
	}
}

// membershipsWait is the time until the next read of me_memberships:
// memberships_s, or inbox_max_s while no seat answers, so that an agent
// with nothing to poll still reaches Core often enough to show present
// (§2.5); jittered, and doubled while Core asks the agent to slow down.
func (a *Agent) membershipsWait() time.Duration {
	p := a.cfg.Polling
	d := config.Seconds(p.MembershipsS)
	if a.answering() == 0 && p.InboxMaxS < p.MembershipsS {
		d = config.Seconds(p.InboxMaxS)
	}
	if a.slow() {
		d *= 2
	}
	return Jitter(d, p.Jitter, a.rand())
}

// minReseat is the least time between two reads of me_memberships asked
// for by refusals.
func (a *Agent) minReseat() time.Duration { return config.Seconds(a.cfg.Polling.InboxHotS) }

// requestReseat asks for me_memberships to be read at once.
func (a *Agent) requestReseat() {
	select {
	case a.reseat <- struct{}{}:
	default:
	}
}

// readMemberships reads the agent's seats and reconciles its Seats with
// them.
func (a *Agent) readMemberships(ctx context.Context) {
	ms, err := a.client.Memberships(core.WithPriority(ctx, core.PriorityBackground))
	a.mu.Lock()
	a.lastRead = a.now()
	a.mu.Unlock()
	switch {
	case isUnauthenticated(err):
		a.stop(core.ErrUnauthenticated)
		return
	case err != nil:
		if ctx.Err() == nil {
			a.log.Warn("me_memberships failed", "err", err)
		}
		return
	}
	a.reconcile(ctx, ms)
}

// reconcile starts a Seat for each seat the agent answers in, updates those
// running, and stops those that left or stopped answering. Every seat in
// me_memberships is recorded as current; one the store knows that is not
// there any more is recorded gone, and its memory purged once
// retention_days_after_removal have passed (§2.5).
func (a *Agent) reconcile(ctx context.Context, ms []core.Membership) {
	now := a.now()
	current := make(map[string]core.Membership, len(ms))
	for _, m := range ms {
		current[m.MemberID] = m
		if err := a.store().SeatSeen(ctx, a.id, m.MemberID, m.CourseID, now); err != nil && ctx.Err() == nil {
			a.log.Warn("seat not recorded", "member", m.MemberID, "err", err)
		}
	}
	known, err := a.store().KnownSeats(ctx, a.id)
	if err != nil && ctx.Err() == nil {
		a.log.Warn("seats not read", "err", err)
	}
	for _, k := range known {
		if _, ok := current[k.MemberID]; !ok && k.GoneAt == nil {
			if err := a.store().SeatGone(ctx, a.id, k.MemberID, now); err != nil && ctx.Err() == nil {
				a.log.Warn("seat not recorded gone", "member", k.MemberID, "err", err)
			}
			a.log.Info("seat gone from me_memberships", "member", k.MemberID, "course", k.CourseID)
		}
	}

	a.mu.Lock()
	running := make(map[string]*Seat, len(a.seats))
	for id, s := range a.seats {
		running[id] = s
	}
	a.mu.Unlock()
	for id, s := range running {
		m, ok := current[id]
		eff, answers := a.answersIn(m)
		if ok && answers {
			s.update(m, eff)
			continue
		}
		a.mu.Lock()
		delete(a.seats, id)
		a.mu.Unlock()
		s.stop()
		why := "it left me_memberships"
		if ok {
			why = "it does not answer now (" + whyNotAnswering(m, eff) + ")"
		}
		a.log.Info("seat stopped: "+why, "member", id, "course", s.course)
	}
	for id, m := range current {
		if _, ok := running[id]; ok {
			continue
		}
		eff, answers := a.answersIn(m)
		if !answers {
			continue
		}
		s, err := newSeat(ctx, a, m, eff)
		if err != nil {
			if ctx.Err() == nil {
				a.log.Error("seat not started", "member", id, "course", m.CourseID, "err", err)
			}
			continue
		}
		a.mu.Lock()
		a.seats[id] = s
		a.mu.Unlock()
		s.start(ctx)
		a.log.Info("seat started", "member", id, "course", m.CourseID, "answers_course", m.AnswersCourse,
			"level", m.Level("conversation_answer"), "tools", len(s.toolNames()))
	}
	a.refreshDetail()
}

// answersIn reports whether the agent answers in the seat m, and the
// configuration for its course: an active seat, of a course not archived,
// whose conversation_answer is not denied (§2.3), in a course the
// configuration does not disable.
func (a *Agent) answersIn(m core.Membership) (*config.Effective, bool) {
	if m.MemberID == "" || !m.Answers() {
		return nil, false
	}
	eff, err := a.cfg.ForCourse(m.CourseID)
	if err != nil {
		a.log.Error("the course's configuration is not usable", "course", m.CourseID, "err", err)
		return nil, false
	}
	return eff, eff.Enabled
}

func whyNotAnswering(m core.Membership, eff *config.Effective) string {
	switch {
	case m.Status != "active":
		return "the seat is " + m.Status
	case m.CourseStatus == "archived":
		return "the course is archived"
	case m.Level("conversation_answer") == core.LevelDenied:
		return "conversation_answer is denied"
	case eff != nil && !eff.Enabled:
		return "the course is disabled in the configuration"
	}
	return "its course's configuration is not usable"
}

// stopSeats stops every seat and waits for their pollers.
func (a *Agent) stopSeats() {
	a.mu.Lock()
	seats := make([]*Seat, 0, len(a.seats))
	for _, s := range a.seats {
		seats = append(seats, s)
	}
	a.seats = map[string]*Seat{}
	a.mu.Unlock()
	for _, s := range seats {
		s.stop()
	}
}

// answering is how many seats the agent answers in now: the courses its
// polling is shared between (§7.3).
func (a *Agent) answering() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.seats)
}

// slowDown halves the agent's polling for Slowdown, after a 429 (§7.2).
func (a *Agent) slowDown() {
	a.mu.Lock()
	was := a.now().Before(a.slowUntil)
	a.slowUntil = a.now().Add(Slowdown)
	a.mu.Unlock()
	if !was {
		a.log.Warn("Core asked the agent to slow down (429): its polling is halved", "for", Slowdown)
	}
}

// slow reports whether the agent's polling is halved.
func (a *Agent) slow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.now().Before(a.slowUntil)
}

// tell records something the owner should know on the agent's detail.
func (a *Agent) tell(notice string) {
	a.mu.Lock()
	a.notice = notice
	a.mu.Unlock()
	a.refreshDetail()
}

// refreshDetail writes the agent's detail: the seats held and why, and the
// last notice for the owner.
func (a *Agent) refreshDetail() {
	// One at a time, so that a detail reckoned before a change is never
	// written after one reckoned since.
	a.detailMu.Lock()
	defer a.detailMu.Unlock()
	a.mu.Lock()
	var parts []string
	for _, s := range a.seats {
		if why := s.heldWhy(); why != "" {
			parts = append(parts, why)
		}
	}
	slices.Sort(parts)
	if a.notice != "" {
		parts = append(parts, a.notice)
	}
	a.mu.Unlock()
	a.s.setDetail(a.id, strings.Join(parts, "; "))
}

// sleep waits d, or until ctx is done, which it reports as false.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
