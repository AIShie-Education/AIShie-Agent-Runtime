package chatwire_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chatwire"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/provider"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/runner"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

func TestPrepareHostedCourseAttachmentsAndToolPolicy(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.stage(t, "image-1", "image/png", []byte{0, 1, 2, 3})
	f.stage(t, "text-1", "text/plain", []byte("reference text"))
	in := f.generation(t, "What does the attachment say?", []string{"image-1", "text-1"})

	runs, err := f.wire.Prepare(t.Context(), in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want primary and fallback", len(runs))
	}
	primary, fallback := runs[0], runs[1]
	if primary.AgentID != runtimeAgent || primary.TenantID != tenantID || primary.KeySource != config.KeyOwn {
		t.Errorf("prepared identity = %+v", primary)
	}
	if primary.Request.Model.ModelID != "gpt-course" || fallback.Request.Model.ModelID != "gpt-fallback" {
		t.Errorf("course models = %q, %q", primary.Request.Model.ModelID, fallback.Request.Model.ModelID)
	}
	for i, run := range []chat.PreparedRun{primary, fallback} {
		if run.Request.CoreToken != testToken || run.Request.Model.APIKey != testModelKey {
			t.Errorf("run %d did not open the expected credentials", i)
		}
		if !run.Request.Model.Hosted || run.Request.CoreMCPURL != f.core.URL+"/mcp" {
			t.Errorf("run %d hosted/Core URL = %+v %q", i, run.Request.Model, run.Request.CoreMCPURL)
		}
		if run.Request.Seat.SelfMemberID != selfMember || run.Request.Seat.PrincipalMemberID != openerMember ||
			run.Request.Seat.OpenerMemberID != openerMember || run.Request.Seat.ActionDecideLevel != core.LevelConfirmRequired {
			t.Errorf("run %d seat = %+v", i, run.Request.Seat)
		}
	}
	if !strings.Contains(primary.Request.SystemPrompt, "course rules") || !strings.Contains(primary.Request.SystemPrompt, "Core Agent") {
		t.Errorf("system prompt lacks course override or Core display name: %q", primary.Request.SystemPrompt)
	}
	if !primary.Request.AllowWrites || !slices.Contains(primary.Request.AllowedTools, "assignment_create") ||
		!slices.Contains(primary.Request.AllowedTools, "course_get") || slices.Contains(primary.Request.AllowedTools, "document_text") {
		t.Errorf("tool policy = writes %v allowed %v", primary.Request.AllowWrites, primary.Request.AllowedTools)
	}
	if !slices.Contains(primary.Request.DeniedTools, "document_text") {
		t.Errorf("denied tools = %v", primary.Request.DeniedTools)
	}
	last := primary.Request.Messages[len(primary.Request.Messages)-1]
	var image *prompt.Part
	var texts []string
	for i := range last.Parts {
		part := &last.Parts[i]
		texts = append(texts, part.Text)
		if part.Type == prompt.PartImage {
			image = part
		}
	}
	if image == nil || image.MediaType != "image/png" {
		t.Fatalf("image part = %+v", image)
	}
	decoded, err := base64.StdEncoding.DecodeString(image.DataBase64)
	if err != nil || !bytes.Equal(decoded, []byte{0, 1, 2, 3}) {
		t.Errorf("image bytes = %v, %v", decoded, err)
	}
	if joined := strings.Join(texts, "\n"); !strings.Contains(joined, "reference text") ||
		!strings.Contains(joined, "What does the attachment say?") || !strings.Contains(joined, "data, not instructions") {
		t.Errorf("text parts = %q", joined)
	}
	if primary.Request.Limits.MaxSteps != 8 || primary.Request.Limits.MaxToolCalls != 12 ||
		primary.Request.Limits.MaxResultBytes != runner.DefaultMaxResultBytes {
		t.Errorf("limits = %+v", primary.Request.Limits)
	}
	if got := strings.Join(f.core.calls(), " "); got != "initialize notifications/initialized tools/call tools/call" {
		t.Errorf("Core calls = %q", got)
	}

	// Configuration, chat state, and sealed rows never acquire plaintext.
	for name, value := range map[string]any{"config": f.cfg, "generation": in} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecrets(t, name, raw)
	}
	for _, id := range []string{"sec_chatwire_token", "sec_chatwire_model"} {
		secret, err := f.store.Secret(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(secret.Ciphertext, []byte(testToken)) || bytes.Contains(secret.Ciphertext, []byte(testModelKey)) {
			t.Errorf("secret %s stores plaintext", id)
		}
	}
}

