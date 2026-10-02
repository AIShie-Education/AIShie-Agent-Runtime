package storetest

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// answer is an attempt at answering message msg of conversation conv, as
// the worker writes it ahead.
func answer(agent, member, conv, msg string, no int, created time.Time) store.Attempt {
	key := fmt.Sprintf("answer:%s:%s:%d", conv, msg, no)
	return store.Attempt{
		Key:            key,
		AgentID:        agent,
		MemberID:       member,
		CourseID:       "course-1",
		ConversationID: conv,
		MessageID:      msg,
		No:             no,
		Tool:           "conversation_answer",
		Args:           []byte(fmt.Sprintf(`{"body":"attempt %d","idempotency_key":%q}`, no, key)),
		Kind:           "model",
		State:          store.AttemptSending,
		CreatedAt:      created,
		UpdatedAt:      created,
	}
}

// put writes a and fails the test if that does not work.
func put(t *testing.T, s store.Store, a store.Attempt) {
	t.Helper()
	if _, err := s.PutAttempt(t.Context(), a); err != nil {
		t.Fatalf("PutAttempt(%s, %s): %v", a.AgentID, a.Key, err)
	}
}

// finish records o for key and fails the test if that does not work.
func finish(t *testing.T, s store.Store, agent, key string, o store.Outcome) {
	t.Helper()
	if err := s.FinishAttempt(t.Context(), agent, key, o); err != nil {
		t.Fatalf("FinishAttempt(%s, %s): %v", agent, key, err)
	}
}

// get reads the attempt under key and fails the test if that does not work.
func get(t *testing.T, s store.Store, agent, key string) store.Attempt {
	t.Helper()
	a, err := s.Attempt(t.Context(), agent, key)
	if err != nil {
		t.Fatalf("Attempt(%s, %s): %v", agent, key, err)
	}
	return *a
}

