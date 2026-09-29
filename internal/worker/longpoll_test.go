package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
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
)

// waitOf is the wait_s a call asked for; 0 when it asked none. Over REST
// to a Core that has no wait_s, the query string's text is what it got.
func waitOf(t *testing.T, c fakecore.Call) int {
	t.Helper()
	var args struct {
		WaitS json.RawMessage `json:"wait_s"`
	}
	if err := json.Unmarshal(c.Args, &args); err != nil {
		t.Fatalf("%s's arguments: %v", c.Tool, err)
	}
	if len(args.WaitS) == 0 {
		return 0
	}
	var text string
	if json.Unmarshal(args.WaitS, &text) == nil {
		args.WaitS = json.RawMessage(text)
	}
	var n int
	if err := json.Unmarshal(args.WaitS, &n); err != nil {
		t.Fatalf("%s's wait_s: %v", c.Tool, err)
	}
	return n
}

// waits are the wait_s of each of actor's calls of tool, in order.
func (w *world) waits(actor, tool string) []int {
	var out []int
	for _, c := range w.calls(actor, tool) {
		out = append(out, waitOf(w.t, c))
	}
	return out
}

// seatStatus is the agent's seat member as the supervisor shows it.
func (wk *worker) seatStatus(agentID, member string) (SeatStatus, bool) {
	for _, a := range wk.sup.Status() {
		if a.AgentID != agentID {
			continue
		}
		for _, s := range a.Seats {
			if s.MemberID == member {
				return s, true
			}
		}
	}
	return SeatStatus{}, false
}

