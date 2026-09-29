package fakecore

import (
	"sync"
	"time"
)

// limiter is Core's per-actor token bucket (internal/ratelimit): perMinute
// calls a minute, bursts of up to burst, a refusal taking nothing. MCP and
// REST share it, as they share it in Core.
type limiter struct {
	perSecond float64
	burst     float64
	now       func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// newLimiter allows perMinute calls a minute per key; perMinute <= 0 is no
// limit, a nil limiter.
func newLimiter(perMinute, burst int, now func() time.Time) *limiter {
	if perMinute <= 0 {
		return nil
	}
	return &limiter{perSecond: float64(perMinute) / 60, burst: float64(max(burst, 1)), now: now, buckets: map[string]*bucket{}}
}

// refund gives key back the token a call took.
func (l *limiter) refund(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.buckets[key]; b != nil {
		b.tokens = min(b.tokens+1, l.burst)
	}
}

// allow reports whether key may call now and, if not, how long until it
// may.
func (l *limiter) allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
	}
	if now.After(b.seen) {
		b.tokens = min(b.tokens+now.Sub(b.seen).Seconds()*l.perSecond, l.burst)
		b.seen = now
	}
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.perSecond * float64(time.Second))
	}
	b.tokens--
	return true, 0
}
