package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

// callerFunc is a core.Caller of a function.
type callerFunc func(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error)

func (f callerFunc) Call(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
	return f(ctx, tool, args)
}

// wrapCaller has the worker's agents connect as by default, through wrap.
func wrapCaller(wrap func(next core.Caller) core.Caller) func(*Options) {
	return func(o *Options) {
		client, m, retry := &http.Client{Timeout: 5 * time.Second}, o.Metrics, o.CoreRetry
		o.NewCaller = func(a *config.Agent, token string, cat *core.Catalogue) (core.Caller, error) {
			c, err := DefaultCaller(a, token, cat, client, m, retry, nil)
			if err != nil {
				return nil, err
			}
			return wrap(c), nil
		}
	}
}

// countCalls has the worker's agents connect as by default, or as an
// earlier edit has them, counting in began the calls the agent id begins,
// as it begins them: unlike the fake Core's log, which has a call when it
// ends, it counts nothing that was in flight when the agent stopped.
func countCalls(id string, began *atomic.Int32) func(*Options) {
	return func(o *Options) {
		if o.NewCaller == nil {
			wrapCaller(func(next core.Caller) core.Caller { return next })(o)
		}
		connect := o.NewCaller
		o.NewCaller = func(a *config.Agent, token string, cat *core.Catalogue) (core.Caller, error) {
			c, err := connect(a, token, cat)
			if err != nil || a.ID != id {
				return c, err
			}
			return callerFunc(func(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
				began.Add(1)
				return c.Call(ctx, tool, args)
			}), nil
		}
	}
}

// leaseTicks is a store that counts each agent's lease ticks: the
// supervisor takes or renews the lease of every agent it has a runner
// for, every Timing.LeaseEvery, and starts the agent then if it is not
// running and may be. An agent Core stopped (a refused token) keeps its
// runner and its lease, so its ticks go on, each one a time it was not
// started again: "nothing more" for it is held over ten of them, which a
// busy machine makes later, not fewer.
type leaseTicks struct {
	store.Store
	mu    sync.Mutex
	ticks map[string]int
}

func (s *leaseTicks) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if id, ok := strings.CutPrefix(name, "agent:"); ok {
		s.mu.Lock()
		if s.ticks == nil {
			s.ticks = map[string]int{}
		}
		s.ticks[id]++
		s.mu.Unlock()
	}
	return s.Store.AcquireLease(ctx, name, holder, ttl)
}

// of is how many lease ticks the agent id has had.
func (s *leaseTicks) of(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ticks[id]
}

// begun reads the sum of counters, as countCalls counts.
func begun(counters ...*atomic.Int32) func() int {
	return func() int {
		n := 0
		for _, c := range counters {
			n += int(c.Load())
		}
		return n
	}
}

// noMoreOver fails the test if what began a call to Core (began) while
// ten more of the events counted by count happened: "nothing more" held
// over events that keep happening, as CONTRIBUTING's "Tests and time" has
// it, not over a time. It is called once what it watches has stopped, its
// calls returned; it fails as well if began counted none at all.
func noMoreOver(t *testing.T, what string, began func() int, events string, count func() int) {
	t.Helper()
	n, from := began(), count()
	if n == 0 {
		t.Fatalf("none of the calls of %s was counted", what)
	}
	eventually(t, "ten more "+events, func() bool { return count() >= from+10 })
	if more := began() - n; more != 0 {
		t.Errorf("%d calls to Core begun by %s over ten %s", more, what, events)
	}
}

