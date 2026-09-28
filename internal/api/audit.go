package api

import (
	"context"
	"net/http"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// Audit records e in the store's audit (D11), after the change it is about
// has committed: who, with which of Core's sessions, from where (from the
// request, when e does not say), what, to what, how it came out, and ids,
// hints, providers, models and results in its detail; never a secret, a
// name, or text anyone wrote. The store has no transaction that spans a
// registry write and an audit row, so an event that cannot be recorded is
// logged at error, with its action and target alone, and counted
// (aishie_api_audit_failures_total); the request it is about does not
// fail.
func (s *Server) Audit(ctx context.Context, r *http.Request, e store.AuditEvent) {
	if c := callerOf(r); c != nil {
		if e.ActorID == "" {
			e.ActorID = c.ActorID
		}
		if e.SessionID == "" {
			e.SessionID = c.SessionID
		}
	}
	if e.IP == "" {
		e.IP = clientAddr(r)
	}
	if e.At.IsZero() {
		e.At = s.o.Now().UTC()
	}
	if _, err := s.o.Store.RecordAudit(context.WithoutCancel(ctx), e); err != nil {
		s.m.auditFailures.Inc()
		s.o.Log.Error("an audit event was not recorded", "action", e.Action, "target_type", e.TargetType, "target", e.TargetID, "err", err)
	}
}
