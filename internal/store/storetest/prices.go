package storetest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// price is a row of the site's price table as the API would create it.
func price(id, provider, model string, from time.Time) store.SitePrice {
	return store.SitePrice{ID: id, Provider: provider, Model: model, From: from, InputPUSD: 400_000, CacheReadPUSD: 100_000,
		CacheWritePUSD: 400_000, OutputPUSD: 1_600_000, CreatedBy: "admin-1", CreatedAt: at(3 * time.Hour)}
}

func sitePrices(t *testing.T, s store.Store) ([]store.SitePrice, time.Time) {
	t.Helper()
	ps, changed, err := s.SitePrices(t.Context())
	if err != nil {
		t.Fatalf("SitePrices: %v", err)
	}
	return ps, changed
}

func priceIDs(ps []store.SitePrice) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func testSitePrices(t *testing.T, open Opener) {
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	t.Run("rows created, read back by id, updated at the version named, and deleted", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		p := price("mini", "openai", "gpt-4.1-mini", day.Add(13*time.Hour))
		got, err := s.CreateSitePrice(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if !got.From.Equal(day) || got.Version != 1 || got.UpdatedBy != "admin-1" || got.InputPUSD != 400_000 || got.CacheReadPUSD != 100_000 {
			t.Errorf("created: %+v", got)
		}
		sameTime(t, "created_at", got.CreatedAt, p.CreatedAt)
		if _, err := s.CreateSitePrice(ctx, price("glob", "openai", "gpt-5*", day)); err != nil {
			t.Fatal(err)
		}
		ps, _ := sitePrices(t, s)
		if !slices.Equal(priceIDs(ps), []string{"glob", "mini"}) || !ps[1].From.Equal(day) || ps[1].OutputPUSD != 1_600_000 {
			t.Fatalf("SitePrices = %+v", ps)
		}
		for name, bad := range map[string]store.SitePrice{
			"an id taken":                      price("mini", "openai", "gpt-4.1", day),
			"the same provider, model and day": price("mini2", "openai", "gpt-4.1-mini", day),
		} {
			if _, err := s.CreateSitePrice(ctx, bad); !errors.Is(err, store.ErrExists) {
				t.Errorf("%s: %v, want ErrExists", name, err)
			}
		}
		for name, bad := range map[string]store.SitePrice{
			"no id":            price("", "openai", "m", day),
			"an id of spaces":  price("a b", "openai", "m", day),
			"a provider Upper": price("x", "OpenAI", "m", day),
			"no model":         price("x", "openai", " ", day),
			"no day":           price("x", "openai", "m", time.Time{}),
			"a negative price": func() store.SitePrice { p := price("x", "openai", "m", day); p.OutputPUSD = -1; return p }(),
		} {
			if _, err := s.CreateSitePrice(ctx, bad); err == nil {
				t.Errorf("%s: created", name)
			}
		}

		u := *got
		u.InputPUSD, u.UpdatedBy = 300_000, "admin-2"
		before := time.Now()
		up, err := s.UpdateSitePrice(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if up.Version != 2 || up.InputPUSD != 300_000 || up.UpdatedBy != "admin-2" || up.CreatedBy != "admin-1" {
			t.Errorf("updated: %+v", up)
		}
		recent(t, "updated_at", up.UpdatedAt, before, time.Now())
		if _, err := s.UpdateSitePrice(ctx, u); !errors.Is(err, store.ErrConflict) {
			t.Errorf("at a version it has left: %v, want ErrConflict", err)
		}
		moved := *up
		moved.Model, moved.Version = "gpt-5*", 0
		if _, err := s.UpdateSitePrice(ctx, moved); !errors.Is(err, store.ErrExists) {
			t.Errorf("onto another row's provider, model and day: %v, want ErrExists", err)
		}
		gone := *up
		gone.ID = "none"
		if _, err := s.UpdateSitePrice(ctx, gone); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a row not there: %v", err)
		}
		if err := s.DeleteSitePrice(ctx, "mini", 1); !errors.Is(err, store.ErrConflict) {
			t.Errorf("a delete at a version it has left: %v", err)
		}
		if err := s.DeleteSitePrice(ctx, "mini", 2); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteSitePrice(ctx, "mini", 0); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleted again: %v", err)
		}
		if ps, _ := sitePrices(t, s); !slices.Equal(priceIDs(ps), []string{"glob"}) {
			t.Errorf("after the delete: %v", priceIDs(ps))
		}
	})

	t.Run("when the prices last changed moves on with each write, a second at least, and the revision with it", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		_, last := sitePrices(t, s)
		r := rev(t, s)
		step := func(what string, write func() error) {
			t.Helper()
			if err := write(); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			_, now := sitePrices(t, s)
			if now.Sub(last) < time.Second || now.Truncate(time.Second) != now {
				t.Errorf("%s: changed at %s, was %s", what, now, last)
			}
			if got := rev(t, s); got <= r {
				t.Errorf("%s: the revision did not move on", what)
			}
			last, r = now, rev(t, s)
		}
		var p *store.SitePrice
		step("created", func() (err error) { p, err = s.CreateSitePrice(ctx, price("a", "openai", "o3", day)); return err })
		step("updated", func() error { _, err := s.UpdateSitePrice(ctx, *p); return err })
		step("created again", func() error { _, err := s.CreateSitePrice(ctx, price("b", "openai", "o4", day)); return err })
		step("deleted", func() error { return s.DeleteSitePrice(ctx, "a", 0) })
		if _, err := s.CreateSitePrice(ctx, price("b", "openai", "o5", day)); err == nil {
			t.Fatal("an id taken was created")
		}
		if err := s.DeleteSitePrice(ctx, "a", 0); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		if _, now := sitePrices(t, s); !now.Equal(last) || rev(t, s) != r {
			t.Errorf("refused writes moved it on: %s, was %s", now, last)
		}
	})

	t.Run("tenants' quotas set, replaced, listed and unset", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		ten, fifty := 10, int64(2_500_000_000_000)
		r := rev(t, s)
		for _, q := range []store.TenantQuota{
			{TenantID: "ten_b", Answers: &ten, UpdatedBy: "admin-1", UpdatedAt: at(time.Minute)},
			{TenantID: "instr_42", USDPUSD: &fifty, UpdatedBy: "admin-1"},
			{TenantID: "ten_b", Answers: &ten, USDPUSD: &fifty, UpdatedBy: "admin-2", UpdatedAt: at(time.Hour)},
			{TenantID: "ten_none", UpdatedBy: "admin-1"},
		} {
			if err := s.PutTenantQuota(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if got := rev(t, s); got <= r {
			t.Error("the revision did not move on")
		}
		qs, err := s.TenantQuotas(ctx)
		if err != nil || len(qs) != 3 || qs[0].TenantID != "instr_42" || qs[1].TenantID != "ten_b" || qs[2].TenantID != "ten_none" {
			t.Fatalf("TenantQuotas = %+v, %v", qs, err)
		}
		if b := qs[1]; b.Answers == nil || *b.Answers != 10 || b.USDPUSD == nil || *b.USDPUSD != fifty || b.UpdatedBy != "admin-2" {
			t.Errorf("replaced: %+v", b)
		}
		sameTime(t, "updated_at", qs[1].UpdatedAt, at(time.Hour))
		if n := qs[2]; n.Answers != nil || n.USDPUSD != nil {
			t.Errorf("none: %+v", n)
		}
		zero := 0
		for _, bad := range []store.TenantQuota{{TenantID: "a b"}, {TenantID: ""}, {TenantID: "ten_x", Answers: &zero}} {
			if err := s.PutTenantQuota(ctx, bad); err == nil {
				t.Errorf("PutTenantQuota(%+v) was taken", bad)
			}
		}
		if err := s.DeleteTenantQuota(ctx, "ten_b"); err != nil {
			t.Fatal(err)
		}
		r = rev(t, s)
		if err := s.DeleteTenantQuota(ctx, "ten_b"); err != nil {
			t.Errorf("unset again: %v", err)
		}
		if got := rev(t, s); got != r {
			t.Error("unsetting what was not set moved the revision on")
		}
		if qs, _ := s.TenantQuotas(ctx); len(qs) != 2 {
			t.Errorf("after the unset: %+v", qs)
		}
	})
}

