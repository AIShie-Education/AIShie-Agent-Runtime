package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

type approvalTestAccess struct {
	*testAccess
	allowed    atomic.Bool
	level      string
	denyOpener bool
}

func (a *approvalTestAccess) CheckChatDecision(_ context.Context, actor string, _ string, _ string, _ string, _ string, _ string) (*core.RuntimeChatDecision, error) {
	if !a.allowed.Load() || a.denyOpener && actor == "actor" {
		return &core.RuntimeChatDecision{Allowed: false}, nil
	}
	level := a.level
	if level == "" {
		level = "autonomous"
	}
	return &core.RuntimeChatDecision{Allowed: true, ActionDecideLevel: level}, nil
}

func newApprovalAccess(auth *Authorization, allowed bool) *approvalTestAccess {
	a := &approvalTestAccess{testAccess: &testAccess{auth: auth}}
	a.allowed.Store(allowed)
	return a
}

func TestCanonicalProposalJSON(t *testing.T) {
	pending := store.ChatApprovalPending
	created := CreateResponse{Snapshot: Snapshot{Messages: []store.ChatMessage{}, Submission: &store.ChatSubmission{Status: store.ChatSubmissionProposed, ApprovalID: "a1", ReviewState: &pending}}}
	body, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{`"submission":{"status":"proposed","approval_id":"a1","review_state":"pending"}`, `"messages":[]`} {
		if !strings.Contains(text, want) {
			t.Errorf("%s missing %s", text, want)
		}
	}
	approval, err := json.Marshal(store.ChatApproval{ID: "a1", Subject: store.ChatSubjectQuestion, State: store.ChatApprovalPending, Mode: store.ChatApprovalModeDecision, GenerationID: "internal-only"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(approval), "internal-only") || strings.Contains(string(approval), "mode") || strings.Contains(string(approval), "generation") {
		t.Fatalf("internal approval fields leaked: %s", approval)
	}
	autonomous, err := json.Marshal(store.ChatSubmission{Status: store.ChatSubmissionExecuted})
	if err != nil {
		t.Fatal(err)
	}
	if string(autonomous) != `{"status":"executed","review_state":null}` {
		t.Fatalf("autonomous submission=%s", autonomous)
	}
}

func TestQuestionProposalExecutesOnlyAfterApproval(t *testing.T) {
	auth := completeAuthorization()
	auth.Grant.ConversationAskLevel = "confirm_required"
	access := newApprovalAccess(auth, true)
	var calls atomic.Int64
	generator := GeneratorFunc(func(context.Context, GenerationInput, DraftSink) (store.ChatResult, error) {
		calls.Add(1)
		return store.ChatResult{Body: "answer"}, nil
	})
	s := testService(t, access, generator)
	in := CreateInput{AgentID: "agent", Body: "pending question", IdempotencyKey: "proposal-key"}
	created, err := s.Create(t.Context(), "actor", "course", in)
	if err != nil {
		t.Fatal(err)
	}
	if created.Submission == nil || created.Submission.Status != store.ChatSubmissionProposed || created.Submission.ApprovalID == "" || created.Message.Status != store.ChatPending {
		t.Fatalf("create=%+v", created)
	}
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("unapproved question generated")
	}
	replay, err := s.Create(t.Context(), "actor", "course", in)
	if err != nil || replay.Submission == nil || replay.Submission.ApprovalID != created.Submission.ApprovalID {
		t.Fatalf("replay=%+v %v", replay, err)
	}
	decided, err := s.Decide(t.Context(), "reviewer", created.Submission.ApprovalID, DecideInput{Decision: "approve", IdempotencyKey: "approve-key"})
	if err != nil {
		t.Fatal(err)
	}
	if decided.Approval.State != store.ChatApprovalApproved || decided.Snapshot.Conversation.State != store.ChatQueued {
		t.Fatalf("decision=%+v", decided)
	}
	waitFor(t, func() bool { return calls.Load() == 1 })
	waitFor(t, func() bool {
		c, _ := s.Get(t.Context(), "actor", "course", created.Conversation.ID)
		return c.State == store.ChatAnswered
	})
	// Replaying the original create after approval still returns the original
	// accepted proposal and never schedules a second generation.
	replay, err = s.Create(t.Context(), "actor", "course", in)
	if err != nil || replay.Submission.Status != store.ChatSubmissionProposed {
		t.Fatalf("post-approval replay=%+v %v", replay, err)
	}
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("generator calls=%d", calls.Load())
	}
}

