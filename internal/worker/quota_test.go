package worker

import (
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// TestQuotaCanned: an asker out of answers for the day gets the canned
// notice under the answer's own key, with no model call (§5.3).
func TestQuotaCanned(t *testing.T) {
	w := newWorld(t)
	tu := w.tutor("cs101-tutor")
	model := scripted.New(scripted.Reply("Your one answer today."), scripted.Reply("Ken's answer."))
	over := map[string]any{"budgets": map[string]any{"per_asker_day": map[string]any{"answers": 1}}}
	wk := w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", over, nil)), models{"m1": model}, workerOpts{})
	c1, _ := w.ask(0, tu, "First question.")
	w.waitAnswers(c1, 1)
	c2, m2 := w.ask(0, tu, "Second question.")
	got := w.waitAnswers(c2, 1)
	if got[0].Body != config.DefaultQuotaText || got[0].IdempotencyKey != core.AnswerKey(c2, m2, 1) {
		t.Errorf("answer %+v", got[0])
	}
	// Ken is another asker, with his own quota.
	c3, _ := w.ask(1, tu, "Ken's question.")
	if got := w.waitAnswers(c3, 1); got[0].Body != "Ken's answer." {
		t.Errorf("Ken's answer %q", got[0].Body)
	}
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	if n := len(model.Requests()); n != 2 {
		t.Errorf("the model was called %d times", n)
	}
	eventually(t, "the canned notice in the ledger", func() bool { return len(wk.st.outcomes(c2)) == 1 })
	_, recs := wk.st.ledger()
	for _, r := range recs {
		if r.ConversationID == c2 && (r.Outcome != store.OutcomeQuota || r.Billable) {
			t.Errorf("the canned notice's row %+v", r)
		}
	}
	if got := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": "asker_answers"}); got != 1 {
		t.Errorf("budget_exhausted_total{asker_answers} = %v", got)
	}
	at := wk.waitAttempt("cs101-tutor", core.AnswerKey(c2, m2, 1), store.AttemptExecuted)
	if at.Kind != kindQuota {
		t.Errorf("attempt kind %q", at.Kind)
	}
}

