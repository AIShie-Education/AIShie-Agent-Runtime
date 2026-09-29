package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit/ratelimittest"
)

// recorder is a Caller that reports each call's tool as it is made.
type recorder chan string

func (r recorder) Call(_ context.Context, tool string, _ json.RawMessage) (*Envelope, error) {
	r <- tool
	return &Envelope{Status: StatusExecuted}, nil
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// TestLimitedServesAnswersThenPollingThenTheRest queues calls of every
// priority behind an empty bucket and lets one token in at a time.
func TestLimitedServesAnswersThenPollingThenTheRest(t *testing.T) {
	clock := ratelimittest.New()
	b := ratelimit.NewWithClock(60, 1, clock)
	calls := make(recorder, 16)
	c := Limited(calls, b)
	bg := context.Background()
	if _, err := c.Call(bg, "first", nil); err != nil {
		t.Fatal(err)
	}
	<-calls

	queued := []struct {
		tool string
		ctx  context.Context
	}{
		{"event_list", WithPriority(bg, PriorityBackground)},
		{"conversation_inbox 1", WithPriority(bg, PriorityPoll)},
		{"me_memberships", bg}, // unmarked: background
		{"conversation_messages", WithPriority(bg, PriorityAnswer)},
		{"conversation_inbox 2", WithPriority(bg, PriorityPoll)},
		{"conversation_answer", WithPriority(bg, PriorityAnswer)},
	}
	errs := make(chan error, len(queued))
	for i, q := range queued {
		go func() {
			_, err := c.Call(q.ctx, q.tool, nil)
			errs <- err
		}()
		waitUntil(t, q.tool+" waits", func() bool { return b.Stats().Waiting == i+1 })
	}
	var got []string
	for range queued {
		clock.Advance(time.Second)
		got = append(got, <-calls)
	}
	want := []string{"conversation_messages", "conversation_answer", "conversation_inbox 1", "conversation_inbox 2", "event_list", "me_memberships"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("served %v, want %v", got, want)
		}
	}
	for range queued {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestLimitedGivesUpWithTheContext(t *testing.T) {
	b := ratelimit.NewWithClock(60, 1, ratelimittest.New())
	calls := make(recorder, 4)
	c := Limited(calls, b)
	if _, err := c.Call(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}
	<-calls
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, "second", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	select {
	case tool := <-calls:
		t.Fatalf("%s was made without a token", tool)
	default:
	}
	if s := b.Stats(); s.Waiting != 0 || s.Cancelled != 1 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestLimitedWithoutABucket(t *testing.T) {
	calls := make(recorder, 1)
	if c := Limited(calls, nil); c != Caller(calls) {
		t.Fatal("a nil bucket should leave the caller as it is")
	}
}

// A best-effort call (a draft of an answer) takes no token, and waits for
// none behind an empty bucket: Core does not count it against the actor's
// limit.
func TestLimitedLetsBestEffortThrough(t *testing.T) {
	b := ratelimit.NewWithClock(60, 1, ratelimittest.New())
	calls := make(recorder, 4)
	c := Limited(calls, b)
	bg := context.Background()
	if _, err := c.Call(bg, "first", nil); err != nil {
		t.Fatal(err)
	}
	<-calls
	ctx, cancel := context.WithTimeout(WithBestEffort(bg), time.Second)
	defer cancel()
	for range 3 {
		if _, err := c.Call(ctx, "conversation_draft", nil); err != nil {
			t.Fatalf("a best-effort call waited: %v", err)
		}
		<-calls
	}
	if s := b.Stats(); s.Granted != 1 || s.Waiting != 0 {
		t.Errorf("the bucket gave %d tokens and has %d waiting; want the first call's alone", s.Granted, s.Waiting)
	}
}
