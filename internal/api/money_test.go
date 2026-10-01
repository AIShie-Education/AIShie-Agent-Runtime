package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

const haikuPrice = `{"id":"haiku-4-5","provider":"anthropic","model":"claude-haiku-4-5","from":"2025-10-01",` +
	`"usd_per_mtok":{"input":1,"output":"5","cache_read":"0.1"}}`

// build is the configuration the registry would run now.
func (h *hostWorld) build(fh *fakeHosting) *config.Config {
	h.t.Helper()
	cfg, _, err := registry.Build(context.Background(), fh.YAML(), h.st, registry.Options{CoreBaseURL: h.srv.URL})
	h.ok(err)
	return cfg
}

// GET /admin/prices is the price file's table with the site's rows before
// it, each with where it comes from and the version a cost it prices is
// recorded under, and the plan's offers no row prices; POST adds a site's
// row, and every change is a new version of the site's table, named by
// the second it was made in, which the registry puts in force: the ledger
// names what it prices by it, and what was priced before keeps its
// version. A site's row of a file's provider, model and from stands
// before it. PATCH and DELETE change the site's rows alone, held to
// If-Match when it names a version. Every write is audited.
func TestPrices(t *testing.T) {
	h, _, fh := newModelWorld(t, schoolRuntime())
	h.st.SetClock(h.clock)
	var table PriceTable
	a := h.admin("GET", "admin/prices", "")
	wantSecured(t, a, "no-store")
	a.decode(t, &table)
	if a.code != 200 || table.Version == nil || *table.Version != "t1" || table.FileVersion == nil || *table.FileVersion != "t1" ||
		table.SiteVersion != nil || table.SiteChangedAt != nil || len(table.Rows) != 5 {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if r := table.Rows[4]; r != (PriceRow{ID: "4", Source: PriceSourceFile, Provider: "deepseek", Model: "deepseek-chat", From: "2025-09-29",
		USDPerMTok: PriceRates{Input: "0.28", CacheRead: "0.28", CacheWrite: "0.28", Output: "0.42"}, Version: "t1/4"}) {
		t.Errorf("a file's row: %+v", r)
	}
	if r := table.Rows[3]; !r.Glob || r.Model != "o*" {
		t.Errorf("a glob: %+v", r)
	}
	if len(table.UnpricedOffers) != 1 || table.UnpricedOffers[0] != (UnpricedOffer{ID: "haiku", Source: SourceConfig, Provider: "anthropic",
		Model: "claude-haiku-4-5", Enabled: true}) {
		t.Errorf("the offers unpriced: %+v", table.UnpricedOffers)
	}

	// A site's row prices the offer the file does not.
	v1 := pricing.SiteVersion(at)
	created := h.admin("POST", "admin/prices", haikuPrice)
	var row PriceRow
	created.decode(t, &row)
	if created.code != 201 || created.header.Get("ETag") != `"1"` || row.Source != PriceSourceSite || row.Version != v1+"/haiku-4-5" ||
		row.USDPerMTok != (PriceRates{Input: "1", CacheRead: "0.1", CacheWrite: "1", Output: "5"}) || row.RowVersion == nil || *row.RowVersion != 1 ||
		row.CreatedBy == nil || *row.CreatedBy != ken || row.From != "2025-10-01" || row.Glob {
		t.Fatalf("created: %d %s", created.code, created.body)
	}
	h.admin("GET", "admin/prices", "").decode(t, &table)
	if *table.Version != "t1+"+v1 || table.SiteVersion == nil || *table.SiteVersion != v1 || table.SiteChangedAt == nil ||
		!table.SiteChangedAt.Equal(at) || len(table.Rows) != 6 || table.Rows[0].ID != "haiku-4-5" || len(table.UnpricedOffers) != 0 {
		t.Fatalf("the table with the site's row: %+v", table)
	}
	var plan SchoolPlan
	h.admin("GET", "admin/school-plan", "").decode(t, &plan)
	if !plan.Offers[1].Priced {
		t.Errorf("the offer is priced now: %+v", plan.Offers[1])
	}
	var m Models
	h.call("GET", "models", h.yuki, "").decode(t, &m)
	if !m.SchoolKey.Offers[1].Priced {
		t.Errorf("GET /models: %+v", m.SchoolKey.Offers)
	}
	cfg := h.build(fh)
	if p, ok := cfg.Runtime.Site.PriceTable(fh.Prices()).Lookup("anthropic", "claude-haiku-4-5", at); !ok || p.Version != v1+"/haiku-4-5" ||
		p.In != 1_000_000 || p.CacheRead != 100_000 || p.CacheWrite != 1_000_000 || p.Out != 5_000_000 {
		t.Errorf("the registry's table: %+v %v", p, ok)
	}

	// A site's row stands before the file's of its provider, model and
	// from.
	h.add(time.Minute)
	v2 := pricing.SiteVersion(at.Add(time.Minute))
	if a := h.admin("POST", "admin/prices", `{"id":"ds","provider":"deepseek","model":"deepseek-chat","from":"2025-09-29",`+
		`"usd_per_mtok":{"input":"0.3","output":"0.5"}}`); a.code != 201 {
		t.Fatalf("over the file's: %d %s", a.code, a.body)
	}
	h.admin("GET", "admin/prices", "").decode(t, &table)
	if !table.Rows[6].Overridden || table.Rows[4].Overridden || table.Rows[1].Version != v2+"/haiku-4-5" {
		t.Errorf("the file's row stood before: %+v", table.Rows)
	}
	if p, _ := h.build(fh).Runtime.Site.PriceTable(fh.Prices()).Lookup("deepseek", "deepseek-chat", at); p.Version != v2+"/ds" {
		t.Errorf("the site's row is not taken: %+v", p)
	}

	// PATCH, at the version read: a new version of the site's table, even
	// within the same second.
	patched := h.admin("PATCH", "admin/prices/haiku-4-5", `{"usd_per_mtok":{"output":"4.5"}}`, "If-Match", `"1"`)
	patched.decode(t, &row)
	v3 := pricing.SiteVersion(at.Add(time.Minute + time.Second))
	if patched.code != 200 || patched.header.Get("ETag") != `"2"` || row.Version != v3+"/haiku-4-5" ||
		row.USDPerMTok != (PriceRates{Input: "1", CacheRead: "0.1", CacheWrite: "1", Output: "4.5"}) {
		t.Fatalf("patched: %d %s", patched.code, patched.body)
	}
	e := wantRefused(t, h.admin("PATCH", "admin/prices/haiku-4-5", `{"usd_per_mtok":{"output":"4"}}`, "If-Match", `"1"`), 412,
		CodeVersionMismatch, ReasonVersionMismatch)
	if e.Details["current_version"] != float64(2) {
		t.Errorf("the version now: %+v", e.Details)
	}
	if a := h.admin("PATCH", "admin/prices/haiku-4-5", `{"usd_per_mtok":{"output":4.5}}`); a.code != 200 || a.header.Get("ETag") != `"2"` {
		t.Errorf("a change of nothing: %d %s", a.code, a.body)
	}
	wantRefused(t, h.admin("PATCH", "admin/prices/haiku-4-5", `{"id":"h"}`), 400, CodeInvalidArgument, ReasonUnknownField)
	e = wantRefused(t, h.admin("PATCH", "admin/prices/ds", `{"model":"claude-haiku-4-5","provider":"anthropic","from":"2025-10-01"}`), 409,
		CodeConflict, ReasonPriceExists)
	if e.Details["field"] != "/from" || e.Details["id"] != "haiku-4-5" {
		t.Errorf("onto another row: %+v", e.Details)
	}

	// The file's rows are the operator's.
	wantRefused(t, h.admin("PATCH", "admin/prices/0", `{"usd_per_mtok":{"output":"1"}}`), 403, CodeForbidden, ReasonPriceReadOnly)
	wantRefused(t, h.admin("DELETE", "admin/prices/0", ""), 403, CodeForbidden, ReasonPriceReadOnly)
	wantRefused(t, h.admin("DELETE", "admin/prices/nope", ""), 404, CodeNotFound, ReasonPriceNotFound)
	wantRefused(t, h.admin("GET", "admin/prices/nope", ""), 404, CodeNotFound, ReasonPriceNotFound)
	if a := h.admin("GET", "admin/prices/0", ""); a.code != 200 || a.header.Get("ETag") != "" || !strings.Contains(a.body, `"source":"file"`) {
		t.Errorf("GET a file's row: %d %s", a.code, a.body)
	}

	// DELETE at a version it left, then at its own.
	wantRefused(t, h.admin("DELETE", "admin/prices/ds", "", "If-Match", `"7"`), 412, CodeVersionMismatch, ReasonVersionMismatch)
	deleted := h.admin("DELETE", "admin/prices/ds", "", "If-Match", `"1"`)
	deleted.decode(t, &table)
	if deleted.code != 200 || len(table.Rows) != 6 || table.Rows[5].Overridden {
		t.Errorf("deleted: %d %s", deleted.code, deleted.body)
	}
	for _, c := range []struct {
		action string
		n      int
	}{{"price.create", 2}, {"price.update", 5}, {"price.delete", 4}} {
		if ev := h.events(c.action); len(ev) != c.n || ev[0].TargetType != "site_price" {
			t.Errorf("%s: %+v", c.action, ev)
		}
	}
	if ev := h.events("price.create"); !strings.Contains(string(ev[0].Detail), `"usd_per_mtok":{"cache_read":"0.1","cache_write":"1","input":"1","output":"5"}`) {
		t.Errorf("the audit's detail: %s", ev[0].Detail)
	}
}

// POST /admin/prices refuses a row that is not one, at its pointer, as the
// price file's are refused, and an id or a provider, model and from the
// site's rows have.
func TestPricesRefused(t *testing.T) {
	h, _, _ := newModelWorld(t, schoolRuntime())
	if a := h.admin("POST", "admin/prices", haikuPrice); a.code != 201 {
		t.Fatalf("%d %s", a.code, a.body)
	}
	body := func(from, to string) string { return strings.Replace(haikuPrice, from, to, 1) }
	for _, tc := range []struct {
		name, body          string
		status              int
		reason, field, code string
	}{
		{"no id", body(`"id":"haiku-4-5",`, ``), 400, ReasonMissingField, "/id", CodeInvalidArgument},
		{"an id of spaces", body(`"haiku-4-5"`, `"haiku 4"`), 400, ReasonInvalidField, "/id", CodeInvalidArgument},
		{"an id of dots", body(`"haiku-4-5"`, `".."`), 400, ReasonInvalidField, "/id", CodeInvalidArgument},
		{"no provider", body(`"provider":"anthropic",`, ``), 400, ReasonMissingField, "/provider", CodeInvalidArgument},
		{"a provider in capitals", body(`"anthropic"`, `"Anthropic"`), 400, ReasonInvalidField, "/provider", CodeInvalidArgument},
		{"no model", body(`"model":"claude-haiku-4-5",`, ``), 400, ReasonMissingField, "/model", CodeInvalidArgument},
		{"a model with a space", body(`claude-haiku-4-5`, `claude haiku`), 400, ReasonInvalidField, "/model", CodeInvalidArgument},
		{"a model too long", body(`claude-haiku-4-5`, strings.Repeat("m", 201)), 400, ReasonInvalidField, "/model", CodeInvalidArgument},
		{"no from", body(`"from":"2025-10-01",`, ``), 400, ReasonMissingField, "/from", CodeInvalidArgument},
		{"a from of no day", body(`2025-10-01`, `2025-13-01`), 400, ReasonInvalidField, "/from", CodeInvalidArgument},
		{"a from with a time", body(`2025-10-01`, `2025-10-01T00:00:00Z`), 400, ReasonInvalidField, "/from", CodeInvalidArgument},
		{"no prices", body(`,"usd_per_mtok":{"input":1,"output":"5","cache_read":"0.1"}`, ``), 400, ReasonMissingField, "/usd_per_mtok",
			CodeInvalidArgument},
		{"no input", body(`"input":1,`, ``), 400, ReasonMissingField, "/usd_per_mtok/input", CodeInvalidArgument},
		{"no output", body(`,"output":"5"`, ``), 400, ReasonMissingField, "/usd_per_mtok/output", CodeInvalidArgument},
		{"a price below zero", body(`"input":1`, `"input":-1`), 400, ReasonInvalidField, "/usd_per_mtok/input", CodeInvalidArgument},
		{"a price past a pico-dollar", body(`"0.1"`, `"0.0000001"`), 400, ReasonInvalidField, "/usd_per_mtok/cache_read", CodeInvalidArgument},
		{"a price of words", body(`"5"`, `"five"`), 400, ReasonInvalidField, "/usd_per_mtok/output", CodeInvalidArgument},
		{"a price of null", body(`"5"`, `null`), 400, ReasonInvalidField, "/usd_per_mtok/output", CodeInvalidArgument},
		{"a price of another name", body(`"cache_read"`, `"cached"`), 400, ReasonUnknownField, "/usd_per_mtok/cached", CodeInvalidArgument},
		{"the id again", body(`"from":"2025-10-01"`, `"from":"2026-01-01"`), 409, ReasonPriceExists, "/id", CodeConflict},
		{"the model and day again", body(`"haiku-4-5"`, `"haiku-again"`), 409, ReasonPriceExists, "/from", CodeConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := wantRefused(t, h.admin("POST", "admin/prices", tc.body), tc.status, tc.code, tc.reason)
			if e.Details["field"] != tc.field {
				t.Errorf("field %v, want %s", e.Details["field"], tc.field)
			}
		})
	}
	if rows, _, _ := h.st.SitePrices(context.Background()); len(rows) != 1 {
		t.Errorf("a refused row was kept: %+v", rows)
	}
	// A price may be exactly the file's syntax: an exponent, a number.
	if a := h.admin("POST", "admin/prices", `{"id":"o3","provider":"openai","model":"o3","from":"2025-04-16","usd_per_mtok":{"input":2e0,"output":"8"}}`); a.code != 201 ||
		!strings.Contains(a.body, `"input":"2"`) {
		t.Errorf("an exponent: %d %s", a.code, a.body)
	}
}

