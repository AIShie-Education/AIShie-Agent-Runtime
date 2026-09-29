package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/api"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/webauth"
)

// apiTakesCoresAssertion is R4 against the real Core: the runtime's API,
// on a listener of its own with its store in PostgreSQL, takes the
// assertion Core mints for a person, checked against the key Core
// publishes. Yuki asks Core with her session as a bearer token, and root
// signs in again with its password and asks with that session's cookie;
// GET /me names each, root as an administrator. An assertion Core made for
// another runtime, one past its time, and one tampered with are refused,
// and /status is not there. No answer and no log holds an assertion.
func apiTakesCoresAssertion(t *testing.T, w *world) {
	audience, other := os.Getenv("E2E_RUNTIME_AUDIENCE"), os.Getenv("E2E_OTHER_AUDIENCE")
	if audience == "" || other == "" {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); ci {
			t.Fatal("E2E_RUNTIME_AUDIENCE and E2E_OTHER_AUDIENCE are not set, and CI is: start Core with scripts/ci-core.sh")
		}
		t.Skip("E2E_RUNTIME_AUDIENCE is not set: this Core makes no assertion for a runtime (scripts/ci-core.sh sets it)")
	}
	st, _ := runtimeStore(t)
	rt := w.startAPI(t, st, audience, nil)

	// What a front end reads first.
	var info api.Info
	rt.getJSON(t, "/runtime/api/v1/info", "", http.StatusOK, &info)
	if info.API != "aishie-runtime" || info.APIVersion != 1 || info.Audience != audience || info.Issuer != w.api.base {
		t.Errorf("GET /info: %+v", info)
	}

	// Yuki, with her session as a bearer token; one agent of hers hosted.
	yukis := w.assertion(t, w.yuki.token, "", audience)
	tok := store.Secret{ID: "sec_e2e_" + w.yuki.id[:8], TenantID: "ten_" + w.yuki.id, Kind: store.SecretCoreToken, KEKID: "local:v1",
		WrappedDEK: []byte{1}, Nonce: []byte{2}, Ciphertext: []byte{3}}
	if _, err := st.CreateHostedAgent(t.Context(), store.HostedAgent{ID: "agt_e2e_" + w.own.id[:8], CoreActorID: w.own.id,
		OwnerActorID: w.yuki.id, OwnerVerified: true, TenantID: tok.TenantID, TokenSecretID: tok.ID}, tok); err != nil {
		t.Fatal(err)
	}
	var me api.Me
	rt.getJSON(t, "/runtime/api/v1/me", yukis, http.StatusOK, &me)
	if me != (api.Me{ActorID: w.yuki.id, DisplayName: w.yuki.name, IsAdmin: false, HostedAgents: 1}) {
		t.Errorf("GET /me as Yuki: %+v", me)
	}
	if p, err := st.Person(t.Context(), w.yuki.id); err != nil || p.DisplayName != w.yuki.name {
		t.Errorf("Yuki is not recorded as a person who used the API: %+v %v", p, err)
	}

	// Root, signed in again with its password, asking with the session's
	// cookie, as a browser does.
	if password := os.Getenv("E2E_PASSWORD"); password != "" {
		roots := w.assertion(t, "", w.signIn(t, "root@e2e.test", password), audience)
		rt.getJSON(t, "/runtime/api/v1/me", roots, http.StatusOK, &me)
		rootID := result[struct {
			ID string `json:"id"`
		}](t, w.api, w.root, "GET", "/v1/me", nil).ID
		if me.ActorID != rootID || !me.IsAdmin {
			t.Errorf("GET /me as root: %+v", me)
		}
	}

	// Refused: made for another runtime, past its time, tampered with.
	others := w.assertion(t, w.yuki.token, "", other)
	rt.wantRefused(t, others, webauth.ReasonInvalid)
	rt.offset.Store(int64(16 * time.Minute)) // longer than Core makes any
	rt.wantRefused(t, yukis, webauth.ReasonExpired)
	rt.offset.Store(0)
	// A bit of the signature itself is flipped, and the signature written
	// again: a bit flipped in its base64url text would, about one time in
	// eleven, leave a character that is not base64url, which is
	// assertion_malformed.
	parts := strings.Split(yukis, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("the assertion's signature is not base64url: %v", err)
	}
	sig[len(sig)/2] ^= 1
	rt.wantRefused(t, parts[0]+"."+parts[1]+"."+base64.RawURLEncoding.EncodeToString(sig), webauth.ReasonInvalid)
	if code, _ := rt.get(t, "/status", ""); code != http.StatusNotFound {
		t.Errorf("GET /status on the API: %d", code)
	}

	for what, text := range map[string]string{"the API's answers": rt.answers.String(), "the API's log": rt.raw.String()} {
		for _, a := range []string{yukis, others} {
			if strings.Contains(text, a) || strings.Contains(text, parts[2]) {
				t.Errorf("%s hold an assertion", what)
			}
		}
	}
}

