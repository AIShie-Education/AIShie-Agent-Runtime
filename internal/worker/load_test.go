package worker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// TestLoad is §8.2's load test, short: 50 agents in 5 courses each, idle,
// against the fake Core's per-actor limiter. Their polling stays within
// max_rate_share of Core's allowance, and Core never answers 429: on the
// schedule, as against a Core from before wait_s, and long-polling their
// inboxes, every call one call to the share however long it waits, with
// none sent back to the schedule (5 seats an agent, 250 in all, are within
// Core's bounds on calls waiting).
//
// Time is scaled down 30 times: a minute of Core's is two seconds here.
// Every interval is divided by 30 (inbox 2 s hot, 10 s idle to 30 s at
// most, events every 45 s, seats every 300 s, as §4 has them) and every
// rate multiplied by 30 (Core's 600 calls a minute, burst 100, becomes
// 18,000 a minute, on the fake's limiter and in assumed_core_rate_per_min
// alike). max_rate_share is 0.03 rather than 0.3, so that the rate share's
// floor (§7.3) binds on the inbox intervals, and polling within the share
// is something the scheduler does, not something idle agents happen to do.
// A long poll waits a second, the least Core takes: 30 s scaled, where 25
// is the default.
func TestLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("the load test is not short enough for -short")
	}
	t.Run("on the schedule", func(t *testing.T) { load(t, 0) })
	t.Run("long-polling", func(t *testing.T) { load(t, 1) })
}

