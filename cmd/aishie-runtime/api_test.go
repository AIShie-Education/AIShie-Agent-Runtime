package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/webauth"
)

// TestRunServesTheAPI: with API_ADDR, run serves the JSON API on a
// listener of its own beside HTTP_ADDR's: GET /info answers anyone, GET /me
// takes an assertion of Core's (its key pinned here), and neither listener
// serves the other's routes. /status refuses what a proxy forwarded, and no
// log line holds the assertion.
func TestRunServesTheAPI(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dbURL := scratchDatabase(t)
	if err := pgstore.Migrate(dbURL, pgstore.Up); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeKey(t, dir, "v1")
	const coreURL, audience = "https://lms.example.edu", "https://lms.example.edu/runtime"
	const person = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b"
	cmd, out, exited := child(t, "run", "CONFIG="+t.TempDir(), "DATABASE_URL="+dbURL, "KMS_KEY_ID=local:"+filepath.Join(dir, "v1"),
		"HTTP_ADDR=127.0.0.1:0", "API_ADDR=127.0.0.1:0", "API_AUDIENCE="+audience, "CORE_BASE_URL="+coreURL,
		"CORE_ASSERTION_KEY="+base64.RawURLEncoding.EncodeToString(pub), "LOG_FORMAT=json", "SHUTDOWN_GRACE=1s", "WORKER_ID=child")
	started := out.wait(t, `"msg":"aishie-runtime started"`)
	var addrs struct {
		Addr string `json:"addr"`
		API  string `json:"api"`
	}
	if err := json.Unmarshal([]byte(started), &addrs); err != nil || addrs.API == "" || addrs.API == addrs.Addr {
		t.Fatalf("the started line: %s", started)
	}

	enc := base64.RawURLEncoding
	part := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return enc.EncodeToString(b)
	}
	now := time.Now()
	input := part(map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": webauth.Thumbprint(pub)}) + "." + part(map[string]any{
		"iss": coreURL, "aud": audience, "sub": person, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"jti": "j1", "kind": "human", "name": "Yuki", "sid": "0192f3c1-0000-7000-8000-000000000001", "platform_role": "admin"})
	assertion := input + "." + enc.EncodeToString(ed25519.Sign(key, []byte(input)))

	get := func(addr, path string, headers ...string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, c := range []struct {
		addr, path string
		headers    []string
		code       int
		body       string
	}{
		{addrs.API, "/runtime/api/v1/info", nil, 200, `"audience":"` + audience + `"`},
		{addrs.API, "/runtime/api/v1/me", []string{"Authorization", "Bearer " + assertion}, 200,
			`{"actor_id":"` + person + `","display_name":"Yuki","is_admin":true,"hosted_agents":0}`},
		{addrs.API, "/runtime/api/v1/me", nil, 401, `"reason":"assertion_missing"`},
		{addrs.API, "/status", nil, 404, `"reason":"no_route"`},
		{addrs.API, "/metrics", nil, 404, `"reason":"no_route"`},
		{addrs.Addr, "/runtime/api/v1/info", nil, 404, ""},
		{addrs.Addr, "/status", nil, 200, `"worker": "child"`},
		{addrs.Addr, "/status", []string{"X-Forwarded-For", "192.0.2.7"}, 403, "never through a proxy"},
		{addrs.Addr, "/status", []string{"Forwarded", "for=192.0.2.7"}, 403, "never through a proxy"},
		{addrs.Addr, "/metrics", nil, 200, `aishie_api_requests_total{code="ok",route="GET /runtime/api/v1/me"} 1`},
	} {
		if code, body := get(c.addr, c.path, c.headers...); code != c.code || !strings.Contains(body, c.body) {
			t.Errorf("GET %s%s: %d %s", c.addr, c.path, code, body)
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, exited, 15*time.Second, out); err != nil {
		t.Errorf("the runtime exited with %v:\n%s", err, out.text())
	}
	out.wait(t, `"msg":"api request"`)
	if strings.Contains(out.text(), assertion) || strings.Contains(out.text(), strings.Split(assertion, ".")[2]) {
		t.Error("a log line holds the assertion")
	}
}

// TestRunRefusesAnAPIWithoutItsSettings: API_ADDR without a database, the
// key that seals secrets, an audience or Core is refused, saying what to
// set.
func TestRunRefusesAnAPIWithoutItsSettings(t *testing.T) {
	code, _, errs := runCmd(t, env("CONFIG", t.TempDir(), "API_ADDR", "127.0.0.1:0"), "run")
	for _, want := range []string{"API_AUDIENCE: required with API_ADDR", "CORE_BASE_URL: required with API_ADDR",
		"DATABASE_URL: required with API_ADDR", "KMS_KEY_ID: required with API_ADDR"} {
		if code != exitFailure || !strings.Contains(errs, want) {
			t.Errorf("run with API_ADDR alone: %d, does not say %q:\n%s", code, want, errs)
		}
	}
}
