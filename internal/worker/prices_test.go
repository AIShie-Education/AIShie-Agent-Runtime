package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// A model call is costed by the price table in force, the site's rows
// among it, and the ledger names the row it was priced by: once the site
// changes its prices, the next call is costed by the new price, under a
// new version, and the call before keeps its own. (The scripted models
// are priced as the provider scripted.)
func TestSitePricesCostTheCalls(t *testing.T) {
	w := newWorld(t)
	yukis := w.ownAgent("agt_yuki", 0)
	h := w.hosting()
	h.host("agt_yuki", yukis, hostedSettings("own-m"))
	ctx := context.Background()
	row, err := h.st.CreateSitePrice(ctx, store.SitePrice{ID: "own", Provider: "scripted", Model: "scripted-model", From: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		InputPUSD: 1_000_000, CacheReadPUSD: 1_000_000, CacheWritePUSD: 1_000_000, OutputPUSD: 2_000_000, CreatedBy: "admin"})
	w.ok(err)
	yaml := &config.Config{}
	cfg := h.build(yaml)
	table := cfg.Runtime.Site.PriceTable(nil)
	own := scripted.New(scripted.Reply("First."), scripted.Reply("Second."))
	wk := w.start(cfg, models{"own-m": own}, workerOpts{store: h.st, prices: table, edit: func(o *Options) {
		o.Secrets = secrets.Resolver{Getenv: w.getenv, Sealed: vault.Opener{Vault: h.v, Store: h.st}}
		o.Sealer = h.v
	}})
	callOf := func(conv string) store.LLMCall {
		t.Helper()
		settled(t, wk, conv)
		calls, _ := wk.st.ledger()
		for _, c := range calls {
			if c.ConversationID == conv {
				return c
			}
		}
		t.Fatalf("no call of %s", conv)
		return store.LLMCall{}
	}
	c1, _ := w.ask(0, yukis, "One.")
	w.waitAnswers(c1, 1)
	first := callOf(c1)
	v1 := pricing.SiteVersion(cfg.Runtime.Site.PricesChanged)
	if first.PriceVersion != v1+"/own" || first.CostPUSD == 0 || !strings.HasPrefix(v1, "site-") {
		t.Fatalf("the first call: %s, %d pUSD (%s %s)", first.PriceVersion, first.CostPUSD, first.Provider, first.Model)
	}

	row.OutputPUSD = 20_000_000
	_, err = h.st.UpdateSitePrice(ctx, *row)
	w.ok(err)
	cfg = h.build(yaml)
	wk.sup.SetPrices(cfg.Runtime.Site.PriceTable(nil))
	wk.sup.Update(cfg)
	c2, _ := w.ask(0, yukis, "Two.")
	w.waitAnswers(c2, 1)
	second := callOf(c2)
	v2 := pricing.SiteVersion(cfg.Runtime.Site.PricesChanged)
	if v2 == v1 || second.PriceVersion != v2+"/own" || second.CostPUSD <= first.CostPUSD {
		t.Errorf("the second call: %s, %d pUSD; the first %s, %d", second.PriceVersion, second.CostPUSD, first.PriceVersion, first.CostPUSD)
	}
	if again := callOf(c1); again.PriceVersion != v1+"/own" || again.CostPUSD != first.CostPUSD {
		t.Errorf("the first call was rewritten: %+v", again)
	}
}
