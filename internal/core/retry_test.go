package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// answer is one scripted outcome.
type answer struct {
	env *Envelope
	err error
}

// scripted is a Caller that plays its answers in order, the last one again
// once they run out, and keeps the bytes of every call.
type scripted struct {
	mu      sync.Mutex
	answers []answer
	each    func(ctx context.Context, n int) // before answering call n (0-based)
	args    [][]byte
}

func (s *scripted) Call(ctx context.Context, _ string, args json.RawMessage) (*Envelope, error) {
	s.mu.Lock()
	n := len(s.args)
	s.args = append(s.args, bytes.Clone(args))
	a := s.answers[min(n, len(s.answers)-1)]
	each := s.each
	s.mu.Unlock()
	if each != nil {
		each(ctx, n)
	}
	return a.env, a.err
}

func (s *scripted) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.args)
}

// clock records the sleeps asked for and makes none. Jitter gives the most
// it may, so that the ceilings show.
type clock struct {
	sleeps  []time.Duration
	jitters []time.Duration
	onSleep func(n int) error
}

func (c *clock) options() RetryOptions {
	return RetryOptions{
		Sleep: func(_ context.Context, d time.Duration) error {
			c.sleeps = append(c.sleeps, d)
			if c.onSleep != nil {
				return c.onSleep(len(c.sleeps))
			}
			return nil
		},
		Jitter: func(max time.Duration) time.Duration {
			c.jitters = append(c.jitters, max)
			return max
		},
	}
}

var (
	transientErr = &TransientError{Status: 502, Err: errors.New("bad gateway")}
	executedEnv  = &Envelope{Status: StatusExecuted}
	internalEnv  = &Envelope{Status: StatusError, Error: &Error{Code: CodeInternal, Message: "retry with the same idempotency_key"}}
)

func durations(ds ...float64) []time.Duration {
	out := make([]time.Duration, len(ds))
	for i, d := range ds {
		out[i] = time.Duration(d * float64(time.Second))
	}
	return out
}

func sameDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRetryingBacksOffAndSendsTheSameBytes(t *testing.T) {
	var answers []answer
	for range 8 {
		answers = append(answers, answer{err: transientErr})
	}
	answers = append(answers, answer{env: internalEnv}, answer{env: executedEnv})
	next := &scripted{answers: answers}
	var clk clock
	args := json.RawMessage(`{"course_id":"c","body":"x","idempotency_key":"answer:x:m:1"}`)

	env, err := NewRetrying(next, clk.options()).Call(context.Background(), "conversation_answer", args)
	if err != nil || env != executedEnv {
		t.Fatalf("got %+v %v", env, err)
	}
	if want := durations(1, 2, 4, 8, 16, 32, 60, 60, 60); !sameDurations(clk.sleeps, want) {
		t.Fatalf("slept %v, want %v", clk.sleeps, want)
	}
	if next.calls() != 10 {
		t.Fatalf("%d calls, want 10", next.calls())
	}
	for i, sent := range next.args {
		if !bytes.Equal(sent, args) {
			t.Fatalf("call %d sent %s, want %s", i+1, sent, args)
		}
	}
}

func TestRetryingFullJitter(t *testing.T) {
	next := &scripted{answers: []answer{{err: transientErr}, {err: transientErr}, {env: executedEnv}}}
	var ceilings, sleeps []time.Duration
	o := RetryOptions{
		Base: 100 * time.Millisecond, Max: 150 * time.Millisecond,
		Jitter: func(max time.Duration) time.Duration { ceilings = append(ceilings, max); return max / 4 },
		Sleep:  func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
	}
	if _, err := NewRetrying(next, o).Call(context.Background(), "me_get", nil); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{100 * time.Millisecond, 150 * time.Millisecond}; !sameDurations(ceilings, want) {
		t.Fatalf("jitter ceilings %v, want %v", ceilings, want)
	}
	if want := []time.Duration{25 * time.Millisecond, 37500 * time.Microsecond}; !sameDurations(sleeps, want) {
		t.Fatalf("slept %v, want %v", sleeps, want)
	}
}

