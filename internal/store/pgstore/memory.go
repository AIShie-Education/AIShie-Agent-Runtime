package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Cursor is the cursor, or "" when there is none yet.
func (s *Store) Cursor(ctx context.Context, agentID, memberID, kind string) (string, error) {
	var v string
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM cursor WHERE agent_id = $1 AND member_id = $2 AND kind = $3`,
		agentID, memberID, kind).Scan(&v)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("store: cursor %s of seat %s: %w", kind, memberID, err)
	}
	return v, nil
}

// SetCursor records value as the seat's cursor of kind.
func (s *Store) SetCursor(ctx context.Context, agentID, memberID, kind, value string) error {
	if err := required("agent_id", agentID, "member_id", memberID, "kind", kind); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cursor (agent_id, member_id, kind, value, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (agent_id, member_id, kind) DO UPDATE
		   SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		agentID, memberID, kind, value)
	if err != nil {
		return fmt.Errorf("store: set cursor %s of seat %s: %w", kind, memberID, err)
	}
	return nil
}

// AddNote remembers n in its conversation.
func (s *Store) AddNote(ctx context.Context, n store.Note) error {
	if err := required("agent_id", n.AgentID, "member_id", n.MemberID, "conversation_id", n.ConversationID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO note (agent_id, member_id, conversation_id, kind, text, message_id, created_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), COALESCE($7::timestamptz, now()))`,
		n.AgentID, n.MemberID, n.ConversationID, n.Kind, n.Text, n.MessageID, orNow(n.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: add note to conversation %s: %w", n.ConversationID, err)
	}
	return nil
}

// Notes are the conversation's newest limit notes, oldest first; none when
// limit is not positive.
func (s *Store) Notes(ctx context.Context, agentID, memberID, conversationID string, limit int) ([]store.Note, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT agent_id, member_id, conversation_id, kind, text, message_id, created_at
		  FROM (SELECT id, agent_id, member_id, conversation_id, kind, text,
		               COALESCE(message_id, '') AS message_id, created_at
		          FROM note
		         WHERE agent_id = $1 AND member_id = $2 AND conversation_id = $3
		         ORDER BY created_at DESC, id DESC
		         LIMIT $4) newest
		 ORDER BY created_at, id`,
		agentID, memberID, conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: notes of conversation %s: %w", conversationID, err)
	}
	defer rows.Close()
	var out []store.Note
	for rows.Next() {
		var n store.Note
		if err := rows.Scan(&n.AgentID, &n.MemberID, &n.ConversationID, &n.Kind, &n.Text, &n.MessageID, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: notes of conversation %s: %w", conversationID, err)
		}
		utc(&n.CreatedAt)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: notes of conversation %s: %w", conversationID, err)
	}
	return out, nil
}

// ForgetMessage removes the notes about messageID. An empty messageID names
// no message, and removes nothing.
func (s *Store) ForgetMessage(ctx context.Context, agentID, memberID, conversationID, messageID string) error {
	if messageID == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM note
		 WHERE agent_id = $1 AND member_id = $2 AND conversation_id = $3 AND message_id = $4`,
		agentID, memberID, conversationID, messageID)
	if err != nil {
		return fmt.Errorf("store: forget message %s: %w", messageID, err)
	}
	return nil
}

// PurgeMember removes everything the store holds in the seat: its notes,
// its attempts, whose bytes hold its answers, and its cursors. The seat's
// row is ForgetSeat's to remove, and the ledger keeps its ids and numbers.
// One statement does it all, so that it is done whole or not at all.
// PurgeAgent removes everything the store holds of an agent but its
// ledger, in one statement: every seat's notes, attempts and cursors, its
// seats, its state and its leases. A lease's name holds the agent's id,
// which a LIKE pattern would read as one, so the prefix is compared as
// text.
func (s *Store) PurgeAgent(ctx context.Context, agentID string) error {
	if err := required("agent_id", agentID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		WITH notes AS (DELETE FROM note WHERE agent_id = $1),
		     attempts AS (DELETE FROM attempt WHERE agent_id = $1),
		     cursors AS (DELETE FROM cursor WHERE agent_id = $1),
		     seats AS (DELETE FROM seat WHERE agent_id = $1),
		     states AS (DELETE FROM agent_state WHERE agent_id = $1)
		DELETE FROM lease WHERE name = 'agent:' || $1 OR left(name, length('conv:' || $1 || ':')) = 'conv:' || $1 || ':'`,
		agentID)
	if err != nil {
		return fmt.Errorf("store: purge agent %s: %w", agentID, err)
	}
	return nil
}

func (s *Store) PurgeMember(ctx context.Context, agentID, memberID string) error {
	_, err := s.pool.Exec(ctx, `
		WITH notes AS (DELETE FROM note WHERE agent_id = $1 AND member_id = $2),
		     attempts AS (DELETE FROM attempt WHERE agent_id = $1 AND member_id = $2)
		DELETE FROM cursor WHERE agent_id = $1 AND member_id = $2`,
		agentID, memberID)
	if err != nil {
		return fmt.Errorf("store: purge seat %s: %w", memberID, err)
	}
	return nil
}
