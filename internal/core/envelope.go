// Package core is the runtime's client of AIshie Core: one agent's
// connection, over MCP (or REST), with that agent's token. Core pushes
// nothing and decides everything; this package only carries calls and reads
// what came back (Core's docs/agent-runtime.md §1.2, §2).
package core

import (
	"encoding/json"
	"fmt"
)

// Status is an envelope's status (§2.1).
type Status string

const (
	StatusExecuted Status = "executed"
	StatusProposed Status = "proposed"
	StatusDenied   Status = "denied"
	StatusFailed   Status = "failed"
	// StatusRejected and StatusCancelled come only on the replay of an old
	// proposal.
	StatusRejected  Status = "rejected"
	StatusCancelled Status = "cancelled"
	// StatusError is a call never attempted: bad arguments, no such tool or
	// target, a reused key, a fault of Core's. Nothing was recorded.
	StatusError Status = "error"
)

// Error codes (§2.1). Classify by code and details, never by message.
const (
	CodeInvalidArgument     = "invalid_argument"
	CodeUnauthenticated     = "unauthenticated"
	CodeForbidden           = "forbidden"
	CodeNotFound            = "not_found"
	CodeConflict            = "conflict"
	CodeIdempotencyConflict = "idempotency_conflict"
	CodeFailedPrecondition  = "failed_precondition"
	CodeRateLimited         = "rate_limited"
	CodeInternal            = "internal"
)

// Reasons Core gives in error.details.reason for an answer it refused
// (§2.4).
const (
	ReasonMovedOn         = "moved_on"
	ReasonAlreadyAnswered = "already_answered"
	ReasonAnswerPending   = "answer_pending"
	ReasonClosed          = "closed"
	ReasonNotAddressable  = "not_addressable"
)

// Review states. executed with pending is done, and a person looks at it
// afterwards.
const (
	ReviewNone      = "none"
	ReviewPending   = "pending"
	ReviewReviewed  = "reviewed"
	ReviewEscalated = "escalated"
)

// Envelope is what every call returns (§2.1).
type Envelope struct {
	Status      Status          `json:"status"`
	ActionID    string          `json:"action_id,omitempty"`
	ReviewState string          `json:"review_state,omitempty"`
	Replayed    bool            `json:"replayed,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Note        string          `json:"note,omitempty"`
	Error       *Error          `json:"error,omitempty"`
}

// Error is an envelope's error.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// Code is the error's code, or "" when there is none.
func (e *Envelope) Code() string {
	if e == nil || e.Error == nil {
		return ""
	}
	return e.Error.Code
}

// Reason is error.details.reason, or "".
func (e *Envelope) Reason() string { return e.Detail("reason") }

// Detail is error.details[key] when it is a string, or "".
func (e *Envelope) Detail(key string) string {
	if e == nil || e.Error == nil || e.Error.Details == nil {
		return ""
	}
	s, _ := e.Error.Details[key].(string)
	return s
}

// OK reports whether the call was executed.
func (e *Envelope) OK() bool { return e != nil && e.Status == StatusExecuted }

// Decode decodes the result into v.
func (e *Envelope) Decode(v any) error {
	if len(e.Result) == 0 {
		return fmt.Errorf("core: the envelope has no result")
	}
	return json.Unmarshal(e.Result, v)
}

// EnvelopeError is a read that came back with any status but executed. The
// envelope says why; callers classify it by Code and Reason.
type EnvelopeError struct {
	Tool     string
	Envelope *Envelope
}

func (e *EnvelopeError) Error() string {
	s := fmt.Sprintf("core: %s: %s", e.Tool, e.Envelope.Status)
	if c := e.Envelope.Code(); c != "" {
		s += " " + c
	}
	if r := e.Envelope.Reason(); r != "" {
		s += " (" + r + ")"
	}
	return s
}
