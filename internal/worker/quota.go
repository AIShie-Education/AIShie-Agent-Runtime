package worker

import (
	"context"
	"math"
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// recentAnswers is how many of an agent's newest answers predict the cost
// of the next (§5.3: "check before a loop, on the p95 of recent answers").
const recentAnswers = 50

// quotaScope is one daily quota and what it counts. school is set for a
// quota of the school's key, which the owner's own key does not draw on.
type quotaScope struct {
	name   string
	quota  config.Quota
	scope  store.SpendScope
	school bool
}

// spentQuota is a quota a question finds spent: its name, as
// Metrics.BudgetExhausted names it ("asker_answers", "tenant_usd",
// "school_owner_answers", …), and whether it is one of the school's key's.
type spentQuota struct {
	name   string
	school bool
}

// quota checks the day's quotas before a loop (§5.3; design §5.3 step 4):
// the asker's, keyed on (course, opener member); the agent's; and, on the
// school's key, the tenant's across its agents, or, on an offer of the
// school's plan, the plan's per asker and per owner (the tenant); and the
// plan's ceiling across everything on the school's key. The school's are
// counted on the school's key alone, and checked after the agent's own,
// which count every answer. Each counts answers and dollars since the
// start of the UTC day; dollars are checked with the p95 of the agent's
// recent answers added to what is spent, so that the answer about to be
// written is likely to fit too. It returns the first quota that is spent,
// or none.
func (c *claim) quota(ctx context.Context) (*spentQuota, error) {
	b := c.eff.Budgets
	scopes := []quotaScope{
		{name: "asker", quota: b.PerAskerDay, scope: store.SpendScope{AgentID: c.a.id, CourseID: c.s.course, OpenerMemberID: c.opener}},
		{name: "agent", quota: b.PerAgentDay, scope: store.SpendScope{AgentID: c.a.id}},
	}
	if m := c.eff.Model; m.KeySource == config.KeySchool {
		school := c.a.s.school()
		if m.Offer != "" && c.a.cfg.TenantID != "" {
			scopes = append(scopes,
				quotaScope{name: "school_asker", quota: school.AskerQuota(), school: true,
					scope: store.SpendScope{AgentID: c.a.id, CourseID: c.s.course, OpenerMemberID: c.opener, KeySource: config.KeySchool}},
				quotaScope{name: "school_owner", quota: school.OwnerQuota(), school: true,
					scope: store.SpendScope{TenantID: c.a.cfg.TenantID, KeySource: config.KeySchool}})
		}
		if t, ok := c.a.s.tenant(c.a.cfg.TenantID); ok {
			scopes = append(scopes, quotaScope{name: "tenant", quota: t.PerDay, school: true,
				scope: store.SpendScope{TenantID: c.a.cfg.TenantID, KeySource: config.KeySchool}})
		}
		scopes = append(scopes, quotaScope{name: "school_day", quota: school.PerDay, school: true,
			scope: store.SpendScope{KeySource: config.KeySchool}})
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
			return nil, err
		}
		if q.quota.Answers != nil && spent.Answers >= *q.quota.Answers {
			return &spentQuota{name: q.name + "_answers", school: q.school}, nil
		}
		if q.quota.USD == nil {
			continue
		}
		if !predicted {
			costs, err := c.a.store().RecentAnswerCosts(ctx, c.a.id, recentAnswers)
			if err != nil {
				return nil, err
			}
			next, predicted = p95(costs), true
		}
		if overUSD(spent.CostPUSD, pricing.PUSD(*q.quota.USD), next) {
			return &spentQuota{name: q.name + "_usd", school: q.school}, nil
		}
	}
	return nil, nil
}

// onOwnKey reports whether a question over quota q is answered with the
// model's fallback on the owner's own key instead (the product owner's
// D8): q is one of the school's key's, and the fallback is the owner's.
func (c *claim) onOwnKey(q *spentQuota) bool {
	return q.school && c.s.fallback != nil && c.s.fallback.keySource == config.KeyOwn
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
// when on_quota_exhausted is silent, nothing until the next UTC day. A
// quota of the school's key other than a tenant's is the school's plan's,
// and its notice the plan's (config.SchoolQuotaText).
func (c *claim) outOfQuota(ctx context.Context, r passResult, q *spentQuota) passResult {
	c.a.s.o.Metrics.BudgetExhausted.WithLabelValues(q.name).Inc()
	c.s.log.Info("the asker, the agent or the tenant is out of quota", "conversation", c.conv, "opener", c.opener,
		"quota", q.name, "on_quota_exhausted", c.eff.Answer.OnQuotaExhausted)
	if c.eff.Answer.OnQuotaExhausted == config.OnQuotaSilent {
		c.s.holdBack(c.conv, nextDay(c.a.now()), "out of quota ("+q.name+")")
		r.outcome, r.kind = store.OutcomeQuota, kindQuota
		return r
	}
	text := c.eff.Prompt.OnQuotaText
	if q.school && !strings.HasPrefix(q.name, "tenant_") {
		text = config.SchoolQuotaText(c.a.s.school().OnQuotaText, c.eff.Prompt.AnswerLanguage)
	}
	return c.post(ctx, r, text, kindQuota, nil)
}
