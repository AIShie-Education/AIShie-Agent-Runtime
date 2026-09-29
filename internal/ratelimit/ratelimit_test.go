package ratelimit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit/ratelimittest"
)

// waitFor polls cond for up to a second: the goroutines under test reach
// the queue on their own schedule, never by sleeping for a fixed time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// queue starts a caller of priority prio that reports id on done once it
// has its token, and returns once it waits.
func queue(t *testing.T, b *ratelimit.Bucket, ctx context.Context, prio, id int, done chan<- int, errs chan<- error) {
	t.Helper()
	before := b.Stats().Waiting
	go func() {
		if err := b.Wait(ctx, prio); err != nil {
			errs <- err
			return
		}
		done <- id
	}()
	waitFor(t, "the caller waits", func() bool { return b.Stats().Waiting == before+1 })
}

func TestBurstThenSteadyRate(t *testing.T) {
	clock := ratelimittest.New()
	b := ratelimit.NewWithClock(60, 3, clock) // one a second, bursts of three
	ctx := context.Background()
	for i := range 3 {
		if err := b.Wait(ctx, 0); err != nil {
			t.Fatalf("call %d of the burst: %v", i+1, err)
		}
	}
	if s := b.Stats(); s.Tokens > 1e-6 || s.Granted != 3 || s.Waited != 0 {
		t.Fatalf("after the burst: %+v", s)
	}

	done, errs := make(chan int, 1), make(chan error, 1)
	queue(t, b, ctx, 0, 4, done, errs)
	clock.Advance(999 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the fourth call went before its token was whole")
	default:
	}
	clock.Advance(time.Millisecond)
	if got := <-done; got != 4 {
		t.Fatalf("got %d", got)
	}
	if s := b.Stats(); s.Granted != 4 || s.Waited != 1 || s.Waiting != 0 {
		t.Fatalf("after the wait: %+v", s)
	}

	// Idle time fills the bucket up to its burst and no further.
	clock.Advance(time.Hour)
	if s := b.Stats(); s.Tokens != 3 {
		t.Fatalf("tokens after an hour = %v, want the burst, 3", s.Tokens)
	}
}

func TestWaitersAreServedByPriorityThenInOrder(t *testing.T) {
	clock := ratelimittest.New()
	b := ratelimit.NewWithClock(60, 1, clock)
	ctx := context.Background()
	if err := b.Wait(ctx, 0); err != nil {
		t.Fatal(err)
	}
	done, errs := make(chan int, 8), make(chan error, 8)
	// id: priority. Queued in this order.
	arrivals := []struct{ id, prio int }{{1, 2}, {2, 1}, {3, 2}, {4, 0}, {5, 1}, {6, 0}}
	for _, a := range arrivals {
		queue(t, b, ctx, a.prio, a.id, done, errs)
	}
	var got []int
	for range arrivals {
		clock.Advance(time.Second) // one token
		got = append(got, <-done)
	}
	want := []int{4, 6, 2, 5, 1, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("served %v, want %v", got, want)
		}
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}

func TestAnUrgentCallerGoesBeforeThoseAlreadyWaiting(t *testing.T) {
	clock := ratelimittest.New()
	b := ratelimit.NewWithClock(60, 1, clock)
	ctx := context.Background()
	_ = b.Wait(ctx, 0)
	done, errs := make(chan int, 4), make(chan error, 4)
	queue(t, b, ctx, 2, 1, done, errs) // an event poll
	clock.Advance(500 * time.Millisecond)
	queue(t, b, ctx, 0, 2, done, errs) // an answer, half a token later
	clock.Advance(500 * time.Millisecond)
	if got := <-done; got != 2 {
		t.Fatalf("first served %d, want the answer (2)", got)
	}
	clock.Advance(time.Second)
	if got := <-done; got != 1 {
		t.Fatalf("then %d, want the poll (1)", got)
	}
}

