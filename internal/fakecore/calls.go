package fakecore

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Call is one call the fake was sent, as the log keeps it for assertions.
type Call struct {
	// ActorID is the caller; for a 401, the actor of the revoked token when
	// the token was one the fake issued, else "".
	ActorID string
	// Transport is "mcp" or "rest".
	Transport string
	// Tool is the MCP name; "" for an MCP request that called no tool.
	Tool string
	// Args are the arguments as sent, the idempotency key included over MCP.
	Args           json.RawMessage
	IdempotencyKey string
	// Revises is the proposal the call named as the one it revises (the
	// revises argument over MCP, the Revises header over REST), "" for
	// none.
	Revises string
	// Status is the envelope's status; "" when none was given (an injected
	// answer, a 401, a 429).
	Status   string
	Code     string
	ActionID string
	Replayed bool
	// HTTPStatus is what the call was answered with at the HTTP level.
	HTTPStatus int
	At         time.Time
}

// InjectedCall is a call as Inject sees it, before it is attempted. The
// token has been checked.
type InjectedCall struct {
	ActorID   string
	Transport string
	// Method is the JSON-RPC method over MCP (tools/call, initialize, …),
	// the HTTP method over REST.
	Method         string
	Tool           string
	Args           json.RawMessage
	IdempotencyKey string
}

// Injection is how a call is to be answered instead of, or around, being
// attempted.
type Injection struct {
	// Status, when not zero, answers the call with this HTTP status without
	// attempting it: 429, 500, 503, 401, ….
	Status int
	// RetryAfter is a 429's Retry-After, in whole seconds rounded up; zero
	// sends none. The body says retry_after_seconds as Core's does.
	RetryAfter time.Duration
	// Body and ContentType replace the answer Core would give for Status.
	Body        []byte
	ContentType string
	// Delay holds the call this long first; then it is answered with
	// Status, or attempted.
	Delay time.Duration
	// DelayAfter attempts the call and holds its answer this long: a call
	// that times out after Core carried it out.
	DelayAfter time.Duration
}

// Inject sets f to look at every authenticated request before it is
// attempted; a nil Injection lets the call through. Inject(nil) removes it.
func (c *Core) Inject(f func(InjectedCall) *Injection) {
	c.hooks.Lock()
	defer c.hooks.Unlock()
	c.inject = f
}

// OnCall sets f to be called with each tool call's tool name and arguments
// as sent, before the call is carried out and outside the fake's lock, so
// that f may use the test controls: the opener writing while an answer is
// being generated, say. OnCall(nil) removes it.
func (c *Core) OnCall(f func(tool string, args json.RawMessage)) {
	c.hooks.Lock()
	defer c.hooks.Unlock()
	c.onCall = f
}

// Calls is every call logged so far, oldest first.
func (c *Core) Calls() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Call(nil), c.calls...)
}

func (c *Core) logCall(call Call, out outcome, httpStatus int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	call.Status, call.Code, call.ActionID, call.Replayed = out.Status, codeOf(out), out.ActionID, out.Replayed
	call.HTTPStatus, call.At = httpStatus, c.now()
	c.calls = append(c.calls, call)
}

// logRefused logs a call answered at the HTTP level, never attempted.
func (c *Core) logRefused(p *peek, httpStatus int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, Call{ActorID: p.actorID, Transport: p.transport, Tool: p.tool, Args: p.args,
		IdempotencyKey: p.key, HTTPStatus: httpStatus, At: c.now()})
}

// injected answers or holds a call as the test's Inject says.
func (c *Core) injected(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hooks.RLock()
		f := c.inject
		c.hooks.RUnlock()
		if f == nil {
			next.ServeHTTP(w, r)
			return
		}
		p := peekOf(r)
		in := f(InjectedCall{ActorID: p.actorID, Transport: p.transport, Method: p.method, Tool: p.tool, Args: p.args, IdempotencyKey: p.key})
		if in == nil {
			next.ServeHTTP(w, r)
			return
		}
		if !sleep(r.Context(), in.Delay) {
			return
		}
		if in.Status != 0 {
			writeInjected(w, p.transport, in)
			c.logRefused(p, in.Status)
			return
		}
		if in.DelayAfter <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		// Carried out whether or not the client is still there to hear it.
		rec := &recorder{header: http.Header{}}
		next.ServeHTTP(rec, r.WithContext(context.WithoutCancel(r.Context())))
		// The call is done; its answer is held as a slow network would
		// hold it, whether or not the client is still waiting.
		sleep(context.WithoutCancel(r.Context()), in.DelayAfter)
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	})
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// writeInjected answers as Core answers with that status.
func writeInjected(w http.ResponseWriter, transport string, in *Injection) {
	if in.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(in.RetryAfter.Seconds()))))
	}
	body, ctype := in.Body, in.ContentType
	if body == nil {
		body, ctype = injectedBody(transport, in)
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	if strings.HasPrefix(ctype, "text/plain") {
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	w.WriteHeader(in.Status)
	_, _ = w.Write(body)
}

func injectedBody(transport string, in *Injection) ([]byte, string) {
	var e *apiError
	switch {
	case in.Status == http.StatusTooManyRequests:
		secs := int(math.Ceil(in.RetryAfter.Seconds()))
		e = newErr(codeRateLimited, "too many calls; try again in %d seconds", secs)
		if secs > 0 {
			e = e.with("retry_after_seconds", secs)
		}
		b, _ := json.Marshal(map[string]any{"error": e})
		return append(b, '\n'), "application/json"
	case in.Status == http.StatusUnauthorized && transport == "mcp":
		return []byte("invalid token: the credential is missing or not valid\n"), "text/plain; charset=utf-8"
	case in.Status == http.StatusUnauthorized:
		e = newErr(codeUnauthenticated, "the credential is missing or not valid")
	case transport == "mcp":
		return []byte(http.StatusText(in.Status) + "\n"), "text/plain; charset=utf-8"
	default:
		e = newErr(codeInternal, "something went wrong on our side; the call can be retried with the same idempotency key")
	}
	b, _ := json.Marshal(map[string]any{"error": e})
	return append(b, '\n'), "application/json"
}

// recorder keeps an answer to be sent later.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

// refusedLogged logs a 401 given to a token the fake issued and has since
// revoked, so that a test can see an agent keep calling after it lost its
// token.
func (c *Core) refusedLogged(transport string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status != http.StatusUnauthorized {
			return
		}
		_, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		c.mu.Lock()
		defer c.mu.Unlock()
		var actorID string
		if cr := c.tokens[strings.TrimSpace(token)]; cr != nil {
			if !cr.revoked() {
				return // an injected 401, logged where it was injected
			}
			actorID = cr.actor.id
		}
		c.calls = append(c.calls, Call{ActorID: actorID, Transport: transport, HTTPStatus: http.StatusUnauthorized, At: c.now()})
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the writer underneath.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
