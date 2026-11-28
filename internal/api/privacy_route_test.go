package api

import (
	"container/list"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/webauth"
)

const (
	privacyAgentID    = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	privacyCourseUUID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	privacyAskerID    = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	privacyOwnerID    = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	privacyOtherID    = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	privacyCoreURL    = "https://core.example.edu"
	privacyAudience   = "https://core.example.edu/runtime"
)

type askCall struct{ actor, agent, course string }

type stubAskPermission struct {
	mu    sync.Mutex
	allow func(actor, agent, course string) bool
	err   error
	calls []askCall
}

func (s *stubAskPermission) CheckAsk(_ context.Context, actor, agent, course string) (*core.RuntimeCheckAskResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, askCall{actor: actor, agent: agent, course: course})
	if s.err != nil {
		return nil, s.err
	}
	return &core.RuntimeCheckAskResult{MayAsk: s.allow != nil && s.allow(actor, agent, course)}, nil
}

func (s *stubAskPermission) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubAskPermission) setError(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *stubAskPermission) allowOnly(actor, agent, course string) {
	s.mu.Lock()
	s.allow = func(gotActor, gotAgent, gotCourse string) bool {
		return gotActor == actor && gotAgent == agent && gotCourse == course
	}
	s.mu.Unlock()
}

type privacyHosting struct {
	mu     sync.Mutex
	config *config.Config
}

func (h *privacyHosting) YAML() *config.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.config
}
func (h *privacyHosting) Prices() *pricing.Table { return nil }
func (h *privacyHosting) set(c *config.Config) {
	h.mu.Lock()
	h.config = c
	h.mu.Unlock()
}

type privacyKeys struct {
	key ed25519.PrivateKey
	pub ed25519.PublicKey
	kid string
}

func (k privacyKeys) Key(_ context.Context, kid string) (ed25519.PublicKey, error) {
	if kid != k.kid {
		return nil, webauth.ErrUnknownKey
	}
	return k.pub, nil
}

type privacyFixture struct {
	store      *memstore.Store
	hosting    *privacyHosting
	permission *stubAskPermission
	api        *Server
	server     *httptest.Server
	keys       privacyKeys
	now        time.Time
}

func newPrivacyFixture(t *testing.T, runtime config.Runtime) *privacyFixture {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := key.Public().(ed25519.PublicKey)
	permission := &stubAskPermission{}
	f := &privacyFixture{store: memstore.New(), hosting: &privacyHosting{config: &config.Config{Runtime: runtime}},
		permission: permission, keys: privacyKeys{key: key, pub: pub, kid: webauth.Thumbprint(pub)}, now: time.Now().UTC()}
	f.api = New(Options{Store: f.store, Hosting: f.hosting, CoreBaseURL: privacyCoreURL, AskPermission: permission,
		Verifier: &webauth.Verifier{Keys: f.keys, Issuer: privacyCoreURL, Audience: privacyAudience, Now: func() time.Time { return f.now }}})
	// The authorization route's production rate limit is not under test; keep
	// the malformed-request matrix independent of timing.
	f.api.token.mu.Lock()
	f.api.token.perSec, f.api.token.burst = 10000, 10000
	f.api.token.byKey = map[string]*list.Element{}
	f.api.token.order = list.New()
	f.api.token.mu.Unlock()
	f.server = httptest.NewServer(f.api.Handler())
	t.Cleanup(f.server.Close)
	return f
}

