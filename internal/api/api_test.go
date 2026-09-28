package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/webauth"
)

const (
	issuer   = "https://lms.example.edu"
	audience = "https://lms.example.edu/runtime"
	yuki     = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b"
	ken      = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1c"
	sid      = "0192f3c1-0000-7000-8000-000000000001"
)

var at = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

// minter mints assertions as Core does, and is the key set they check
// against.
type minter struct {
	key ed25519.PrivateKey
	kid string
}

func newCore(t *testing.T) minter {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return minter{key: key, kid: webauth.Thumbprint(pub)}
}

func (c minter) Key(_ context.Context, kid string) (ed25519.PublicKey, error) {
	if kid != c.kid {
		return nil, webauth.ErrUnknownKey
	}
	return c.key.Public().(ed25519.PublicKey), nil
}

func b64json(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// claims are what Core says of a person at the test's clock; edit changes
// them.
func claims(sub, role string, edit func(map[string]any)) map[string]any {
	c := map[string]any{"iss": issuer, "aud": audience, "sub": sub, "iat": at.Unix(), "nbf": at.Unix(),
		"exp": at.Add(5 * time.Minute).Unix(), "jti": "j-" + sub, "kind": "human", "name": "Yuki Tanaka",
		"email": "yuki@example.edu", "sid": sid}
	if role != "" {
		c["platform_role"] = role
	}
	if edit != nil {
		edit(c)
	}
	return c
}

func (c minter) assert(t *testing.T, cl map[string]any) string {
	t.Helper()
	input := b64json(t, map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": c.kid}) + "." + b64json(t, cl)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(c.key, []byte(input)))
}

// logBuffer is a log the test reads back.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// lines are the log's lines whose msg is msg, decoded.
func (l *logBuffer) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(l.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// failingStore fails the reads and writes the API makes, when told to.
type failingStore struct {
	*memstore.Store
	mu   sync.Mutex
	fail bool
}

var errDown = errors.New("store: the database is down")

func (s *failingStore) down() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fail
}

func (s *failingStore) HostedAgentsOwnedBy(ctx context.Context, owner string) ([]store.HostedAgent, error) {
	if s.down() {
		return nil, errDown
	}
	return s.Store.HostedAgentsOwnedBy(ctx, owner)
}

func (s *failingStore) PutPerson(ctx context.Context, p store.Person) error {
	if s.down() {
		return errDown
	}
	return s.Store.PutPerson(ctx, p)
}

func (s *failingStore) RecordAudit(ctx context.Context, e store.AuditEvent) (int64, error) {
	if s.down() {
		return 0, errDown
	}
	return s.Store.RecordAudit(ctx, e)
}

type fixture struct {
	core minter
	s    *Server
	st   *failingStore
	reg  *prometheus.Registry
	log  *logBuffer
	mu   sync.Mutex
	now  time.Time
}

func newFixture(t *testing.T, edit func(*Options)) *fixture {
	t.Helper()
	f := &fixture{core: newCore(t), st: &failingStore{Store: memstore.New()}, reg: prometheus.NewRegistry(), log: &logBuffer{}, now: at}
	o := Options{
		Addr:       "127.0.0.1:0",
		Verifier:   &webauth.Verifier{Keys: f.core, Issuer: issuer, Audience: audience, Now: f.clock},
		Store:      f.st,
		Registerer: f.reg,
		Log:        slog.New(slog.NewJSONHandler(f.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:        f.clock,
	}
	if edit != nil {
		edit(&o)
	}
	f.s = New(o)
	return f
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fixture) add(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// answer is a response, whole.
type answer struct {
	code   int
	header http.Header
	body   string
}

func (a answer) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(a.body), v); err != nil {
		t.Fatalf("%d %s: %v", a.code, a.body, err)
	}
}

// refusal is an answer's error.
type refusal struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func (a answer) refusal(t *testing.T) refusal {
	t.Helper()
	var e struct {
		Error refusal `json:"error"`
	}
	a.decode(t, &e)
	return e.Error
}

// req is a request to send.
type req struct {
	method, path, token string
	body                io.Reader
	headers             []string
	remote              string
}

func (f *fixture) send(r req) answer {
	hr := httptest.NewRequest(r.method, r.path, r.body)
	hr.RemoteAddr = "127.0.0.1:40000"
	if r.remote != "" {
		hr.RemoteAddr = r.remote
	}
	if r.token != "" {
		hr.Header.Set("Authorization", "Bearer "+r.token)
	}
	for i := 0; i+1 < len(r.headers); i += 2 {
		hr.Header.Set(r.headers[i], r.headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(rec, hr)
	return answer{code: rec.Code, header: rec.Header(), body: rec.Body.String()}
}

func (f *fixture) get(path, token string) answer {
	return f.send(req{method: "GET", path: path, token: token})
}

// wantSecured holds an answer to the headers of the API contract's §1,
// which every answer carries, and to no CORS and no cookie.
func wantSecured(t *testing.T, a answer, cache string) {
	t.Helper()
	for k, v := range map[string]string{
		"Content-Type": "application/json; charset=utf-8", "X-Content-Type-Options": "nosniff", "Cache-Control": cache,
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'", "Referrer-Policy": "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := a.header.Get(k); got != v {
			t.Errorf("%s: %q, want %q (%d %s)", k, got, v, a.code, a.body)
		}
	}
	for _, k := range []string{"Set-Cookie", "Access-Control-Allow-Origin", "Access-Control-Allow-Credentials"} {
		if a.header.Get(k) != "" {
			t.Errorf("an answer sets %s", k)
		}
	}
}

// wantRefused holds an answer to a refusal of code and reason.
func wantRefused(t *testing.T, a answer, status int, code, reason string) refusal {
	t.Helper()
	e := a.refusal(t)
	if a.code != status || e.Code != code || e.Details["reason"] != reason || e.Message == "" || len(e.Message) > 400 {
		t.Errorf("%d %+v; want %d %s %s", a.code, e, status, code, reason)
	}
	return e
}

// GET /info answers anyone, cached a minute: what the runtime is, its
// version, the audience to ask Core for, the issuer, and what it offers:
// hosting only when it has a Core to ask and a vault to seal with, as run
// gives it both.
func TestInfo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*Options)
		hosts bool
	}{
		{"no vault, no Core", nil, false},
		{"no Core", func(o *Options) { o.Vault = testVault(t) }, false},
		{"no vault", func(o *Options) { o.CoreBaseURL = issuer }, false},
		{"both", func(o *Options) { o.Vault, o.CoreBaseURL = testVault(t), issuer }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.edit)
			a := f.get(Prefix+"info", "")
			wantSecured(t, a, "public, max-age=60")
			var info Info
			a.decode(t, &info)
			want := Info{API: "aishie-runtime", APIVersion: 1, Version: version.Version, Commit: version.Commit, Audience: audience, Issuer: issuer,
				Features: Features{ConnectByToken: tc.hosts, OwnKey: tc.hosts}}
			if a.code != http.StatusOK || info != want {
				t.Errorf("info: %d %+v", a.code, info)
			}
			if !strings.Contains(a.body, `"school_key":false`) || !strings.Contains(a.body, `"api_version":1`) {
				t.Errorf("info: %s", a.body)
			}
		})
	}
}