// TestLongPollPicksUpAtOnce: where Core offers wait_s, an idle seat's inbox
// call waits for a question, and a question asked meanwhile is answered at
// once, whatever the schedule says; the call asks for long_poll_wait_s, and
// the seat waits again after its answer.
func TestLongPollPicksUpAtOnce(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	polling := map[string]any{"polling": map[string]any{"inbox_hot_s": 0.1, "inbox_idle_s": 0.2, "inbox_max_s": 30, "long_poll_wait_s": 20}}
	model := scripted.New(scripted.Reply("At once."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", polling, nil)), models{"m1": model}, workerOpts{})
	eventually(t, "the inbox waiting for a question", func() bool { return w.fc.WaitingOf(own.actor.ID) == 1 })
	if st, ok := wk.seatStatus("yuki-helper", own.seat.ID); !ok || !st.LongPolling {
		t.Errorf("the seat's status: %+v", st)
	}
	if got := counter(t, wk.reg, "long_polls", map[string]string{"agent": "yuki-helper"}); got != 1 {
		t.Errorf("long_polls = %v", got)
	}
	asked := time.Now()
	conv, _ := w.ask(0, own, "Is anyone there?")
	w.waitAnswers(conv, 1)
	// The only inbox call in flight waits 20 s: nothing but it could have
	// found the question.
	if took := time.Since(asked); took > time.Second {
		t.Errorf("answered %s after the question", took)
	}
	eventually(t, "the inbox waiting again", func() bool { return w.fc.WaitingOf(own.actor.ID) == 1 })
	for i, s := range w.waits(own.actor.ID, "conversation_inbox") {
		if s != 20 {
			t.Errorf("inbox call %d asked to wait %d s", i, s)
		}
	}
}

// TestOlderCorePollsOnSchedule: against a Core from before wait_s, whose
// catalogue offers none, nothing changes: the inbox is polled on the
// schedule, no call asks to wait, and questions are answered.
func TestOlderCorePollsOnSchedule(t *testing.T) {
	w := newWorldWith(t, fakecore.Options{WithoutWait: true})
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Reply("On schedule."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{})
	eventually(t, "the inbox polled a few times", func() bool { return len(w.calls(own.actor.ID, "conversation_inbox")) > 3 })
	conv, _ := w.ask(0, own, "Is this an older Core?")
	w.waitAnswers(conv, 1)
	for _, tool := range []string{"conversation_inbox", "event_list"} {
		for _, c := range w.calls(own.actor.ID, tool) {
			if s := waitOf(t, c); s != 0 || c.Status != "executed" {
				t.Errorf("%s asked to wait %d s: %s %s", tool, s, c.Status, c.Code)
			}
		}
	}
	if got := counter(t, wk.reg, "long_poll_fallbacks_total", nil); got != 0 {
		t.Errorf("%v fallbacks from long polls never made", got)
	}
}

// TestLongPollFallsBackWhenCoreDoesNotWait: a Core that lets none of the
// agent's calls wait answers each at once, empty; the seat takes that as
// Core not waiting, polls on its schedule for a while, and then tries again.
// Questions are answered all along.
func TestLongPollFallsBackWhenCoreDoesNotWait(t *testing.T) {
	w := newWorldWith(t, fakecore.Options{LongPollWaitersPerActor: -1})
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Reply("Answered all the same."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{
		edit: func(o *Options) { o.Timing.LongPollFallback = 300 * time.Millisecond },
	})
	eventually(t, "the seat back on its schedule twice", func() bool {
		return counter(t, wk.reg, "long_poll_fallbacks_total", map[string]string{"agent": "yuki-helper", "why": fallbackEarly}) >= 2
	})
	waits := w.waits(own.actor.ID, "conversation_inbox")
	first := slices.Index(waits, 1)
	if first < 0 {
		t.Fatalf("no call asked to wait: %v", waits)
	}
	scheduled := 0
	for _, s := range waits[first+1:] {
		if s != 0 {
			break
		}
		scheduled++
	}
	// The schedule polls every 20 to 50 ms for the fallback's 300 ms.
	if scheduled < 3 || slices.Index(waits[first+1+scheduled:], 1) < 0 {
		t.Errorf("after a call Core did not wait on, %d calls on the schedule, then %v", scheduled, waits[first+1+scheduled:])
	}
	conv, _ := w.ask(0, own, "Still answering?")
	w.waitAnswers(conv, 1)
}

// TestOlderCoreBehindTheCatalogue: the catalogue offers wait_s, but the
// Core that answers is older, and refuses a call that gives it
// (invalid_argument, "unexpected additional properties [\"wait_s\"]", in
// an envelope over MCP and with a 400 over REST). The seat takes that as
// Core not waiting: it reads its inbox again at once without wait_s, polls
// on its schedule for a while, and counts no error.
func TestOlderCoreBehindTheCatalogue(t *testing.T) {
	snapshot, err := os.ReadFile("../core/testdata/catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{config.TransportMCP, config.TransportREST} {
		t.Run(transport, func(t *testing.T) {
			w := newWorldWith(t, fakecore.Options{WithoutWait: true})
			w.tools.Store(&snapshot)
			own := w.ownAgent("yuki-helper", 0)
			model := scripted.New(scripted.Reply("From an older Core."))
			over := map[string]any{"core": map[string]any{"transport": transport}}
			wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, nil)), models{"m1": model}, workerOpts{
				edit: func(o *Options) { o.Timing.LongPollFallback = time.Hour },
			})
			eventually(t, "the seat on its schedule", func() bool {
				return counter(t, wk.reg, "long_poll_fallbacks_total", map[string]string{"agent": "yuki-helper", "why": fallbackRefused}) == 1
			})
			conv, _ := w.ask(0, own, "Which Core is this?")
			w.waitAnswers(conv, 1)
			calls := w.calls(own.actor.ID, "conversation_inbox")
			if len(calls) < 2 || waitOf(t, calls[0]) != 1 || calls[0].Code != "invalid_argument" {
				t.Fatalf("the first inbox call: %+v", calls[0])
			}
			for _, c := range calls[1:] {
				if waitOf(t, c) != 0 || c.Status != "executed" {
					t.Errorf("an inbox call after Core refused wait_s: %s, %s %s", c.Args, c.Status, c.Code)
				}
			}
			if got := counter(t, wk.reg, "inbox_polls_total", map[string]string{"agent": "yuki-helper", "result": "error"}); got != 0 {
				t.Errorf("inbox_polls_total{result=error} = %v", got)
			}
			if strings.Contains(w.logs.String(), "Core could not be read") {
				t.Error("the refusal was logged as Core failing")
			}
			if st, _ := wk.seatStatus("yuki-helper", own.seat.ID); st.ScheduledUntil == nil || time.Until(*st.ScheduledUntil) < 30*time.Minute {
				t.Errorf("the seat's status: %+v", st)
			}
		})
	}
}

