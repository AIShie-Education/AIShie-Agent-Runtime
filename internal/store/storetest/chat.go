package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

func chatCreate(id, actor, key string) store.ChatCreateParams {
	c := store.ChatConversation{ID: "conv-" + id, CourseID: "course-1", AgentID: "agent-core", RuntimeAgentID: "agent-ar", OpenerActorID: actor, Title: "Question"}
	return store.ChatCreateParams{Conversation: c, Message: store.ChatMessage{ID: "user-" + id, Body: "hello"},
		GenerationID: "gen-" + id, AssistantMsgID: "assistant-" + id, IdempotencyKey: key, RequestFingerprint: "fp-" + id}
}

func TestChatContractMarker(t *testing.T) { t.Helper() }

func testChat(t *testing.T, open Opener) {
	t.Run("CreateOwnershipIdempotencyAndPages", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()
		p := chatCreate("one", "alice", "key-one")
		snap, replay, err := st.CreateChatConversation(ctx, p)
		if err != nil || replay || len(snap.Messages) != 1 || snap.Conversation.State != store.ChatQueued || snap.Conversation.Revision != 1 {
			t.Fatalf("create = %+v %v %v", snap, replay, err)
		}
		again, replay, err := st.CreateChatConversation(ctx, p)
		if err != nil || !replay || again.Messages[0].ID != snap.Messages[0].ID {
			t.Fatalf("create replay = %+v %v %v", again, replay, err)
		}
		p.Message.Body = "different"
		p.RequestFingerprint = "different"
		if _, _, err := st.CreateChatConversation(ctx, p); !errors.Is(err, store.ErrChatIdempotency) {
			t.Fatalf("key reuse error = %v", err)
		}
		if _, err := st.ChatSnapshot(ctx, "course-1", "conv-one", "bob", 50, 0); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cross-owner read = %v", err)
		}
		if _, err := st.ChatSnapshot(ctx, "course-2", "conv-one", "alice", 50, 0); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cross-course read = %v", err)
		}

		g, c, ok, err := st.BeginChatGeneration(ctx, "gen-one", "worker-1", time.Minute)
		if err != nil || !ok || g.Attempt != 1 || c.State != store.ChatAnswering {
			t.Fatalf("begin = %+v %+v %v %v", g, c, ok, err)
		}
		stated := true
		result := store.ChatResult{Body: "answer", Sources: []store.ChatSource{{DocumentID: "doc", Page: 2}}, SourcesStated: &stated,
			Usage: &store.ChatUsage{Input: 3, Output: 2}, FinishReason: "stop"}
		if _, _, ok, err := st.PutChatDraft(ctx, g.ID, g.Holder, g.Fence, "ans", []store.ChatStep{{Kind: "writing", State: "running"}}); err != nil || !ok {
			t.Fatalf("draft = %v %v", ok, err)
		}
		if ok, err := st.WriteChatResult(ctx, g.ID, g.Holder, g.Fence, result); err != nil || !ok {
			t.Fatalf("write ahead = %v %v", ok, err)
		}
		done, changed, err := st.FinalizeChatGeneration(ctx, g.ID, g.Holder, g.Fence)
		if err != nil || !changed || done.Draft != nil || done.Conversation.State != store.ChatAnswered || len(done.Messages) != 2 {
			t.Fatalf("final = %+v %v %v", done, changed, err)
		}
		if done.Messages[1].Body != "answer" || done.Messages[1].Usage.Output != 2 || len(done.Messages[1].Sources) != 1 {
			t.Fatalf("answer = %+v", done.Messages[1])
		}
		if _, changed, err := st.FinalizeChatGeneration(ctx, g.ID, g.Holder, g.Fence); err != nil || changed {
			t.Fatalf("final replay = %v %v", changed, err)
		}

		for i := 0; i < 4; i++ {
			id := fmt.Sprintf("many-%d", i)
			cp := chatCreate(id, "alice", "key-"+id)
			if _, _, err := st.CreateChatConversation(ctx, cp); err != nil {
				t.Fatal(err)
			}
		}
		page, next, err := st.ListChatConversations(ctx, "course-1", "alice", "", 2)
		if err != nil || len(page) != 2 || next == "" {
			t.Fatalf("page one = %d %q %v", len(page), next, err)
		}
		page2, _, err := st.ListChatConversations(ctx, "course-1", "alice", next, 10)
		if err != nil || len(page2) != 3 {
			t.Fatalf("page two = %d %v", len(page2), err)
		}
		for _, c := range page {
			for _, d := range page2 {
				if c.ID == d.ID {
					t.Fatalf("duplicate across pages: %s", c.ID)
				}
			}
		}

		msgs, err := st.ChatSnapshot(ctx, "course-1", "conv-one", "alice", 1, 0)
		if err != nil || len(msgs.Messages) != 1 || !msgs.More || msgs.Messages[0].Seq != 2 {
			t.Fatalf("newest message page = %+v %v", msgs, err)
		}
		older, err := st.ChatSnapshot(ctx, "course-1", "conv-one", "alice", 10, 2)
		if err != nil || len(older.Messages) != 1 || older.Messages[0].Seq != 1 {
			t.Fatalf("before_seq = %+v %v", older, err)
		}
	})

	t.Run("AskAttachmentsWithdrawCloseAndRecovery", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()
		p := chatCreate("two", "alice", "create-two")
		for i := 0; i < 5; i++ {
			a := store.ChatAttachment{ID: fmt.Sprintf("img-%d", i), CourseID: "course-1", OwnerActorID: "alice", Filename: fmt.Sprintf("%d.png", i), MediaType: "image/png", Bytes: []byte{1}, Size: 1}
			if _, err := st.StageChatAttachment(ctx, a); err != nil {
				t.Fatal(err)
			}
			p.AttachmentIDs = append(p.AttachmentIDs, a.ID)
		}
		if _, _, err := st.CreateChatConversation(ctx, p); !errors.Is(err, store.ErrChatAttachments) {
			t.Fatalf("five images = %v", err)
		}
		p.AttachmentIDs = p.AttachmentIDs[:4]
		if _, _, err := st.CreateChatConversation(ctx, p); err != nil {
			t.Fatal(err)
		}
		got, err := st.ChatAttachment(ctx, "course-1", "img-0", "alice")
		if err != nil || got.BoundMessage != "user-two" || len(got.Bytes) != 1 {
			t.Fatalf("bound attachment = %+v %v", got, err)
		}
		if _, err := st.ChatAttachment(ctx, "course-1", "img-0", "bob"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cross-owner attachment = %v", err)
		}
		if _, _, _, err := st.BeginChatGeneration(ctx, "gen-two", "worker-1", time.Minute); err != nil {
			t.Fatal(err)
		}
		withdrawn, err := st.WithdrawChatMessage(ctx, "course-1", "conv-two", "alice", "user-two", "withdrawn")
		if err != nil || withdrawn.Draft != nil || withdrawn.Messages[len(withdrawn.Messages)-1].Status != store.ChatRetracted || withdrawn.Messages[0].Body != "" {
			t.Fatalf("withdraw = %+v %v", withdrawn, err)
		}
		if _, _, _, err := st.BeginChatGeneration(ctx, "gen-two", "worker-1", time.Minute); err != nil {
			t.Fatal(err)
		} // cancelled: begin is false, not an error
		g, _ := st.ChatGeneration(ctx, "gen-two")
		if g.State != store.ChatGenerationCancelled {
			t.Fatalf("generation after withdrawal = %s", g.State)
		}

		ap := store.ChatAppendParams{CourseID: "course-1", ConversationID: "conv-two", ActorID: "alice", Message: store.ChatMessage{ID: "user-next", Body: "next"},
			GenerationID: "gen-next", AssistantMsgID: "assistant-next", IdempotencyKey: "ask-next", RequestFingerprint: "ask-next"}
		if _, _, err := st.AppendChatMessage(ctx, ap); err != nil {
			t.Fatal(err)
		}
		_, replay, err := st.AppendChatMessage(ctx, ap)
		if err != nil || !replay {
			t.Fatalf("exact append replay = %v %v", replay, err)
		}
		g, c, ok, err := st.BeginChatGeneration(ctx, "gen-next", "worker-1", time.Minute)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if ok, err := st.WriteChatResult(ctx, g.ID, g.Holder, g.Fence, store.ChatResult{Body: "persisted"}); err != nil || !ok {
			t.Fatal(err)
		}
		recovered, err := st.RecoverChatGenerations(ctx, 100)
		if err != nil || len(recovered) != 1 || recovered[0].Result == nil || recovered[0].Result.Body != "persisted" {
			t.Fatalf("recovery = %+v %v", recovered, err)
		}
		if _, _, err := st.FinalizeChatGeneration(ctx, g.ID, g.Holder, g.Fence); err != nil {
			t.Fatal(err)
		}
		closed, err := st.CloseChatConversation(ctx, "course-1", "conv-two", "alice", "done")
		if err != nil || closed.State != store.ChatClosed || closed.CloseReason != "done" {
			t.Fatalf("close = %+v %v", closed, err)
		}
		if _, _, err := st.AppendChatMessage(ctx, store.ChatAppendParams{CourseID: "course-1", ConversationID: "conv-two", ActorID: "alice", Message: store.ChatMessage{ID: "x"}, GenerationID: "gx", AssistantMsgID: "ax", IdempotencyKey: "kx", RequestFingerprint: "fx"}); !errors.Is(err, store.ErrChatNotAwaiting) {
			t.Fatalf("ask closed = %v", err)
		}
		if _, err := st.CloseChatConversation(ctx, "course-1", "conv-two", "bob", ""); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("close cross-owner = %v", err)
		}
		_ = c
	})

	t.Run("AttachmentByteLimitsAndFailure", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()
		big := store.ChatAttachment{ID: "big", CourseID: "course-1", OwnerActorID: "alice", Filename: "big.txt", MediaType: "text/plain", Bytes: make([]byte, 32<<10+1), Size: 32<<10 + 1}
		if _, err := st.StageChatAttachment(ctx, big); err == nil {
			t.Fatal("oversized text accepted")
		}
		p := chatCreate("fail", "alice", "create-fail")
		if _, _, err := st.CreateChatConversation(ctx, p); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := st.BeginChatGeneration(ctx, "gen-fail", "worker-1", time.Minute); err != nil {
			t.Fatal(err)
		}
		c, changed, err := st.FailChatGeneration(ctx, "gen-fail", "worker-1", 1, "provider_failed", true)
		if err != nil || !changed || c.State != store.ChatFailed {
			t.Fatalf("fail = %+v %v %v", c, changed, err)
		}
		snap, err := st.ChatSnapshot(ctx, "course-1", "conv-fail", "alice", 50, 0)
		if err != nil || len(snap.Messages) != 2 || snap.Messages[1].Error == nil || snap.Messages[1].Status != store.ChatMsgFailed {
			t.Fatalf("failed message = %+v %v", snap, err)
		}
	})

	t.Run("ProposalDecisionAndPostReview", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()
		pending := store.ChatApprovalPending
		p := chatCreate("approval", "alice", "create-approval")
		p.AnswerLevel = "confirm_required"
		p.Submission = store.ChatSubmission{Status: store.ChatSubmissionProposed, ApprovalID: "question-approval", ReviewState: &pending}
		p.Approval = &store.ChatApproval{ID: "question-approval", Subject: store.ChatSubjectQuestion, State: store.ChatApprovalPending, Mode: store.ChatApprovalModeDecision}
		snap, replay, err := st.CreateChatConversation(ctx, p)
		if err != nil || replay {
			t.Fatal(err)
		}
		if snap.Conversation.State != store.ChatReplyPendingApproval || snap.Messages[0].Status != store.ChatPending || snap.Submission == nil || snap.Submission.Status != store.ChatSubmissionProposed {
			t.Fatalf("question proposal=%+v", snap)
		}
		if _, _, ok, err := st.BeginChatGeneration(ctx, "gen-approval", "worker", time.Minute); err != nil || ok {
			t.Fatalf("unapproved question began=%v %v", ok, err)
		}
		if replaySnap, replay, err := st.CreateChatConversation(ctx, p); err != nil || !replay || replaySnap.Submission == nil || replaySnap.Submission.ApprovalID != "question-approval" {
			t.Fatalf("proposal replay=%+v %v %v", replaySnap, replay, err)
		}
		decision := store.ChatDecisionParams{ActorID: "staff", ApprovalID: "question-approval", Decision: "approve", IdempotencyKey: "approve-question", Fingerprint: "approve-question", Operation: "decide"}
		approval, approved, schedule, err := st.DecideChatApproval(ctx, decision)
		if err != nil || !schedule || approval.State != store.ChatApprovalApproved || approved.Conversation.State != store.ChatQueued || approved.Messages[0].Status != store.ChatPosted {
			t.Fatalf("approve question=%+v %+v %v %v", approval, approved, schedule, err)
		}
		if _, _, schedule, err := st.DecideChatApproval(ctx, decision); err != nil || schedule {
			t.Fatalf("decision replay schedule=%v err=%v", schedule, err)
		}
		g, _, ok, err := st.BeginChatGeneration(ctx, "gen-approval", "worker", time.Minute)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if ok, err := st.WriteChatResult(ctx, g.ID, g.Holder, g.Fence, store.ChatResult{Body: "hidden answer", Usage: &store.ChatUsage{Output: 2}}); err != nil || !ok {
			t.Fatal(err)
		}
		proposed, changed, err := st.ProposeChatGeneration(ctx, g.ID, g.Holder, g.Fence, store.ChatApproval{ID: "answer-approval", Subject: store.ChatSubjectAnswer, Mode: store.ChatApprovalModeDecision, State: store.ChatApprovalPending})
		if err != nil || !changed {
			t.Fatal(err)
		}
		last := proposed.Messages[len(proposed.Messages)-1]
		if last.Status != store.ChatPending || last.Body != "" || last.Usage != nil || proposed.Conversation.State != store.ChatReplyPendingApproval {
			t.Fatalf("public pending answer=%+v", last)
		}
		if _, changed, err := st.FinalizeChatGeneration(ctx, g.ID, g.Holder, g.Fence); err != nil || changed {
			t.Fatalf("stale final after proposal=%v %v", changed, err)
		}
		answerDecision := store.ChatDecisionParams{ActorID: "staff", ApprovalID: "answer-approval", Decision: "approve", IdempotencyKey: "approve-answer", Fingerprint: "approve-answer", Operation: "decide"}
		approval, published, schedule, err := st.DecideChatApproval(ctx, answerDecision)
		if err != nil || schedule || approval.State != store.ChatApprovalApproved {
			t.Fatalf("approve answer=%+v %v %v", approval, schedule, err)
		}
		if published.Messages[len(published.Messages)-1].Body != "hidden answer" || published.Conversation.State != store.ChatAnswered {
			t.Fatalf("published=%+v", published)
		}

		// changes_requested starts a fresh fenced generation; reject then makes
		// its answer a safe terminal failure without exposing the draft body.
		if _, _, schedule, err := st.DecideChatApproval(ctx, answerDecision); err != nil || schedule {
			t.Fatal("exact decision replay was not a successful no-op")
		}

		// A pending question can be rejected without running a model.
		reject := chatCreate("reject", "alice", "create-reject")
		reject.Submission = store.ChatSubmission{Status: store.ChatSubmissionProposed, ApprovalID: "reject-approval", ReviewState: &pending}
		reject.Approval = &store.ChatApproval{ID: "reject-approval", Subject: store.ChatSubjectQuestion, State: store.ChatApprovalPending, Mode: store.ChatApprovalModeDecision}
		if _, _, err := st.CreateChatConversation(ctx, reject); err != nil {
			t.Fatal(err)
		}
		rejectedApproval, rejected, schedule, err := st.DecideChatApproval(ctx, store.ChatDecisionParams{ActorID: "staff", ApprovalID: "reject-approval", Decision: "reject", Reason: "no", IdempotencyKey: "reject-question", Fingerprint: "reject-question"})
		if err != nil || schedule || rejectedApproval.State != store.ChatApprovalRejected || rejected.Conversation.State != store.ChatAnswered || rejected.Messages[0].Status != store.ChatMsgFailed {
			t.Fatalf("reject=%+v %v %v", rejected, schedule, err)
		}

		// A changes_requested answer starts a fresh fenced generation; rejecting
		// the revised proposal is terminal and publishes no body.
		change := chatCreate("changes", "alice", "create-changes")
		change.AnswerLevel = "confirm_required"
		change.Submission = store.ChatSubmission{Status: store.ChatSubmissionProposed, ApprovalID: "change-question", ReviewState: &pending}
		change.Approval = &store.ChatApproval{ID: "change-question", Subject: store.ChatSubjectQuestion, State: store.ChatApprovalPending, Mode: store.ChatApprovalModeDecision}
		if _, _, err := st.CreateChatConversation(ctx, change); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := st.DecideChatApproval(ctx, store.ChatDecisionParams{ActorID: "staff", ApprovalID: "change-question", Decision: "approve", IdempotencyKey: "change-q-approve", Fingerprint: "approve"}); err != nil {
			t.Fatal(err)
		}
		cg, _, ok, err := st.BeginChatGeneration(ctx, "gen-changes", "worker", time.Minute)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if ok, err := st.WriteChatResult(ctx, cg.ID, cg.Holder, cg.Fence, store.ChatResult{Body: "first"}); err != nil || !ok {
			t.Fatal(err)
		}
		if _, _, err := st.ProposeChatGeneration(ctx, cg.ID, cg.Holder, cg.Fence, store.ChatApproval{ID: "change-answer", Subject: store.ChatSubjectAnswer, Mode: store.ChatApprovalModeDecision, State: store.ChatApprovalPending}); err != nil {
			t.Fatal(err)
		}
		_, changedSnap, schedule, err := st.DecideChatApproval(ctx, store.ChatDecisionParams{ActorID: "staff", ApprovalID: "change-answer", Decision: "changes_requested", Reason: "revise", IdempotencyKey: "change-a-request", Fingerprint: "changes", NewGenerationID: "gen-revised", NewAssistantID: "assistant-revised"})
		if err != nil || !schedule || changedSnap.Conversation.State != store.ChatQueued {
			t.Fatalf("changes=%+v %v %v", changedSnap, schedule, err)
		}
		rg2, _, ok, err := st.BeginChatGeneration(ctx, "gen-revised", "worker", time.Minute)
		if err != nil || !ok || rg2.RevisionReason != "revise" || rg2.RevisionBody != "first" || rg2.RevisionOfGenerationID != cg.ID {
			t.Fatal(err)
		}
		if ok, err := st.WriteChatResult(ctx, rg2.ID, rg2.Holder, rg2.Fence, store.ChatResult{Body: "second"}); err != nil || !ok {
			t.Fatal(err)
		}
		if _, _, err := st.ProposeChatGeneration(ctx, rg2.ID, rg2.Holder, rg2.Fence, store.ChatApproval{ID: "revised-answer", Subject: store.ChatSubjectAnswer, Mode: store.ChatApprovalModeDecision, State: store.ChatApprovalPending}); err != nil {
			t.Fatal(err)
		}
		_, rejectedAnswer, _, err := st.DecideChatApproval(ctx, store.ChatDecisionParams{ActorID: "staff", ApprovalID: "revised-answer", Decision: "reject", IdempotencyKey: "reject-revised", Fingerprint: "reject"})
		if err != nil || rejectedAnswer.Messages[len(rejectedAnswer.Messages)-1].Body != "" {
			t.Fatalf("rejected answer=%+v %v", rejectedAnswer, err)
		}

		// pending_review executes/posts and retains review records until review.
		review := chatCreate("review", "alice", "create-review")
		review.AnswerLevel = "pending_review"
		review.Submission = store.ChatSubmission{Status: store.ChatSubmissionExecuted, ApprovalID: "review-question", ReviewState: &pending}
		review.Approval = &store.ChatApproval{ID: "review-question", Subject: store.ChatSubjectQuestion, State: store.ChatApprovalPending, Mode: store.ChatApprovalModeReview}
		if _, _, err := st.CreateChatConversation(ctx, review); err != nil {
			t.Fatal(err)
		}
		rg, _, ok, err := st.BeginChatGeneration(ctx, "gen-review", "worker", time.Minute)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if ok, err := st.WriteChatResult(ctx, rg.ID, rg.Holder, rg.Fence, store.ChatResult{Body: "posted"}); err != nil || !ok {
			t.Fatal(err)
		}
		if _, _, err := st.FinalizeReviewedChatGeneration(ctx, rg.ID, rg.Holder, rg.Fence, store.ChatApproval{ID: "review-answer", Subject: store.ChatSubjectAnswer, Mode: store.ChatApprovalModeReview, State: store.ChatApprovalPending}); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"review-question", "review-answer"} {
			rp := store.ChatDecisionParams{ActorID: "staff", ApprovalID: id, IdempotencyKey: "review-" + id, Fingerprint: "review", Operation: "review"}
			a, reviewed, err := st.ReviewChatApproval(ctx, rp)
			if err != nil || a.State != store.ChatApprovalReviewed {
				t.Fatalf("review %s=%+v %v", id, a, err)
			}
			a, _, err = st.ReviewChatApproval(ctx, rp)
			if err != nil || a.State != store.ChatApprovalReviewed {
				t.Fatalf("review replay=%+v %v", a, err)
			}
			if len(reviewed.Messages) == 0 {
				t.Fatal("review snapshot missing")
			}
		}
		// A pending_review reviewer may execute, but the same decision
		// transaction leaves one new review item.
		reviewer := chatCreate("reviewer", "alice", "create-reviewer")
		reviewer.Submission = store.ChatSubmission{Status: store.ChatSubmissionProposed, ApprovalID: "reviewer-question", ReviewState: &pending}
		reviewer.Approval = &store.ChatApproval{ID: "reviewer-question", Subject: store.ChatSubjectQuestion, State: store.ChatApprovalPending, Mode: store.ChatApprovalModeDecision}
		if _, _, err := st.CreateChatConversation(ctx, reviewer); err != nil {
			t.Fatal(err)
		}
		_, _, schedule, err = st.DecideChatApproval(ctx, store.ChatDecisionParams{ActorID: "limited-staff", ApprovalID: "reviewer-question", Decision: "approve", IdempotencyKey: "reviewer-approve", Fingerprint: "approve", ReviewerLevel: "pending_review", ReviewApprovalID: "reviewer-followup"})
		if err != nil || !schedule {
			t.Fatalf("pending reviewer decision=%v %v", schedule, err)
		}
		followup, _, err := st.ChatApprovalRecord(ctx, "reviewer-followup")
		if err != nil || followup.Mode != store.ChatApprovalModeReview || followup.State != store.ChatApprovalPending {
			t.Fatalf("review followup=%+v %v", followup, err)
		}
		if _, _, err := st.ReviewChatApproval(ctx, store.ChatDecisionParams{ActorID: "limited-staff", ApprovalID: "reviewer-followup", IdempotencyKey: "reviewer-review", Fingerprint: "review", Operation: "review", ReviewerLevel: "pending_review"}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ReviewerAttachmentUsesExactApprovalBinding", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()
		pending := store.ChatApprovalPending
		p := chatCreate("attachment-approval", "alice", "create-attachment-approval")
		p.AnswerLevel = "confirm_required"
		p.AttachmentIDs = []string{"question-file"}
		p.Submission = store.ChatSubmission{Status: store.ChatSubmissionProposed, ApprovalID: "attachment-question-approval", ReviewState: &pending}
		p.Approval = &store.ChatApproval{ID: "attachment-question-approval", Subject: store.ChatSubjectQuestion, State: store.ChatApprovalPending, Mode: store.ChatApprovalModeDecision}
		if _, err := st.StageChatAttachment(ctx, store.ChatAttachment{ID: "question-file", CourseID: "course-1", OwnerActorID: "alice", Filename: "q.txt", MediaType: "text/plain", Bytes: []byte("question bytes"), Size: 14}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.CreateChatConversation(ctx, p); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := st.DecideChatApproval(ctx, store.ChatDecisionParams{ActorID: "staff", ApprovalID: "attachment-question-approval", Decision: "approve", IdempotencyKey: "approve-attachment-q", Fingerprint: "approve"}); err != nil {
			t.Fatal(err)
		}
		g, _, ok, err := st.BeginChatGeneration(ctx, "gen-attachment-approval", "worker", time.Minute)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if ok, err := st.WriteChatResult(ctx, g.ID, g.Holder, g.Fence, store.ChatResult{Body: "pending answer"}); err != nil || !ok {
			t.Fatal(err)
		}
		if _, _, err := st.ProposeChatGeneration(ctx, g.ID, g.Holder, g.Fence, store.ChatApproval{ID: "attachment-answer-approval", Subject: store.ChatSubjectAnswer, Mode: store.ChatApprovalModeDecision, State: store.ChatApprovalPending}); err != nil {
			t.Fatal(err)
		}
		for _, approval := range []string{"attachment-question-approval", "attachment-answer-approval"} {
			a, err := st.ChatApprovalAttachment(ctx, approval, "question-file")
			if err != nil || string(a.Bytes) != "question bytes" {
				t.Fatalf("%s attachment=%+v %v", approval, a, err)
			}
		}
		other := chatCreate("other-approval", "alice", "create-other-approval")
		other.Submission = store.ChatSubmission{Status: store.ChatSubmissionProposed, ApprovalID: "other-question-approval", ReviewState: &pending}
		other.Approval = &store.ChatApproval{ID: "other-question-approval", Subject: store.ChatSubjectQuestion, State: store.ChatApprovalPending, Mode: store.ChatApprovalModeDecision}
		if _, _, err := st.CreateChatConversation(ctx, other); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ChatApprovalAttachment(ctx, "other-question-approval", "question-file"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cross-approval attachment=%v", err)
		}
		if _, err := st.ChatApprovalAttachment(ctx, "attachment-answer-approval", "missing-file"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown attachment=%v", err)
		}
		if _, err := st.ChatAttachment(ctx, "course-1", "question-file", "alice"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ChatAttachment(ctx, "course-1", "question-file", "bob"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("owner isolation changed=%v", err)
		}
	})

	t.Run("WriteAheadEmptySourcesRemainAnArray", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()
		stated := true
		result := store.ChatResult{Body: "answer", SourcesStated: &stated}
		roundtrip, err := store.ParseChatResult(result.JSON())
		if err != nil || roundtrip == nil || roundtrip.Sources == nil {
			t.Fatalf("result roundtrip=%+v %v", roundtrip, err)
		}
		for _, propose := range []bool{false, true} {
			suffix := "final"
			if propose {
				suffix = "proposal"
			}
			p := chatCreate("empty-sources-"+suffix, "alice", "create-empty-sources-"+suffix)
			p.AnswerLevel = "autonomous"
			var approvalID string
			if propose {
				p.AnswerLevel = "confirm_required"
				approvalID = "approval-empty-sources"
			}
			if _, _, err := st.CreateChatConversation(ctx, p); err != nil {
				t.Fatal(err)
			}
			g, _, ok, err := st.BeginChatGeneration(ctx, "gen-empty-sources-"+suffix, "worker", time.Minute)
			if err != nil || !ok {
				t.Fatal(err)
			}
			if ok, err := st.WriteChatResult(ctx, g.ID, g.Holder, g.Fence, result); err != nil || !ok {
				t.Fatal(err)
			}
			if propose {
				snap, changed, err := st.ProposeChatGeneration(ctx, g.ID, g.Holder, g.Fence, store.ChatApproval{ID: approvalID, Subject: store.ChatSubjectAnswer, Mode: store.ChatApprovalModeDecision, State: store.ChatApprovalPending})
				if err != nil || !changed {
					t.Fatal(err)
				}
				_ = snap
				_, published, _, err := st.DecideChatApproval(ctx, store.ChatDecisionParams{ActorID: "staff", ApprovalID: approvalID, Decision: "approve", IdempotencyKey: "approve-empty-sources", Fingerprint: "approve"})
				if err != nil {
					t.Fatal(err)
				}
				if published.Messages[len(published.Messages)-1].Sources == nil {
					t.Fatal("approved answer has null sources")
				}
			} else {
				snap, changed, err := st.FinalizeChatGeneration(ctx, g.ID, g.Holder, g.Fence)
				if err != nil || !changed {
					t.Fatal(err)
				}
				if snap.Messages[len(snap.Messages)-1].Sources == nil {
					t.Fatal("final answer has null sources")
				}
			}
		}
	})

	t.Run("GenerationFenceRejectsStaleWriters", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()
		p := chatCreate("fence", "alice", "create-fence")
		if _, _, err := st.CreateChatConversation(ctx, p); err != nil {
			t.Fatal(err)
		}
		first, _, ok, err := st.BeginChatGeneration(ctx, "gen-fence", "worker-1", time.Second)
		if err != nil || !ok || first.Fence != 1 {
			t.Fatalf("first begin = %+v %v %v", first, ok, err)
		}
		if _, _, ok, err := st.BeginChatGeneration(ctx, "gen-fence", "worker-2", time.Minute); err != nil || ok {
			t.Fatalf("unexpired takeover = %v %v", ok, err)
		}
		if ok, err := st.WriteChatResult(ctx, first.ID, first.Holder, first.Fence, store.ChatResult{Body: "written ahead"}); err != nil || !ok {
			t.Fatalf("first write-ahead = %v %v", ok, err)
		}
		time.Sleep(1100 * time.Millisecond)
		second, _, ok, err := st.BeginChatGeneration(ctx, "gen-fence", "worker-2", time.Minute)
		if err != nil || !ok || second.Fence != 2 || second.Result == nil {
			t.Fatalf("expired takeover = %+v %v %v", second, ok, err)
		}
		if _, _, ok, err := st.PutChatDraft(ctx, first.ID, first.Holder, first.Fence, "stale", nil); err != nil || ok {
			t.Fatalf("stale draft = %v %v", ok, err)
		}
		if ok, err := st.WriteChatResult(ctx, first.ID, first.Holder, first.Fence, store.ChatResult{Body: "stale"}); err != nil || ok {
			t.Fatalf("stale result = %v %v", ok, err)
		}
		if _, changed, err := st.FinalizeChatGeneration(ctx, first.ID, first.Holder, first.Fence); err != nil || changed {
			t.Fatalf("stale final = %v %v", changed, err)
		}
		if _, changed, err := st.FailChatGeneration(ctx, first.ID, first.Holder, first.Fence, "stale", false); err != nil || changed {
			t.Fatalf("stale fail = %v %v", changed, err)
		}
		if ok, err := st.HeartbeatChatGeneration(ctx, second.ID, second.Holder, second.Fence, time.Minute); err != nil || !ok {
			t.Fatalf("heartbeat = %v %v", ok, err)
		}
		done, changed, err := st.FinalizeChatGeneration(ctx, second.ID, second.Holder, second.Fence)
		if err != nil || !changed || len(done.Messages) != 2 || done.Messages[1].Body != "written ahead" {
			t.Fatalf("fenced final = %+v %v %v", done, changed, err)
		}
	})

	t.Run("SameHolderReclaimsInterruptedGeneration", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()
		p := chatCreate("same-holder", "alice", "create-same-holder")
		if _, _, err := st.CreateChatConversation(ctx, p); err != nil {
			t.Fatal(err)
		}
		first, _, ok, err := st.BeginChatGeneration(ctx, "gen-same-holder", "worker-stable", time.Minute)
		if err != nil || !ok {
			t.Fatal(err)
		}
		second, _, ok, err := st.BeginChatGeneration(ctx, "gen-same-holder", "worker-stable", time.Minute)
		if err != nil || !ok || second.Fence != first.Fence+1 {
			t.Fatalf("same holder could not reclaim: %+v %v %v", second, ok, err)
		}
		if ok, err := st.WriteChatResult(ctx, first.ID, first.Holder, first.Fence, store.ChatResult{Body: "stale"}); err != nil || ok {
			t.Fatalf("old fence wrote after reclaim: %v %v", ok, err)
		}
		if ok, err := st.WriteChatResult(ctx, second.ID, second.Holder, second.Fence, store.ChatResult{Body: "recovered"}); err != nil || !ok {
			t.Fatalf("reclaimed result: %v %v", ok, err)
		}
		done, changed, err := st.FinalizeChatGeneration(ctx, second.ID, second.Holder, second.Fence)
		if err != nil || !changed || len(done.Messages) != 2 || done.Messages[1].Body != "recovered" {
			t.Fatalf("reclaimed final: %+v %v %v", done, changed, err)
		}
	})

	t.Run("ConcurrentCreateIsOneConversation", func(t *testing.T) {
		st := open(t)
		ctx := context.Background()
		p := chatCreate("race", "alice", "race-key")
		const n = 12
		var wg sync.WaitGroup
		errs := make(chan error, n)
		replays := make(chan bool, n)
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, replay, err := st.CreateChatConversation(ctx, p)
				errs <- err
				replays <- replay
			}()
		}
		wg.Wait()
		close(errs)
		close(replays)
		for err := range errs {
			if err != nil {
				t.Errorf("concurrent create: %v", err)
			}
		}
		created := 0
		for replay := range replays {
			if !replay {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("actual creates = %d, want 1", created)
		}
		rows, _, err := st.ListChatConversations(ctx, "course-1", "alice", "", 100)
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows = %+v %v", rows, err)
		}
	})
}
