package worker

import (
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
)

// defaults are §4's polling settings.
var defaults = config.Polling{
	InboxHotS: 2, HotWindowS: 120, InboxIdleS: 10, InboxMaxS: 30, EventsS: 45, MembershipsS: 300,
	Jitter: 0.25, MaxRateShare: 0.3, AssumedCoreRatePerMin: 600, AssumedCoreBurst: 100,
}

func TestRateFloorMatchesTheHandout(t *testing.T) {
	// §7.3: "An agent in 10 courses may poll each every 4 s or so."
	got := RateFloor(defaults, 10)
	if got < 3500*time.Millisecond || got > 4*time.Second {
		t.Errorf("RateFloor(10 courses) = %s, want about 3.6 s", got)
	}
	// One course: a third of a second, far below the hot interval.
	if got := RateFloor(defaults, 1); got > 500*time.Millisecond {
		t.Errorf("RateFloor(1 course) = %s", got)
	}
	// So many courses that events alone spend the share: the slowest.
	if got := RateFloor(defaults, 300); got != 30*time.Second {
		t.Errorf("RateFloor(300 courses) = %s, want inbox_max_s", got)
	}
}

func TestPollingStaysWithinTheShare(t *testing.T) {
	for _, courses := range []int{1, 5, 10, 40, 100} {
		floor := RateFloor(defaults, courses)
		// Every seat hot at once, the worst case.
		interval := InboxInterval(defaults, true, 0, floor, false, 0.5)
		perMinute := float64(courses)*float64(time.Minute)/float64(interval) + backgroundPerMinute(defaults, courses)
		if limit := pollShare(defaults); perMinute > limit*1.0001 && floor < 30*time.Second {
			t.Errorf("%d courses, all hot: %.1f calls a minute, over the share of %.0f", courses, perMinute, limit)
		}
	}
}

func TestInboxInterval(t *testing.T) {
	floor := RateFloor(defaults, 1)
	for _, c := range []struct {
		name  string
		hot   bool
		empty int
		slow  bool
		want  time.Duration
	}{
		{"hot", true, 0, false, 2 * time.Second},
		{"idle", false, 0, false, 10 * time.Second},
		{"one empty poll", false, 1, false, 15 * time.Second},
		{"two empty polls", false, 2, false, 22500 * time.Millisecond},
		{"capped", false, 10, false, 30 * time.Second},
		{"hot but slowed by a 429", true, 0, true, 4 * time.Second},
	} {
		if got := InboxInterval(defaults, c.hot, c.empty, floor, c.slow, 0.5); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// The floor wins over the hot interval.
	if got := InboxInterval(defaults, true, 0, 5*time.Second, false, 0.5); got != 5*time.Second {
		t.Errorf("under a 5 s floor: %s", got)
	}
}

func TestJitterSpreadsByTheFraction(t *testing.T) {
	if got := Jitter(10*time.Second, 0.25, 0); got != 7500*time.Millisecond {
		t.Errorf("lowest = %s", got)
	}
	if got := Jitter(10*time.Second, 0.25, 0.999999); got < 12499*time.Millisecond || got > 12500*time.Millisecond {
		t.Errorf("highest = %s", got)
	}
	if got := Jitter(10*time.Second, 0, 0.9); got != 10*time.Second {
		t.Errorf("no jitter = %s", got)
	}
}

func TestBackoff(t *testing.T) {
	var got []time.Duration
	for n := range 8 {
		got = append(got, Backoff(n, time.Second, time.Minute, 0.999999999))
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i := range want {
		if d := got[i] - want[i]*time.Second; d > time.Millisecond || d < -time.Millisecond {
			t.Errorf("Backoff(%d) = %s, want about %ds", i, got[i], want[i])
		}
	}
	if Backoff(3, time.Second, time.Minute, 0) != 0 {
		t.Error("full jitter can wait no time at all")
	}
}

func TestFirstPollSpreadsOverTheIdleInterval(t *testing.T) {
	if got := FirstPoll(defaults, 0); got != 0 {
		t.Errorf("FirstPoll(0) = %s", got)
	}
	if got := FirstPoll(defaults, 0.5); got != 5*time.Second {
		t.Errorf("FirstPoll(0.5) = %s", got)
	}
	if got := FirstPoll(defaults, 0.999999); got >= 10*time.Second {
		t.Errorf("FirstPoll(1-) = %s", got)
	}
}

func TestInboxIntervalJitterStaysAroundTheFloor(t *testing.T) {
	floor := 20 * time.Second
	lo, hi := InboxInterval(defaults, false, 0, floor, false, 0), InboxInterval(defaults, false, 0, floor, false, 0.999999)
	if lo != 15*time.Second || hi < 24999*time.Millisecond || hi > 25*time.Second {
		t.Errorf("jittered around a 20 s floor: %s to %s", lo, hi)
	}
}

func TestProposalFollowUps(t *testing.T) {
	want := []time.Duration{0, 5 * time.Second, 15 * time.Second, 45 * time.Second}
	if len(ProposalFollowUps) != len(want) {
		t.Fatalf("ProposalFollowUps = %v", ProposalFollowUps)
	}
	for i := range want {
		if ProposalFollowUps[i] != want[i] {
			t.Errorf("ProposalFollowUps = %v, want %v (§7.2)", ProposalFollowUps, want)
		}
	}
}
