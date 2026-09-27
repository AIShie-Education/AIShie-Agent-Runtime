package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// seatWant is a seat as a test expects it; a zero gone means current.
type seatWant struct {
	agent, member, course string
	seen, gone            time.Time
}

// sameSeats compares seats in order, times to the microsecond.
func sameSeats(t *testing.T, what string, got []store.SeatRef, want []seatWant) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %+v, want %d seats", what, got, len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.AgentID != w.agent || g.MemberID != w.member || g.CourseID != w.course {
			t.Errorf("%s[%d] = (%s, %s, %s), want (%s, %s, %s)", what, i, g.AgentID, g.MemberID, g.CourseID, w.agent, w.member, w.course)
		}
		sameTime(t, what+" seen_at", g.SeenAt, w.seen)
		switch {
		case w.gone.IsZero() && g.GoneAt != nil:
			t.Errorf("%s[%d] gone at %s, want current", what, i, *g.GoneAt)
		case !w.gone.IsZero() && g.GoneAt == nil:
			t.Errorf("%s[%d] current, want gone at %s", what, i, us(w.gone))
		case !w.gone.IsZero():
			sameTime(t, what+" gone_at", *g.GoneAt, w.gone)
		}
	}
}

func knownSeats(t *testing.T, s store.Store, agent string) []store.SeatRef {
	t.Helper()
	seats, err := s.KnownSeats(t.Context(), agent)
	if err != nil {
		t.Fatalf("KnownSeats(%s): %v", agent, err)
	}
	return seats
}

func seen(t *testing.T, s store.Store, agent, member, course string, when time.Time) {
	t.Helper()
	if err := s.SeatSeen(t.Context(), agent, member, course, when); err != nil {
		t.Fatalf("SeatSeen(%s, %s): %v", agent, member, err)
	}
}

func gone(t *testing.T, s store.Store, agent, member string, when time.Time) {
	t.Helper()
	if err := s.SeatGone(t.Context(), agent, member, when); err != nil {
		t.Fatalf("SeatGone(%s, %s): %v", agent, member, err)
	}
}

func testSeats(t *testing.T, open Opener) {
	t.Run("seen, gone, gone again, seen again", func(t *testing.T) {
		s := open(t)
		t0, t1, t2, t3 := at(0), at(time.Hour), at(2*time.Hour), at(3*time.Hour)

		seen(t, s, "a1", "m1", "c1", t0)
		sameSeats(t, "after SeatSeen", knownSeats(t, s, "a1"), []seatWant{{"a1", "m1", "c1", t0, time.Time{}}})

		gone(t, s, "a1", "m1", t1)
		sameSeats(t, "after SeatGone", knownSeats(t, s, "a1"), []seatWant{{"a1", "m1", "c1", t0, t1}})

		// Retention counts from when the seat was first missed.
		gone(t, s, "a1", "m1", t2)
		sameSeats(t, "after a second SeatGone", knownSeats(t, s, "a1"), []seatWant{{"a1", "m1", "c1", t0, t1}})

		seen(t, s, "a1", "m1", "c1", t3)
		sameSeats(t, "after SeatSeen again", knownSeats(t, s, "a1"), []seatWant{{"a1", "m1", "c1", t3, time.Time{}}})
	})

	t.Run("a seat never seen cannot go", func(t *testing.T) {
		s := open(t)
		if err := s.SeatGone(t.Context(), "a1", "m1", base); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if seats := knownSeats(t, s, "a1"); len(seats) != 0 {
			t.Fatalf("KnownSeats = %+v, want none", seats)
		}
	})

	t.Run("a zero time is the store's now", func(t *testing.T) {
		s := open(t)
		before := time.Now()
		seen(t, s, "a1", "m1", "c1", time.Time{})
		gone(t, s, "a1", "m1", time.Time{})
		after := time.Now()
		seats := knownSeats(t, s, "a1")
		if len(seats) != 1 || seats[0].GoneAt == nil {
			t.Fatalf("KnownSeats = %+v, want one seat, gone", seats)
		}
		recent(t, "seen_at", seats[0].SeenAt, before, after)
		recent(t, "gone_at", *seats[0].GoneAt, before, after)
	})

	t.Run("KnownSeats lists one agent's seats, current and gone, by member", func(t *testing.T) {
		s := open(t)
		seen(t, s, "a1", "m2", "c2", at(0))
		seen(t, s, "a1", "m1", "c1", at(time.Minute))
		gone(t, s, "a1", "m1", at(time.Hour))
		seen(t, s, "a2", "m3", "c1", at(0))
		sameSeats(t, "KnownSeats(a1)", knownSeats(t, s, "a1"), []seatWant{
			{"a1", "m1", "c1", at(time.Minute), at(time.Hour)},
			{"a1", "m2", "c2", at(0), time.Time{}},
		})
		sameSeats(t, "KnownSeats(a2)", knownSeats(t, s, "a2"), []seatWant{{"a2", "m3", "c1", at(0), time.Time{}}})
		sameSeats(t, "KnownSeats(a3)", knownSeats(t, s, "a3"), nil)
	})

	t.Run("SeatsGoneBefore spans agents, and takes only seats gone before the time", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		cutoff := at(10 * time.Hour)
		seen(t, s, "a2", "m1", "c1", at(0))
		gone(t, s, "a2", "m1", at(2*time.Hour))
		seen(t, s, "a1", "m1", "c1", at(0))
		gone(t, s, "a1", "m1", at(time.Hour))
		seen(t, s, "a1", "m2", "c1", at(0))
		gone(t, s, "a1", "m2", cutoff) // gone at the time, not before it
		seen(t, s, "a1", "m3", "c2", at(0))
		gone(t, s, "a1", "m3", at(11*time.Hour))
		seen(t, s, "a2", "m2", "c2", at(0)) // current

		got, err := s.SeatsGoneBefore(ctx, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		sameSeats(t, "SeatsGoneBefore", got, []seatWant{
			{"a1", "m1", "c1", at(0), at(time.Hour)},
			{"a2", "m1", "c1", at(0), at(2 * time.Hour)},
		})
	})

	t.Run("ForgetSeat removes the seat's row, and only that", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		seen(t, s, "a1", "m1", "c1", base)
		seen(t, s, "a1", "m2", "c1", base)
		seen(t, s, "a2", "m1", "c1", base)
		if err := s.ForgetSeat(ctx, "a1", "m1"); err != nil {
			t.Fatal(err)
		}
		if err := s.ForgetSeat(ctx, "a1", "m1"); err != nil {
			t.Fatalf("forgetting a seat again: %v", err)
		}
		sameSeats(t, "KnownSeats(a1)", knownSeats(t, s, "a1"), []seatWant{{"a1", "m2", "c1", base, time.Time{}}})
		sameSeats(t, "KnownSeats(a2)", knownSeats(t, s, "a2"), []seatWant{{"a2", "m1", "c1", base, time.Time{}}})
	})

	t.Run("refuses a seat without an agent or member", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for _, c := range [][2]string{{"", "m1"}, {"a1", ""}} {
			if err := s.SeatSeen(ctx, c[0], c[1], "c1", base); err == nil {
				t.Errorf("SeatSeen(%q, %q) was taken", c[0], c[1])
			}
		}
	})
}