func TestAnswerProposalHidesBodyUntilApproval(t *testing.T) {
	auth := completeAuthorization()
	auth.Grant.ConversationAnswerLevel = "confirm_required"
	access := newApprovalAccess(auth, true)
	access.denyOpener = true
	generator := GeneratorFunc(func(_ context.Context, _ GenerationInput, sink DraftSink) (store.ChatResult, error) {
		sink.Set("secret draft", []store.ChatStep{{Kind: "thinking", State: "running"}})
		return store.ChatResult{Body: "secret answer", Usage: &store.ChatUsage{Output: 3}}, nil
	})
	s := testService(t, access, generator)
	created, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		c, _ := s.Get(t.Context(), "actor", "course", created.Conversation.ID)
		return c.State == store.ChatReplyPendingApproval
	})
	snap, err := s.Messages(t.Context(), "actor", "course", created.Conversation.ID, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	pending := snap.Messages[len(snap.Messages)-1]
	if pending.Status != store.ChatPending || pending.Body != "" || pending.Usage != nil || snap.Draft != nil {
		t.Fatalf("pending answer leaked=%+v draft=%+v", pending, snap.Draft)
	}
	approvals, _, err := s.ListApprovals(t.Context(), "reviewer", "course", store.ChatApprovalPending, "", 50)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("approvals=%+v %v", approvals, err)
	}
	a := approvals[0]
	if a.Subject != store.ChatSubjectAnswer {
		t.Fatalf("subject=%s", a.Subject)
	}
	detail, err := s.GetApproval(t.Context(), "reviewer", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := detail.Snapshot.Messages[len(detail.Snapshot.Messages)-1]
	if target.Body != "secret answer" || target.Usage == nil || target.Usage.Output != 3 {
		t.Fatalf("reviewer detail=%+v", target)
	}
	if _, err := s.GetApproval(t.Context(), "actor", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("opener detail=%v", err)
	}
	ordinary, _ := s.Messages(t.Context(), "actor", "course", created.Conversation.ID, 50, 0)
	if ordinary.Messages[len(ordinary.Messages)-1].Body != "" {
		t.Fatal("reviewer detail unscrubbed the opener snapshot")
	}
	decided, err := s.Decide(t.Context(), "reviewer", a.ID, DecideInput{Decision: "approve", IdempotencyKey: "publish"})
	if err != nil {
		t.Fatal(err)
	}
	published := decided.Snapshot.Messages[len(decided.Snapshot.Messages)-1]
	if published.Status != store.ChatPosted || published.Body != "secret answer" || published.Usage.Output != 3 {
		t.Fatalf("published=%+v", published)
	}
}

func TestPendingReviewRecordsSuccessfulReview(t *testing.T) {
	auth := completeAuthorization()
	auth.Grant.ConversationAskLevel = "pending_review"
	auth.Grant.ConversationAnswerLevel = "pending_review"
	access := newApprovalAccess(auth, true)
	access.level = "pending_review"
	s := testService(t, access, GeneratorFunc(func(context.Context, GenerationInput, DraftSink) (store.ChatResult, error) {
		return store.ChatResult{Body: "posted"}, nil
	}))
	created, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Submission.Status != store.ChatSubmissionExecuted || created.Submission.ReviewState == nil || *created.Submission.ReviewState != store.ChatApprovalPending {
		t.Fatalf("submission=%+v", created.Submission)
	}
	waitFor(t, func() bool {
		c, _ := s.Get(t.Context(), "actor", "course", created.Conversation.ID)
		return c.State == store.ChatAnswered
	})
	approvals, _, err := s.ListApprovals(t.Context(), "reviewer", "course", store.ChatApprovalPending, "", 50)
	if err != nil || len(approvals) != 2 {
		t.Fatalf("review items=%+v %v", approvals, err)
	}
	for _, a := range approvals {
		reviewed, err := s.Review(t.Context(), "reviewer", a.ID, ReviewInput{IdempotencyKey: "review-" + a.ID})
		if err != nil || reviewed.Approval.State != store.ChatApprovalReviewed {
			t.Fatalf("review=%+v %v", reviewed, err)
		}
	}
	remaining, _, err := s.ListApprovals(t.Context(), "reviewer", "course", store.ChatApprovalPending, "", 50)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("review created another review record: %+v %v", remaining, err)
	}
}

