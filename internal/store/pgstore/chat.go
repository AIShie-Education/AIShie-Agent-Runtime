package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

type chatQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

const chatConversationColumns = `id, course_id, agent_id, runtime_agent_id, opener_actor_id, title, state,
	revision, created_at, updated_at, last_activity_at, closed_at, close_reason`

func scanChatConversation(row pgx.Row) (store.ChatConversation, error) {
	var c store.ChatConversation
	var closed *time.Time
	err := row.Scan(&c.ID, &c.CourseID, &c.AgentID, &c.RuntimeAgentID, &c.OpenerActorID, &c.Title, &c.State,
		&c.Revision, &c.CreatedAt, &c.UpdatedAt, &c.LastActivityAt, &closed, &c.CloseReason)
	c.ClosedAt = closed
	utc(&c.CreatedAt)
	utc(&c.UpdatedAt)
	utc(&c.LastActivityAt)
	if closed != nil {
		t := closed.UTC()
		c.ClosedAt = &t
	}
	return c, err
}

const chatMessageColumns = `id, conversation_id, seq, role, status, body, sources, sources_stated, usage,
	finish_reason, error_kind, error_retryable, created_at, retracted_at, retracted_reason`

func scanChatMessage(row pgx.Row) (store.ChatMessage, error) {
	var m store.ChatMessage
	var sources, usage []byte
	var stated, retryable *bool
	var kind string
	var retracted *time.Time
	err := row.Scan(&m.ID, &m.ConversationID, &m.Seq, &m.Role, &m.Status, &m.Body, &sources, &stated, &usage,
		&m.FinishReason, &kind, &retryable, &m.CreatedAt, &retracted, &m.RetractedReason)
	if err != nil {
		return m, err
	}
	m.CreatedAt = m.CreatedAt.UTC()
	m.RetractedAt = retracted
	if retracted != nil {
		t := retracted.UTC()
		m.RetractedAt = &t
	}
	if len(sources) > 0 {
		if err := json.Unmarshal(sources, &m.Sources); err != nil {
			return m, err
		}
	}
	if len(usage) > 0 {
		var u store.ChatUsage
		if err := json.Unmarshal(usage, &u); err != nil {
			return m, err
		}
		m.Usage = &u
	}
	m.SourcesStated = stated
	if kind != "" {
		m.Error = &store.ChatMessageError{Kind: kind, Retryable: retryable != nil && *retryable}
	}
	return m, nil
}

func publicChatMessagePG(m store.ChatMessage) store.ChatMessage {
	if m.Role == store.ChatRoleAssistant && m.Status == store.ChatPending {
		m.Body, m.Sources, m.SourcesStated, m.Usage, m.FinishReason = "", nil, nil, nil, ""
	}
	return m
}

func chatAttachmentMetadata(ctx context.Context, q chatQuerier, messageID string) ([]store.ChatAttachment, error) {
	rows, err := q.Query(ctx, `SELECT id, course_id, owner_actor_id, message_id, position, filename, media_type, size, created_at
		FROM chat_attachment WHERE message_id = $1 ORDER BY position, id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ChatAttachment
	for rows.Next() {
		var a store.ChatAttachment
		var bound *string
		if err := rows.Scan(&a.ID, &a.CourseID, &a.OwnerActorID, &bound, &a.Position, &a.Filename, &a.MediaType, &a.Size, &a.CreatedAt); err != nil {
			return nil, err
		}
		if bound != nil {
			a.BoundMessage = *bound
		}
		a.CreatedAt = a.CreatedAt.UTC()
		out = append(out, a)
	}
	if out == nil {
		out = []store.ChatAttachment{}
	}
	return out, rows.Err()
}

func chatSnapshotQ(ctx context.Context, q chatQuerier, course, conv, actor string, limit int, before int64) (store.ChatSnapshot, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	c, err := scanChatConversation(q.QueryRow(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation
		WHERE id = $1 AND course_id = $2 AND opener_actor_id = $3`, conv, course, actor))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ChatSnapshot{}, fmt.Errorf("chat conversation %s: %w", conv, store.ErrNotFound)
	}
	if err != nil {
		return store.ChatSnapshot{}, err
	}
	rows, err := q.Query(ctx, `SELECT `+chatMessageColumns+` FROM chat_message
		WHERE conversation_id = $1 AND ($2::bigint = 0 OR seq < $2)
		ORDER BY seq DESC LIMIT $3::bigint + 1`, conv, before, limit)
	if err != nil {
		return store.ChatSnapshot{}, err
	}
	messages := make([]store.ChatMessage, 0, limit)
	for rows.Next() {
		m, err := scanChatMessage(rows)
		if err != nil {
			rows.Close()
			return store.ChatSnapshot{}, err
		}
		messages = append(messages, publicChatMessagePG(m))
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return store.ChatSnapshot{}, rowsErr
	}
	more := len(messages) > limit
	if more {
		messages = messages[:limit]
	}
	slices.Reverse(messages)
	for i := range messages {
		atts, err := chatAttachmentMetadata(ctx, q, messages[i].ID)
		if err != nil {
			return store.ChatSnapshot{}, err
		}
		if len(atts) > 0 {
			messages[i].Attachments = atts
		}
	}
	out := store.ChatSnapshot{Conversation: c, Messages: messages, More: more}
	var d store.ChatDraft
	var steps []byte
	err = q.QueryRow(ctx, `SELECT version, text, steps, updated_at FROM chat_draft WHERE conversation_id = $1`, conv).
		Scan(&d.Version, &d.Text, &steps, &d.UpdatedAt)
	if err == nil {
		if err := json.Unmarshal(steps, &d.Steps); err != nil {
			return store.ChatSnapshot{}, err
		}
		d.UpdatedAt = d.UpdatedAt.UTC()
		if d.Steps == nil {
			d.Steps = []store.ChatStep{}
		}
		out.Draft = &d
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.ChatSnapshot{}, err
	}
	return out, nil
}

