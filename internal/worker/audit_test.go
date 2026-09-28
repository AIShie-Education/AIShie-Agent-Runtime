package worker

import (
	"context"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
)

// Housekeeping destroys the API's audit events older than AuditRetention,
// and keeps the rest.
func TestHousekeepingPrunesTheAudit(t *testing.T) {
	w := newWorld(t)
	st := memstore.New()
	now := time.Now()
	for _, age := range []time.Duration{AuditRetention + time.Hour, AuditRetention - time.Hour, time.Minute} {
		if _, err := st.RecordAudit(context.Background(), store.AuditEvent{At: now.Add(-age), Action: "agent.pause", Outcome: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	w.start(&config.Config{}, models{}, workerOpts{store: st})
	eventually(t, "the oldest audit event destroyed", func() bool {
		events, err := st.AuditEvents(context.Background(), time.Time{}, 10)
		return err == nil && len(events) == 2 && events[0].At.After(now.Add(-AuditRetention))
	})
}
