package config

import (
	"fmt"
	"slices"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
)

// noTable is what a quota in dollars with no price table at all is told.
const noTable = "there is no price table (PRICES, runtime.prices_ref or the site's prices) to hold it to"

// USDWithoutPrices lists the agents with a quota in dollars that no price
// can hold: there is no price table, or no row prices their model (or its
// fallback) today; and the school's plan, with such a quota, whose offers
// the table does not all price. A course's own settings count as the
// agent's.
func USDWithoutPrices(cfg *Config, t *pricing.Table, at time.Time) []string {
	var out []string
	if sc := cfg.Runtime.School; sc.USD() && t == nil {
		out = append(out, "runtime.school: it has a quota in dollars, and "+noTable)
	} else if sc.USD() {
		for _, id := range UnpricedOffers(sc, t, at) {
			o, _ := sc.OfferOf(id)
			m := o.AsModel()
			out = append(out, fmt.Sprintf("runtime.school: it has a quota in dollars, and the price table has no price for offer %s, %s %s",
				o.ID, m.EffectiveProvider(), m.Model))
		}
	}
	return append(out, AgentsUSDWithoutPrices(cfg, t, at)...)
}

// UnpricedOffers are the ids of the offers of sc that t does not price at
// at, in sc's order: with a quota of the plan's in dollars, none may be.
func UnpricedOffers(sc School, t *pricing.Table, at time.Time) []string {
	var out []string
	for _, o := range sc.Offers {
		m := o.AsModel()
		if _, ok := t.Lookup(m.EffectiveProvider(), m.Model, at); !ok {
			out = append(out, o.ID)
		}
	}
	return out
}

// AgentsUSDWithoutPrices is USDWithoutPrices of cfg's agents alone: of an
// agent on an offer of the school's plan, the offer's model is its own.
func AgentsUSDWithoutPrices(cfg *Config, t *pricing.Table, at time.Time) []string {
	var out []string
	for _, a := range cfg.Agents {
		views := []*Effective{{Agent: *a, Enabled: true}}
		courses := make([]string, 0, len(a.Courses))
		for id := range a.Courses {
			courses = append(courses, id)
		}
		slices.Sort(courses)
		for _, id := range courses {
			if e, err := a.ForCourse(id); err == nil {
				views = append(views, e)
			}
		}
		seen := map[string]bool{}
		for _, e := range views {
			if !usdApplies(cfg, e) {
				continue
			}
			models := []Model{e.Model}
			if e.Model.Fallback != nil {
				models = append(models, *e.Model.Fallback)
			}
			for _, m := range models {
				provider := m.EffectiveProvider()
				var p string
				switch _, ok := t.Lookup(provider, m.Model, at); {
				case t == nil:
					p = fmt.Sprintf("agent %q: it has a quota in dollars, and %s", a.ID, noTable)
				case !ok:
					p = fmt.Sprintf("agent %q: it has a quota in dollars, and the price table has no price for %s %s", a.ID, provider, m.Model)
				}
				if p != "" && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// usdApplies reports whether a quota in dollars applies to e: its own per
// agent or per asker; on the school's key, its tenant's, or the school
// plan's ceiling; on an offer of the plan, any of the plan's.
func usdApplies(cfg *Config, e *Effective) bool {
	if e.Budgets.PerAgentDay.USD != nil || e.Budgets.PerAskerDay.USD != nil {
		return true
	}
	if e.Model.KeySource != KeySchool {
		return false
	}
	sc := cfg.Runtime.School
	if sc.PerDay.USD != nil || e.Model.Offer != "" && sc.USD() {
		return true
	}
	if e.TenantID == "" {
		return false
	}
	t, ok := cfg.Runtime.Tenants[e.TenantID]
	return ok && t.PerDay.USD != nil
}
