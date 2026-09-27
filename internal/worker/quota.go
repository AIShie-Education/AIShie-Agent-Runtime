package worker

import (
	"context"
	"math"
	"slices"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// recentAnswers is how many of an agent's newest answers predict the cost
// of the next (§5.3: "check before a loop, on the p95 of recent answers").
const recentAnswers = 50

// quotaScope is one daily quota and what it counts.
type quotaScope struct {
	name  string
	quota config.Quota
	scope store.SpendScope
}

// quota checks the day's quotas before a loop (§5.3; design §5.3 step 4):
// the asker's, keyed on (course, opener member); the agent's; and, on the
// school's key, the tenant's across its agents. Each counts answers and
// dollars since the start of the UTC day; dollars are checked with the p95
// of the agent's recent answers added to what is spent, so that the answer
// about to be written is likely to fit too. It returns the quota that is
// spent, as Metrics.BudgetExhausted names it ("asker_answers",
// "tenant_usd", …), or "".
func (c *claim) quota(ctx context.Context) (string, error) {
	b := c.eff.Budgets
	scopes := []quotaScope{
		{"asker", b.PerAskerDay, store.SpendScope{AgentID: c.a.id, CourseID: c.s.course, OpenerMemberID: c.opener}},
		{"agent", b.PerAgentDay, store.SpendScope{AgentID: c.a.id}},
	}
	if c.eff.Model.KeySource == config.KeySchool && c.a.cfg.TenantID != "" {
		if t, ok := c.a.s.tenant(c.a.cfg.TenantID); ok {
			scopes = append(scopes, quotaScope{"tenant", t.PerDay,
				store.SpendScope{TenantID: c.a.cfg.TenantID, KeySource: config.KeySchool}})
		}
	}
	since := startOfDay(c.a.now())
	var next int64
	predicted := false
	for _, q := range scopes {
		if q.quota.Answers == nil && q.quota.USD == nil {
			continue
		}
		spent, err := c.a.store().Spend(ctx, q.scope, since)
		if err != nil {
			return "", err
		}
		if q.quota.Answers != nil && spent.Answers >= *q.quota.Answers {
			return q.name + "_answers", nil
		}
		if q.quota.USD == nil {
			continue
		}
		if !predicted {
			costs, err := c.a.store().RecentAnswerCosts(ctx, c.a.id, recentAnswers)
			if err != nil {
				return "", err
			}
			next, predicted = p95(costs), true
		}
		if overUSD(spent.CostPUSD, pricing.PUSD(*q.quota.USD), next) {
			return q.name + "_usd", nil
		}
	}
	return "", nil
}

// overUSD reports whether a quota of quota pico-dollars, of which spent is
// spent, cannot take an answer predicted to cost predicted: it is spent
// already, or what is spent and the prediction pass it. A scope that has
// spent nothing today is never refused on the prediction alone: were the
// p95 of the agent's answers above the whole quota, every asker would
// otherwise be refused for ever, since a refusal is never a billable answer
// and the p95 would never come down.
func overUSD(spent, quota, predicted int64) bool {
	if spent >= quota {
		return true
	}
	return spent > 0 && spent > quota-predicted
}

// p95 is the 95th percentile of costs, by the nearest rank; 0 for none.
func p95(costs []int64) int64 {
	if len(costs) == 0 {
		return 0
	}
	s := slices.Clone(costs)
	slices.Sort(s)
	i := int(math.Ceil(0.95*float64(len(s)))) - 1
	return s[max(i, 0)]
}

// outOfQuota answers a question over quota: the canned notice under the
// answer's own key, with no model call, so that the asker knows why; or,
// when on_quota_exhausted is silent, nothing until the next UTC day.
func (c *claim) outOfQuota(ctx context.Context, r passResult, quota string) passResult {
	c.a.s.o.Metrics.BudgetExhausted.WithLabelValues(quota).Inc()
	c.s.log.Info("the asker, the agent or the tenant is out of quota", "conversation", c.conv, "opener", c.opener,
		"quota", quota, "on_quota_exhausted", c.eff.Answer.OnQuotaExhausted)
	if c.eff.Answer.OnQuotaExhausted == config.OnQuotaSilent {
		c.s.holdBack(c.conv, nextDay(c.a.now()), "out of quota ("+quota+")")
		r.outcome, r.kind = store.OutcomeQuota, kindQuota
		return r
	}
	return c.post(ctx, r, c.eff.Prompt.OnQuotaText, kindQuota)
}