func lockChatIdempotency(ctx context.Context, tx pgx.Tx, course, actor, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1729))`, course+"\n"+actor+"\n"+key)
	return err
}

type chatIdem struct {
	op, fingerprint, conversation string
	submissionStatus              *string
	reviewState                   *string
	approvalID                    *string
}

func readChatIdem(ctx context.Context, q chatQuerier, course, actor, key, op, fingerprint, conv string) (*chatIdem, error) {
	var v chatIdem
	err := q.QueryRow(ctx, `SELECT operation, fingerprint, conversation_id, submission_status, review_state, approval_id FROM chat_idempotency
		WHERE course_id = $1 AND actor_id = $2 AND key = $3`, course, actor, key).Scan(&v.op, &v.fingerprint, &v.conversation, &v.submissionStatus, &v.reviewState, &v.approvalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if v.op != op || v.fingerprint != fingerprint || conv != "" && v.conversation != conv {
		return nil, store.ErrChatIdempotency
	}
	return &v, nil
}

func bindChatAttachments(ctx context.Context, tx pgx.Tx, course, actor, message string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			return store.ErrChatAttachments
		}
		seen[id] = true
	}
	rows, err := tx.Query(ctx, `SELECT id, media_type FROM chat_attachment
		WHERE id = ANY($1) AND course_id = $2 AND owner_actor_id = $3 AND message_id IS NULL
		ORDER BY id FOR UPDATE`, ids, course, actor)
	if err != nil {
		return err
	}
	found := map[string]string{}
	for rows.Next() {
		var id, media string
		if err := rows.Scan(&id, &media); err != nil {
			rows.Close()
			return err
		}
		found[id] = media
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return rowsErr
	}
	if len(found) != len(ids) {
		// Distinguish an unowned/unknown id from one already bound without
		// revealing another actor's metadata.
		for _, id := range ids {
			var bound bool
			err := tx.QueryRow(ctx, `SELECT message_id IS NOT NULL FROM chat_attachment
				WHERE id = $1 AND course_id = $2 AND owner_actor_id = $3`, id, course, actor).Scan(&bound)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("chat attachment %s: %w", id, store.ErrNotFound)
			}
			if err != nil {
				return err
			}
			if bound {
				return store.ErrChatAttachments
			}
		}
		return store.ErrChatAttachments
	}
	images := 0
	for _, id := range ids {
		if strings.HasPrefix(found[id], "image/") {
			images++
		}
	}
	if images > 4 {
		return store.ErrChatAttachments
	}
	for i, id := range ids {
		if _, err := tx.Exec(ctx, `UPDATE chat_attachment SET message_id = $2, position = $3 WHERE id = $1`, id, message, i); err != nil {
			return err
		}
	}
	return nil
}

func questionStates(approval *store.ChatApproval) (conversation, message string) {
	conversation, message = store.ChatQueued, store.ChatPosted
	if approval != nil && approval.Mode == store.ChatApprovalModeDecision && approval.State == store.ChatApprovalPending {
		conversation, message = store.ChatReplyPendingApproval, store.ChatPending
	}
	return
}

func submissionDB(s store.ChatSubmission, approval *store.ChatApproval) (status, review, approvalID any) {
	if s.Status != "" {
		status = s.Status
	}
	if s.ReviewState != nil {
		review = *s.ReviewState
	}
	if approval != nil {
		approvalID = approval.ID
	}
	return
}

func (v *chatIdem) apply(snap *store.ChatSnapshot) {
	if v == nil || v.submissionStatus == nil {
		return
	}
	snap.Submission = &store.ChatSubmission{Status: *v.submissionStatus, ReviewState: v.reviewState}
	if v.approvalID != nil {
		snap.Submission.ApprovalID = *v.approvalID
	}
}

func writeChatApproval(ctx context.Context, tx pgx.Tx, a store.ChatApproval, c store.ChatConversation, messageID, generationID string, now time.Time) error {
	if a.ID == "" {
		return errors.New("store: chat approval id required")
	}
	if a.Mode == "" {
		a.Mode = store.ChatApprovalModeDecision
	}
	if a.State == "" {
		a.State = store.ChatApprovalPending
	}
	_, err := tx.Exec(ctx, `INSERT INTO chat_approval(id,course_id,conversation_id,subject,message_id,generation_id,mode,state,reason,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'',COALESCE($9::timestamptz,now()))`, a.ID, c.CourseID, c.ID, a.Subject, messageID, generationID, a.Mode, a.State, orNow(now))
	return err
}

func (s *Store) CreateChatConversation(ctx context.Context, p store.ChatCreateParams) (snap store.ChatSnapshot, replay bool, err error) {
	if p.Conversation.ID == "" || p.Conversation.CourseID == "" || p.Conversation.AgentID == "" || p.Conversation.OpenerActorID == "" || p.Message.ID == "" || p.GenerationID == "" || p.AssistantMsgID == "" || p.IdempotencyKey == "" || p.RequestFingerprint == "" {
		return snap, false, errors.New("store: chat create ids and idempotency key required")
	}
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		course, actor := p.Conversation.CourseID, p.Conversation.OpenerActorID
		if err := lockChatIdempotency(ctx, tx, course, actor, p.IdempotencyKey); err != nil {
			return err
		}
		old, err := readChatIdem(ctx, tx, course, actor, p.IdempotencyKey, "create", p.RequestFingerprint, "")
		if err != nil {
			return err
		}
		if old != nil {
			replay = true
			snap, err = chatSnapshotQ(ctx, tx, course, old.conversation, actor, 50, 0)
			old.apply(&snap)
			return err
		}
		c, m := p.Conversation, p.Message
		conversationState, messageState := questionStates(p.Approval)
		now := orNow(c.CreatedAt)
		_, err = tx.Exec(ctx, `INSERT INTO chat_conversation
			(id, course_id, agent_id, runtime_agent_id, opener_actor_id, title, state, revision, created_at, updated_at, last_activity_at)
			VALUES ($1,$2,$3,$4,$5,$6,$8,1,COALESCE($7::timestamptz,now()),COALESCE($7::timestamptz,now()),COALESCE($7::timestamptz,now()))`,
			c.ID, course, c.AgentID, c.RuntimeAgentID, actor, c.Title, now, conversationState)
		if err != nil {
			return err
		}
		mTime := orNow(m.CreatedAt)
		_, err = tx.Exec(ctx, `INSERT INTO chat_message (id,conversation_id,seq,role,status,body,created_at)
			VALUES ($1,$2,1,'user',$5,$3,COALESCE($4::timestamptz,now()))`, m.ID, c.ID, m.Body, mTime, messageState)
		if err != nil {
			return err
		}
		if err := bindChatAttachments(ctx, tx, course, actor, m.ID, p.AttachmentIDs); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO chat_generation (id,conversation_id,user_message_id,assistant_message_id,answer_level,state,created_at,updated_at)
			VALUES ($1,$2,$3,$4,COALESCE(NULLIF($5,''),'autonomous'),'queued',now(),now())`, p.GenerationID, c.ID, m.ID, p.AssistantMsgID, p.AnswerLevel)
		if err != nil {
			return err
		}
		if p.Approval != nil {
			if err := writeChatApproval(ctx, tx, *p.Approval, c, m.ID, p.GenerationID, time.Time{}); err != nil {
				return err
			}
		}
		subStatus, reviewState, approvalID := submissionDB(p.Submission, p.Approval)
		_, err = tx.Exec(ctx, `INSERT INTO chat_idempotency (course_id,actor_id,key,operation,fingerprint,conversation_id,message_id,submission_status,review_state,approval_id,created_at)
			VALUES ($1,$2,$3,'create',$4,$5,$6,$7,$8,$9,now())`, course, actor, p.IdempotencyKey, p.RequestFingerprint, c.ID, m.ID, subStatus, reviewState, approvalID)
		if err != nil {
			return err
		}
		snap, err = chatSnapshotQ(ctx, tx, course, c.ID, actor, 50, 0)
		if p.Submission.Status != "" {
			submission := p.Submission
			snap.Submission = &submission
		}
		return err
	})
	return snap, replay, err
}