// TestLongPollCutFallsBack: a long poll that does not come back (a proxy
// that cuts it short) sends the seat to its schedule for a while, which
// still answers.
func TestLongPollCutFallsBack(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	var cut atomic.Int32
	model := scripted.New(scripted.Reply("On the schedule."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{
		edit: func(o *Options) {
			wrapCaller(func(next core.Caller) core.Caller {
				return callerFunc(func(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
					if core.WaitOf(ctx) > 0 {
						cut.Add(1)
						return nil, &core.TransientError{Status: http.StatusGatewayTimeout, Err: errors.New("the proxy gave up")}
					}
					return next.Call(ctx, tool, args)
				})
			})(o)
			o.Timing.LongPollFallback = time.Hour
		},
	})
	eventually(t, "the seat back on its schedule", func() bool {
		return counter(t, wk.reg, "long_poll_fallbacks_total", map[string]string{"agent": "yuki-helper", "why": fallbackCut}) == 1
	})
	conv, _ := w.ask(0, own, "Are you there?")
	w.waitAnswers(conv, 1)
	if n := cut.Load(); n != 1 {
		t.Errorf("%d long polls made, want 1 before an hour on the schedule", n)
	}
	if st, _ := wk.seatStatus("yuki-helper", own.seat.ID); st.ScheduledUntil == nil || time.Until(*st.ScheduledUntil) < 30*time.Minute {
		t.Errorf("the seat's status: %+v", st)
	}
}

// TestLongPollCapAcrossSeats: an agent in four courses with long_poll_max
// 2 never has more than two calls waiting; its other seats poll on their
// schedule; and a seat that has just answered, the most recently active,
// is among those that long-poll.
func TestLongPollCapAcrossSeats(t *testing.T) {
	w := newWorld(t)
	a, err := w.fc.AddAgent("CS Tutor", w.sato.ID)
	w.ok(err)
	w.env.Store(tokenVar("tutor"), a.Token)
	courses := []fakecore.Course{w.co}
	principals := []fakecore.Member{w.satoSeat}
	for i := 2; i <= 4; i++ {
		co := w.fc.AddCourse(fmt.Sprintf("CS10%d", i))
		courses = append(courses, co)
		principals = append(principals, w.must(w.fc.Seat(w.sato.ID, co.ID, fakecore.SeatOptions{Preset: "instructor"})))
	}
	var seats []fakecore.Member
	for i, co := range courses {
		seats = append(seats, w.must(w.fc.Seat(a.ID, co.ID, fakecore.SeatOptions{Preset: "course_tutor", Principal: principals[i].ID})))
	}
	polling := map[string]any{"polling": map[string]any{"long_poll_max": 2}}
	model := scripted.New(scripted.Reply("From the busiest seat."))
	wk := w.start(w.config(nil, w.agentDoc("tutor", "m1", polling, nil)), models{"m1": model}, workerOpts{})

	var most atomic.Int32
	stop := make(chan struct{})
	var sampled sync.WaitGroup
	sampled.Add(1)
	go func() {
		defer sampled.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			if n := int32(w.fc.WaitingOf(a.ID)); n > most.Load() {
				most.Store(n)
			}
			if st := wk.sup.Status(); len(st) == 1 && int32(st[0].LongPolls) > most.Load() {
				most.Store(int32(st[0].LongPolls))
			}
		}
	}()
	eventually(t, "two seats long-polling", func() bool { return w.fc.WaitingOf(a.ID) == 2 })
	eventually(t, "the other seats on their schedule", func() bool {
		scheduled := 0
		for _, c := range w.calls(a.ID, "conversation_inbox") {
			if waitOf(t, c) == 0 {
				scheduled++
			}
		}
		return scheduled > 3
	})

	// The seats that wait are the two first by member id, none having been
	// active; the last is asked a question, which it answers from its
	// schedule, and then it waits too.
	last := seats[0]
	for _, s := range seats {
		if s.ID > last.ID {
			last = s
		}
	}
	co := courses[slices.IndexFunc(seats, func(m fakecore.Member) bool { return m.ID == last.ID })]
	student := w.must(w.fc.Seat(w.students[0].ID, co.ID, fakecore.SeatOptions{Preset: "student"}))
	eventually(t, "the tutor declaring that it answers in the site", func() bool { return w.fc.SiteChat(a.ID) })
	cv, _, err := w.fc.Ask(co.ID, student.ID, last.ID, "Which seat answers me?")
	w.ok(err)
	w.waitAnswers(cv.ID, 1)
	eventually(t, "the seat that answered long-polling", func() bool {
		st, _ := wk.seatStatus("tutor", last.ID)
		return st.LongPolling
	})
	close(stop)
	sampled.Wait()
	if n := most.Load(); n > 2 {
		t.Errorf("%d calls waited at once; long_poll_max is 2", n)
	}
}

