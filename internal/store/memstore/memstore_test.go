package memstore

import (
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/storetest"
)

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s := New()
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		return s
	})
}

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// A lease lasts exactly its ttl on the store's clock: held at its last
// instant, and taken over only once past it, as pgstore's expires_at < now().
func TestLeaseExpiresOnTheStoresClock(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.September, 27, 9, 0, 0, 0, time.UTC)}
	s := New()
	s.SetClock(clock.Now)
	ctx := t.Context()

	acquire := func(holder string, want bool) {
		t.Helper()
		got, err := s.AcquireLease(ctx, "agent:a1", holder, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("at %s, AcquireLease(%s) = %v, want %v", clock.Now().Format(time.TimeOnly), holder, got, want)
		}
	}

	acquire("w1", true)
	clock.Advance(30 * time.Second)
	acquire("w2", false) // its last instant
	clock.Advance(time.Nanosecond)
	acquire("w2", true)
	clock.Advance(20 * time.Second)
	acquire("w2", true) // renewed: 30 s from now
	clock.Advance(20 * time.Second)
	acquire("w1", false)
}

// Leases long expired are dropped, so that a holder that never releases
// does not grow the store for ever; one still live is kept.
func TestExpiredLeasesAreSwept(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, time.September, 27, 9, 0, 0, 0, time.UTC)}
	s := New()
	s.SetClock(clock.Now)
	ctx := t.Context()
	for _, name := range []string{"conv:a1:x1", "conv:a1:x2", "conv:a1:x3"} {
		if _, err := s.AcquireLease(ctx, name, "w1", time.Second); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2 * time.Minute)
	if _, err := s.AcquireLease(ctx, "agent:a1", "w1", time.Hour); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	n := len(s.leases)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d leases kept, want only the live one", n)
	}
}

// A zero time is filled from the store's clock, and times from any zone
// come back in UTC, to the microsecond, as Postgres gives them.
func TestTimesAreKeptAsPostgresKeepsThem(t *testing.T) {
	now := time.Date(2026, time.September, 27, 9, 0, 0, 987654321, time.UTC)
	s := New()
	s.SetClock(func() time.Time { return now })
	ctx := t.Context()

	tokyo := time.FixedZone("JST", 9*60*60)
	given := time.Date(2026, time.September, 27, 18, 0, 0, 123456789, tokyo)
	if err := s.SeatSeen(ctx, store.SeatRef{AgentID: "a1", MemberID: "m1", CourseID: "c1", SeenAt: given}); err != nil {
		t.Fatal(err)
	}
	if err := s.SeatGone(ctx, "a1", "m1", time.Time{}); err != nil {
		t.Fatal(err)
	}
	seats, err := s.KnownSeats(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 1 || seats[0].GoneAt == nil {
		t.Fatalf("KnownSeats = %+v, want one seat, gone", seats)
	}
	if want := time.Date(2026, time.September, 27, 9, 0, 0, 123456000, time.UTC); seats[0].SeenAt != want {
		t.Errorf("seen_at = %v, want %v", seats[0].SeenAt, want)
	}
	if want := time.Date(2026, time.September, 27, 9, 0, 0, 987654000, time.UTC); *seats[0].GoneAt != want {
		t.Errorf("gone_at = %v, want %v", *seats[0].GoneAt, want)
	}

	// The pointer handed out is the caller's own.
	*seats[0].GoneAt = time.Time{}
	seats, err = s.KnownSeats(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if seats[0].GoneAt.IsZero() {
		t.Fatal("a caller changed the store's gone_at through the pointer it was given")
	}
}