// GET /me is the person Core's assertion names, in lower case, whether
// they are one of the runtime's administrators (Core's root or admin,
// narrowed by ADMIN_ACTOR_IDS), and how many agents they host here.
func TestMe(t *testing.T) {
	for _, tc := range []struct {
		name, role string
		admins     []string
		admin      bool
	}{
		{"a person with no role", "", nil, false},
		{"Core's admin", "admin", nil, true},
		{"Core's root", "root", nil, true},
		{"another role", "auditor", nil, false},
		{"Core's admin, named in ADMIN_ACTOR_IDS", "admin", []string{ken, strings.ToUpper(yuki)}, true},
		{"Core's admin, not named in ADMIN_ACTOR_IDS", "admin", []string{ken}, false},
		{"named in ADMIN_ACTOR_IDS, not Core's admin", "", []string{yuki}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(o *Options) { o.AdminActorIDs = tc.admins })
			for _, id := range []string{"agt_one", "agt_two"} {
				tok := store.Secret{ID: "sec_" + id, TenantID: "ten_yuki", Kind: store.SecretCoreToken, KEKID: "local:v1",
					WrappedDEK: []byte{1}, Nonce: []byte{2}, Ciphertext: []byte{3}}
				if _, err := f.st.CreateHostedAgent(t.Context(), store.HostedAgent{ID: id, CoreActorID: "actor-" + id, OwnerActorID: yuki,
					OwnerVerified: true, TenantID: "ten_yuki", TokenSecretID: tok.ID}, tok); err != nil {
					t.Fatal(err)
				}
			}
			a := f.get(Prefix+"me", f.core.assert(t, claims(strings.ToUpper(yuki), tc.role, nil)))
			wantSecured(t, a, "no-store")
			var me Me
			a.decode(t, &me)
			if a.code != http.StatusOK || me != (Me{ActorID: yuki, DisplayName: "Yuki Tanaka", IsAdmin: tc.admin, HostedAgents: 2}) {
				t.Errorf("me: %d %+v", a.code, me)
			}
			if strings.Contains(a.body, "yuki@example.edu") {
				t.Error("GET /me gives the email")
			}
		})
	}
}

