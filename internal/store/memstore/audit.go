package memstore

import (
	"context"
	"slices"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// RecordAudit records e, and returns its id.
func (s *Store) RecordAudit(_ context.Context, e store.AuditEvent) (int64, error) {
	e, err := store.CheckAuditEvent(e)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditID++
	e.ID, e.At, e.Detail = s.auditID, s.orNow(e.At), slices.Clone(e.Detail)
	s.audit = append(s.audit, e)
	return e.ID, nil
}

// AuditEvents lists up to limit events recorded at or after since, oldest
// first.
func (s *Store) AuditEvents(_ context.Context, since time.Time, limit int) ([]store.AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.AuditEvent
	for _, e := range s.audit {
		if !e.At.Before(since) {
			e.Detail = slices.Clone(e.Detail)
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(x, y store.AuditEvent) int {
		if c := x.At.Compare(y.At); c != 0 {
			return c
		}
		return int(x.ID - y.ID)
	})
	if limit < len(out) {
		out = out[:max(limit, 0)]
	}
	return out, nil
}

// PruneAudit destroys the events recorded before before.
func (s *Store) PruneAudit(_ context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.audit)
	s.audit = slices.DeleteFunc(s.audit, func(e store.AuditEvent) bool { return e.At.Before(before) })
	return int64(n - len(s.audit)), nil
}