func testAttempts(t *testing.T, open Opener) {
	t.Run("written ahead, and the first bytes under a key are the ones kept", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		first := answer("a1", "m1", "x1", "q1", 1, base)
		got, err := s.PutAttempt(ctx, first)
		if err != nil {
			t.Fatal(err)
		}
		sameAttempt(t, *got, first)

		second := first
		second.Args = []byte(`{"body":"regenerated","idempotency_key":"answer:x1:q1:1"}`)
		second.Kind = "budget"
		second.CreatedAt = at(time.Hour)
		got, err = s.PutAttempt(ctx, second)
		if !errors.Is(err, store.ErrExists) {
			t.Fatalf("PutAttempt of a key taken: err = %v, want ErrExists", err)
		}
		if got == nil {
			t.Fatal("PutAttempt of a key taken returned no row")
		}
		// What must be sent under the key is what was sent first.
		sameAttempt(t, *got, first)
		sameAttempt(t, get(t, s, "a1", first.Key), first)
	})

	t.Run("a zero CreatedAt is the store's now, and an empty state is sending", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		a := answer("a1", "m1", "x1", "q1", 1, time.Time{})
		a.State = ""
		before := time.Now()
		got, err := s.PutAttempt(ctx, a)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		recent(t, "created_at", got.CreatedAt, before, after)
		if !got.UpdatedAt.Equal(got.CreatedAt) {
			t.Errorf("updated_at = %s, want created_at %s", got.UpdatedAt, got.CreatedAt)
		}
		if got.State != store.AttemptSending {
			t.Errorf("state = %q, want sending", got.State)
		}
		stored := get(t, s, "a1", a.Key)
		if !stored.CreatedAt.Equal(got.CreatedAt) || stored.State != store.AttemptSending {
			t.Errorf("stored %+v, want what PutAttempt returned", stored)
		}
	})

	t.Run("bytes are copied in and out", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		a := answer("a1", "m1", "x1", "q1", 1, base)
		want := bytes.Clone(a.Args)
		got, err := s.PutAttempt(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		a.Args[0] = 'X'
		got.Args[1] = 'Y'
		again := get(t, s, "a1", a.Key)
		again.Args[2] = 'Z'
		if stored := get(t, s, "a1", a.Key); !bytes.Equal(stored.Args, want) {
			t.Fatalf("stored args = %q, want %q: the store shares a caller's slice", stored.Args, want)
		}
	})

	t.Run("finished with what became of it", func(t *testing.T) {
		s := open(t)
		a := answer("a1", "m1", "x1", "q1", 1, base)
		put(t, s, a)

		before := time.Now()
		finish(t, s, "a1", a.Key, store.Outcome{State: store.AttemptProposed, ActionID: "act-1"})
		after := time.Now()
		got := get(t, s, "a1", a.Key)
		recent(t, "updated_at", got.UpdatedAt, before, after)
		a.State, a.ActionID, a.UpdatedAt = store.AttemptProposed, "act-1", got.UpdatedAt
		sameAttempt(t, got, a)

		// The action id already known is kept when the outcome has none.
		finish(t, s, "a1", a.Key, store.Outcome{State: store.AttemptExecuted, PostedMessageID: "msg-2"})
		got = get(t, s, "a1", a.Key)
		a.State, a.PostedMessageID, a.UpdatedAt = store.AttemptExecuted, "msg-2", got.UpdatedAt
		sameAttempt(t, got, a)

		// So is the posted message id: a replay that names neither keeps
		// both.
		finish(t, s, "a1", a.Key, store.Outcome{State: store.AttemptExecuted})
		got = get(t, s, "a1", a.Key)
		a.UpdatedAt = got.UpdatedAt
		sameAttempt(t, got, a)

		// An error code and reason say why the attempt stands where it now
		// does, so the newest replace the old.
		b := answer("a1", "m1", "x1", "q1", 2, at(time.Minute))
		put(t, s, b)
		finish(t, s, "a1", b.Key, store.Outcome{State: store.AttemptFailed, ErrorCode: "conflict", Reason: "moved_on"})
		finish(t, s, "a1", b.Key, store.Outcome{State: store.AttemptError, ErrorCode: "idempotency_conflict"})
		got = get(t, s, "a1", b.Key)
		b.State, b.ErrorCode, b.UpdatedAt = store.AttemptError, "idempotency_conflict", got.UpdatedAt
		sameAttempt(t, got, b)
	})

	t.Run("sent back for changes: settled, with its action and what to change", func(t *testing.T) {
		s := open(t)
		a := answer("a1", "m1", "x1", "q1", 1, base)
		put(t, s, a)
		finish(t, s, "a1", a.Key, store.Outcome{State: store.AttemptProposed, ActionID: "act-1"})
		finish(t, s, "a1", a.Key, store.Outcome{State: store.AttemptChangesRequested, ActionID: "act-1", Reason: "Cite the syllabus."})
		got := get(t, s, "a1", a.Key)
		a.State, a.ActionID, a.Reason, a.UpdatedAt = store.AttemptChangesRequested, "act-1", "Cite the syllabus.", got.UpdatedAt
		sameAttempt(t, got, a)
		if !got.State.Settled() || got.State.Posted() {
			t.Errorf("an attempt sent back for changes: settled %v, posted %v; want settled, not posted", got.State.Settled(), got.State.Posted())
		}
		if un, err := s.Unsettled(t.Context(), "a1", "m1"); err != nil || len(un) != 0 {
			t.Errorf("Unsettled = %v, %v; want none", keys(un), err)
		}
	})

	t.Run("finishing an unknown key is ErrNotFound; an unknown state is refused", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		err := s.FinishAttempt(ctx, "a1", "answer:x1:q1:1", store.Outcome{State: store.AttemptExecuted})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		a := answer("a1", "m1", "x1", "q1", 1, base)
		put(t, s, a)
		for _, state := range []store.AttemptState{"", "posted"} {
			err := s.FinishAttempt(ctx, "a1", a.Key, store.Outcome{State: state})
			if err == nil || errors.Is(err, store.ErrNotFound) {
				t.Errorf("FinishAttempt with state %q: err = %v, want a refusal", state, err)
			}
		}
		sameAttempt(t, get(t, s, "a1", a.Key), a)
	})

	t.Run("an unknown key is ErrNotFound", func(t *testing.T) {
		s := open(t)
		if _, err := s.Attempt(t.Context(), "a1", "answer:x1:q1:1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("AttemptsFor lists one message's attempts by number", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		// Written out of order, so that the order is the number's.
		for i, no := range []int{3, 1, 2} {
			put(t, s, answer("a1", "m1", "x1", "q1", no, at(time.Duration(i)*time.Minute)))
		}
		put(t, s, answer("a1", "m1", "x1", "q2", 1, base)) // another message
		put(t, s, answer("a1", "m1", "x2", "q1", 4, base)) // another conversation
		put(t, s, closing("a1", "m1", "x1", base))

		got, err := s.AttemptsFor(ctx, "a1", "x1", "q1")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"answer:x1:q1:1", "answer:x1:q1:2", "answer:x1:q1:3"}
		if !slices.Equal(keys(got), want) {
			t.Fatalf("AttemptsFor = %v, want %v", keys(got), want)
		}
		sameAttempt(t, got[0], answer("a1", "m1", "x1", "q1", 1, at(time.Minute)))

		got, err = s.AttemptsFor(ctx, "a1", "x9", "q1")
		if err != nil || len(got) != 0 {
			t.Fatalf("AttemptsFor of a conversation never answered = %v, %v; want none", keys(got), err)
		}
	})

	t.Run("AttemptByAction finds the attempt Core recorded as the action", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		a := answer("a1", "m1", "x1", "q1", 1, base)
		b := answer("a1", "m1", "x1", "q1", 2, at(time.Minute))
		put(t, s, a)
		put(t, s, b)
		finish(t, s, "a1", b.Key, store.Outcome{State: store.AttemptProposed, ActionID: "act-2"})

		got, err := s.AttemptByAction(ctx, "a1", "act-2")
		if err != nil {
			t.Fatal(err)
		}
		if got.Key != b.Key {
			t.Fatalf("AttemptByAction(act-2) = %s, want %s", got.Key, b.Key)
		}
		// a has no action: an empty id names none.
		for _, id := range []string{"act-9", ""} {
			if _, err := s.AttemptByAction(ctx, "a1", id); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("AttemptByAction(%q): err = %v, want ErrNotFound", id, err)
			}
		}

		// Should two attempts ever name one action, the oldest is the one.
		finish(t, s, "a1", a.Key, store.Outcome{State: store.AttemptError, ActionID: "act-2", ErrorCode: "idempotency_conflict"})
		got, err = s.AttemptByAction(ctx, "a1", "act-2")
		if err != nil {
			t.Fatal(err)
		}
		if got.Key != a.Key {
			t.Fatalf("AttemptByAction(act-2) = %s, want the oldest, %s", got.Key, a.Key)
		}
	})

	t.Run("Unsettled lists a seat's attempts sending or proposed, oldest first", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		// Every state, each in a message of its own, written in an order
		// that is not their age.
		states := []struct {
			state   store.AttemptState
			created time.Duration
		}{
			{store.AttemptSending, 3 * time.Minute},
			{store.AttemptProposed, time.Minute},
			{store.AttemptExecuted, 2 * time.Minute},
			{store.AttemptFailed, 0},
			{store.AttemptDenied, 0},
			{store.AttemptError, 0},
			{store.AttemptRejected, 0},
			{store.AttemptCancelled, 0},
			{store.AttemptSending, 0},
			{store.AttemptProposed, 3 * time.Minute}, // as old as the first: written after it
		}
		for i, c := range states {
			a := answer("a1", "m1", "x1", fmt.Sprintf("q%d", i), 1, at(c.created))
			a.State = c.state
			put(t, s, a)
		}
		// Another seat's, and another agent's, in the same seat's ids.
		put(t, s, answer("a1", "m2", "x2", "q0", 1, base))
		put(t, s, answer("a2", "m1", "x1", "q0", 1, base))

		got, err := s.Unsettled(ctx, "a1", "m1")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"answer:x1:q8:1", "answer:x1:q1:1", "answer:x1:q0:1", "answer:x1:q9:1"}
		if !slices.Equal(keys(got), want) {
			t.Fatalf("Unsettled = %v, want %v", keys(got), want)
		}
		for _, a := range got {
			if a.AgentID != "a1" || a.MemberID != "m1" {
				t.Errorf("Unsettled(a1, m1) returned %s's attempt in seat %s", a.AgentID, a.MemberID)
			}
		}

		// Settling one takes it off the list.
		finish(t, s, "a1", "answer:x1:q8:1", store.Outcome{State: store.AttemptExecuted, PostedMessageID: "msg-9"})
		got, err = s.Unsettled(ctx, "a1", "m1")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(keys(got), want[1:]) {
			t.Fatalf("Unsettled after settling one = %v, want %v", keys(got), want[1:])
		}
	})

	t.Run("one of many racing writers of a key wins", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		const racers = 20
		type result struct {
			row *store.Attempt
			err error
		}
		var (
			wg      sync.WaitGroup
			start   = make(chan struct{})
			results = make([]result, racers)
		)
		for i := range racers {
			wg.Go(func() {
				a := answer("a1", "m1", "x1", "q1", 1, base)
				a.Args = []byte(fmt.Sprintf(`{"body":"racer %d"}`, i))
				<-start
				row, err := s.PutAttempt(ctx, a)
				results[i] = result{row, err}
			})
		}
		close(start)
		wg.Wait()

		var winner []byte
		for i, r := range results {
			if r.err == nil {
				if winner != nil {
					t.Fatalf("racer %d also wrote the key", i)
				}
				winner = r.row.Args
			}
		}
		if winner == nil {
			t.Fatal("no racer wrote the key")
		}
		for i, r := range results {
			switch {
			case r.err == nil:
			case !errors.Is(r.err, store.ErrExists):
				t.Errorf("racer %d: %v, want ErrExists", i, r.err)
			case r.row == nil || !bytes.Equal(r.row.Args, winner):
				t.Errorf("racer %d was told to send other bytes than the winner's", i)
			}
		}
		if got := get(t, s, "a1", "answer:x1:q1:1"); !bytes.Equal(got.Args, winner) {
			t.Fatalf("stored %q, want the winner's %q", got.Args, winner)
		}
	})

	t.Run("agents never see each other's attempts, even under one key", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		a := answer("a1", "m1", "x1", "q1", 1, base)
		b := answer("a2", "m1", "x1", "q1", 1, base)
		b.Args = []byte(`{"body":"agent two"}`)
		put(t, s, a)
		put(t, s, b) // not ErrExists: keys are unique per agent
		finish(t, s, "a1", a.Key, store.Outcome{State: store.AttemptProposed, ActionID: "act-1"})

		sameAttempt(t, get(t, s, "a2", b.Key), b)
		if _, err := s.AttemptByAction(ctx, "a2", "act-1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("AttemptByAction of another agent's action: err = %v, want ErrNotFound", err)
		}
		list, err := s.AttemptsFor(ctx, "a2", "x1", "q1")
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].AgentID != "a2" || !bytes.Equal(list[0].Args, b.Args) {
			t.Errorf("AttemptsFor(a2) = %+v, want only a2's", list)
		}
		list, err = s.Unsettled(ctx, "a2", "m1")
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].AgentID != "a2" {
			t.Errorf("Unsettled(a2) = %+v, want only a2's", list)
		}
		if _, err := s.Attempt(ctx, "a3", a.Key); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Attempt of an agent that wrote nothing: err = %v, want ErrNotFound", err)
		}
		if err := s.FinishAttempt(ctx, "a3", a.Key, store.Outcome{State: store.AttemptExecuted}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("FinishAttempt of an agent that wrote nothing: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("refuses an attempt without an agent, key or seat, or in an unknown state", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for _, mutate := range []func(*store.Attempt){
			func(a *store.Attempt) { a.AgentID = "" },
			func(a *store.Attempt) { a.Key = "" },
			// Unsettled and PurgeMember find a row by its seat: one without
			// would hold an answer's bytes for ever.
			func(a *store.Attempt) { a.MemberID = "" },
			func(a *store.Attempt) { a.State = "posted" },
		} {
			a := answer("a1", "m1", "x1", "q1", 1, base)
			mutate(&a)
			if _, err := s.PutAttempt(ctx, a); err == nil || errors.Is(err, store.ErrExists) {
				t.Errorf("PutAttempt(%+v): err = %v, want a refusal", a, err)
			}
		}
		// Nothing was written by the refusals.
		put(t, s, answer("a1", "m1", "x1", "q1", 1, base))
	})
}

// closing is the attempt at closing conversation conv.
func closing(agent, member, conv string, created time.Time) store.Attempt {
	key := "close:" + conv
	return store.Attempt{
		Key:            key,
		AgentID:        agent,
		MemberID:       member,
		CourseID:       "course-1",
		ConversationID: conv,
		Tool:           "conversation_close",
		Args:           []byte(fmt.Sprintf(`{"reason":"closed","idempotency_key":%q}`, key)),
		Kind:           "close",
		State:          store.AttemptSending,
		CreatedAt:      created,
		UpdatedAt:      created,
	}
}
