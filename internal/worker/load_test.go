package worker

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// TestLoad is §8.2's load test, short: 50 agents in 5 courses each, idle,
// against the fake Core's per-actor limiter. Their polling stays within
// max_rate_share of Core's allowance, and Core never answers 429.
//
// Time is scaled down 30 times: a minute of Core's is two seconds here.
// Every interval is divided by 30 (inbox 2 s hot, 10 s idle to 30 s at
// most, events every 45 s, seats every 300 s, as §4 has them) and every
// rate multiplied by 30 (Core's 600 calls a minute, burst 100, becomes
// 18,000 a minute, on the fake's limiter and in assumed_core_rate_per_min
// alike). max_rate_share is 0.03 rather than 0.3, so that the rate share's
// floor (§7.3) binds on the inbox intervals, and polling within the share
// is something the scheduler does, not something idle agents happen to do.
func TestLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("the load test is not short enough for -short")
	}
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
		"assumed_core_rate_per_min": int(rate), "assumed_core_burst": 100,
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
		w.env.Store(tokenVar(id), a.Token)
		actors[a.ID] = id
		docs = append(docs, w.agentDoc(id, "m1", polling, nil))
	}
	wk := w.start(w.config(nil, docs...), models{"m1": scripted.New()}, workerOpts{
		edit: func(o *Options) { o.Timing.LeaseEvery = 200 * time.Millisecond },
	})
	eventually(t, "every agent polling every course", func() bool {
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

	// Half a scaled minute to settle, then four seconds, two scaled
	// minutes, measured.
	time.Sleep(time.Second)
	from := time.Now()
	window := 4 * time.Second
	time.Sleep(window)
	to := time.Now()

	polls := map[string]int{}
	inboxes := map[string]map[string]bool{}
	limited := 0
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
			if inboxes[id] == nil {
				inboxes[id] = map[string]bool{}
			}
			inboxes[id][string(c.Args)] = true
		}
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

func docsIDs(docs []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, d := range docs {
		out[d["agent"].(map[string]any)["id"].(string)] = true
	}
	return out
}