// The money's routes refuse anyone but the runtime's administrators
// (not_admin), audited for the writes, and change nothing.
func TestMoneyAdminOnly(t *testing.T) {
	h, _, _ := newModelWorld(t, schoolRuntime())
	for _, c := range []struct{ method, path, body string }{
		{"GET", "admin/prices", ""}, {"POST", "admin/prices", haikuPrice}, {"GET", "admin/prices/0", ""},
		{"PATCH", "admin/prices/0", `{"usd_per_mtok":{"input":"1"}}`}, {"DELETE", "admin/prices/0", ""},
		{"GET", "admin/tenants", ""}, {"GET", "admin/tenants/ten_x", ""}, {"PUT", "admin/tenants/ten_x", `{"per_day":{"answers":1,"usd":null}}`},
		{"DELETE", "admin/tenants/ten_x", ""},
		{"GET", "admin/agent-budgets", ""}, {"PUT", "admin/agent-budgets", `{"per_agent_day":{"answers":1,"usd":null},"per_asker_day":{"answers":1,"usd":null}}`},
		{"DELETE", "admin/agent-budgets", ""}, {"GET", "admin/costs", ""},
	} {
		for _, role := range []string{"", "instructor"} {
			wantRefused(t, h.adminCall(t, role, c.method, c.path, c.body), 403, CodeForbidden, ReasonNotAdmin)
		}
	}
	for _, action := range []string{"price.create", "price.update", "price.delete", "tenant_quota.update", "tenant_quota.reset",
		"agent_budgets.update", "agent_budgets.reset"} {
		if ev := h.events(action); len(ev) != 2 || ev[0].Outcome != ReasonNotAdmin {
			t.Errorf("%s: %+v", action, ev)
		}
	}
	ctx := context.Background()
	rows, _, _ := h.st.SitePrices(ctx)
	quotas, _ := h.st.TenantQuotas(ctx)
	settings, _ := h.st.SiteSettings(ctx)
	if len(rows)+len(quotas)+len(settings) != 0 {
		t.Errorf("a refused write was kept: %+v %+v %+v", rows, quotas, settings)
	}
}

