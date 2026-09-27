// Package ratelimit keeps one agent's calls to Core under Core's per-actor
// limit (Core's docs/agent-runtime.md §2.5, §7.3): a token bucket that makes
// a caller wait for a token rather than refusing it, and that serves the
// callers waiting by priority, so that answers go before polling and polling
// before events.
//
// Core refuses a call over its limit with a 429 and attempts nothing; a
// runtime that met that limit in steady state would spend its allowance on
// refusals. So the runtime keeps each agent's bucket below Core's: at
// CoreShare of it, 540 calls a minute and bursts of 90 at Core's defaults.
// The caller of New decides the numbers.
package ratelimit

import (
	"container/heap"
	"context"
	"math"
	"sync"
	"time"
)

// CoreShare is the part of Core's per-actor limit an agent's bucket is set
// to (§7.3): New(CoreShare×RATE_LIMIT_PER_MINUTE, CoreShare×RATE_LIMIT_BURST).
// The rest is headroom for clocks that disagree and for the two calls an MCP
// connection makes before its first tool call, which do not pass through the
// bucket.
const CoreShare = 0.9

// Clock is the time a Bucket keeps. Tests give it one they move by hand.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f in its own goroutine once d has passed.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending AfterFunc.
type Timer interface {
	// Stop prevents the call if it has not happened yet.
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Bucket is a token bucket that fills at a steady rate up to its burst.
// Wait takes one token, waiting while there is none; the callers waiting are
// served lowest priority number first, and in the order they came within a
// priority. It is safe for concurrent use.
type Bucket struct {
	clock Clock

	mu        sync.Mutex
	perMinute float64 // <= 0: no limit
	burst     int
	tokens    float64
	filled    time.Time // when tokens was last brought up to date
	queue     waiters
	seq       uint64
	timer     Timer  // pending wake-up for the head of the queue, or nil
	gen       uint64 // which timer is timer: a stopped one may still fire
	granted   uint64
	waited    uint64
	cancelled uint64
}

// New fills perMinute tokens a minute, up to burst, and starts full.
// perMinute <= 0 means no limit, as it does for Core's RATE_LIMIT_PER_MINUTE;
// a burst below 1 is 1.
func New(perMinute float64, burst int) *Bucket {
	return NewWithClock(perMinute, burst, realClock{})
}

// NewWithClock is New on the clock c.
func NewWithClock(perMinute float64, burst int, c Clock) *Bucket {
	burst = max(burst, 1)
	return &Bucket{clock: c, perMinute: perMinute, burst: burst, tokens: float64(burst), filled: c.Now()}
}

// waiter is one caller waiting for a token. ready is closed when it is
// given one.
type waiter struct {
	prio  int
	seq   uint64
	index int // in the heap; -1 once it left the queue
	ready chan struct{}
}

// Wait takes one token for a call of priority prio, lower numbers first,
// waiting while the bucket is empty or callers of the same or a more urgent
// priority wait before it. It returns ctx's error, having taken nothing, if
// ctx ends first; a caller given up on leaves the queue at once.
func (b *Bucket) Wait(ctx context.Context, prio int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	if b.perMinute <= 0 {
		b.granted++
		b.mu.Unlock()
		return nil
	}
	b.fill()
	// Those already waiting were owed the tokens that came while they
	// waited, before this caller came.
	b.dispatch()
	if b.queue.Len() == 0 && b.tokens >= 1-epsilon {
		b.take()
		b.mu.Unlock()
		return nil
	}
	w := &waiter{prio: prio, seq: b.seq, ready: make(chan struct{})}
	b.seq++
	heap.Push(&b.queue, w)
	b.waited++
	b.schedule()
	b.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		b.leave(w)
		return ctx.Err()
	}
}

// leave takes w out of the queue, its context having ended. If it was given
// a token just then, the call will not be made, and the token goes to
// whoever waits next.
func (b *Bucket) leave(w *waiter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled++
	if w.index >= 0 {
		heap.Remove(&b.queue, w.index)
		b.schedule()
		return
	}
	b.fill()
	b.tokens = min(b.tokens+1, float64(b.burst))
	b.granted--
	b.dispatch()
	b.schedule()
}

