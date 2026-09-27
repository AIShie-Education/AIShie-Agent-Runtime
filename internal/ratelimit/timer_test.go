package ratelimit

import (
	"context"
	"testing"
	"time"
)

// timersClock records how long each timer is set for, and fires none.
type timersClock struct {
	stillClock
	set []time.Duration
}

func (c *timersClock) AfterFunc(d time.Duration, _ func()) Timer {
	c.set = append(c.set, d)
	return stopper{}
}

// The timer is set for when the next token is whole; at a rate too slow for
// that to fit a Duration, for maxWake, never for a moment (which would spin).
func TestTimerIsSetForTheNextToken(t *testing.T) {
	for _, tc := range []struct {
		perMinute float64
		want      time.Duration
	}{
		{60, time.Second},
		{6000, 10 * time.Millisecond},
		{1, time.Minute},
		{0.001, maxWake},
		{1e-15, maxWake},
	} {
		clock := &timersClock{stillClock: stillClock{now: time.Unix(0, 0)}}
		b := NewWithClock(tc.perMinute, 1, clock)
		if err := b.Wait(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error)
		go func() { done <- b.Wait(ctx, 0) }()
		deadline := time.Now().Add(time.Second)
		for b.Stats().Waiting == 0 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Microsecond)
		}
		cancel()
		if err := <-done; err == nil {
			t.Fatalf("%v a minute: a token came with the clock standing still", tc.perMinute)
		}
		if len(clock.set) != 1 || clock.set[0] != tc.want {
			t.Errorf("%v a minute: timers %v, want [%s]", tc.perMinute, clock.set, tc.want)
		}
	}
}