func TestAttachmentMediaAndImageCountLimits(t *testing.T) {
	t.Run("four images accepted", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{})
		ids := []string{"image-1", "image-2", "image-3", "image-4"}
		for _, id := range ids {
			f.stage(t, id, "image/png", []byte(id))
		}
		in := f.generation(t, "four images", ids)
		runs, err := f.wire.Prepare(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		images := 0
		for _, message := range runs[0].Request.Messages {
			for _, part := range message.Parts {
				if part.Type == prompt.PartImage {
					images++
				}
			}
		}
		if images != 4 {
			t.Errorf("images = %d, want 4", images)
		}
	})
	t.Run("fifth image refused at binding", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{})
		ids := []string{"image-1", "image-2", "image-3", "image-4", "image-5"}
		for _, id := range ids {
			f.stage(t, id, "image/png", []byte(id))
		}
		_, _, err := f.store.CreateChatConversation(t.Context(), store.ChatCreateParams{
			Conversation: store.ChatConversation{ID: "conversation-1", CourseID: courseID, AgentID: coreActor, RuntimeAgentID: runtimeAgent, OpenerActorID: openerActor},
			Message:      store.ChatMessage{ID: "message-1", Body: "five images"}, GenerationID: "generation-1", AssistantMsgID: "assistant-1",
			AttachmentIDs: ids, IdempotencyKey: "idem-1", RequestFingerprint: "fingerprint",
		})
		if !errors.Is(err, store.ErrChatAttachments) {
			t.Fatalf("five images error = %v", err)
		}
	})
	t.Run("media byte ceilings", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{})
		for _, attachment := range []store.ChatAttachment{
			{ID: "large-image", CourseID: courseID, OwnerActorID: openerActor, Filename: "large.png", MediaType: "image/png", Bytes: make([]byte, 5<<20+1), Size: 5<<20 + 1},
			{ID: "large-text", CourseID: courseID, OwnerActorID: openerActor, Filename: "large.txt", MediaType: "text/plain", Bytes: make([]byte, 32<<10+1), Size: 32<<10 + 1},
		} {
			if _, err := f.store.StageChatAttachment(t.Context(), attachment); err == nil {
				t.Errorf("%s exceeded its ceiling but was staged", attachment.ID)
			}
		}
	})
	t.Run("invalid UTF-8 text refused by Prepare", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{})
		f.stage(t, "invalid-text", "text/plain", []byte{0xff, 0xfe})
		in := f.generation(t, "invalid text", []string{"invalid-text"})
		_, err := f.wire.Prepare(t.Context(), in)
		assertRunKind(t, err, "invalid_attachment")
		assertNoSecrets(t, "attachment error", []byte(err.Error()))
	})
}

func TestPrepareRefusesWrongActorCourseAgentAndGrant(t *testing.T) {
	t.Run("Core token names another actor", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{})
		f.core.actor.ID = "another-core-agent"
		in := f.generation(t, "question", nil)
		_, err := f.wire.Prepare(t.Context(), in)
		assertRunKind(t, err, "core_token_actor_mismatch")
		assertNoSecrets(t, "actor error", []byte(err.Error()))
	})
	t.Run("course seat does not match", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{})
		in := f.generation(t, "question", nil)
		in.Conversation.CourseID = "course-two"
		_, err := f.wire.Prepare(t.Context(), in)
		assertRunKind(t, err, "authorization_grant_incomplete")
		assertNoSecrets(t, "course error", []byte(err.Error()))
	})
	t.Run("runtime agent id does not match Core actor configuration", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{})
		in := f.generation(t, "question", nil)
		in.Conversation.RuntimeAgentID = "agt_other"
		_, err := f.wire.Prepare(t.Context(), in)
		assertRunKind(t, err, "agent_configuration_unavailable")
	})
	t.Run("required respondent grant is missing", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{})
		in := f.generation(t, "question", nil)
		in.Authorization.Grant.SelfMemberID = ""
		_, err := f.wire.Prepare(t.Context(), in)
		assertRunKind(t, err, "authorization_grant_incomplete")
	})
}