func TestReviewerConfirmationRequiredDoesNotExecute(t *testing.T) {
	auth := completeAuthorization()
	auth.Grant.ConversationAskLevel = "confirm_required"
	access := newApprovalAccess(auth, true)
	access.level = "confirm_required"
	var calls atomic.Int64
	s := testService(t, access, GeneratorFunc(func(context.Context, GenerationInput, DraftSink) (store.ChatResult, error) {
		calls.Add(1)
		return store.ChatResult{}, nil
	}))
	created, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Decide(t.Context(), "reviewer", created.Submission.ApprovalID, DecideInput{Decision: "approve", IdempotencyKey: "approve"})
	var chatErr *Error
	if !errors.As(err, &chatErr) || chatErr.Reason != "reviewer_confirmation_required" {
		t.Fatalf("reviewer decision=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("confirm-required reviewer executed a question")
	}
}

func TestApprovalAttachmentRequiresReviewerAndExactBinding(t *testing.T) {
	auth := completeAuthorization()
	auth.Grant.ConversationAskLevel = "confirm_required"
	access := newApprovalAccess(auth, true)
	s := testService(t, access, nil)
	file, err := s.StageAttachment(t.Context(), "actor", "course", "q.txt", "text/plain", []byte("question bytes"), "upload")
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", AttachmentIDs: []string{file.ID}, IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ApprovalAttachment(t.Context(), "reviewer", created.Submission.ApprovalID, file.ID)
	if err != nil || string(got.Bytes) != "question bytes" {
		t.Fatalf("reviewer attachment=%+v %v", got, err)
	}
	other, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "other", IdempotencyKey: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovalAttachment(t.Context(), "reviewer", other.Submission.ApprovalID, file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-approval attachment=%v", err)
	}
	access.allowed.Store(false)
	if _, err := s.ApprovalAttachment(t.Context(), "reviewer", created.Submission.ApprovalID, file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("denied reviewer attachment=%v", err)
	}
	owner, err := s.Attachment(t.Context(), "actor", "course", file.ID)
	if err != nil || owner.ID != file.ID {
		t.Fatalf("ordinary owner route changed=%+v %v", owner, err)
	}
	if _, err := s.Attachment(t.Context(), "other-user", "course", file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ordinary non-owner route changed=%v", err)
	}
}

func TestApprovalRequiresFreshCoreDecision(t *testing.T) {
	auth := completeAuthorization()
	auth.Grant.ConversationAskLevel = "confirm_required"
	access := newApprovalAccess(auth, false)
	s := testService(t, access, GeneratorFunc(func(context.Context, GenerationInput, DraftSink) (store.ChatResult, error) {
		t.Error("generation ran")
		return store.ChatResult{}, nil
	}))
	created, err := s.Create(t.Context(), "actor", "course", CreateInput{AgentID: "agent", Body: "question", IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(t.Context(), "reviewer", created.Submission.ApprovalID, DecideInput{Decision: "approve", IdempotencyKey: "approve"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("denied reviewer=%v", err)
	}
	if _, err := s.GetApproval(t.Context(), "reviewer", created.Submission.ApprovalID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("denied get=%v", err)
	}
}
