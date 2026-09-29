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

type bestEffortKey struct{}

// WithBestEffort marks ctx's calls as best effort, as the drafts of
// answers in progress are (conversation_draft): Limited lets them through
// without a token of the agent's bucket, since Core does not count them
// against the actor's limit, and Retrying never sends one again, whatever
// came back, so that its caller alone decides whether one is worth
// another try.
func WithBestEffort(ctx context.Context) context.Context {
	return context.WithValue(ctx, bestEffortKey{}, true)
}

// BestEffort reports whether ctx's calls are best effort (WithBestEffort).
func BestEffort(ctx context.Context) bool {
	b, _ := ctx.Value(bestEffortKey{}).(bool)
	return b
}

// MaxWait is the longest a call may wait for news (Core's wait_s, 0 to 25
// seconds): a Core whose catalogue offers wait_s bounds it so.
const MaxWait = 25 * time.Second

// WaitMargin is how much longer than its wait a call that waits for news
// is given to answer: Core holds its end open for the wait, then answers
// as any call does (Core's docs/agent-runtime.md §7.2 asks for wait_s plus
// 15 s or so).
const WaitMargin = 15 * time.Second

type waitKey struct{}

// WithWait marks ctx's calls as waiting for news up to d (wait_s): the
// transports give each d plus WaitMargin to answer, where the HTTP client's
// own timeout is shorter, and Retrying does not send one again after a
// transient failure. A call that is not marked keeps the client's timeout.
func WithWait(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, waitKey{}, d)
}

// WaitOf is how long ctx's calls wait for news; 0 when unmarked.
func WaitOf(ctx context.Context) time.Duration {
	d, _ := ctx.Value(waitKey{}).(time.Duration)
	return max(d, 0)
}

// waitSeconds is d as wait_s: whole seconds, rounded up, at most MaxWait;
// 0 for no wait.
func waitSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((min(d, MaxWait) + time.Second - 1) / time.Second)
}