// TestDeniedStopsPolling: the seat's level set to denied; Core denies its
// inbox, the seat is held and its seats read again, and it polls its inbox
// no more until its level is back; then it answers. On the schedule: a
// long poll waiting when the level changes, which is no news to it, would
// be cancelled by the seat stopping, me_memberships having shown it
// denied, before Core denied it.
//
// Core denies the inbox before me_memberships shows the seat denied, as a
// step that must come first is made to: the reads of me_memberships under
// way are let come back before the level changes, and every read begun
// from then on waits for the denial. The polls are counted as
// the seat begins them, on its connection to Core, and "no more" is over
// ten reads of me_memberships the agent, still running, begins after the
// denial, not over a time, in which a busy machine would poll less.
func TestDeniedStopsPolling(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Reply("Back again."))
	var (
		// gated is set before the level changes; ungated counts the
		// reads of me_memberships under way that began before.
		mu      sync.Mutex
		gated   bool
		ungated int
		// denied is closed as Core's denial of the inbox comes back.
		denied = make(chan struct{})
		once   sync.Once
		// polls counts the inbox polls begun; after, those begun after
		// the denial; reads, the reads of me_memberships begun after it.
		polls, after, reads atomic.Int32
	)
	isDenied := func() bool {
		select {
		case <-denied:
			return true
		default:
			return false
		}
	}
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", map[string]any{"polling": onSchedule(nil)}, nil)), models{"m1": model}, workerOpts{
		edit: wrapCaller(func(next core.Caller) core.Caller {
			return callerFunc(func(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
				switch tool {
				case "conversation_inbox":
					polls.Add(1)
					if isDenied() {
						after.Add(1)
					}
					env, err := next.Call(ctx, tool, args)
					if err == nil && env.Status == core.StatusDenied {
						// The seat's one poller of its inbox begins its
						// next poll after this one has come back.
						once.Do(func() { close(denied) })
					}
					return env, err
				case "me_memberships":
					mu.Lock()
					wait := gated
					if !wait {
						ungated++
					}
					mu.Unlock()
					if !wait {
						defer func() { mu.Lock(); ungated--; mu.Unlock() }()
						break
					}
					select {
					case <-denied:
						reads.Add(1)
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return next.Call(ctx, tool, args)
			})
		}),
	})
	eventually(t, "the inbox polled", func() bool { return polls.Load() > 0 })

	mu.Lock()
	gated = true
	mu.Unlock()
	eventually(t, "the reads of me_memberships under way to come back", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return ungated == 0
	})
	w.ok(w.fc.SetLevel(own.seat.ID, "conversation_answer", "denied"))
	eventually(t, "Core denying the inbox", isDenied)
	eventually(t, "ten reads of me_memberships after the denial", func() bool { return reads.Load() >= 10 })
	if more := after.Load(); more != 0 {
		t.Errorf("the inbox was polled %d times after Core denied it", more)
	}

	w.ok(w.fc.SetLevel(own.seat.ID, "conversation_answer", "autonomous"))
	conv, _ := w.ask(0, own, "Are you back?")
	w.waitAnswers(conv, 1)
}