// The plan's quotas in dollars: refused while an offer of the plan has no
// price today (offer_not_priced, naming it), and taken once a site's row
// prices it, which the registry then puts in force; left out, a quota in
// dollars stays as it is in force, and null is none. A price the quota
// needs cannot then be deleted, or moved off its model.
func TestSchoolPlanDollarQuotas(t *testing.T) {
	h, _, fh := newModelWorld(t, schoolRuntime())
	e := wantRefused(t, h.admin("PUT", "admin/school-plan/quotas", `{"per_owner_day":5,"per_asker_day":2,"per_day":null,"per_day_usd":"2.5"}`), 422,
		CodeFailedPrecondition, ReasonOfferNotPriced)
	if e.Details["field"] != "/per_day_usd" || fmt.Sprint(e.Details["offers"]) != "[haiku]" {
		t.Errorf("the refusal: %+v", e.Details)
	}
	if set, _ := h.s.siteSetting(context.Background(), store.SettingSchoolQuotas); set != nil {
		t.Fatalf("a refused quota was kept: %s", set.Value)
	}
	if a := h.admin("POST", "admin/prices", haikuPrice); a.code != 201 {
		t.Fatalf("%d %s", a.code, a.body)
	}
	var plan SchoolPlan
	put := h.admin("PUT", "admin/school-plan/quotas", `{"per_owner_day":5,"per_asker_day":2,"per_day":null,"per_owner_day_usd":0.75,"per_day_usd":"2.5"}`)
	put.decode(t, &plan)
	if put.code != 200 || plan.Quotas.PerDayUSD == nil || *plan.Quotas.PerDayUSD != "2.500000" || plan.Quotas.PerOwnerDayUSD == nil ||
		*plan.Quotas.PerOwnerDayUSD != "0.750000" || plan.Quotas.PerAskerDayUSD != nil || plan.QuotaDefaults.PerDayUSD != nil {
		t.Fatalf("%d %s", put.code, put.body)
	}
	sc := h.build(fh).Runtime.School
	if sc.PerDay.USD == nil || *sc.PerDay.USD != 2.5 || sc.PerOwnerDay.USD == nil || *sc.PerOwnerDay.USD != 0.75 || sc.PerAskerDay.USD != nil {
		t.Errorf("the plan in force: %+v", sc)
	}
	var usage SchoolPlanUsage
	h.admin("GET", "admin/school-plan/usage", "").decode(t, &usage)
	if usage.Limits.PerDayUSD == nil || *usage.Limits.PerDayUSD != "2.500000" {
		t.Errorf("the usage's limits: %+v", usage.Limits)
	}
	if ev := h.events("school_quotas.update"); len(ev) != 2 || ev[0].Outcome != ReasonOfferNotPriced ||
		!strings.Contains(string(ev[1].Detail), `"per_day_usd":"2.500000"`) {
		t.Errorf("the audit: %+v", ev)
	}

	// Left out, as in force.
	h.admin("PUT", "admin/school-plan/quotas", `{"per_owner_day":6,"per_asker_day":2,"per_day":null}`).decode(t, &plan)
	if plan.Quotas.PerOwnerDay != 6 || plan.Quotas.PerDayUSD == nil || *plan.Quotas.PerDayUSD != "2.500000" {
		t.Errorf("left out: %+v", plan.Quotas)
	}

	// The price the quota needs.
	e = wantRefused(t, h.admin("DELETE", "admin/prices/haiku-4-5", ""), 422, CodeFailedPrecondition, ReasonOfferNotPriced)
	if fmt.Sprint(e.Details["offers"]) != "[haiku]" {
		t.Errorf("deleting the price: %+v", e.Details)
	}
	wantRefused(t, h.admin("PATCH", "admin/prices/haiku-4-5", `{"model":"claude-haiku-9"}`), 422, CodeFailedPrecondition, ReasonOfferNotPriced)
	wantRefused(t, h.admin("PATCH", "admin/prices/haiku-4-5", `{"from":"2027-01-01"}`), 422, CodeFailedPrecondition, ReasonOfferNotPriced)
	if a := h.admin("PATCH", "admin/prices/haiku-4-5", `{"usd_per_mtok":{"input":"0.8"}}`); a.code != 200 {
		t.Errorf("a price changed: %d %s", a.code, a.body)
	}

	// Null is none; then the price may go.
	h.admin("PUT", "admin/school-plan/quotas", `{"per_owner_day":6,"per_asker_day":2,"per_day":null,"per_owner_day_usd":null,"per_day_usd":null}`).
		decode(t, &plan)
	if plan.Quotas.PerDayUSD != nil || plan.Quotas.PerOwnerDayUSD != nil {
		t.Errorf("null: %+v", plan.Quotas)
	}
	if a := h.admin("DELETE", "admin/prices/haiku-4-5", ""); a.code != 200 {
		t.Errorf("the price no quota needs: %d %s", a.code, a.body)
	}
}