func TestRetryingRateLimited(t *testing.T) {
	next := &scripted{answers: []answer{
		{err: transientErr},
		{err: &RateLimitedError{RetryAfter: 7 * time.Second}},
		{err: transientErr},
		{env: executedEnv},
	}}
	var clk clock
	var told []time.Duration
	o := clk.options()
	o.OnRateLimited = func(d time.Duration) { told = append(told, d) }
	env, err := NewRetrying(next, o).Call(context.Background(), "conversation_inbox", nil)
	if err != nil || env != executedEnv {
		t.Fatalf("got %v %v", env, err)
	}
	// A 429 is not a failure of Core's: the backoff after it goes on from
	// where it was.
	if want := durations(1, 8, 2); !sameDurations(clk.sleeps, want) {
		t.Fatalf("slept %v, want %v", clk.sleeps, want)
	}
	if want := durations(1, 1, 2); !sameDurations(clk.jitters, want) {
		t.Fatalf("jitter asked for %v, want %v (up to a second after a 429)", clk.jitters, want)
	}
	if want := durations(7); !sameDurations(told, want) {
		t.Fatalf("OnRateLimited told %v, want %v", told, want)
	}
}

func TestRetryingReturnsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    answer
	}{
		{"401", answer{err: ErrUnauthenticated}},
		{"a protocol error", answer{err: &ProtocolError{Code: -32602, Message: "unknown tool"}}},
		{"an error of no known kind", answer{err: errors.New("odd")}},
		{"denied", answer{env: &Envelope{Status: StatusDenied, Error: &Error{Code: CodeForbidden}}}},
		{"failed", answer{env: &Envelope{Status: StatusFailed, Error: &Error{Code: CodeConflict}}}},
		{"an error that is not internal", answer{env: &Envelope{Status: StatusError, Error: &Error{Code: CodeIdempotencyConflict}}}},
		{"proposed", answer{env: &Envelope{Status: StatusProposed}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := &scripted{answers: []answer{tc.a, {env: executedEnv}}}
			var clk clock
			env, err := NewRetrying(next, clk.options()).Call(context.Background(), "x", nil)
			if env != tc.a.env || !errors.Is(err, tc.a.err) {
				t.Fatalf("got %v %v, want %v %v", env, err, tc.a.env, tc.a.err)
			}
			if next.calls() != 1 || len(clk.sleeps) != 0 {
				t.Fatalf("%d calls, %d sleeps", next.calls(), len(clk.sleeps))
			}
		})
	}
}

func TestRetryingStopsWithTheContext(t *testing.T) {
	t.Run("while sleeping, after an error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		next := &scripted{answers: []answer{{err: transientErr}}}
		var clk clock
		clk.onSleep = func(n int) error {
			if n == 3 {
				cancel()
				return ctx.Err()
			}
			return nil
		}
		_, err := NewRetrying(next, clk.options()).Call(ctx, "x", nil)
		var te *TransientError
		if !errors.As(err, &te) || te != transientErr || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		if next.calls() != 3 {
			t.Fatalf("%d calls", next.calls())
		}
	})
	t.Run("after an internal envelope", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		next := &scripted{answers: []answer{{env: internalEnv}}}
		var clk clock
		clk.onSleep = func(int) error { cancel(); return nil } // a Sleep that misses the end
		env, err := NewRetrying(next, clk.options()).Call(ctx, "x", nil)
		if err != nil || env != internalEnv {
			t.Fatalf("got %v %v, want the last envelope", env, err)
		}
	})
	t.Run("while waiting for a token", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		next := &scripted{answers: []answer{{err: transientErr}, {err: context.Canceled}}}
		next.each = func(_ context.Context, n int) {
			if n == 1 {
				cancel() // Limited's Wait gives up and returns ctx.Err()
			}
		}
		var clk clock
		_, err := NewRetrying(next, clk.options()).Call(ctx, "x", nil)
		var te *TransientError
		if !errors.As(err, &te) || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want the 502 and the context's end", err)
		}
	})
	t.Run("the call itself cut off", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		next := &scripted{answers: []answer{{err: &TransientError{Err: context.Canceled}}}}
		next.each = func(context.Context, int) { cancel() }
		var clk clock
		_, err := NewRetrying(next, clk.options()).Call(ctx, "x", nil)
		var te *TransientError
		if !errors.As(err, &te) || !errors.Is(err, context.Canceled) || next.calls() != 1 {
			t.Fatalf("got %v after %d calls", err, next.calls())
		}
	})
	t.Run("the real sleep", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		next := &scripted{answers: []answer{{err: transientErr}}}
		start := time.Now()
		_, err := NewRetrying(next, RetryOptions{Base: time.Hour}).Call(ctx, "x", nil)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
			t.Fatalf("got %v after %s", err, time.Since(start))
		}
	})
}

func TestRetryingWithTheRealClock(t *testing.T) {
	next := &scripted{answers: []answer{{err: transientErr}, {err: transientErr}, {env: internalEnv}, {env: executedEnv}}}
	env, err := NewRetrying(next, RetryOptions{Base: time.Millisecond, Max: 2 * time.Millisecond}).Call(context.Background(), "x", nil)
	if err != nil || env != executedEnv || next.calls() != 4 {
		t.Fatalf("got %v %v after %d calls", env, err, next.calls())
	}
}