func (f *privacyFixture) assertion(t *testing.T, actor string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"` + f.keys.kid + `"}`))
	claims := map[string]any{"iss": privacyCoreURL, "aud": privacyAudience, "sub": actor, "kind": "human", "name": "Privacy User",
		"iat": f.now.Unix(), "nbf": f.now.Unix(), "exp": f.now.Add(5 * time.Minute).Unix(), "jti": "privacy-" + actor, "sid": "privacy-session"}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed := header + "." + base64.RawURLEncoding.EncodeToString(raw)
	return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.keys.key, []byte(signed)))
}

type privacyAnswer struct {
	code int
	body string
}

func (f *privacyFixture) get(t *testing.T, actor, path, body string) privacyAnswer {
	t.Helper()
	// Move the shared test clock between independent requests so the rate-limit
	// matrix tests authorization rather than waiting on the production bucket.
	f.now = f.now.Add(time.Minute)
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/runtime/api/v1/"+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if actor != "" {
		req.Header.Set("Authorization", "Bearer "+f.assertion(t, actor))
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return privacyAnswer{code: resp.StatusCode, body: string(raw)}
}

func (a privacyAnswer) decode(t *testing.T, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(a.body), out); err != nil {
		t.Fatalf("HTTP %d: %s: %v", a.code, a.body, err)
	}
}

func privacyRefusal(t *testing.T, a privacyAnswer, status int, code, reason string) {
	t.Helper()
	var out struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	a.decode(t, &out)
	if a.code != status || out.Error.Code != code || out.Error.Details["reason"] != reason {
		t.Fatalf("HTTP %d: %s; want %d %s/%s", a.code, a.body, status, code, reason)
	}
}

func privacyPath(agentID, courseID string) string {
	return "agents/" + agentID + "/privacy?course_id=" + courseID
}

func (f *privacyFixture) hosted(t *testing.T, settings string) store.HostedAgent {
	t.Helper()
	v := testVault(t)
	key, err := v.Seal(t.Context(), store.Secret{ID: vault.NewSecretID(), TenantID: "ten_privacy", Kind: store.SecretModelKey}, "sk-privacy-key")
	if err != nil {
		t.Fatal(err)
	}
	row, err := f.store.CreateHostedAgent(t.Context(), store.HostedAgent{ID: "agt_privacy", CoreActorID: privacyAgentID,
		OwnerActorID: privacyOwnerID, DisplayName: "Privacy tutor", TenantID: "ten_privacy", Settings: json.RawMessage(settings), KeySecretID: key.ID}, key)
	if err != nil {
		t.Fatal(err)
	}
	return *row
}

func privacySchoolRuntime() config.Runtime {
	return config.Runtime{School: config.School{Offers: []config.SchoolOffer{
		{ID: "standard", Label: "School AI", Adapter: "openai_chat", Model: "deepseek-chat", Provider: "deepseek",
			BaseURL: "https://api.deepseek.com", KeyRef: "secret://school/deepseek"},
	}}}
}

func ownPrivacyModel() string {
	return `{"model":{"adapter":"openai_chat","model":"gpt-private","provider":"openai","key_source":"own","base_url":"https://api.openai.com/v1"}}`
}

func TestAgentPrivacyAuthorization(t *testing.T) {
	f := newPrivacyFixture(t, privacySchoolRuntime())
	row := f.hosted(t, ownPrivacyModel())
	if _, err := registry.ForCourse(t.Context(), f.hosting.config, row, nil, registry.Options{CoreBaseURL: privacyCoreURL}, privacyCourseUUID); err != nil {
		t.Fatalf("hosted privacy model is invalid: %v", err)
	}
	f.permission.allowOnly(privacyAskerID, privacyAgentID, privacyCourseUUID)
	a := f.get(t, privacyAskerID, privacyPath(privacyAgentID, privacyCourseUUID), "")
	var got AgentPrivacy
	a.decode(t, &got)
	openAI := "OpenAI"
	want := AgentPrivacy{AgentID: privacyAgentID, Primary: &PrimaryModelProvider{Plan: "own", Provider: "openai", ProviderLabel: &openAI}}
	if a.code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed asker: %d %s", a.code, a.body)
	}
	for _, secret := range []string{"sk-privacy-key", "gpt-private", "sk-proj", "base_url", "key_hint", "owner_actor_id", "agt_privacy", privacyOwnerID} {
		if strings.Contains(a.body, secret) {
			t.Errorf("the disclosure contains %q: %s", secret, a.body)
		}
	}
	otherCourse := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	unknown := "99999999-9999-4999-8999-999999999999"
	for _, tc := range []struct{ name, agent, course, actor string }{
		{"the owner is not silently authorized", privacyAgentID, privacyCourseUUID, privacyOwnerID},
		{"another agent of the same asker", privacyOtherID, privacyCourseUUID, privacyAskerID},
		{"the same agent through another course", privacyAgentID, otherCourse, privacyAskerID},
		{"an unknown agent", unknown, privacyCourseUUID, privacyAskerID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := f.get(t, tc.actor, privacyPath(tc.agent, tc.course), "")
			privacyRefusal(t, a, http.StatusNotFound, CodeNotFound, ReasonAgentNotFound)
			if strings.Contains(a.body, "openai") || strings.Contains(a.body, "provider") {
				t.Errorf("a refusal discloses a provider: %s", a.body)
			}
		})
	}
	if n := f.permission.count(); n != 5 {
		t.Errorf("Core authorization calls = %d, want one allowed and four refused", n)
	}
}

func TestAgentPrivacyFailsClosed(t *testing.T) {
	f := newPrivacyFixture(t, privacySchoolRuntime())
	f.hosted(t, `{}`)
	f.permission.allowOnly(privacyAskerID, privacyAgentID, privacyCourseUUID)
	for _, tc := range []struct {
		name, path, body string
		code             int
		reason           string
	}{
		{"no course", "agents/" + privacyAgentID + "/privacy", "", 400, ReasonMissingField},
		{"invalid course", privacyPath(privacyAgentID, "course-1"), "", 400, ReasonInvalidField},
		{"invalid agent", privacyPath("agent-1", privacyCourseUUID), "", 400, ReasonInvalidField},
		{"unknown query", privacyPath(privacyAgentID, privacyCourseUUID) + "&agent_id=x", "", 400, ReasonUnknownParameter},
		{"course twice", "agents/" + privacyAgentID + "/privacy?course_id=" + privacyCourseUUID + "&course_id=" + privacyCourseUUID, "", 400, ReasonInvalidField},
		{"a member in the body", privacyPath(privacyAgentID, privacyCourseUUID), `{"provider":"openai"}`, 400, ReasonUnknownField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.permission.count()
			privacyRefusal(t, f.get(t, privacyAskerID, tc.path, tc.body), tc.code, CodeInvalidArgument, tc.reason)
			if f.permission.count() != before {
				t.Error("an invalid request asked Core for an authorization decision")
			}
		})
	}
	before := f.permission.count()
	privacyRefusal(t, f.get(t, "", privacyPath(privacyAgentID, privacyCourseUUID), ""), http.StatusUnauthorized, CodeUnauthenticated, "assertion_missing")
	if f.permission.count() != before {
		t.Error("an unauthenticated request asked Core for an authorization decision")
	}
	f.permission.setError(errors.New("Core is down"))
	privacyRefusal(t, f.get(t, privacyAskerID, privacyPath(privacyAgentID, privacyCourseUUID), ""), 503, CodeUnavailable, ReasonCoreUnavailable)
	f.permission.setError(&core.ServiceError{Tool: "agent_runtime.check_ask", Reason: core.ReasonCoreTooOld})
	privacyRefusal(t, f.get(t, privacyAskerID, privacyPath(privacyAgentID, privacyCourseUUID), ""), 422, CodeFailedPrecondition, ReasonCoreTooOld)

	withoutCredential := newPrivacyFixture(t, privacySchoolRuntime())
	withoutCredential.api.o.AskPermission = nil
	privacyRefusal(t, withoutCredential.get(t, privacyAskerID, privacyPath(privacyAgentID, privacyCourseUUID), ""),
		503, CodeUnavailable, ReasonRuntimeMisconfigured)
}

func TestAgentPrivacySchoolOfferAndFallback(t *testing.T) {
	f := newPrivacyFixture(t, privacySchoolRuntime())
	f.hosted(t, `{"model":{"key_source":"school","offer":"standard","fallback":{"adapter":"openai_chat","model":"gpt-private","provider":"openai","key_source":"own","base_url":"https://api.openai.com/v1"}}}`)
	f.permission.allowOnly(privacyAskerID, privacyAgentID, privacyCourseUUID)
	a := f.get(t, privacyAskerID, privacyPath(privacyAgentID, privacyCourseUUID), "")
	var got AgentPrivacy
	a.decode(t, &got)
	if a.code != 200 || got.Primary == nil || got.Primary.Plan != "school" || got.Primary.Provider != "deepseek" ||
		got.Primary.ProviderLabel == nil || *got.Primary.ProviderLabel != "DeepSeek" ||
		got.Primary.SchoolOfferLabel == nil || *got.Primary.SchoolOfferLabel != "School AI" ||
		got.Fallback == nil || got.Fallback.Provider != "openai" || got.Fallback.ProviderLabel == nil || *got.Fallback.ProviderLabel != "OpenAI" {
		t.Fatalf("school and fallback: %d %s", a.code, a.body)
	}
	for _, secret := range []string{"deepseek-chat", "gpt-private", "standard", "secret://", "sealed://", "key_ref", "endpoint", "sk-privacy-key"} {
		if strings.Contains(a.body, secret) {
			t.Errorf("the disclosure contains %q: %s", secret, a.body)
		}
	}
}

func TestAgentPrivacySchoolOwnServer(t *testing.T) {
	rt := privacySchoolRuntime()
	rt.School.Offers = append(rt.School.Offers, config.SchoolOffer{ID: "gpu", Label: "CS GPU server", Adapter: "openai_chat",
		Model: "llama-private", Provider: "openai_compatible", BaseURL: "https://gpu.internal.example/v1", KeyRef: "secret://school/gpu"})
	f := newPrivacyFixture(t, rt)
	f.hosted(t, `{"model":{"key_source":"school","offer":"gpu"}}`)
	f.permission.allowOnly(privacyAskerID, privacyAgentID, privacyCourseUUID)
	a := f.get(t, privacyAskerID, privacyPath(privacyAgentID, privacyCourseUUID), "")
	var got AgentPrivacy
	a.decode(t, &got)
	if a.code != 200 || got.Primary == nil || got.Primary.Plan != "school" || got.Primary.Provider != "openai_compatible" ||
		got.Primary.ProviderLabel != nil || got.Primary.SchoolOfferLabel == nil || *got.Primary.SchoolOfferLabel != "CS GPU server" || got.Fallback != nil {
		t.Fatalf("school's own server: %d %s", a.code, a.body)
	}
	for _, secret := range []string{"gpu.internal.example", "llama-private", "secret://", "base_url", "key_ref"} {
		if strings.Contains(a.body, secret) {
			t.Errorf("the disclosure contains %q: %s", secret, a.body)
		}
	}
}

func TestAgentPrivacyWithdrawnOfferUsesEffectiveModel(t *testing.T) {
	f := newPrivacyFixture(t, privacySchoolRuntime())
	f.hosted(t, `{"model":{"key_source":"school","offer":"standard","fallback":{"adapter":"openai_chat","model":"gpt-private","provider":"openai","key_source":"own","base_url":"https://api.openai.com/v1"}}}`)
	f.permission.allowOnly(privacyAskerID, privacyAgentID, privacyCourseUUID)
	f.hosting.set(&config.Config{})
	a := f.get(t, privacyAskerID, privacyPath(privacyAgentID, privacyCourseUUID), "")
	var got AgentPrivacy
	a.decode(t, &got)
	if a.code != 200 || got.Primary == nil || got.Primary.Plan != "own" || got.Primary.Provider != "openai" ||
		got.Primary.SchoolOfferLabel != nil || got.Fallback != nil {
		t.Fatalf("withdrawn offer: %d %s", a.code, a.body)
	}
}

func TestAgentPrivacyWithoutAModel(t *testing.T) {
	f := newPrivacyFixture(t, privacySchoolRuntime())
	f.hosted(t, `{}`)
	f.permission.allowOnly(privacyAskerID, privacyAgentID, privacyCourseUUID)
	a := f.get(t, privacyAskerID, privacyPath(privacyAgentID, privacyCourseUUID), "")
	var got AgentPrivacy
	a.decode(t, &got)
	if a.code != 200 || got.AgentID != privacyAgentID || got.Primary != nil || got.Fallback != nil ||
		!strings.Contains(a.body, `"primary":null,"fallback":null`) {
		t.Fatalf("no model: %d %s", a.code, a.body)
	}
}

func TestAgentPrivacyYAMLAgentCourseOverride(t *testing.T) {
	f := newPrivacyFixture(t, config.Runtime{})
	f.permission.allowOnly(privacyAskerID, privacyAgentID, privacyCourseUUID)
	raw, err := json.Marshal(map[string]any{
		"agent": map[string]any{
			"id": "privacy-yaml", "display_name": "Privacy YAML tutor", "tenant_id": "ten_privacy",
			"core":  map[string]any{"base_url": privacyCoreURL, "agent_id": privacyAgentID},
			"model": map[string]any{"adapter": "openai_chat", "model": "gpt-private", "provider": "openai", "key_source": "own", "key_ref": "env://OPENAI"},
		},
		"courses": map[string]any{privacyCourseUUID: map[string]any{"model": map[string]any{
			"adapter": "anthropic", "model": "claude-private", "provider": "anthropic", "key_source": "own", "key_ref": "env://ANTHROPIC"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	agents, rejected := config.LoadDocuments(f.hosting.YAML(), nil, config.Source{Name: "privacy-test", Data: raw})
	if len(rejected) != 0 || len(agents) != 1 {
		t.Fatalf("YAML agent: %+v %+v", agents, rejected)
	}
	current := f.hosting.YAML()
	current.Agents = agents
	f.hosting.set(current)
	if _, err := f.hosting.config.Agents[0].ForCourse(privacyCourseUUID); err != nil {
		t.Fatalf("YAML course override is invalid: %v", err)
	}
	a := f.get(t, privacyAskerID, privacyPath(privacyAgentID, privacyCourseUUID), "")
	var got AgentPrivacy
	a.decode(t, &got)
	if a.code != 200 || got.Primary == nil || got.Primary.Provider != "anthropic" || got.Primary.ProviderLabel == nil ||
		*got.Primary.ProviderLabel != "Anthropic" || got.Primary.Plan != "own" || got.Fallback != nil {
		t.Fatalf("course override: %d %s", a.code, a.body)
	}
	if strings.Contains(a.body, "claude-private") || strings.Contains(a.body, "env://") {
		t.Errorf("the course disclosure leaks model configuration: %s", a.body)
	}
}

// A tiny HTTP Core checks the actual RuntimeService wire contract as well as
// the API's stub boundary. It never returns a provider error or secret.
func TestPrivacyCheckAskHTTPSmoke(t *testing.T) {
	const credential = "aissvc_privacy_test"
	tool := func(name, method, path string) map[string]any {
		return map[string]any{"name": name, "kind": "read", "method": method, "path": path,
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{
				"actor_id": map[string]any{"type": "string"}, "agent_id": map[string]any{"type": "string"}, "course_id": map[string]any{"type": "string"}}},
			"output_schema": map[string]any{}}
	}
	catalogue, err := json.Marshal(map[string]any{"tools": []any{
		tool("agent_runtime.agent", "GET", "/v1/services/agent_runtime/agents/{agent_id}"),
		tool("agent_runtime.check_owner", "GET", "/v1/services/agent_runtime/owners/{actor_id}/agents/{agent_id}"),
		tool("agent_runtime.issue_token", "POST", "/v1/services/agent_runtime/agents/{agent_id}/token"),
		tool("agent_runtime.revoke_token", "POST", "/v1/services/agent_runtime/agents/{agent_id}/token/revoke"),
		tool("agent_runtime.check_ask", "GET", "/v1/services/agent_runtime/actors/{actor_id}/courses/{course_id}/agents/{agent_id}/may-ask"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var authOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authOK = r.Header.Get("Authorization") == "Bearer "+credential
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/tools":
			_, _ = w.Write(catalogue)
		case r.URL.Path == "/v1/services/agent_runtime/actors/"+privacyAskerID+"/courses/"+privacyCourseUUID+"/agents/"+privacyAgentID+"/may-ask":
			_, _ = fmt.Fprintln(w, `{"status":"executed","result":{"may_ask":true}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	svc := core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: srv.URL, HTTPClient: srv.Client(),
		Credential: func(context.Context) (string, error) { return credential, nil }}))
	got, err := svc.CheckAsk(t.Context(), privacyAskerID, privacyAgentID, privacyCourseUUID)
	if err != nil || got == nil || !got.MayAsk || !authOK {
		t.Fatalf("CheckAsk = %+v, %v, auth %v", got, err, authOK)
	}
}
