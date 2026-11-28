package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/jackc/pgx/v5"
)

const chatApprovalColumns = `id,course_id,conversation_id,subject,message_id,generation_id,mode,state,reason,created_at,decided_at`

func scanChatApproval(row pgx.Row) (store.ChatApproval, error) {
	var a store.ChatApproval
	var decided *time.Time
	err := row.Scan(&a.ID, &a.CourseID, &a.ConversationID, &a.Subject, &a.MessageID, &a.GenerationID, &a.Mode, &a.State, &a.Reason, &a.CreatedAt, &decided)
	a.CreatedAt = a.CreatedAt.UTC()
	if decided != nil {
		t := decided.UTC()
		a.DecidedAt = &t
	}
	return a, err
}

func (s *Store) ChatApprovalRecord(ctx context.Context, id string) (store.ChatApproval, store.ChatConversation, error) {
	a, err := scanChatApproval(s.pool.QueryRow(ctx, `SELECT `+chatApprovalColumns+` FROM chat_approval WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ChatApproval{}, store.ChatConversation{}, store.ErrNotFound
	}
	if err != nil {
		return a, store.ChatConversation{}, err
	}
	c, err := scanChatConversation(s.pool.QueryRow(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation WHERE id=$1`, a.ConversationID))
	return a, c, err
}

