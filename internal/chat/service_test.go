package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/provider"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/runner"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

func completeAuthorization() *Authorization {
	return &Authorization{Agent: Agent{AgentID: "agent", AnswerLevel: "autonomous", AnswersCourse: true}, GrantPresent: true,
		Grant: Grant{SelfMemberID: "respondent", OpenerMemberID: "opener", ActionDecideLevel: "denied",
			ConversationAskLevel: "autonomous", ConversationAnswerLevel: "autonomous", AnswersCourse: true, AnswersCourseKnown: true}}
}

type testAccess struct {
	mu   sync.Mutex
	auth *Authorization
	err  error
}

func (a *testAccess) Check(context.Context, string, string, string) (*Authorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return nil, a.err
	}
	if a.auth == nil {
		return nil, ErrAccessDenied
	}
	v := *a.auth
	return &v, nil
}
func (a *testAccess) List(context.Context, string, string) ([]Agent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return nil, a.err
	}
	if a.auth == nil {
		return []Agent{}, nil
	}
	return []Agent{a.auth.Agent}, nil
}

func testService(t *testing.T, access Access, generator Generator) *Service {
	t.Helper()
	var n atomic.Int64
	s, err := NewService(Options{Store: memstore.New(), Access: access, Generator: generator,
		NewID: func() string { return fmt.Sprintf("id-%d", n.Add(1)) }, DraftEvery: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestServiceCreateGeneratesSafePersistedAnswer(t *testing.T) {
	generator := GeneratorFunc(func(_ context.Context, in GenerationInput, sink DraftSink) (store.ChatResult, error) {
		if in.Generation.Fence != 1 || in.Generation.Holder == "" {
			t.Errorf("generation fence = %+v", in.Generation)
		}
		sink.Set("working", []store.ChatStep{{Kind: "thinking", State: "running"}})
		stated := true
		return store.ChatResult{Body: "safe [x](https://example.com/?secret=value)", SourcesStated: &stated,
			Usage: &store.ChatUsage{Input: 4, Output: 2}, FinishReason: "stop"}, nil
	})
	s := testService(t, &testAccess{auth: completeAuthorization()}, generator)
	created, err := s.Create(t.Context(), "ACTOR", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Message.Role != store.ChatRoleUser || created.Conversation.AgentID != "agent" {
		t.Fatalf("create = %+v", created)
	}
	waitFor(t, func() bool {
		c, err := s.Get(t.Context(), "actor", "course", created.Conversation.ID)
		return err == nil && c.State == store.ChatAnswered
	})
	snap, err := s.Messages(t.Context(), "actor", "course", created.Conversation.ID, 50, 0)
	if err != nil || len(snap.Messages) != 2 || snap.Draft != nil {
		t.Fatalf("snapshot = %+v %v", snap, err)
	}
	if strings.Contains(snap.Messages[1].Body, "secret=value") || snap.Messages[1].Usage.Output != 2 {
		t.Fatalf("unsafe answer = %+v", snap.Messages[1])
	}
	if other, err := s.Messages(t.Context(), "other", "course", created.Conversation.ID, 50, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner = %v %+v", err, other)
	}
}

func TestServiceFailsClosedOnIncompleteGrant(t *testing.T) {
	for _, edit := range []func(*Authorization){
		func(a *Authorization) { a.GrantPresent = false },
		func(a *Authorization) { a.Grant.OpenerMemberID = "" },
		func(a *Authorization) { a.Grant.AnswersCourseKnown = false },
	} {
		auth := completeAuthorization()
		edit(auth)
		s := testService(t, &testAccess{auth: auth}, GeneratorFunc(func(context.Context, GenerationInput, DraftSink) (store.ChatResult, error) {
			t.Error("generator ran for an incomplete grant")
			return store.ChatResult{}, nil
		}))
		_, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "key"})
		if err == nil {
			t.Fatal("create accepted an incomplete grant")
		}
	}
}

func TestServiceQuestionProposalIsSuccessfulWithoutGenerating(t *testing.T) {
	auth := completeAuthorization()
	auth.Grant.ConversationAskLevel = "confirm_required"
	var generated atomic.Int64
	s := testService(t, &testAccess{auth: auth}, GeneratorFunc(func(context.Context, GenerationInput, DraftSink) (store.ChatResult, error) {
		generated.Add(1)
		return store.ChatResult{Body: "must not run"}, nil
	}))
	created, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Submission == nil || created.Submission.Status != store.ChatSubmissionProposed || created.Submission.ApprovalID == "" ||
		created.Submission.ReviewState == nil || *created.Submission.ReviewState != store.ChatApprovalPending ||
		created.Conversation.State != store.ChatReplyPendingApproval || created.Message.Status != store.ChatPending {
		t.Fatalf("proposal = %+v", created)
	}
	replayed, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "key"})
	if err != nil || replayed.Submission == nil || replayed.Submission.ApprovalID != created.Submission.ApprovalID {
		t.Fatalf("proposal replay = %+v, %v", replayed.Submission, err)
	}
	if generated.Load() != 0 {
		t.Fatal("a pending question invoked the model")
	}
}