func load(t *testing.T, waitS float64) {
	const (
		scale   = 30.0
		agents  = 50
		courses = 5
		share   = 0.03
		rate    = 600 * scale // calls a minute, scaled
	)
	w := newWorldWith(t, fakecore.Options{RatePerMinute: int(rate), RateBurst: 100})
	var cos []fakecore.Course
	var principals []fakecore.Member
	cos = append(cos, w.co)
	principals = append(principals, w.satoSeat)
	for i := 1; i < courses; i++ {
		co := w.fc.AddCourse(fmt.Sprintf("CS10%d", i+1))
		cos = append(cos, co)
		principals = append(principals, w.must(w.fc.Seat(w.sato.ID, co.ID, fakecore.SeatOptions{Preset: "instructor"})))
	}
	polling := map[string]any{"polling": map[string]any{
		"inbox_hot_s": 2 / scale, "hot_window_s": 120 / scale, "inbox_idle_s": 10 / scale, "inbox_max_s": 30 / scale,
		"events_s": 45 / scale, "memberships_s": 300 / scale, "jitter": 0.25, "max_rate_share": share,
		"assumed_core_rate_per_min": int(rate), "assumed_core_burst": 100, "long_poll_wait_s": waitS,
	}}
	var docs []map[string]any
	actors := map[string]string{}
	for i := range agents {
		id := fmt.Sprintf("tutor-%02d", i)
		a, err := w.fc.AddAgent("Tutor "+id, w.sato.ID)
		w.ok(err)
		for c, co := range cos {
			w.must(w.fc.Seat(a.ID, co.ID, fakecore.SeatOptions{Preset: "course_tutor", Principal: principals[c].ID}))
		}
		w.inCore(id, a.ID)
		actors[a.ID] = id
		docs = append(docs, w.agentDoc(id, "m1", polling, nil))
	}
	wk := w.start(w.config(nil, docs...), models{"m1": scripted.New()}, workerOpts{
		edit: func(o *Options) { o.Timing.LeaseEvery = 200 * time.Millisecond },
	})
	// Fifty agents and their 250 seats start much more slowly than one,
	// and on a busy machine slower still: they are given a minute.
	eventuallyWithin(t, time.Minute, "every agent polling every course", func() bool {
		st := wk.sup.Status()
		if len(st) != agents {
			return false
		}
		for _, s := range st {
			if s.State != store.AgentRunning || len(s.Seats) != courses {
				return false
			}
		}
		return true
	})

	// Settled once every agent has polled every course's inbox, its first
	// polls spread over the idle interval behind it; then measured for
	// four seconds, two scaled minutes, and for as long after as it takes
	// every agent to poll every course's inbox within the window, which a
	// busy machine, its calls slower, makes longer. The share is reckoned
	// over the window as it was: a slower machine polls less, never more.
	waitPolled(t, w.fc, actors, courses, time.Time{})
	from := time.Now()
	time.Sleep(4 * time.Second)
	waitPolled(t, w.fc, actors, courses, from)
	to := time.Now()
	window := to.Sub(from).Round(time.Millisecond)

	polls := map[string]int{}
	inboxes := map[string]map[string]bool{}
	limited, waited := 0, 0
	for _, c := range w.fc.Calls() {
		if c.HTTPStatus == http.StatusTooManyRequests {
			limited++
		}
		id, ok := actors[c.ActorID]
		if !ok || c.At.Before(from) || c.At.After(to) {
			continue
		}
		switch c.Tool {
		case "conversation_inbox", "event_list", "me_memberships":
			polls[id]++
		}
		if c.Tool == "conversation_inbox" {
			var args struct {
				CourseID string `json:"course_id"`
				WaitS    int    `json:"wait_s"`
			}
			w.ok(json.Unmarshal(c.Args, &args))
			if inboxes[id] == nil {
				inboxes[id] = map[string]bool{}
			}
			inboxes[id][args.CourseID] = true
			if args.WaitS > 0 {
				waited++
			}
		}
	}
	if long := waitS > 0; long != (waited > 0) {
		t.Errorf("%d inbox calls waited for news, with long_poll_wait_s %v", waited, waitS)
	}
	if n := counter(t, wk.reg, "long_poll_fallbacks_total", nil); n != 0 {
		t.Errorf("%v long polls went back to the schedule", n)
	}
	if limited != 0 {
		t.Errorf("Core answered 429 %d times", limited)
	}
	allowed := share * rate / 60 * window.Seconds() // calls an agent's polling may make in the window
	total := 0
	for id := range docsIDs(docs) {
		n := polls[id]
		total += n
		if float64(n) > allowed*1.3 {
			t.Errorf("%s polled %d times in %s; its share is %.0f", id, n, window, allowed)
		}
		if len(inboxes[id]) != courses {
			t.Errorf("%s polled %d courses' inboxes, not %d", id, len(inboxes[id]), courses)
		}
	}
	if float64(total) > allowed*agents*1.1 {
		t.Errorf("the agents polled %d times in %s; their share is %.0f", total, window, allowed*agents)
	}
	t.Logf("%d agents in %d courses: %d polling calls in %s, %.0f%% of their share; no 429",
		agents, courses, total, window, 100*float64(total)/(allowed*agents))
}

// waitPolled waits until each of actors (agents' ids by their actors')
// has polled the inbox of every one of courses courses since from, failing
// the test, with those not yet polled, after a deadline. It reads the
// fake's log at intervals of 50 ms: it holds every call made.
func waitPolled(t *testing.T, fc *fakecore.Core, actors map[string]string, courses int, from time.Time) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		inboxes := map[string]map[string]bool{}
		for _, c := range fc.Calls() {
			if c.Tool != "conversation_inbox" || c.At.Before(from) {
				continue
			}
			var args struct {
				CourseID string `json:"course_id"`
			}
			if json.Unmarshal(c.Args, &args) != nil {
				continue
			}
			if inboxes[c.ActorID] == nil {
				inboxes[c.ActorID] = map[string]bool{}
			}
			inboxes[c.ActorID][args.CourseID] = true
		}
		var short []string
		for actor, id := range actors {
			if n := len(inboxes[actor]); n < courses {
				short = append(short, fmt.Sprintf("%s %d", id, n))
			}
		}
		if len(short) == 0 {
			return
		}
		if time.Now().After(deadline) {
			slices.Sort(short)
			t.Fatalf("not every agent polled all %d courses' inboxes (agent, courses polled): %s", courses, strings.Join(short, ", "))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func docsIDs(docs []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, d := range docs {
		out[d["agent"].(map[string]any)["id"].(string)] = true
	}
	return out
}
