package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// schoolKey is the school's key, in the file the plan's offer refers to (in
// these tests, the variable a secret:// reference falls back to): no log
// line may hold it.
const schoolKey = "sk-school-plan-0123456789abcdefghijklmn"

// planRuntime is a runtime document with the school's plan: one offer,
// whose scripted model is "school-m", and school's settings over it.
func planRuntime(school map[string]any) map[string]any {
	plan := map[string]any{"offers": []any{map[string]any{
		"id": "standard", "label": "School AI", "adapter": "openai_chat", "model": "school-m",
		"base_url": "https://llm.school.example/v1", "key_ref": "secret://school/keys/plan",
	}}}
	for k, v := range school {
		plan[k] = v
	}
	return map[string]any{"school": plan}
}

// hostOnPlan hosts ag as the hosted agent id on the school's plan, in its
// owner's tenant, as the API writes one: the offer's id, and, when own is
// not "", the owner's own model (the scripted model own) behind it on their
// sealed key.
func (h *hosting) hostOnPlan(id string, ag agent, own string) {
	h.w.t.Helper()
	tenant := "ten_" + ag.owner.ID
	model := map[string]any{"key_source": "school", "offer": "standard"}
	var secrets []store.Secret
	tok := h.seal(tenant, store.SecretCoreToken, ag.actor.Token)
	secrets = append(secrets, tok)
	row := store.HostedAgent{ID: id, CoreActorID: ag.actor.ID, OwnerActorID: ag.owner.ID, TenantID: tenant, DisplayName: "Hosted " + id,
		TokenSecretID: tok.ID}
	if own != "" {
		model["fallback"] = map[string]any{"adapter": "openai_chat", "model": own, "key_source": "own", "params": map[string]any{"max_output_tokens": 500}}
		key := h.seal(tenant, store.SecretModelKey, modelKey)
		secrets = append(secrets, key)
		row.KeySecretID = key.ID
	}
	settings, err := json.Marshal(map[string]any{
		"model": model, "polling": fastPolling(), "budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 10}},
	})
	h.w.ok(err)
	row.Settings = settings
	_, err = h.st.CreateHostedAgent(context.Background(), row, secrets...)
	h.w.ok(err)
}

// adapterKeys are the keys and clients the worker built each model's
// adapter with, by model.
type adapterKeys struct {
	mu      sync.Mutex
	keys    map[string]string
	clients map[string]*http.Client
}

// startPlan runs a worker on the registry's store, recording what each
// model's adapter was built with; hostedHTTP is the hosted-model client.
func (h *hosting) startPlan(cfg *config.Config, ms models, hostedHTTP *http.Client) (*worker, *adapterKeys) {
	h.w.t.Helper()
	h.w.env.Store("AISHIE_SECRET_SCHOOL_KEYS_PLAN", schoolKey)
	built := &adapterKeys{keys: map[string]string{}, clients: map[string]*http.Client{}}
	wk := h.w.start(cfg, ms, workerOpts{store: h.st, edit: func(o *Options) {
		o.Secrets = secrets.Resolver{Getenv: h.w.getenv, Sealed: vault.Opener{Vault: h.v, Store: h.st}}
		o.HostedHTTPClient = hostedHTTP
		next := o.NewAdapter
		o.NewAdapter = func(c llm.Config) (llm.Adapter, error) {
			built.mu.Lock()
			built.keys[c.Model], built.clients[c.Model] = c.APIKey, c.HTTPClient
			built.mu.Unlock()
			return next(c)
		}
	}})
	return wk, built
}

// settled waits until the ledger has an answer for conv: the quotas are
// measured on it, a moment after Core shows the answer.
func settled(t *testing.T, wk *worker, conv string) {
	t.Helper()
	eventually(t, "the answer in the ledger", func() bool { return len(wk.st.outcomes(conv)) == 1 })
}