// GET /me records the person, at most every five minutes, and says so
// when the store cannot be reached.
func TestMeRecordsThePerson(t *testing.T) {
	f := newFixture(t, nil)
	token := f.core.assert(t, claims(yuki, "admin", nil))
	f.get(Prefix+"me", token)
	p, err := f.st.Person(t.Context(), yuki)
	if err != nil || p.DisplayName != "Yuki Tanaka" || p.PlatformRole != "admin" || !p.LastSeenAt.Equal(at) {
		t.Fatalf("the person: %+v %v", p, err)
	}
	f.add(time.Minute)
	f.get(Prefix+"me", token)
	if p, _ := f.st.Person(t.Context(), yuki); !p.LastSeenAt.Equal(at) {
		t.Errorf("recorded again within five minutes: %s", p.LastSeenAt)
	}
	f.add(4 * time.Minute)
	f.get(Prefix+"me", token)
	if p, _ := f.st.Person(t.Context(), yuki); !p.LastSeenAt.Equal(at.Add(5 * time.Minute)) {
		t.Errorf("not recorded again after five minutes: %s", p.LastSeenAt)
	}

	f.st.mu.Lock()
	f.st.fail = true
	f.st.mu.Unlock()
	a := f.get(Prefix+"me", token)
	wantSecured(t, a, "no-store")
	wantRefused(t, a, http.StatusServiceUnavailable, CodeUnavailable, ReasonStoreUnavailable)
	if a.header.Get("Retry-After") != "5" {
		t.Errorf("Retry-After %q", a.header.Get("Retry-After"))
	}
}

