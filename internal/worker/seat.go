package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolset"
)

// Seat is one seat an agent answers in (design §5.2): its toolset, built
// from the seat's perms and the course's configuration; its inbox poller,
// which hands each question waiting to the agent's scheduler; and its events
// poller, which follows proposals, retractions and the opener's writing.
type Seat struct {
	a      *Agent
	id     string // the seat's member_id
	course string
	log    *slog.Logger

	cancel     context.CancelFunc
	done       chan struct{}
	wakeInbox  chan struct{}
	wakeEvents chan struct{}

	mu       sync.Mutex
	m        core.Membership
	eff      *config.Effective
	primary  *model
	fallback *model
	tools    map[toolschema.Dialect]*toolset.Set
	base     string
	appended string

	hotUntil   time.Time
	emptyPolls int
	lastPoll   time.Time
	lastEvents time.Time
	// hold stops inbox polling after Core denied the seat an answer, until
	// me_memberships shows the seat changed.
	hold *seatHold
	// heldBack are conversations not to be answered before a time.
	heldBack map[string]heldBack
	// failures counts, per message, answers whose providers all failed.
	failures map[string]int
	// followUps are when events are read after a proposal.
	followUps []time.Time
}

type seatHold struct {
	since time.Time
	why   string
	seen  core.Membership
}

type heldBack struct {
	until time.Time
	why   string
}

// newSeat makes a seat of a: its model (the course may name its own), its
// prompts, and its toolset for the model's dialect.
func newSeat(ctx context.Context, a *Agent, m core.Membership, eff *config.Effective) (*Seat, error) {
	s := &Seat{
		a: a, id: m.MemberID, course: m.CourseID, log: a.log.With("course", m.CourseID, "member", m.MemberID),
		wakeInbox: make(chan struct{}, 1), wakeEvents: make(chan struct{}, 1),
		m: m, eff: eff, tools: map[toolschema.Dialect]*toolset.Set{},
		heldBack: map[string]heldBack{}, failures: map[string]int{},
	}
	var err error
	if s.primary, s.fallback, err = a.models(ctx, eff.Model); err != nil {
		return nil, err
	}
	s.base = prompt.Builtin(m.AnswersCourse)
	if ref := eff.Prompt.SystemRef; ref != "" {
		if s.base, err = readPrompt(a.cfg.Path(ref)); err != nil {
			return nil, fmt.Errorf("prompt.system_ref: %w", err)
		}
	}
	if ref := eff.PromptAppendRef; ref != "" {
		if s.appended, err = readPrompt(a.cfg.Path(ref)); err != nil {
			return nil, fmt.Errorf("prompt_append_ref: %w", err)
		}
	}
	if _, err := s.toolsFor(s.primary.ad.Dialect()); err != nil {
		return nil, err
	}
	return s, nil
}

// maxPromptBytes bounds a prompt file.
const maxPromptBytes = 256 << 10

func readPrompt(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's configuration names the file.
	if err != nil {
		return "", err
	}
	if len(b) > maxPromptBytes {
		return "", fmt.Errorf("%s is larger than %d bytes", path, maxPromptBytes)
	}
	return string(b), nil
}

// start runs the seat's pollers until stop, or ctx is done.
func (s *Seat) start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	pollers := make(chan struct{}, 2)
	go func() { defer func() { pollers <- struct{}{} }(); s.pollInbox(ctx) }()
	go func() { defer func() { pollers <- struct{}{} }(); s.pollEvents(ctx) }()
	go func() {
		<-pollers
		<-pollers
		close(s.done)
	}()
}

// stop stops the seat's pollers and waits for them. Answers in progress
// go on: they are the agent's.
func (s *Seat) stop() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	<-s.done
}

// update takes the seat as me_memberships shows it now: a toolset built
// again when its perms changed, and a hold lifted when the seat changed
// since it was held.
func (s *Seat) update(m core.Membership, eff *config.Effective) {
	s.mu.Lock()
	if !maps.Equal(s.m.Perms, m.Perms) || !reflect.DeepEqual(s.eff.Tools, eff.Tools) {
		s.tools = map[toolschema.Dialect]*toolset.Set{}
	}
	s.m, s.eff = m, eff
	lifted := s.hold != nil && !reflect.DeepEqual(s.hold.seen, m)
	if lifted {
		s.hold = nil
	}
	s.mu.Unlock()
	if lifted {
		s.log.Info("seat hold lifted: me_memberships shows the seat changed")
		poke(s.wakeInbox)
	}
}

// membership is the seat as last read.
func (s *Seat) membership() core.Membership {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m
}