// assertion is an assertion Core makes for audience, of the person whose
// token or session cookie is given.
func (w *world) assertion(t *testing.T, token, cookie, audience string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"audience": audience})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.api.base+"/v1/auth/assertion", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := w.api.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Assertion string    `json:"assertion"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil || out.Assertion == "" {
		t.Fatalf("POST /v1/auth/assertion for %s: HTTP %d %s", audience, resp.StatusCode, redact.String(string(raw)))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Core's assertion is cacheable: %q", resp.Header.Get("Cache-Control"))
	}
	w.addSecret("an assertion Core made for "+audience, out.Assertion)
	return out.Assertion
}

// signIn is the session cookie Core gives email for password, as a Cookie
// header sends it back.
func (w *world) signIn(t *testing.T, email, password string) string {
	t.Helper()
	session := w.api.session(t, "/v1/auth/login", map[string]string{"email": email, "password": password})
	w.addSecret("root's session in a cookie", session)
	return "ais_session=" + session
}

// apiInstance is the runtime's API, served as run serves it.
type apiInstance struct {
	base string
	hc   *http.Client
	// offset moves the API's clock on, for an assertion past its time.
	offset atomic.Int64
	// answers are every body the API answered; raw is its log before
	// redaction.
	answers *logBuffer
	raw     *logBuffer
}

// startAPI serves the API on 127.0.0.1:0 as run does with API_ADDR: its
// store st, its assertions Core's for audience, checked against the keys
// Core publishes, and its Core the world's; its log, redacted and not,
// among the world's logs. edit sets what else it is given.
func (w *world) startAPI(t *testing.T, st store.Store, audience string, edit func(*api.Options)) *apiInstance {
	t.Helper()
	rt := &apiInstance{answers: &logBuffer{}, raw: &logBuffer{}}
	redacted := &logBuffer{}
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	logger := slog.New(teeHandler{redact.NewHandler(slog.NewJSONHandler(redacted, opts), nil), slog.NewJSONHandler(rt.raw, opts)})
	w.addLog(t.Name()+" (the API)", redacted.String)
	w.addLog(t.Name()+" (the API, before redaction)", rt.raw.String)
	w.addLog(t.Name()+" (the API's answers)", rt.answers.String)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(tr.CloseIdleConnections)
	o := api.Options{
		Addr: "127.0.0.1:0",
		Verifier: &webauth.Verifier{Keys: webauth.NewRemoteKeys(w.api.base, &http.Client{Transport: tr}), Issuer: w.api.base, Audience: audience,
			Now: func() time.Time { return time.Now().Add(time.Duration(rt.offset.Load())) }},
		Store: st, CoreBaseURL: w.api.base, CoreHTTP: &http.Client{Transport: tr}, Registerer: prometheus.NewRegistry(), Log: logger,
	}
	if edit != nil {
		edit(&o)
	}
	s := api.New(o)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the API: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the API did not stop within 10 s")
		}
	})
	rt.base, rt.hc = "http://"+s.Addr(), &http.Client{Timeout: requestTimeout}
	return rt
}

// get asks the API for path with the assertion, and returns the status and
// the body, which it keeps with the API's answers.
func (rt *apiInstance) get(t *testing.T, path, assertion string) (int, []byte) {
	t.Helper()
	code, _, raw := rt.do(t, http.MethodGet, path, assertion, "")
	return code, raw
}

// do sends a request to the API, with the assertion, a JSON body when body
// is not "", and the headers given; it returns the status, the headers and
// the body, which it keeps with the API's answers.
func (rt *apiInstance) do(t *testing.T, method, path, assertion, body string, headers ...string) (int, http.Header, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, rt.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if assertion != "" {
		req.Header.Set("Authorization", "Bearer "+assertion)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := rt.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_, _ = rt.answers.Write(append(raw, '\n'))
	return resp.StatusCode, resp.Header, raw
}

func (rt *apiInstance) getJSON(t *testing.T, path, assertion string, want int, v any) {
	t.Helper()
	code, raw := rt.get(t, path, assertion)
	if code != want || json.Unmarshal(raw, v) != nil {
		t.Fatalf("GET %s: %d %s", path, code, redact.String(string(raw)))
	}
}

// wantRefused holds the API to a 401 of reason for GET /me with the
// assertion.
func (rt *apiInstance) wantRefused(t *testing.T, assertion, reason string) {
	t.Helper()
	code, raw := rt.get(t, "/runtime/api/v1/me", assertion)
	var e struct {
		Error struct {
			Code    string            `json:"code"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
	if code != http.StatusUnauthorized || json.Unmarshal(raw, &e) != nil || e.Error.Code != "unauthenticated" || e.Error.Details["reason"] != reason {
		t.Errorf("GET /me: %d %s; want 401 %s", code, redact.String(string(raw)), reason)
	}
}
