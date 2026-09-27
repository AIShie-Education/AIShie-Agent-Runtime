package gemini

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

// maxErrorBody bounds how much of a refusal is kept to read. Google's error
// bodies are a few kilobytes.
const maxErrorBody = 64 << 10

// refusal is a RoundTripper that keeps the body of a 4xx or 5xx answer for
// the adapter, and passes it on to httpx unchanged. The adapter reads more
// from Google's error than httpx.Classify does: the status name, the
// ErrorInfo reason, and the RetryInfo delay a 429 carries in its body
// rather than in Retry-After.
type refusal struct {
	next http.RoundTripper
	body []byte
}

func (r *refusal) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	if err != nil || resp.StatusCode < 400 {
		return resp, err
	}
	// A read that fails part way keeps what it read; httpx then reads on
	// from the same body and meets the same failure.
	head, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	r.body = head
	resp.Body = replayed{Reader: io.MultiReader(bytes.NewReader(head), resp.Body), Closer: resp.Body}
	return resp, nil
}

// replayed is a body whose start was read ahead.
type replayed struct {
	io.Reader
	io.Closer
}

// capturingClient is the configured client sending through a new refusal.
// The copy shares the configured transport, and so its connections; the
// client's RoundTrip runs on the calling goroutine, so the refusal is read
// safely once the call returns.
func (a *Adapter) capturingClient() (*http.Client, *refusal) {
	next := a.client.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	r := &refusal{next: next}
	c := *a.client
	c.Transport = r
	return &c, r
}

// googleError is Google's error body, a google.rpc.Status as JSON.
type googleError struct {
	Error struct {
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []struct {
			Type       string `json:"@type"`
			Reason     string `json:"reason"`
			RetryDelay string `json:"retryDelay"`
		} `json:"details"`
	} `json:"error"`
}

// parseGoogleError reads Google's error body, which the streaming endpoints
// wrap in a list. It reports false for anything else, such as a proxy's
// HTML page.
func parseGoogleError(body []byte) (googleError, bool) {
	var g googleError
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var list []googleError
		if json.Unmarshal(trimmed, &list) != nil || len(list) == 0 {
			return googleError{}, false
		}
		g = list[0]
	} else if json.Unmarshal(trimmed, &g) != nil {
		return googleError{}, false
	}
	return g, g.Error.Status != "" || g.Error.Message != ""
}

// refine corrects httpx's classification of a refusal with what Google's
// error says (§3.4's errors for Gemini):
//   - 429 RESOURCE_EXHAUSTED is rate_limited, with RetryInfo's delay when
//     no Retry-After came;
//   - 403 PERMISSION_DENIED, 401 UNAUTHENTICATED, and a 400 about the key
//     (ErrorInfo reason API_KEY_INVALID and the like) are auth;
//   - 503 UNAVAILABLE is overloaded, 504 DEADLINE_EXCEEDED a timeout;
//   - 400 INVALID_ARGUMENT about the input's tokens, or a payload over
//     Google's size limit (400 or 413), is context_overflow.
//
// The code is Google's status name, and the message never holds the key.
func (a *Adapter) refine(e *llm.Error, body []byte) {
	msg := e.Message
	var status, reason string
	if g, ok := parseGoogleError(body); ok {
		status = g.Error.Status
		if status != "" {
			e.Code = status
		}
		if g.Error.Message != "" {
			msg = g.Error.Message
		}
		for _, d := range g.Error.Details {
			if reason == "" {
				reason = d.Reason
			}
			if e.RetryAfter == 0 && d.RetryDelay != "" {
				if wait, err := time.ParseDuration(d.RetryDelay); err == nil && wait > 0 {
					e.RetryAfter = wait
				}
			}
		}
	}
	if a.key != "" {
		msg = strings.ReplaceAll(msg, a.key, "[key]")
	}
	e.Message = llm.Clip(msg)
	lower := strings.ToLower(msg)
	badRequest := status == "INVALID_ARGUMENT" || e.Status == http.StatusBadRequest
	switch {
	case status == "RESOURCE_EXHAUSTED" || e.Status == http.StatusTooManyRequests:
		e.Kind = llm.ErrRateLimited
	case strings.HasPrefix(reason, "API_KEY_"), status == "UNAUTHENTICATED", status == "PERMISSION_DENIED",
		badRequest && strings.Contains(lower, "api key"):
		e.Kind = llm.ErrAuth
	case status == "UNAVAILABLE":
		e.Kind = llm.ErrOverloaded
	case status == "DEADLINE_EXCEEDED":
		e.Kind = llm.ErrTimeout
	case badRequest && tokenLimit(lower), e.Status == http.StatusRequestEntityTooLarge,
		badRequest && strings.Contains(lower, "payload size exceeds"):
		// A request too large in bytes (files, long results) is met as one
		// too long in tokens: a shorter history is what mends it.
		e.Kind = llm.ErrContextOverflow
	}
}

// tokenLimit reports whether a refusal says the input is longer than the
// model takes, as in "The input token count (1196265) exceeds the maximum
// number of tokens allowed (1048575)." A complaint about the output cap or
// the thinking budget is not one: a shorter history would not help it.
func tokenLimit(lower string) bool {
	for _, s := range []string{"maxoutputtokens", "max_output_tokens", "output token", "thinking"} {
		if strings.Contains(lower, s) {
			return false
		}
	}
	if !strings.Contains(lower, "token") {
		return false
	}
	for _, s := range []string{"exceed", "too long", "too many", "limit"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}
