package worker

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Next is what the runtime does after posting an answer, or trying to.
type Next int

const (
	// NextDone: the answer is posted. Mark the course hot; note it.
	NextDone Next = iota
	// NextProposed: it waits for a person. Keep the action id and follow
	// event_list; never send it again under a new key.
	NextProposed
	// NextHoldSeat: the seat may not answer now (denied). Stop polling the
	// course until me_memberships changes, and tell the owner.
	NextHoldSeat
	// NextMovedOn: the opener wrote again. Answer
	// Decision.LatestMessageID, under its own key.
	NextMovedOn
	// NextLeave: that message is answered, or an answer to it waits. Leave
	// the conversation.
	NextLeave
	// NextDrop: closed, or no conversation the agent may read now. Drop it.
	NextDrop
	// NextDropReseat: the opener may no longer address the agent. Drop it
	// and read me_memberships again.
	NextDropReseat
	// NextFix: Core refused the body (too long, empty). Shorten or fix it,
	// and post again under the next attempt number.
	NextFix
	// NextAttempt: nothing was posted under this key and nothing will be
	// (a proposal rejected, sent back for changes or expired, a key
	// another body took, a failure the runtime has no better answer to).
	// Regenerate under the next attempt number, if the conversation is
	// still waiting: one sent back names, in revises, the proposal it
	// revises.
	NextAttempt
	// NextRetryLater: Core could not be reached, or asked the runtime to
	// slow down. Nothing is known to have happened; send the same bytes
	// again under the same key later.
	NextRetryLater
	// NextStopAgent: the token was refused (401). Stop the agent until its
	// owner gives it a new one.
	NextStopAgent
	// NextDropSource: Core refused a source of the answer (§2.10), one the
	// seat may not read now or one purged (Decision.Source), or the
	// answer's sources as they were given (Decision.Source -1). Post the
	// same body again without it, under the next attempt number: the
	// refused call is recorded, and its key spent.
	NextDropSource
)

func (n Next) String() string {
	switch n {
	case NextDone:
		return "done"
	case NextProposed:
		return "proposed"
	case NextHoldSeat:
		return "hold_seat"
	case NextMovedOn:
		return "moved_on"
	case NextLeave:
		return "leave"
	case NextDrop:
		return "drop"
	case NextDropReseat:
		return "drop_reseat"
	case NextFix:
		return "fix"
	case NextAttempt:
		return "next_attempt"
	case NextRetryLater:
		return "retry_later"
	case NextStopAgent:
		return "stop_agent"
	case NextDropSource:
		return "drop_source"
	}
	return "unknown"
}

// Decision is what an answer's envelope, or the failure to get one, means
// (Core's docs/agent-runtime.md §2.4).
type Decision struct {
	Next Next
	// State is how the attempt settles; "" leaves it as it was written
	// ahead (sending), because nothing is known to have reached Core.
	State store.AttemptState
	// Outcome is the answer's outcome for the ledger (store.Outcome*).
	Outcome string
	// LatestMessageID is the message to answer instead, for NextMovedOn.
	LatestMessageID string
	// Code and Reason are Core's error code and details.reason, if any.
	Code, Reason string
	// Source is the source Core refused, by its place in the answer's
	// sources, for NextDropSource; -1 for all of them.
	Source int
}

// Classify reads what came back from conversation_answer. A replayed
// envelope is taken as its stored status, which Core reports as it stands
// now: a proposal approved since replays executed, one rejected replays
// rejected, one sent back for changes replays changes_requested.
func Classify(env *core.Envelope, err error) Decision {
	if err != nil {
		return classifyError(err)
	}
	d := Decision{Code: env.Code(), Reason: env.Reason()}
	switch env.Status {
	case core.StatusExecuted:
		d.Next, d.State, d.Outcome = NextDone, store.AttemptExecuted, store.OutcomePosted
	case core.StatusProposed:
		d.Next, d.State, d.Outcome = NextProposed, store.AttemptProposed, store.OutcomeProposed
	case core.StatusDenied:
		d.Next, d.State, d.Outcome = NextHoldSeat, store.AttemptDenied, store.OutcomeDenied
	case core.StatusRejected:
		d.Next, d.State, d.Outcome = NextAttempt, store.AttemptRejected, store.OutcomeFailed
	case core.StatusChangesRequested:
		d.Next, d.State, d.Outcome = NextAttempt, store.AttemptChangesRequested, store.OutcomeFailed
	case core.StatusCancelled:
		d.Next, d.State, d.Outcome = NextAttempt, store.AttemptCancelled, store.OutcomeFailed
	case core.StatusFailed:
		d.State = store.AttemptFailed
		classifyFailed(env, &d)
	case core.StatusError:
		d.State = store.AttemptError
		classifyNeverAttempted(env, &d)
	default:
		// A status this runtime does not know: nothing to build on, and
		// nothing known to be posted. Hold the conversation back.
		d.Next, d.State, d.Outcome = NextRetryLater, store.AttemptError, store.OutcomeError
	}
	return d
}