func TestRunnerGeneratorFallbackRecordsEveryCall(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	in := f.generation(t, "question", nil)
	var requests []runner.RunRequest
	fake := chat.RunnerFunc(func(_ context.Context, request runner.RunRequest, _ func(runner.Event) error) (*runner.Done, error) {
		requests = append(requests, request)
		if len(requests) == 1 {
			return &runner.Done{Usage: usage(100, 20, 3, 10, 5)}, &runner.Error{Kind: "server", Retryable: true}
		}
		return &runner.Done{Text: "safe answer", FinishReason: "stop", Usage: usage(50, 10, 2, 0, 0)}, nil
	})
	generator := chat.RunnerGenerator{Preparer: f.wire, Runner: fake, Usage: f.wire}
	result, err := generator.Generate(t.Context(), in, nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Body != "safe answer" || result.Usage == nil || result.Usage.Reasoning != 2 {
		t.Errorf("result = %+v", result)
	}
	if len(requests) != 2 || requests[0].Model.ModelID != "gpt-course" || requests[1].Model.ModelID != "gpt-fallback" {
		t.Fatalf("fallback requests = %+v", requests)
	}
	calls := f.store.modelCalls()
	if len(calls) != 2 {
		t.Fatalf("ledger calls = %d, want 2", len(calls))
	}
	first, second := calls[0], calls[1]
	if first.Stop != "server" || second.Stop != "stop" || first.ID == second.ID {
		t.Errorf("call outcomes = %q %q ids %q %q", first.Stop, second.Stop, first.ID, second.ID)
	}
	if first.Input != 100 || first.Output != 20 || first.Reasoning != 3 || first.CacheRead != 10 || first.CacheWrite != 5 ||
		second.Input != 50 || second.Output != 10 || second.Reasoning != 2 {
		t.Errorf("usage calls = %+v %+v", first, second)
	}
	if first.CostPUSD != 137_500_000 || second.CostPUSD != 70_000_000 || first.PriceVersion == "" || second.PriceVersion == "" {
		t.Errorf("costs = %d %d, versions %q %q", first.CostPUSD, second.CostPUSD, first.PriceVersion, second.PriceVersion)
	}
	for _, call := range calls {
		if call.AgentID != runtimeAgent || call.MemberID != selfMember || call.OpenerMemberID != openerMember ||
			call.KeySource != config.KeyOwn || call.Kind != store.CallAnswer || call.LatencyMS < 0 {
			t.Errorf("call metadata = %+v", call)
		}
		raw, _ := json.Marshal(call)
		assertNoSecrets(t, "ledger call", raw)
		if bytes.Contains(raw, []byte("question")) || bytes.Contains(raw, []byte("safe answer")) {
			t.Errorf("ledger stores request or response text: %s", raw)
		}
	}
	spend, err := f.store.Spend(t.Context(), store.SpendScope{AgentID: runtimeAgent}, store.Day(fixedNow, time.UTC))
	if err != nil || spend.CostPUSD != 207_500_000 {
		t.Errorf("spend = %+v, %v", spend, err)
	}
}

func TestSchoolQuotaUsesOnlyOwnerFallback(t *testing.T) {
	t.Run("school quota spent", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{school: true})
		in := f.generation(t, "question", nil)
		recordBillableAnswer(t, f, "already-spent", config.KeySchool)
		runs, err := f.wire.Prepare(t.Context(), in)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if len(runs) != 1 || runs[0].KeySource != config.KeyOwn || runs[0].Request.Model.ModelID != "gpt-fallback" ||
			runs[0].Request.Model.APIKey != testModelKey {
			t.Fatalf("quota fallback = %+v", runs)
		}
		if runs[0].Request.Model.APIKey == testSchoolKey {
			t.Fatal("school key was used after its quota was spent")
		}
	})
	t.Run("school quota available", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{school: true})
		in := f.generation(t, "question", nil)
		runs, err := f.wire.Prepare(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 2 || runs[0].KeySource != config.KeySchool || runs[0].Request.Model.APIKey != testSchoolKey ||
			runs[1].Request.Model.APIKey != testModelKey {
			t.Fatalf("school primary/fallback = %+v", runs)
		}
	})
	t.Run("agent quota is not bypassed by school fallback", func(t *testing.T) {
		f := newFixture(t, fixtureOptions{school: true})
		one := 1
		f.cfg.Agents[0].Budgets.PerAgentDay.Answers = &one
		in := f.generation(t, "question", nil)
		recordBillableAnswer(t, f, "agent-spent", config.KeySchool)
		_, err := f.wire.Prepare(t.Context(), in)
		assertRunKind(t, err, "quota_exhausted")
	})
}