func (s *Store) AppendChatMessage(ctx context.Context, p store.ChatAppendParams) (snap store.ChatSnapshot, replay bool, err error) {
	if p.CourseID == "" || p.ConversationID == "" || p.ActorID == "" || p.Message.ID == "" || p.GenerationID == "" || p.AssistantMsgID == "" || p.IdempotencyKey == "" || p.RequestFingerprint == "" {
		return snap, false, errors.New("store: chat append ids and idempotency key required")
	}
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		if err := lockChatIdempotency(ctx, tx, p.CourseID, p.ActorID, p.IdempotencyKey); err != nil {
			return err
		}
		old, err := readChatIdem(ctx, tx, p.CourseID, p.ActorID, p.IdempotencyKey, "ask", p.RequestFingerprint, p.ConversationID)
		if err != nil {
			return err
		}
		if old != nil {
			replay = true
			snap, err = chatSnapshotQ(ctx, tx, p.CourseID, old.conversation, p.ActorID, 50, 0)
			old.apply(&snap)
			return err
		}
		c, err := scanChatConversation(tx.QueryRow(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation
			WHERE id = $1 AND course_id = $2 AND opener_actor_id = $3 FOR UPDATE`, p.ConversationID, p.CourseID, p.ActorID))
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		if c.State == store.ChatClosed || c.State == store.ChatQueued || c.State == store.ChatAnswering || c.State == store.ChatReplyPendingApproval {
			return store.ErrChatNotAwaiting
		}
		var seq int64
		conversationState, messageState := questionStates(p.Approval)
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM chat_message WHERE conversation_id = $1`, c.ID).Scan(&seq); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO chat_message (id,conversation_id,seq,role,status,body,created_at)
			VALUES ($1,$2,$3,'user',$6,$4,COALESCE($5::timestamptz,now()))`, p.Message.ID, c.ID, seq, p.Message.Body, orNow(p.Message.CreatedAt), messageState)
		if err != nil {
			return err
		}
		if err := bindChatAttachments(ctx, tx, p.CourseID, p.ActorID, p.Message.ID, p.AttachmentIDs); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO chat_generation (id,conversation_id,user_message_id,assistant_message_id,answer_level,state,created_at,updated_at)
			VALUES ($1,$2,$3,$4,COALESCE(NULLIF($5,''),'autonomous'),'queued',now(),now())`, p.GenerationID, c.ID, p.Message.ID, p.AssistantMsgID, p.AnswerLevel)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE chat_conversation SET state=$2,revision=revision+1,updated_at=now(),last_activity_at=now() WHERE id=$1`, c.ID, conversationState)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM chat_draft WHERE conversation_id=$1`, c.ID)
		if err != nil {
			return err
		}
		if p.Approval != nil {
			if err := writeChatApproval(ctx, tx, *p.Approval, c, p.Message.ID, p.GenerationID, time.Time{}); err != nil {
				return err
			}
		}
		subStatus, reviewState, approvalID := submissionDB(p.Submission, p.Approval)
		_, err = tx.Exec(ctx, `INSERT INTO chat_idempotency (course_id,actor_id,key,operation,fingerprint,conversation_id,message_id,submission_status,review_state,approval_id,created_at)
			VALUES ($1,$2,$3,'ask',$4,$5,$6,$7,$8,$9,now())`, p.CourseID, p.ActorID, p.IdempotencyKey, p.RequestFingerprint, c.ID, p.Message.ID, subStatus, reviewState, approvalID)
		if err != nil {
			return err
		}
		snap, err = chatSnapshotQ(ctx, tx, p.CourseID, c.ID, p.ActorID, 50, 0)
		if p.Submission.Status != "" {
			submission := p.Submission
			snap.Submission = &submission
		}
		return err
	})
	return snap, replay, err
}