func TestServicePendingReviewQuestionExecutesWithReviewRecord(t *testing.T) {
	auth := completeAuthorization()
	auth.Grant.ConversationAskLevel = "pending_review"
	s := testService(t, &testAccess{auth: auth}, GeneratorFunc(func(context.Context, GenerationInput, DraftSink) (store.ChatResult, error) {
		return store.ChatResult{Body: "answer"}, nil
	}))
	created, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Submission == nil || created.Submission.Status != store.ChatSubmissionExecuted || created.Submission.ReviewState == nil ||
		*created.Submission.ReviewState != store.ChatApprovalPending {
		t.Fatalf("post-review submission = %+v", created.Submission)
	}
	waitFor(t, func() bool {
		c, err := s.Get(t.Context(), "actor", "course", created.Conversation.ID)
		return err == nil && c.State == store.ChatAnswered
	})
}

func TestServiceAnswerProposalWithholdsBodyFromOpener(t *testing.T) {
	auth := completeAuthorization()
	auth.Grant.ConversationAnswerLevel = "confirm_required"
	s := testService(t, &testAccess{auth: auth}, GeneratorFunc(func(context.Context, GenerationInput, DraftSink) (store.ChatResult, error) {
		return store.ChatResult{Body: "not yet public"}, nil
	}))
	created, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		snap, err := s.Messages(t.Context(), "actor", "course", created.Conversation.ID, 50, 0)
		return err == nil && snap.Conversation.State == store.ChatReplyPendingApproval
	})
	snap, err := s.Messages(t.Context(), "actor", "course", created.Conversation.ID, 50, 0)
	if err != nil || len(snap.Messages) != 2 {
		t.Fatalf("pending answer snapshot = %+v, %v", snap, err)
	}
	answer := snap.Messages[1]
	if answer.Status != store.ChatPending || answer.Body != "" || strings.Contains(fmt.Sprint(snap), "not yet public") {
		t.Fatalf("opener can read a pending answer: %+v", answer)
	}
}

func TestWithdrawalAbortsRunnerAndClearsDraft(t *testing.T) {
	started := make(chan struct{})
	generator := GeneratorFunc(func(ctx context.Context, _ GenerationInput, sink DraftSink) (store.ChatResult, error) {
		sink.Set("partial", nil)
		close(started)
		<-ctx.Done()
		return store.ChatResult{}, ctx.Err()
	})
	s := testService(t, &testAccess{auth: completeAuthorization()}, generator)
	created, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	withdrawn, err := s.Withdraw(t.Context(), "actor", "course", created.Conversation.ID, created.Message.ID)
	if err != nil || withdrawn.Retracted.Body != "" || withdrawn.Draft != nil {
		t.Fatalf("withdraw = %+v %v", withdrawn, err)
	}
	waitFor(t, func() bool {
		snap, _ := s.Messages(t.Context(), "actor", "course", created.Conversation.ID, 50, 0)
		return snap.Conversation.State == store.ChatAnswered && snap.Draft == nil
	})
}

