package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
)

// coreAPI is Core's REST API as the tests' people use it: plain requests
// with the sessions they signed in with (agents' with their API tokens), as
// Core's scripts/e2e.sh makes them with curl. It is written here, apart
// from the runtime's own client, so that a fault in the runtime cannot hide
// in how the tests build their worlds.
type coreAPI struct {
	base string
	hc   *http.Client
	// svc is the site's agent runtime's own credential in Core, which
	// the tests' runtimes host their agents with.
	svc string
	// run begins every idempotency key the people use, so that no two
	// runs against one Core share a key.
	run  string
	keys atomic.Int64
}

// requestTimeout bounds one request to Core.
const requestTimeout = 30 * time.Second

// liveCore is the Core under test, from E2E_CORE_URL and E2E_ROOT_TOKEN,
// and root's token: the session root signed in with (scripts/ci-core.sh),
// not an API token, since people hold none. Without them the tests skip, or
// fail when CI is true: CI's end to end must never pass by testing nothing.
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
	c.svc = runtimeCredential(t, c, root)
	return c, root
}

// The site's agent runtime's own credential in Core (agent_runtime, a site
// service's), issued once for the run by root, as the operator issues it at
// setup (aishie-core service issue agent_runtime): every runtime of every
// test hosts its agents with it, as every worker of a site shares one.
var (
	credentialMu sync.Mutex
	credential   string
)

// runtimeCredential is the run's agent_runtime credential, issued the
// first time (replacing any an earlier run left: the Core is a throwaway).
func runtimeCredential(t *testing.T, c *coreAPI, root string) string {
	t.Helper()
	credentialMu.Lock()
	defer credentialMu.Unlock()
	if credential == "" {
		credential = result[struct {
			Token string `json:"token"`
		}](t, c, root, "POST", "/v1/services/agent_runtime/credentials", map[string]any{"label": "runtime e2e " + c.run, "replace": true}).Token
		if !strings.HasPrefix(credential, "aissvc_") {
			t.Fatal("Core issued the agent runtime no credential (aissvc_…): is it older than AIShie-Core #52?")
		}
	}
	return credential
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
	return c.sendRevising(ctx, token, method, path, body, key, "")
}

// sendRevising is send for a write that proposes again revises, a
// proposal sent back for changes, which a POST names in its Revises
// header; none for "".
func (c *coreAPI) sendRevising(ctx context.Context, token, method, path string, body any, key, revises string) (reply, error) {
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
		r, retryAfter, err := c.once(ctx, token, method, path, payload, key, revises)
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

func (c *coreAPI) once(ctx context.Context, token, method, path string, payload []byte, key, revises string) (reply, time.Duration, error) {
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
		if revises != "" {
			req.Header.Set("Revises", revises)
		}
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

// newPerson is registrar registering a person (POST /v1/actors, with the
// fields more adds, such as a platform role), who then signs in with a
// password as people do: people hold no API tokens, only agents do. They
// are given an email of this run's, invited to choose a password
// (actor.invite), choose one as the front end's page for invitations does
// (POST /v1/auth/invite), and sign in with it (POST /v1/auth/login). It
// returns their actor's id and that session, which the tests send as a
// bearer token, as Core takes it.
func (c *coreAPI) newPerson(t testing.TB, registrar, name string, more map[string]any) (id, session string) {
	t.Helper()
	local := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.ToLower(name))
	email := fmt.Sprintf("%s-%s-%d@e2e.test", local, c.run, c.keys.Add(1))
	body := map[string]any{"kind": "human", "display_name": name, "email": email}
	maps.Copy(body, more)
	id = result[struct {
		ActorID string `json:"actor_id"`
	}](t, c, registrar, "POST", "/v1/actors", body).ActorID
	invite := result[struct {
		Token string `json:"token"`
	}](t, c, registrar, "POST", "/v1/actors/"+id+"/invite", nil).Token
	password := randomHex(16)
	c.session(t, "/v1/auth/invite", map[string]string{"token": invite, "password": password})
	return id, c.session(t, "/v1/auth/login", map[string]string{"email": email, "password": password})
}

// session posts body to path, where Core signs someone in (POST
// /v1/auth/login, or /v1/auth/invite), as the front end does, and returns
// the session Core sets as its cookie. A 429 is waited out as its
// Retry-After says. A session whose password must be changed first is no
// use to the tests, and fails t.
func (c *coreAPI) session(t testing.TB, path string, body map[string]string) string {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.hc.Do(req)
		if err != nil {
			cancel()
			t.Fatalf("POST %s: %v", path, err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = time.Duration(s) * time.Second
			}
			time.Sleep(wait)
			continue
		}
		var out struct {
			PasswordChangeRequired bool `json:"password_change_required"`
		}
		for _, ck := range resp.Cookies() {
			if ck.Name == "ais_session" && ck.Value != "" && json.Unmarshal(raw, &out) == nil && !out.PasswordChangeRequired {
				return ck.Value
			}
		}
		t.Fatalf("POST %s: HTTP %d, and no session to use: %s", path, resp.StatusCode, redact.String(string(raw)))
	}
	t.Fatalf("POST %s: refused as too many sign-ins twenty times", path)
	return ""
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
