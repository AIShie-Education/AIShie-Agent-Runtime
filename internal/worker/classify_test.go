package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

func failed(code, reason string, details ...string) *core.Envelope {
	d := map[string]any{}
	if reason != "" {
		d["reason"] = reason
	}
	for i := 0; i+1 < len(details); i += 2 {
		d[details[i]] = details[i+1]
	}
	return &core.Envelope{Status: core.StatusFailed, ActionID: "a", Error: &core.Error{Code: code, Details: d}}
}

func notAttempted(code string) *core.Envelope {
	return &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: code}}
}

// notRevisable is Core refusing what an answer named in revises, nothing
// recorded: code says whether it was no proposal of the agent's
// (invalid_argument) or one not sent back for changes (failed_precondition).
func notRevisable(code string) *core.Envelope {
	return &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: code, Details: map[string]any{"reason": core.ReasonNotRevisable}}}
}

// TestClassifyEveryRowOfTheHandout walks §2.4's table, row by row.
func TestClassifyEveryRowOfTheHandout(t *testing.T) {
	for _, c := range []struct {
		name  string
		env   *core.Envelope
		err   error
		next  Next
		state store.AttemptState
	}{
		{"executed, review none", &core.Envelope{Status: core.StatusExecuted, ReviewState: core.ReviewNone}, nil, NextDone, store.AttemptExecuted},
		{"executed, review pending", &core.Envelope{Status: core.StatusExecuted, ReviewState: core.ReviewPending}, nil, NextDone, store.AttemptExecuted},
		{"proposed", &core.Envelope{Status: core.StatusProposed, ActionID: "a"}, nil, NextProposed, store.AttemptProposed},
		{"denied", &core.Envelope{Status: core.StatusDenied}, nil, NextHoldSeat, store.AttemptDenied},
		{"moved_on", failed(core.CodeConflict, core.ReasonMovedOn, "latest_opener_message_id", "m3"), nil, NextMovedOn, store.AttemptFailed},
		{"already_answered", failed(core.CodeConflict, core.ReasonAlreadyAnswered), nil, NextLeave, store.AttemptFailed},
		{"answer_pending", failed(core.CodeConflict, core.ReasonAnswerPending), nil, NextLeave, store.AttemptFailed},
		{"closed", failed(core.CodeConflict, core.ReasonClosed), nil, NextDrop, store.AttemptFailed},
		{"not_addressable", failed(core.CodeForbidden, core.ReasonNotAddressable), nil, NextDropReseat, store.AttemptFailed},
		{"invalid_argument", failed(core.CodeInvalidArgument, ""), nil, NextFix, store.AttemptFailed},
		// A body refused as the arguments are read, never attempted (AIShie-Core #60).
		{"invalid_argument, refused as read", notAttempted(core.CodeInvalidArgument), nil, NextFix, store.AttemptError},
		{"idempotency_conflict", notAttempted(core.CodeIdempotencyConflict), nil, NextAttempt, store.AttemptError},
		{"not_found", notAttempted(core.CodeNotFound), nil, NextDrop, store.AttemptError},
		{"replayed executed", &core.Envelope{Status: core.StatusExecuted, Replayed: true}, nil, NextDone, store.AttemptExecuted},
		{"replayed proposed", &core.Envelope{Status: core.StatusProposed, Replayed: true}, nil, NextProposed, store.AttemptProposed},
		{"replayed rejected", &core.Envelope{Status: core.StatusRejected, Replayed: true}, nil, NextAttempt, store.AttemptRejected},
		{"replayed changes_requested", &core.Envelope{Status: core.StatusChangesRequested, Replayed: true}, nil, NextAttempt, store.AttemptChangesRequested},
		// What the answer named in revises refused: the next attempt
		// names none, not the same body fixed or sent again.
		{"not_revisable, no proposal of the agent's", notRevisable(core.CodeInvalidArgument), nil, NextAttempt, store.AttemptError},
		{"not_revisable, not sent back", notRevisable(core.CodeFailedPrecondition), nil, NextAttempt, store.AttemptError},
		{"replayed cancelled", &core.Envelope{Status: core.StatusCancelled, Replayed: true}, nil, NextAttempt, store.AttemptCancelled},
		// What came back was not an envelope at all.
		{"401", nil, core.ErrUnauthenticated, NextStopAgent, ""},
		{"429", nil, &core.RateLimitedError{}, NextRetryLater, ""},
		{"unreachable", nil, &core.TransientError{Err: fmt.Errorf("reset")}, NextRetryLater, ""},
		{"cancelled", nil, context.Canceled, NextRetryLater, ""},
		{"internal after retries", notAttempted(core.CodeInternal), nil, NextRetryLater, ""},
		{"protocol error", nil, &core.ProtocolError{Code: -32602}, NextRetryLater, store.AttemptError},
		{"failed, some other conflict", failed(core.CodeFailedPrecondition, ""), nil, NextAttempt, store.AttemptFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := Classify(c.env, c.err)
			if d.Next != c.next || d.State != c.state {
				t.Errorf("Classify = %s, %q; want %s, %q", d.Next, d.State, c.next, c.state)
			}
			if d.Outcome == "" {
				t.Error("no outcome for the ledger")
			}
		})
	}
}