func TestStageAttachmentIdempotency(t *testing.T) {
	s := testService(t, &testAccess{auth: completeAuthorization()}, nil)
	a, err := s.StageAttachment(t.Context(), "actor", "course", "../a.txt", "text/plain", []byte("one"), "upload-key")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.StageAttachment(t.Context(), "actor", "course", "../a.txt", "text/plain", []byte("one"), "upload-key")
	if err != nil || a.ID != b.ID || a.Filename != "a.txt" {
		t.Fatalf("replay = %+v %+v %v", a, b, err)
	}
	if _, err := s.StageAttachment(t.Context(), "actor", "course", "../a.txt", "text/plain", []byte("two"), "upload-key"); err == nil {
		t.Fatal("same key with different bytes was accepted")
	}
}

func TestCoreAccessUsesFreshGrantMetadata(t *testing.T) {
	truth := true
	client := checkAskFunc(func(context.Context, string, string, string) (*core.RuntimeCheckAskResult, error) {
		return &core.RuntimeCheckAskResult{MayAsk: true, Grant: &core.RuntimeAskGrant{OpenerMemberID: "open", RespondentMemberID: "self",
			ConversationAskLevel: "autonomous", ActionDecideLevel: "denied", ConversationAnswerLevel: "autonomous", AnswersCourse: &truth}}, nil
	})
	access := CoreAccess{Core: client}
	auth, err := access.Check(t.Context(), "actor", "course", "agent")
	if err != nil || !auth.Grant.AnswersCourse || auth.Grant.ConversationAskLevel != "autonomous" || auth.Agent.AnswerLevel != "autonomous" {
		t.Fatalf("auth = %+v %v", auth, err)
	}
}

type checkAskFunc func(context.Context, string, string, string) (*core.RuntimeCheckAskResult, error)

func (f checkAskFunc) CheckAsk(ctx context.Context, actor, agent, course string) (*core.RuntimeCheckAskResult, error) {
	return f(ctx, actor, agent, course)
}

type draftCapture struct {
	mu   sync.Mutex
	text string
}

func (d *draftCapture) Set(text string, _ []store.ChatStep) {
	d.mu.Lock()
	d.text = text
	d.mu.Unlock()
}

func TestRunnerGeneratorFallbackAndSafeSources(t *testing.T) {
	var calls int
	preparer := PreparerFunc(func(context.Context, GenerationInput) ([]PreparedRun, error) { return []PreparedRun{{}, {}}, nil })
	run := RunnerFunc(func(_ context.Context, _ runner.RunRequest, event func(runner.Event) error) (*runner.Done, error) {
		calls++
		if calls == 1 {
			return nil, &runner.Error{Kind: "provider_failed", Retryable: true, Message: "provider secret text"}
		}
		if err := event(runner.Event{Type: "text_delta", Delta: "draft"}); err != nil {
			return nil, err
		}
		return &runner.Done{Text: "answer", SourcesStated: true, Sources: []core.Source{{DocumentID: "doc", VersionID: "version", Page: 3}},
			Usage: provider.Usage{Input: 10, Output: 4}, FinishReason: "stop"}, nil
	})
	g := RunnerGenerator{Preparer: preparer, Runner: run}
	draft := &draftCapture{}
	result, err := g.Generate(t.Context(), GenerationInput{Conversation: store.ChatConversation{ID: "conv", CourseID: "course"}, Generation: store.ChatGeneration{UserMessageID: "message", Attempt: 1}}, draft)
	if err != nil || calls != 2 || result.Body != "answer" || result.Usage.Output != 4 || len(result.Sources) != 1 || result.Sources[0].Page != 3 {
		t.Fatalf("result = %+v calls=%d err=%v", result, calls, err)
	}
	if draft.text != "draft" {
		t.Fatalf("draft = %q", draft.text)
	}
}