func (s *Store) ListChatApprovals(ctx context.Context, course, state, after string, limit int) ([]store.ChatApproval, string, error) {
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
	rows, err := s.pool.Query(ctx, `SELECT `+chatApprovalColumns+` FROM chat_approval WHERE course_id=$1 AND ($2='' OR state=$2)
		AND ($3::timestamptz IS NULL OR (created_at,id)>($3::timestamptz,$4)) ORDER BY created_at,id LIMIT $5::bigint+1`, course, state, orNow(at), id, limit)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []store.ChatApproval{}
	for rows.Next() {
		a, err := scanChatApproval(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, a)
	}
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = store.EncodeChatCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func readApprovalForUpdate(ctx context.Context, tx pgx.Tx, id string) (store.ChatApproval, store.ChatConversation, error) {
	a, err := scanChatApproval(tx.QueryRow(ctx, `SELECT `+chatApprovalColumns+` FROM chat_approval WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, store.ChatConversation{}, store.ErrNotFound
	}
	if err != nil {
		return a, store.ChatConversation{}, err
	}
	c, err := scanChatConversation(tx.QueryRow(ctx, `SELECT `+chatConversationColumns+` FROM chat_conversation WHERE id=$1 FOR UPDATE`, a.ConversationID))
	return a, c, err
}

func approvalActionReplay(ctx context.Context, tx pgx.Tx, p store.ChatDecisionParams, course, operation string) (bool, error) {
	var approval, op, fp string
	err := tx.QueryRow(ctx, `SELECT approval_id,operation,fingerprint FROM chat_approval_action WHERE course_id=$1 AND actor_id=$2 AND key=$3`, course, p.ActorID, p.IdempotencyKey).Scan(&approval, &op, &fp)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if approval != p.ApprovalID || op != operation || fp != p.Fingerprint {
		return false, store.ErrChatIdempotency
	}
	return true, nil
}

func insertApprovalAction(ctx context.Context, tx pgx.Tx, p store.ChatDecisionParams, course, operation string) error {
	_, err := tx.Exec(ctx, `INSERT INTO chat_approval_action(course_id,actor_id,key,approval_id,operation,fingerprint,created_at) VALUES($1,$2,$3,$4,$5,$6,now())`, course, p.ActorID, p.IdempotencyKey, p.ApprovalID, operation, p.Fingerprint)
	return err
}

func (s *Store) DecideChatApproval(ctx context.Context, p store.ChatDecisionParams) (a store.ChatApproval, snap store.ChatSnapshot, schedule bool, err error) {
	if p.ActorID == "" || p.ApprovalID == "" || p.IdempotencyKey == "" || p.Fingerprint == "" {
		return a, snap, false, errors.New("store: decision fields required")
	}
	_, existing, readErr := s.ChatApprovalRecord(ctx, p.ApprovalID)
	if readErr != nil {
		err = readErr
		return
	}
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,1731))`, existing.CourseID+"\n"+p.ActorID+"\n"+p.IdempotencyKey); err != nil {
			return err
		}
		var c store.ChatConversation
		a, c, err = readApprovalForUpdate(ctx, tx, p.ApprovalID)
		if err != nil {
			return err
		}
		replay, err := approvalActionReplay(ctx, tx, p, c.CourseID, "decide")
		if err != nil {
			return err
		}
		if replay {
			snap, err = chatSnapshotQ(ctx, tx, c.CourseID, c.ID, c.OpenerActorID, 50, 0)
			return err
		}
		if a.Mode != store.ChatApprovalModeDecision || a.State != store.ChatApprovalPending || c.State != store.ChatReplyPendingApproval {
			return store.ErrChatApproval
		}
		g, err := scanChatGeneration(tx.QueryRow(ctx, `SELECT `+chatGenerationColumns+` FROM chat_generation WHERE id=$1 FOR UPDATE`, a.GenerationID))
		if err != nil {
			return err
		}
		u, err := latestChatUserPG(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		if u.ID != g.UserMessageID || u.Status == store.ChatRetracted {
			return store.ErrChatApproval
		}
		m, err := scanChatMessage(tx.QueryRow(ctx, `SELECT `+chatMessageColumns+` FROM chat_message WHERE id=$1 FOR UPDATE`, a.MessageID))
		if err != nil {
			return err
		}
		if p.Decision != "approve" && p.Decision != "reject" && p.Decision != "changes_requested" {
			return store.ErrChatApproval
		}
		newState, conversationState, messageState := "", "", ""
		if a.Subject == store.ChatSubjectQuestion {
			if m.Role != store.ChatRoleUser || m.Status != store.ChatPending || g.State != store.ChatGenerationQueued {
				return store.ErrChatApproval
			}
			switch p.Decision {
			case "approve":
				newState = store.ChatApprovalApproved
				conversationState = store.ChatQueued
				messageState = store.ChatPosted
				schedule = true
			default:
				newState = store.ChatApprovalChanges
				if p.Decision == "reject" {
					newState = store.ChatApprovalRejected
				}
				conversationState = store.ChatAnswered
				messageState = store.ChatMsgFailed
				g.State = store.ChatGenerationCancelled
			}
		} else if a.Subject == store.ChatSubjectAnswer {
			if m.Role != store.ChatRoleAssistant || m.Status != store.ChatPending || g.State != store.ChatGenerationProposed {
				return store.ErrChatApproval
			}
			switch p.Decision {
			case "approve":
				newState = store.ChatApprovalApproved
				conversationState = store.ChatAnswered
				messageState = store.ChatPosted
				g.State = store.ChatGenerationDone
			case "reject":
				newState = store.ChatApprovalRejected
				conversationState = store.ChatFailed
				messageState = store.ChatMsgFailed
				g.State = store.ChatGenerationFailed
			case "changes_requested":
				newState = p.Decision
				conversationState = store.ChatQueued
				messageState = store.ChatMsgFailed
				g.State = store.ChatGenerationCancelled
				schedule = true
				if p.NewGenerationID == "" || p.NewAssistantID == "" {
					return store.ErrChatApproval
				}
				if _, err = tx.Exec(ctx, `INSERT INTO chat_generation(id,conversation_id,user_message_id,assistant_message_id,answer_level,revision_reason,revision_body,revision_of_generation_id,state,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'queued',now(),now())`, p.NewGenerationID, c.ID, g.UserMessageID, p.NewAssistantID, g.AnswerLevel, p.Reason, g.Result.Body, g.ID); err != nil {
					return err
				}
			}
		} else {
			return store.ErrChatApproval
		}
		if messageState == store.ChatMsgFailed {
			var err error
			if a.Subject == store.ChatSubjectAnswer {
				err = tx.QueryRow(ctx, `UPDATE chat_message SET status=$2,body='',error_kind=$3 WHERE id=$1 RETURNING id`, m.ID, messageState, p.Decision).Scan(&m.ID)
			} else {
				err = tx.QueryRow(ctx, `UPDATE chat_message SET status=$2,error_kind=$3 WHERE id=$1 RETURNING id`, m.ID, messageState, p.Decision).Scan(&m.ID)
			}
			if err != nil {
				return err
			}
		} else {
			if _, err = tx.Exec(ctx, `UPDATE chat_message SET status=$2 WHERE id=$1`, m.ID, messageState); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE chat_generation SET state=$2,updated_at=now() WHERE id=$1`, g.ID, g.State); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE chat_conversation SET state=$2,revision=revision+1,updated_at=now(),last_activity_at=now() WHERE id=$1`, c.ID, conversationState); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM chat_draft WHERE conversation_id=$1`, c.ID); err != nil {
			return err
		}
		a.State = newState
		a.Reason = p.Reason
		a.DecidedAt = nil
		err = tx.QueryRow(ctx, `UPDATE chat_approval SET state=$2,reason=$3,decided_at=now() WHERE id=$1 RETURNING decided_at`, a.ID, newState, p.Reason).Scan(&a.DecidedAt)
		if err != nil {
			return err
		}
		if err = insertApprovalAction(ctx, tx, p, c.CourseID, "decide"); err != nil {
			return err
		}
		// A reviewer whose own decisions execute with post-review creates the
		// corresponding review item in the same transaction. The original
		// approval's decision remains immutable beside it.
		if p.ReviewerLevel == "pending_review" {
			if p.ReviewApprovalID == "" {
				return errors.New("store: post-review approval id required")
			}
			if _, err = tx.Exec(ctx, `INSERT INTO chat_approval(id,course_id,conversation_id,subject,message_id,generation_id,mode,state,reason,created_at)
				VALUES($1,$2,$3,$4,$5,$6,'review','pending','',now())`, p.ReviewApprovalID, c.CourseID, c.ID, a.Subject, a.MessageID, a.GenerationID); err != nil {
				return err
			}
		}
		snap, err = chatSnapshotQ(ctx, tx, c.CourseID, c.ID, c.OpenerActorID, 50, 0)
		return err
	})
	return
}

func (s *Store) ChatApprovalAttachment(ctx context.Context, approvalID, attachmentID string) (store.ChatAttachment, error) {
	var a store.ChatAttachment
	var bound string
	err := s.pool.QueryRow(ctx, `SELECT at.id,at.course_id,at.owner_actor_id,at.message_id,at.position,at.filename,at.media_type,at.size,at.bytes,at.created_at
		FROM chat_approval ap
		JOIN chat_generation g ON g.id = ap.generation_id
		JOIN chat_attachment at ON at.id = $2 AND at.course_id = ap.course_id
		   AND (at.message_id = ap.message_id OR (ap.subject = 'answer' AND at.message_id = g.user_message_id))
		WHERE ap.id = $1`, approvalID, attachmentID).
		Scan(&a.ID, &a.CourseID, &a.OwnerActorID, &bound, &a.Position, &a.Filename, &a.MediaType, &a.Size, &a.Bytes, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, store.ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.BoundMessage, a.CreatedAt = bound, a.CreatedAt.UTC()
	return a, nil
}

func (s *Store) ReviewChatApproval(ctx context.Context, p store.ChatDecisionParams) (a store.ChatApproval, snap store.ChatSnapshot, err error) {
	if p.ActorID == "" || p.ApprovalID == "" || p.IdempotencyKey == "" || p.Fingerprint == "" {
		return a, snap, errors.New("store: review fields required")
	}
	_, existing, readErr := s.ChatApprovalRecord(ctx, p.ApprovalID)
	if readErr != nil {
		err = readErr
		return
	}
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,1732))`, existing.CourseID+"\n"+p.ActorID+"\n"+p.IdempotencyKey); err != nil {
			return err
		}
		var c store.ChatConversation
		a, c, err = readApprovalForUpdate(ctx, tx, p.ApprovalID)
		if err != nil {
			return err
		}
		replay, err := approvalActionReplay(ctx, tx, p, c.CourseID, "review")
		if err != nil {
			return err
		}
		if !replay {
			if a.Mode != store.ChatApprovalModeReview || a.State != store.ChatApprovalPending || c.State == store.ChatClosed {
				return store.ErrChatApproval
			}
			g, err := scanChatGeneration(tx.QueryRow(ctx, `SELECT `+chatGenerationColumns+` FROM chat_generation WHERE id=$1 FOR UPDATE`, a.GenerationID))
			if err != nil {
				return err
			}
			u, err := latestChatUserPG(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			if u.ID != g.UserMessageID || u.Status == store.ChatRetracted {
				return store.ErrChatApproval
			}
			if err = tx.QueryRow(ctx, `UPDATE chat_approval SET state='reviewed',decided_at=now() WHERE id=$1 RETURNING decided_at`, a.ID).Scan(&a.DecidedAt); err != nil {
				return err
			}
			a.State = store.ChatApprovalReviewed
			if _, err = tx.Exec(ctx, `UPDATE chat_conversation SET revision=revision+1,updated_at=now() WHERE id=$1`, c.ID); err != nil {
				return err
			}
			if err = insertApprovalAction(ctx, tx, p, c.CourseID, "review"); err != nil {
				return err
			}
		}
		snap, err = chatSnapshotQ(ctx, tx, c.CourseID, c.ID, c.OpenerActorID, 50, 0)
		return err
	})
	return
}