// TestHoldLiftsWhenTheSeatChanges: Core denies the seat's inbox while
// me_memberships still shows it answering; the seat is held until
// me_memberships shows it changed.
func TestHoldLiftsWhenTheSeatChanges(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	var deny atomic.Bool
	var polls atomic.Int32
	deny.Store(true)
	model := scripted.New(scripted.Reply("Answered after the hold."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{
		edit: wrapCaller(func(next core.Caller) core.Caller {
			return callerFunc(func(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
				if tool == "conversation_inbox" {
					polls.Add(1)
					if deny.Load() {
						return &core.Envelope{Status: core.StatusDenied, Error: &core.Error{Code: core.CodeForbidden, Details: map[string]any{"reason": "level_denied"}}}, nil
					}
				}
				return next.Call(ctx, tool, args)
			})
		}),
	})
	eventually(t, "the seat held", func() bool {
		st := wk.sup.Status()
		return len(st) == 1 && len(st[0].Seats) == 1 && st[0].Seats[0].Held
	})
	eventually(t, "the agent's detail saying the seat is held", func() bool {
		return strings.Contains(wk.state("yuki-helper").Detail, "held")
	})
	n := polls.Load()
	time.Sleep(400 * time.Millisecond)
	if more := polls.Load() - n; more != 0 {
		t.Errorf("the inbox was polled %d times while the seat was held", more)
	}

	deny.Store(false)
	w.ok(w.fc.SetLevel(own.seat.ID, "grade_read", "denied")) // the seat changes
	conv, _ := w.ask(0, own, "Are you back?")
	w.waitAnswers(conv, 1)
	eventually(t, "the agent's detail no longer saying the seat is held", func() bool {
		return !strings.Contains(wk.state("yuki-helper").Detail, "held")
	})
}

// TestUnauthorizedStopsTheAgent: Core refuses the token (401): it was
// revoked in Core, by the agent's owner or an administrator. The agent
// stops, its state says so, it makes no more calls, and the runtime
// forgets the token; a reload is issued another, and runs the agent.
// Its calls are counted as it begins them, on its connection to Core, and
// "no more" is over ten lease ticks (leaseTicks), not over a time: the
// state is written once the agent's calls have returned, but the fake Core
// logs a long poll the stop cancelled as it ends, on a busy machine after.
func TestUnauthorizedStopsTheAgent(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	st := &leaseTicks{Store: memstore.New()}
	var began atomic.Int32
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": scripted.New()},
		workerOpts{store: st, edit: countCalls("yuki-helper", &began)})
	wk.waitState("yuki-helper", store.AgentRunning)
	eventually(t, "the inbox polled", func() bool { return len(w.calls(own.actor.ID, "conversation_inbox")) > 0 })
	first := w.fc.RuntimeToken(own.actor.ID)

	_, err := w.fc.RevokeRuntimeToken(own.actor.ID)
	w.ok(err)
	state := wk.waitState("yuki-helper", store.AgentUnauthorized)
	if !strings.Contains(state.Detail, "token") || !strings.Contains(state.Detail, "SIGHUP") {
		t.Errorf("detail %q", state.Detail)
	}
	noMoreOver(t, "the stopped agent", begun(&began), "lease ticks", func() int { return st.of("yuki-helper") })
	if got := counter(t, wk.reg, "agents", map[string]string{"state": store.AgentUnauthorized}); got != 1 {
		t.Errorf("agents{state=unauthorized} = %v", got)
	}
	if issues := w.fc.RuntimeIssues(own.actor.ID); issues != 2 {
		t.Errorf("Core issued the agent's token %d times before the reload; want 2 (its creation's and the worker's)", issues)
	}

	// A reload is issued another token, and starts it again.
	wk.sup.Reload(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)))
	wk.waitState("yuki-helper", store.AgentRunning)
	if again := w.fc.RuntimeToken(own.actor.ID); again.Token == "" || again.CredentialID == first.CredentialID {
		t.Errorf("the reload was not issued another token: %+v", again)
	}
}

// TestUnauthenticatedEnvelopeStopsTheAgent: over MCP, Core answers a call
// whose actor no longer exists with an envelope of status error and code
// unauthenticated; that is a 401 too. The calls after it are counted as
// the agent begins them, and "no more" is over ten lease ticks.
func TestUnauthenticatedEnvelopeStopsTheAgent(t *testing.T) {
	w := newWorld(t)
	w.ownAgent("yuki-helper", 0)
	st := &leaseTicks{Store: memstore.New()}
	var gone atomic.Bool
	var after atomic.Int32
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": scripted.New()}, workerOpts{
		store: st,
		edit: wrapCaller(func(next core.Caller) core.Caller {
			return callerFunc(func(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
				if gone.Load() {
					after.Add(1)
					return &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeUnauthenticated, Message: "no such actor"}}, nil
				}
				return next.Call(ctx, tool, args)
			})
		}),
	})
	wk.waitState("yuki-helper", store.AgentRunning)
	gone.Store(true)
	wk.waitState("yuki-helper", store.AgentUnauthorized)
	noMoreOver(t, "the stopped agent", begun(&after), "lease ticks", func() int { return st.of("yuki-helper") })
}

