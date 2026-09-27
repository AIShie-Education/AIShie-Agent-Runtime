package ratelimit

import (
	"container/heap"
	"testing"
	"time"
)

type stillClock struct{ now time.Time }

func (c *stillClock) Now() time.Time { return c.now }

func (c *stillClock) AfterFunc(time.Duration, func()) Timer { return stopper{} }

type stopper struct{}

func (stopper) Stop() bool { return true }

// A caller given its token in the same moment its context ended does not
// make the call, so the token must go back: to the next caller waiting, or
// to the bucket.
func TestLeavingWithATokenGivesItBack(t *testing.T) {
	b := NewWithClock(60, 1, &stillClock{now: time.Unix(0, 0)})
	a := &waiter{prio: 0, seq: 0, ready: make(chan struct{})}
	next := &waiter{prio: 1, seq: 1, ready: make(chan struct{})}
	b.mu.Lock()
	heap.Push(&b.queue, a)
	heap.Push(&b.queue, next)
	b.dispatch() // the one token goes to a
	b.mu.Unlock()
	select {
	case <-a.ready:
	default:
		t.Fatal("a was not given the token")
	}

	b.leave(a)
	select {
	case <-next.ready:
	default:
		t.Fatal("the token a gave back did not go to the next caller")
	}
	if s := b.Stats(); s.Granted != 1 || s.Cancelled != 1 || s.Waiting != 0 {
		t.Fatalf("stats: %+v", s)
	}

	b.leave(next)
	if s := b.Stats(); s.Tokens != 1 || s.Granted != 0 {
		t.Fatalf("with nobody waiting the token goes back to the bucket: %+v", s)
	}
}