// TestStopCancelsLongPolls: a seat that stops answering, and a worker that
// stops, end their calls waiting for news at once, not when the wait is up.
func TestStopCancelsLongPolls(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	polling := map[string]any{"polling": map[string]any{"long_poll_wait_s": 25}}
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", polling, nil)), models{"m1": scripted.New()}, workerOpts{})
	eventually(t, "the inbox waiting", func() bool { return w.fc.WaitingOf(own.actor.ID) == 1 })

	// Denied: no news to the call waiting, which Core would answer only in
	// 25 s; me_memberships shows the seat no longer answering, and the seat
	// stops.
	w.ok(w.fc.SetLevel(own.seat.ID, "conversation_answer", "denied"))
	within(t, 3*time.Second, "the seat's long poll to end", func() bool { return w.fc.WaitingOf(own.actor.ID) == 0 })
	// Core lets go of the call when the seat cancels it; the seat gives
	// its place back once the call has returned to it, a moment later.
	within(t, time.Second, "long_polls to be 0 after the seat stopped", func() bool {
		return counter(t, wk.reg, "long_polls", map[string]string{"agent": "yuki-helper"}) == 0
	})

	w.ok(w.fc.SetLevel(own.seat.ID, "conversation_answer", "autonomous"))
	eventually(t, "the seat waiting again", func() bool { return w.fc.WaitingOf(own.actor.ID) == 1 })
	start := time.Now()
	wk.stop()
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the worker took %s to stop", took)
	}
	within(t, time.Second, "Core to see the worker's long poll end", func() bool { return w.fc.WaitingOf(own.actor.ID) == 0 })
}

// within waits for cond, failing the test after d.
func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s took longer than %s", what, d)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestLongPollSlowdown: after a 429 the agent's polling is halved for five
// minutes, long polls too: the next begins no sooner after the last began
// than its schedule, doubled, allows.
func TestLongPollSlowdown(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	var once atomic.Bool
	w.fc.Inject(func(c fakecore.InjectedCall) *fakecore.Injection {
		if c.Tool == "conversation_inbox" && once.CompareAndSwap(false, true) {
			return &fakecore.Injection{Status: http.StatusTooManyRequests, RetryAfter: time.Second}
		}
		return nil
	})
	// Idle, the schedule would poll every 1.5 s; slowed, every 3 s. A long
	// poll waits a second.
	polling := map[string]any{"polling": map[string]any{"inbox_hot_s": 1.5, "inbox_idle_s": 1.5, "inbox_max_s": 1.5, "jitter": 0,
		"events_s": 5, "memberships_s": 5}}
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", polling, nil)), models{"m1": scripted.New()}, workerOpts{
		edit: func(o *Options) {
			o.CoreRetry.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
		},
	})
	eventually(t, "the agent slowed", func() bool {
		st := wk.sup.Status()
		return len(st) == 1 && st[0].SlowUntil != nil
	})
	time.Sleep(7 * time.Second)
	var at []time.Time
	for _, c := range w.calls(own.actor.ID, "conversation_inbox") {
		if c.Status == "executed" && waitOf(t, c) == 1 {
			at = append(at, c.At)
		}
	}
	if len(at) < 2 {
		t.Fatalf("%d long polls in 7 s", len(at))
	}
	for i := 1; i < len(at); i++ {
		// Each began 3 s after the last began, and waited a second.
		if gap := at[i].Sub(at[i-1]); gap < 2800*time.Millisecond {
			t.Errorf("long polls came back %s apart while the agent was slowed", gap)
		}
	}
	if len(at) > 4 {
		t.Errorf("%d long polls in 7 s while slowed", len(at))
	}
}