// An agent with a quota in dollars whose model no price holds is not run:
// the site's budgets in dollars are refused while a hosted agent's model
// has no price (model_not_priced, saying which), and taken once it has;
// the owner may not then move it to a model no price holds.
func TestAgentBudgets(t *testing.T) {
	rt := schoolRuntime()
	rt.Defaults = map[string]any{"budgets": map[string]any{"per_asker_day": map[string]any{"answers": 20}}}
	yamlAgent := &config.Agent{ID: "agt_yaml", TenantID: "t_ops"}
	h, _, fh := newModelWorld(t, rt)
	fh.yaml.Agents = []*config.Agent{yamlAgent}
	v := h.host(h.yuki, h.helper.ID)
	nano := `{"model":{"own":{"provider":"openai","model":"gpt-4.1-nano"}},"own_key":{"value":"` + ownKey + `"}}`
	if a := h.patch(v.ID, `"1"`, nano); a.code != 200 {
		t.Fatalf("an unpriced model: %d %s", a.code, a.body)
	}
	var b AgentBudgets
	a := h.admin("GET", "admin/agent-budgets", "")
	wantSecured(t, a, "no-store")
	a.decode(t, &b)
	twenty := 20
	if a.code != 200 || b.Set || b.PerAgentDay != (DailyQuota{}) || b.PerAskerDay.Answers == nil || *b.PerAskerDay.Answers != twenty ||
		b.Defaults.PerAskerDay.Answers == nil || b.UpdatedAt != nil {
		t.Fatalf("%d %s", a.code, a.body)
	}

	budgets := `{"per_agent_day":{"answers":200,"usd":"1.5"},"per_asker_day":{"answers":null,"usd":null}}`
	e := wantRefused(t, h.admin("PUT", "admin/agent-budgets", budgets), 422, CodeFailedPrecondition, ReasonModelNotPriced)
	if e.Details["field"] != "/per_agent_day/usd" || !strings.Contains(fmt.Sprint(e.Details["problems"]), v.ID) ||
		!strings.Contains(fmt.Sprint(e.Details["problems"]), "gpt-4.1-nano") {
		t.Errorf("the refusal: %+v", e.Details)
	}
	put := h.admin("PUT", "admin/agent-budgets", `{"per_agent_day":{"answers":200,"usd":null},"per_asker_day":{"answers":null,"usd":null}}`)
	put.decode(t, &b)
	if put.code != 200 || !b.Set || b.PerAgentDay.Answers == nil || *b.PerAgentDay.Answers != 200 || b.PerAskerDay != (DailyQuota{}) ||
		b.Defaults.PerAskerDay.Answers == nil || *b.Defaults.PerAskerDay.Answers != 20 || b.UpdatedBy == nil || *b.UpdatedBy != ken {
		t.Fatalf("answers alone: %d %s", put.code, put.body)
	}
	cfg := h.build(fh)
	for _, ag := range cfg.Agents {
		switch ag.ID {
		case v.ID:
			if bu := ag.Budgets; bu.PerAgentDay.Answers == nil || *bu.PerAgentDay.Answers != 200 || bu.PerAskerDay.Answers != nil {
				t.Errorf("the hosted agent's budgets: %+v", bu)
			}
		case yamlAgent.ID:
			if ag.Budgets.PerAgentDay.Answers != nil {
				t.Errorf("runtime.yaml's agent took the site's budgets: %+v", ag.Budgets)
			}
		}
	}

	// On a priced model, the budget in dollars is taken; the owner may
	// not then choose a model no price holds.
	if a := h.patch(v.ID, `"2"`, ownOpenAI); a.code != 200 {
		t.Fatalf("a priced model: %d %s", a.code, a.body)
	}
	h.admin("PUT", "admin/agent-budgets", budgets).decode(t, &b)
	if b.PerAgentDay.USD == nil || *b.PerAgentDay.USD != "1.500000" {
		t.Fatalf("in dollars: %+v", b)
	}
	cfg = h.build(fh)
	if len(cfg.Agents) != 2 || len(config.AgentsUSDWithoutPrices(cfg, cfg.Runtime.Site.PriceTable(fh.Prices()), at)) != 0 {
		t.Errorf("the build: %+v %+v", cfg.Agents, cfg.Rejected)
	}
	e = wantRefused(t, h.patch(v.ID, `"3"`, `{"model":{"own":{"provider":"openai","model":"gpt-4.1-nano"}}}`), 422, CodeFailedPrecondition,
		ReasonSettingsRejected)
	if !strings.Contains(fmt.Sprint(e.Details["problems"]), "quota in dollars") {
		t.Errorf("the owner's refusal: %+v", e.Details)
	}

	for _, tc := range []struct{ body, reason, field string }{
		{`{"per_agent_day":{"answers":1,"usd":null}}`, ReasonMissingField, "/per_asker_day"},
		{`{"per_agent_day":{"answers":1},"per_asker_day":{"answers":null,"usd":null}}`, ReasonMissingField, "/per_agent_day/usd"},
		{`{"per_agent_day":{"answers":0,"usd":null},"per_asker_day":{"answers":null,"usd":null}}`, ReasonInvalidField, "/per_agent_day/answers"},
		{`{"per_agent_day":{"answers":1,"usd":"0"},"per_asker_day":{"answers":null,"usd":null}}`, ReasonInvalidField, "/per_agent_day/usd"},
		{`{"per_agent_day":7,"per_asker_day":{"answers":null,"usd":null}}`, ReasonInvalidField, "/per_agent_day"},
		{`{"per_agent_day":{"answers":1,"usd":null,"tokens":3},"per_asker_day":{"answers":null,"usd":null}}`, ReasonUnknownField,
			"/per_agent_day/tokens"},
	} {
		e := wantRefused(t, h.admin("PUT", "admin/agent-budgets", tc.body), 400, CodeInvalidArgument, tc.reason)
		if e.Details["field"] != tc.field {
			t.Errorf("%s: field %v", tc.body, e.Details["field"])
		}
	}

	reset := h.admin("DELETE", "admin/agent-budgets", "")
	reset.decode(t, &b)
	if reset.code != 200 || b.Set || b.PerAgentDay != (DailyQuota{}) || b.PerAskerDay.Answers == nil || *b.PerAskerDay.Answers != 20 {
		t.Errorf("reset: %d %s", reset.code, reset.body)
	}
	if a := h.admin("DELETE", "admin/agent-budgets", ""); a.code != 200 {
		t.Errorf("reset again: %d %s", a.code, a.body)
	}
	if up, re := h.events("agent_budgets.update"), h.events("agent_budgets.reset"); len(up) != 9 || len(re) != 1 {
		t.Errorf("the audit: %d updates, %d resets", len(up), len(re))
	}
}

