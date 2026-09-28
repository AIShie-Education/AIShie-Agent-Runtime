package storetest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// testAudit: an event is kept as recorded, its detail as the same JSON, a
// zero time the store's now; one without its action or outcome, or whose
// detail is not an object, is refused; events come back oldest first, from
// a time on and up to a limit; pruning destroys those before a time, and
// says how many.
func testAudit(t *testing.T, open Opener) {
	s := open(t)
	ctx := t.Context()
	rev0 := rev(t, s)
	e := store.AuditEvent{At: at(time.Minute), ActorID: "p1", SessionID: "s1", IP: "192.0.2.1", Action: "agent.connect",
		TargetType: "hosted_agent", TargetID: "agt_1", Outcome: "ok", Detail: json.RawMessage(`{"hint": "ais_k7v2m4qhx3ab…", "seats": 2}`)}
	id1, err := s.RecordAudit(ctx, e)
	if err != nil || id1 == 0 {
		t.Fatalf("RecordAudit: %d %v", id1, err)
	}
	before := time.Now()
	id2, err := s.RecordAudit(ctx, store.AuditEvent{Action: "agent.inspect", Outcome: "not_owner", ActorID: "p2"})
	if err != nil || id2 <= id1 {
		t.Fatalf("RecordAudit: %d %v", id2, err)
	}
	after := time.Now()
	id0, err := s.RecordAudit(ctx, store.AuditEvent{At: at(0), Action: "agent.delete", Outcome: "ok", ActorID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []store.AuditEvent{
		{Outcome: "ok"}, {Action: "agent.connect"},
		{Action: "agent.connect", Outcome: "ok", Detail: json.RawMessage(`[1]`)},
		{Action: "agent.connect", Outcome: "ok", Detail: json.RawMessage(`not json`)},
	} {
		if _, err := s.RecordAudit(ctx, bad); err == nil {
			t.Errorf("%+v was recorded", bad)
		}
	}

	all, err := s.AuditEvents(ctx, time.Time{}, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("AuditEvents: %+v %v", all, err)
	}
	if all[0].ID != id0 || all[1].ID != id1 || all[2].ID != id2 {
		t.Errorf("not oldest first: %d %d %d", all[0].ID, all[1].ID, all[2].ID)
	}
	got := all[1]
	sameTime(t, "at", got.At, e.At)
	if got.ActorID != "p1" || got.SessionID != "s1" || got.IP != "192.0.2.1" || got.Action != "agent.connect" ||
		got.TargetType != "hosted_agent" || got.TargetID != "agt_1" || got.Outcome != "ok" {
		t.Errorf("recorded %+v", got)
	}
	sameJSON(t, "detail", got.Detail, e.Detail)
	recent(t, "a zero time", all[2].At, before, after)
	sameJSON(t, "no detail", all[2].Detail, json.RawMessage(`{}`))
	if all[2].TargetType != "" || all[2].SessionID != "" {
		t.Errorf("an event without them: %+v", all[2])
	}

	if some, err := s.AuditEvents(ctx, us(at(time.Minute)), 1); err != nil || len(some) != 1 || some[0].ID != id1 {
		t.Errorf("from a time, one: %+v %v", some, err)
	}
	if none, err := s.AuditEvents(ctx, time.Time{}, 0); err != nil || len(none) != 0 {
		t.Errorf("a limit of none: %+v %v", none, err)
	}

	n, err := s.PruneAudit(ctx, us(at(time.Minute)))
	if err != nil || n != 1 {
		t.Fatalf("PruneAudit: %d %v", n, err)
	}
	left, err := s.AuditEvents(ctx, time.Time{}, 10)
	if err != nil || len(left) != 2 || left[0].ID != id1 {
		t.Errorf("after pruning: %+v %v", left, err)
	}
	if n, err := s.PruneAudit(ctx, us(at(time.Minute))); err != nil || n != 0 {
		t.Errorf("pruning again: %d %v", n, err)
	}
	if r := rev(t, s); r != rev0 {
		t.Errorf("the audit moved the registry's revision from %d to %d", rev0, r)
	}
}
