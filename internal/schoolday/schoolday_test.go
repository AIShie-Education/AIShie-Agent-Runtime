package schoolday

import (
	"testing"
	"time"
)

func TestDayAndCivilDate(t *testing.T) {
	hk, err := time.LoadLocation("Asia/Hong_Kong")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 16, 30, 0, 0, time.UTC) // 00:30 on 2 October in Hong Kong.
	wantDay := time.Date(2026, 10, 2, 0, 0, 0, 0, hk)
	if got := Day(at, hk); !got.Equal(wantDay) || got.Location() != hk {
		t.Errorf("Day = %v (%v), want %v", got, got.Location(), wantDay)
	}
	if got := Day(at, hk).AddDate(0, 0, 1); !got.Equal(time.Date(2026, 10, 3, 0, 0, 0, 0, hk)) {
		t.Errorf("the next school day = %v", got)
	}
	if got := CivilDate(at, hk); !got.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("CivilDate = %v", got)
	}
	if got := Day(at, nil); !got.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the unset zone did not stay UTC: %v", got)
	}
	if got := Days(at, at.AddDate(0, 0, 1), hk); got != 1 {
		t.Errorf("Days = %d, want 1", got)
	}
}

func TestDayAcrossDaylightSavingChanges(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		at   time.Time
		day  time.Time
		next time.Time
	}{
		{"spring forward", time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 3, 8, 0, 0, 0, 0, ny), time.Date(2026, 3, 9, 0, 0, 0, 0, ny)},
		{"fall back", time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 1, 0, 0, 0, 0, ny), time.Date(2026, 11, 2, 0, 0, 0, 0, ny)},
	} {
		t.Run(c.name, func(t *testing.T) {
			day := Day(c.at, ny)
			if !day.Equal(c.day) {
				t.Fatalf("Day = %v, want %v", day, c.day)
			}
			next := day.AddDate(0, 0, 1)
			if !next.Equal(c.next) || Days(day, next, ny) != 1 {
				t.Errorf("next = %v (after %v), want %v and one calendar day", next, next.Sub(day), c.next)
			}
		})
	}
}
