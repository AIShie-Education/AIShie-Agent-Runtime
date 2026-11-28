package chatwire_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chatwire"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

const (
	testToken     = "ais_ChatwireSecretToken_0123456789"
	testModelKey  = "sk-chatwire-owner-key-0123456789"
	testSchoolKey = "sk-chatwire-school-key-0123456789"
	coreActor     = "11111111-1111-1111-1111-111111111111"
	runtimeAgent  = "agt_chatwire"
	ownerActor    = "owner-actor"
	tenantID      = "ten_owner"
	courseID      = "22222222-2222-2222-2222-222222222222"
	openerActor   = "opener-actor"
	selfMember    = "member-agent"
	openerMember  = "member-owner"
)

var fixedNow = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

type captureStore struct {
	*memstore.Store
	mu        sync.Mutex
	calls     []store.LLMCall
	hostedErr error
	seatsErr  error
	recordErr error
}

func (s *captureStore) RecordLLMCall(ctx context.Context, call store.LLMCall) error {
	s.mu.Lock()
	if s.recordErr != nil {
		err := s.recordErr
		s.mu.Unlock()
		return err
	}
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	return s.Store.RecordLLMCall(ctx, call)
}

func (s *captureStore) HostedAgents(ctx context.Context) ([]store.HostedAgent, error) {
	if s.hostedErr != nil {
		return nil, s.hostedErr
	}
	return s.Store.HostedAgents(ctx)
}

func (s *captureStore) KnownSeats(ctx context.Context, agentID string) ([]store.SeatRef, error) {
	if s.seatsErr != nil {
		return nil, s.seatsErr
	}
	return s.Store.KnownSeats(ctx, agentID)
}

func (s *captureStore) modelCalls() []store.LLMCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.LLMCall(nil), s.calls...)
}

type coreStub struct {
	*httptest.Server
	actor       core.Actor
	memberships []core.Membership
	token       string

	mu      sync.Mutex
	methods []string
}

func newCoreStub(t *testing.T) *coreStub {
	principal := openerMember
	f := &coreStub{
		token: testToken,
		actor: core.Actor{ID: coreActor, Kind: core.KindAgent, DisplayName: "Core Agent", Status: core.StatusActive,
			OwnerActorID: ownerActor, Hosting: core.HostingRuntime, TimeZone: "UTC"},
		memberships: []core.Membership{{
			MemberID: selfMember, CourseID: courseID, Code: "CS101", Section: "A", Title: "Computing",
			Status: core.StatusActive, CourseStatus: "active", PrincipalMemberID: &principal, AnswersCourse: false,
			Perms: map[string]string{
				"conversation_answer": core.LevelAutonomous, "action_decide": core.LevelConfirmRequired,
				"document_read": core.LevelAutonomous, "assignment_write": core.LevelAutonomous,
				"member_read": core.LevelDenied, "submission_read": core.LevelDenied, "grade_read": core.LevelDenied,
			},
		}},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *coreStub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &request)
	f.mu.Lock()
	f.methods = append(f.methods, request.Method)
	f.mu.Unlock()
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		writeRPCResult(w, request.ID, map[string]any{"protocolVersion": params.ProtocolVersion, "capabilities": map[string]any{}})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(request.Params, &params)
		var result any
		switch params.Name {
		case "me_get":
			result = f.actor
		case "me_memberships":
			result = map[string]any{"memberships": f.memberships}
		default:
			http.Error(w, "unknown tool", http.StatusBadRequest)
			return
		}
		writeRPCResult(w, request.ID, map[string]any{"structuredContent": map[string]any{"status": "executed", "result": result}})
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *coreStub) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

type fixtureOptions struct {
	settings       json.RawMessage
	courseSettings json.RawMessage
	runtime        config.Runtime
	school         bool
}

type fixture struct {
	store    *captureStore
	core     *coreStub
	cfg      *config.Config
	wire     *chatwire.Wiring
	resolver secrets.Resolver
	vault    *vault.Vault
	logs     bytes.Buffer
	seq      atomic.Int64
}