// TestRateLimitedSlowsTheAgent: Core says 429 with Retry-After 7; the call
// waits it out and succeeds, and the agent's polling is halved for five
// minutes (§7.2).
func TestRateLimitedSlowsTheAgent(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	var once atomic.Bool
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == "conversation_inbox" && once.CompareAndSwap(false, true) {
			return &fakecore.Injection{Status: http.StatusTooManyRequests, RetryAfter: 7 * time.Second}
		}
		return nil
	})
	var mu sync.Mutex
	var waits []time.Duration
	model := scripted.New(scripted.Reply("Answered after the wait."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{
		edit: func(o *Options) {
			o.CoreRetry.Sleep = func(ctx context.Context, d time.Duration) error {
				mu.Lock()
				waits = append(waits, d)
				mu.Unlock()
				time.Sleep(time.Millisecond)
				return ctx.Err()
			}
		},
	})
	eventually(t, "the agent slowed", func() bool {
		st := wk.sup.Status()
		return len(st) == 1 && st[0].SlowUntil != nil
	})
	if until := *wk.sup.Status()[0].SlowUntil; time.Until(until) < Slowdown-time.Minute {
		t.Errorf("slowed until %s", until)
	}
	// Retrying tells the agent before it waits.
	eventually(t, "the wait", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(waits) > 0
	})
	mu.Lock()
	if waits[0] < 7*time.Second {
		t.Errorf("waited %v, want Retry-After's 7 s", waits)
	}
	mu.Unlock()
	conv, _ := w.ask(0, own, "Still there?")
	w.waitAnswers(conv, 1)
	var limited, ok int
	for _, c := range w.calls(own.actor.ID, "conversation_inbox") {
		switch {
		case c.HTTPStatus == http.StatusTooManyRequests:
			limited++
		case c.Status == "executed":
			ok++
		}
	}
	if limited != 1 || ok == 0 {
		t.Errorf("inbox calls: %d refused, %d executed", limited, ok)
	}
	if got := counter(t, wk.reg, "core_calls_total", map[string]string{"tool": "conversation_inbox", "status": "rate_limited"}); got != 1 {
		t.Errorf("core_calls_total{rate_limited} = %v", got)
	}
}

// TestUnauthorizedAtStart: the token Core issued refused from the first
// call stops the agent at once, and it is not started again until a
// reload, which is issued another. Its calls are counted as it begins
// them, on its connection to Core: one, which Core refused, and no more
// over ten lease ticks, at each of which it would have been started again
// had it not been stopped for good.
func TestUnauthorizedAtStart(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	var refusing atomic.Bool
	refusing.Store(true)
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.ActorID != own.actor.ID || !refusing.Load() {
			return nil
		}
		return &fakecore.Injection{Status: http.StatusUnauthorized}
	})
	st := &leaseTicks{Store: memstore.New()}
	var began atomic.Int32
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": scripted.New()},
		workerOpts{store: st, edit: countCalls("yuki-helper", &began)})
	wk.waitState("yuki-helper", store.AgentUnauthorized)
	noMoreOver(t, "the agent refused", begun(&began), "lease ticks", func() int { return st.of("yuki-helper") })
	if n := began.Load(); n != 1 {
		t.Errorf("the agent began %d calls to Core; it should have stopped at the first", n)
	}
	// The fake logs a refusal once it has answered it: the one refused is
	// read once it is there.
	refused := func() (n int) {
		for _, c := range w.fc.Calls() {
			if c.HTTPStatus == http.StatusUnauthorized {
				n++
			}
		}
		return n
	}
	eventually(t, "Core's refusal logged", func() bool { return refused() > 0 })
	if n := refused(); n != 1 {
		t.Errorf("Core refused %d calls; the agent should have stopped at the first", n)
	}
	if issues := w.fc.RuntimeIssues(own.actor.ID); issues != 2 {
		t.Errorf("Core issued the agent's token %d times; want 2 (its creation's and the worker's)", issues)
	}
	refusing.Store(false)
	wk.sup.Reload(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)))
	wk.waitState("yuki-helper", store.AgentRunning)
	if issues := w.fc.RuntimeIssues(own.actor.ID); issues != 3 {
		t.Errorf("Core issued the agent's token %d times after the reload; want 3", issues)
	}
}

