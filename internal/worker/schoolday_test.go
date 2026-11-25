package worker

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// me_get's time_zone reaches the daily quota. The two questions are on the
// same UTC date but different sides of Hong Kong's midnight.
func TestQuotaStartsAtSchoolMidnight(t *testing.T) {
	hk, err := time.LoadLocation("Asia/Hong_Kong")
	if err != nil {
		t.Fatal(err)
	}
	var offset atomic.Int64
	nowFn := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	// Start at 23:30 in Hong Kong, but keep the clock advancing so polling
	// and timers behave exactly as they do in production.
	offset.Store(int64(time.Until(store.Day(time.Now(), hk).Add(23*time.Hour + 30*time.Minute))))
	w := newWorldWith(t, fakecore.Options{TimeZone: "Asia/Hong_Kong"})
	tu := w.tutor("cs101-tutor")
	model := scripted.New(scripted.Reply("Before midnight."), scripted.Reply("After midnight."))
	over := map[string]any{"budgets": map[string]any{"per_asker_day": map[string]any{"answers": 1}}}
	wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", over, nil)), models{"m1": model}, workerOpts{edit: func(o *Options) {
		o.Now = nowFn
	}})
	eventually(t, "the school time zone from me_get", func() bool { return wk.sup.Location().String() == "Asia/Hong_Kong" })
	first, _ := w.ask(0, tu, "Before the school's midnight.")
	if got := w.waitAnswers(first, 1); len(got) != 1 || got[0].Body != "Before midnight." {
		t.Fatalf("the first answer: %+v", got)
	}
	eventually(t, "the first answer in the ledger", func() bool { return len(wk.st.outcomes(first)) == 1 })
	next := store.Day(nowFn(), wk.sup.Location()).AddDate(0, 0, 1).Add(30 * time.Minute)
	offset.Store(int64(time.Until(next)))
	second, _ := w.ask(0, tu, "After the school's midnight.")
	if got := w.waitAnswers(second, 1); len(got) != 1 || got[0].Body != "After midnight." {
		t.Fatalf("the new school day's answer: %+v", got)
	}
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	if n := len(model.Requests()); n != 2 {
		t.Errorf("the model was called %d times, want once on each school day", n)
	}
}