// config is the course's configuration.
func (s *Seat) config() *config.Effective {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eff
}

// toolsFor is the seat's toolset declared in dialect d, built once per
// dialect while the seat's perms stay the same.
func (s *Seat) toolsFor(d toolschema.Dialect) (*toolset.Set, error) {
	s.mu.Lock()
	set, ok := s.tools[d]
	perms, tools := s.m.Perms, s.eff.Tools
	s.mu.Unlock()
	if ok {
		return set, nil
	}
	set, err := toolset.Build(s.a.cat, perms, tools, d, s.a.s.schemas)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.tools[d] = set
	s.mu.Unlock()
	return set, nil
}

// toolNames are the tools the seat's model is offered.
func (s *Seat) toolNames() []string {
	set, err := s.toolsFor(s.primary.ad.Dialect())
	if err != nil {
		return nil
	}
	return set.Names()
}

// markHot makes the seat's inbox polled at inbox_hot_s for hot_window_s:
// after an answer is posted, and after the opener writes (§7.2).
func (s *Seat) markHot() {
	p := s.config().Polling
	s.mu.Lock()
	s.hotUntil = s.a.now().Add(config.Seconds(p.HotWindowS))
	s.emptyPolls = 0
	s.mu.Unlock()
	poke(s.wakeInbox)
}

// holdSeat stops inbox polling until me_memberships shows the seat
// changed: Core denied it (§2.4).
func (s *Seat) holdSeat(why string) {
	s.mu.Lock()
	fresh := s.hold == nil
	if fresh {
		s.hold = &seatHold{since: s.a.now(), why: why, seen: s.m}
	}
	s.mu.Unlock()
	if fresh {
		s.log.Warn("seat held: its inbox is not polled until me_memberships shows it changed", "why", why)
		s.a.refreshDetail()
	}
	s.a.requestReseat()
}

// held reports whether the seat is held.
func (s *Seat) held() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hold != nil
}

// heldWhy says why the seat is held, for the agent's detail; "" when it
// is not.
func (s *Seat) heldWhy() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hold == nil {
		return ""
	}
	return fmt.Sprintf("the seat %s in %s is held: %s; it waits for its seat in Core to change", s.id, s.m.Code, s.hold.why)
}

// holdBack keeps conversation conv from being answered before until.
func (s *Seat) holdBack(conv string, until time.Time, why string) {
	s.mu.Lock()
	s.heldBack[conv] = heldBack{until: until, why: why}
	s.mu.Unlock()
	s.log.Info("conversation held back", "conversation", conv, "until", until.UTC().Format(time.RFC3339), "why", why)
}

// heldBackNow reports whether conv is held back at now, forgetting holds
// that have passed.
func (s *Seat) heldBackNow(conv string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.heldBack[conv]
	if ok && !now.Before(h.until) {
		delete(s.heldBack, conv)
		return false
	}
	return ok
}

// holdingBack reports whether any conversation is held back now.
func (s *Seat) holdingBack(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conv, h := range s.heldBack {
		if !now.Before(h.until) {
			delete(s.heldBack, conv)
		}
	}
	return len(s.heldBack) > 0
}

// providerFailed counts an answer to msg whose providers all failed, and
// returns how many have, and how long to hold the conversation back: a
// minute doubling to ten (design §5.3 step 7).
func (s *Seat) providerFailed(msg string) (int, time.Duration) {
	t := s.a.s.o.Timing
	s.mu.Lock()
	s.failures[msg]++
	n := s.failures[msg]
	s.mu.Unlock()
	return n, Backoff(n-1, t.HoldBack, t.HoldBackMax, 1)
}

// providerRecovered forgets msg's failures.
func (s *Seat) providerRecovered(msg string) {
	s.mu.Lock()
	delete(s.failures, msg)
	s.mu.Unlock()
}

// followUp reads events at once after a proposal, then 5, 15 and 45
// seconds later (§7.2).
func (s *Seat) followUp() {
	now := s.a.now()
	s.mu.Lock()
	for _, d := range ProposalFollowUps {
		s.followUps = append(s.followUps, now.Add(d))
	}
	sort.Slice(s.followUps, func(i, j int) bool { return s.followUps[i].Before(s.followUps[j]) })
	s.mu.Unlock()
	poke(s.wakeEvents)
}

// polling is the course's polling settings.
func (s *Seat) polling() config.Polling { return s.config().Polling }

