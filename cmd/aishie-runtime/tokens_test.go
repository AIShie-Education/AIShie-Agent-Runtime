package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

const (
	tokenAgent = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a11"
	tokenOwner = "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a12"
)

type tokenCore struct {
	mu        sync.Mutex
	issued    int
	revoked   []string
	catalogue []byte
}

func newTokenCore(t *testing.T) (*tokenCore, *httptest.Server) {
	t.Helper()
	raw, err := os.ReadFile("../../internal/core/testdata/catalogue.json") // #nosec G304 -- repository test fixture.
	if err != nil {
		t.Fatal(err)
	}
	fc := &tokenCore{catalogue: raw}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/tools":
			_, _ = w.Write(fc.catalogue)
		case strings.HasSuffix(r.URL.Path, "/token/revoke"):
			fc.mu.Lock()
			fc.revoked = append(fc.revoked, r.URL.Path)
			fc.mu.Unlock()
			writeTokenEnvelope(t, w, map[string]any{"revoked": []string{}})
		case strings.HasSuffix(r.URL.Path, "/token"):
			fc.mu.Lock()
			fc.issued++
			n := fc.issued
			fc.mu.Unlock()
			writeTokenEnvelope(t, w, map[string]any{"agent_id": tokenAgent, "credential_id": fmt.Sprintf("cred_%d", n),
				"token": fmt.Sprintf("ais_test_token_%d", n), "token_prefix": "ais_test_"})
		case strings.Contains(r.URL.Path, "/services/agent_runtime/agents/"):
			writeTokenEnvelope(t, w, map[string]any{"agent_id": tokenAgent, "display_name": "Agent", "hosting": core.HostingRuntime,
				"status": core.StatusActive, "owner_actor_id": tokenOwner, "owner_status": core.StatusActive, "hostable": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return fc, srv
}

func writeTokenEnvelope(t *testing.T, w http.ResponseWriter, result any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"status": "executed", "result": result}); err != nil {
		t.Error(err)
	}
}

