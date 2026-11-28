package memstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

type chatIdemKey struct{ course, actor, key string }
type chatIdemRow struct {
	op, fingerprint, conversation, message string
	submission                             store.ChatSubmission
}
type chatActionKey struct{ course, actor, key string }
type chatActionRow struct{ approval, operation, fingerprint string }

func copyChatConversation(c store.ChatConversation) store.ChatConversation {
	if c.ClosedAt != nil {
		t := *c.ClosedAt
		c.ClosedAt = &t
	}
	return c
}

func copyChatUsage(u *store.ChatUsage) *store.ChatUsage {
	if u == nil {
		return nil
	}
	v := *u
	return &v
}

func copyChatResult(r *store.ChatResult) *store.ChatResult {
	if r == nil {
		return nil
	}
	v := *r
	v.Sources = slices.Clone(r.Sources)
	if v.SourcesStated != nil && *v.SourcesStated && v.Sources == nil {
		v.Sources = []store.ChatSource{}
	}
	v.Usage = copyChatUsage(r.Usage)
	if r.SourcesStated != nil {
		b := *r.SourcesStated
		v.SourcesStated = &b
	}
	return &v
}

func copyChatAttachment(a store.ChatAttachment, bytes bool) store.ChatAttachment {
	if bytes {
		a.Bytes = slices.Clone(a.Bytes)
	} else {
		a.Bytes = nil
	}
	return a
}

func copyChatMessageFixed(in store.ChatMessage) store.ChatMessage {
	m := in
	m.Attachments = slices.Clone(in.Attachments)
	for i := range m.Attachments {
		m.Attachments[i].Bytes = nil
	}
	m.Sources = slices.Clone(in.Sources)
	m.Usage = copyChatUsage(in.Usage)
	if in.SourcesStated != nil {
		b := *in.SourcesStated
		m.SourcesStated = &b
	}
	if in.Error != nil {
		e := *in.Error
		m.Error = &e
	}
	if in.RetractedAt != nil {
		t := *in.RetractedAt
		m.RetractedAt = &t
	}
	return m
}

func publicChatMessage(m store.ChatMessage) store.ChatMessage {
	m = copyChatMessageFixed(m)
	if m.Role == store.ChatRoleAssistant && m.Status == store.ChatPending {
		m.Body, m.Sources, m.SourcesStated, m.Usage, m.FinishReason = "", nil, nil, nil, ""
	}
	return m
}

func copyChatDraft(d store.ChatDraft) store.ChatDraft {
	d.Steps = slices.Clone(d.Steps)
	return d
}

func (s *Store) chatSnapshotLocked(course, conv, actor string, limit int, before int64) (store.ChatSnapshot, error) {
	c, ok := s.chatConversations[conv]
	if !ok || c.CourseID != course || c.OpenerActorID != actor {
		return store.ChatSnapshot{}, fmt.Errorf("chat conversation %s: %w", conv, store.ErrNotFound)
	}
	if limit <= 0 {
		limit = 50
	}
	rows := s.chatMessages[conv]
	eligible := make([]store.ChatMessage, 0, len(rows))
	for _, m := range rows {
		if before <= 0 || m.Seq < before {
			eligible = append(eligible, publicChatMessage(m))
		}
	}
	more := len(eligible) > limit
	if more {
		eligible = eligible[len(eligible)-limit:]
	}
	if eligible == nil {
		eligible = []store.ChatMessage{}
	}
	out := store.ChatSnapshot{Conversation: copyChatConversation(c), Messages: eligible, More: more}
	if d, ok := s.chatDrafts[conv]; ok {
		v := copyChatDraft(d)
		out.Draft = &v
	}
	return out, nil
}

func chatIdemReplay(existing chatIdemRow, ok bool, op, fingerprint, conv string) (bool, error) {
	if !ok {
		return false, nil
	}
	if existing.op != op || existing.fingerprint != fingerprint || existing.conversation != conv && conv != "" {
		return false, store.ErrChatIdempotency
	}
	return true, nil
}