func newFixture(t *testing.T, options fixtureOptions) *fixture {
	t.Helper()
	ctx := t.Context()
	st := &captureStore{Store: memstore.New()}
	st.SetClock(func() time.Time { return fixedNow })
	coreServer := newCoreStub(t)

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
	seal := func(id, kind, plaintext string) store.Secret {
		sealed, err := v.Seal(ctx, store.Secret{ID: id, TenantID: tenantID, Kind: kind}, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}
	tokenSecret := seal("sec_chatwire_token", store.SecretCoreToken, testToken)
	keySecret := seal("sec_chatwire_model", store.SecretModelKey, testModelKey)

	settings := options.settings
	if len(settings) == 0 {
		if options.school {
			settings = json.RawMessage(`{"model":{"key_source":"school","offer":"basic","fallback":{"adapter":"openai_chat","model":"gpt-fallback","key_source":"own"}},"prompt":{"system_text":"Base {{agent}}"},"tools":{"writes":true}}`)
		} else {
			settings = json.RawMessage(`{"model":{"adapter":"openai_chat","model":"gpt-primary","key_source":"own","fallback":{"adapter":"openai_chat","model":"gpt-fallback","key_source":"own"}},"prompt":{"system_text":"Base {{agent}}"},"tools":{"writes":true}}`)
		}
	}
	row := store.HostedAgent{
		ID: runtimeAgent, CoreActorID: coreActor, OwnerActorID: ownerActor, OwnerVerified: true, TenantID: tenantID,
		DisplayName: "Configured Agent", TokenSecretID: tokenSecret.ID, TokenIssued: true, TokenCredentialID: "cred-chatwire",
		KeySecretID: keySecret.ID, KeyProvider: "openai", Settings: settings,
	}
	if _, err := st.CreateHostedAgent(ctx, row, tokenSecret, keySecret); err != nil {
		t.Fatalf("CreateHostedAgent: %v", err)
	}
	courseSettings := options.courseSettings
	if len(courseSettings) == 0 && !options.school {
		courseSettings = json.RawMessage(`{"model":{"model":"gpt-course"},"prompt_append_text":"course rules","tools":{"deny":["document_text"]}}`)
	}
	if len(courseSettings) > 0 {
		if err := st.PutHostedCourse(ctx, store.HostedCourse{AgentID: runtimeAgent, CourseID: courseID, Settings: courseSettings}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := options.runtime
	if options.school {
		one := 1
		runtime.School = config.School{
			Offers: []config.SchoolOffer{{ID: "basic", Label: "Basic", Adapter: "openai_chat", Model: "gpt-school",
				KeyRef: "secret://school/keys/basic"}},
			PerOwnerDay: config.Quota{Answers: &one},
		}
	}
	base := &config.Config{Runtime: runtime}
	cfg, _, err := registry.Build(ctx, base, st, registry.Options{CoreBaseURL: coreServer.URL})
	if err != nil {
		t.Fatalf("registry.Build: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("agents = %d, rejected = %+v", len(cfg.Agents), cfg.Rejected)
	}
	prices, err := pricing.Parse([]byte(`version: "test"
prices:
  - provider: openai
    model: "gpt-*"
    from: 2020-01-01
    usd_per_mtok: {input: 1, cache_read: 0.5, cache_write: 1.5, output: 2}
`))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: st, core: coreServer, cfg: cfg, vault: v}
	resolver := secrets.Resolver{
		Getenv: func(name string) string {
			if name == secrets.EnvName("school/keys/basic") {
				return testSchoolKey
			}
			return ""
		},
		Sealed: vault.Opener{Vault: v, Store: st},
	}
	f.resolver = resolver
	w, err := chatwire.New(chatwire.Options{
		Config: func() *config.Config { return cfg }, Store: st, Secrets: resolver,
		Now: func() time.Time { return fixedNow }, NewID: func() string { return fmt.Sprintf("call-%d", f.seq.Add(1)) },
		Prices: prices, Location: time.UTC, Log: slog.New(slog.NewTextHandler(&f.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.wire = w
	return f
}

func (f *fixture) authorization() *chat.Authorization {
	return &chat.Authorization{
		Agent: chat.Agent{AgentID: coreActor, DisplayName: "Core Agent"},
		Grant: chat.Grant{
			SelfMemberID: selfMember, PrincipalMemberID: openerMember, OpenerMemberID: openerMember,
			ActionDecideLevel: core.LevelConfirmRequired, ConversationAskLevel: core.LevelAutonomous,
			ConversationAnswerLevel: core.LevelAutonomous, AnswersCourseKnown: true, AnswersCourse: false,
		},
		GrantPresent: true,
	}
}

func (f *fixture) generation(t *testing.T, body string, attachmentIDs []string) chat.GenerationInput {
	t.Helper()
	conversation := store.ChatConversation{ID: "conversation-1", CourseID: courseID, AgentID: coreActor,
		RuntimeAgentID: runtimeAgent, OpenerActorID: openerActor, Title: "Question"}
	snapshot, _, err := f.store.CreateChatConversation(t.Context(), store.ChatCreateParams{
		Conversation: conversation, Message: store.ChatMessage{ID: "message-1", Body: body}, GenerationID: "generation-1",
		AssistantMsgID: "assistant-1", AttachmentIDs: attachmentIDs, IdempotencyKey: "idem-1", RequestFingerprint: "fingerprint",
	})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := f.store.ChatGeneration(t.Context(), "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	generation.Attempt = 1
	return chat.GenerationInput{Conversation: snapshot.Conversation, Messages: snapshot.Messages, Generation: generation,
		Authorization: f.authorization()}
}

func (f *fixture) stage(t *testing.T, id, media string, data []byte) {
	t.Helper()
	_, err := f.store.StageChatAttachment(t.Context(), store.ChatAttachment{ID: id, CourseID: courseID, OwnerActorID: openerActor,
		Filename: id + ".dat", MediaType: media, Size: int64(len(data)), Bytes: data, CreatedAt: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
}
