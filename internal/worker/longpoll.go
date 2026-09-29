package worker

import (
	"errors"
	"math"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
)

// Long polling (Core 2c1fe1b; its docs/agent-runtime.md §7.2, design
// §5.2). Where Core's catalogue offers wait_s on conversation_inbox, a
// seat's inbox call waits for a question (polling.long_poll_wait_s, 25 s by
// default) and is made again at once each time it comes back with nothing,
// so that a question is noticed within milliseconds of being asked. The
// agent's seats together keep at most polling.long_poll_max such calls
// waiting (12 by default, below Core's 16 per actor), the most recently
// active seats first; the rest, and every seat against an older Core, poll
// on the schedule of schedule.go. So does a seat for a while
// (Timing.LongPollFallback, a minute) after Core did not wait (an empty answer in under half the wait: Core's
// bound on one actor's calls waiting, or on all, or Core shutting down),
// after Core refused wait_s (an older Core behind the same URL), and after
// a long poll that did not come back (a proxy that cuts a request shorter
// than its wait); then it tries again.
//
// After an answer is proposed, the seat's events are read the same way
// for the follow-ups' window (ProposalFollowUps), where a place is left
// over by the inboxes: a decision made meanwhile is seen at once instead
// of at the next of the 0, 5, 15 and 45 s reads, which stand where no
// place is free. The background read every events_s stays: only the feed
// tells a proposal approved later, or a message retracted.

// Why a seat went back to its schedule, as long_poll_fallbacks_total says.
const (
	fallbackEarly   = "early"
	fallbackCut     = "cut"
	fallbackRefused = "refused"
)

// longPollWait is the wait p asks for, at most offered, Core's: whole
// seconds, rounded up; 0 when either is none.
func longPollWait(p config.Polling, offered time.Duration) time.Duration {
	if p.LongPollWaitS <= 0 || offered <= 0 {
		return 0
	}
	return min(time.Duration(math.Ceil(p.LongPollWaitS))*time.Second, offered)
}

// inboxWaitLocked is how long the seat's next inbox call may wait for
// news at now: its course's long_poll_wait_s, at most what Core offers;
// 0 while it falls back to its schedule. s.mu is held.
func (s *Seat) inboxWaitLocked(now time.Time) time.Duration {
	if now.Before(s.fallbackUntil) {
		return 0
	}
	return longPollWait(s.eff.Polling, s.a.inboxMaxWait)
}

func (s *Seat) inboxWait(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inboxWaitLocked(now)
}

// eventsWait is how long the seat's next read of events may wait for news
// at now: while the follow-ups' window after a proposal is open, until it
// closes and at most its course's long_poll_wait_s and what Core offers; 0
// otherwise, while it falls back, and while the agent is slowed after a
// 429.
func (s *Seat) eventsWait(now time.Time) time.Duration {
	s.mu.Lock()
	left, fallback, p := s.followUntil.Sub(now), now.Before(s.eventsFallbackUntil), s.eff.Polling
	s.mu.Unlock()
	if left < time.Second || fallback || s.a.slow() {
		return 0
	}
	return min(longPollWait(p, s.a.eventsMaxWait), left.Truncate(time.Second))
}

// longPollCandidate is when the seat was last active, and whether it would
// long-poll its inbox at now if it had a place. The agent's mu is held.
func (s *Seat) longPollCandidate(now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActive, s.hold == nil && s.inboxWaitLocked(now) > 0
}

// activity is when the seat was last active: found a question, posted an
// answer, or saw its opener write.
func (s *Seat) activity() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActive
}

// takeLongPoll takes one of the agent's long_poll_max places for a call of
// seat s's that waits for news, and reports whether it got one. An inbox
// call gets one while fewer than long_poll_max are taken, and fewer than
// long_poll_max of the seats that would long-poll their inbox were active
// more recently than s (ties go by member id): the most recently active
// seats long-poll, and the rest poll on their schedule. A seat that loses
// its rank gives its place back when its call returns. A read of events
// (events) gets one only while more are free than the inboxes would take.
func (a *Agent) takeLongPoll(s *Seat, events bool) bool {
	most := a.cfg.Polling.LongPollMax
	now := a.now()
	mine := s.activity()
	a.mu.Lock()
	defer a.mu.Unlock()
	if most <= 0 || a.longPolls+a.eventLongPolls >= most {
		return false
	}
	candidates, above := 0, 0
	for _, o := range a.seats {
		act, ok := o.longPollCandidate(now)
		if !ok {
			continue
		}
		candidates++
		if o != s && (act.After(mine) || act.Equal(mine) && o.id < s.id) {
			above++
		}
	}
	switch {
	case events && a.eventLongPolls+max(min(candidates, most), a.longPolls) >= most:
		return false
	case events:
		a.eventLongPolls++
	case above >= most:
		return false
	default:
		a.longPolls++
	}
	a.s.o.Metrics.LongPolls.WithLabelValues(a.id).Set(float64(a.longPolls + a.eventLongPolls))
	return true
}

// giveLongPoll gives back a place takeLongPoll took.
func (a *Agent) giveLongPoll(events bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if events {
		a.eventLongPolls--
	} else {
		a.longPolls--
	}
	a.s.o.Metrics.LongPolls.WithLabelValues(a.id).Set(float64(a.longPolls + a.eventLongPolls))
}

// longPollsNow is how many of the agent's calls wait for news now.
func (a *Agent) longPollsNow() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.longPolls + a.eventLongPolls
}

// refusesWait reports whether err is Core refusing wait_s: invalid_argument
// naming it, as a Core from before 2c1fe1b answers a call that gives it,
// whose input schemas take no argument they do not name ("unexpected
// additional properties [\"wait_s\"]"), over MCP in an envelope and over
// REST with a 400. Nothing was attempted: the call is made again without
// wait_s, and is no error.
func refusesWait(err error) bool {
	var ee *core.EnvelopeError
	return errors.As(err, &ee) && ee.Envelope.Code() == core.CodeInvalidArgument &&
		ee.Envelope.Error != nil && strings.Contains(ee.Envelope.Error.Message, "wait_s")
}

// longPollCut reports whether a call that asked to wait, and came back
// with err, did not come back: a transient failure, which Retrying does not
// send again for a long poll. The seat polls on its schedule for a while.
func longPollCut(err error) bool {
	var te *core.TransientError
	return errors.As(err, &te)
}

// fallBack has the seat's inbox (or, with events, its reads of events)
// polled on the schedule for Timing.LongPollFallback from now, for why.
func (s *Seat) fallBack(events bool, why string) {
	now := s.a.now()
	until := now.Add(Jitter(s.a.s.o.Timing.LongPollFallback, s.polling().Jitter, s.a.rand()))
	s.mu.Lock()
	if events {
		s.eventsFallbackUntil = until
	} else {
		s.fallbackUntil = until
	}
	s.mu.Unlock()
	s.a.s.o.Metrics.LongPollFallbacks.WithLabelValues(s.a.id, why).Inc()
	what := "conversation_inbox"
	if events {
		what = "event_list"
	}
	msg := map[string]string{
		fallbackEarly:   "Core answered a long poll at once with nothing, so did not wait (its bound on calls waiting reached, or it is stopping)",
		fallbackCut:     "a long poll did not come back: something between the runtime and Core may cut requests shorter than wait_s plus 15 s",
		fallbackRefused: "Core refused wait_s: it is older than the catalogue the runtime read",
	}[why]
	s.log.Info(msg+"; polling on the schedule for a while", "tool", what, "why", why, "until", until.UTC().Format(time.RFC3339))
}