// TestOneActorOneAgent: two agents named as one agent in Core (which the
// configuration refuses; two files loaded apart, here); the worker runs one
// of them, and holds the other in error, saying why, rather than answer
// every question twice over. The one held is issued no token.
func TestOneActorOneAgent(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	w.inCore("yuki-twin", own.actor.ID)
	model := scripted.New(scripted.Reply("Answered once."))
	cfg := w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil))
	cfg.Agents = append(cfg.Agents, w.config(nil, w.agentDoc("yuki-twin", "m1", nil, nil)).Agents...)
	wk := w.start(cfg, models{"m1": model}, workerOpts{})
	var running, refused string
	eventually(t, "one agent running, the other refused", func() bool {
		a, b := wk.state("yuki-helper"), wk.state("yuki-twin")
		switch {
		case a.State == store.AgentRunning && b.State == store.AgentError:
			running, refused = "yuki-helper", b.Detail
		case b.State == store.AgentRunning && a.State == store.AgentError:
			running, refused = "yuki-twin", a.Detail
		default:
			return false
		}
		return true
	})
	if !strings.Contains(refused, running) || !strings.Contains(refused, "one agent in Core is one agent here") {
		t.Errorf("the refused agent's detail: %q", refused)
	}
	conv, _ := w.ask(0, own, "How many of you are there?")
	w.waitAnswers(conv, 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(model.Requests()); n != 1 {
		t.Errorf("the model was called %d times", n)
	}
	if issues := w.fc.RuntimeIssues(own.actor.ID); issues != 2 {
		t.Errorf("Core issued the agent's token %d times; want 2 (its creation's and the one agent's)", issues)
	}
}

// TestSeatSnapshot: the worker writes each seat as me_memberships shows
// it, at every read, so that the API can show an agent's seats without its
// token: a delegate's principal, a tutor's answers_course, the course and
// the perms; and a change in Core is in the store at the next read.
func TestSeatSnapshot(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	tu := w.tutor("cs101-tutor")
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil), w.agentDoc("cs101-tutor", "m1", nil, nil)),
		models{"m1": scripted.New()}, workerOpts{})
	wk.waitState("yuki-helper", store.AgentRunning)
	wk.waitState("cs101-tutor", store.AgentRunning)
	seat := func(agent string) store.SeatRef {
		t.Helper()
		var got []store.SeatRef
		eventually(t, "the seat of "+agent+" recorded", func() bool {
			var err error
			got, err = wk.st.KnownSeats(context.Background(), agent)
			return err == nil && len(got) == 1
		})
		return got[0]
	}
	s := seat("yuki-helper")
	if s.MemberID != own.seat.ID || s.CourseID != w.co.ID || s.CourseCode != "CS101" || s.CourseTitle != "CS101" || s.Section != "A" ||
		s.Status != "active" || s.AnswersCourse || s.PrincipalMemberID != w.studentSeats[0].ID || s.Perms["conversation_answer"] == "" {
		t.Errorf("the delegate's seat: %+v", s)
	}
	if s := seat("cs101-tutor"); s.MemberID != tu.seat.ID || !s.AnswersCourse || s.PrincipalMemberID != w.satoSeat.ID {
		t.Errorf("the tutor's seat: %+v", s)
	}
	w.ok(w.fc.SetLevel(own.seat.ID, "grade_read", "denied"))
	eventually(t, "the changed perm in the store", func() bool { return seat("yuki-helper").Perms["grade_read"] == "denied" })
}