func tokenTestVault(t *testing.T) *vault.Vault {
	t.Helper()
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "v1"), []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open("local:" + filepath.Join(dir, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func tokenTestAgent(base string) *config.Agent {
	return &config.Agent{ID: "yaml-agent", Core: config.Core{BaseURL: base, AgentID: tokenAgent, Transport: config.TransportMCP}}
}

func newTokenTestManager(st store.Store, v *vault.Vault, srv *httptest.Server) *tokenManager {
	res := secrets.Resolver{}
	if v != nil {
		res.Sealed = vault.Opener{Vault: v, Store: st}
	}
	m := newTokenManager(st, res, v, func(context.Context) (string, error) { return "aissvc_test", nil }, srv.Client(), nil, nil, "test-worker")
	m.inspect = func(_ context.Context, a *config.Agent, _ string) (*core.Actor, error) {
		return &core.Actor{ID: a.Core.AgentID, Kind: core.KindAgent, Status: core.StatusActive, Hosting: core.HostingRuntime,
			OwnerActorID: tokenOwner}, nil
	}
	return m
}

func TestTokenManagerSealsAndReusesYAMLTokens(t *testing.T) {
	fc, srv := newTokenCore(t)
	st := memstore.New()
	v := tokenTestVault(t)
	m := newTokenTestManager(st, v, srv)
	a := tokenTestAgent(srv.URL)
	cfg := &config.Config{Agents: []*config.Agent{a}}
	if err := m.Sync(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	held, err := st.AgentToken(t.Context(), a.ID)
	if err != nil || held == nil || held.CoreActorID != tokenAgent {
		t.Fatalf("AgentToken = %+v, %v", held, err)
	}
	got, err := m.secrets.Resolve(t.Context(), secrets.SchemeSealed+held.SecretID, "")
	if err != nil || got != "ais_test_token_1" {
		t.Fatalf("token = %q, %v", got, err)
	}
	if err := m.Sync(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	issued := fc.issued
	fc.mu.Unlock()
	if issued != 1 {
		t.Fatalf("issued = %d, want one", issued)
	}
	m.Close()
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.revoked) != 0 {
		t.Errorf("ordinary shutdown revoked tokens: %v", fc.revoked)
	}
}

func TestTokenManagerHostedIssueAndPause(t *testing.T) {
	fc, srv := newTokenCore(t)
	st := memstore.New()
	v := tokenTestVault(t)
	_, err := st.CreateHostedAgent(t.Context(), store.HostedAgent{ID: "agt_hosted", CoreActorID: tokenAgent,
		OwnerActorID: tokenOwner, TenantID: "ten_owner", Settings: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	m := newTokenTestManager(st, v, srv)
	a := &config.Agent{ID: "agt_hosted", Core: config.Core{BaseURL: srv.URL, AgentID: tokenAgent},
		Hosted: &config.Hosted{CoreActorID: tokenAgent, OwnerActorID: tokenOwner}}
	if err := m.Sync(t.Context(), &config.Config{Agents: []*config.Agent{a}}); err != nil {
		t.Fatal(err)
	}
	row, err := st.HostedAgent(t.Context(), a.ID)
	if err != nil || !row.TokenIssued || row.TokenSecretID == "" || !row.OwnerVerified {
		t.Fatalf("hosted row = %+v, %v", row, err)
	}
	got, err := m.secrets.Resolve(t.Context(), secrets.SchemeSealed+row.TokenSecretID, "")
	if err != nil || got != "ais_test_token_1" {
		t.Fatalf("hosted token = %q, %v", got, err)
	}
	a.Paused = true
	if err := m.Sync(t.Context(), &config.Config{Agents: []*config.Agent{a}}); err != nil {
		t.Fatal(err)
	}
	row, err = st.HostedAgent(t.Context(), a.ID)
	if err != nil || row.TokenSecretID != "" || row.TokenIssued {
		t.Errorf("after pause = %+v, %v", row, err)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.revoked) != 1 {
		t.Errorf("pause revocations = %v", fc.revoked)
	}
}

func TestTokenManagerMemoryFallbackAndActorRetry(t *testing.T) {
	fc, srv := newTokenCore(t)
	st := memstore.New()
	m := newTokenTestManager(st, nil, srv)
	a := tokenTestAgent(srv.URL)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	m.inspect = func(context.Context, *config.Agent, string) (*core.Actor, error) {
		return &core.Actor{ID: "another-actor", Kind: core.KindAgent}, nil
	}
	if err := m.Sync(t.Context(), &config.Config{Agents: []*config.Agent{a}}); err == nil {
		t.Fatal("an actor mismatch was accepted")
	}
	m.inspect = func(_ context.Context, a *config.Agent, _ string) (*core.Actor, error) {
		return &core.Actor{ID: a.Core.AgentID, Kind: core.KindAgent, Status: core.StatusActive, Hosting: core.HostingRuntime}, nil
	}
	now = now.Add(2 * time.Second)
	if err := m.Sync(t.Context(), &config.Config{Agents: []*config.Agent{a}}); err != nil {
		t.Fatal(err)
	}
	got, found, err := m.OperatorToken(t.Context(), a)
	if err != nil || !found || got != "ais_test_token_2" {
		t.Fatalf("OperatorToken = %q, %v, %v", got, found, err)
	}
	if _, err := st.AgentToken(t.Context(), a.ID); err == nil {
		t.Error("memory fallback unexpectedly wrote a sealed token")
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.issued != 2 || len(fc.revoked) == 0 {
		t.Errorf("issued %d, revoked %v; want retry and cleanup", fc.issued, fc.revoked)
	}
}

func TestTokenManagerDoesNotHostWithoutCredential(t *testing.T) {
	_, srv := newTokenCore(t)
	st := memstore.New()
	m := newTokenManager(st, secrets.Resolver{}, nil, func(context.Context) (string, error) { return "", errors.New("missing") }, srv.Client(), nil, nil, "test-worker")
	a := tokenTestAgent(srv.URL)
	if err := m.Sync(t.Context(), &config.Config{Agents: []*config.Agent{a}}); err == nil {
		t.Fatal("missing runtime credential was accepted")
	}
}