func (s *Store) bindChatAttachmentsLocked(actor, course, message string, ids []string) ([]store.ChatAttachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(ids))
	images := 0
	out := make([]store.ChatAttachment, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			return nil, fmt.Errorf("attachment %s duplicated: %w", id, store.ErrChatAttachments)
		}
		seen[id] = true
		a, ok := s.chatAttachments[id]
		if !ok || a.CourseID != course || a.OwnerActorID != actor {
			return nil, fmt.Errorf("chat attachment %s: %w", id, store.ErrNotFound)
		}
		if a.BoundMessage != "" {
			return nil, fmt.Errorf("attachment %s already bound: %w", id, store.ErrChatAttachments)
		}
		if strings.HasPrefix(a.MediaType, "image/") {
			images++
			if images > 4 {
				return nil, fmt.Errorf("more than four chat images: %w", store.ErrChatAttachments)
			}
		}
		out = append(out, a)
	}
	// Validate the whole set before changing any attachment, preserving the
	// same all-or-none behaviour as a PostgreSQL transaction.
	for i := range out {
		out[i].BoundMessage, out[i].Position = message, i
		s.chatAttachments[out[i].ID] = out[i]
		out[i] = copyChatAttachment(out[i], false)
	}
	return out, nil
}

func copySubmission(s store.ChatSubmission) *store.ChatSubmission {
	v := s
	if s.ReviewState != nil {
		text := *s.ReviewState
		v.ReviewState = &text
	}
	return &v
}

func (s *Store) applyQuestionApproval(c store.ChatConversation, m store.ChatMessage, generationID string, sub store.ChatSubmission, approval *store.ChatApproval, now time.Time) (store.ChatConversation, store.ChatMessage) {
	if approval == nil {
		return c, m
	}
	a := *approval
	a.CourseID, a.ConversationID, a.MessageID, a.GenerationID = c.CourseID, c.ID, m.ID, generationID
	if a.State == "" {
		a.State = store.ChatApprovalPending
	}
	if a.Mode == "" {
		a.Mode = store.ChatApprovalModeDecision
	}
	a.CreatedAt = s.orNow(a.CreatedAt)
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	if a.Mode == store.ChatApprovalModeDecision && a.State == store.ChatApprovalPending {
		m.Status, c.State = store.ChatPending, store.ChatReplyPendingApproval
	}
	s.chatApprovals[a.ID] = a
	return c, m
}