func classifyFailed(env *core.Envelope, d *Decision) {
	switch {
	case d.Code == core.CodeConflict && d.Reason == core.ReasonMovedOn:
		d.Next, d.Outcome = NextMovedOn, store.OutcomeDropped
		d.LatestMessageID = env.Detail("latest_opener_message_id")
		if d.LatestMessageID == "" {
			// Core names no message when the opener withdrew what they
			// asked last, and nothing waits for an answer: the
			// conversation is read again (stillWaiting), and what waits
			// then, if anything, answered.
			d.Next = NextAttempt
		}
	case d.Code == core.CodeConflict && (d.Reason == core.ReasonAlreadyAnswered || d.Reason == core.ReasonAnswerPending):
		d.Next, d.Outcome = NextLeave, store.OutcomeDropped
	case d.Code == core.CodeConflict && d.Reason == core.ReasonClosed:
		d.Next, d.Outcome = NextDrop, store.OutcomeDropped
	case d.Code == core.CodeForbidden && d.Reason == core.ReasonNotAddressable:
		d.Next, d.Outcome = NextDropReseat, store.OutcomeDropped
	case d.Code == core.CodeInvalidArgument && refusedSource(env, d):
		d.Next, d.Outcome = NextDropSource, store.OutcomeFailed
	case d.Code == core.CodeInvalidArgument:
		d.Next, d.Outcome = NextFix, store.OutcomeFailed
	case d.Code == core.CodeForbidden, d.Code == core.CodeNotFound:
		// Refused for the seat's sake in some way §2.4 does not list: the
		// seat's standing is what to find out about.
		d.Next, d.Outcome = NextDropReseat, store.OutcomeDropped
	default:
		d.Next, d.Outcome = NextAttempt, store.OutcomeFailed
	}
}

func classifyNeverAttempted(env *core.Envelope, d *Decision) {
	if d.Reason == core.ReasonNotRevisable {
		// What the answer named in revises is no proposal of the agent's
		// sent back for changes, as Core has it now (one a rollback of
		// Core made a rejection, say): nothing was recorded, and the next
		// attempt answers anew, naming none (revised).
		d.Next, d.Outcome = NextAttempt, store.OutcomeFailed
		return
	}
	switch d.Code {
	case core.CodeIdempotencyConflict:
		// Another body went under this key. Never regenerate under it.
		d.Next, d.Outcome = NextAttempt, store.OutcomeFailed
	case core.CodeNotFound:
		d.Next, d.Outcome = NextDrop, store.OutcomeDropped
	case core.CodeInvalidArgument:
		// The arguments did not match the tool's schema, or Core's check
		// of what they say alone refused them (AIShie-Core #60: an empty
		// or overlong body, say); the body is the only part the model
		// writes, and the sources the only part the runtime gathers.
		d.Next, d.Outcome = NextFix, store.OutcomeFailed
		if refusedSource(env, d) {
			d.Next = NextDropSource
		}
	case core.CodeForbidden:
		d.Next, d.Outcome = NextDropReseat, store.OutcomeDropped
	case core.CodeUnauthenticated:
		d.Next, d.State, d.Outcome = NextStopAgent, "", store.OutcomeError
	default:
		// internal (after Retrying gave up), rate_limited, or a code this
		// runtime does not know: nothing was attempted.
		d.Next, d.State, d.Outcome = NextRetryLater, "", store.OutcomeError
	}
}

// Core's refusals of an answer's sources (§2.10): one the answering seat
// may not read now, and one purged, each naming the source as
// field sources[i]. Its other refusals of them name the field sources, or
// a source of it, with another reason: too many, one named twice, one
// that is not as Core takes it.
const (
	reasonSourceUnreadable = "source_unreadable"
	reasonSourcePurged     = "source_purged"
)

// refusedSource reports whether env, an invalid_argument refusal of an
// answer, refused its sources, and sets d.Source to the one it names
// where it is one the seat cannot read now or one purged, and to -1 for
// all of them otherwise. A Core older than the catalogue it served, which
// takes no sources, refuses them as an argument its schema does not name,
// naming no field.
func refusedSource(env *core.Envelope, d *Decision) bool {
	field := env.Detail("field")
	switch {
	case field == "" && env.Status == core.StatusError && env.Error != nil &&
		strings.Contains(env.Error.Message, "unexpected additional properties") && strings.Contains(env.Error.Message, `"sources"`):
		d.Source = -1
		return true
	case field != "sources" && !strings.HasPrefix(field, "sources["):
		return false
	}
	d.Source = -1
	if d.Reason != reasonSourceUnreadable && d.Reason != reasonSourcePurged {
		return true
	}
	if i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(field, "sources["), "]")); err == nil && i >= 0 {
		d.Source = i
	}
	return true
}

func classifyError(err error) Decision {
	var pe *core.ProtocolError
	switch {
	case errors.Is(err, core.ErrUnauthenticated):
		return Decision{Next: NextStopAgent, Outcome: store.OutcomeError, Code: core.CodeUnauthenticated}
	case errors.As(err, &pe):
		// A bug on one side: the same bytes would fail the same way.
		return Decision{Next: NextRetryLater, State: store.AttemptError, Outcome: store.OutcomeError, Code: "protocol"}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return Decision{Next: NextRetryLater, Outcome: store.OutcomeError, Code: "cancelled"}
	}
	// *core.TransientError, *core.RateLimitedError: the same bytes again.
	return Decision{Next: NextRetryLater, Outcome: store.OutcomeError, Code: "unreachable"}
}