func TestCandidatesMetadataAndFailures(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	if err := f.store.SeatSeen(t.Context(), store.SeatRef{AgentID: runtimeAgent, MemberID: selfMember, CourseID: courseID,
		Status: "active", CourseStatus: "active", AnswersCourse: true, SeenAt: fixedNow,
		Perms: map[string]string{"conversation_answer": core.LevelAutonomous}}); err != nil {
		t.Fatal(err)
	}
	candidates, err := f.wire.ChatCandidates(t.Context(), courseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].AgentID != coreActor || candidates[0].DisplayName != "Configured Agent" ||
		!candidates[0].AnswersCourse || candidates[0].AnswerLevel != core.LevelAutonomous {
		t.Fatalf("candidates = %+v", candidates)
	}

	f.store.hostedErr = errors.New("hosted read failed")
	if got, err := f.wire.ChatCandidates(t.Context(), courseID); err == nil || len(got) != 0 {
		t.Fatalf("HostedAgents error became empty success: %+v, %v", got, err)
	}
	f.store.hostedErr = nil
	f.store.seatsErr = errors.New("seat read failed")
	if got, err := f.wire.ChatCandidates(t.Context(), courseID); err == nil || len(got) != 0 {
		t.Fatalf("KnownSeats error became empty success: %+v, %v", got, err)
	}
}

