package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// RetryOptions configure Retrying. The zero value is the handout's (§7.2):
// back off 1, 2, 4 … 60 s with full jitter.
type RetryOptions struct {
	// Base is the first backoff's ceiling; 1 s when 0.
	Base time.Duration
	// Max is the ceiling backoff doubles up to; 60 s when 0.
	Max time.Duration
	// OnRateLimited is told of every 429, with Core's Retry-After, when it
	// comes and before the wait: the agent's pollers slow down for a while
	// (§7.2). It is called from the goroutine making the call, so from many
	// at once when the Retrying is shared.
	OnRateLimited func(retryAfter time.Duration)
	// Sleep waits d or until ctx ends, returning ctx's error then; a timer
	// when nil. For tests.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter is a random duration in [0, max]; uniform when nil. For tests.
	Jitter func(max time.Duration) time.Duration
}

// Retrying is a Caller that sends a call again, the same bytes under the
// same key (§2.1, §2.2), for as long as its context allows:
//   - on a *TransientError, and on an envelope of status error with code
//     internal, after a backoff of 1, 2, 4 … 60 s with full jitter;
//   - on a *RateLimitedError, after Core's Retry-After and up to a second of
//     jitter, having told OnRateLimited.
//
// ErrUnauthenticated, a *ProtocolError, any other error and any other
// envelope come back at once. When the context ends, the last answer comes
// back: the last envelope as it was, or the last error joined with the
// context's, so that errors.As still finds it.
//
// A call that waits for news (WithWait) is a read, with no key, that its
// caller makes again as its schedule says: it is sent again after a 429,
// but not after a transient failure or an error internal, which come back
// at once. A long poll cut short again and again, by a proxy that gives a
// request less than its wait, is then the caller's to see. A best-effort
// call (WithBestEffort) is never sent again: what came back comes back.
type Retrying struct {
	next Caller
	o    RetryOptions
}

// NewRetrying wraps next.
func NewRetrying(next Caller, o RetryOptions) *Retrying {
	if o.Base <= 0 {
		o.Base = time.Second
	}
	if o.Max <= 0 {
		o.Max = time.Minute
	}
	o.Max = max(o.Max, o.Base)
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.Jitter == nil {
		o.Jitter = fullJitter
	}
	return &Retrying{next: next, o: o}
}

// Call makes the call, and makes it again as Retrying says.
func (r *Retrying) Call(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error) {
	if BestEffort(ctx) {
		return r.next.Call(ctx, tool, args)
	}
	failures := 0
	var lastEnv *Envelope
	var lastErr error
	for {
		env, err := r.next.Call(ctx, tool, args)
		var wait time.Duration
		var rl *RateLimitedError
		switch {
		case err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) && (lastEnv != nil || lastErr != nil):
			// The context ended during the call (or while it waited for a
			// token): what Core said last is the answer.
			return r.last(ctx, lastEnv, lastErr)
		case WaitOf(ctx) > 0 && (isTransient(err) || err == nil && env != nil && env.Status == StatusError && env.Code() == CodeInternal):
			return env, err
		case err == nil && env != nil && env.Status == StatusError && env.Code() == CodeInternal:
			lastEnv, lastErr = env, nil
			wait = r.backoff(failures)
			failures++
		case err == nil:
			return env, nil
		case errors.As(err, &rl):
			lastEnv, lastErr = nil, err
			if r.o.OnRateLimited != nil {
				r.o.OnRateLimited(rl.RetryAfter)
			}
			wait = rl.RetryAfter + r.o.Jitter(time.Second)
		case isTransient(err):
			lastEnv, lastErr = nil, err
			wait = r.backoff(failures)
			failures++
		default:
			return env, err
		}
		if err := r.o.Sleep(ctx, wait); err != nil || ctx.Err() != nil {
			return r.last(ctx, lastEnv, lastErr)
		}
	}
}

func (r *Retrying) last(ctx context.Context, env *Envelope, err error) (*Envelope, error) {
	if err == nil {
		return env, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return nil, fmt.Errorf("%w (retrying stopped: %w)", err, ctxErr)
	}
	return nil, err
}

// backoff is the wait after the (n+1)th failure in a row: a random time up
// to Base×2ⁿ, at most Max.
func (r *Retrying) backoff(n int) time.Duration {
	ceiling := r.o.Max
	if n < 32 {
		if d := r.o.Base << n; d > 0 && d < ceiling {
			ceiling = d
		}
	}
	return r.o.Jitter(ceiling)
}

func isTransient(err error) bool {
	var t *TransientError
	return errors.As(err, &t)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func fullJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return rand.N(max + 1) //nolint:gosec // jitter spreads retries; it guards nothing
}
