package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultURL is the loopback runner's listener.
const DefaultURL = "http://127.0.0.1:3001"

// Client calls one TypeScript runner. It is safe for concurrent use.
type Client struct {
	baseURL   string
	http      *http.Client
	targetErr error
}

// validateRunnerTarget keeps credentials off arbitrary hosts. RUNNER_URL may
// select another runner process, but it must still be a loopback HTTP(S) base
// URL with no userinfo, query, fragment or path.
func validateRunnerTarget(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return errors.New("runner: URL must be a loopback http(s) base URL without credentials, path, query or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return errors.New("runner: URL host must be a loopback IP address")
	}
	return nil
}

// NewClient makes a runner client. A blank baseURL uses DefaultURL. Redirects
// are never followed: a redirect must not forward Core tokens or model keys.
func NewClient(baseURL string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultURL
	}
	if hc == nil {
		hc = &http.Client{}
	}
	baseURL = strings.TrimRight(baseURL, "/")
	cp := *hc
	cp.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: baseURL, http: &cp, targetErr: validateRunnerTarget(baseURL)}
}

// Healthy checks GET /healthz and its {"status":"ok"} answer.
func (c *Client) Healthy(ctx context.Context) error {
	if c.targetErr != nil {
		return c.targetErr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil) // #nosec G704 -- target is a validated loopback IP; redirects are disabled.
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req) // #nosec G704 -- NewClient validates an IP-loopback URL and forbids redirects.
	if err != nil {
		return fmt.Errorf("runner: health: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorResponseBody))
		return fmt.Errorf("runner: health: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxErrorResponseBody))
	if err := dec.Decode(&body); err != nil || body.Status != "ok" {
		return errors.New("runner: health: invalid response")
	}
	return nil
}

// Run streams POST /run. onEvent is called synchronously for each valid
// event, including the terminal one. A done event is returned; an error event
// becomes *Error. Cancellation closes the HTTP request, which asks the runner
// to abort its model call and MCP connection.
func (c *Client) Run(ctx context.Context, run RunRequest, onEvent func(Event) error) (*Done, error) {
	if c.targetErr != nil {
		return nil, c.targetErr
	}
	body, err := json.Marshal(run)
	if err != nil {
		return nil, fmt.Errorf("runner: encode run: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/run", bytes.NewReader(body)) // #nosec G704 -- target is a validated loopback IP; redirects are disabled.
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := c.http.Do(req) // #nosec G704 -- NewClient validates an IP-loopback URL and forbids redirects.
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Kind: "provider_failed", Retryable: true, Message: "the runner could not be reached"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorResponseBody))
		return nil, &Error{Kind: "provider_failed", Retryable: resp.StatusCode >= 500 || resp.StatusCode == 429,
			HTTPStatus: resp.StatusCode, Message: "the runner refused the request"}
	}
	if typ, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || typ != "application/x-ndjson" {
		return nil, errors.New("runner: /run returned a non-NDJSON response")
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxNDJSONEventBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, errors.New("runner: invalid NDJSON event")
		}
		if onEvent != nil {
			if err := onEvent(ev); err != nil {
				return nil, err
			}
		}
		switch ev.Type {
		case "step", "tool_start", "tool_end", "text_delta":
		case "done":
			done := ev.Done()
			return &done, nil
		case "error":
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			e := &Error{Kind: ev.Kind, Retryable: ev.Retryable, Message: ev.Message}
			if ev.HTTPStatus != nil {
				e.HTTPStatus = *ev.HTTPStatus
			}
			return nil, e
		default:
			return nil, fmt.Errorf("runner: unknown event type %q", ev.Type)
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("runner: read NDJSON: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, errors.New("runner: the stream ended without a terminal event")
}

// Probe classifies one model key through POST /probe. A runner or network
// failure is ResultUnreachable, not a Go error; a malformed response is a Go
// error. No provider error text is accepted or returned.
func (c *Client) Probe(ctx context.Context, model Model) (ProbeResult, error) {
	if c.targetErr != nil {
		return ProbeResult{}, c.targetErr
	}
	started := time.Now()
	unreachable := func() ProbeResult {
		return ProbeResult{Result: ResultUnreachable, LatencyMS: time.Since(started).Milliseconds()}
	}
	// A routing refusal must not be mistaken for a bad key or missing model.
	model.OpenRouter = nil
	body, err := json.Marshal(ProbeRequest{Model: model, TimeoutMS: ProbeTimeout.Milliseconds()})
	if err != nil {
		return ProbeResult{}, fmt.Errorf("runner: encode probe: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/probe", bytes.NewReader(body)) // #nosec G704 -- target is a validated loopback IP; redirects are disabled.
	if err != nil {
		return ProbeResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req) // #nosec G704 -- NewClient validates an IP-loopback URL and forbids redirects.
	if err != nil {
		return unreachable(), nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorResponseBody))
		return unreachable(), nil
	}
	var out ProbeResult
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	if err := dec.Decode(&out); err != nil {
		return ProbeResult{}, errors.New("runner: invalid /probe response")
	}
	switch out.Result {
	case ResultOK, ResultKeyRefused, ResultModelNotFound, ResultKeyAccepted, ResultUnreachable:
	default:
		return ProbeResult{}, errors.New("runner: unknown /probe result")
	}
	return out, nil
}