// An assertion that is not Core's, for this runtime, of a person, now, is
// refused with its reason, a 401 with WWW-Authenticate (error=invalid_token
// when one was sent), counted and logged at debug with its reason alone;
// without Core's keys, 503 keys_unavailable. No answer and no log line
// holds the assertion.
func TestMeRefusesAssertions(t *testing.T) {
	f := newFixture(t, nil)
	other := newCore(t)
	expired := f.core.assert(t, claims(yuki, "", func(c map[string]any) {
		c["iat"], c["nbf"], c["exp"] = at.Add(-10*time.Minute).Unix(), at.Add(-10*time.Minute).Unix(), at.Add(-5*time.Minute).Unix()
	}))
	for _, tc := range []struct {
		name, auth, reason string
	}{
		{"no Authorization", "", webauth.ReasonMissing},
		{"not a bearer", "Basic eXVraTpodW50ZXIy", webauth.ReasonMalformed},
		{"a bearer of nothing", "Bearer ", webauth.ReasonMalformed},
		{"over 8 KB", "Bearer " + strings.Repeat("a", webauth.MaxAssertionBytes), webauth.ReasonMalformed},
		{"not a JWS", "Bearer eyJhbGciOiJFZERTQSJ9.not-a-jws", webauth.ReasonMalformed},
		{"a Core token", "Bearer ais_k7v2m4qhx3ab_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG", webauth.ReasonMalformed},
		{"expired", "Bearer " + expired, webauth.ReasonExpired},
		{"for another audience", "Bearer " + f.core.assert(t, claims(yuki, "", func(c map[string]any) { c["aud"] = issuer + "/other-runtime" })), webauth.ReasonInvalid},
		{"from another issuer", "Bearer " + f.core.assert(t, claims(yuki, "", func(c map[string]any) { c["iss"] = "https://evil.example.edu" })), webauth.ReasonInvalid},
		{"signed with another key", "Bearer " + other.assert(t, claims(yuki, "admin", nil)), webauth.ReasonInvalid},
		{"of an agent", "Bearer " + f.core.assert(t, claims(yuki, "", func(c map[string]any) { c["kind"] = "agent" })), webauth.ReasonInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := req{method: "GET", path: Prefix + "me", remote: "192.0.2.1:1"}
			if tc.auth != "" {
				r.headers = []string{"Authorization", tc.auth}
			}
			a := f.send(r)
			wantSecured(t, a, "no-store")
			wantRefused(t, a, http.StatusUnauthorized, CodeUnauthenticated, tc.reason)
			want := `Bearer realm="aishie-runtime", error="invalid_token"`
			if tc.auth == "" {
				want = `Bearer realm="aishie-runtime"`
			}
			if got := a.header.Get("WWW-Authenticate"); got != want {
				t.Errorf("WWW-Authenticate: %q", got)
			}
			if token := strings.TrimPrefix(tc.auth, "Bearer "); len(token) > 8 && (strings.Contains(a.body, token) || strings.Contains(f.log.String(), token)) {
				t.Error("the assertion is repeated")
			}
		})
	}
	if n := testutil.ToFloat64(f.s.m.authFailures.WithLabelValues(webauth.ReasonInvalid)); n != 4 {
		t.Errorf("%v assertion_invalid counted", n)
	}
	var debug int
	for _, l := range f.log.lines(t, "an assertion was refused") {
		if l["level"] == "DEBUG" && l["reason"] != "" {
			debug++
		}
	}
	if debug != 11 {
		t.Errorf("%d debug lines of refusals", debug)
	}

	down := newFixture(t, func(o *Options) { o.Verifier.Keys = webauth.NewRemoteKeys("http://127.0.0.1:1", nil) })
	a := down.get(Prefix+"me", down.core.assert(t, claims(yuki, "", nil)))
	wantSecured(t, a, "no-store")
	wantRefused(t, a, http.StatusServiceUnavailable, CodeUnavailable, ReasonKeysUnavailable)
	if a.header.Get("Retry-After") != "5" {
		t.Errorf("Retry-After %q", a.header.Get("Retry-After"))
	}
}

// The API serves its routes and nothing else, all in JSON: not /status,
// /healthz or /metrics, which are HTTP_ADDR's; not a path it would tidy;
// a route asked with another method (OPTIONS among them) is a 405 naming
// those it takes; a query parameter is unknown_parameter.
func TestRoutes(t *testing.T) {
	f := newFixture(t, nil)
	for _, p := range []string{"/status", "/healthz", "/metrics", "/", "/runtime/api/v1/", "/runtime/api/v1/nothing",
		"/runtime/api/v2/me", "/runtime/api/v1//info", "/runtime/api/v1/./info", "/runtime/api/v1/%69nfo", "/runtime/api/v1/info/"} {
		a := f.get(p, "")
		wantSecured(t, a, "no-store")
		wantRefused(t, a, http.StatusNotFound, CodeNotFound, ReasonNoRoute)
	}
	token := f.core.assert(t, claims(yuki, "", nil))
	for _, m := range []string{"DELETE", "OPTIONS", "PUT"} {
		a := f.send(req{method: m, path: Prefix + "me", token: token})
		wantRefused(t, a, http.StatusMethodNotAllowed, CodeMethodNotAllowed, ReasonMethodNotAllowed)
		if !strings.Contains(a.header.Get("Allow"), "GET") {
			t.Errorf("%s /me: Allow %q", m, a.header.Get("Allow"))
		}
	}
	for _, p := range []string{Prefix + "info?x=1", Prefix + "me?revoke_token=true&a=b"} {
		a := f.get(p, token)
		e := wantRefused(t, a, http.StatusBadRequest, CodeInvalidArgument, ReasonUnknownParameter)
		if e.Details["field"] == "" {
			t.Errorf("%s: no field", p)
		}
	}
}