func TestUsageSitePriceFailureAndSecretFreeLog(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.cfg.Runtime.Site = config.Site{PricesChanged: fixedNow, Prices: []pricing.Row{{
		ID: "override", Provider: "openai", Model: "gpt-course", From: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		In: 2_000_000, CacheRead: 1_000_000, CacheWrite: 3_000_000, Out: 4_000_000,
	}}}
	in := f.generation(t, "question", nil)
	prepared := chat.PreparedRun{
		// These deliberately live only in the input request. The recorder must
		// never inspect or serialize them.
		Request:  runner.RunRequest{CoreToken: testToken, Model: runner.Model{APIKey: testModelKey}},
		TenantID: tenantID, AgentID: runtimeAgent, KeySource: config.KeyOwn,
		Adapter: "openai_chat", Provider: "openai", Model: "gpt-course",
	}
	// PriceTable takes precedence and supplies the complete current table. With
	// no file Prices fallback, this assertion proves the new closure is used.
	currentTable := f.cfg.Runtime.Site.PriceTable(nil)
	dynamic, err := chatwire.New(chatwire.Options{Config: func() *config.Config { return f.cfg }, Store: f.store, Secrets: f.resolver,
		PriceTable: func() *pricing.Table { return currentTable }, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	dynamic.RecordModelCall(t.Context(), in, prepared, &runner.Done{FinishReason: "stop", Usage: usage(10, 5, 1, 2, 1)}, "", 3*time.Millisecond)
	f.wire.RecordModelCall(t.Context(), in, prepared, nil, "rate_limited", 4*time.Millisecond)
	calls := f.store.modelCalls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].CostPUSD != 39_000_000 || calls[0].PriceVersion != pricing.SiteVersion(fixedNow)+"/override" {
		t.Errorf("site-priced call = %+v", calls[0])
	}
	if calls[1].Stop != "rate_limited" || calls[1].CostPUSD != 0 || calls[1].LatencyMS != 4 || len(calls[1].RawUsage) != 0 {
		t.Errorf("failed call = %+v", calls[1])
	}
	f.store.recordErr = errors.New("store unavailable")
	f.wire.RecordModelCall(t.Context(), in, prepared, nil, "timeout", time.Millisecond)
	assertNoSecrets(t, "usage log", f.logs.Bytes())
	if !strings.Contains(f.logs.String(), "chat model call was not recorded") {
		t.Errorf("ledger failure was not reported: %q", f.logs.String())
	}
}

func TestOperatorModelOpenRouterHeadersRegionAndKeys(t *testing.T) {
	ctx := t.Context()
	st := &captureStore{Store: memstore.New()}
	operatorVault := testVault(t)
	tokenSecret := sealOperatorToken(t, operatorVault)
	if err := st.PutAgentToken(ctx, store.AgentToken{AgentID: "operator-agent", CoreActorID: coreActor, SecretID: tokenSecret.ID}, tokenSecret, ""); err != nil {
		t.Fatal(err)
	}
	agent := &config.Agent{
		ID: "operator-agent", DisplayName: "Operator", TenantID: "operator",
		Core: config.Core{BaseURL: "https://core.example", AgentID: coreActor, Transport: config.TransportMCP},
		Model: config.Model{Adapter: "openai_chat", Provider: "openrouter", Model: "openai/gpt-test",
			BaseURL: "https://openrouter.ai/api/v1", KeyRef: "env://PRIMARY_KEY", KeySource: config.KeyOwn,
			Region: "region-one", Headers: map[string]string{"X-Test": "header-value"},
			OpenRouter: &openrouter.Routing{Order: []string{"openai"}}, Params: config.ModelParams{MaxOutputTokens: 123},
			Fallback: &config.Model{Adapter: "openai_chat", Model: "fallback", KeyRef: "env://FALLBACK_KEY", KeySource: config.KeyOwn}},
		Prompt: config.Prompt{SystemText: "operator prompt"}, Tools: config.Tools{Mode: config.ToolsNone},
	}
	cfg := &config.Config{Agents: []*config.Agent{agent}}
	resolver := secrets.Resolver{
		Getenv: func(name string) string {
			return map[string]string{"PRIMARY_KEY": "primary-secret", "FALLBACK_KEY": "fallback-secret"}[name]
		},
		Sealed: vault.Opener{Vault: operatorVault, Store: st},
	}
	if err := st.SeatSeen(ctx, store.SeatRef{AgentID: "operator-agent", MemberID: selfMember, CourseID: courseID,
		Status: core.StatusActive, CourseStatus: "active", PrincipalMemberID: openerMember, SeenAt: fixedNow,
		Perms: map[string]string{"conversation_answer": core.LevelAutonomous, "action_decide": core.LevelConfirmRequired}}); err != nil {
		t.Fatal(err)
	}
	inspector := chatwire.CoreInspectorFunc(func(_ context.Context, _ config.Core, token string) (*chatwire.CoreIdentity, error) {
		if token != testToken {
			return nil, errors.New("wrong token")
		}
		// HasMemberships false deliberately exercises the KnownSeats fallback.
		return &chatwire.CoreIdentity{Actor: core.Actor{ID: coreActor, Kind: core.KindAgent, Status: core.StatusActive}}, nil
	})
	w, err := chatwire.New(chatwire.Options{Config: func() *config.Config { return cfg }, Store: st, Secrets: resolver, Core: inspector,
		Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	in := chat.GenerationInput{
		Conversation:  store.ChatConversation{ID: "conversation", CourseID: courseID, AgentID: coreActor, RuntimeAgentID: "operator-agent", OpenerActorID: openerActor},
		Messages:      []store.ChatMessage{{ID: "message", Role: store.ChatRoleUser, Status: store.ChatPosted, Body: "question"}},
		Generation:    store.ChatGeneration{ID: "generation", ConversationID: "conversation", UserMessageID: "message", Attempt: 1},
		Authorization: (&fixture{}).authorization(),
	}
	runs, err := w.Prepare(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Request.Model.APIKey != "primary-secret" || runs[1].Request.Model.APIKey != "fallback-secret" {
		t.Fatalf("operator model keys = %+v", runs)
	}
	model := runs[0].Request.Model
	if model.Hosted || model.Region != "region-one" || model.Headers["X-Test"] != "header-value" ||
		model.OpenRouter == nil || !slices.Equal(model.OpenRouter.Order, []string{"openai"}) || model.BaseURL == nil ||
		*model.BaseURL != "https://openrouter.ai/api/v1" || model.MaxOutputTokens != 123 {
		t.Errorf("operator model = %+v", model)
	}
	if len(runs[0].Request.AllowedTools) != 0 || runs[0].Request.AllowWrites {
		t.Errorf("tools.none was not enforced: %+v", runs[0].Request)
	}

	called := false
	memoryWire, err := chatwire.New(chatwire.Options{Config: func() *config.Config { return cfg }, Store: st, Secrets: resolver, Core: inspector,
		Now: func() time.Time { return fixedNow }, OperatorToken: func(_ context.Context, got *config.Agent) (string, bool, error) {
			called = true
			if got != agent {
				t.Errorf("token reader agent = %p, want %p", got, agent)
			}
			return testToken, true, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	memoryRuns, err := memoryWire.Prepare(ctx, in)
	if err != nil || !called || len(memoryRuns) != 2 || memoryRuns[0].Request.CoreToken != testToken {
		t.Errorf("in-memory operator token: called %v runs %d, err %v", called, len(memoryRuns), err)
	}
}

func usage(input, output, reasoning, cacheRead, cacheWrite int64) (u provider.Usage) {
	u.Input, u.Output, u.Reasoning, u.CacheRead, u.CacheWrite = input, output, reasoning, cacheRead, cacheWrite
	return
}

func recordBillableAnswer(t *testing.T, f *fixture, id, keySource string) {
	t.Helper()
	if err := f.store.RecordAnswer(t.Context(), store.AnswerRecord{ID: id, At: fixedNow, TenantID: tenantID, AgentID: runtimeAgent,
		CourseID: courseID, OpenerMemberID: openerMember, Outcome: store.OutcomePosted, Billable: true, KeySource: keySource}); err != nil {
		t.Fatal(err)
	}
}

func assertRunKind(t *testing.T, err error, kind string) {
	t.Helper()
	var runErr *chat.RunError
	if !errors.As(err, &runErr) || runErr.Kind != kind {
		t.Fatalf("error = %T %v, want chat.RunError kind %q", err, err, kind)
	}
}

func assertNoSecrets(t *testing.T, what string, raw []byte) {
	t.Helper()
	for _, secret := range []string{testToken, testModelKey, testSchoolKey, "primary-secret", "fallback-secret"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("%s contains a secret", what)
		}
	}
}

func testVault(t *testing.T) *vault.Vault {
	t.Helper()
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	if err := os.WriteFile(filepath.Join(dir, "v1"), []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open("local:" + filepath.Join(dir, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func sealOperatorToken(t *testing.T, v *vault.Vault) store.Secret {
	t.Helper()
	secret, err := v.Seal(t.Context(), store.Secret{ID: "sec_operator_token", TenantID: "operator", Kind: store.SecretCoreToken}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}