func (s *Store) CreateChatConversation(_ context.Context, p store.ChatCreateParams) (store.ChatSnapshot, bool, error) {
	if p.Conversation.ID == "" || p.Conversation.CourseID == "" || p.Conversation.AgentID == "" || p.Conversation.OpenerActorID == "" ||
		p.Message.ID == "" || p.GenerationID == "" || p.AssistantMsgID == "" || p.IdempotencyKey == "" || p.RequestFingerprint == "" {
		return store.ChatSnapshot{}, false, errors.New("store: chat create ids and idempotency key required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	course, actor := p.Conversation.CourseID, p.Conversation.OpenerActorID
	k := chatIdemKey{course, actor, p.IdempotencyKey}
	old, ok := s.chatIdempotency[k]
	replay, err := chatIdemReplay(old, ok, "create", p.RequestFingerprint, "")
	if err != nil {
		return store.ChatSnapshot{}, false, err
	}
	if replay {
		snap, err := s.chatSnapshotLocked(course, old.conversation, actor, 50, 0)
		snap.Submission = copySubmission(old.submission)
		return snap, true, err
	}
	if _, exists := s.chatConversations[p.Conversation.ID]; exists {
		return store.ChatSnapshot{}, false, store.ErrExists
	}
	now := s.clock()
	c := copyChatConversation(p.Conversation)
	c.CreatedAt, c.UpdatedAt, c.LastActivityAt = s.orNow(c.CreatedAt), now, now
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt, c.LastActivityAt = c.CreatedAt, c.CreatedAt
	c.State, c.Revision = store.ChatQueued, 1
	m := copyChatMessageFixed(p.Message)
	m.ConversationID, m.Seq, m.Role, m.Status = c.ID, 1, store.ChatRoleUser, store.ChatPosted
	m.CreatedAt = s.orNow(m.CreatedAt)
	atts, err := s.bindChatAttachmentsLocked(actor, course, m.ID, p.AttachmentIDs)
	if err != nil {
		return store.ChatSnapshot{}, false, err
	}
	m.Attachments = atts
	c, m = s.applyQuestionApproval(c, m, p.GenerationID, p.Submission, p.Approval, now)
	s.chatConversations[c.ID] = c
	s.chatMessages[c.ID] = []store.ChatMessage{m}
	s.chatGenerations[p.GenerationID] = store.ChatGeneration{ID: p.GenerationID, ConversationID: c.ID,
		UserMessageID: m.ID, AssistantMsgID: p.AssistantMsgID, AnswerLevel: p.AnswerLevel, State: store.ChatGenerationQueued, CreatedAt: now, UpdatedAt: now}
	s.chatIdempotency[k] = chatIdemRow{op: "create", fingerprint: p.RequestFingerprint, conversation: c.ID, message: m.ID, submission: p.Submission}
	snap, err := s.chatSnapshotLocked(course, c.ID, actor, 50, 0)
	if p.Submission.Status != "" {
		snap.Submission = copySubmission(p.Submission)
	}
	return snap, false, err
}

func (s *Store) AppendChatMessage(_ context.Context, p store.ChatAppendParams) (store.ChatSnapshot, bool, error) {
	if p.CourseID == "" || p.ConversationID == "" || p.ActorID == "" || p.Message.ID == "" || p.GenerationID == "" ||
		p.AssistantMsgID == "" || p.IdempotencyKey == "" || p.RequestFingerprint == "" {
		return store.ChatSnapshot{}, false, errors.New("store: chat append ids and idempotency key required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := chatIdemKey{p.CourseID, p.ActorID, p.IdempotencyKey}
	old, ok := s.chatIdempotency[k]
	replay, err := chatIdemReplay(old, ok, "ask", p.RequestFingerprint, p.ConversationID)
	if err != nil {
		return store.ChatSnapshot{}, false, err
	}
	if replay {
		snap, err := s.chatSnapshotLocked(p.CourseID, old.conversation, p.ActorID, 50, 0)
		snap.Submission = copySubmission(old.submission)
		return snap, true, err
	}
	c, ok := s.chatConversations[p.ConversationID]
	if !ok || c.CourseID != p.CourseID || c.OpenerActorID != p.ActorID {
		return store.ChatSnapshot{}, false, fmt.Errorf("chat conversation %s: %w", p.ConversationID, store.ErrNotFound)
	}
	if c.State == store.ChatClosed || c.State == store.ChatQueued || c.State == store.ChatAnswering || c.State == store.ChatReplyPendingApproval {
		return store.ChatSnapshot{}, false, store.ErrChatNotAwaiting
	}
	if _, exists := s.chatGenerations[p.GenerationID]; exists {
		return store.ChatSnapshot{}, false, store.ErrExists
	}
	now := s.clock()
	m := copyChatMessageFixed(p.Message)
	m.ConversationID, m.Seq, m.Role, m.Status = c.ID, int64(len(s.chatMessages[c.ID]))+1, store.ChatRoleUser, store.ChatPosted
	m.CreatedAt = s.orNow(m.CreatedAt)
	atts, err := s.bindChatAttachmentsLocked(p.ActorID, p.CourseID, m.ID, p.AttachmentIDs)
	if err != nil {
		return store.ChatSnapshot{}, false, err
	}
	m.Attachments = atts
	s.chatMessages[c.ID] = append(s.chatMessages[c.ID], m)
	c.State, c.Revision = store.ChatQueued, c.Revision+1
	c, m = s.applyQuestionApproval(c, m, p.GenerationID, p.Submission, p.Approval, now)
	s.chatMessages[c.ID][len(s.chatMessages[c.ID])-1] = m
	c.UpdatedAt, c.LastActivityAt = now, now
	s.chatConversations[c.ID] = c
	s.chatGenerations[p.GenerationID] = store.ChatGeneration{ID: p.GenerationID, ConversationID: c.ID,
		UserMessageID: m.ID, AssistantMsgID: p.AssistantMsgID, AnswerLevel: p.AnswerLevel, State: store.ChatGenerationQueued, CreatedAt: now, UpdatedAt: now}
	s.chatIdempotency[k] = chatIdemRow{op: "ask", fingerprint: p.RequestFingerprint, conversation: c.ID, message: m.ID, submission: p.Submission}
	delete(s.chatDrafts, c.ID)
	snap, err := s.chatSnapshotLocked(p.CourseID, c.ID, p.ActorID, 50, 0)
	if p.Submission.Status != "" {
		snap.Submission = copySubmission(p.Submission)
	}
	return snap, false, err
}

func (s *Store) ChatConversation(_ context.Context, course, conv, actor string) (store.ChatConversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chatConversations[conv]
	if !ok || c.CourseID != course || c.OpenerActorID != actor {
		return store.ChatConversation{}, fmt.Errorf("chat conversation %s: %w", conv, store.ErrNotFound)
	}
	return copyChatConversation(c), nil
}

func (s *Store) ListChatConversations(_ context.Context, course, actor, after string, limit int) ([]store.ChatConversation, string, error) {
	if limit <= 0 {
		limit = 50
	}
	var at time.Time
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
	rows := make([]store.ChatConversation, 0)
	for _, c := range s.chatConversations {
		if c.CourseID != course || c.OpenerActorID != actor {
			continue
		}
		if after != "" && (c.LastActivityAt.After(at) || c.LastActivityAt.Equal(at) && c.ID >= id) {
			continue
		}
		rows = append(rows, copyChatConversation(c))
	}
	slices.SortFunc(rows, func(a, b store.ChatConversation) int {
		return cmp.Or(b.LastActivityAt.Compare(a.LastActivityAt), strings.Compare(b.ID, a.ID))
	})
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = store.EncodeChatCursor(last.LastActivityAt, last.ID)
	}
	if rows == nil {
		rows = []store.ChatConversation{}
	}
	return rows, next, nil
}

func (s *Store) ChatSnapshot(_ context.Context, course, conv, actor string, limit int, before int64) (store.ChatSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chatSnapshotLocked(course, conv, actor, limit, before)
}

func (s *Store) StageChatAttachment(_ context.Context, a store.ChatAttachment) (store.ChatAttachment, error) {
	if err := store.CheckChatAttachment(a); err != nil {
		return store.ChatAttachment{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.chatAttachments[a.ID]; ok {
		return store.ChatAttachment{}, store.ErrExists
	}
	a.CreatedAt = s.orNow(a.CreatedAt)
	a.Position = -1
	s.chatAttachments[a.ID] = copyChatAttachment(a, true)
	return copyChatAttachment(a, false), nil
}

func (s *Store) StageChatAttachmentIdempotent(_ context.Context, a store.ChatAttachment, key, fp string) (store.ChatAttachment, bool, error) {
	if key == "" || fp == "" {
		return store.ChatAttachment{}, false, errors.New("store: chat attachment idempotency key and fingerprint required")
	}
	if err := store.CheckChatAttachment(a); err != nil {
		return store.ChatAttachment{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.chatAttachments {
		if old.CourseID == a.CourseID && old.OwnerActorID == a.OwnerActorID && old.IdempotencyKey == key {
			if old.RequestFingerprint != fp {
				return store.ChatAttachment{}, false, store.ErrChatIdempotency
			}
			return copyChatAttachment(old, false), true, nil
		}
	}
	if _, ok := s.chatAttachments[a.ID]; ok {
		return store.ChatAttachment{}, false, store.ErrExists
	}
	a.CreatedAt, a.Position = s.orNow(a.CreatedAt), -1
	a.IdempotencyKey, a.RequestFingerprint = key, fp
	s.chatAttachments[a.ID] = copyChatAttachment(a, true)
	return copyChatAttachment(a, false), false, nil
}

func (s *Store) ChatAttachment(_ context.Context, course, id, actor string) (store.ChatAttachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.chatAttachments[id]
	if !ok || a.CourseID != course || a.OwnerActorID != actor {
		return store.ChatAttachment{}, fmt.Errorf("chat attachment %s: %w", id, store.ErrNotFound)
	}
	return copyChatAttachment(a, true), nil
}

func (s *Store) WithdrawChatMessage(_ context.Context, course, conv, actor, message, reason string) (store.ChatSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chatConversations[conv]
	if !ok || c.CourseID != course || c.OpenerActorID != actor {
		return store.ChatSnapshot{}, fmt.Errorf("chat conversation %s: %w", conv, store.ErrNotFound)
	}
	rows := s.chatMessages[conv]
	if len(rows) == 0 {
		return store.ChatSnapshot{}, store.ErrChatWithdraw
	}
	last := latestChatUser(rows)
	if last == nil {
		return store.ChatSnapshot{}, store.ErrChatWithdraw
	}
	if last.ID == message && last.Status == store.ChatRetracted {
		return s.chatSnapshotLocked(course, conv, actor, 50, 0)
	}
	if last.ID != message || last.Status != store.ChatPosted && last.Status != store.ChatPending ||
		(c.State != store.ChatQueued && c.State != store.ChatAnswering && c.State != store.ChatFailed && c.State != store.ChatReplyPendingApproval) {
		return store.ChatSnapshot{}, store.ErrChatWithdraw
	}
	now := s.clock()
	last.Status, last.Body, last.RetractedAt, last.RetractedReason = store.ChatRetracted, "", &now, reason
	s.chatMessages[conv] = rows
	for id, g := range s.chatGenerations {
		if g.ConversationID == conv && g.UserMessageID == message && (g.State == store.ChatGenerationQueued || g.State == store.ChatGenerationRunning || g.State == store.ChatGenerationFailed || g.State == store.ChatGenerationProposed) {
			g.State, g.UpdatedAt = store.ChatGenerationCancelled, now
			s.chatGenerations[id] = g
		}
	}
	for id, a := range s.chatApprovals {
		if a.ConversationID == conv && a.State == store.ChatApprovalPending {
			a.State = store.ChatApprovalCancelled
			a.DecidedAt = &now
			s.chatApprovals[id] = a
		}
	}
	for i := range rows {
		if rows[i].Role == store.ChatRoleAssistant && rows[i].Status == store.ChatPending {
			rows[i].Status = store.ChatMsgFailed
			rows[i].Body = ""
			rows[i].Error = &store.ChatMessageError{Kind: "withdrawn"}
		}
	}
	delete(s.chatDrafts, conv)
	c.State, c.Revision, c.UpdatedAt, c.LastActivityAt = store.ChatAnswered, c.Revision+1, now, now
	s.chatConversations[conv] = c
	return s.chatSnapshotLocked(course, conv, actor, 50, 0)
}

func (s *Store) CloseChatConversation(_ context.Context, course, conv, actor, reason string) (store.ChatConversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chatConversations[conv]
	if !ok || c.CourseID != course || c.OpenerActorID != actor {
		return store.ChatConversation{}, fmt.Errorf("chat conversation %s: %w", conv, store.ErrNotFound)
	}
	if c.State == store.ChatClosed {
		return copyChatConversation(c), nil
	}
	now := s.clock()
	c.State, c.Revision, c.UpdatedAt, c.LastActivityAt, c.ClosedAt, c.CloseReason = store.ChatClosed, c.Revision+1, now, now, &now, reason
	s.chatConversations[conv] = c
	delete(s.chatDrafts, conv)
	for id, g := range s.chatGenerations {
		if g.ConversationID == conv && (g.State == store.ChatGenerationQueued || g.State == store.ChatGenerationRunning || g.State == store.ChatGenerationProposed) {
			g.State, g.UpdatedAt = store.ChatGenerationCancelled, now
			s.chatGenerations[id] = g
		}
	}
	for id, a := range s.chatApprovals {
		if a.ConversationID == conv && a.State == store.ChatApprovalPending {
			a.State = store.ChatApprovalCancelled
			a.DecidedAt = &now
			s.chatApprovals[id] = a
		}
	}
	for id, messages := range s.chatMessages {
		if id == conv {
			for i := range messages {
				if messages[i].Role == store.ChatRoleAssistant && messages[i].Status == store.ChatPending {
					messages[i].Status = store.ChatMsgFailed
					messages[i].Body = ""
					messages[i].Error = &store.ChatMessageError{Kind: "closed"}
				}
			}
			s.chatMessages[id] = messages
		}
	}
	return copyChatConversation(c), nil
}

func copyChatGeneration(g store.ChatGeneration) store.ChatGeneration {
	g.Result = copyChatResult(g.Result)
	if g.Error != nil {
		e := *g.Error
		g.Error = &e
	}
	return g
}

func (s *Store) ChatGeneration(_ context.Context, id string) (store.ChatGeneration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.chatGenerations[id]
	if !ok {
		return store.ChatGeneration{}, fmt.Errorf("chat generation %s: %w", id, store.ErrNotFound)
	}
	return copyChatGeneration(g), nil
}

func (s *Store) RecoverChatGenerations(_ context.Context, limit int) ([]store.ChatGeneration, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]store.ChatGeneration, 0)
	for _, g := range s.chatGenerations {
		if g.State == store.ChatGenerationQueued || g.State == store.ChatGenerationRunning {
			rows = append(rows, copyChatGeneration(g))
		}
	}
	slices.SortFunc(rows, func(a, b store.ChatGeneration) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func latestChatUser(rows []store.ChatMessage) *store.ChatMessage {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Role == store.ChatRoleUser {
			return &rows[i]
		}
	}
	return nil
}

func (s *Store) BeginChatGeneration(_ context.Context, id, holder string, ttl time.Duration) (store.ChatGeneration, store.ChatConversation, bool, error) {
	if holder == "" || ttl <= 0 {
		return store.ChatGeneration{}, store.ChatConversation{}, false, errors.New("store: chat generation holder and positive ttl required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.chatGenerations[id]
	if !ok {
		return g, store.ChatConversation{}, false, store.ErrNotFound
	}
	c := s.chatConversations[g.ConversationID]
	now := s.clock()
	if g.State != store.ChatGenerationQueued && g.State != store.ChatGenerationRunning || c.State == store.ChatClosed ||
		g.State == store.ChatGenerationRunning && now.Before(g.LeaseExpiresAt) && g.Holder != holder {
		return copyChatGeneration(g), copyChatConversation(c), false, nil
	}
	rows := s.chatMessages[c.ID]
	latestUser := latestChatUser(rows)
	if latestUser == nil || latestUser.ID != g.UserMessageID || latestUser.Status != store.ChatPosted {
		return copyChatGeneration(g), copyChatConversation(c), false, nil
	}
	g.State, g.Attempt, g.Fence, g.Holder, g.LeaseExpiresAt, g.UpdatedAt = store.ChatGenerationRunning, g.Attempt+1, g.Fence+1, holder, now.Add(ttl), now
	s.chatGenerations[id] = g
	c.State, c.Revision, c.UpdatedAt, c.LastActivityAt = store.ChatAnswering, c.Revision+1, now, now
	s.chatConversations[c.ID] = c
	return copyChatGeneration(g), copyChatConversation(c), true, nil
}

func (s *Store) HeartbeatChatGeneration(_ context.Context, id, holder string, fence int64, ttl time.Duration) (bool, error) {
	if holder == "" || fence <= 0 || ttl <= 0 {
		return false, errors.New("store: chat generation heartbeat holder, fence and positive ttl required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.chatGenerations[id]
	if !ok {
		return false, store.ErrNotFound
	}
	now := s.clock()
	if g.State != store.ChatGenerationRunning || g.Holder != holder || g.Fence != fence || !now.Before(g.LeaseExpiresAt) {
		return false, nil
	}
	g.LeaseExpiresAt, g.UpdatedAt = now.Add(ttl), now
	s.chatGenerations[id] = g
	return true, nil
}

func chatGenerationWritable(g store.ChatGeneration, c store.ChatConversation, rows []store.ChatMessage, holder string, fence int64, now time.Time) bool {
	latestUser := latestChatUser(rows)
	return g.State == store.ChatGenerationRunning && g.Holder == holder && g.Fence == fence && now.Before(g.LeaseExpiresAt) &&
		c.State != store.ChatClosed && latestUser != nil && latestUser.ID == g.UserMessageID && latestUser.Status == store.ChatPosted
}

func (s *Store) PutChatDraft(_ context.Context, id, holder string, fence int64, text string, steps []store.ChatStep) (store.ChatConversation, store.ChatDraft, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.chatGenerations[id]
	if !ok {
		return store.ChatConversation{}, store.ChatDraft{}, false, store.ErrNotFound
	}
	c := s.chatConversations[g.ConversationID]
	rows := s.chatMessages[c.ID]
	now := s.clock()
	if !chatGenerationWritable(g, c, rows, holder, fence, now) {
		return copyChatConversation(c), store.ChatDraft{}, false, nil
	}
	d := s.chatDrafts[c.ID]
	d.Version++
	if d.Version == 0 {
		d.Version = 1
	}
	d.Text, d.Steps, d.UpdatedAt = text, slices.Clone(steps), now
	s.chatDrafts[c.ID] = d
	c.Revision, c.UpdatedAt = c.Revision+1, now
	s.chatConversations[c.ID] = c
	return copyChatConversation(c), copyChatDraft(d), true, nil
}

func (s *Store) WriteChatResult(_ context.Context, id, holder string, fence int64, result store.ChatResult) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.chatGenerations[id]
	if !ok {
		return false, store.ErrNotFound
	}
	c := s.chatConversations[g.ConversationID]
	rows := s.chatMessages[c.ID]
	now := s.clock()
	if !chatGenerationWritable(g, c, rows, holder, fence, now) {
		return false, nil
	}
	g.Result, g.UpdatedAt = copyChatResult(&result), now
	s.chatGenerations[id] = g
	return true, nil
}

func (s *Store) finalizeChatGenerationLocked(id, holder string, fence int64, review *store.ChatApproval) (store.ChatSnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.chatGenerations[id]
	if !ok {
		return store.ChatSnapshot{}, false, store.ErrNotFound
	}
	c := s.chatConversations[g.ConversationID]
	if g.State == store.ChatGenerationDone {
		snap, err := s.chatSnapshotLocked(c.CourseID, c.ID, c.OpenerActorID, 50, 0)
		return snap, false, err
	}
	rows := s.chatMessages[c.ID]
	now := s.clock()
	if g.Result == nil || !chatGenerationWritable(g, c, rows, holder, fence, now) {
		return store.ChatSnapshot{}, false, nil
	}
	r := g.Result
	m := store.ChatMessage{ID: g.AssistantMsgID, ConversationID: c.ID, Seq: int64(len(rows)) + 1,
		Role: store.ChatRoleAssistant, Status: store.ChatPosted, Body: r.Body, CreatedAt: now,
		Sources: slices.Clone(r.Sources), SourcesStated: r.SourcesStated, Usage: copyChatUsage(r.Usage), FinishReason: r.FinishReason}
	s.chatMessages[c.ID] = append(rows, copyChatMessageFixed(m))
	g.State, g.UpdatedAt = store.ChatGenerationDone, now
	s.chatGenerations[id] = g
	if review != nil {
		a := *review
		a.CourseID, a.ConversationID, a.MessageID, a.GenerationID = c.CourseID, c.ID, g.AssistantMsgID, g.ID
		a.Subject, a.Mode, a.State, a.CreatedAt = store.ChatSubjectAnswer, store.ChatApprovalModeReview, store.ChatApprovalPending, now
		s.chatApprovals[a.ID] = a
	}
	delete(s.chatDrafts, c.ID)
	c.State, c.Revision, c.UpdatedAt, c.LastActivityAt = store.ChatAnswered, c.Revision+1, now, now
	s.chatConversations[c.ID] = c
	snap, err := s.chatSnapshotLocked(c.CourseID, c.ID, c.OpenerActorID, 50, 0)
	return snap, true, err
}

func (s *Store) FinalizeChatGeneration(ctx context.Context, id, holder string, fence int64) (store.ChatSnapshot, bool, error) {
	return s.finalizeChatGenerationLocked(id, holder, fence, nil)
}

func (s *Store) FinalizeReviewedChatGeneration(ctx context.Context, id, holder string, fence int64, approval store.ChatApproval) (store.ChatSnapshot, bool, error) {
	return s.finalizeChatGenerationLocked(id, holder, fence, &approval)
}

func (s *Store) ProposeChatGeneration(_ context.Context, id, holder string, fence int64, approval store.ChatApproval) (store.ChatSnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.chatGenerations[id]
	if !ok {
		return store.ChatSnapshot{}, false, store.ErrNotFound
	}
	c := s.chatConversations[g.ConversationID]
	rows := s.chatMessages[c.ID]
	now := s.clock()
	if g.Result == nil || !chatGenerationWritable(g, c, rows, holder, fence, now) {
		return store.ChatSnapshot{}, false, nil
	}
	r := g.Result
	m := store.ChatMessage{ID: g.AssistantMsgID, ConversationID: c.ID, Seq: int64(len(rows)) + 1, Role: store.ChatRoleAssistant,
		Status: store.ChatPending, Body: r.Body, CreatedAt: now, Sources: slices.Clone(r.Sources), SourcesStated: r.SourcesStated,
		Usage: copyChatUsage(r.Usage), FinishReason: r.FinishReason}
	s.chatMessages[c.ID] = append(rows, m)
	g.State, g.UpdatedAt = store.ChatGenerationProposed, now
	s.chatGenerations[id] = g
	delete(s.chatDrafts, c.ID)
	c.State, c.Revision, c.UpdatedAt, c.LastActivityAt = store.ChatReplyPendingApproval, c.Revision+1, now, now
	s.chatConversations[c.ID] = c
	approval.CourseID, approval.ConversationID, approval.MessageID, approval.GenerationID = c.CourseID, c.ID, m.ID, g.ID
	approval.Subject, approval.Mode, approval.State, approval.CreatedAt = store.ChatSubjectAnswer, store.ChatApprovalModeDecision, store.ChatApprovalPending, now
	s.chatApprovals[approval.ID] = approval
	snap, err := s.chatSnapshotLocked(c.CourseID, c.ID, c.OpenerActorID, 50, 0)
	return snap, err == nil, err
}

func (s *Store) FailChatGeneration(_ context.Context, id, holder string, fence int64, kind string, retryable bool) (store.ChatConversation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.chatGenerations[id]
	if !ok {
		return store.ChatConversation{}, false, store.ErrNotFound
	}
	c := s.chatConversations[g.ConversationID]
	if g.State == store.ChatGenerationFailed || g.State == store.ChatGenerationDone || g.State == store.ChatGenerationCancelled {
		return copyChatConversation(c), false, nil
	}
	rows := s.chatMessages[c.ID]
	now := s.clock()
	if !chatGenerationWritable(g, c, rows, holder, fence, now) {
		return copyChatConversation(c), false, nil
	}
	if kind == "" {
		kind = "provider_failed"
	}
	g.State, g.Error, g.UpdatedAt = store.ChatGenerationFailed, &store.ChatMessageError{Kind: kind, Retryable: retryable}, now
	s.chatGenerations[id] = g
	m := store.ChatMessage{ID: g.AssistantMsgID, ConversationID: c.ID, Seq: int64(len(rows)) + 1,
		Role: store.ChatRoleAssistant, Status: store.ChatMsgFailed, CreatedAt: now, Error: g.Error}
	s.chatMessages[c.ID] = append(rows, m)
	delete(s.chatDrafts, c.ID)
	c.State, c.Revision, c.UpdatedAt, c.LastActivityAt = store.ChatFailed, c.Revision+1, now, now
	s.chatConversations[c.ID] = c
	return copyChatConversation(c), true, nil
}
