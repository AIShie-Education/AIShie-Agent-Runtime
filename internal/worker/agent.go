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
	"sync/atomic"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/providers"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
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
	// inboxMaxWait and eventsMaxWait are how long Core lets a call of
	// conversation_inbox and of event_list wait for news, as the catalogue
	// it serves says (Catalogue.MaxWait): 0 against a Core from before
	// wait_s, which is polled on the schedule alone (longpoll.go).
	inboxMaxWait, eventsMaxWait time.Duration
	// drafts is whether Core takes the drafts of answers being written,
	// as the catalogue it serves says (Catalogue.Drafts): nothing is sent
	// to a Core without conversation_draft.
	drafts bool

	// drafters are the conversations' drafters sending now (draft.go),
	// and draftCounts what came of their writes.
	draftMu     sync.Mutex
	drafters    map[string]*drafter
	draftCounts struct{ sent, dropped, failed atomic.Int64 }

	// underWay are the answers being written now, by conversation, for a
	// withdrawal of their question to stop, and retracted the messages
	// whose retraction was read lately, by id, when (withdraw.go).
	wayMu     sync.Mutex
	underWay  map[string]*underWay
	retracted map[string]time.Time

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
	// heldSecret is the store's secret of the token the agent runs with,
	// "" for one kept in memory: what a 401 forgets of an operator's
	// agent, and nothing newer.
	heldSecret string
	// longPolls and eventLongPolls are the agent's inbox and event calls
	// waiting for news now, at most polling.long_poll_max together
	// (takeLongPoll).
	longPolls, eventLongPolls int
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
		a.s.writeState(ctx, a.id, store.AgentRunning, "", "")
		a.loop(ctx)
	}
	a.stopSeats()
	a.answers.Wait()
	a.cancelAnswers(nil)
	fail(nil)
	if err == nil {
		a.s.releaseActor(a.cfg.Core.BaseURL, agentID(a.cfg), a.id)
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

// start fetches Core's catalogue, claims the agent's Core actor, reads the
// agent as Core hosts it (host), has its token (the one the runtime keeps
// for it, or one issued now by its id), connects, checks the token with
// me_get, and builds the model's adapters. The claim is let go when it
// fails.
func (a *Agent) start(ctx context.Context) (err error) {
	cat, err := a.s.catalogue(ctx, a.cfg.Core.BaseURL)
	if err != nil {
		return err
	}
	rs, err := a.s.runtime(a.cfg.Core.BaseURL, cat)
	if err != nil {
		return err
	}
	// One actor in Core is one agent here: two agents on one actor would
	// answer every question twice over, and each be issued its token,
	// revoking the other's. It is claimed before the agent is hosted, so
	// that an agent the operator's configuration runs is never issued a
	// token by a hosted agent on its actor.
	if err := a.claim(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			a.s.releaseActor(a.cfg.Core.BaseURL, agentID(a.cfg), a.id)
		}
	}()
	if _, err := a.host(ctx, rs); err != nil {
		return err
	}
	token, err := a.token(ctx, rs)
	if err != nil {
		return err
	}
	caller, err := a.s.newCaller(a.cfg, token, cat)
	if err != nil {
		return err
	}
	client := core.NewClient(authChecked{next: caller})
	me, err := client.Me(core.WithPriority(ctx, core.PriorityBackground))
	switch {
	case isUnauthenticated(err):
		return core.ErrUnauthenticated
	case isSuspended(err):
		return errSuspended
	case err != nil:
		return fmt.Errorf("me_get: %w", err)
	case me.Status != "" && me.Status != core.StatusActive:
		return errSuspended
	case !strings.EqualFold(me.ID, agentID(a.cfg)):
		// Never so: Core issued the token for the agent of this id.
		return &blockedError{reason: store.ReasonTokenOtherAgent,
			msg: "Core names another actor than core.agent_id for the token it issued the runtime for it"}
	}
	primary, _, err := a.models(ctx, a.cfg.Model)
	if err != nil {
		return err
	}
	// Status reads these from another goroutine; the agent's own
	// goroutines are started after.
	a.mu.Lock()
	a.cat, a.client, a.me, a.primary = cat, client, me, primary
	a.sched = newScheduler(a.cfg.Answer.MaxConcurrent)
	a.inboxMaxWait, a.eventsMaxWait = cat.MaxWait("conversation_inbox"), cat.MaxWait("event_list")
	a.drafts = cat.Drafts()
	a.mu.Unlock()
	a.log.Info("agent started", "actor", me.ID, "catalogue", cat.Hash(), "transport", a.cfg.Core.Transport,
		"adapter", a.primary.ad.Name(), "provider", a.primary.ad.Provider(), "model", a.primary.ad.Model(),
		"long_poll", longPollWait(a.cfg.Polling, a.inboxMaxWait) > 0 && a.cfg.Polling.LongPollMax > 0, "drafts", a.drafts)
	return nil
}

