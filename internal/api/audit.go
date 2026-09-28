package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

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

// auditing is the audit event of one request to a route that writes, or
// asks Core about a token: its handler names the target and adds the
// detail as it learns them, and the route records the event once the
// answer is written, as ok or as the refusal's reason (a row per write and
// per refusal). Skip is set where nothing changed that the audit keeps (a
// pause of an agent paused already).
type auditing struct {
	targetType, targetID string
	detail               map[string]any
	skip                 bool
}

// target names what the request is about.
func (au *auditing) target(typ, id string) { au.targetType, au.targetID = typ, id }

// audited serves h, then records its audit event for action: the target
// the handler named (a hosted agent, the id in the path, by default), the
// outcome of its answer, and its detail. What is recorded is ids, hints,
// providers, models and results: never a secret, a name, or text anyone
// wrote, which the handlers never add.
func (s *Server) audited(action string, h func(http.ResponseWriter, *http.Request, *Caller, *auditing)) func(http.ResponseWriter, *http.Request, *Caller) {
	return func(w http.ResponseWriter, r *http.Request, c *Caller) {
		au := &auditing{targetType: "hosted_agent", targetID: r.PathValue("id"), detail: map[string]any{}}
		if au.targetID == "" {
			au.targetType = ""
		}
		h(w, r, c, au)
		if au.skip {
			return
		}
		outcome := "ok"
		if rec, ok := w.(*recorder); ok && rec.status >= 400 {
			outcome = rec.reason
			if outcome == "" {
				outcome = strconv.Itoa(rec.status)
			}
		}
		detail, err := json.Marshal(au.detail)
		if err != nil {
			detail = []byte(`{}`)
		}
		s.Audit(r.Context(), r, store.AuditEvent{Action: action, TargetType: au.targetType, TargetID: au.targetID, Outcome: outcome,
			Detail: detail})
	}
}

// mustJSON is v as JSON: {} when it cannot be.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