// A body must be JSON, one object, at most 64 KB, naming no key twice and
// nothing the route does not take; a route of no body takes an empty one or
// {}. A request from another site that would change something is refused;
// one from the front end's own origin, or from no browser, is served.
func TestBodies(t *testing.T) {
	f := newFixture(t, nil)
	f.s.mux.Handle("POST "+Prefix+"echo", f.s.authed(func(w http.ResponseWriter, _ *http.Request, _ *Caller) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	}))
	type named struct {
		A int `json:"a"`
	}
	f.s.mux.Handle("PUT "+Prefix+"named", f.s.authedBody(func(w http.ResponseWriter, r *http.Request, _ *Caller) {
		var v named
		if decodeBody(w, r, &v, false) {
			writeJSON(w, http.StatusOK, v)
		}
	}))
	token := f.core.assert(t, claims(yuki, "", nil))
	json := []string{"Content-Type", "application/json; charset=utf-8"}
	for _, tc := range []struct {
		name, method, path, body string
		headers                  []string
		code                     int
		reason                   string
	}{
		{"GET with {}", "GET", "me", "{}", json, 200, ""},
		{"GET with empty", "GET", "me", "", nil, 200, ""},
		{"GET with a member", "GET", "me", `{"x":1}`, json, 400, ReasonUnknownField},
		{"POST {}", "POST", "echo", " {} ", json, 200, ""},
		{"from the front end", "POST", "echo", "{}", append([]string{"Sec-Fetch-Site", "same-origin", "Origin", "http://example.com"}, json...), 200, ""},
		{"text", "POST", "echo", "{}", []string{"Content-Type", "text/plain"}, 400, ReasonNotJSON},
		{"no type", "POST", "echo", "{}", nil, 400, ReasonNotJSON},
		{"a key twice", "POST", "echo", `{"a":1,"a":2}`, json, 400, ReasonMalformedJSON},
		{"a list", "POST", "echo", `[{}]`, json, 400, ReasonMalformedJSON},
		{"two objects", "POST", "echo", `{} {}`, json, 400, ReasonMalformedJSON},
		{"not JSON", "POST", "echo", `{"a":`, json, 400, ReasonMalformedJSON},
		{"too large", "POST", "echo", `{"a":"` + strings.Repeat("x", MaxBodyBytes) + `"}`, json, 400, ReasonBodyTooLarge},
		{"just too large", "POST", "echo", "{}" + strings.Repeat(" ", MaxBodyBytes-1), json, 400, ReasonBodyTooLarge},
		{"just small enough", "POST", "echo", "{}" + strings.Repeat(" ", MaxBodyBytes-2), json, 200, ""},
		{"a member of the route's", "PUT", "named", `{"a":7}`, json, 200, ""},
		{"a member not of the route's", "PUT", "named", `{"a":7,"b/c":1}`, json, 400, ReasonUnknownField},
		{"no body where one is needed", "PUT", "named", ``, json, 400, ReasonMissingField},
		{"from another site", "POST", "echo", "{}", append([]string{"Sec-Fetch-Site", "cross-site"}, json...), 403, ReasonCrossOrigin},
		{"from another origin", "POST", "echo", "{}", append([]string{"Origin", "https://evil.example.org"}, json...), 403, ReasonCrossOrigin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := f.send(req{method: tc.method, path: Prefix + tc.path, token: token, body: strings.NewReader(tc.body), headers: tc.headers})
			wantSecured(t, a, "no-store")
			if tc.code == 200 {
				if a.code != 200 {
					t.Errorf("%d %s", a.code, a.body)
				}
				return
			}
			e := wantRefused(t, a, tc.code, map[int]string{400: CodeInvalidArgument, 403: CodeForbidden}[tc.code], tc.reason)
			if tc.reason == ReasonUnknownField && e.Details["field"] != map[string]string{"named": "/b~1c", "me": "/x"}[tc.path] {
				t.Errorf("field %v", e.Details["field"])
			}
		})
	}
}

