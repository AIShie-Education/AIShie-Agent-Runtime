package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// SeatSeen records the seat as current, clearing any GoneAt.
func (s *Store) SeatSeen(ctx context.Context, agentID, memberID, courseID string, at time.Time) error {
	if err := required("agent_id", agentID, "member_id", memberID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO seat (agent_id, member_id, course_id, seen_at, gone_at)
		VALUES ($1, $2, $3, COALESCE($4::timestamptz, now()), NULL)
		ON CONFLICT (agent_id, member_id) DO UPDATE
		   SET course_id = EXCLUDED.course_id, seen_at = EXCLUDED.seen_at, gone_at = NULL`,
		agentID, memberID, courseID, orNow(at))
	if err != nil {
		return fmt.Errorf("store: seat %s seen: %w", memberID, err)
	}
	return nil
}

// SeatGone records when the seat was first missed; a later call keeps the
// first time. A seat never seen is store.ErrNotFound.
func (s *Store) SeatGone(ctx context.Context, agentID, memberID string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE seat SET gone_at = COALESCE(gone_at, $3::timestamptz, now())
		 WHERE agent_id = $1 AND member_id = $2`,
		agentID, memberID, orNow(at))
	if err != nil {
		return fmt.Errorf("store: seat %s gone: %w", memberID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("seat %s: %w", memberID, store.ErrNotFound)
	}
	return nil
}

// querySeats lists the seats a query returns.
func (s *Store) querySeats(ctx context.Context, sql string, args ...any) ([]store.SeatRef, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.SeatRef
	for rows.Next() {
		var r store.SeatRef
		if err := rows.Scan(&r.AgentID, &r.MemberID, &r.CourseID, &r.SeenAt, &r.GoneAt); err != nil {
			return nil, err
		}
		utc(&r.SeenAt)
		if r.GoneAt != nil {
			utc(r.GoneAt)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// KnownSeats lists the agent's seats, current and gone, by member id. Ids
// sort bytewise (COLLATE "C"), as memstore sorts them, whatever the
// database's collation.
func (s *Store) KnownSeats(ctx context.Context, agentID string) ([]store.SeatRef, error) {
	out, err := s.querySeats(ctx, `
		SELECT agent_id, member_id, course_id, seen_at, gone_at FROM seat
		 WHERE agent_id = $1
		 ORDER BY member_id COLLATE "C"`, agentID)
	if err != nil {
		return nil, fmt.Errorf("store: seats of agent %s: %w", agentID, err)
	}
	return out, nil
}

// SeatsGoneBefore lists seats of any agent gone before t, in the order they
// went.
func (s *Store) SeatsGoneBefore(ctx context.Context, t time.Time) ([]store.SeatRef, error) {
	out, err := s.querySeats(ctx, `
		SELECT agent_id, member_id, course_id, seen_at, gone_at FROM seat
		 WHERE gone_at < $1
		 ORDER BY gone_at, agent_id COLLATE "C", member_id COLLATE "C"`, t)
	if err != nil {
		return nil, fmt.Errorf("store: seats gone: %w", err)
	}
	return out, nil
}

// ForgetSeat removes the seat's row; a seat not known is nothing.
func (s *Store) ForgetSeat(ctx context.Context, agentID, memberID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM seat WHERE agent_id = $1 AND member_id = $2`, agentID, memberID); err != nil {
		return fmt.Errorf("store: forget seat %s: %w", memberID, err)
	}
	return nil
}
