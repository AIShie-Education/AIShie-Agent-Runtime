package chat

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// DecideInput is POST /chat/approvals/{id}/decision.
type DecideInput struct {
	Decision       string `json:"decision"`
	Reason         string `json:"reason,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
}

// ReviewInput is POST /chat/approvals/{id}/review. Post-review has no outcome
// choice: successful review is recorded without changing publication.
type ReviewInput struct {
	IdempotencyKey string `json:"idempotency_key"`
}

type DecisionResponse struct {
	Approval Approval `json:"approval"`
	Snapshot Snapshot `json:"snapshot"`
}

func (s *Service) authorizeApproval(ctx context.Context, actor, id, operation string) (store.ChatApproval, store.ChatConversation, string, error) {
	a, c, err := s.store.ChatApprovalRecord(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.ChatApproval{}, store.ChatConversation{}, "", ErrNotFound
	}
	if err != nil {
		return store.ChatApproval{}, store.ChatConversation{}, "", mapStoreError(err)
	}
	if s.decisions == nil {
		return store.ChatApproval{}, store.ChatConversation{}, "", ErrCore
	}
	decision, err := s.decisions.CheckChatDecision(ctx, normalizeActor(actor), c.AgentID, a.CourseID, c.OpenerActorID, a.Subject, operation)
	if err != nil {
		return store.ChatApproval{}, store.ChatConversation{}, "", ErrCore
	}
	if decision == nil || !decision.Allowed {
		return store.ChatApproval{}, store.ChatConversation{}, "", ErrNotFound
	}
	return a, c, decision.ActionDecideLevel, nil
}

func (s *Service) ListApprovals(ctx context.Context, actor, course, state, after string, limit int) ([]Approval, string, error) {
	if course == "" {
		return nil, "", serviceError("invalid_argument", "invalid_field", "course_id is required")
	}
	rows, next, err := s.store.ListChatApprovals(ctx, course, state, after, limit)
	if err != nil {
		return nil, "", mapStoreError(err)
	}
	out := make([]Approval, 0, len(rows))
	for _, a := range rows {
		operation := "decide"
		if a.Mode == store.ChatApprovalModeReview {
			operation = "review"
		}
		_, _, _, err := s.authorizeApproval(ctx, actor, a.ID, operation)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		out = append(out, a)
	}
	return out, next, nil
}

func (s *Service) GetApproval(ctx context.Context, actor, id string) (ApprovalDetail, error) {
	a, _, _ := s.store.ChatApprovalRecord(ctx, id)
	operation := "decide"
	if a.Mode == store.ChatApprovalModeReview {
		operation = "review"
	}
	a, c, _, err := s.authorizeApproval(ctx, actor, id, operation)
	if err != nil {
		return ApprovalDetail{}, err
	}
	snap, err := s.store.ChatSnapshot(ctx, c.CourseID, c.ID, c.OpenerActorID, 50, 0)
	if err != nil {
		return ApprovalDetail{}, mapStoreError(err)
	}
	// Only after fresh reviewer authorization, restore the write-ahead answer
	// into this reviewer's detail. The opener's normal snapshots stay scrubbed.
	if a.Subject == store.ChatSubjectAnswer {
		gen, err := s.store.ChatGeneration(ctx, a.GenerationID)
		if err != nil {
			return ApprovalDetail{}, mapStoreError(err)
		}
		if gen.Result != nil {
			for i := range snap.Messages {
				m := &snap.Messages[i]
				if m.ID == a.MessageID && m.Status == store.ChatPending {
					m.Body = gen.Result.Body
					m.Sources = append([]store.ChatSource(nil), gen.Result.Sources...)
					m.SourcesStated, m.Usage, m.FinishReason = gen.Result.SourcesStated, gen.Result.Usage, gen.Result.FinishReason
				}
			}
		}
	}
	return ApprovalDetail{Approval: a, Snapshot: snap}, nil
}

func validDecisionReason(s string) bool { return utf8.RuneCountInString(s) <= 500 }

func (s *Service) Decide(ctx context.Context, actor, id string, in DecideInput) (DecisionResponse, error) {
	if in.Decision != "approve" && in.Decision != "reject" && in.Decision != "changes_requested" {
		return DecisionResponse{}, serviceError("invalid_argument", "invalid_field", "decision is approve, reject or changes_requested")
	}
	if !validKey(in.IdempotencyKey) || !validDecisionReason(in.Reason) {
		return DecisionResponse{}, serviceError("invalid_argument", "invalid_field", "a valid idempotency_key and reason of at most 500 characters are required")
	}
	a, c, reviewerLevel, err := s.authorizeApproval(ctx, actor, id, "decide")
	if err != nil {
		return DecisionResponse{}, err
	}
	if reviewerLevel != "autonomous" && reviewerLevel != "pending_review" {
		return DecisionResponse{}, serviceError("failed_precondition", "reviewer_confirmation_required", "this reviewer's decision requires a higher-level approval and was not executed")
	}
	// The opener's actor/seat and the agent must still be live. This is an
	// authorization query only and never opens/writes a Core conversation.
	openerAuth, err := s.authorize(ctx, c.OpenerActorID, c.CourseID, c.AgentID)
	if err != nil {
		return DecisionResponse{}, err
	}
	p := store.ChatDecisionParams{ActorID: normalizeActor(actor), ApprovalID: id, Decision: in.Decision, Reason: in.Reason,
		IdempotencyKey: in.IdempotencyKey, Operation: "decide", ReviewerLevel: reviewerLevel, Fingerprint: fingerprint(struct{ Decision, Reason string }{in.Decision, in.Reason})}
	if reviewerLevel == "pending_review" {
		p.ReviewApprovalID = s.newID()
	}
	if a.Subject == store.ChatSubjectAnswer && in.Decision == "changes_requested" {
		p.NewGenerationID, p.NewAssistantID = s.newID(), s.newID()
	}
	a, snap, schedule, err := s.store.DecideChatApproval(ctx, p)
	if err != nil {
		return DecisionResponse{}, mapStoreError(err)
	}
	if schedule {
		generation := a.GenerationID
		if p.NewGenerationID != "" {
			generation = p.NewGenerationID
		}
		s.dispatcher.schedule(generation)
	}
	if a.Subject == store.ChatSubjectAnswer && a.State == store.ChatApprovalApproved {
		if gen, err := s.store.ChatGeneration(ctx, a.GenerationID); err == nil && gen.Result != nil {
			s.recordAnswer(gen, snap.Conversation, *gen.Result, &openerAuth.Grant)
		}
	}
	s.hub.Publish(c.ID)
	return DecisionResponse{Approval: a, Snapshot: snap}, nil
}

func (s *Service) Review(ctx context.Context, actor, id string, in ReviewInput) (DecisionResponse, error) {
	if !validKey(in.IdempotencyKey) {
		return DecisionResponse{}, serviceError("invalid_argument", "invalid_field", "idempotency_key is required")
	}
	_, c, reviewerLevel, err := s.authorizeApproval(ctx, actor, id, "review")
	if err != nil {
		return DecisionResponse{}, err
	}
	if reviewerLevel != "autonomous" && reviewerLevel != "pending_review" {
		return DecisionResponse{}, serviceError("failed_precondition", "reviewer_confirmation_required", "this reviewer's review requires a higher-level approval and was not recorded")
	}
	if _, err := s.authorize(ctx, c.OpenerActorID, c.CourseID, c.AgentID); err != nil {
		return DecisionResponse{}, err
	}
	p := store.ChatDecisionParams{ActorID: normalizeActor(actor), ApprovalID: id, IdempotencyKey: in.IdempotencyKey, Operation: "review", ReviewerLevel: reviewerLevel, Fingerprint: fingerprint("review")}
	a, snap, err := s.store.ReviewChatApproval(ctx, p)
	if err != nil {
		return DecisionResponse{}, mapStoreError(err)
	}
	s.hub.Publish(c.ID)
	return DecisionResponse{Approval: a, Snapshot: snap}, nil
}

// ApprovalAttachment serves bytes only after fresh reviewer authorization. The
// store checks the exact approval/generation binding; the ordinary owner
// attachment route is intentionally not reused.
func (s *Service) ApprovalAttachment(ctx context.Context, actor, approvalID, attachmentID string) (Attachment, error) {
	a, _, _ := s.store.ChatApprovalRecord(ctx, approvalID)
	operation := "decide"
	if a.Mode == store.ChatApprovalModeReview {
		operation = "review"
	}
	_, _, level, err := s.authorizeApproval(ctx, actor, approvalID, operation)
	if err != nil {
		return Attachment{}, err
	}
	if level != "autonomous" && level != "pending_review" {
		return Attachment{}, serviceError("failed_precondition", "reviewer_confirmation_required", "this reviewer cannot read proposal attachments before a higher-level approval")
	}
	attachment, err := s.store.ChatApprovalAttachment(ctx, approvalID, attachmentID)
	if err != nil {
		return Attachment{}, mapStoreError(err)
	}
	return attachment, nil
}