// The tenants' quotas: GET lists runtime.yaml's, the site's and the
// agents' tenants, a person's with their name, a page at a time; PUT sets
// the site's in place of runtime.yaml's, which the registry puts in force,
// refused in dollars where an agent of the tenant's on the school's key
// has no price; DELETE takes runtime.yaml's again.
func TestTenantQuotas(t *testing.T) {
	rt := schoolRuntime()
	hundred := 100
	rt.Tenants = map[string]config.Tenant{"t_ops": {PerDay: config.Quota{Answers: &hundred}}}
	h, _, fh := newModelWorld(t, rt)
	h.call("GET", "me", h.yuki, "")
	v := h.host(h.yuki, h.helper.ID)
	if a := h.patch(v.ID, `"1"`, `{"model":{"school":{"offer":"haiku"}}}`); a.code != 200 {
		t.Fatalf("on the plan: %d %s", a.code, a.body)
	}
	yukis := "ten_" + h.yuki.ID
	var list TenantList
	a := h.admin("GET", "admin/tenants", "")
	wantSecured(t, a, "no-store")
	a.decode(t, &list)
	if a.code != 200 || len(list.Tenants) != 2 || list.Next != nil {
		t.Fatalf("%d %s", a.code, a.body)
	}
	ops, yuki := list.Tenants[0], list.Tenants[1]
	if ops.TenantID != "t_ops" || ops.Source != SourceConfig || ops.PerDay.Answers == nil || *ops.PerDay.Answers != 100 || ops.PerDay.USD != nil ||
		ops.ConfigPerDay == nil || ops.OwnerActorID != nil || ops.Agents != 0 {
		t.Errorf("runtime.yaml's: %+v", ops)
	}
	if yuki.TenantID != yukis || yuki.Source != TenantSourceNone || yuki.PerDay != (DailyQuota{}) || yuki.ConfigPerDay != nil ||
		yuki.OwnerActorID == nil || *yuki.OwnerActorID != h.yuki.ID || yuki.DisplayName == nil || *yuki.DisplayName != "Yuki" || yuki.Agents != 1 {
		t.Errorf("Yuki's: %+v", yuki)
	}

	var q TenantQuota
	put := h.admin("PUT", "admin/tenants/t_ops", `{"per_day":{"answers":50,"usd":"1.5"}}`)
	put.decode(t, &q)
	if put.code != 200 || q.Source != SourceSite || *q.PerDay.Answers != 50 || q.PerDay.USD == nil || *q.PerDay.USD != "1.500000" ||
		q.ConfigPerDay == nil || *q.ConfigPerDay.Answers != 100 || q.UpdatedBy == nil || *q.UpdatedBy != ken {
		t.Fatalf("put: %d %s", put.code, put.body)
	}
	if tq := h.build(fh).Runtime.Tenants["t_ops"]; *tq.PerDay.Answers != 50 || tq.PerDay.USD == nil || *tq.PerDay.USD != 1.5 {
		t.Errorf("the tenant in force: %+v", tq)
	}
	// Yuki's agent is on an offer no price holds.
	e := wantRefused(t, h.admin("PUT", "admin/tenants/"+yukis, `{"per_day":{"answers":null,"usd":"0.5"}}`), 422, CodeFailedPrecondition,
		ReasonModelNotPriced)
	if e.Details["field"] != "/per_day/usd" || !strings.Contains(fmt.Sprint(e.Details["problems"]), "claude-haiku-4-5") {
		t.Errorf("the refusal: %+v", e.Details)
	}
	if a := h.admin("PUT", "admin/tenants/"+yukis, `{"per_day":{"answers":10,"usd":null}}`); a.code != 200 || !strings.Contains(a.body, `"source":"site"`) {
		t.Errorf("in answers: %d %s", a.code, a.body)
	}
	for _, tc := range []struct{ path, body, reason, field string }{
		{"admin/tenants/t_ops", `{"per_day":{"answers":null,"usd":null}}`, ReasonInvalidField, "/per_day"},
		{"admin/tenants/t_ops", `{}`, ReasonMissingField, "/per_day"},
		{"admin/tenants/t_ops", `{"per_day":{"answers":5}}`, ReasonMissingField, "/per_day/usd"},
		{"admin/tenants/t_ops", `{"per_day":{"answers":5,"usd":"-2"}}`, ReasonInvalidField, "/per_day/usd"},
		{"admin/tenants/t.ops", `{"per_day":{"answers":5,"usd":null}}`, ReasonInvalidField, "tenant_id"},
	} {
		e := wantRefused(t, h.admin("PUT", tc.path, tc.body), 400, CodeInvalidArgument, tc.reason)
		if e.Details["field"] != tc.field {
			t.Errorf("%s %s: field %v", tc.path, tc.body, e.Details["field"])
		}
	}

	// A page at a time.
	h.admin("GET", "admin/tenants?limit=1", "").decode(t, &list)
	if len(list.Tenants) != 1 || list.Next == nil || *list.Next != "t_ops" {
		t.Fatalf("the first page: %+v", list)
	}
	h.admin("GET", "admin/tenants?limit=1&after="+*list.Next, "").decode(t, &list)
	if len(list.Tenants) != 1 || list.Tenants[0].TenantID != yukis || list.Next != nil {
		t.Errorf("the last page: %+v", list)
	}
	for q, field := range map[string]string{"limit=0": "limit", "limit=501": "limit", "limit=x": "limit", "limit=1&limit=2": "limit",
		"offset=1": "offset"} {
		a := h.admin("GET", "admin/tenants?"+q, "")
		if a.code != 400 || a.refusal(t).Details["field"] != field {
			t.Errorf("?%s: %d %s", q, a.code, a.body)
		}
	}
	if a := h.admin("GET", "admin/tenants/nobody_yet", ""); a.code != 200 || !strings.Contains(a.body, `"source":"none"`) {
		t.Errorf("a tenant of no one: %d %s", a.code, a.body)
	}

	reset := h.admin("DELETE", "admin/tenants/t_ops", "")
	reset.decode(t, &q)
	if reset.code != 200 || q.Source != SourceConfig || *q.PerDay.Answers != 100 || q.PerDay.USD != nil || q.UpdatedAt != nil {
		t.Errorf("reset: %d %s", reset.code, reset.body)
	}
	if a := h.admin("DELETE", "admin/tenants/t_ops", ""); a.code != 200 {
		t.Errorf("reset again: %d %s", a.code, a.body)
	}
	if tq := h.build(fh).Runtime.Tenants["t_ops"]; *tq.PerDay.Answers != 100 || tq.PerDay.USD != nil {
		t.Errorf("runtime.yaml's again: %+v", tq)
	}
	up, re := h.events("tenant_quota.update"), h.events("tenant_quota.reset")
	if len(up) != 8 || len(re) != 1 || up[0].TargetType != "tenant" || up[0].TargetID != "t_ops" ||
		string(up[0].Detail) != `{"answers":50,"usd":"1.500000"}` {
		t.Errorf("the audit: %+v %+v", up, re)
	}
}

