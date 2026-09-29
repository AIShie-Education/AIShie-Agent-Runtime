// Package httpx is the transport every adapter shares: one POST of JSON,
// its answer read whole or, where the adapter streams, as server-sent
// events (stream.go), and a refusal classified into an *llm.Error. It keeps
// keys out of every error it builds: messages come from response bodies
// only, never from the request.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// MaxResponseBytes bounds what a provider's answer may be.
const MaxResponseBytes = 32 << 20

// Response is a provider's answer, read whole.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// PostJSON sends body to url with headers and reads the answer. A 2xx comes
// back as a Response; anything else is an *llm.Error classified by
// Classify. A network error or timeout is an *llm.Error too.
func PostJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte) (*Response, error) {
	return Do(ctx, client, http.MethodPost, url, headers, body, nil)
}

// Do is PostJSON for any method, with sign called on the request just
// before it is sent (Bedrock's SigV4), when not nil.
func Do(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body []byte, sign func(*http.Request) error) (*Response, error) {
	resp, err := send(ctx, client, method, url, "application/json", headers, body, sign)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return readWhole(ctx, resp, headers)
}

// send sends one request, asking for accept, and returns the provider's
// answer unread, whatever its status.
func send(ctx context.Context, client *http.Client, method, url, accept string, headers map[string]string, body []byte, sign func(*http.Request) error) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: llm.Clip(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if sign != nil {
		if err := sign(req); err != nil {
			return nil, &llm.Error{Kind: llm.ErrAuth, Message: llm.Clip("signing the request: " + err.Error())}
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, networkError(ctx, err)
	}
	return resp, nil
}

// readWhole reads an answer whole: a 2xx is a Response, anything else an
// *llm.Error classified by Classify, with the request's credentials taken
// out of what it quotes.
func readWhole(ctx context.Context, resp *http.Response, headers map[string]string) (*Response, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, networkError(ctx, err)
	}
	if len(data) > MaxResponseBytes {
		return nil, &llm.Error{Kind: llm.ErrServer, Status: resp.StatusCode, Message: "the response is larger than the runtime reads"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, Classify(resp.StatusCode, resp.Header, withoutCredentials(data, headers))
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

func networkError(ctx context.Context, err error) *llm.Error {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return &llm.Error{Kind: llm.ErrTimeout, Message: "the provider did not answer in time"}
	case ctx.Err() != nil:
		return &llm.Error{Kind: llm.ErrTimeout, Message: "the call was cancelled"}
	}
	// The URL is in err's text; it holds no key (keys go in headers), but
	// Gemini's key could be in a query string if someone put it there, so
	// only the error's kind is kept.
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			err = inner
		}
	}
	return &llm.Error{Kind: llm.ErrNetwork, Message: llm.Clip(err.Error())}
}

// credentialHeaders carry keys. A provider or a proxy that echoes the
// request back in its refusal must not put one into an error.
var credentialHeaders = []string{"authorization", "api-key", "x-api-key", "x-goog-api-key", "proxy-authorization"}

// withoutCredentials is body with the values of the request's credential
// headers taken out.
func withoutCredentials(body []byte, headers map[string]string) []byte {
	for name, value := range headers {
		if !slices.Contains(credentialHeaders, strings.ToLower(name)) {
			continue
		}
		secret := value
		if _, token, ok := strings.Cut(value, " "); ok {
			secret = token // "Bearer <token>": the scheme is no secret
		}
		if len(secret) >= 8 {
			body = bytes.ReplaceAll(body, []byte(secret), []byte("[redacted]"))
		}
	}
	return body
}

