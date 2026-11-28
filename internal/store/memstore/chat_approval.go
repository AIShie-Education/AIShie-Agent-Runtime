package memstore

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

func copyChatApproval(a store.ChatApproval) store.ChatApproval {
	if a.DecidedAt != nil {
		t := *a.DecidedAt
		a.DecidedAt = &t
	}
	return a
}

func chatDecisionState(decision string) string {
	if decision == "reject" {
		return store.ChatApprovalRejected
	}
	return decision
}

func (s *Store) ChatApprovalRecord(_ context.Context, id string) (store.ChatApproval, store.ChatConversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.chatApprovals[id]
	if !ok {
		return store.ChatApproval{}, store.ChatConversation{}, store.ErrNotFound
	}
	c, ok := s.chatConversations[a.ConversationID]
	if !ok {
		return store.ChatApproval{}, store.ChatConversation{}, store.ErrNotFound
	}
	return copyChatApproval(a), copyChatConversation(c), nil
}

func (s *Store) ListChatApprovals(_ context.Context, course, state, after string, limit int) ([]store.ChatApproval, string, error) {
	if limit <= 0 {
		limit = 50
	}
	var at = time.Time{}
	var id string
	var err error
	if after != "" {
		at, id, err = store.ParseChatCursor(after)
		if err != nil {
			return nil, "", err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []store.ChatApproval{}
	for _, a := range s.chatApprovals {
		if a.CourseID != course || state != "" && a.State != state {
			continue
		}
		if after != "" && (a.CreatedAt.Before(at) || a.CreatedAt.Equal(at) && a.ID <= id) {
			continue
		}
		out = append(out, copyChatApproval(a))
	}
	slices.SortFunc(out, func(a, b store.ChatApproval) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = store.EncodeChatCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func (s *Store) chatApprovalReplay(p store.ChatDecisionParams, course, operation string) (bool, error) {
	old, ok := s.chatApprovalActions[chatActionKey{course, p.ActorID, p.IdempotencyKey}]
	if !ok {
		return false, nil
	}
	if old.approval != p.ApprovalID || old.operation != operation || old.fingerprint != p.Fingerprint {
		return false, store.ErrChatIdempotency
	}
	return true, nil
}

func (s *Store) DecideChatApproval(_ context.Context, p store.ChatDecisionParams) (store.ChatApproval, store.ChatSnapshot, bool, error) {
	if p.ActorID == "" || p.ApprovalID == "" || p.IdempotencyKey == "" || p.Fingerprint == "" || p.ReviewerLevel == "pending_review" && p.ReviewApprovalID == "" {
		return store.ChatApproval{}, store.ChatSnapshot{}, false, errors.New("store: decision actor, approval, key and fingerprint required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.chatApprovals[p.ApprovalID]
	if !ok {
		return store.ChatApproval{}, store.ChatSnapshot{}, false, store.ErrNotFound
	}
	c := s.chatConversations[a.ConversationID]
	replay, err := s.chatApprovalReplay(p, c.CourseID, "decide")
	if err != nil {
		return store.ChatApproval{}, store.ChatSnapshot{}, false, err
	}
	if replay {
		snap, err := s.chatSnapshotLocked(c.CourseID, c.ID, c.OpenerActorID, 50, 0)
		return copyChatApproval(a), snap, false, err
	}
	if a.Mode != store.ChatApprovalModeDecision || a.State != store.ChatApprovalPending || c.State != store.ChatReplyPendingApproval {
		return store.ChatApproval{}, store.ChatSnapshot{}, false, store.ErrChatApproval
	}
	g, ok := s.chatGenerations[a.GenerationID]
	if !ok {
		return store.ChatApproval{}, store.ChatSnapshot{}, false, store.ErrChatApproval
	}
	rows := s.chatMessages[c.ID]
	u := latestChatUser(rows)
	if u == nil || u.ID != g.UserMessageID || u.Status == store.ChatRetracted {
		return store.ChatApproval{}, store.ChatSnapshot{}, false, store.ErrChatApproval
	}
	now := s.clock()
	schedule := false
	messageIndex := -1
	for i := range rows {
		if rows[i].ID == a.MessageID {
			messageIndex = i
		}
	}
	if messageIndex < 0 {
		return store.ChatApproval{}, store.ChatSnapshot{}, false, store.ErrChatApproval
	}
	m := &rows[messageIndex]
	if p.Decision != "approve" && p.Decision != "reject" && p.Decision != "changes_requested" {
		return store.ChatApproval{}, store.ChatSnapshot{}, false, store.ErrChatApproval
	}
	switch a.Subject {
	case store.ChatSubjectQuestion:
		if m.Role != store.ChatRoleUser || m.Status != store.ChatPending || g.State != store.ChatGenerationQueued {
			return store.ChatApproval{}, store.ChatSnapshot{}, false, store.ErrChatApproval
		}
		switch p.Decision {
		case "approve":
			m.Status = store.ChatPosted
			c.State = store.ChatQueued
			a.State = store.ChatApprovalApproved
			schedule = true
		default:
			m.Status = store.ChatMsgFailed
			m.Error = &store.ChatMessageError{Kind: p.Decision}
			g.State = store.ChatGenerationCancelled
			c.State = store.ChatAnswered
			a.State = store.ChatApprovalChanges
			if p.Decision == "reject" {
				a.State = store.ChatApprovalRejected
			}
		}
	case store.ChatSubjectAnswer:
		if m.Role != store.ChatRoleAssistant || m.Status != store.ChatPending || g.State != store.ChatGenerationProposed {
			return store.ChatApproval{}, store.ChatSnapshot{}, false, store.ErrChatApproval
		}
		switch p.Decision {
		case "approve":
			m.Status = store.ChatPosted
			g.State = store.ChatGenerationDone
			c.State = store.ChatAnswered
			a.State = store.ChatApprovalApproved
		case "reject":
			m.Status = store.ChatMsgFailed
			m.Body = ""
			m.Error = &store.ChatMessageError{Kind: p.Decision}
			g.State = store.ChatGenerationFailed
			c.State = store.ChatFailed
			a.State = store.ChatApprovalRejected
		case "changes_requested":
			if p.NewGenerationID == "" || p.NewAssistantID == "" {
				return store.ChatApproval{}, store.ChatSnapshot{}, false, store.ErrChatApproval
			}
			m.Status = store.ChatMsgFailed
			m.Body = ""
			m.Error = &store.ChatMessageError{Kind: p.Decision}
			g.State = store.ChatGenerationCancelled
			s.chatGenerations[p.NewGenerationID] = store.ChatGeneration{ID: p.NewGenerationID, ConversationID: c.ID, UserMessageID: g.UserMessageID, AssistantMsgID: p.NewAssistantID, AnswerLevel: g.AnswerLevel, RevisionReason: p.Reason, RevisionBody: g.Result.Body, RevisionOfGenerationID: g.ID, State: store.ChatGenerationQueued, CreatedAt: now, UpdatedAt: now}
			c.State = store.ChatQueued
			a.State = chatDecisionState(p.Decision)
			schedule = true
		}
	default:
		return store.ChatApproval{}, store.ChatSnapshot{}, false, store.ErrChatApproval
	}
	a.Reason, a.DecidedAt = p.Reason, &now
	s.chatApprovals[a.ID] = a
	s.chatMessages[c.ID] = rows
	s.chatGenerations[g.ID] = g
	c.Revision++
	c.UpdatedAt = now
	c.LastActivityAt = now
	s.chatConversations[c.ID] = c
	delete(s.chatDrafts, c.ID)
	if p.ReviewerLevel == "pending_review" {
		s.chatApprovals[p.ReviewApprovalID] = store.ChatApproval{ID: p.ReviewApprovalID, CourseID: c.CourseID, ConversationID: c.ID,
			Subject: a.Subject, MessageID: a.MessageID, GenerationID: a.GenerationID, Mode: store.ChatApprovalModeReview, State: store.ChatApprovalPending, CreatedAt: now}
	}
	s.chatApprovalActions[chatActionKey{c.CourseID, p.ActorID, p.IdempotencyKey}] = chatActionRow{approval: a.ID, operation: "decide", fingerprint: p.Fingerprint}
	snap, err := s.chatSnapshotLocked(c.CourseID, c.ID, c.OpenerActorID, 50, 0)
	return copyChatApproval(a), snap, schedule, err
}

func (s *Store) ChatApprovalAttachment(_ context.Context, approvalID, attachmentID string) (store.ChatAttachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.chatApprovals[approvalID]
	if !ok {
		return store.ChatAttachment{}, store.ErrNotFound
	}
	g, ok := s.chatGenerations[a.GenerationID]
	if !ok {
		return store.ChatAttachment{}, store.ErrNotFound
	}
	attachment, ok := s.chatAttachments[attachmentID]
	if !ok || attachment.CourseID != a.CourseID {
		return store.ChatAttachment{}, store.ErrNotFound
	}
	if attachment.BoundMessage != a.MessageID && (a.Subject != store.ChatSubjectAnswer || attachment.BoundMessage != g.UserMessageID) {
		return store.ChatAttachment{}, store.ErrNotFound
	}
	return copyChatAttachment(attachment, true), nil
}

func (s *Store) ReviewChatApproval(_ context.Context, p store.ChatDecisionParams) (store.ChatApproval, store.ChatSnapshot, error) {
	if p.ActorID == "" || p.ApprovalID == "" || p.IdempotencyKey == "" || p.Fingerprint == "" {
		return store.ChatApproval{}, store.ChatSnapshot{}, errors.New("store: review actor, approval, key and fingerprint required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.chatApprovals[p.ApprovalID]
	if !ok {
		return store.ChatApproval{}, store.ChatSnapshot{}, store.ErrNotFound
	}
	c := s.chatConversations[a.ConversationID]
	replay, err := s.chatApprovalReplay(p, c.CourseID, "review")
	if err != nil {
		return store.ChatApproval{}, store.ChatSnapshot{}, err
	}
	if !replay {
		if a.Mode != store.ChatApprovalModeReview || a.State != store.ChatApprovalPending || c.State == store.ChatClosed {
			return store.ChatApproval{}, store.ChatSnapshot{}, store.ErrChatApproval
		}
		g, ok := s.chatGenerations[a.GenerationID]
		u := latestChatUser(s.chatMessages[c.ID])
		if !ok || u == nil || u.ID != g.UserMessageID || u.Status == store.ChatRetracted {
			return store.ChatApproval{}, store.ChatSnapshot{}, store.ErrChatApproval
		}
		now := s.clock()
		a.State = store.ChatApprovalReviewed
		a.DecidedAt = &now
		s.chatApprovals[a.ID] = a
		c.Revision++
		c.UpdatedAt = now
		s.chatConversations[c.ID] = c
		s.chatApprovalActions[chatActionKey{c.CourseID, p.ActorID, p.IdempotencyKey}] = chatActionRow{approval: a.ID, operation: "review", fingerprint: p.Fingerprint}
	}
	snap, err := s.chatSnapshotLocked(c.CourseID, c.ID, c.OpenerActorID, 50, 0)
	return copyChatApproval(a), snap, err
}
