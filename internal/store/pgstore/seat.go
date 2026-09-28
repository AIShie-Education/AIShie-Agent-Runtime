package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// SeatSeen records the seat as current, as r says it is, clearing any
// GoneAt.
func (s *Store) SeatSeen(ctx context.Context, r store.SeatRef) error {
	if err := required("agent_id", r.AgentID, "member_id", r.MemberID); err != nil {
		return err
	}
	perms, err := json.Marshal(r.Perms)
	if err != nil || r.Perms == nil {
		perms = []byte(`{}`)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO seat (agent_id, member_id, course_id, seen_at, gone_at, course_code, course_title, section, status,
		                  answers_course, principal_member_id, perms, course_status)
		VALUES ($1, $2, $3, COALESCE($4::timestamptz, now()), NULL, $5, $6, $7, $8, $9, $10, $11::jsonb, $12)
		ON CONFLICT (agent_id, member_id) DO UPDATE
		   SET course_id = EXCLUDED.course_id, seen_at = EXCLUDED.seen_at, gone_at = NULL,
		       course_code = EXCLUDED.course_code, course_title = EXCLUDED.course_title, section = EXCLUDED.section,
		       status = EXCLUDED.status, answers_course = EXCLUDED.answers_course,
		       principal_member_id = EXCLUDED.principal_member_id, perms = EXCLUDED.perms,
		       course_status = EXCLUDED.course_status`,
		r.AgentID, r.MemberID, r.CourseID, orNow(r.SeenAt), r.CourseCode, r.CourseTitle, r.Section, r.Status,
		r.AnswersCourse, orNull(r.PrincipalMemberID), string(perms), r.CourseStatus)
	if err != nil {
		return fmt.Errorf("store: seat %s seen: %w", r.MemberID, err)
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

// seatColumns are what querySeats reads, in its order.
const seatColumns = `agent_id, member_id, course_id, seen_at, gone_at, course_code, course_title, section, status,
	answers_course, COALESCE(principal_member_id, ''), perms::text, course_status`

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
		var perms string
		if err := rows.Scan(&r.AgentID, &r.MemberID, &r.CourseID, &r.SeenAt, &r.GoneAt, &r.CourseCode, &r.CourseTitle,
			&r.Section, &r.Status, &r.AnswersCourse, &r.PrincipalMemberID, &perms, &r.CourseStatus); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(perms), &r.Perms); err != nil || r.Perms == nil {
			r.Perms = map[string]string{}
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
		SELECT `+seatColumns+` FROM seat
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
		SELECT `+seatColumns+` FROM seat
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