// TestQuotaSilent: on_quota_exhausted silent posts nothing, and holds the
// conversation back until the next UTC day.
func TestQuotaSilent(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Reply("Your one answer today."))
	over := map[string]any{
		"budgets": map[string]any{"per_agent_day": map[string]any{"answers": 1}},
		"answer":  map[string]any{"on_quota_exhausted": "silent"},
	}
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, nil)), models{"m1": model}, workerOpts{})
	c1, _ := w.ask(0, own, "First question.")
	w.waitAnswers(c1, 1)
	c2, _ := w.ask(0, own, "Second question.")
	eventually(t, "the question held back", func() bool {
		st := wk.sup.Status()
		return len(st) == 1 && len(st[0].Seats) == 1 && st[0].Seats[0].HeldBack == 1
	})
	eventually(t, "the question's row in the ledger", func() bool { return len(wk.st.outcomes(c2)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := len(w.answers(c2)); n != 0 {
		t.Errorf("%d answers posted", n)
	}
	if o := wk.st.outcomes(c2); len(o) != 1 || o[0] != store.OutcomeQuota {
		t.Errorf("outcomes %v", o)
	}
	if n := len(model.Requests()); n != 1 {
		t.Errorf("the model was called %d times", n)
	}
}

// TestQuotaDollars: dollars are checked with the p95 of recent answers
// added to what is spent; and a tenant's quota holds across its agents on
// the school's key.
func TestQuotaDollars(t *testing.T) {
	// $1 for 100 input tokens, $0.20 for 20 output: a scripted call costs
	// $1.20.
	prices, err := pricing.Parse([]byte(`version: "test"
prices:
  - {provider: scripted, model: scripted-model, from: 2020-01-01, usd_per_mtok: {input: 10000, cache_read: 0, cache_write: 0, output: 10000}}
`))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("the agent's day", func(t *testing.T) {
		w := newWorld(t)
		own := w.ownAgent("yuki-helper", 0)
		model := scripted.New(scripted.Reply("$1.20 spent."))
		over := map[string]any{"budgets": map[string]any{"per_agent_day": map[string]any{"usd": 2.0}}}
		wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, nil)), models{"m1": model}, workerOpts{prices: prices})
		c1, _ := w.ask(0, own, "First question.")
		w.waitAnswers(c1, 1)
		// $1.20 spent and $1.20 likely: over $2.00.
		c2, _ := w.ask(0, own, "Second question.")
		if got := w.waitAnswers(c2, 1); got[0].Body != config.DefaultQuotaText {
			t.Errorf("second answer %q", got[0].Body)
		}
		calls, _ := wk.st.ledger()
		if len(calls) != 1 || calls[0].CostPUSD != pricing.PUSD(1.2) || calls[0].PriceVersion == "" {
			t.Errorf("model calls %+v", calls)
		}
		if got := counter(t, wk.reg, "llm_cost_usd_total", map[string]string{"key_source": "own"}); got < 1.19 || got > 1.21 {
			t.Errorf("llm_cost_usd_total = %v", got)
		}
	})
	t.Run("the tenant's day, across agents", func(t *testing.T) {
		w := newWorld(t)
		yuki, ken := w.ownAgent("yuki-helper", 0), w.ownAgent("ken-helper", 1)
		school := map[string]any{
			"tenant_id": "ten1",
			"model":     map[string]any{"key_source": "school"},
			"budgets":   map[string]any{"per_agent_day": map[string]any{"answers": 10}, "per_asker_day": map[string]any{"answers": 10}},
		}
		rt := map[string]any{"tenants": map[string]any{"ten1": map[string]any{"per_day": map[string]any{"answers": 1}}}}
		cfg := w.config(rt, w.agentDoc("yuki-helper", "m1", school, nil), w.agentDoc("ken-helper", "m1", school, nil))
		model := scripted.New(scripted.Reply("The tenant's one answer."))
		wk := w.start(cfg, models{"m1": model}, workerOpts{prices: prices})
		c1, _ := w.ask(0, yuki, "Yuki's question.")
		w.waitAnswers(c1, 1)
		c2, _ := w.ask(1, ken, "Ken's question.")
		if got := w.waitAnswers(c2, 1); got[0].Body != config.DefaultQuotaText {
			t.Errorf("Ken's answer %q", got[0].Body)
		}
		if got := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": "tenant_answers"}); got != 1 {
			t.Errorf("budget_exhausted_total{tenant_answers} = %v", got)
		}
	})
}

// TestOverUSD holds the dollar check: spent plus the p95 of recent answers
// against the quota; a quota spent is spent whatever the p95; and a scope
// that has spent nothing today is not refused on a prediction alone.
func TestOverUSD(t *testing.T) {
	for _, c := range []struct {
		name                    string
		spent, quota, predicted int64
		over                    bool
	}{
		{"nothing spent, cheap answers", 0, 100, 10, false},
		{"room for one more", 50, 100, 40, false},
		{"exactly room for one more", 60, 100, 40, false},
		{"one more would pass it", 61, 100, 40, true},
		{"spent, and no answer to predict with", 100, 100, 0, true},
		{"past it", 150, 100, 0, true},
		{"nothing spent, answers dearer than the quota", 0, 100, 250, false},
		{"something spent, answers dearer than the quota", 1, 100, 250, true},
	} {
		if got := overUSD(c.spent, c.quota, c.predicted); got != c.over {
			t.Errorf("%s: overUSD(%d, %d, %d) = %v", c.name, c.spent, c.quota, c.predicted, got)
		}
	}
}

func TestP95(t *testing.T) {
	for _, c := range []struct {
		in   []int64
		want int64
	}{
		{nil, 0},
		{[]int64{5}, 5},
		{[]int64{3, 1, 2}, 3},
		{[]int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160, 170, 180, 190, 200}, 190},
	} {
		if got := p95(c.in); got != c.want {
			t.Errorf("p95(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
