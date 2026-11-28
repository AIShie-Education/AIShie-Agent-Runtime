package chatwire

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

const recentAnswers = 50

type quotaScope struct {
	name   string
	quota  config.Quota
	scope  store.SpendScope
	school bool
}

// checkQuotas applies the existing ledger's daily spend before any model key
// is resolved. fallbackOnly is selected only for a school-key quota with an
// owner-key fallback, matching the school plan's existing behavior.
func (w *Wiring) checkQuotas(ctx context.Context, runtime config.Runtime, eff *config.Effective, in chat.GenerationInput,
	grant chat.Grant, location *time.Location) (bool, error) {
	scopes := []quotaScope{
		{name: "asker", quota: eff.Budgets.PerAskerDay, scope: store.SpendScope{
			AgentID: eff.ID, CourseID: in.Conversation.CourseID, OpenerMemberID: grant.OpenerMemberID}},
		{name: "agent", quota: eff.Budgets.PerAgentDay, scope: store.SpendScope{AgentID: eff.ID}},
	}
	primarySchool := effectiveKeySource(eff.Model) == config.KeySchool
	if primarySchool {
		school := runtime.School
		if eff.Model.Offer != "" && eff.TenantID != "" {
			scopes = append(scopes,
				quotaScope{name: "school_asker", quota: school.AskerQuota(), school: true, scope: store.SpendScope{
					AgentID: eff.ID, CourseID: in.Conversation.CourseID, OpenerMemberID: grant.OpenerMemberID, KeySource: config.KeySchool}},
				quotaScope{name: "school_owner", quota: school.OwnerQuota(), school: true, scope: store.SpendScope{
					TenantID: eff.TenantID, KeySource: config.KeySchool}},
			)
		}
		if tenant, ok := runtime.Tenants[eff.TenantID]; ok {
			scopes = append(scopes, quotaScope{name: "tenant", quota: tenant.PerDay, school: true, scope: store.SpendScope{
				TenantID: eff.TenantID, KeySource: config.KeySchool}})
		}
		scopes = append(scopes, quotaScope{name: "school_day", quota: school.PerDay, school: true, scope: store.SpendScope{
			KeySource: config.KeySchool}})
	}
	since := store.Day(w.now(), location)
	predicted := false
	var next int64
	for _, q := range scopes {
		if q.quota.Answers == nil && q.quota.USD == nil {
			continue
		}
		spent, err := w.opts.Store.Spend(ctx, q.scope, since)
		if err != nil {
			return false, errStore
		}
		exhausted := q.quota.Answers != nil && spent.Answers >= *q.quota.Answers
		if !exhausted && q.quota.USD != nil {
			if !predicted {
				costs, err := w.opts.Store.RecentAnswerCosts(ctx, eff.ID, recentAnswers)
				if err != nil {
					return false, errStore
				}
				next, predicted = p95(costs), true
			}
			exhausted = overUSD(spent.CostPUSD, pricing.PUSD(*q.quota.USD), next)
		}
		if !exhausted {
			continue
		}
		if q.school && eff.Model.Fallback != nil && effectiveKeySource(*eff.Model.Fallback) == config.KeyOwn {
			return true, nil
		}
		return false, errQuota
	}
	return false, nil
}

func overUSD(spent, quota, predicted int64) bool {
	if spent >= quota {
		return true
	}
	return spent > 0 && spent > quota-predicted
}

func p95(costs []int64) int64 {
	if len(costs) == 0 {
		return 0
	}
	costs = slices.Clone(costs)
	slices.Sort(costs)
	i := int(math.Ceil(0.95*float64(len(costs)))) - 1
	return costs[max(i, 0)]
}