// SetRate changes the rate and the burst from now on, keeping the tokens
// already earned up to the new burst. Callers waiting are served at the new
// rate; perMinute <= 0 lets them all through.
func (b *Bucket) SetRate(perMinute float64, burst int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fill()
	b.perMinute = perMinute
	b.burst = max(burst, 1)
	b.tokens = min(b.tokens, float64(b.burst))
	b.stopTimer()
	if perMinute <= 0 {
		for b.queue.Len() > 0 {
			b.granted++
			b.grant(heap.Pop(&b.queue).(*waiter))
		}
		return
	}
	b.dispatch()
	b.schedule()
}

// Stats is a Bucket's state, for metrics and the status page.
type Stats struct {
	PerMinute float64 `json:"per_minute"`
	Burst     int     `json:"burst"`
	// Tokens are those available now.
	Tokens float64 `json:"tokens"`
	// Waiting is how many callers wait now.
	Waiting int `json:"waiting"`
	// Granted counts tokens taken; Waited, callers that had to wait;
	// Cancelled, waiting callers whose context ended first.
	Granted   uint64 `json:"granted"`
	Waited    uint64 `json:"waited"`
	Cancelled uint64 `json:"cancelled"`
}

// Stats is the bucket as it stands now.
func (b *Bucket) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fill()
	return Stats{
		PerMinute: b.perMinute, Burst: b.burst, Tokens: b.tokens, Waiting: b.queue.Len(),
		Granted: b.granted, Waited: b.waited, Cancelled: b.cancelled,
	}
}

// epsilon absorbs the rounding of float tokens, so that a timer set for the
// moment a token is whole finds it whole.
const epsilon = 1e-9

// fill adds the tokens earned since the last fill. Called with mu held.
func (b *Bucket) fill() {
	now := b.clock.Now()
	if now.After(b.filled) && b.perMinute > 0 {
		b.tokens = min(b.tokens+now.Sub(b.filled).Minutes()*b.perMinute, float64(b.burst))
	}
	b.filled = now
}

// take spends one token. Called with mu held.
func (b *Bucket) take() {
	b.tokens = max(b.tokens-1, 0)
	b.granted++
}

// dispatch gives the tokens there are to the callers waiting, most urgent
// first. Called with mu held, after fill.
func (b *Bucket) dispatch() {
	for b.queue.Len() > 0 && b.tokens >= 1-epsilon {
		b.take()
		b.grant(heap.Pop(&b.queue).(*waiter))
	}
}

func (b *Bucket) grant(w *waiter) {
	w.index = -1
	close(w.ready)
}

// schedule sets a timer for when the next token is whole, if anyone waits
// for it and no timer is set. Called with mu held.
func (b *Bucket) schedule() {
	if b.queue.Len() == 0 {
		b.stopTimer()
		return
	}
	if b.timer != nil || b.perMinute <= 0 {
		return
	}
	// At a rate so slow that the wait would not fit a Duration, the timer
	// comes back within maxWake and sets another.
	d := maxWake
	if wait := math.Ceil((1 - b.tokens) / b.perMinute * float64(time.Minute)); wait < float64(maxWake) {
		d = time.Duration(wait)
	}
	b.gen++
	gen := b.gen
	b.timer = b.clock.AfterFunc(max(d, 1), func() { b.wake(gen) })
}

// maxWake is the longest a timer is set for.
const maxWake = time.Hour

func (b *Bucket) stopTimer() {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}

// wake is the timer's: the token the head of the queue waited for is whole.
// A timer stopped too late to stop it still comes here, harmlessly: it
// finds nothing to do, or does what the current one would have.
func (b *Bucket) wake(gen uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if gen == b.gen {
		b.timer = nil
	}
	b.fill()
	b.dispatch()
	b.schedule()
}

// waiters is a heap of callers: lowest priority number first, then the
// earliest.
type waiters []*waiter

func (q waiters) Len() int { return len(q) }

func (q waiters) Less(i, j int) bool {
	if q[i].prio != q[j].prio {
		return q[i].prio < q[j].prio
	}
	return q[i].seq < q[j].seq
}

func (q waiters) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}

func (q *waiters) Push(x any) {
	w := x.(*waiter)
	w.index = len(*q)
	*q = append(*q, w)
}

func (q *waiters) Pop() any {
	old := *q
	n := len(old)
	w := old[n-1]
	old[n-1] = nil
	w.index = -1
	*q = old[:n-1]
	return w
}