// TestLongPollNext: a long poll that found nothing is made again at once,
// but no sooner after the last began than the rate share's floor, and while
// slowed after a 429, than the schedule's interval.
func TestLongPollNext(t *testing.T) {
	start := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	if got := LongPollNext(start, 300*time.Millisecond, 20*time.Second, false); !got.Equal(start.Add(300 * time.Millisecond)) {
		t.Errorf("not slowed: %s", got.Sub(start))
	}
	if got := LongPollNext(start, 300*time.Millisecond, 20*time.Second, true); !got.Equal(start.Add(20 * time.Second)) {
		t.Errorf("slowed: %s", got.Sub(start))
	}
	if got := LongPollNext(start, 40*time.Second, 20*time.Second, true); !got.Equal(start.Add(40 * time.Second)) {
		t.Errorf("slowed, the floor longer: %s", got.Sub(start))
	}
	p := config.Polling{LongPollWaitS: 25}
	for _, tc := range []struct {
		waitS   float64
		offered time.Duration
		want    time.Duration
	}{
		{25, 25 * time.Second, 25 * time.Second},
		{25, 0, 0},
		{0, 25 * time.Second, 0},
		{0.5, 25 * time.Second, time.Second},
		{25, 10 * time.Second, 10 * time.Second},
	} {
		p.LongPollWaitS = tc.waitS
		if got := longPollWait(p, tc.offered); got != tc.want {
			t.Errorf("long_poll_wait_s %v, Core offering %s: %s, want %s", tc.waitS, tc.offered, got, tc.want)
		}
	}
}

// TestLongPollOutlastsTheClientTimeout: the operator's HTTP client gives a
// request a second; a long poll of three waits its time all the same, and
// is not taken for one cut short.
func TestLongPollOutlastsTheClientTimeout(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	polling := map[string]any{"polling": map[string]any{"long_poll_wait_s": 3}}
	model := scripted.New(scripted.Reply("Late, but there."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", polling, nil)), models{"m1": model}, workerOpts{
		edit: func(o *Options) { o.HTTPClient = &http.Client{Timeout: time.Second} },
	})
	eventually(t, "the inbox waiting", func() bool { return w.fc.WaitingOf(own.actor.ID) == 1 })
	time.Sleep(2 * time.Second)
	conv, _ := w.ask(0, own, "Did the call last?")
	w.waitAnswers(conv, 1)
	eventually(t, "a long poll that waited its time", func() bool {
		for _, c := range w.calls(own.actor.ID, "conversation_inbox") {
			if c.Status == "executed" && waitOf(t, c) == 3 {
				return true
			}
		}
		return false
	})
	if got := counter(t, wk.reg, "long_poll_fallbacks_total", nil); got != 0 {
		t.Errorf("%v long polls taken for failed", got)
	}
}

// TestProposalFollowedByLongPoll: after an answer is proposed, the seat's
// events wait for news for the follow-ups' window, so that a decision is
// settled at once, not at the next of the follow-ups' reads.
func TestProposalFollowedByLongPoll(t *testing.T) {
	w := newWorld(t)
	model := scripted.New(scripted.Reply("An answer to approve."))
	// Events are read every 45 s, and the follow-ups at 0, 5, 15 and 45 s.
	polling := map[string]any{"polling": map[string]any{"events_s": 45}}
	tu, wk := confirmedTutor(t, w, model, polling)
	conv, msg := w.ask(0, tu, "May I have an answer soon?")
	key := core.AnswerKey(conv, msg, 1)
	p := w.waitProposal(key)
	wk.waitAttempt("cs101-tutor", key, store.AttemptProposed)
	eventually(t, "the seat's events waiting for news", func() bool { return w.fc.WaitingOf(tu.actor.ID) == 2 })
	time.Sleep(1500 * time.Millisecond)
	approved := time.Now()
	outcome, err := w.fc.Approve(p.ActionID)
	w.ok(err)
	if outcome != "executed" {
		t.Fatalf("approval: %s", outcome)
	}
	wk.waitAttempt("cs101-tutor", key, store.AttemptExecuted)
	if took := time.Since(approved); took > time.Second {
		t.Errorf("the proposal was settled %s after it was approved", took)
	}
	waited := 0
	for _, c := range w.calls(tu.actor.ID, "event_list") {
		if waitOf(t, c) > 0 {
			waited++
		}
	}
	if waited == 0 {
		t.Error("no read of events waited for news")
	}
}
