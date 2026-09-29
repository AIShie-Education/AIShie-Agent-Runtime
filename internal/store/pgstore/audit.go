package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// RecordAudit records e, and returns its id.
func (s *Store) RecordAudit(ctx context.Context, e store.AuditEvent) (int64, error) {
	e, err := store.CheckAuditEvent(e)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO audit (at, actor_id, session_id, ip, action, target_type, target_id, outcome, detail)
		VALUES (COALESCE($1::timestamptz, now()), $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
		RETURNING id`,
		orNow(e.At), e.ActorID, e.SessionID, e.IP, e.Action, e.TargetType, e.TargetID, e.Outcome, string(e.Detail)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: record audit %s: %w", e.Action, err)
	}
	return id, nil
}

// AuditEvents lists up to limit events recorded at or after since, oldest
// first.
func (s *Store) AuditEvents(ctx context.Context, since time.Time, limit int) ([]store.AuditEvent, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, at, actor_id, session_id, ip, action, target_type, target_id, outcome, detail::text
		  FROM audit WHERE at >= $1 ORDER BY at, id LIMIT $2`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("store: audit events: %w", err)
	}
	defer rows.Close()
	var out []store.AuditEvent
	for rows.Next() {
		var e store.AuditEvent
		var detail string
		if err := rows.Scan(&e.ID, &e.At, &e.ActorID, &e.SessionID, &e.IP, &e.Action, &e.TargetType, &e.TargetID, &e.Outcome, &detail); err != nil {
			return nil, fmt.Errorf("store: audit events: %w", err)
		}
		utc(&e.At)
		e.Detail = []byte(detail)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: audit events: %w", err)
	}
	return out, nil
}

// PruneAudit destroys the events recorded before before.
func (s *Store) PruneAudit(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM audit WHERE at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("store: prune audit: %w", err)
	}
	return tag.RowsAffected(), nil
}
