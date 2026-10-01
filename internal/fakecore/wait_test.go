package fakecore

import (
	"context"
	"testing"
	"time"
)

// TestWaitBounds: a call that asks to wait waits only while the bounds on
// calls waiting allow (Options.LongPollWaitersPerActor, LongPollWaiters);
// past them, with none allowed, or once the fake shuts down, it answers at
// once, empty, as Core's does. A waiting call is one call to the rate limit.
func TestWaitBounds(t *testing.T) {
	t.Run("per actor", func(t *testing.T) {
		clk := newClock()
		w := newFakeWorld(t, Options{LongPollWaitersPerActor: 1, RatePerMinute: 60, RateBurst: 4, Now: clk.now})
		first := waitInBackground(w.agentC, inCourseArgs(w, "wait_s", 5))
		eventually(t, "a call waiting", func() bool { return w.fc.Waiting() == 1 })
		if took := timeCall(t, w.agentC, inCourseArgs(w, "wait_s", 5)); took > 2*time.Second {
			t.Errorf("a call past the actor's bound waited %s", took)
		}
		// Another actor has bounds of its own.
		other := waitInBackground(w.ownAgent(), inCourseArgs(w, "wait_s", 5))
		eventually(t, "the other actor's call waiting", func() bool { return w.fc.Waiting() == 2 })
		w.ask(0, "Q1")
		w.askOwn("Q2")
		for _, done := range []chan toolAnswer{first, other} {
			select {
			case a := <-done:
				wantEnvelope(t, a, "executed", "", "")
			case <-time.After(3 * time.Second):
				t.Fatal("a question did not wake a call waiting")
			}
		}
		// initialize, notifications/initialized and the tutor's two calls
		// spent the burst of four: however long the first waited, it was
		// one call.
		if a, err := w.agentC.call(context.Background(), "me_get", map[string]any{}); err != nil || a.Status != 429 {
			t.Errorf("the fifth request: %v %d", err, a.Status)
		}
	})
	t.Run("in all", func(t *testing.T) {
		w := newFakeWorld(t, Options{LongPollWaiters: 1})
		waitInBackground(w.agentC, inCourseArgs(w, "wait_s", 3))
		eventually(t, "a call waiting", func() bool { return w.fc.Waiting() == 1 })
		if took := timeCall(t, w.ownAgent(), inCourseArgs(w, "wait_s", 3)); took > 2*time.Second {
			t.Errorf("a call past the bound on all waited %s", took)
		}
	})
	t.Run("none", func(t *testing.T) {
		w := newFakeWorld(t, Options{LongPollWaitersPerActor: -1})
		if took := timeCall(t, w.agentC, inCourseArgs(w, "wait_s", 3)); took > 2*time.Second {
			t.Errorf("a call waited %s where none may", took)
		}
	})
	t.Run("shut down", func(t *testing.T) {
		w := newFakeWorld(t, Options{})
		done := waitInBackground(w.agentC, inCourseArgs(w, "wait_s", 20))
		eventually(t, "a call waiting", func() bool { return w.fc.Waiting() == 1 })
		w.fc.Shutdown()
		select {
		case a := <-done:
			wantEnvelope(t, a, "executed", "", "")
		case <-time.After(3 * time.Second):
			t.Fatal("Shutdown left a call waiting")
		}
		if took := timeCall(t, w.agentC, inCourseArgs(w, "wait_s", 3)); took > 2*time.Second {
			t.Errorf("a call waited %s after Shutdown", took)
		}
	})
	t.Run("the client goes", func(t *testing.T) {
		w := newFakeWorld(t, Options{})
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _, _ = w.agentC.call(ctx, "conversation_inbox", inCourseArgs(w, "wait_s", 20)) }()
		eventually(t, "a call waiting", func() bool { return w.fc.Waiting() == 1 })
		cancel()
		eventually(t, "the call to stop waiting", func() bool { return w.fc.Waiting() == 0 })
	})
}

// TestWaitFilters: a waiting call wakes for news it would read, as Core's
// filters have it: an inbox for its own seat's conversations, a feed for
// any news of its course; a change that is no news to it leaves it waiting.
func TestWaitFilters(t *testing.T) {
	w := newFakeWorld(t, Options{})
	own := w.ownAgent()
	inbox := waitInBackground(w.agentC, inCourseArgs(w, "wait_s", 5))
	next := resultOf(mustCall(t, own, "event_list", inCourseArgs(w)), "next_seq")
	events := make(chan toolAnswer, 1)
	go func() {
		a, _ := own.call(context.Background(), "event_list", inCourseArgs(w, "since_seq", next, "wait_s", 5))
		events <- a
	}()
	eventually(t, "two calls waiting", func() bool { return w.fc.Waiting() == 2 })
	w.askOwn("Q1: for Yuki's own agent")
	w.setTutorLevel("autonomous") // a seat's perms: no news to an inbox
	time.Sleep(200 * time.Millisecond)
	if n := w.fc.WaitingOf(w.tutorA.ID); n != 1 {
		t.Fatalf("the tutor's inbox stopped waiting for news that was not its own (%d waiting)", n)
	}
	w.ask(0, "Q2: for the tutor")
	for name, done := range map[string]chan toolAnswer{"the tutor's inbox": inbox, "the own agent's feed": events} {
		select {
		case a := <-done:
			wantEnvelope(t, a, "executed", "", "")
		case <-time.After(3 * time.Second):
			t.Fatalf("%s was not woken", name)
		}
	}
}

// TestWithoutWait: a fake of a Core from before wait_s offers none, and
// refuses a call that gives it, as its schema does.
func TestWithoutWait(t *testing.T) {
	w := newFakeWorld(t, Options{WithoutWait: true})
	for _, name := range []string{"conversation.inbox", "conversation.messages", "event.list"} {
		if _, ok := w.fc.cat.byName[name].props["wait_s"]; ok {
			t.Errorf("%s offers wait_s", name)
		}
	}
	if _, ok := w.fc.cat.byName["conversation.messages"].props["seen_state"]; ok {
		t.Error("conversation.messages offers seen_state")
	}
	wantEnvelope(t, mustCall(t, w.agentC, "conversation_inbox", inCourseArgs(w, "wait_s", 5)), "error", codeInvalidArgument, "")
	wantEnvelope(t, mustCall(t, w.agentC, "conversation_inbox", inCourseArgs(w)), "executed", "", "")
	both := newFakeWorld(t, Options{WithoutWait: true, WithoutHosting: true})
	if both.fc.cat.byName["agent_runtime.agent"] != nil || both.fc.cat.byName["event.list"].props["wait_s"] != nil {
		t.Error("the older Cores' catalogue is not each of them")
	}
}

// waitInBackground calls conversation_inbox as c, and hands over its
// answer when it comes.
func waitInBackground(c *mcpClient, args map[string]any) chan toolAnswer {
	done := make(chan toolAnswer, 1)
	go func() {
		a, _ := c.call(context.Background(), "conversation_inbox", args)
		done <- a
	}()
	return done
}

// timeCall calls conversation_inbox as c, and says how long it took.
func timeCall(t *testing.T, c *mcpClient, args map[string]any) time.Duration {
	t.Helper()
	start := time.Now()
	a := mustCall(t, c, "conversation_inbox", args)
	wantEnvelope(t, a, "executed", "", "")
	return time.Since(start)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("still waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