// pollInbox asks conversation_inbox for questions waiting, until ctx is
// done: first after a spread of the idle interval, then as InboxInterval
// says (design §5.2). A wake-up (the seat turned hot, a hold lifted)
// reckons the next poll again from the last one.
func (s *Seat) pollInbox(ctx context.Context) {
	due := s.a.now().Add(FirstPoll(s.polling(), s.a.rand()))
	r := s.a.rand()
	for {
		for {
			if s.held() {
				select {
				case <-ctx.Done():
					return
				case <-s.wakeInbox:
				}
				continue
			}
			d := due.Sub(s.a.now())
			if d <= 0 {
				break
			}
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			case <-s.wakeInbox:
				t.Stop()
				if last := s.lastInboxPoll(); !last.IsZero() {
					due = s.nextInbox(last, r)
				}
			}
		}
		s.pollInboxOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		r = s.a.rand()
		due = s.nextInbox(s.lastInboxPoll(), r)
	}
}

func (s *Seat) lastInboxPoll() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPoll
}

// nextInbox is when the inbox is polled next, after a poll at last.
func (s *Seat) nextInbox(last time.Time, r float64) time.Time {
	p := s.polling()
	s.mu.Lock()
	hot := s.a.now().Before(s.hotUntil)
	empty := s.emptyPolls
	s.mu.Unlock()
	floor := RateFloor(p, s.a.answering())
	return last.Add(InboxInterval(p, hot, empty, floor, s.a.slow(), r))
}

// inboxLimit is how many rows a poll asks for; more while conversations
// are held back, so that the rows held do not hide others (§2.3 allows 100).
const (
	inboxLimit     = 20
	inboxLimitHeld = 100
)

// pollInboxOnce polls the inbox once and hands each row that is not held
// back and not being answered to the scheduler. A poll that found a row
// to answer, or one left waiting for a slot, is not an empty poll: only
// empty polls put the next one off (§7.2).
func (s *Seat) pollInboxOnce(ctx context.Context) {
	now := s.a.now()
	limit := inboxLimit
	if s.holdingBack(now) {
		limit = inboxLimitHeld
	}
	rows, err := s.a.client.Inbox(core.WithPriority(ctx, core.PriorityPoll), s.course, limit)
	now = s.a.now()
	s.mu.Lock()
	s.lastPoll = now
	s.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			s.a.s.o.Metrics.InboxPolls.WithLabelValues(s.a.id, "error").Inc()
		}
		s.readFailed(ctx, "conversation_inbox", err)
		return
	}
	perCourse := s.config().Answer.MaxConcurrentPerCourse
	started, waiting := 0, 0
	for _, row := range rows {
		if row.LatestOpenerMessageID == nil || *row.LatestOpenerMessageID == "" || s.heldBackNow(row.ID, now) {
			continue
		}
		if !s.a.sched.tryStart(s.course, row.ID, perCourse) {
			if !s.a.sched.has(row.ID) {
				// No slot for it: it waits for the next poll, which is
				// not to be put off as if the inbox were empty.
				waiting++
			}
			continue
		}
		started++
		s.a.answers.Add(1)
		go func(row core.Conversation) {
			defer s.a.answers.Done()
			s.answer(s.a.answerCtx, row)
		}(row)
	}
	s.mu.Lock()
	if started+waiting == 0 {
		s.emptyPolls++
	} else {
		s.emptyPolls = 0
	}
	s.mu.Unlock()
	result := "empty"
	if started+waiting > 0 {
		result = "work"
	}
	s.a.s.o.Metrics.InboxPolls.WithLabelValues(s.a.id, result).Inc()
}

// readFailed acts on a read of Core's that did not come back executed:
// a 401 stops the agent; denied holds the seat; forbidden and not_found
// have me_memberships read again; anything else is logged, and the next
// poll tries again.
func (s *Seat) readFailed(ctx context.Context, tool string, err error) {
	var ee *core.EnvelopeError
	switch {
	case isUnauthenticated(err):
		s.a.stop(core.ErrUnauthenticated)
	case ctx.Err() != nil:
	case errors.As(err, &ee) && ee.Envelope.Status == core.StatusDenied:
		s.holdSeat("Core denied " + tool + " (" + ee.Envelope.Reason() + ")")
	case errors.As(err, &ee) && (ee.Envelope.Code() == core.CodeForbidden || ee.Envelope.Code() == core.CodeNotFound):
		s.log.Warn("Core refused a read; its seats are read again", "tool", tool, "err", err)
		s.a.requestReseat()
	default:
		s.log.Warn("Core could not be read", "tool", tool, "err", err)
	}
}

// poke wakes whoever waits on ch, without waiting.
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
