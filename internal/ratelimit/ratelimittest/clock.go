// Package ratelimittest is a clock for tests of what waits on a
// ratelimit.Bucket: time moves only when the test says so, and the timers
// that fall due fire in order, in the test's goroutine.
package ratelimittest

import (
	"sort"
	"sync"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
)

// Clock is a ratelimit.Clock moved by Advance.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*timer
	seq    int
}

var _ ratelimit.Clock = (*Clock)(nil)

// New starts a clock at a fixed time.
func New() *Clock {
	return &Clock{now: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)}
}

type timer struct {
	c       *Clock
	at      time.Time
	seq     int
	f       func()
	stopped bool
}

// Stop prevents the timer from firing.
func (t *timer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

// Now is the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc schedules f for d from now. Advance calls it.
func (c *Clock) AfterFunc(d time.Duration, f func()) ratelimit.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{c: c, at: c.now.Add(d), seq: c.seq, f: f}
	c.seq++
	c.timers = append(c.timers, t)
	return t
}

// Pending is how many timers wait to fire.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped {
			n++
		}
	}
	return n
}

// Advance moves the clock on by d, firing each timer that falls due at its
// own time, earliest first, including those the timers set while firing.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		sort.SliceStable(c.timers, func(i, j int) bool {
			if !c.timers[i].at.Equal(c.timers[j].at) {
				return c.timers[i].at.Before(c.timers[j].at)
			}
			return c.timers[i].seq < c.timers[j].seq
		})
		var due *timer
		for i, t := range c.timers {
			if t.stopped {
				continue
			}
			if !t.at.After(end) {
				due = t
				c.timers = append(c.timers[:i:i], c.timers[i+1:]...)
			}
			break
		}
		if due == nil {
			c.now = end
			c.timers = live(c.timers)
			c.mu.Unlock()
			return
		}
		if due.at.After(c.now) {
			c.now = due.at
		}
		due.stopped = true
		c.mu.Unlock()
		due.f()
	}
}

func live(ts []*timer) []*timer {
	out := ts[:0]
	for _, t := range ts {
		if !t.stopped {
			out = append(out, t)
		}
	}
	return out
}
