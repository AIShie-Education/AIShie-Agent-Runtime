package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Caller makes one call to Core as one agent, with that agent's token. Its
// transports are MCP (MCPCaller) and REST (RESTCaller); Retrying and the
// rate limiter wrap it.
//
// A call Core answered, whatever it said, is an *Envelope and a nil error:
// denied, failed and error are outcomes to act on, not transport failures.
// The error is for a call Core did not answer:
//   - ErrUnauthenticated: HTTP 401, the token is missing, expired or revoked.
//     Stop the agent and tell its owner.
//   - *RateLimitedError: HTTP 429; nothing was attempted.
//   - *TransientError: a 5xx, a network error or a timeout. Send the same
//     bytes again, after a backoff.
//   - *ProtocolError: a JSON-RPC error, or an answer that is not an
//     envelope: a bug on one side or the other, never retried.
type Caller interface {
	Call(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error)
}

// ErrUnauthenticated is Core's 401.
var ErrUnauthenticated = errors.New("core: the token is missing, expired or revoked (401)")

// RateLimitedError is Core's 429.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("core: rate limited; retry after %s", e.RetryAfter)
}

// TransientError is a call that may succeed if sent again unchanged.
type TransientError struct {
	// Status is the HTTP status, 0 for a network error or timeout.
	Status int
	Err    error
}

func (e *TransientError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("core: HTTP %d: %v", e.Status, e.Err)
	}
	return fmt.Sprintf("core: %v", e.Err)
}

func (e *TransientError) Unwrap() error { return e.Err }

// ProtocolError is a JSON-RPC error, or an answer that could not be read.
type ProtocolError struct {
	Code    int64
	Message string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("core: protocol error %d: %s", e.Code, e.Message)
}

// Priority orders calls waiting for an agent's rate limit (§7.3): answers
// before polling, polling before events.
type Priority int

const (
	// PriorityAnswer is everything that serves a claimed question: reading
	// it, the model's tool calls, posting the answer.
	PriorityAnswer Priority = iota
	// PriorityPoll is conversation_inbox.
	PriorityPoll
	// PriorityBackground is event_list, action_list_mine, me_memberships.
	PriorityBackground
)

type priorityKey struct{}

// WithPriority marks ctx's calls with p.
func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityKey{}, p)
}

// PriorityOf is ctx's priority; PriorityBackground when unmarked.
func PriorityOf(ctx context.Context) Priority {
	if p, ok := ctx.Value(priorityKey{}).(Priority); ok {
		return p
	}
	return PriorityBackground
}