func (s *Store) ChatConversation(ctx context.Context, course, conv, actor string) (store.ChatConversation, error) {
	c, err := scanChatConversation(s.pool.QueryRow(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation WHERE id=$1 AND course_id=$2 AND opener_actor_id=$3`, conv, course, actor))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, store.ErrNotFound
	}
	return c, err
}

func (s *Store) ListChatConversations(ctx context.Context, course, actor, after string, limit int) ([]store.ChatConversation, string, error) {
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
	rows, err := s.pool.Query(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation
		WHERE course_id=$1 AND opener_actor_id=$2 AND ($3::timestamptz IS NULL OR (last_activity_at,id) < ($3::timestamptz,$4))
		ORDER BY last_activity_at DESC,id DESC LIMIT $5::bigint+1`, course, actor, orNow(at), id, limit)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []store.ChatConversation{}
	for rows.Next() {
		c, err := scanChatConversation(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = store.EncodeChatCursor(last.LastActivityAt, last.ID)
	}
	return out, next, nil
}

func (s *Store) ChatSnapshot(ctx context.Context, course, conv, actor string, limit int, before int64) (store.ChatSnapshot, error) {
	return chatSnapshotQ(ctx, s.pool, course, conv, actor, limit, before)
}

func (s *Store) StageChatAttachment(ctx context.Context, a store.ChatAttachment) (store.ChatAttachment, error) {
	if err := store.CheckChatAttachment(a); err != nil {
		return store.ChatAttachment{}, err
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO chat_attachment (id,course_id,owner_actor_id,filename,media_type,size,bytes,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,COALESCE($8::timestamptz,now())) RETURNING created_at`, a.ID, a.CourseID, a.OwnerActorID, a.Filename, a.MediaType, a.Size, a.Bytes, orNow(a.CreatedAt)).Scan(&a.CreatedAt)
	if err != nil {
		return store.ChatAttachment{}, err
	}
	a.Bytes = nil
	a.Position = -1
	return a, nil
}

func (s *Store) StageChatAttachmentIdempotent(ctx context.Context, a store.ChatAttachment, key, fp string) (out store.ChatAttachment, replay bool, err error) {
	if key == "" || fp == "" {
		return out, false, errors.New("store: chat attachment idempotency key and fingerprint required")
	}
	if err := store.CheckChatAttachment(a); err != nil {
		return out, false, err
	}
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 1730))`, a.CourseID+"\n"+a.OwnerActorID+"\n"+key); err != nil {
			return err
		}
		var oldFP string
		err := tx.QueryRow(ctx, `SELECT id,course_id,owner_actor_id,position,filename,media_type,size,created_at,request_fingerprint
			FROM chat_attachment WHERE course_id=$1 AND owner_actor_id=$2 AND idempotency_key=$3`, a.CourseID, a.OwnerActorID, key).
			Scan(&out.ID, &out.CourseID, &out.OwnerActorID, &out.Position, &out.Filename, &out.MediaType, &out.Size, &out.CreatedAt, &oldFP)
		if err == nil {
			if oldFP != fp {
				return store.ErrChatIdempotency
			}
			replay = true
			out.CreatedAt = out.CreatedAt.UTC()
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO chat_attachment(id,course_id,owner_actor_id,filename,media_type,size,bytes,idempotency_key,request_fingerprint,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,COALESCE($10::timestamptz,now())) RETURNING created_at`,
			a.ID, a.CourseID, a.OwnerActorID, a.Filename, a.MediaType, a.Size, a.Bytes, key, fp, orNow(a.CreatedAt)).Scan(&a.CreatedAt)
		if err != nil {
			return err
		}
		a.CreatedAt = a.CreatedAt.UTC()
		a.Bytes = nil
		a.Position = -1
		a.IdempotencyKey = key
		a.RequestFingerprint = fp
		out = a
		return nil
	})
	return out, replay, err
}

func (s *Store) ChatAttachment(ctx context.Context, course, id, actor string) (store.ChatAttachment, error) {
	var a store.ChatAttachment
	var bound *string
	err := s.pool.QueryRow(ctx, `SELECT id,course_id,owner_actor_id,message_id,position,filename,media_type,size,bytes,created_at
		FROM chat_attachment WHERE id=$1 AND course_id=$2 AND owner_actor_id=$3`, id, course, actor).
		Scan(&a.ID, &a.CourseID, &a.OwnerActorID, &bound, &a.Position, &a.Filename, &a.MediaType, &a.Size, &a.Bytes, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, store.ErrNotFound
	}
	if bound != nil {
		a.BoundMessage = *bound
	}
	a.CreatedAt = a.CreatedAt.UTC()
	return a, err
}

func (s *Store) WithdrawChatMessage(ctx context.Context, course, conv, actor, message, reason string) (snap store.ChatSnapshot, err error) {
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		c, err := scanChatConversation(tx.QueryRow(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation WHERE id=$1 AND course_id=$2 AND opener_actor_id=$3 FOR UPDATE`, conv, course, actor))
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		m, err := scanChatMessage(tx.QueryRow(ctx, `SELECT `+chatMessageColumns+` FROM chat_message WHERE conversation_id=$1 AND role='user' ORDER BY seq DESC LIMIT 1 FOR UPDATE`, conv))
		if err != nil {
			return err
		}
		if m.ID == message && m.Status != store.ChatPosted {
			snap, err = chatSnapshotQ(ctx, tx, course, conv, actor, 50, 0)
			return err
		}
		if m.ID != message || m.Status != store.ChatPosted && m.Status != store.ChatPending || c.State != store.ChatQueued && c.State != store.ChatAnswering && c.State != store.ChatFailed && c.State != store.ChatReplyPendingApproval {
			return store.ErrChatWithdraw
		}
		_, err = tx.Exec(ctx, `UPDATE chat_message SET status='retracted',body='',retracted_at=now(),retracted_reason=$2 WHERE id=$1`, message, reason)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE chat_generation SET state='cancelled',updated_at=now() WHERE conversation_id=$1 AND user_message_id=$2 AND state IN ('queued','running','failed','proposed')`, conv, message)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE chat_approval SET state='cancelled',decided_at=now() WHERE conversation_id=$1 AND state='pending'`, conv)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE chat_message SET status='failed',body='',error_kind='withdrawn' WHERE conversation_id=$1 AND role='assistant' AND status='pending'`, conv)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM chat_draft WHERE conversation_id=$1`, conv)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE chat_conversation SET state='answered',revision=revision+1,updated_at=now(),last_activity_at=now() WHERE id=$1`, conv)
		if err != nil {
			return err
		}
		snap, err = chatSnapshotQ(ctx, tx, course, conv, actor, 50, 0)
		return err
	})
	return snap, err
}

func (s *Store) CloseChatConversation(ctx context.Context, course, conv, actor, reason string) (c store.ChatConversation, err error) {
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		c, err = scanChatConversation(tx.QueryRow(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation WHERE id=$1 AND course_id=$2 AND opener_actor_id=$3 FOR UPDATE`, conv, course, actor))
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		if c.State == store.ChatClosed {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE chat_generation SET state='cancelled',updated_at=now() WHERE conversation_id=$1 AND state IN ('queued','running','proposed')`, conv)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE chat_approval SET state='cancelled',decided_at=now() WHERE conversation_id=$1 AND state='pending'`, conv)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE chat_message SET status='failed',body='',error_kind='closed' WHERE conversation_id=$1 AND role='assistant' AND status='pending'`, conv)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM chat_draft WHERE conversation_id=$1`, conv)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `UPDATE chat_conversation SET state='closed',revision=revision+1,updated_at=now(),last_activity_at=now(),closed_at=now(),close_reason=$2 WHERE id=$1 RETURNING `+chatConversationColumns, conv, reason).Scan(&c.ID, &c.CourseID, &c.AgentID, &c.RuntimeAgentID, &c.OpenerActorID, &c.Title, &c.State, &c.Revision, &c.CreatedAt, &c.UpdatedAt, &c.LastActivityAt, &c.ClosedAt, &c.CloseReason)
	})
	if c.ClosedAt != nil {
		t := c.ClosedAt.UTC()
		c.ClosedAt = &t
	}
	c.CreatedAt = c.CreatedAt.UTC()
	c.UpdatedAt = c.UpdatedAt.UTC()
	c.LastActivityAt = c.LastActivityAt.UTC()
	return c, err
}