// Each bucket answers 429 rate_limited with Retry-After and
// retry_after_seconds past its allowance: a person's calls; an address's
// requests without an assertion, and its refused assertions. Another
// person's and another address's are their own, and they fill again with
// time.
func TestRateLimits(t *testing.T) {
	f := newFixture(t, nil)
	f.s.general, f.s.perIP, f.s.failures = newLimiter(Rate{60, 2}), newLimiter(Rate{60, 3}), newLimiter(Rate{60, 2})
	limited := func(t *testing.T, a answer) {
		t.Helper()
		e := wantRefused(t, a, http.StatusTooManyRequests, CodeRateLimited, ReasonRateLimited)
		if a.header.Get("Retry-After") != "1" || e.Details["retry_after_seconds"] != 1.0 {
			t.Errorf("Retry-After %q, %v", a.header.Get("Retry-After"), e.Details)
		}
	}
	token := f.core.assert(t, claims(yuki, "", nil))
	t.Run("a person's", func(t *testing.T) {
		for range 2 {
			if a := f.get(Prefix+"me", token); a.code != http.StatusOK {
				t.Fatalf("%d %s", a.code, a.body)
			}
		}
		limited(t, f.get(Prefix+"me", token))
		if a := f.get(Prefix+"me", f.core.assert(t, claims(ken, "", nil))); a.code != http.StatusOK {
			t.Errorf("another person: %d", a.code)
		}
	})
	t.Run("an address's refused assertions", func(t *testing.T) {
		bad := req{method: "GET", path: Prefix + "me", token: "not-an-assertion", remote: "198.51.100.1:1"}
		for range 2 {
			if a := f.send(bad); a.code != http.StatusUnauthorized {
				t.Fatalf("%d %s", a.code, a.body)
			}
		}
		limited(t, f.send(bad))
		// A good assertion from that address is still served.
		if a := f.send(req{method: "GET", path: Prefix + "me", token: f.core.assert(t, claims(ken, "", nil)), remote: "198.51.100.1:1"}); a.code != http.StatusOK {
			t.Errorf("a good assertion from the address: %d", a.code)
		}
	})
	t.Run("an address's requests without an assertion", func(t *testing.T) {
		info := req{method: "GET", path: Prefix + "info", remote: "198.51.100.2:1"}
		for range 3 {
			if a := f.send(info); a.code != http.StatusOK {
				t.Fatalf("%d %s", a.code, a.body)
			}
		}
		limited(t, f.send(info))
		if a := f.send(req{method: "GET", path: Prefix + "info", remote: "198.51.100.3:1"}); a.code != http.StatusOK {
			t.Errorf("another address: %d", a.code)
		}
	})
	f.add(time.Second)
	if a := f.get(Prefix+"me", token); a.code != http.StatusOK {
		t.Errorf("a second later: %d", a.code)
	}
}

// Each limiter keeps at most maxBuckets buckets, the least recently used
// going first.
func TestLimiterIsBounded(t *testing.T) {
	l := newLimiter(Rate{60, 1})
	l.take("first", at)
	for i := range maxBuckets {
		l.take(strings.Repeat("k", i%7)+string(rune('a'+i%26))+string(rune(i)), at)
	}
	if n := l.size(); n != maxBuckets {
		t.Fatalf("%d buckets", n)
	}
	if ok, _ := l.take("first", at); !ok {
		t.Error("the bucket used least recently was kept, empty")
	}
}

// Every request is one log line and counted by route and code; no line
// holds the assertion, a token, a key, a name or an email; a panic is a 500
// in JSON, with the headers.
func TestLogsAndMetrics(t *testing.T) {
	f := newFixture(t, nil)
	f.s.mux.HandleFunc("GET "+Prefix+"panic", func(http.ResponseWriter, *http.Request) { panic("boom") })
	token := f.core.assert(t, claims(yuki, "admin", nil))
	f.get(Prefix+"me", token)
	f.get(Prefix+"me", "not-a-jws")
	f.get(Prefix+"nothing", "")
	a := f.get(Prefix+"panic", "")
	wantSecured(t, a, "no-store")
	wantRefused(t, a, http.StatusInternalServerError, CodeInternal, ReasonInternal)

	lines := f.log.lines(t, "api request")
	if len(lines) != 4 {
		t.Fatalf("%d request lines:\n%s", len(lines), f.log.String())
	}
	for i, want := range []map[string]any{
		{"method": "GET", "route": "GET " + Prefix + "me", "status": 200.0, "reason": "", "actor": yuki},
		{"method": "GET", "route": "GET " + Prefix + "me", "status": 401.0, "reason": webauth.ReasonMalformed, "actor": ""},
		{"method": "GET", "route": "", "status": 404.0, "reason": ReasonNoRoute, "actor": ""},
		{"method": "GET", "route": "GET " + Prefix + "panic", "status": 500.0, "reason": ReasonInternal},
	} {
		for k, v := range want {
			if lines[i][k] != v {
				t.Errorf("line %d: %s is %v, want %v", i+1, k, lines[i][k], v)
			}
		}
		if _, ok := lines[i]["ms"]; !ok {
			t.Errorf("line %d has no ms", i+1)
		}
	}
	for _, s := range []string{token, "eyJ", "ais_", "Yuki Tanaka", "yuki@example.edu"} {
		if strings.Contains(f.log.String(), s) {
			t.Errorf("the log holds %.12s…", s)
		}
	}
	for labels, want := range map[[2]string]float64{{"GET " + Prefix + "me", "ok"}: 1, {"GET " + Prefix + "me", webauth.ReasonMalformed}: 1,
		{"none", ReasonNoRoute}: 1, {"GET " + Prefix + "panic", ReasonInternal}: 1} {
		if got := testutil.ToFloat64(f.s.m.requests.WithLabelValues(labels[0], labels[1])); got != want {
			t.Errorf("requests %v: %v", labels, got)
		}
	}
	if n, err := testutil.GatherAndCount(f.reg, "aishie_api_request_seconds", "aishie_api_auth_failures_total", "aishie_api_audit_failures_total"); err != nil || n == 0 {
		t.Errorf("the metrics on the registerer: %d %v", n, err)
	}
}

