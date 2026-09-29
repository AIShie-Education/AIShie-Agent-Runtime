package worker

import (
	"math"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
)

// Polling spends a share of Core's rate limit (Core's docs/agent-runtime.md
// §7.2, §7.3). Core pushes nothing and has no inbox across courses, so
// every seat's inbox is asked on its own. Where Core's reads wait for news
// (wait_s), a seat's inbox call waits for a question and is made again at
// once (longpoll.go); against an older Core, and while it does not wait, a
// seat polls on the schedule here: often while a student is likely to
// follow up, less often as it stays quiet, and never so often that the
// agent's seats together pass their share. A call that waits is one call
// to the share, however long it waits.

// Slowdown is how long intervals stay doubled after Core says 429.
const Slowdown = 5 * time.Minute

// pollShare is the calls a minute an agent's polling may make: its share of
// Core's allowance.
func pollShare(p config.Polling) float64 {
	return p.MaxRateShare * float64(p.AssumedCoreRatePerMin)
}

// backgroundPerMinute is what events and seats cost a minute, for an agent
// in courses courses: event_list per seat every events_s, me_memberships
// every memberships_s.
func backgroundPerMinute(p config.Polling, courses int) float64 {
	var n float64
	if p.EventsS > 0 {
		n += float64(courses) * 60 / p.EventsS
	}
	if p.MembershipsS > 0 {
		n += 60 / p.MembershipsS
	}
	return n
}

// RateFloor is the least interval between one seat's inbox polls when the
// agent sits in courses courses: courses × 60 / (share − event and seat
// calls a minute) seconds (§7.3). If events and seats alone spend the share,
// the floor is inbox_max_s: polling still happens, at its slowest.
func RateFloor(p config.Polling, courses int) time.Duration {
	if courses < 1 {
		courses = 1
	}
	left := pollShare(p) - backgroundPerMinute(p, courses)
	if left <= 0 {
		return config.Seconds(p.InboxMaxS)
	}
	return time.Duration(float64(courses) * 60 / left * float64(time.Second))
}

// InboxInterval is how long a seat waits before its next inbox poll:
// inbox_hot_s while hot, else inbox_idle_s grown ×1.5 per empty poll up to
// inbox_max_s; never below floor; doubled while slow (after a 429); then
// jittered by ±jitter with r, a number in [0, 1).
func InboxInterval(p config.Polling, hot bool, emptyPolls int, floor time.Duration, slow bool, r float64) time.Duration {
	var base float64
	if hot {
		base = p.InboxHotS
	} else {
		base = p.InboxIdleS * math.Pow(1.5, float64(max(emptyPolls, 0)))
		if base > p.InboxMaxS {
			base = p.InboxMaxS
		}
	}
	d := config.Seconds(base)
	if d < floor {
		d = floor
	}
	if slow {
		d *= 2
	}
	return Jitter(d, p.Jitter, r)
}

// LongPollNext is when a seat that long-polls asks its inbox again, after
// a call that began at start and found nothing, or on a wake-up: at once,
// but no sooner after start than floor, the rate share's floor, allows; and
// while the agent is slowed after a 429 (slow), no sooner than slowed, its
// schedule's interval, doubled as InboxInterval doubles it.
func LongPollNext(start time.Time, floor, slowed time.Duration, slow bool) time.Time {
	if slow {
		return start.Add(max(floor, slowed))
	}
	return start.Add(floor)
}

// Jitter spreads d by ±frac, with r in [0, 1): r = 0.5 leaves it as it is.
func Jitter(d time.Duration, frac, r float64) time.Duration {
	if frac <= 0 {
		return d
	}
	return time.Duration(float64(d) * (1 + frac*(2*r-1)))
}

// FirstPoll is when a seat polls first, spread over the idle interval so
// that an agent's seats, or a restarted worker's agents, do not all ask at
// once.
func FirstPoll(p config.Polling, r float64) time.Duration {
	return time.Duration(r * float64(config.Seconds(p.InboxIdleS)))
}

// ProposalFollowUps are when events are read after an answer is proposed:
// at once, then 5, 15 and 45 seconds later (§7.2), before the seat's usual
// events_s takes over.
var ProposalFollowUps = []time.Duration{0, 5 * time.Second, 15 * time.Second, 45 * time.Second}

// Backoff is the n-th wait (from 0) of 1, 2, 4 … max, with full jitter r in
// [0, 1): what a provider or Core that cannot be reached is given (§7.2).
func Backoff(n int, base, maxWait time.Duration, r float64) time.Duration {
	d := base
	for i := 0; i < n && d < maxWait; i++ {
		d *= 2
	}
	if d > maxWait {
		d = maxWait
	}
	return time.Duration(r * float64(d))
}