const chatGenerationColumns = `id,conversation_id,user_message_id,assistant_message_id,state,attempt,holder,fence,answer_level,revision_reason,revision_body,revision_of_generation_id,lease_expires_at,result,error_kind,error_retryable,created_at,updated_at`

func scanChatGeneration(row pgx.Row) (store.ChatGeneration, error) {
	var g store.ChatGeneration
	var raw []byte
	var kind string
	var retryable *bool
	err := row.Scan(&g.ID, &g.ConversationID, &g.UserMessageID, &g.AssistantMsgID, &g.State, &g.Attempt, &g.Holder, &g.Fence, &g.AnswerLevel, &g.RevisionReason, &g.RevisionBody, &g.RevisionOfGenerationID, &g.LeaseExpiresAt, &raw, &kind, &retryable, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return g, err
	}
	g.CreatedAt = g.CreatedAt.UTC()
	g.UpdatedAt = g.UpdatedAt.UTC()
	g.LeaseExpiresAt = g.LeaseExpiresAt.UTC()
	g.Result, err = store.ParseChatResult(raw)
	if err != nil {
		return g, err
	}
	if kind != "" {
		g.Error = &store.ChatMessageError{Kind: kind, Retryable: retryable != nil && *retryable}
	}
	return g, nil
}

func (s *Store) ChatGeneration(ctx context.Context, id string) (store.ChatGeneration, error) {
	g, err := scanChatGeneration(s.pool.QueryRow(ctx, `SELECT `+chatGenerationColumns+` FROM chat_generation WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		err = store.ErrNotFound
	}
	return g, err
}

func (s *Store) RecoverChatGenerations(ctx context.Context, limit int) ([]store.ChatGeneration, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+chatGenerationColumns+` FROM chat_generation WHERE state IN ('queued','running') ORDER BY created_at,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.ChatGeneration{}
	for rows.Next() {
		g, err := scanChatGeneration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// lockChatGeneration locks the conversation before its outbox row, the same
// order used by withdrawal and close, so recovery cannot deadlock them.
func lockChatGeneration(ctx context.Context, tx pgx.Tx, id string) (store.ChatGeneration, store.ChatConversation, error) {
	var conv string
	err := tx.QueryRow(ctx, `SELECT conversation_id FROM chat_generation WHERE id=$1`, id).Scan(&conv)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ChatGeneration{}, store.ChatConversation{}, store.ErrNotFound
	}
	if err != nil {
		return store.ChatGeneration{}, store.ChatConversation{}, err
	}
	c, err := scanChatConversation(tx.QueryRow(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation WHERE id=$1 FOR UPDATE`, conv))
	if err != nil {
		return store.ChatGeneration{}, c, err
	}
	g, err := scanChatGeneration(tx.QueryRow(ctx, `SELECT `+chatGenerationColumns+` FROM chat_generation WHERE id=$1 FOR UPDATE`, id))
	return g, c, err
}

func latestChatUserPG(ctx context.Context, q chatQuerier, conv string) (store.ChatMessage, error) {
	return scanChatMessage(q.QueryRow(ctx, `SELECT `+chatMessageColumns+` FROM chat_message WHERE conversation_id=$1 AND role='user' ORDER BY seq DESC LIMIT 1`, conv))
}

func (s *Store) BeginChatGeneration(ctx context.Context, id, holder string, ttl time.Duration) (g store.ChatGeneration, c store.ChatConversation, begin bool, err error) {
	if holder == "" || ttl <= 0 {
		return g, c, false, errors.New("store: chat generation holder and positive ttl required")
	}
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		g, c, err = lockChatGeneration(ctx, tx, id)
		if err != nil {
			return err
		}
		if g.State != store.ChatGenerationQueued && g.State != store.ChatGenerationRunning || c.State == store.ChatClosed {
			return nil
		}
		if g.State == store.ChatGenerationRunning {
			var live bool
			if err := tx.QueryRow(ctx, `SELECT lease_expires_at > now() FROM chat_generation WHERE id=$1`, id).Scan(&live); err != nil {
				return err
			}
			// A restarted process with the same deliberate WORKER_ID may reclaim
			// its interrupted generation. A different live holder may not.
			if live && g.Holder != holder {
				return nil
			}
		}
		m, err := latestChatUserPG(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if m.ID != g.UserMessageID || m.Status != store.ChatPosted {
			return nil
		}
		g, err = scanChatGeneration(tx.QueryRow(ctx, `UPDATE chat_generation SET state='running',attempt=attempt+1,holder=$2,fence=fence+1,lease_expires_at=now()+$3::interval,updated_at=now() WHERE id=$1 RETURNING `+chatGenerationColumns, id, holder, ttl))
		if err != nil {
			return err
		}
		c, err = scanChatConversation(tx.QueryRow(ctx, `UPDATE chat_conversation SET state='answering',revision=revision+1,updated_at=now(),last_activity_at=now() WHERE id=$1 RETURNING `+chatConversationColumns, c.ID))
		begin = err == nil
		return err
	})
	return
}

func (s *Store) HeartbeatChatGeneration(ctx context.Context, id, holder string, fence int64, ttl time.Duration) (bool, error) {
	if holder == "" || fence <= 0 || ttl <= 0 {
		return false, errors.New("store: chat generation heartbeat holder, fence and positive ttl required")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE chat_generation SET lease_expires_at=now()+$4::interval,updated_at=now()
		WHERE id=$1 AND holder=$2 AND fence=$3 AND state='running' AND lease_expires_at > now()`, id, holder, fence, ttl)
	return tag.RowsAffected() == 1, err
}

func chatGenerationWritablePG(ctx context.Context, tx pgx.Tx, g store.ChatGeneration, c store.ChatConversation, holder string, fence int64) (bool, error) {
	if g.State != store.ChatGenerationRunning || g.Holder != holder || g.Fence != fence || c.State == store.ChatClosed {
		return false, nil
	}
	var live bool
	err := tx.QueryRow(ctx, `SELECT lease_expires_at > now() FROM chat_generation WHERE id=$1`, g.ID).Scan(&live)
	return live, err
}

func (s *Store) PutChatDraft(ctx context.Context, id, holder string, fence int64, text string, steps []store.ChatStep) (c store.ChatConversation, d store.ChatDraft, written bool, err error) {
	raw, _ := json.Marshal(steps)
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		var g store.ChatGeneration
		g, c, err = lockChatGeneration(ctx, tx, id)
		if err != nil {
			return err
		}
		writable, err := chatGenerationWritablePG(ctx, tx, g, c, holder, fence)
		if err != nil || !writable {
			return err
		}
		m, err := latestChatUserPG(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if m.ID != g.UserMessageID || m.Status != store.ChatPosted {
			return nil
		}
		err = tx.QueryRow(ctx, `INSERT INTO chat_draft(conversation_id,version,text,steps,updated_at) VALUES($1,1,$2,$3,now())
			ON CONFLICT(conversation_id)DO UPDATE SET version=chat_draft.version+1,text=EXCLUDED.text,steps=EXCLUDED.steps,updated_at=now()
			RETURNING version,text,steps,updated_at`, c.ID, text, raw).Scan(&d.Version, &d.Text, &raw, &d.UpdatedAt)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &d.Steps); err != nil {
			return err
		}
		d.UpdatedAt = d.UpdatedAt.UTC()
		c, err = scanChatConversation(tx.QueryRow(ctx, `UPDATE chat_conversation SET revision=revision+1,updated_at=now() WHERE id=$1 RETURNING `+chatConversationColumns, c.ID))
		written = err == nil
		return err
	})
	return
}

func (s *Store) WriteChatResult(ctx context.Context, id, holder string, fence int64, result store.ChatResult) (written bool, err error) {
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		g, c, err := lockChatGeneration(ctx, tx, id)
		if err != nil {
			return err
		}
		writable, err := chatGenerationWritablePG(ctx, tx, g, c, holder, fence)
		if err != nil || !writable {
			return err
		}
		m, err := latestChatUserPG(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if m.ID != g.UserMessageID || m.Status != store.ChatPosted {
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE chat_generation SET result=$4,updated_at=now() WHERE id=$1 AND holder=$2 AND fence=$3 AND state='running' AND lease_expires_at > now()`, id, holder, fence, result.JSON())
		written = tag.RowsAffected() == 1
		return err
	})
	return
}

func (s *Store) finalizeChatGeneration(ctx context.Context, id, holder string, fence int64, review *store.ChatApproval) (snap store.ChatSnapshot, finished bool, err error) {
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		g, c, err := lockChatGeneration(ctx, tx, id)
		if err != nil {
			return err
		}
		if g.State == store.ChatGenerationDone {
			snap, err = chatSnapshotQ(ctx, tx, c.CourseID, c.ID, c.OpenerActorID, 50, 0)
			return err
		}
		if g.Result == nil {
			return nil
		}
		writable, err := chatGenerationWritablePG(ctx, tx, g, c, holder, fence)
		if err != nil || !writable {
			return err
		}
		m, err := latestChatUserPG(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if m.ID != g.UserMessageID || m.Status != store.ChatPosted {
			return nil
		}
		var seq int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM chat_message WHERE conversation_id=$1`, c.ID).Scan(&seq); err != nil {
			return err
		}
		r := g.Result
		var sources, usage any
		if r.SourcesStated != nil && *r.SourcesStated {
			b, _ := json.Marshal(r.Sources)
			sources = b
		}
		if r.Usage != nil {
			b, _ := json.Marshal(r.Usage)
			usage = b
		}
		_, err = tx.Exec(ctx, `INSERT INTO chat_message(id,conversation_id,seq,role,status,body,sources,sources_stated,usage,finish_reason,created_at)
			VALUES($1,$2,$3,'assistant','posted',$4,$5,$6,$7,$8,now())`, g.AssistantMsgID, c.ID, seq, r.Body, sources, r.SourcesStated, usage, r.FinishReason)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE chat_generation SET state='done',updated_at=now() WHERE id=$1 AND holder=$2 AND fence=$3 AND state='running'`, id, holder, fence)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return store.ErrConflict
		}
		if _, err = tx.Exec(ctx, `DELETE FROM chat_draft WHERE conversation_id=$1`, c.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE chat_conversation SET state='answered',revision=revision+1,updated_at=now(),last_activity_at=now() WHERE id=$1`, c.ID); err != nil {
			return err
		}
		if review != nil {
			if err := writeChatApproval(ctx, tx, *review, c, g.AssistantMsgID, g.ID, time.Time{}); err != nil {
				return err
			}
		}
		snap, err = chatSnapshotQ(ctx, tx, c.CourseID, c.ID, c.OpenerActorID, 50, 0)
		finished = err == nil
		return err
	})
	return
}

