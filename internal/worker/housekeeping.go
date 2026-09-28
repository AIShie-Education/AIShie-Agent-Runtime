package worker

import (
	"context"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// defaultRetentionDays is how long a seat's memory is kept after it left,
// for an agent no longer configured (config.Defaults' own).
const defaultRetentionDays = 30

// AuditRetention is how long the API's audit is kept: about 13 months
// (the product owner's D11).
const AuditRetention = 400 * 24 * time.Hour

// OrphanAge is how long what the store holds of a hosted agent that is
// gone (deleted through the API) is kept after its last state: the API
// purges it at once, and housekeeping purges what a worker still running
// the agent wrote meanwhile.
const OrphanAge = 10 * time.Minute

// housekeep purges the memory of seats gone longer ago than their agent's
// retention_days_after_removal (§2.5): notes, attempts and cursors, then the
// seat's row. The ledger's ids and numbers stay. It also destroys the
// audit's events older than AuditRetention, the texts OCR recognized
// older than ocr.TextRetention and its failures older than
// ocr.FailedRetention, and purges hosted agents that are gone
// (purgeGone). Any worker may do it; it is the same work done twice at
// worst.
func (s *Supervisor) housekeep(ctx context.Context) {
	if n, err := s.o.Store.PruneAudit(ctx, s.o.Now().Add(-AuditRetention)); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("housekeeping: the audit not pruned", "err", err)
		}
	} else if n > 0 {
		s.log.Info("housekeeping: the audit's oldest events were destroyed", "events", n)
	}
	if n, err := s.o.Store.PurgeOCRTexts(ctx, s.o.Now().Add(-ocr.TextRetention), s.o.Now().Add(-ocr.FailedRetention)); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("housekeeping: the texts OCR recognized not purged", "err", err)
		}
	} else if n > 0 {
		s.log.Info("housekeeping: the oldest texts OCR recognized were destroyed, and its failures tried again when next asked", "texts", n)
	}
	cfg := s.config()
	if cfg == nil {
		return
	}
	s.purgeGone(ctx, cfg)
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

// purgeGone purges what the store holds of each hosted agent (agt_…) that
// has a state, is in no configuration (cfg's agents or rejections) and not
// in the registry, and whose last state is OrphanAge old or more: all of
// it but its ledger (store.PurgeAgent). A registry that cannot be read
// purges nothing.
func (s *Supervisor) purgeGone(ctx context.Context, cfg *config.Config) {
	states, err := s.o.Store.AgentStates(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("housekeeping: the agents' states not read", "err", err)
		}
		return
	}
	hosted, err := s.o.Store.HostedAgents(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("housekeeping: the registry not read; no agent gone is purged", "err", err)
		}
		return
	}
	known := map[string]bool{}
	for _, a := range cfg.Agents {
		known[a.ID] = true
	}
	for _, r := range cfg.Rejected {
		known[r.AgentID] = true
	}
	for _, a := range hosted {
		known[a.ID] = true
	}
	now := s.o.Now()
	for _, st := range states {
		if !store.IsHostedAgentID(st.AgentID) || known[st.AgentID] || now.Sub(st.UpdatedAt) < OrphanAge {
			continue
		}
		if err := s.o.Store.PurgeAgent(ctx, st.AgentID); err != nil {
			s.log.Warn("housekeeping: a hosted agent gone not purged", "agent", st.AgentID, "err", err)
			continue
		}
		s.log.Info("housekeeping: what the store held of a hosted agent gone was purged; its ledger stays", "agent", st.AgentID)
	}
}