// A hosted agent on the school's plan answers with the offer's model and
// the school's key, over the runtime's own client, the operator having
// chosen the endpoint. The plan's quota per owner holds across all of the
// owner's agents: spent, a question is given the plan's notice, in English
// and Traditional Chinese, with no model call; another owner has a quota of
// their own. The key is in no log line.
func TestSchoolPlanOwnerQuotaAcrossAgents(t *testing.T) {
	w := newWorld(t)
	first := w.ownAgent("agt_first", 0)
	second := w.ownAgent("agt_second", 0)
	kens := w.ownAgent("agt_kens", 1)
	h := w.hosting()
	h.hostOnPlan("agt_first", first, "")
	h.hostOnPlan("agt_second", second, "")
	h.hostOnPlan("agt_kens", kens, "")
	cfg := h.build(w.config(planRuntime(map[string]any{"per_owner_day": map[string]any{"answers": 2}})))
	if len(cfg.Agents) != 3 {
		t.Fatalf("agents %d, rejected %+v", len(cfg.Agents), cfg.Rejected)
	}
	school := scripted.New(scripted.Reply("From the school."), scripted.Reply("From the school, again."), scripted.Reply("Ken's."))
	hostedHTTP := &http.Client{}
	wk, built := h.startPlan(cfg, models{"school-m": school}, hostedHTTP)

	c1, _ := w.ask(0, first, "One.")
	w.waitAnswers(c1, 1)
	settled(t, wk, c1)
	c2, _ := w.ask(0, second, "Two, of another agent of Yuki's.")
	if got := w.waitAnswers(c2, 1); got[0].Body != "From the school, again." {
		t.Fatalf("the second agent's answer: %q", got[0].Body)
	}
	settled(t, wk, c2)
	c3, _ := w.ask(0, first, "Three.")
	want := config.SchoolQuotaTextEn + "\n\n" + config.SchoolQuotaTextZhHant
	if got := w.waitAnswers(c3, 1); got[0].Body != want {
		t.Errorf("over the owner's quota: %q", got[0].Body)
	}
	c4, _ := w.ask(1, kens, "Ken's first.")
	if got := w.waitAnswers(c4, 1); got[0].Body != "Ken's." {
		t.Errorf("another owner's: %q", got[0].Body)
	}
	if n := len(school.Requests()); n != 3 {
		t.Errorf("the school's model was called %d times", n)
	}
	built.mu.Lock()
	if built.keys["school-m"] != schoolKey || built.clients["school-m"] == hostedHTTP {
		t.Errorf("the offer's adapter: key %q, over the hosted-model client %v", built.keys["school-m"], built.clients["school-m"] == hostedHTTP)
	}
	built.mu.Unlock()
	settled(t, wk, c3)
	calls, recs := wk.st.ledger()
	for _, c := range calls {
		if c.KeySource != config.KeySchool || c.TenantID != "ten_"+w.students[0].ID && c.TenantID != "ten_"+w.students[1].ID {
			t.Errorf("a call %+v", c)
		}
	}
	for _, r := range recs {
		if r.ConversationID == c3 && (r.Outcome != store.OutcomeQuota || r.Billable) {
			t.Errorf("the notice's row %+v", r)
		}
	}
	if got := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": "school_owner_answers"}); got != 1 {
		t.Errorf("budget_exhausted_total{school_owner_answers} = %v", got)
	}
	if strings.Contains(w.logs.String(), schoolKey) {
		t.Error("a log line holds the school's key")
	}
}

// With the owner's own key behind the plan, a question over the plan's
// quota is answered with it (D8): its calls and its answer are the owner's
// and not the school's, so the school's quota stays where it is, and the
// next question goes to the owner's key too. The plan's quota per asker is
// the school's alone in the same way.
func TestSchoolPlanFallsBackToTheOwnersKey(t *testing.T) {
	for _, tc := range []struct {
		name, quota string
		school      map[string]any
	}{
		{"per owner", "school_owner_answers", map[string]any{"per_owner_day": map[string]any{"answers": 1}}},
		{"per asker", "school_asker_answers", map[string]any{"per_asker_day": map[string]any{"answers": 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			yukis := w.ownAgent("agt_yuki", 0)
			h := w.hosting()
			h.hostOnPlan("agt_yuki", yukis, "own-m")
			cfg := h.build(w.config(planRuntime(tc.school)))
			school := scripted.New(scripted.Reply("From the school."))
			own := scripted.New(scripted.Reply("From Yuki's key."), scripted.Reply("From Yuki's key, again."))
			wk, built := h.startPlan(cfg, models{"school-m": school, "own-m": own}, &http.Client{})

			c1, _ := w.ask(0, yukis, "One.")
			if got := w.waitAnswers(c1, 1); got[0].Body != "From the school." {
				t.Fatalf("first: %q", got[0].Body)
			}
			settled(t, wk, c1)
			c2, _ := w.ask(0, yukis, "Two.")
			if got := w.waitAnswers(c2, 1); got[0].Body != "From Yuki's key." {
				t.Fatalf("over the school's quota: %q", got[0].Body)
			}
			settled(t, wk, c2)
			c3, _ := w.ask(0, yukis, "Three.")
			if got := w.waitAnswers(c3, 1); got[0].Body != "From Yuki's key, again." {
				t.Fatalf("the next: %q", got[0].Body)
			}
			settled(t, wk, c3)
			if len(school.Requests()) != 1 || len(own.Requests()) != 2 {
				t.Errorf("school's model called %d times, the owner's %d", len(school.Requests()), len(own.Requests()))
			}
			built.mu.Lock()
			if built.keys["own-m"] != modelKey || built.keys["school-m"] != schoolKey {
				t.Errorf("keys %v", built.keys)
			}
			built.mu.Unlock()
			calls, recs := wk.st.ledger()
			sources := map[string]string{}
			for _, c := range calls {
				sources[c.ConversationID] += c.KeySource + " "
			}
			for _, r := range recs {
				sources[r.ConversationID] += "answer:" + r.KeySource
			}
			if sources[c1] != "school answer:school" || sources[c2] != "own answer:own" || sources[c3] != "own answer:own" {
				t.Errorf("key sources %q %q %q", sources[c1], sources[c2], sources[c3])
			}
			if got := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": tc.quota}); got != 2 {
				t.Errorf("budget_exhausted_total{%s} = %v", tc.quota, got)
			}
		})
	}
}

// The plan's ceiling across the school holds every agent on the school's
// key, whoever owns it; its notice is in the language answers are fixed
// to.
func TestSchoolPlanCeiling(t *testing.T) {
	w := newWorld(t)
	yukis := w.ownAgent("agt_yuki", 0)
	kens := w.ownAgent("agt_ken", 1)
	h := w.hosting()
	h.hostOnPlan("agt_yuki", yukis, "")
	h.hostOnPlan("agt_ken", kens, "")
	rt := planRuntime(map[string]any{"per_day": map[string]any{"answers": 1}})
	rt["defaults"] = map[string]any{"prompt": map[string]any{"answer_language": "fixed:zh-Hant"}}
	cfg := h.build(w.config(rt))
	school := scripted.New(scripted.Reply("From the school."))
	wk, _ := h.startPlan(cfg, models{"school-m": school}, &http.Client{})

	c1, _ := w.ask(0, yukis, "One.")
	w.waitAnswers(c1, 1)
	settled(t, wk, c1)
	c2, _ := w.ask(1, kens, "Ken's.")
	if got := w.waitAnswers(c2, 1); got[0].Body != config.SchoolQuotaTextZhHant {
		t.Errorf("over the school's ceiling: %q", got[0].Body)
	}
	if n := len(school.Requests()); n != 1 {
		t.Errorf("the school's model was called %d times", n)
	}
	if got := counter(t, wk.reg, "budget_exhausted_total", map[string]string{"budget": "school_day_answers"}); got != 1 {
		t.Errorf("budget_exhausted_total{school_day_answers} = %v", got)
	}
}