func (s *Store) FinalizeChatGeneration(ctx context.Context, id, holder string, fence int64) (store.ChatSnapshot, bool, error) {
	return s.finalizeChatGeneration(ctx, id, holder, fence, nil)
}
func (s *Store) FinalizeReviewedChatGeneration(ctx context.Context, id, holder string, fence int64, approval store.ChatApproval) (store.ChatSnapshot, bool, error) {
	return s.finalizeChatGeneration(ctx, id, holder, fence, &approval)
}

func (s *Store) ProposeChatGeneration(ctx context.Context, id, holder string, fence int64, approval store.ChatApproval) (snap store.ChatSnapshot, proposed bool, err error) {
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		g, c, err := lockChatGeneration(ctx, tx, id)
		if err != nil {
			return err
		}
		if g.Result == nil {
			return nil
		}
		writable, err := chatGenerationWritablePG(ctx, tx, g, c, holder, fence)
		if err != nil || !writable {
			return err
		}
		m, err := latestChatUserPG(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if m.ID != g.UserMessageID || m.Status != store.ChatPosted {
			return nil
		}
		var seq int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM chat_message WHERE conversation_id=$1`, c.ID).Scan(&seq); err != nil {
			return err
		}
		r := g.Result
		var sources, usage any
		if r.SourcesStated != nil && *r.SourcesStated {
			b, _ := json.Marshal(r.Sources)
			sources = b
		}
		if r.Usage != nil {
			b, _ := json.Marshal(r.Usage)
			usage = b
		}
		_, err = tx.Exec(ctx, `INSERT INTO chat_message(id,conversation_id,seq,role,status,body,sources,sources_stated,usage,finish_reason,created_at)
			VALUES($1,$2,$3,'assistant','pending',$4,$5,$6,$7,$8,now())`, g.AssistantMsgID, c.ID, seq, r.Body, sources, r.SourcesStated, usage, r.FinishReason)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE chat_generation SET state='proposed',updated_at=now() WHERE id=$1 AND holder=$2 AND fence=$3 AND state='running'`, id, holder, fence); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM chat_draft WHERE conversation_id=$1`, c.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE chat_conversation SET state='reply_pending_approval',revision=revision+1,updated_at=now(),last_activity_at=now() WHERE id=$1`, c.ID); err != nil {
			return err
		}
		approval.Subject = store.ChatSubjectAnswer
		approval.Mode = store.ChatApprovalModeDecision
		approval.State = store.ChatApprovalPending
		if err = writeChatApproval(ctx, tx, approval, c, g.AssistantMsgID, g.ID, time.Time{}); err != nil {
			return err
		}
		snap, err = chatSnapshotQ(ctx, tx, c.CourseID, c.ID, c.OpenerActorID, 50, 0)
		proposed = err == nil
		return err
	})
	return
}