// Core's refusals of an answer's sources (§2.10) name the source, before
// anything is posted (failed: one the seat may not read now, one purged)
// or as the call is read (error: too many, one named twice, one not as
// Core takes it): the answer is posted again without that source, or
// without any, under the next attempt. Any other invalid_argument is the
// body's.
func TestClassifyARefusedSource(t *testing.T) {
	refused := func(status core.Status, reason, field string) *core.Envelope {
		return &core.Envelope{Status: status, Error: &core.Error{Code: core.CodeInvalidArgument,
			Details: map[string]any{"reason": reason, "field": field, "index": float64(1)}}}
	}
	for _, c := range []struct {
		name   string
		env    *core.Envelope
		next   Next
		source int
		state  store.AttemptState
	}{
		{"source_unreadable", refused(core.StatusFailed, reasonSourceUnreadable, "sources[1]"), NextDropSource, 1, store.AttemptFailed},
		{"source_purged", refused(core.StatusFailed, reasonSourcePurged, "sources[12]"), NextDropSource, 12, store.AttemptFailed},
		{"duplicate_source", refused(core.StatusError, "duplicate_source", "sources[1]"), NextDropSource, -1, store.AttemptError},
		{"too_many_sources", refused(core.StatusError, "too_many_sources", "sources"), NextDropSource, -1, store.AttemptError},
		{"a field that names no source", refused(core.StatusFailed, reasonSourceUnreadable, "sources[x]"), NextDropSource, -1, store.AttemptFailed},
		{"the body", refused(core.StatusFailed, "", "body"), NextFix, 0, store.AttemptFailed},
		{"a Core that takes no sources", &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeInvalidArgument,
			Message: `the arguments do not match the tool's schema: unexpected additional properties ["sources"]`}}, NextDropSource, -1, store.AttemptError},
		{"a Core that takes no other argument", &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeInvalidArgument,
			Message: `unexpected additional properties ["mood"]`}}, NextFix, 0, store.AttemptError},
		{"no field", failed(core.CodeInvalidArgument, reasonSourceUnreadable), NextFix, 0, store.AttemptFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := Classify(c.env, nil)
			if d.Next != c.next || d.Source != c.source || d.State != c.state || d.Outcome != store.OutcomeFailed {
				t.Errorf("Classify = %s, source %d, %q, %q; want %s, source %d, %q", d.Next, d.Source, d.State, d.Outcome, c.next, c.source, c.state)
			}
		})
	}
}

func TestClassifyMovedOnNamesTheNewestMessage(t *testing.T) {
	d := Classify(failed(core.CodeConflict, core.ReasonMovedOn, "latest_opener_message_id", "m3"), nil)
	if d.LatestMessageID != "m3" {
		t.Errorf("LatestMessageID = %q", d.LatestMessageID)
	}
	// Without the id (the question withdrawn), the conversation is read
	// again rather than guessed, and what was sent is dropped.
	if d := Classify(failed(core.CodeConflict, core.ReasonMovedOn), nil); d.Next != NextAttempt || d.Outcome != store.OutcomeDropped {
		t.Errorf("moved_on without an id = %s, %s", d.Next, d.Outcome)
	}
}

// TestNothingPostedIsSettledAsPosted holds the one line that matters most:
// only an executed answer counts as posted.
func TestNothingPostedIsSettledAsPosted(t *testing.T) {
	for _, s := range []core.Status{core.StatusProposed, core.StatusDenied, core.StatusFailed, core.StatusRejected, core.StatusChangesRequested,
		core.StatusCancelled, core.StatusError, "something new"} {
		d := Classify(&core.Envelope{Status: s, Error: &core.Error{Code: "x"}}, nil)
		if d.State.Posted() || d.Outcome == store.OutcomePosted {
			t.Errorf("%s settles as posted", s)
		}
	}
}
