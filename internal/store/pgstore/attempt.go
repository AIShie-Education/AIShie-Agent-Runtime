package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// attemptColumns are what scanAttempt reads, in its order.
const attemptColumns = `key, agent_id, member_id, course_id, conversation_id, message_id, attempt_no,
	tool, args, kind, state, COALESCE(action_id, ''), COALESCE(posted_message_id, ''),
	COALESCE(error_code, ''), COALESCE(reason, ''), created_at, updated_at`

func scanAttempt(row pgx.Row) (*store.Attempt, error) {
	var a store.Attempt
	err := row.Scan(&a.Key, &a.AgentID, &a.MemberID, &a.CourseID, &a.ConversationID, &a.MessageID, &a.No,
		&a.Tool, &a.Args, &a.Kind, (*string)(&a.State), &a.ActionID, &a.PostedMessageID,
		&a.ErrorCode, &a.Reason, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	utc(&a.CreatedAt)
	utc(&a.UpdatedAt)
	return &a, nil
}

// queryAttempts lists the attempts a query returns.
func (s *Store) queryAttempts(ctx context.Context, sql string, args ...any) ([]store.Attempt, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// knownState reports whether st is one of store.go's attempt states. The
// schema checks it too; this says so before the database is asked, and
// without its words.
func knownState(st store.AttemptState) bool {
	switch st {
	case store.AttemptSending, store.AttemptExecuted, store.AttemptProposed,
		store.AttemptFailed, store.AttemptDenied, store.AttemptError,
		store.AttemptRejected, store.AttemptCancelled:
		return true
	}
	return false
}

// putTries bounds PutAttempt's loop: a key found taken and then gone (its
// seat purged in between) is tried again, but not for ever.
const putTries = 3

// PutAttempt writes a's row, or returns the row already under its key with
// store.ErrExists. An empty State is written as sending. Of writers racing
// for one key, the first insert wins; the others wait for it to commit, do
// nothing, and read its row.
func (s *Store) PutAttempt(ctx context.Context, a store.Attempt) (*store.Attempt, error) {
	if err := required("agent_id", a.AgentID, "key", a.Key); err != nil {
		return nil, err
	}
	if a.State == "" {
		a.State = store.AttemptSending
	}
	if !knownState(a.State) {
		return nil, fmt.Errorf("store: attempt %s: unknown state %q", a.Key, a.State)
	}
	if a.Args == nil {
		a.Args = []byte{}
	}
	for range putTries {
		row, err := scanAttempt(s.pool.QueryRow(ctx, `
			INSERT INTO attempt (agent_id, key, member_id, course_id, conversation_id, message_id, attempt_no,
			                     tool, args, kind, state, action_id, posted_message_id, error_code, reason,
			                     created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			        NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, ''), NULLIF($15, ''),
			        COALESCE($16::timestamptz, now()), COALESCE($16::timestamptz, now()))
			ON CONFLICT (agent_id, key) DO NOTHING
			RETURNING `+attemptColumns,
			a.AgentID, a.Key, a.MemberID, a.CourseID, a.ConversationID, a.MessageID, a.No,
			a.Tool, a.Args, a.Kind, string(a.State), a.ActionID, a.PostedMessageID, a.ErrorCode, a.Reason,
			orNow(a.CreatedAt)))
		if err == nil {
			return row, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("store: put attempt %s: %w", a.Key, err)
		}
		row, err = s.Attempt(ctx, a.AgentID, a.Key)
		if err == nil {
			return row, store.ErrExists
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("store: put attempt %s: the key was taken and freed %d times over", a.Key, putTries)
}

// FinishAttempt records o for the attempt under key. An action id or posted
// message id already known is kept when o has none; o's error code and
// reason replace the old ones.
func (s *Store) FinishAttempt(ctx context.Context, agentID, key string, o store.Outcome) error {
	if !knownState(o.State) {
		return fmt.Errorf("store: attempt %s: unknown state %q", key, o.State)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE attempt
		   SET state = $3,
		       action_id = COALESCE(NULLIF($4, ''), action_id),
		       posted_message_id = COALESCE(NULLIF($5, ''), posted_message_id),
		       error_code = NULLIF($6, ''),
		       reason = NULLIF($7, ''),
		       updated_at = now()
		 WHERE agent_id = $1 AND key = $2`,
		agentID, key, string(o.State), o.ActionID, o.PostedMessageID, o.ErrorCode, o.Reason)
	if err != nil {
		return fmt.Errorf("store: finish attempt %s: %w", key, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("attempt %s: %w", key, store.ErrNotFound)
	}
	return nil
}

// Attempt is the attempt under key, or store.ErrNotFound.
func (s *Store) Attempt(ctx context.Context, agentID, key string) (*store.Attempt, error) {
	a, err := scanAttempt(s.pool.QueryRow(ctx,
		`SELECT `+attemptColumns+` FROM attempt WHERE agent_id = $1 AND key = $2`, agentID, key))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("attempt %s: %w", key, store.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: attempt %s: %w", key, err)
	}
	return a, nil
}

// AttemptsFor lists the attempts at answering one message, by number.
func (s *Store) AttemptsFor(ctx context.Context, agentID, conversationID, messageID string) ([]store.Attempt, error) {
	out, err := s.queryAttempts(ctx, `
		SELECT `+attemptColumns+` FROM attempt
		 WHERE agent_id = $1 AND conversation_id = $2 AND message_id = $3
		 ORDER BY attempt_no, created_at, seq`, agentID, conversationID, messageID)
	if err != nil {
		return nil, fmt.Errorf("store: attempts at message %s: %w", messageID, err)
	}
	return out, nil
}

// AttemptByAction is the oldest attempt Core recorded as actionID, or
// store.ErrNotFound. An empty actionID names no action.
func (s *Store) AttemptByAction(ctx context.Context, agentID, actionID string) (*store.Attempt, error) {
	if actionID == "" {
		return nil, fmt.Errorf("attempt of no action: %w", store.ErrNotFound)
	}
	a, err := scanAttempt(s.pool.QueryRow(ctx, `
		SELECT `+attemptColumns+` FROM attempt
		 WHERE agent_id = $1 AND action_id = $2
		 ORDER BY created_at, seq
		 LIMIT 1`, agentID, actionID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("attempt of action %s: %w", actionID, store.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: attempt of action %s: %w", actionID, err)
	}
	return a, nil
}

// Unsettled lists one seat's attempts still sending or proposed, oldest
// first.
func (s *Store) Unsettled(ctx context.Context, agentID, memberID string) ([]store.Attempt, error) {
	out, err := s.queryAttempts(ctx, `
		SELECT `+attemptColumns+` FROM attempt
		 WHERE agent_id = $1 AND member_id = $2 AND state IN ('sending', 'proposed')
		 ORDER BY created_at, seq`, agentID, memberID)
	if err != nil {
		return nil, fmt.Errorf("store: unsettled attempts of seat %s: %w", memberID, err)
	}
	return out, nil
}