// GET /admin/costs sums the ledger's model calls over a span of UTC days,
// as a whole and by day, tenant (a person's with their name), agent,
// model (with the plan's offers of it on the school's key) or key source,
// on one key source or both, a page at a time; each sum in lines by kind
// of cost. A span it cannot take is refused, naming the parameter.
func TestCostReport(t *testing.T) {
	h, _, _ := newModelWorld(t, schoolRuntime())
	ctx := context.Background()
	h.call("GET", "me", h.yuki, "")
	yukis := "ten_" + h.yuki.ID
	n := 0
	rec := func(when time.Time, tenant, agent, key, provider, model string, cost int64, version string) {
		n++
		id := fmt.Sprintf("c%d", n)
		h.ok(h.st.RecordLLMCall(ctx, store.LLMCall{ID: id, At: when, TenantID: tenant, AgentID: agent, CourseID: "c", ConversationID: "x",
			MessageID: id, Provider: provider, Model: model, Input: 1000, CacheRead: 400, Output: 100, CostPUSD: cost, PriceVersion: version,
			KeySource: key}))
	}
	day := store.UTCDay(at)
	rec(day.Add(time.Hour), yukis, "agt_y", config.KeySchool, "deepseek", "deepseek-chat", 2_000_000, "t1/4")
	rec(day.Add(2*time.Hour), yukis, "agt_y", config.KeyOwn, "openai", "gpt-4.1-mini", 1_000_000, "t1/0")
	rec(day.AddDate(0, 0, -1), "t_ops", "agt_ops", config.KeySchool, "deepseek", "deepseek-chat", 500_000, "site-20260927T000000Z/ds")
	rec(day.AddDate(0, 0, -1), "t_ops", "agt_ops", config.KeySchool, "anthropic", "claude-haiku-4-5", 0, "")
	rec(day.AddDate(0, 0, -40), "t_ops", "agt_ops", config.KeySchool, "deepseek", "deepseek-chat", 9_000_000, "t1/4")

	var r CostReport
	a := h.admin("GET", "admin/costs", "")
	wantSecured(t, a, "no-store")
	a.decode(t, &r)
	if a.code != 200 || r.Since != "2026-08-30" || r.Until != "2026-09-28" || r.Group != store.CostByDay || r.KeySource != nil || r.Next != nil ||
		r.Total.CostUSD != "0.000004" || len(r.Total.Lines) != 1 || len(r.Rows) != 2 {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if l := r.Total.Lines[0]; l.Kind != CostKindModelCalls || l.Calls != 4 || l.UnpricedCalls != 1 || l.Tokens == nil ||
		*l.Tokens != (CostTokens{Input: 4000, CacheRead: 1600, Output: 400}) || l.CostUSD != "0.000004" {
		t.Errorf("the total's line: %+v", l)
	}
	if r.Rows[0].Day == nil || *r.Rows[0].Day != "2026-09-27" || r.Rows[1].Key != "2026-09-28" || r.Rows[1].CostUSD != "0.000003" {
		t.Errorf("by day: %+v", r.Rows)
	}

	h.admin("GET", "admin/costs?group=tenant", "").decode(t, &r)
	if len(r.Rows) != 2 || r.Rows[1].TenantID == nil || *r.Rows[1].TenantID != yukis || r.Rows[1].DisplayName == nil || *r.Rows[1].DisplayName != "Yuki" ||
		r.Rows[0].OwnerActorID != nil {
		t.Errorf("by tenant: %+v", r.Rows)
	}
	h.admin("GET", "admin/costs?group=model&key_source=school", "").decode(t, &r)
	if len(r.Rows) != 2 || r.Rows[0].Key != "school/anthropic/claude-haiku-4-5" || !slices.Equal(r.Rows[0].Offers, []string{"haiku"}) ||
		r.Rows[1].Provider == nil || *r.Rows[1].Model != "deepseek-chat" || !slices.Equal(r.Rows[1].Offers, []string{"standard"}) ||
		r.KeySource == nil || *r.KeySource != "school" || r.Total.Lines[0].Calls != 3 {
		t.Errorf("by model on the school's key: %+v", r)
	}
	h.admin("GET", "admin/costs?group=key_source&since=2026-07-01&until=2026-09-28", "").decode(t, &r)
	if len(r.Rows) != 2 || r.Rows[0].Key != "own" || r.Rows[1].CostUSD != "0.000012" {
		t.Errorf("by key source over a longer span: %+v", r.Rows)
	}
	h.admin("GET", "admin/costs?group=agent&limit=1", "").decode(t, &r)
	if len(r.Rows) != 1 || r.Next == nil || *r.Next != "agt_ops" || r.Rows[0].TenantID == nil || *r.Rows[0].TenantID != "t_ops" {
		t.Fatalf("by agent, the first page: %+v", r)
	}
	h.admin("GET", "admin/costs?group=agent&limit=1&after=agt_ops", "").decode(t, &r)
	if len(r.Rows) != 1 || r.Rows[0].AgentID == nil || *r.Rows[0].AgentID != "agt_y" || r.Next != nil || r.Total.Lines[0].Calls != 4 {
		t.Errorf("by agent, the last page: %+v", r)
	}
	h.admin("GET", "admin/costs?group=total&since=2026-09-28", "").decode(t, &r)
	if len(r.Rows) != 1 || r.Rows[0].Key != "" || r.Rows[0].Lines[0].Calls != 2 {
		t.Errorf("the total of a day: %+v", r.Rows)
	}
	for q, field := range map[string]string{
		"since=2026-09-29&until=2026-09-28": "since", "since=2025-01-01": "since", "until=yesterday": "until", "since=28-09-2026": "since",
		"group=course": "group", "key_source=operator": "key_source", "limit=0": "limit", "limit=501": "limit",
		"after=" + strings.Repeat("a", 301): "after", "page=2": "page",
	} {
		a := h.admin("GET", "admin/costs?"+q, "")
		if a.code != 400 || a.refusal(t).Details["field"] != field {
			t.Errorf("?%s: %d %s", q, a.code, a.body)
		}
	}
}
