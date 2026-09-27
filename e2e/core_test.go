package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/redact"
)

// coreAPI is Core's REST API as the tests' people use it: plain requests
// with their own tokens, as Core's scripts/e2e.sh makes them with curl. It
// is written here, apart from the runtime's own client, so that a fault in
// the runtime cannot hide in how the tests build their worlds.
type coreAPI struct {
	base string
	hc   *http.Client
	// run begins every idempotency key the people use, so that no two
	// runs against one Core share a key.
	run  string
	keys atomic.Int64
}

// requestTimeout bounds one request to Core.
const requestTimeout = 30 * time.Second

// liveCore is the Core under test, from E2E_CORE_URL and E2E_ROOT_TOKEN,
// and root's token. Without them the tests skip, or fail when CI is true:
// CI's end to end must never pass by testing nothing.
func liveCore(t *testing.T) (*coreAPI, string) {
	t.Helper()
	base, root := strings.TrimRight(os.Getenv("E2E_CORE_URL"), "/"), os.Getenv("E2E_ROOT_TOKEN")
	if base == "" || root == "" {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); ci {
			t.Fatal("E2E_CORE_URL and E2E_ROOT_TOKEN are not set, and CI is: run the end to end as scripts/e2e.sh (make e2e) does")
		}
		t.Skip("E2E_CORE_URL and E2E_ROOT_TOKEN are not set: no Core to test against (scripts/e2e.sh starts one)")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(tr.CloseIdleConnections)
	c := &coreAPI{base: base, hc: &http.Client{Transport: tr}, run: randomHex(4)}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatalf("Core at %s cannot be reached: %v", base, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Core at %s is not healthy: GET /healthz is HTTP %d", base, resp.StatusCode)
	}
	return c, root
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// envelope is what Core's REST API answers, whatever the HTTP status: the
// fields of the envelope (§2.1).
type envelope struct {
	Status      string          `json:"status"`
	ActionID    string          `json:"action_id"`
	ReviewState string          `json:"review_state"`
	Replayed    bool            `json:"replayed"`
	Result      json.RawMessage `json:"result"`
	Error       *struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// reply is Core's answer to one request.
type reply struct {
	HTTP int
	envelope
	Body []byte
}

// String is the reply for a failure message: its status and its body, with
// anything that looks like a token or a key redacted, since some answers
// carry one.
func (r reply) String() string {
	return fmt.Sprintf("HTTP %d: %s", r.HTTP, redact.String(string(r.Body)))
}

// send makes one request as token. A POST carries key as its
// Idempotency-Key, a fresh one when key is empty, and body as JSON. A 429
// is waited out as its Retry-After says, while ctx lasts.
func (c *coreAPI) send(ctx context.Context, token, method, path string, body any, key string) (reply, error) {
	var payload []byte
	if method == http.MethodPost {
		if body == nil {
			body = map[string]any{}
		}
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return reply{}, err
		}
		if key == "" {
			key = fmt.Sprintf("e2e-%s-%d", c.run, c.keys.Add(1))
		}
	}
	for {
		r, retryAfter, err := c.once(ctx, token, method, path, payload, key)
		if err != nil || r.HTTP != http.StatusTooManyRequests {
			return r, err
		}
		t := time.NewTimer(retryAfter)
		select {
		case <-ctx.Done():
			t.Stop()
			return r, fmt.Errorf("%s %s: still rate limited: %w", method, path, ctx.Err())
		case <-t.C:
		}
	}
}

func (c *coreAPI) once(ctx context.Context, token, method, path string, payload []byte, key string) (reply, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return reply{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return reply{}, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return reply{}, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	r := reply{HTTP: resp.StatusCode, Body: raw}
	// A 401 is text, and a 429 has only an error: what does not parse
	// leaves the envelope empty.
	_ = json.Unmarshal(raw, &r.envelope)
	wait := time.Second
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		wait = time.Duration(s) * time.Second
	}
	return r, wait, nil
}

// call is send for a test: it fails t unless Core answers with HTTP want.
func (c *coreAPI) call(t testing.TB, want int, token, method, path string, body any) reply {
	t.Helper()
	r, err := c.send(context.Background(), token, method, path, body, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.HTTP != want {
		t.Fatalf("%s %s: %s; want HTTP %d", method, path, r, want)
	}
	return r
}

// result is the result of a request that must be executed, decoded into a
// T.
func result[T any](t testing.TB, c *coreAPI, token, method, path string, body any) T {
	t.Helper()
	r := c.call(t, http.StatusOK, token, method, path, body)
	if r.Status != "executed" {
		t.Fatalf("%s %s: %s; want executed", method, path, r)
	}
	return decode[T](t, r)
}

// decode is r's result as a T.
func decode[T any](t testing.TB, r reply) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(r.Result, &out); err != nil {
		t.Fatalf("the result is not what was expected: %v; %s", err, r)
	}
	return out
}

// eventually checks cond every tick until it holds, and fails t if it
// has not within d. Nothing waits a fixed time: every wait in these tests
// is for a condition, with a deadline.
func eventually(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()
	if !within(d, cond) {
		t.Fatalf("%s did not happen within %s", what, d)
	}
}

// within reports whether cond held within d, checking it every tick.
func within(d time.Duration, cond func() bool) bool {
	const tick = 150 * time.Millisecond
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(tick)
	}
}

// errNoResult is a result without the id it must have.
var errNoResult = errors.New("the result holds no id")
