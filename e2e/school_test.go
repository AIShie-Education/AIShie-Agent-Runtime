package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/api"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// schoolKey is the school's key in the tests' secret store, and siteKey
// the one an administrator gives an offer through the API: no log, answer
// or row may hold either.
const (
	schoolKey = "sk-school-e2e-0123456789abcdefghijkl"
	siteKey   = "sk-site-e2e-9876543210zyxwvutsrqpo"
)

// schoolPlanThroughTheAPI is the school's AI plan (D8) against the real
// Core: the runtime offers one model on the school's key, which is a file
// in its secret store; Yuki connects her agent and chooses the offer, with
// no key of her own; it answers her with the school's key, at the offer's
// endpoint, which the operator chose. Her quota for the day, one answer,
// spent, her next question is given the plan's notice, in English and
// Chinese, with no model call. With her own model and key behind the
// plan, her next is answered with her key, and the school's quota stays
// where it was. Then the school's administrator makes an offer through the
// API, its key tried with the model at OpenAI's own endpoint and sealed,
// and raises the quota per owner; Yuki puts her agent on it, and it
// answers her with that key, over the hosted-model client. A quota of the
// plan's in dollars is refused until the site prices every offer, and the
// model behind Yuki's; priced, the next call is costed by the site's row,
// under its version, and the cost report says so. The offer turned off,
// her own key answers, without a restart. No log, answer or row holds the
// school's keys or the reference of runtime.yaml's.
func schoolPlanThroughTheAPI(t *testing.T, w *world) {
	audience := os.Getenv("E2E_RUNTIME_AUDIENCE")
	if audience == "" {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); ci {
			t.Fatal("E2E_RUNTIME_AUDIENCE is not set, and CI is: start Core with scripts/ci-core.sh")
		}
		t.Skip("E2E_RUNTIME_AUDIENCE is not set: this Core makes no assertion for a runtime (scripts/ci-core.sh sets it)")
	}
	st, dbURL := runtimeStore(t)
	v, kek := keyring(t)
	w.addSecret("the key that seals the school plan's runtime's secrets", kek)
	w.addSecret("the school's key", schoolKey)
	w.addSecret("the key of the school's the administrator gives", siteKey)
	w.secretsDir = t.TempDir()
	keyFile := filepath.Join(w.secretsDir, "school", "keys", "e2e")
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(schoolKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(t, fakellm.DefaultResponder)
	one := 1
	// The offer's endpoint is the scripted model's own address, as an
	// operator's gateway would be: the hosted-model client would not call
	// it, the runtime's own does.
	yaml := &config.Config{Runtime: config.Runtime{
		Defaults: map[string]any{"polling": polling(), "budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 60}}},
		School: config.School{
			Offers: []config.SchoolOffer{{ID: "standard", Label: "School AI (e2e)", Adapter: "openai_chat", Model: "e2e-school-model",
				BaseURL: m.URL(), KeyRef: "secret://school/keys/e2e"}},
			PerOwnerDay: config.Quota{Answers: &one},
		},
	}}
	rt := w.startHosted(t, m, st, v, yaml)
	target, err := url.Parse(m.URL())
	if err != nil {
		t.Fatal(err)
	}
	keyTrials := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(keyTrials.CloseIdleConnections)
	a := w.startAPI(t, st, audience, func(o *api.Options) {
		o.Vault, o.Actors, o.Hosting = v, rt.sup, staticHosting{yaml: yaml}
		// An offer's key is tried at OpenAI's own endpoint, which is the
		// scripted model here, as the worker's hosted-model client has it.
		o.ModelHTTP = modelClient(keyTrials, target)
	})
	type answer struct {
		code int
		body []byte
	}
	call := func(method, path, body string, headers ...string) answer {
		t.Helper()
		code, _, raw := a.do(t, method, "/runtime/api/v1/"+path, w.assertion(t, w.yuki.token, "", audience), body, headers...)
		return answer{code, raw}
	}
	decodeAs := func(an answer, want int, v any) {
		t.Helper()
		if an.code != want {
			t.Fatalf("%d, want %d: %s", an.code, want, an.body)
		}
		if err := json.Unmarshal(an.body, v); err != nil {
			t.Fatalf("%v: %s", err, an.body)
		}
	}

	var info api.Info
	decodeAs(answer{200, mustGet(t, a, "/runtime/api/v1/info")}, 200, &info)
	if !info.Features.SchoolKey {
		t.Errorf("GET /info: %+v", info.Features)
	}
	var models api.Models
	decodeAs(call("GET", "models", ""), 200, &models)
	if o := models.SchoolKey.Offers; !models.SchoolKey.Offered || len(o) != 1 || o[0].ID != "standard" || o[0].Label != "School AI (e2e)" ||
		o[0].Model != "e2e-school-model" || models.SchoolKey.Limits.PerOwnerDay != 1 {
		t.Fatalf("GET /models' school_key: %+v", models.SchoolKey)
	}

	// Yuki hosts her agent by its id, and puts it on the school's plan.
	host, _ := json.Marshal(map[string]string{"agent_id": w.own.id})
	var agent api.HostedAgent
	decodeAs(call("POST", "agents", string(host)), http.StatusCreated, &agent)
	id := agent.ID
	decodeAs(call("PATCH", "agents/"+id, `{"model":{"school":{"offer":"standard"}}}`, "If-Match", `"1"`), 200, &agent)
	if agent.Model.School == nil || agent.Model.School.Label != "School AI (e2e)" || agent.Model.Own != nil || agent.OwnKey != nil ||
		agent.Today.School == nil || agent.Today.School.Limit != 1 {
		t.Fatalf("on the plan: %+v", agent)
	}
	waitStatus := func(want string, version int) {
		t.Helper()
		var got api.HostedAgent
		eventually(t, answerWait, "the hosted agent "+want, func() bool {
			an := call("GET", "agents/"+id, "")
			return an.code == 200 && json.Unmarshal(an.body, &got) == nil && got.Status == want && got.Version == version
		})
	}
	// Running at 3: the worker kept the token it was issued in the row.
	waitStatus(api.StatusRunning, 3)
	calls := func() []fakellm.ChatRequest { return m.Requests() }

	// It answers her, on the school's key.
	const q1 = "Does the school's AI answer me?"
	conv, _ := w.ask(t, w.yuki, w.own.member, q1)
	if ans := w.waitAnswer(t, w.yuki, conv, w.own.member); !strings.HasPrefix(ans.text(), "Answer: "+q1) {
		t.Fatalf("the plan's answer: %q", ans.text())
	}
	reqs := calls()
	if len(reqs) == 0 || reqs[len(reqs)-1].Model != "e2e-school-model" || reqs[len(reqs)-1].Header.Get("Authorization") != "Bearer "+schoolKey {
		t.Fatalf("the offer's call: %d calls", len(reqs))
	}
	var used api.HostedAgent
	eventually(t, answerWait, "the plan's use counting the answer", func() bool {
		an := call("GET", "agents/"+id, "")
		return an.code == 200 && json.Unmarshal(an.body, &used) == nil && used.Today.School != nil && used.Today.School.Used == 1
	})

	// Her day's quota spent: the plan's notice, and no model call.
	before := len(calls())
	conv2, _ := w.ask(t, w.yuki, w.own.member, "And again?")
	want := config.SchoolQuotaTextEn + "\n\n" + config.SchoolQuotaTextZhHant
	if ans := w.waitAnswer(t, w.yuki, conv2, w.own.member); ans.text() != want {
		t.Errorf("over the plan's quota: %q", ans.text())
	}
	if n := len(calls()) - before; n != 0 {
		t.Errorf("%d model calls over the quota", n)
	}

	// Her own model and key behind the plan: her key answers.
	own, _ := json.Marshal(map[string]any{"model": map[string]any{"own": map[string]any{"provider": "openai", "model": "e2e-own-model"}},
		"own_key": map[string]any{"value": w.modelKey}})
	decodeAs(call("PATCH", "agents/"+id, string(own), "If-Match", `"3"`), 200, &agent)
	if agent.Model.School == nil || !agent.Model.School.Fallback || agent.Model.Own == nil {
		t.Fatalf("with her key behind the plan: %+v", agent)
	}
	waitStatus(api.StatusRunning, 4)
	const q3 = "And with my own key?"
	conv3, _ := w.ask(t, w.yuki, w.own.member, q3)
	if ans := w.waitAnswer(t, w.yuki, conv3, w.own.member); !strings.HasPrefix(ans.text(), "Answer: "+q3) {
		t.Fatalf("the fallback's answer: %q", ans.text())
	}
	reqs = calls()
	if last := reqs[len(reqs)-1]; last.Model != "e2e-own-model" || last.Header.Get("Authorization") != "Bearer "+w.modelKey {
		t.Errorf("the fallback's call: model %q", last.Model)
	}
	eventually(t, answerWait, "today's use counting the fallback's answer", func() bool {
		an := call("GET", "agents/"+id, "")
		return an.code == 200 && json.Unmarshal(an.body, &used) == nil && used.Today.Answers >= 2
	})
	if used.Today.School == nil || used.Today.School.Used != 1 {
		t.Errorf("the plan's use after the fallback: %+v", used.Today.School)
	}

	// The administrator makes an offer of the school's through the API,
	// and gives owners four answers more a day.
	asAdmin := func(method, path, body string, headers ...string) answer {
		t.Helper()
		code, _, raw := a.do(t, method, "/runtime/api/v1/"+path, w.assertion(t, w.admin.token, "", audience), body, headers...)
		return answer{code, raw}
	}
	before = len(calls())
	var offer api.PlanOffer
	made, _ := json.Marshal(map[string]any{"id": "site", "label": "School AI (site)", "provider": "openai", "model": "e2e-site-model",
		"max_output_tokens": 800, "key": siteKey})
	decodeAs(asAdmin("POST", "admin/school-plan/offers", string(made)), http.StatusCreated, &offer)
	if offer.Source != api.SourceSite || offer.KeyStatus == nil || *offer.KeyStatus != api.KeyTested || offer.KeyHint == nil ||
		*offer.KeyHint != "sk-…rqpo" || offer.Status != api.OfferOffered {
		t.Fatalf("the offer made: %+v", offer)
	}
	if reqs = calls(); len(reqs) != before+1 || reqs[len(reqs)-1].Model != "e2e-site-model" ||
		reqs[len(reqs)-1].Header.Get("Authorization") != "Bearer "+siteKey {
		t.Fatalf("the offer's key tried: %d calls", len(reqs)-before)
	}
	var plan api.SchoolPlan
	decodeAs(asAdmin("PUT", "admin/school-plan/quotas", `{"per_owner_day":5,"per_asker_day":20,"per_day":null}`), 200, &plan)
	if !plan.QuotasSet || plan.Quotas.PerOwnerDay != 5 || plan.QuotaDefaults.PerOwnerDay != 1 || len(plan.Offers) != 2 {
		t.Fatalf("the quotas set: %+v", plan)
	}
	decodeAs(call("GET", "models", ""), 200, &models)
	if o := models.SchoolKey.Offers; len(o) != 2 || o[1].ID != "site" || models.SchoolKey.Limits.PerOwnerDay != 5 {
		t.Fatalf("GET /models with the site's offer: %+v", models.SchoolKey)
	}

	// Yuki puts her agent on it, her own model still behind it: the offer
	// answers, with the key the administrator gave.
	decodeAs(call("PATCH", "agents/"+id, `{"model":{"school":{"offer":"site"}}}`, "If-Match", `"4"`), 200, &agent)
	if agent.Model.School == nil || agent.Model.School.Offer != "site" || !agent.Model.School.Fallback || agent.Today.School.Limit != 5 {
		t.Fatalf("on the site's offer: %+v", agent)
	}
	waitStatus(api.StatusRunning, 5)
	const q4 = "Does the site's offer answer me?"
	conv4, _ := w.ask(t, w.yuki, w.own.member, q4)
	if ans := w.waitAnswer(t, w.yuki, conv4, w.own.member); !strings.HasPrefix(ans.text(), "Answer: "+q4) {
		t.Fatalf("the site's offer's answer: %q", ans.text())
	}
	reqs = calls()
	if last := reqs[len(reqs)-1]; last.Model != "e2e-site-model" || last.Header.Get("Authorization") != "Bearer "+siteKey {
		t.Errorf("the site's offer's call: model %q", last.Model)
	}

	// A quota in dollars needs a price for every offer, and for the model
	// behind Yuki's: the site adds them, and the quota is taken.
	refused := func(an answer, reason string) map[string]any {
		t.Helper()
		var e struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		decodeAs(an, http.StatusUnprocessableEntity, &e)
		if e.Error.Details["reason"] != reason {
			t.Fatalf("refused as %v, want %s: %s", e.Error.Details["reason"], reason, an.body)
		}
		return e.Error.Details
	}
	const dollars = `{"per_owner_day":5,"per_asker_day":20,"per_day":null,"per_day_usd":"50"}`
	if d := refused(asAdmin("PUT", "admin/school-plan/quotas", dollars), api.ReasonOfferNotPriced); fmt.Sprint(d["offers"]) != "[standard site]" {
		t.Errorf("the offers unpriced: %v", d["offers"])
	}
	price := func(id, provider, model string) {
		t.Helper()
		body := `{"id":"` + id + `","provider":"` + provider + `","model":"` + model + `","from":"2025-01-01","usd_per_mtok":{"input":"1.25","output":10}}`
		var row api.PriceRow
		decodeAs(asAdmin("POST", "admin/prices", body), http.StatusCreated, &row)
		if row.Source != api.PriceSourceSite || !strings.HasPrefix(row.Version, "site-") || !strings.HasSuffix(row.Version, "/"+id) {
			t.Fatalf("the price made: %+v", row)
		}
	}
	// runtime.yaml's offer is at an endpoint of the operator's, an
	// OpenAI-compatible one; the site's, at OpenAI's own.
	price("e2e-school", llm.ProviderOpenAICompat, "e2e-school-model")
	price("e2e-site", llm.ProviderOpenAI, "e2e-site-model")
	if d := refused(asAdmin("PUT", "admin/school-plan/quotas", dollars), api.ReasonModelNotPriced); !strings.Contains(fmt.Sprint(d["problems"]),
		"e2e-own-model") {
		t.Errorf("the model behind Yuki's: %v", d["problems"])
	}
	price("e2e-own", llm.ProviderOpenAI, "e2e-own-model")
	decodeAs(asAdmin("PUT", "admin/school-plan/quotas", dollars), 200, &plan)
	if plan.Quotas.PerDayUSD == nil || *plan.Quotas.PerDayUSD != "50.000000" {
		t.Fatalf("the quota in dollars: %+v", plan.Quotas)
	}
	const q4b = "Is this answer costed?"
	conv4b, _ := w.ask(t, w.yuki, w.own.member, q4b)
	if ans := w.waitAnswer(t, w.yuki, conv4b, w.own.member); !strings.HasPrefix(ans.text(), "Answer: "+q4b) {
		t.Fatalf("the answer under the quota in dollars: %q", ans.text())
	}
	var costs api.CostReport
	eventually(t, answerWait, "the cost report counting the priced call", func() bool {
		an := asAdmin("GET", "admin/costs?group=model&key_source=school", "")
		if an.code != 200 || json.Unmarshal(an.body, &costs) != nil {
			return false
		}
		for _, r := range costs.Rows {
			if r.Key == "school/openai/e2e-site-model" && r.Lines[0].Calls > r.Lines[0].UnpricedCalls && r.CostUSD != "0.000000" {
				return slices.Equal(r.Offers, []string{"site"})
			}
		}
		return false
	})
	if !regexp.MustCompile(`site-[0-9]{8}T[0-9]{6}Z/e2e-site\b`).MatchString(dumpDatabase(t, dbURL)) {
		t.Error("no cost in the ledger names the site's row")
	}

	// The offer turned off: the agent is on her own model again, without
	// a restart of the runtime, and her key answers.
	running, err := st.AgentState(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	decodeAs(asAdmin("PATCH", "admin/school-plan/offers/site", `{"enabled":false}`, "If-Match", `"1"`), 200, &offer)
	if offer.Enabled || offer.Status != api.OfferDisabled || offer.Agents != 1 {
		t.Fatalf("the offer turned off: %+v", offer)
	}
	eventually(t, answerWait, "the agent started again on her own model", func() bool {
		s, err := st.AgentState(t.Context(), id)
		return err == nil && s.State == store.AgentRunning && s.UpdatedAt.After(running.UpdatedAt)
	})
	const q5 = "And with the site's offer off?"
	conv5, _ := w.ask(t, w.yuki, w.own.member, q5)
	if ans := w.waitAnswer(t, w.yuki, conv5, w.own.member); !strings.HasPrefix(ans.text(), "Answer: "+q5) {
		t.Fatalf("the answer with the offer off: %q", ans.text())
	}
	reqs = calls()
	if last := reqs[len(reqs)-1]; last.Model != "e2e-own-model" || last.Header.Get("Authorization") != "Bearer "+w.modelKey {
		t.Errorf("the call with the offer off: model %q", last.Model)
	}
	var gone api.OfferDeleted
	decodeAs(asAdmin("DELETE", "admin/school-plan/offers/site", ""), 200, &gone)
	if gone.Deleted.ID != "site" || gone.Agents != 1 {
		t.Errorf("the offer deleted: %+v", gone)
	}

	// Nothing the runtime keeps or says holds the school's keys, or refers
	// to runtime.yaml's (its logs are searched with the others').
	for what, text := range map[string]string{"the runtime's database": dumpDatabase(t, dbURL), "the API's answers": a.answers.String()} {
		if strings.Contains(text, schoolKey) || strings.Contains(text, siteKey) || strings.Contains(text, "school/keys") {
			t.Errorf("%s holds or refers to the school's keys", what)
		}
	}
}