// Classify turns a provider's refusal into an *llm.Error: the kind from the
// status and what the body says, the code and message from the body.
func Classify(status int, header http.Header, body []byte) *llm.Error {
	code, msg := errorFields(body)
	e := &llm.Error{Status: status, Code: code, Message: llm.Clip(msg), RetryAfter: RetryAfter(header)}
	if e.RetryAfter == 0 {
		e.RetryAfter = retryInfo(body)
	}
	lower := strings.ToLower(code + " " + msg)
	switch {
	case status == http.StatusTooManyRequests:
		e.Kind = llm.ErrRateLimited
		// OpenAI says insufficient_quota with a 429: no retry will help.
		if strings.Contains(lower, "insufficient_quota") {
			e.Kind = llm.ErrAuth
		}
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusPaymentRequired:
		e.Kind = llm.ErrAuth
	case status == 529 || status == http.StatusServiceUnavailable:
		e.Kind = llm.ErrOverloaded
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		e.Kind = llm.ErrTimeout
	case status == http.StatusRequestEntityTooLarge:
		// The request was too big for the provider: a shorter history is
		// what fixes it, as for a context window exceeded.
		e.Kind = llm.ErrContextOverflow
	case status >= 500:
		e.Kind = llm.ErrServer
	case contextOverflow(lower):
		e.Kind = llm.ErrContextOverflow
	case strings.Contains(lower, "content_filter") || strings.Contains(lower, "content management policy") ||
		strings.Contains(lower, "responsibleaipolicyviolation") || strings.Contains(lower, "safety"):
		e.Kind = llm.ErrContentFilter
	default:
		e.Kind = llm.ErrBadRequest
	}
	return e
}

// contextOverflow reports whether an error text says the input was too
// long for the model, in the words the providers use.
func contextOverflow(lower string) bool {
	for _, s := range []string{
		"context_length_exceeded", "maximum context length", "context window",
		"prompt is too long", "input is too long", "too many tokens", "exceeds the maximum number of tokens",
		"model_context_window_exceeded", "input length", "reduce the length", "exceed context limit",
	} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// errorFields finds a code and a message in the error bodies providers
// send: {"error":{"code","type","message","status"}} (OpenAI, Gemini,
// most compatible servers), {"type":"error","error":{"type","message"}}
// (Anthropic), {"message","__type"} (AWS), {"error":"text"} (Ollama).
func errorFields(body []byte) (code, message string) {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Type    string          `json:"__type"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(body, &v) != nil {
		return "", strings.TrimSpace(string(body))
	}
	if len(v.Error) > 0 {
		var s string
		if json.Unmarshal(v.Error, &s) == nil {
			return "", s
		}
		// status is a string for Gemini (RESOURCE_EXHAUSTED) and a number
		// for Azure OpenAI (400), so it and code are read either way.
		var e struct {
			Code    json.RawMessage `json:"code"`
			Type    string          `json:"type"`
			Status  json.RawMessage `json:"status"`
			Message string          `json:"message"`
		}
		if json.Unmarshal(v.Error, &e) == nil {
			code = rawString(e.Code)
			// Gemini's code is the HTTP status again, and its status the
			// name that says something (RESOURCE_EXHAUSTED).
			if status := rawString(e.Status); status != "" && (code == "" || isNumber(e.Code)) && !isNumber(e.Status) {
				code = status
			}
			if code == "" {
				code = e.Type
			}
			if code == "" {
				code = rawString(e.Status)
			}
			return code, e.Message
		}
	}
	code = rawString(v.Code)
	if code == "" {
		code = v.Type
	}
	return code, v.Message
}

func isNumber(raw json.RawMessage) bool {
	var n json.Number
	return len(raw) > 0 && raw[0] != '"' && json.Unmarshal(raw, &n) == nil
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// retryInfo reads the wait Google puts in its error body when it sends no
// Retry-After header: a google.rpc.RetryInfo detail whose retryDelay is a
// duration like "36s".
func retryInfo(body []byte) time.Duration {
	var v struct {
		Error struct {
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0
	}
	for _, d := range v.Error.Details {
		if strings.HasSuffix(d.Type, "RetryInfo") && d.RetryDelay != "" {
			if wait, err := time.ParseDuration(d.RetryDelay); err == nil && wait > 0 {
				return wait
			}
		}
	}
	return 0
}

// RetryAfter reads Retry-After (seconds or an HTTP date), and the
// retry-after-ms some providers send; 0 when there is none.
func RetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	if ms := h.Get("Retry-After-Ms"); ms != "" {
		if n, err := strconv.ParseFloat(ms, 64); err == nil && n >= 0 {
			return time.Duration(n * float64(time.Millisecond))
		}
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
		return time.Duration(n * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Join builds an endpoint URL from a base and a path, with exactly one
// slash between them.
func Join(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// DecodeError is a 2xx body that did not decode: the provider answered
// something the adapter cannot read.
func DecodeError(err error) *llm.Error {
	return &llm.Error{Kind: llm.ErrServer, Message: llm.Clip(fmt.Sprintf("the response does not decode: %v", err))}
}
