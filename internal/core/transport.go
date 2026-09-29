package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version"
)

// DefaultMaxResponseBytes bounds what one answer of Core's may be. The
// largest a runtime asks for is a page of 200 messages of up to 20,000
// characters each, which MCP carries twice (§2.1), escaped.
const DefaultMaxResponseBytes int64 = 32 << 20

// DefaultTimeout bounds one request to Core when the caller gives no
// http.Client of its own. A call that times out is a *TransientError, sent
// again unchanged: its idempotency key makes that safe (§2.2). A call that
// waits for news (WithWait) is given longer (forCall).
const DefaultTimeout = 30 * time.Second

// DefaultRetryAfter is how long a 429 that names no time asks for.
const DefaultRetryAfter = time.Second

// maxRetryAfter holds a 429's wait to something a runtime can sit out: a
// wait of days is a fault somewhere, and the context ends it anyway.
const maxRetryAfter = time.Hour

// clientName is what the runtime calls itself to Core.
const clientName = "aishie-runtime"

// defaultHTTPClient follows no redirect: Core never answers a call with
// one, and one followed would make the call somewhere else.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: DefaultTimeout, CheckRedirect: stopAtRedirect}
}

// withoutRedirects is c, or the default client when c is nil, made to
// follow no redirect: a copy, sharing c's transport, jar and timeout. A
// client that followed one would send the call again elsewhere, a write's
// body and the token with it (Go keeps Authorization on a redirect to the
// same host), before the answer could be refused.
func withoutRedirects(c *http.Client) *http.Client {
	if c == nil {
		return defaultHTTPClient()
	}
	cp := *c
	cp.CheckRedirect = stopAtRedirect
	return &cp
}

func stopAtRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// forCall is c for one call made with ctx: for a call that waits for news
// (WithWait), a copy, sharing c's transport, whose timeout is the wait plus
// WaitMargin where c's own is shorter; c itself for any other call, and for
// a client with no timeout, which the context alone bounds.
func forCall(ctx context.Context, c *http.Client) *http.Client {
	w := WaitOf(ctx)
	if w <= 0 || c.Timeout <= 0 || c.Timeout >= w+WaitMargin {
		return c
	}
	cp := *c
	cp.Timeout = w + WaitMargin
	return &cp
}

// redirected reports whether the answer is a redirect, or came from
// somewhere other than where the request went (a client that follows
// redirects followed one). The call is refused then, whatever came back,
// since it was not the call made.
func redirected(req *http.Request, resp *http.Response) bool {
	return resp.StatusCode >= 300 && resp.StatusCode <= 399 ||
		resp.Request != nil && resp.Request.URL.String() != req.URL.String()
}

// errRedirected is what a redirected call returns.
func errRedirected(status int) *ProtocolError {
	return &ProtocolError{Message: fmt.Sprintf("HTTP %d: the call was redirected, and the runtime follows no redirect", status)}
}

func userAgent(name, ver string) string { return name + "/" + ver }

func defaultVersion() string { return version.Version }

// errTooLarge is an answer longer than the caller reads.
var errTooLarge = errors.New("the answer is larger than the runtime reads")

// boundedReader reads r, failing with errTooLarge past max bytes rather than
// stopping short, so that a long answer is never taken for a whole one.
type boundedReader struct {
	r   io.Reader
	max int64
	n   int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.n > b.max {
		return 0, errTooLarge
	}
	if rest := b.max + 1 - b.n; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.max {
		return n, errTooLarge
	}
	return n, err
}

// readBody reads an answer's body whole, up to max bytes. A read that fails
// part-way (the connection dropped, the body cut short) is a
// *TransientError; one past max, a *ProtocolError.
func readBody(resp *http.Response, max int64) ([]byte, error) {
	data, err := io.ReadAll(&boundedReader{r: resp.Body, max: max})
	switch {
	case errors.Is(err, errTooLarge):
		return nil, &ProtocolError{Message: fmt.Sprintf("HTTP %d: %v (%d bytes)", resp.StatusCode, errTooLarge, max)}
	case err != nil:
		return nil, &TransientError{Status: resp.StatusCode, Err: fmt.Errorf("reading the answer: %w", err)}
	}
	return data, nil
}