// claim claims the agent's Core actor for it (Supervisor.claimActor): a
// hosted agent on an actor the operator's configuration runs is stopped
// until its configuration changes, and stops the other the other way
// round; two of the configuration's are one in use.
func (a *Agent) claim() error {
	other, preempted := a.s.claimActor(a.cfg.Core.BaseURL, agentID(a.cfg), a.id)
	switch {
	case other != "" && a.cfg.Hosted != nil && !a.s.hostedAgent(other):
		return &blockedError{reason: store.ReasonOperatorAgent,
			msg: fmt.Sprintf("its Core actor runs here as agent %q, of the operator's configuration, which wins", other)}
	case other != "":
		return &reasonError{reason: store.ReasonActorInUse,
			err: fmt.Errorf("its Core actor is agent %q's too: one agent in Core is one agent here", other)}
	}
	if preempted != nil {
		a.log.Warn("a hosted agent ran as this agent's Core actor; the operator's configuration wins, and it is stopped", "hosted", preempted.id)
		preempted.stop(&blockedError{reason: store.ReasonOperatorAgent,
			msg: fmt.Sprintf("its Core actor runs here as agent %q, of the operator's configuration, which wins", a.id)})
	}
	return nil
}

// errSuspended stops an agent Core has suspended: every call it makes is
// denied, me_get's among them, and its hosting ends (its token revoked).
// It is a failure like any other, tried again after a backoff, so that the
// agent is hosted again by itself once it is reactivated.
var errSuspended = &reasonError{reason: store.ReasonAgentSuspended,
	err: errors.New("the agent is suspended in Core: its token is revoked, and it is hosted again by itself once it is reactivated")}

// reasonActorNotActive is the reason Core's authorization gives for a
// call of an actor that is not active.
const reasonActorNotActive = "actor_not_active"

// isSuspended reports whether err is Core denying a call because the
// actor who made it is not active.
func isSuspended(err error) bool {
	var ee *core.EnvelopeError
	return errors.As(err, &ee) && ee.Envelope.Status == core.StatusDenied && ee.Envelope.Reason() == reasonActorNotActive
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

// buildModel resolves a model's key and builds its adapter: a hosted
// agent's over the hosted-model client (Options.HostedHTTPClient), which
// connects to public addresses alone and follows no redirect, but for an
// offer of runtime.yaml's plan, whose endpoint is the operator's, as a
// YAML agent's is (config.Agent.OverHostedClient).
func (a *Agent) buildModel(ctx context.Context, m config.Model) (*model, error) {
	var key string
	if m.KeyRef != "" {
		var err error
		if key, err = a.s.o.Secrets.Resolve(ctx, m.KeyRef, a.cfg.Dir); err != nil {
			return nil, err
		}
	}
	client := a.s.o.HTTPClient
	if a.cfg.OverHostedClient(m) {
		client = a.s.o.HostedHTTPClient
	}
	ad, err := a.s.o.NewAdapter(providers.Config(m, key, client))
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
	case isSuspended(err):
		// Suspended while it ran: every call it makes is denied until it
		// is reactivated, which its next start finds out.
		a.stop(errSuspended)
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
// me_memberships is recorded as current, as it now is; one the store knows
// that is not there any more is recorded gone, and its memory purged once
// retention_days_after_removal have passed (§2.5).
func (a *Agent) reconcile(ctx context.Context, ms []core.Membership) {
	now := a.now()
	current := make(map[string]core.Membership, len(ms))
	for _, m := range ms {
		current[m.MemberID] = m
		if err := a.store().SeatSeen(ctx, SeatSnapshot(a.id, m, now)); err != nil && ctx.Err() == nil {
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
			"level", m.Level("conversation_answer"), "tools", len(s.toolNames(toolset.ReadOnly)), "owner_writes", len(s.ownerWrites()))
	}
	a.refreshDetail()
}

// SeatSnapshot is the seat m of agent agentID as the store keeps it: what
// me_memberships says of it at at, so that the API can show it without the
// agent's token. The API records a newly connected agent's seats with it.
func SeatSnapshot(agentID string, m core.Membership, at time.Time) store.SeatRef {
	r := store.SeatRef{
		AgentID: agentID, MemberID: m.MemberID, CourseID: m.CourseID, CourseCode: m.Code, CourseTitle: m.Title,
		Section: m.Section, Status: m.Status, CourseStatus: m.CourseStatus, AnswersCourse: m.AnswersCourse, Perms: m.Perms, SeenAt: at,
	}
	if m.PrincipalMemberID != nil {
		r.PrincipalMemberID = *m.PrincipalMemberID
	}
	return r
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