// The cost report groups the model calls of a span, by day, tenant, agent,
// model (with its key source) or key source, or all together; counts the
// calls no price held; filters by key source; and pages by key.
func testCostReport(t *testing.T, open Opener) {
	s, ctx := open(t), t.Context()
	d1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	d2 := d1.AddDate(0, 0, 1)
	yuki := scope{agent: "agt_y", tenant: "ten_yuki", course: "c1", opener: "m1", key: "school"}
	ken := scope{agent: "agt_k", tenant: "ten_ken", course: "c1", opener: "m2", key: "own"}
	c := func(id string, when time.Time, sc scope, cost int64, model, version string) store.LLMCall {
		x := call(id, when, sc, cost)
		x.Model, x.PriceVersion = model, version
		return x
	}
	record(t, s, []store.LLMCall{
		c("c1", d1, yuki, 100, "m-a", "v1/a"),
		c("c2", d1.Add(time.Hour), yuki, 200, "m-a", "v1/a"),
		c("c3", d2, yuki, 0, "m-b", ""),
		c("c4", d2, ken, 50, "m-a", "v1/a"),
		c("c5", d2.AddDate(0, 0, 5), ken, 999, "m-a", "v1/a"),
	}, nil)
	report := func(q store.CostQuery) []store.CostRow {
		t.Helper()
		if q.Since.IsZero() {
			q.Since, q.Until = store.UTCDay(d1), store.UTCDay(d2).AddDate(0, 0, 1)
		}
		if q.Limit == 0 {
			q.Limit = 100
		}
		rows, err := s.CostReport(ctx, q)
		if err != nil {
			t.Fatalf("CostReport(%+v): %v", q, err)
		}
		return rows
	}
	keysOf := func(rows []store.CostRow) []string {
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.Key
		}
		return out
	}
	total := report(store.CostQuery{Group: store.CostByTotal})
	if len(total) != 1 || total[0].Key != "" || total[0].ModelCalls != 4 || total[0].Unpriced != 1 || total[0].CostPUSD != 350 ||
		total[0].InputTokens != 4800 || total[0].CacheReadTokens != 4000 || total[0].OutputTokens != 840 {
		t.Errorf("the total: %+v", total)
	}
	days := report(store.CostQuery{Group: store.CostByDay})
	if !slices.Equal(keysOf(days), []string{"2026-09-01", "2026-09-02"}) || !days[0].Day.Equal(store.UTCDay(d1)) || days[0].CostPUSD != 300 ||
		days[1].ModelCalls != 2 {
		t.Errorf("by day: %+v", days)
	}
	tenants := report(store.CostQuery{Group: store.CostByTenant})
	if !slices.Equal(keysOf(tenants), []string{"ten_ken", "ten_yuki"}) || tenants[1].TenantID != "ten_yuki" || tenants[1].CostPUSD != 300 ||
		tenants[1].Unpriced != 1 {
		t.Errorf("by tenant: %+v", tenants)
	}
	agents := report(store.CostQuery{Group: store.CostByAgent})
	if !slices.Equal(keysOf(agents), []string{"agt_k", "agt_y"}) || agents[1].TenantID != "ten_yuki" || agents[1].AgentID != "agt_y" {
		t.Errorf("by agent: %+v", agents)
	}
	models := report(store.CostQuery{Group: store.CostByModel})
	if !slices.Equal(keysOf(models), []string{"own/openai/m-a", "school/openai/m-a", "school/openai/m-b"}) ||
		models[1].KeySource != "school" || models[1].Provider != "openai" || models[1].Model != "m-a" || models[1].CostPUSD != 300 {
		t.Errorf("by model: %+v", models)
	}
	if ks := report(store.CostQuery{Group: store.CostByKeySource}); !slices.Equal(keysOf(ks), []string{"own", "school"}) || ks[0].CostPUSD != 50 {
		t.Errorf("by key source: %+v", ks)
	}
	if school := report(store.CostQuery{Group: store.CostByTenant, KeySource: "school"}); !slices.Equal(keysOf(school), []string{"ten_yuki"}) {
		t.Errorf("on the school's key: %+v", school)
	}
	page := report(store.CostQuery{Group: store.CostByModel, Limit: 2})
	next := report(store.CostQuery{Group: store.CostByModel, Limit: 2, After: page[1].Key})
	if !slices.Equal(keysOf(page), keysOf(models)[:2]) || !slices.Equal(keysOf(next), keysOf(models)[2:]) {
		t.Errorf("paged: %v then %v", keysOf(page), keysOf(next))
	}
	if none := report(store.CostQuery{Group: store.CostByTotal, Since: d1.AddDate(1, 0, 0), Until: d1.AddDate(1, 0, 1)}); len(none) != 0 {
		t.Errorf("a span of nothing: %+v", none)
	}
	for _, bad := range []store.CostQuery{
		{Group: "course", Since: d1, Until: d2, Limit: 1},
		{Group: store.CostByDay, Since: d2, Until: d1, Limit: 1},
		{Group: store.CostByDay, Since: d1, Until: d2, Limit: 0},
		{Group: store.CostByDay, Since: d1, Until: d2, Limit: store.MaxCostRows + 1},
	} {
		if _, err := s.CostReport(ctx, bad); err == nil {
			t.Errorf("CostReport(%+v) was taken", bad)
		}
	}
}