func (s *Store) FailChatGeneration(ctx context.Context, id, holder string, fence int64, kind string, retryable bool) (c store.ChatConversation, failed bool, err error) {
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		var g store.ChatGeneration
		g, c, err = lockChatGeneration(ctx, tx, id)
		if err != nil {
			return err
		}
		if g.State == store.ChatGenerationFailed || g.State == store.ChatGenerationDone || g.State == store.ChatGenerationCancelled || c.State == store.ChatClosed {
			return nil
		}
		writable, err := chatGenerationWritablePG(ctx, tx, g, c, holder, fence)
		if err != nil || !writable {
			return err
		}
		m, err := latestChatUserPG(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if m.ID != g.UserMessageID || m.Status != store.ChatPosted {
			return nil
		}
		if kind == "" {
			kind = "provider_failed"
		}
		var seq int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM chat_message WHERE conversation_id=$1`, c.ID).Scan(&seq); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO chat_message(id,conversation_id,seq,role,status,body,error_kind,error_retryable,created_at)
			VALUES($1,$2,$3,'assistant','failed','',$4,$5,now())ON CONFLICT(id)DO NOTHING`, g.AssistantMsgID, c.ID, seq, kind, retryable)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE chat_generation SET state='failed',error_kind=$4,error_retryable=$5,updated_at=now() WHERE id=$1 AND holder=$2 AND fence=$3 AND state='running'`, id, holder, fence, kind, retryable)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return store.ErrConflict
		}
		if _, err = tx.Exec(ctx, `DELETE FROM chat_draft WHERE conversation_id=$1`, c.ID); err != nil {
			return err
		}
		c, err = scanChatConversation(tx.QueryRow(ctx, `UPDATE chat_conversation SET state='failed',revision=revision+1,updated_at=now(),last_activity_at=now() WHERE id=$1 RETURNING `+chatConversationColumns, c.ID))
		failed = err == nil
		return err
	})
	return
}
