package api

import (
	"container/list"
	"math"
	"sync"
	"time"
)

// Rate is a token bucket's rate and burst.
type Rate struct {
	PerMinute int
	Burst     int
}

// The buckets of the API contract (§3.4).
var (
	// RatePerIP is each client address's allowance of unauthenticated
	// requests and of requests whose assertion was refused.
	RatePerIP = Rate{PerMinute: 120, Burst: 60}
	// RateFailures is each client address's allowance of refused
	// assertions alone.
	RateFailures = Rate{PerMinute: 30, Burst: 30}
	// RateGeneral is each person's allowance of authenticated requests.
	RateGeneral = Rate{PerMinute: 120, Burst: 40}
	// RateToken is each person's allowance of requests that ask Core about
	// a token: inspect, POST /agents, PUT /token.
	RateToken = Rate{PerMinute: 10, Burst: 5}
	// RateKeyTest is each person's allowance of keys/test, beside
	// KeyTestsPerDay.
	RateKeyTest = Rate{PerMinute: 6, Burst: 3}
)

// KeyTestsPerDay is each person's allowance of keys/test in a UTC day:
// each spends a token of their own key, and asks a provider.
const KeyTestsPerDay = 100

// dailyLimiter counts each key's requests in the current UTC day, up to a
// limit, bounded by maxBuckets as limiter is.
type dailyLimiter struct {
	limit int

	mu    sync.Mutex
	day   time.Time
	count map[string]int
}

func newDailyLimiter(limit int) *dailyLimiter {
	return &dailyLimiter{limit: limit, count: map[string]int{}}
}

// take counts one of key's requests at now: ok, or how many whole seconds
// until the next UTC day.
func (d *dailyLimiter) take(key string, now time.Time) (bool, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	y, m, day := now.UTC().Date()
	today := time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
	if !today.Equal(d.day) {
		d.day, d.count = today, map[string]int{}
	}
	if d.count[key] >= d.limit {
		return false, int(math.Ceil(today.Add(24 * time.Hour).Sub(now).Seconds()))
	}
	if _, ok := d.count[key]; !ok && len(d.count) >= maxBuckets {
		// Past the bound, a new key is refused rather than an old one
		// forgotten: forgetting a count would give its key a new day.
		return false, int(math.Ceil(today.Add(24 * time.Hour).Sub(now).Seconds()))
	}
	d.count[key]++
	return true, 0
}

// maxBuckets bounds each limiter: past it, the bucket used least recently
// goes, which forgets only how little of its allowance it had left.
const maxBuckets = 10000

// limiter is a token bucket per key (a person, or a client address),
// bounded by maxBuckets, least recently used first out. A request over it
// is refused, with how long until the next token, never made to wait.
type limiter struct {
	perSec float64
	burst  float64

	mu    sync.Mutex
	byKey map[string]*list.Element
	order *list.List // of *bucket, most recently used at the front
}

type bucket struct {
	key    string
	tokens float64
	at     time.Time
}

func newLimiter(r Rate) *limiter {
	return &limiter{perSec: float64(r.PerMinute) / 60, burst: float64(r.Burst), byKey: map[string]*list.Element{}, order: list.New()}
}

// take takes a token of key's at now: ok, or how many whole seconds until
// there is one.
func (l *limiter) take(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(key, now)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, int(math.Ceil((1 - b.tokens) / l.perSec))
}

// bucket is key's bucket, filled as it stands at now, made when there is
// none. Called with the lock held.
func (l *limiter) bucket(key string, now time.Time) *bucket {
	if e, ok := l.byKey[key]; ok {
		l.order.MoveToFront(e)
		b := e.Value.(*bucket)
		if now.After(b.at) {
			b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.perSec)
			b.at = now
		}
		return b
	}
	if l.order.Len() >= maxBuckets {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.byKey, oldest.Value.(*bucket).key)
	}
	b := &bucket{key: key, tokens: l.burst, at: now}
	l.byKey[key] = l.order.PushFront(b)
	return b
}

// size is how many buckets are kept.
func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}
