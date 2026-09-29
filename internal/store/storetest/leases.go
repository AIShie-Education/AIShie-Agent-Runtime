package storetest

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

func testLeases(t *testing.T, open Opener) {
	t.Run("acquired when free, renewed by its holder, refused to another", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		const name = "agent:a1"
		for _, step := range []struct {
			holder string
			want   bool
		}{
			{"w1", true},  // free
			{"w1", true},  // its holder renews it
			{"w2", false}, // live, and someone else's
			{"w1", true},
		} {
			got, err := s.AcquireLease(ctx, name, step.holder, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if got != step.want {
				t.Fatalf("AcquireLease(%s, %s) = %v, want %v", name, step.holder, got, step.want)
			}
		}
	})

	t.Run("released only by its holder", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		const name = "conv:a1:x1"
		mustAcquire(t, s, name, "w1", time.Minute, true)
		// Another's release is nothing, and says nothing.
		if err := s.ReleaseLease(ctx, name, "w2"); err != nil {
			t.Fatalf("release by another: %v", err)
		}
		mustAcquire(t, s, name, "w3", time.Minute, false)
		if err := s.ReleaseLease(ctx, name, "w1"); err != nil {
			t.Fatal(err)
		}
		mustAcquire(t, s, name, "w2", time.Minute, true)
		// Releasing what nobody holds is nothing too.
		if err := s.ReleaseLease(ctx, "conv:a1:none", "w1"); err != nil {
			t.Fatalf("release of a lease never taken: %v", err)
		}
	})

	t.Run("taken over once it expires", func(t *testing.T) {
		s := open(t)
		const name = "agent:a2"
		mustAcquire(t, s, name, "w1", 5*time.Millisecond, true)
		waitAcquire(t, s, name, "w2")
		// w1 has lost it: it is refused, and its release frees nothing of
		// w2's.
		mustAcquire(t, s, name, "w1", time.Minute, false)
		if err := s.ReleaseLease(t.Context(), name, "w1"); err != nil {
			t.Fatal(err)
		}
		mustAcquire(t, s, name, "w3", time.Minute, false)
	})

	t.Run("a renewal extends it", func(t *testing.T) {
		s := open(t)
		const name = "agent:a3"
		mustAcquire(t, s, name, "w1", 20*time.Millisecond, true)
		mustAcquire(t, s, name, "w1", time.Minute, true)
		time.Sleep(40 * time.Millisecond)
		mustAcquire(t, s, name, "w2", time.Minute, false)
	})

	t.Run("names are independent", func(t *testing.T) {
		s := open(t)
		mustAcquire(t, s, "conv:a1:x1", "w1", time.Minute, true)
		mustAcquire(t, s, "conv:a1:x2", "w2", time.Minute, true)
		mustAcquire(t, s, "conv:a2:x1", "w2", time.Minute, true)
		mustAcquire(t, s, "conv:a1:x1", "w2", time.Minute, false)
	})

	t.Run("one of many racing holders wins", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		const racers = 20
		var (
			wg    sync.WaitGroup
			wins  atomic.Int32
			start = make(chan struct{})
			errs  = make(chan error, racers)
		)
		for i := range racers {
			wg.Go(func() {
				<-start
				ok, err := s.AcquireLease(ctx, "agent:raced", holderName(i), time.Minute)
				if err != nil {
					errs <- err
					return
				}
				if ok {
					wins.Add(1)
				}
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if n := wins.Load(); n != 1 {
			t.Fatalf("%d of %d racers hold the lease, want exactly one", n, racers)
		}
	})

	t.Run("one of many racing holders takes over an expired lease", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		const (
			name   = "agent:lapsed"
			racers = 20
		)
		mustAcquire(t, s, name, "dead-worker", time.Millisecond, true)
		// Long past its millisecond on any clock, the process's or the
		// database's: both move on with real time.
		time.Sleep(20 * time.Millisecond)
		var (
			wg     sync.WaitGroup
			start  = make(chan struct{})
			errs   = make(chan error, racers)
			winner = make(chan string, racers)
		)
		for i := range racers {
			wg.Go(func() {
				<-start
				ok, err := s.AcquireLease(ctx, name, holderName(i), time.Minute)
				switch {
				case err != nil:
					errs <- err
				case ok:
					winner <- holderName(i)
				}
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		close(winner)
		for err := range errs {
			t.Error(err)
		}
		var won []string
		for w := range winner {
			won = append(won, w)
		}
		if len(won) != 1 {
			t.Fatalf("%v took over the expired lease, want exactly one racer", won)
		}
		// The winner holds it: it renews, and the dead worker and the other
		// racers are refused.
		mustAcquire(t, s, name, won[0], time.Minute, true)
		mustAcquire(t, s, name, "dead-worker", time.Minute, false)
		for i := range racers {
			if holderName(i) != won[0] {
				mustAcquire(t, s, name, holderName(i), time.Minute, false)
			}
		}
	})

	t.Run("refuses a lease without a name or holder, or that never lasts", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for _, c := range []struct {
			name, holder string
			ttl          time.Duration
		}{
			{"", "w1", time.Minute},
			{"agent:a1", "", time.Minute},
			{"agent:a1", "w1", 0},
			{"agent:a1", "w1", -time.Second},
		} {
			if ok, err := s.AcquireLease(ctx, c.name, c.holder, c.ttl); err == nil || ok {
				t.Errorf("AcquireLease(%q, %q, %s) = %v, %v; want an error", c.name, c.holder, c.ttl, ok, err)
			}
		}
		// Nothing was taken by the refusals.
		mustAcquire(t, s, "agent:a1", "w2", time.Minute, true)
	})
}

// mustAcquire fails unless AcquireLease reports want.
func mustAcquire(t *testing.T, s store.Store, name, holder string, ttl time.Duration, want bool) {
	t.Helper()
	got, err := s.AcquireLease(t.Context(), name, holder, ttl)
	if err != nil {
		t.Fatalf("AcquireLease(%s, %s): %v", name, holder, err)
	}
	if got != want {
		t.Fatalf("AcquireLease(%s, %s) = %v, want %v", name, holder, got, want)
	}
}

// waitAcquire tries until holder has name, for as long as an expiry of a
// few milliseconds may take to be seen on a busy machine.
func waitAcquire(t *testing.T, s store.Store, name, holder string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok, err := s.AcquireLease(t.Context(), name, holder, time.Minute)
		if err != nil {
			t.Fatalf("AcquireLease(%s, %s): %v", name, holder, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never took over %s after it expired", holder, name)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func holderName(i int) string { return fmt.Sprintf("worker-%02d", i) }