func TestBackoffDoesNotOverflow(t *testing.T) {
	r := NewRetrying(nil, RetryOptions{Jitter: func(max time.Duration) time.Duration { return max }})
	for _, n := range []int{0, 5, 6, 30, 31, 32, 62, 63, 64, 1000} {
		d := r.backoff(n)
		if d <= 0 || d > time.Minute {
			t.Fatalf("backoff(%d) = %s", n, d)
		}
	}
	if d := fullJitter(0); d != 0 {
		t.Fatalf("fullJitter(0) = %s", d)
	}
	for range 1000 {
		if d := fullJitter(time.Second); d < 0 || d > time.Second {
			t.Fatalf("fullJitter(1s) = %s", d)
		}
	}
}

// Over the wire, a retry is the very same request: the same body, under
// the same key, over MCP and over REST.
func TestRetryingResendsTheSameRequest(t *testing.T) {
	args := json.RawMessage(`{"course_id":"C","conversation_id":"X","in_reply_to_message_id":"M","body":"a <b>","idempotency_key":"answer:X:M:1"}`)
	o := RetryOptions{Base: time.Millisecond, Max: time.Millisecond}

	t.Run("MCP", func(t *testing.T) {
		f := newFakeMCP(t)
		var mu sync.Mutex
		var got []json.RawMessage
		f.call = func(w http.ResponseWriter, id json.RawMessage, _ string, a json.RawMessage) {
			mu.Lock()
			got = append(got, a)
			n := len(got)
			mu.Unlock()
			if n < 3 {
				http.Error(w, "down", http.StatusBadGateway)
				return
			}
			writeResult(w, id, toolResult(`{"status":"executed","action_id":"a1","result":{"message_id":"m2"}}`))
		}
		env, err := NewRetrying(newTestMCP(f.URL), o).Call(context.Background(), "conversation_answer", args)
		if err != nil || !env.OK() {
			t.Fatalf("got %+v %v", env, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(got) != 3 {
			t.Fatalf("%d calls", len(got))
		}
		for _, a := range got {
			if !bytes.Equal(a, args) {
				t.Fatalf("sent %s, want %s", a, args)
			}
		}
	})
	t.Run("REST", func(t *testing.T) {
		var n atomic.Int32
		srv, seen := newFakeREST(t, func(w http.ResponseWriter, _ *http.Request) {
			if n.Add(1) < 3 {
				writeJSON(w, 500, map[string]any{"error": map[string]any{"code": "internal", "message": "retry"}})
				return
			}
			writeJSON(w, 200, map[string]any{"status": "executed", "action_id": "a1"})
		})
		c := NewRESTCaller(RESTOptions{BaseURL: srv.URL, Token: testToken, Catalogue: testCatalogue(t)})
		env, err := NewRetrying(c, o).Call(context.Background(), "conversation_answer", args)
		if err != nil || !env.OK() {
			t.Fatalf("got %+v %v", env, err)
		}
		reqs := seen()
		if len(reqs) != 3 {
			t.Fatalf("%d requests", len(reqs))
		}
		for _, r := range reqs {
			if r.Path != reqs[0].Path || r.Body != reqs[0].Body || r.Header.Get("Idempotency-Key") != "answer:X:M:1" {
				t.Fatalf("request %+v differs from the first %+v", r, reqs[0])
			}
		}
	})
}

// A best-effort call is never sent again, whatever came back, and a 429 of
// its does not slow the agent's polling.
func TestRetryingSendsBestEffortOnce(t *testing.T) {
	for _, a := range []answer{
		{err: transientErr},
		{err: &RateLimitedError{RetryAfter: time.Second}},
		{env: internalEnv},
	} {
		s := &scripted{answers: []answer{a, {env: executedEnv}}}
		c := &clock{}
		o := c.options()
		slowed := false
		o.OnRateLimited = func(time.Duration) { slowed = true }
		env, err := NewRetrying(s, o).Call(WithBestEffort(context.Background()), "conversation_draft", json.RawMessage(`{}`))
		if s.calls() != 1 || len(c.sleeps) != 0 || slowed {
			t.Errorf("%v %v: %d calls, sleeps %v, slowed %v", a.env, a.err, s.calls(), c.sleeps, slowed)
		}
		if env != a.env || !errors.Is(err, a.err) {
			t.Errorf("came back %v %v, want %v %v", env, err, a.env, a.err)
		}
	}
}
