package worker

import (
	"context"
	"time"
)

// defaultRetentionDays is how long a seat's memory is kept after it left,
// for an agent no longer configured (config.Defaults' own).
const defaultRetentionDays = 30

// housekeep purges the memory of seats gone longer ago than their agent's
// retention_days_after_removal (§2.5): notes, attempts and cursors, then the
// seat's row. The ledger's ids and numbers stay. Any worker may do it; it
// is the same work done twice at worst.
func (s *Supervisor) housekeep(ctx context.Context) {
	cfg := s.config()
	if cfg == nil {
		return
	}
	retention := map[string]int{}
	least := defaultRetentionDays
	for _, a := range cfg.Agents {
		days := max(a.Memory.RetentionDaysAfterRemoval, 0)
		retention[a.ID] = days
		least = min(least, days)
	}
	now := s.o.Now()
	gone, err := s.o.Store.SeatsGoneBefore(ctx, now.Add(-days(least)))
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("housekeeping: seats gone not read", "err", err)
		}
		return
	}
	for _, seat := range gone {
		keep, ok := retention[seat.AgentID]
		if !ok {
			keep = defaultRetentionDays
		}
		if seat.GoneAt == nil || seat.GoneAt.After(now.Add(-days(keep))) {
			continue
		}
		if err := s.o.Store.PurgeMember(ctx, seat.AgentID, seat.MemberID); err != nil {
			s.log.Warn("housekeeping: a seat's memory not purged", "agent", seat.AgentID, "member", seat.MemberID, "err", err)
			continue
		}
		if err := s.o.Store.ForgetSeat(ctx, seat.AgentID, seat.MemberID); err != nil {
			s.log.Warn("housekeeping: a seat not forgotten", "agent", seat.AgentID, "member", seat.MemberID, "err", err)
			continue
		}
		s.log.Info("housekeeping: the memory of a seat gone was purged", "agent", seat.AgentID, "member", seat.MemberID,
			"course", seat.CourseID, "gone_at", seat.GoneAt.UTC().Format(time.RFC3339))
	}
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }
