package registry

import (
	"reflect"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

// orSiteOffer is an offer of the site's at OpenRouter, with routing r.
func orSiteOffer(t *testing.T, st *memstore.Store, id string, r *openrouter.Routing) {
	t.Helper()
	o := store.SchoolOffer{ID: id, Label: "Site " + id, Adapter: "openai_chat", Provider: "openrouter", Model: "meta-llama/llama-3.3-70b-instruct",
		BaseURL: "https://openrouter.ai/api/v1", Enabled: true, KeySecretID: "sec_school_" + id, KeyHint: "sk-or-…aaaa", CreatedBy: "admin", OpenRouter: r}
	if _, err := st.CreateSchoolOffer(t.Context(), o, fakeSecret(o.KeySecretID, store.SchoolTenantID, store.SecretModelKey)); err != nil {
		t.Fatal(err)
	}
}

// A hosted agent on a site's offer at OpenRouter is built with the
// offer's upstream routing, and one on an offer without any, without; the
// offer's routing changed, the agent is built anew with it, so that the
// worker, holding it to what it ran, starts it again.
func TestBuildWithTheOffersRouting(t *testing.T) {
	yaml := yamlConfig(t, "")
	yaml.Runtime.School = schoolPlan()
	st := hostedStore(t, []store.HostedAgent{
		row("agt_routed", `{"model": {"key_source": "school", "offer": "routed"}}`),
		row("agt_plain", `{"model": {"key_source": "school", "offer": "plain"}}`),
	})
	deny := "deny"
	orSiteOffer(t, st, "routed", &openrouter.Routing{DataCollection: &deny, Only: []string{"groq", "together"}, Sort: &openrouter.Sort{By: "price"}})
	orSiteOffer(t, st, "plain", nil)
	build := func() map[string]*config.Agent {
		t.Helper()
		cfg, _, err := Build(t.Context(), yaml, st, Options{CoreBaseURL: core})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*config.Agent{}
		for _, a := range cfg.Agents {
			out[a.ID] = a
		}
		if out["agt_routed"] == nil || out["agt_plain"] == nil {
			t.Fatalf("agents %v, rejected %v", agentIDs(cfg), rejectedWhy(cfg))
		}
		return out
	}
	first := build()
	if got := string(first["agt_routed"].Model.OpenRouter.JSON()); got != `{"data_collection":"deny","only":["groq","together"],"sort":"price"}` {
		t.Errorf("on the routed offer: %s", got)
	}
	if r := first["agt_plain"].Model.OpenRouter; r != nil {
		t.Errorf("on the offer without routing: %s", r.JSON())
	}
	same := build()
	if !sameAgent(first["agt_routed"], same["agt_routed"]) {
		t.Error("a build of nothing changed made the agent anew")
	}

	o, err := st.SchoolOffer(t.Context(), "routed")
	if err != nil {
		t.Fatal(err)
	}
	o.OpenRouter = &openrouter.Routing{DataCollection: &deny, Only: []string{"together"}}
	if _, err := st.UpdateSchoolOffer(t.Context(), *o); err != nil {
		t.Fatal(err)
	}
	next := build()
	if got := string(next["agt_routed"].Model.OpenRouter.JSON()); got != `{"data_collection":"deny","only":["together"]}` {
		t.Errorf("after the change: %s", got)
	}
	if sameAgent(first["agt_routed"], next["agt_routed"]) {
		t.Error("the agent was built alike after its offer's routing changed")
	}
	if !sameAgent(first["agt_plain"], next["agt_plain"]) {
		t.Error("an agent on another offer changed")
	}
}

// sameAgent reports whether two builds of an agent are alike, as the
// worker compares them to know whether to start it again (sameRun).
func sameAgent(x, y *config.Agent) bool { return reflect.DeepEqual(x, y) }