// drain reads a little of a body that will not be used, so that the
// connection can be used again, and closes it.
func drain(resp *http.Response) {
	_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
	_ = resp.Body.Close()
}

// sendError is a request that got no answer: a network error or a timeout,
// always a *TransientError. The request's URL is in err's text; it holds no
// secret (the token goes in a header), but redact takes care of that too.
func sendError(ctx context.Context, err error, token string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &TransientError{Err: ctxErr}
	}
	return &TransientError{Err: errors.New(redact(err.Error(), token))}
}

// rateLimited reads a 429: Retry-After, else the body's
// error.details.retry_after_seconds, else DefaultRetryAfter. Nothing was
// attempted (§2.1).
func rateLimited(resp *http.Response, max int64) *RateLimitedError {
	if d := retryAfterHeader(resp.Header.Get("Retry-After"), time.Now()); d > 0 {
		drain(resp)
		return &RateLimitedError{RetryAfter: d}
	}
	body, err := io.ReadAll(&boundedReader{r: resp.Body, max: min(max, 64<<10)})
	_ = resp.Body.Close()
	if err == nil {
		var b struct {
			Error struct {
				Details struct {
					RetryAfterSeconds json.Number `json:"retry_after_seconds"`
				} `json:"details"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &b) == nil {
			if s, err := b.Error.Details.RetryAfterSeconds.Float64(); err == nil && s > 0 {
				return &RateLimitedError{RetryAfter: seconds(s)}
			}
		}
	}
	return &RateLimitedError{RetryAfter: DefaultRetryAfter}
}

// retryAfterHeader reads Retry-After as seconds or as an HTTP date; 0 when
// it is absent, unreadable or already past.
func retryAfterHeader(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		if n <= 0 || math.IsNaN(n) {
			return 0
		}
		return seconds(n)
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return min(d, maxRetryAfter)
		}
	}
	return 0
}

// seconds is n seconds, at most maxRetryAfter.
func seconds(n float64) time.Duration {
	if n >= maxRetryAfter.Seconds() {
		return maxRetryAfter
	}
	return time.Duration(n * float64(time.Second))
}

// serverError is a 5xx: Core, or something in front of it, failed. Only the
// start of what it said is kept.
func serverError(status int, body []byte, token string) *TransientError {
	return &TransientError{Status: status, Err: errors.New(said(body, token))}
}

// said is the start of what a server said, on one line, fit for an error:
// never the token, never much of it.
func said(body []byte, token string) string {
	s := strings.TrimSpace(string(body[:min(len(body), maxQuoted)]))
	if s == "" {
		return "no body"
	}
	return quote(s, token, 200)
}

// maxQuoted is as much of what a server said as is looked at for an
// error: far more than is kept, so that what is kept was redacted whole.
const maxQuoted = 64 << 10

// quote is s fit for an error: redacted, then cut to n bytes on one line.
// Redacted first, so that a cut never leaves part of a token to be seen.
func quote(s, token string, n int) string {
	return clip(redact(s[:min(len(s), maxQuoted)], token), n)
}

// clip cuts s to at most n bytes, on a rune boundary, on one line.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// tokenShape is Core's token forms (ais_… and aisinv_…), removed from any
// text that goes into an error whether or not it is this caller's.
var tokenShape = regexp.MustCompile(`ais(inv)?_[A-Za-z0-9_-]+`)

// redact removes token and anything shaped like one of Core's from s. A
// server should never echo a token, but an error is logged, and a token in
// a log is a token leaked (§6.1).
func redact(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "[redacted]")
	}
	return tokenShape.ReplaceAllString(s, "[redacted]")
}

// decodeEnvelope reads an envelope from JSON: an object with a status.
func decodeEnvelope(raw []byte) (*Envelope, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var env Envelope
	if json.Unmarshal(raw, &env) != nil || env.Status == "" {
		return nil, false
	}
	return &env, true
}

// validHeaderValue reports whether v can be sent as an HTTP header's value:
// no control characters, which would end the header or be refused.
func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}