// Audit records an event with the person, their session and their address
// (a trusted proxy's X-Forwarded-For believed, another's not); one the
// store cannot take is logged, with its action and target alone, and
// counted, and fails nothing.
func TestAudit(t *testing.T) {
	proxies, err := ParseProxies([]string{"127.0.0.1", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(o *Options) { o.TrustedProxies = proxies })
	f.s.mux.Handle("POST "+Prefix+"act", f.s.authed(func(w http.ResponseWriter, r *http.Request, _ *Caller) {
		f.s.Audit(r.Context(), r, store.AuditEvent{Action: "agent.pause", TargetType: "hosted_agent", TargetID: "agt_1", Outcome: "ok",
			Detail: json.RawMessage(`{"paused":true}`)})
		writeJSON(w, http.StatusOK, map[string]string{})
	}))
	token := f.core.assert(t, claims(yuki, "", nil))
	for _, r := range []req{
		{method: "POST", path: Prefix + "act", token: token, headers: []string{"X-Forwarded-For", "198.51.100.9, 203.0.113.5, 10.1.2.3"}},
		{method: "POST", path: Prefix + "act", token: token, headers: []string{"X-Forwarded-For", "198.51.100.9"}, remote: "192.0.2.1:5555"},
	} {
		if a := f.send(r); a.code != http.StatusOK {
			t.Fatalf("%d %s", a.code, a.body)
		}
	}
	events, err := f.st.AuditEvents(t.Context(), time.Time{}, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("%+v %v", events, err)
	}
	for i, ip := range []string{"203.0.113.5", "192.0.2.1"} {
		e := events[i]
		if e.ActorID != yuki || e.SessionID != sid || e.IP != ip || e.Action != "agent.pause" || e.TargetID != "agt_1" || !e.At.Equal(at) {
			t.Errorf("event %d: %+v", i+1, e)
		}
	}

	f.st.mu.Lock()
	f.st.fail = true
	f.st.mu.Unlock()
	if a := f.send(req{method: "POST", path: Prefix + "act", token: token}); a.code != http.StatusOK {
		t.Errorf("a request whose audit failed: %d", a.code)
	}
	if n := testutil.ToFloat64(f.s.m.auditFailures); n != 1 {
		t.Errorf("%v audit failures counted", n)
	}
	if l := f.log.lines(t, "an audit event was not recorded"); len(l) != 1 || l[0]["action"] != "agent.pause" || l[0]["target"] != "agt_1" {
		t.Errorf("the failure's line: %v", l)
	}

	if _, err := ParseProxies([]string{"10.0.0.0/8", "proxy.internal"}); err == nil || !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("a proxy that is not an address: %v", err)
	}
	if n, _ := ParseProxies([]string{"::1"}); len(n) != 1 || !n[0].Contains(net.ParseIP("::1")) || n[0].Contains(net.ParseIP("::2")) {
		t.Errorf("::1: %v", n)
	}
}

// Served on its own listener, the API answers its routes and nothing of
// HTTP_ADDR's, and stops with its context.
func TestServe(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.s.Serve(ctx) }()
	for p, want := range map[string]int{Prefix + "info": 200, "/status": 404, "/metrics": 404, "/healthz": 404} {
		resp, err := http.Get("http://" + f.s.Addr() + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: %d, want %d", p, resp.StatusCode, want)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