func TestCancellingAWaiterTakesItOutOfTheQueue(t *testing.T) {
	clock := ratelimittest.New()
	b := ratelimit.NewWithClock(60, 1, clock)
	bg := context.Background()
	_ = b.Wait(bg, 0)
	done, errs := make(chan int, 4), make(chan error, 4)
	ctx, cancel := context.WithCancel(bg)
	queue(t, b, ctx, 0, 1, done, errs)
	queue(t, b, bg, 1, 2, done, errs)

	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled caller got %v", err)
	}
	waitFor(t, "the cancelled caller leaves", func() bool { return b.Stats().Waiting == 1 })
	clock.Advance(time.Second)
	if got := <-done; got != 2 {
		t.Fatalf("served %d, want 2", got)
	}
	if s := b.Stats(); s.Cancelled != 1 || s.Granted != 2 || s.Waiting != 0 {
		t.Fatalf("stats: %+v", s)
	}
	if n := clock.Pending(); n != 0 {
		t.Fatalf("%d timers left with nobody waiting", n)
	}
}

func TestAWaiterCancelledBeforeItWaitsTakesNothing(t *testing.T) {
	b := ratelimit.NewWithClock(60, 1, ratelimittest.New())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Wait(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if s := b.Stats(); s.Tokens != 1 || s.Granted != 0 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestSetRate(t *testing.T) {
	clock := ratelimittest.New()
	b := ratelimit.NewWithClock(60, 10, clock)
	ctx := context.Background()
	for range 10 {
		_ = b.Wait(ctx, 0)
	}
	done, errs := make(chan int, 4), make(chan error, 4)
	queue(t, b, ctx, 1, 1, done, errs)
	queue(t, b, ctx, 1, 2, done, errs)

	// Six hundred a minute: a token every 100 ms, for those already waiting.
	b.SetRate(600, 5)
	clock.Advance(100 * time.Millisecond)
	if got := <-done; got != 1 {
		t.Fatalf("got %d", got)
	}
	clock.Advance(100 * time.Millisecond)
	if got := <-done; got != 2 {
		t.Fatalf("got %d", got)
	}
	if s := b.Stats(); s.PerMinute != 600 || s.Burst != 5 {
		t.Fatalf("stats: %+v", s)
	}

	// A smaller burst caps what was earned.
	clock.Advance(time.Minute)
	b.SetRate(600, 2)
	if s := b.Stats(); s.Tokens != 2 {
		t.Fatalf("tokens = %v, want the new burst, 2", s.Tokens)
	}

	// No limit lets everyone waiting through at once.
	_ = b.Wait(ctx, 0)
	_ = b.Wait(ctx, 0)
	queue(t, b, ctx, 2, 3, done, errs)
	queue(t, b, ctx, 2, 4, done, errs)
	granted := b.Stats().Granted
	b.SetRate(0, 0)
	got := map[int]bool{<-done: true, <-done: true}
	if !got[3] || !got[4] {
		t.Fatalf("released %v", got)
	}
	if s := b.Stats(); s.Granted != granted+2 || s.Waiting != 0 {
		t.Fatalf("stats after release: %+v", s)
	}
	for range 1000 {
		if err := b.Wait(ctx, 2); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNoLimit(t *testing.T) {
	b := ratelimit.New(0, 0)
	for range 10000 {
		if err := b.Wait(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}
	if s := b.Stats(); s.Granted != 10000 || s.Waited != 0 {
		t.Fatalf("stats: %+v", s)
	}
}

// TestRealClock runs the bucket on the real clock: 6000 a minute is a token
// every 10 ms, so ten calls past a burst of one take about 90 ms.
func TestRealClock(t *testing.T) {
	b := ratelimit.New(6000, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.Wait(ctx, i%3); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if took := time.Since(start); took < 80*time.Millisecond {
		t.Fatalf("ten calls took %s, faster than the rate allows", took)
	}
	if s := b.Stats(); s.Granted != 10 || s.Waiting != 0 {
		t.Fatalf("stats: %+v", s)
	}
}

// TestManyCallersCancelling has callers come, wait and give up at random,
// under -race: every one either gets a token or its context's error, and
// nobody is left waiting.
func TestManyCallersCancelling(t *testing.T) {
	b := ratelimit.New(60000, 5) // a token every millisecond
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, cancelled int
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%20)*time.Millisecond)
			defer cancel()
			err := b.Wait(ctx, i%3)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, context.DeadlineExceeded):
				cancelled++
			default:
				t.Errorf("unexpected %v", err)
			}
		}()
	}
	wg.Wait()
	s := b.Stats()
	if ok+cancelled != 200 || s.Waiting != 0 || int(s.Granted) != ok {
		t.Fatalf("ok %d, cancelled %d, stats %+v", ok, cancelled, s)
	}
}
