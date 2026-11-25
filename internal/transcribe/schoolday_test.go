package transcribe

import (
	"context"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

// Pages used before the school's midnight do not consume the new school
// day's allowance, although both instants are on the same UTC date.
func TestReserveStartsPagesAtSchoolMidnight(t *testing.T) {
	hk, err := time.LoadLocation("Asia/Hong_Kong")
	if err != nil {
		t.Fatal(err)
	}
	st := memstore.New()
	before := time.Date(2026, 10, 1, 15, 30, 0, 0, time.UTC) // 23:30 on 1 October in Hong Kong.
	now := time.Date(2026, 10, 1, 16, 30, 0, 0, time.UTC)    // 00:30 on 2 October in Hong Kong.
	finished := before.Add(time.Minute)
	if _, err := st.PutTranscriptionJob(context.Background(), store.TranscriptionJob{
		ID: "trj_before_school_midnight", VersionID: "v1", Status: store.JobDone,
		PagesSent: 1, StartedAt: before, HeartbeatAt: before, FinishedAt: &finished,
	}); err != nil {
		t.Fatal(err)
	}
	limit := 1
	s := New(Options{Store: st, Now: func() time.Time { return now }, Location: func() *time.Location { return hk }})
	ok, err := s.reserve(context.Background(), Setting{Site: config.SiteTranscription{PerDayPages: &limit}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the previous school day's page blocked the new school day")
	}
	if got := store.Day(now, hk); !s.reservedOn.Equal(got) || s.reserved != 1 {
		t.Errorf("reserved %d pages on %v, want 1 on %v", s.reserved, s.reservedOn, got)
	}
}
