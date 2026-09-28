package storetest

import (
	"errors"
	"reflect"
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
	if err := s.SeatSeen(t.Context(), store.SeatRef{AgentID: agent, MemberID: member, CourseID: course, SeenAt: when}); err != nil {
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

	t.Run("one agent's seat is never another's, under one member id", func(t *testing.T) {
		s := open(t)
		t0, t1, t2, t3 := at(0), at(time.Hour), at(2*time.Hour), at(3*time.Hour)
		seen(t, s, "a1", "m1", "c1", t0)
		seen(t, s, "a2", "m1", "c2", t0)

		gone(t, s, "a1", "m1", t1)
		sameSeats(t, "KnownSeats(a2) after a1's seat went", knownSeats(t, s, "a2"), []seatWant{{"a2", "m1", "c2", t0, time.Time{}}})

		gone(t, s, "a2", "m1", t2)
		sameSeats(t, "KnownSeats(a1) after a2's seat went", knownSeats(t, s, "a1"), []seatWant{{"a1", "m1", "c1", t0, t1}})

		seen(t, s, "a1", "m1", "c1", t3)
		sameSeats(t, "KnownSeats(a2) after a1's seat came back", knownSeats(t, s, "a2"), []seatWant{{"a2", "m1", "c2", t0, t2}})
		sameSeats(t, "KnownSeats(a1) after it came back", knownSeats(t, s, "a1"), []seatWant{{"a1", "m1", "c1", t3, time.Time{}}})
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

	t.Run("the seat as me_memberships last showed it", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		tutor := store.SeatRef{
			AgentID: "a1", MemberID: "m1", CourseID: "c1", CourseCode: "CS101", CourseTitle: "Introduction to Computing",
			Section: "A", Status: "active", CourseStatus: "active", AnswersCourse: true,
			Perms: map[string]string{"conversation_answer": "confirm_required", "document_read": "autonomous"}, SeenAt: at(0),
		}
		own := store.SeatRef{
			AgentID: "a1", MemberID: "m2", CourseID: "c2", CourseCode: "MA201", CourseTitle: "Linear Algebra",
			Status: "active", PrincipalMemberID: "p1", SeenAt: at(0),
		}
		for _, r := range []store.SeatRef{tutor, own} {
			if err := s.SeatSeen(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		got := knownSeats(t, s, "a1")
		own.Perms = map[string]string{} // none are none
		for i, want := range []store.SeatRef{tutor, own} {
			sameTime(t, "seen_at", got[i].SeenAt, want.SeenAt)
			got[i].SeenAt, want.SeenAt = time.Time{}, time.Time{}
			if !reflect.DeepEqual(got[i], want) {
				t.Errorf("KnownSeats[%d]:\n got %+v\nwant %+v", i, got[i], want)
			}
		}
		// Read again, the seat is as it is now: paused, and tutoring no
		// more, its perms changed.
		changed := tutor
		changed.Status, changed.CourseStatus, changed.AnswersCourse, changed.SeenAt = "paused", "archived", false, at(time.Hour)
		changed.Perms = map[string]string{"conversation_answer": "denied"}
		if err := s.SeatSeen(ctx, changed); err != nil {
			t.Fatal(err)
		}
		got = knownSeats(t, s, "a1")
		if got[0].Status != "paused" || got[0].CourseStatus != "archived" || got[0].AnswersCourse || !reflect.DeepEqual(got[0].Perms, changed.Perms) {
			t.Errorf("after a second read: %+v", got[0])
		}
		// What a caller does to the perms it got is its own.
		got[0].Perms["conversation_answer"] = "autonomous"
		if again := knownSeats(t, s, "a1"); again[0].Perms["conversation_answer"] != "denied" {
			t.Error("a caller changed the store's perms through the map it was given")
		}
		// Gone, it keeps what it was.
		gone(t, s, "a1", "m1", at(2*time.Hour))
		went, err := s.SeatsGoneBefore(ctx, at(3*time.Hour))
		if err != nil || len(went) != 1 || went[0].CourseCode != "CS101" || went[0].Status != "paused" {
			t.Errorf("SeatsGoneBefore = %+v, %v", went, err)
		}
	})

	t.Run("refuses a seat without an agent or member", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for _, c := range [][2]string{{"", "m1"}, {"a1", ""}} {
			if err := s.SeatSeen(ctx, store.SeatRef{AgentID: c[0], MemberID: c[1], CourseID: "c1", SeenAt: base}); err == nil {
				t.Errorf("SeatSeen(%q, %q) was taken", c[0], c[1])
			}
		}
	})
}
