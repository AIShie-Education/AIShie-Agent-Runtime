package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// The school's keys the administrators give in these tests: nothing the
// runtime says, logs, audits or keeps in the clear may hold them.
const (
	fastKey  = "sk-school-fast-0123456789abcdefghij"
	fastKey2 = "sk-school-fast-9876543210zyxwvutsrq"
)

// admin sends a request of Ken's as one of the runtime's administrators.
func (h *hostWorld) admin(method, path, body string, headers ...string) answer {
	h.t.Helper()
	return h.adminCall(h.t, "admin", method, path, body, headers...)
}

// noSchoolKeys fails when anything the API said, logged or audited, or
// any row of the store but a secret's sealed bytes, holds a key the
// administrators gave, in the clear or in part.
func (h *hostWorld) noSchoolKeys(answers ...answer) {
	h.t.Helper()
	ctx := context.Background()
	var b strings.Builder
	for _, a := range answers {
		b.WriteString(a.body)
	}
	b.WriteString(h.log.String())
	events, err := h.st.AuditEvents(ctx, at.AddDate(-1, 0, 0), 1000)
	h.ok(err)
	for _, e := range events {
		b.Write(e.Detail)
	}
	offers, err := h.st.SchoolOffers(ctx)
	h.ok(err)
	for _, o := range offers {
		b.WriteString(o.Label + o.KeyHint + o.KeySecretID)
	}
	secrets, err := h.st.ListSecrets(ctx, "", 1000)
	h.ok(err)
	for _, s := range secrets {
		b.WriteString(s.Hint)
		b.Write(s.Ciphertext)
	}
	text := b.String()
	for _, k := range []string{fastKey, fastKey2} {
		if strings.Contains(text, k) || strings.Contains(text, k[len("sk-"):len(k)-4]) {
			h.t.Errorf("a school's key is kept or said: %.12s…", k)
		}
	}
}

const fastOffer = `{"id":"fast","label":"School AI (fast)","provider":"deepseek","model":"deepseek-chat","max_output_tokens":1200,` +
	`"reasoning_effort":"low","key":"` + fastKey + `"}`

// The school's plan as its administrators manage it: GET lists
// runtime.yaml's offers, read-only, and its quotas; an offer made through
// the API is tried with its key, which is sealed under the school's tenant
// and shown only as its hint, and joins what owners choose from; an agent
// put on it runs with its sealed key; turned off, owners are no longer
// offered it and the agent on it, with no model of its owner's behind it,
// is not run, saying so; a new key replaces the one before, which is
// destroyed; a new model leaves the key untried with it; deleted, its key
// goes with it. Every write is audited, with the key's hint alone.
func TestSchoolPlanAdmin(t *testing.T) {
	h, p, fh := newModelWorld(t, schoolRuntime())
	ctx := context.Background()
	var plan SchoolPlan
	a := h.admin("GET", "admin/school-plan", "")
	wantSecured(t, a, "no-store")
	a.decode(t, &plan)
	if a.code != 200 || len(plan.Offers) != 2 || plan.QuotasSet || plan.Quotas != (SchoolPlanLimits{PerOwnerDay: 3, PerAskerDay: 20}) ||
		plan.QuotaDefaults != plan.Quotas || plan.QuotasUpdatedAt != nil {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if o := plan.Offers[0]; o.ID != "standard" || o.Source != SourceConfig || o.Provider != "deepseek" || o.Adapter != "openai_chat" ||
		o.BaseURL == nil || *o.BaseURL != "https://api.deepseek.com" || o.Endpoint != nil || !o.Enabled || o.Status != OfferOffered || !o.Priced ||
		o.KeyHint != nil || o.KeyStatus != nil || o.Version != nil || o.CreatedAt != nil {
		t.Errorf("runtime.yaml's offer: %+v", o)
	}
	if o := plan.Offers[1]; o.ID != "haiku" || o.Provider != "anthropic" || o.BaseURL != nil || o.Priced {
		t.Errorf("runtime.yaml's other offer: %+v", o)
	}

	// An offer made: tried with its key at the provider's own endpoint.
	created := h.admin("POST", "admin/school-plan/offers", fastOffer)
	var o PlanOffer
	created.decode(t, &o)
	if created.code != 201 || created.header.Get("ETag") != `"1"` {
		t.Fatalf("made: %d %s", created.code, created.body)
	}
	want := PlanOffer{ID: "fast", Source: SourceSite, Label: "School AI (fast)", Provider: "deepseek", Adapter: "openai_chat", Model: "deepseek-chat",
		Enabled: true, Status: OfferOffered, Priced: true}
	if o.ID != want.ID || o.Source != want.Source || o.Label != want.Label || o.Provider != want.Provider || o.Adapter != want.Adapter ||
		o.Model != want.Model || !o.Enabled || o.Status != want.Status || !o.Priced || o.BaseURL == nil || *o.BaseURL != "https://api.deepseek.com" ||
		o.MaxOutputTokens == nil || *o.MaxOutputTokens != 1200 || o.ReasoningEffort == nil || *o.ReasoningEffort != "low" ||
		o.KeyHint == nil || *o.KeyHint != "sk-…ghij" || o.KeyStatus == nil || *o.KeyStatus != KeyTested || o.Version == nil || *o.Version != 1 ||
		o.CreatedBy == nil || *o.CreatedBy != ken || o.Agents != 0 {
		t.Errorf("the offer made: %s", created.body)
	}
	p.mu.Lock()
	if len(p.seen) != 1 || p.seen[0].URL.Host != "api.deepseek.com" || p.seen[0].Header.Get("Authorization") != "Bearer "+fastKey {
		t.Errorf("the key's trial: %d calls", len(p.seen))
	}
	p.mu.Unlock()
	row, err := h.st.SchoolOffer(ctx, "fast")
	h.ok(err)
	sec, err := h.st.Secret(ctx, row.KeySecretID)
	h.ok(err)
	if sec.TenantID != store.SchoolTenantID || sec.Kind != store.SecretModelKey || sec.CreatedBy != ken || !row.KeyTested || !row.Enabled {
		t.Errorf("the key kept: %+v, the offer %+v", sec, row)
	}
	if plain, err := h.v.Open(ctx, sec); err != nil || plain != fastKey {
		t.Error("the sealed key does not open to the key given")
	}
	ev := h.events("school_offer.create")
	if len(ev) != 1 || ev[0].TargetType != "school_offer" || ev[0].TargetID != "fast" || ev[0].Outcome != "ok" ||
		!strings.Contains(string(ev[0].Detail), `"key_hint":"sk-…ghij"`) || !strings.Contains(string(ev[0].Detail), `"key_test":"ok"`) {
		t.Errorf("the audit: %+v", ev)
	}
	got := h.admin("GET", "admin/school-plan/offers/fast", "")
	if got.code != 200 || got.header.Get("ETag") != `"1"` || !strings.Contains(got.body, `"id":"fast"`) {
		t.Errorf("GET the offer: %d %s", got.code, got.body)
	}

	// Owners are offered it, and an agent put on it runs with its key.
	var m Models
	models := h.call("GET", "models", h.yuki, "")
	models.decode(t, &m)
	if len(m.SchoolKey.Offers) != 3 || m.SchoolKey.Offers[2] != (SchoolOffer{ID: "fast", Label: "School AI (fast)", Provider: "deepseek",
		Model: "deepseek-chat", Priced: true}) {
		t.Errorf("GET /models: %+v", m.SchoolKey)
	}
	v := h.host(h.yuki, h.helper.ID)
	var agent HostedAgent
	onFast := h.patch(v.ID, `"1"`, `{"model":{"school":{"offer":"fast"}}}`)
	onFast.decode(t, &agent)
	if onFast.code != 200 || agent.Model.School == nil || !agent.Model.School.Offered || agent.Model.School.Label != "School AI (fast)" {
		t.Fatalf("on the site's offer: %d %s", onFast.code, onFast.body)
	}
	cfg, _, err := registry.Build(ctx, fh.YAML(), h.st, registry.Options{CoreBaseURL: h.srv.URL})
	h.ok(err)
	if len(cfg.Agents) != 1 || cfg.Agents[0].Model.KeyRef != "sealed://"+row.KeySecretID || cfg.Agents[0].Model.Offer != "fast" {
		t.Fatalf("the build: %+v %+v", cfg.Agents, cfg.Rejected)
	}
	h.admin("GET", "admin/school-plan", "").decode(t, &plan)
	if plan.Offers[2].Agents != 1 {
		t.Errorf("the agents on it: %+v", plan.Offers[2])
	}

	// Turned off and relabelled, at its version: owners are no longer
	// offered it, and the agent on it is not run, saying so.
	off := h.admin("PATCH", "admin/school-plan/offers/fast", `{"enabled":false,"label":"School AI (quick)"}`, "If-Match", `"1"`)
	off.decode(t, &o)
	if off.code != 200 || off.header.Get("ETag") != `"2"` || o.Enabled || o.Status != OfferDisabled || o.Label != "School AI (quick)" ||
		*o.KeyStatus != KeyTested || *o.UpdatedBy != ken {
		t.Fatalf("turned off: %d %s", off.code, off.body)
	}
	h.call("GET", "models", h.yuki, "").decode(t, &m)
	if len(m.SchoolKey.Offers) != 2 {
		t.Errorf("GET /models with the offer off: %+v", m.SchoolKey.Offers)
	}
	h.call("GET", "agents/"+v.ID, h.yuki, "").decode(t, &agent)
	if agent.Model.School == nil || agent.Model.School.Offered {
		t.Errorf("the agent's view with its offer off: %+v", agent.Model.School)
	}
	cfg, _, err = registry.Build(ctx, fh.YAML(), h.st, registry.Options{CoreBaseURL: h.srv.URL})
	h.ok(err)
	if len(cfg.Agents) != 0 || len(cfg.Rejected) != 1 || cfg.Rejected[0].Reason != store.ReasonOfferWithdrawn {
		t.Errorf("the build with the offer off: %+v %+v", cfg.Agents, cfg.Rejected)
	}
	h.ok(h.st.SetAgentState(ctx, store.AgentState{AgentID: v.ID, State: store.AgentError, Reason: store.ReasonOfferWithdrawn,
		Detail: cfg.Rejected[0].Detail(), ConfigVersion: 2}))
	h.call("GET", "agents/"+v.ID, h.yuki, "").decode(t, &agent)
	if agent.Status != StatusError || agent.Problem == nil || agent.Problem.Reason != store.ReasonOfferWithdrawn {
		t.Errorf("the agent's status: %s %+v", agent.Status, agent.Problem)
	}
	stale := h.admin("PATCH", "admin/school-plan/offers/fast", `{"enabled":true}`, "If-Match", `"1"`)
	if e := wantRefused(t, stale, 412, CodeVersionMismatch, ReasonVersionMismatch); e.Details["current_version"] != float64(2) {
		t.Errorf("stale: %s", stale.body)
	}

	// A new key: tried, sealed, the one before destroyed.
	rekeyed := h.admin("PATCH", "admin/school-plan/offers/fast", `{"enabled":true,"key":"`+fastKey2+`"}`)
	rekeyed.decode(t, &o)
	if rekeyed.code != 200 || !o.Enabled || *o.KeyHint != "sk-…tsrq" || *o.KeyStatus != KeyTested || *o.Version != 3 {
		t.Fatalf("a new key: %d %s", rekeyed.code, rekeyed.body)
	}
	if _, err := h.st.Secret(ctx, row.KeySecretID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the key before: %v", err)
	}
	row, err = h.st.SchoolOffer(ctx, "fast")
	h.ok(err)
	// Another model of the provider's: the key is kept, untried with it.
	other := h.admin("PATCH", "admin/school-plan/offers/fast", `{"model":"deepseek-reasoner","max_output_tokens":null}`)
	other.decode(t, &o)
	if other.code != 200 || o.Model != "deepseek-reasoner" || *o.KeyStatus != KeyUntested || o.MaxOutputTokens != nil || o.Priced ||
		*o.KeyHint != "sk-…tsrq" {
		t.Errorf("another model: %d %s", other.code, other.body)
	}
	if again, _ := h.st.SchoolOffer(ctx, "fast"); again.KeySecretID != row.KeySecretID {
		t.Error("another model replaced the key")
	}
	// Nothing changed: nothing written.
	same := h.admin("PATCH", "admin/school-plan/offers/fast", `{"model":"deepseek-reasoner","enabled":true}`)
	if same.code != 200 || same.header.Get("ETag") != `"4"` || len(h.events("school_offer.update")) != 4 {
		t.Errorf("a patch of nothing: %d %s, %d events", same.code, same.body, len(h.events("school_offer.update")))
	}

	// Deleted, with its key; the agent on it is counted.
	gone := h.admin("DELETE", "admin/school-plan/offers/fast", "", "If-Match", `"4"`)
	var del OfferDeleted
	gone.decode(t, &del)
	if gone.code != 200 || del.Deleted.ID != "fast" || del.Agents != 1 {
		t.Fatalf("deleted: %d %s", gone.code, gone.body)
	}
	if _, err := h.st.SchoolOffer(ctx, "fast"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the offer after: %v", err)
	}
	if _, err := h.st.Secret(ctx, row.KeySecretID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("its key after: %v", err)
	}
	wantRefused(t, h.admin("DELETE", "admin/school-plan/offers/fast", ""), 404, CodeNotFound, ReasonOfferNotFound)
	if ev := h.events("school_offer.delete"); len(ev) != 2 || ev[0].Outcome != "ok" || !strings.Contains(string(ev[0].Detail), `"agents":1`) ||
		ev[1].Outcome != ReasonOfferNotFound {
		t.Errorf("the deletes' audit: %+v", ev)
	}
	h.noSchoolKeys(created, got, models, onFast, off, stale, rekeyed, other, same, gone)
	h.noSecrets(created, rekeyed)
}

// The administrators' routes of the plan refuse anyone else (not_admin),
// audited for the writes; and refuse runtime.yaml's offers, which are
// the operator's alone (offer_read_only), and an offer that is not there.
func TestSchoolPlanAdminOnly(t *testing.T) {
	h, p, _ := newModelWorld(t, schoolRuntime())
	for _, c := range []struct{ method, path, body string }{
		{"GET", "admin/school-plan", ""}, {"GET", "admin/school-plan/offers/standard", ""},
		{"POST", "admin/school-plan/offers", fastOffer}, {"PATCH", "admin/school-plan/offers/standard", `{"enabled":false}`},
		{"DELETE", "admin/school-plan/offers/standard", ""},
		{"PUT", "admin/school-plan/quotas", `{"per_owner_day":1,"per_asker_day":1,"per_day":null}`}, {"DELETE", "admin/school-plan/quotas", ""},
	} {
		for _, role := range []string{"", "instructor"} {
			wantRefused(t, h.adminCall(t, role, c.method, c.path, c.body), 403, CodeForbidden, ReasonNotAdmin)
		}
	}
	for _, action := range []string{"school_offer.create", "school_offer.update", "school_offer.delete", "school_quotas.update", "school_quotas.reset"} {
		if ev := h.events(action); len(ev) != 2 || ev[0].Outcome != ReasonNotAdmin {
			t.Errorf("%s: %+v", action, ev)
		}
	}
	p.mu.Lock()
	if len(p.seen) != 0 {
		t.Errorf("a refused request tried a key: %d calls", len(p.seen))
	}
	p.mu.Unlock()

	wantRefused(t, h.admin("PATCH", "admin/school-plan/offers/standard", `{"enabled":false}`), 403, CodeForbidden, ReasonOfferReadOnly)
	wantRefused(t, h.admin("DELETE", "admin/school-plan/offers/standard", ""), 403, CodeForbidden, ReasonOfferReadOnly)
	wantRefused(t, h.admin("PATCH", "admin/school-plan/offers/premium", `{"enabled":false}`), 404, CodeNotFound, ReasonOfferNotFound)
	wantRefused(t, h.admin("GET", "admin/school-plan/offers/premium", ""), 404, CodeNotFound, ReasonOfferNotFound)
	if a := h.admin("GET", "admin/school-plan/offers/standard", ""); a.code != 200 || a.header.Get("ETag") != "" || !strings.Contains(a.body, `"source":"config"`) {
		t.Errorf("GET runtime.yaml's offer: %d %s", a.code, a.body)
	}
}

// POST refuses an offer that is not one, at its pointer, and keeps
// nothing of it: its members' own rules, a key that is none or a Core
// token, a provider or endpoint not offered, an id the plan has already
// (runtime.yaml's or the site's), a model the school's lists do not allow
// or, with a quota in dollars, the price table does not price; and a key
// the provider refuses, or does not answer for, with what it said; the
// trial skipped, the offer is kept, its key untried.
func TestSchoolPlanAdminRefuses(t *testing.T) {
	rt := schoolRuntime()
	rt.DeniedModels = []string{"*:*:*-preview"}
	h, p, _ := newModelWorld(t, rt)
	ctx := context.Background()
	body := func(edit string) string { return strings.Replace(fastOffer, `"id":"fast",`, edit, 1) }
	for _, tc := range []struct {
		name, body          string
		status              int
		code, reason, field string
	}{
		{"no id", body(``), 400, CodeInvalidArgument, ReasonMissingField, "/id"},
		{"an id of no offer's shape", body(`"id":"fast plan",`), 400, CodeInvalidArgument, ReasonInvalidField, "/id"},
		{"no label", strings.Replace(fastOffer, `"label":"School AI (fast)",`, "", 1), 400, CodeInvalidArgument, ReasonMissingField, "/label"},
		{"a label of two lines", strings.Replace(fastOffer, `School AI (fast)`, `School\nAI`, 1), 400, CodeInvalidArgument, ReasonInvalidField, "/label"},
		{"a label too long", strings.Replace(fastOffer, `School AI (fast)`, strings.Repeat("L", 81), 1), 400, CodeInvalidArgument, ReasonInvalidField, "/label"},
		{"no provider", strings.Replace(fastOffer, `"provider":"deepseek",`, "", 1), 400, CodeInvalidArgument, ReasonMissingField, "/provider"},
		{"no model", strings.Replace(fastOffer, `"model":"deepseek-chat",`, "", 1), 400, CodeInvalidArgument, ReasonMissingField, "/model"},
		{"a model of no id's shape", strings.Replace(fastOffer, `deepseek-chat`, `deep seek`, 1), 400, CodeInvalidArgument, ReasonInvalidField, "/model"},
		{"an output bound out of bounds", strings.Replace(fastOffer, `1200`, `99999`, 1), 400, CodeInvalidArgument, ReasonInvalidField, "/max_output_tokens"},
		{"an effort of none", strings.Replace(fastOffer, `"low"`, `"max"`, 1), 400, CodeInvalidArgument, ReasonInvalidField, "/reasoning_effort"},
		{"no key", strings.Replace(fastOffer, `,"key":"`+fastKey+`"`, "", 1), 400, CodeInvalidArgument, ReasonMissingField, "/key"},
		{"a Core token as its key", strings.Replace(fastOffer, fastKey, "ais_k7v2m4qhx3ab_0123456789abcdefghijklmnopqrstuvwxyzABCDEF", 1), 400,
			CodeInvalidArgument, ReasonKeyMalformed, "/key"},
		{"a provider not offered", strings.Replace(fastOffer, `"deepseek",`, `"ollama",`, 1), 400, CodeInvalidArgument, ReasonUnknownProvider, "/provider"},
		{"an endpoint not the provider's", body(`"id":"fast","endpoint":"eu",`), 400, CodeInvalidArgument, ReasonInvalidField, "/endpoint"},
		{"an adapter not the provider's", body(`"id":"fast","adapter":"anthropic",`), 400, CodeInvalidArgument, ReasonAdapterNotOffered, "/adapter"},
		{"a URL of its own", body(`"id":"fast","base_url":"http://10.0.0.1/v1",`), 400, CodeInvalidArgument, ReasonUnknownField, "/base_url"},
		{"runtime.yaml's id", body(`"id":"standard",`), 409, CodeConflict, ReasonOfferExists, "/id"},
		{"a model denied", strings.Replace(fastOffer, `deepseek-chat`, `deepseek-v4-preview`, 1), 422, CodeFailedPrecondition, ReasonModelDenied, "/model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := wantRefused(t, h.admin("POST", "admin/school-plan/offers", tc.body), tc.status, tc.code, tc.reason)
			if e.Details["field"] != tc.field {
				t.Errorf("field %v, want %s", e.Details["field"], tc.field)
			}
		})
	}
	p.mu.Lock()
	if len(p.seen) != 0 {
		t.Errorf("a refused offer's key was tried: %d calls", len(p.seen))
	}
	p.mu.Unlock()

	// The provider refuses the key, or cannot be reached.
	p.set(http.StatusUnauthorized, nil)
	e := wantRefused(t, h.admin("POST", "admin/school-plan/offers", fastOffer), 422, CodeFailedPrecondition, ReasonKeyTestFailed)
	if e.Details["result"] != "key_refused" || e.Details["http_status"] != float64(401) || e.Details["field"] != "/key" {
		t.Errorf("a key refused: %+v", e.Details)
	}
	p.set(0, errors.New("dial tcp: no route to host"))
	e = wantRefused(t, h.admin("POST", "admin/school-plan/offers", fastOffer), 422, CodeFailedPrecondition, ReasonKeyTestFailed)
	if e.Details["result"] != "unreachable" || e.Details["http_status"] != nil {
		t.Errorf("a provider not reached: %+v", e.Details)
	}
	if offers, _ := h.st.SchoolOffers(ctx); len(offers) != 0 {
		t.Errorf("a refused offer was kept: %+v", offers)
	}
	if secrets, _ := h.st.ListSecrets(ctx, "", 100); len(secrets) != 0 {
		t.Errorf("a refused offer's key was kept: %d secrets", len(secrets))
	}
	if ev := h.events("school_offer.create"); len(ev) == 0 || ev[len(ev)-1].Outcome != ReasonKeyTestFailed ||
		!strings.Contains(string(ev[len(ev)-1].Detail), `"key_test":"unreachable"`) {
		t.Errorf("the refusal's audit: %+v", ev[len(ev)-1])
	}

	// The trial skipped: kept, its key untried; and the same id again is
	// the site's.
	p.mu.Lock()
	tried := len(p.seen)
	p.mu.Unlock()
	skipped := h.admin("POST", "admin/school-plan/offers", body(`"id":"fast","skip_key_test":true,"enabled":false,`))
	var o PlanOffer
	skipped.decode(t, &o)
	if skipped.code != 201 || *o.KeyStatus != KeyUntested || o.Enabled || o.Status != OfferDisabled {
		t.Fatalf("the trial skipped: %d %s", skipped.code, skipped.body)
	}
	p.mu.Lock()
	if len(p.seen) != tried {
		t.Errorf("a trial skipped tried the key")
	}
	p.mu.Unlock()
	e = wantRefused(t, h.admin("POST", "admin/school-plan/offers", fastOffer), 409, CodeConflict, ReasonOfferExists)
	if e.Details["source"] != SourceSite {
		t.Errorf("the site's id again: %+v", e.Details)
	}
	// Another provider's model needs a key of that provider's.
	e = wantRefused(t, h.admin("PATCH", "admin/school-plan/offers/fast", `{"provider":"openai","model":"gpt-4.1-mini"}`), 422,
		CodeFailedPrecondition, ReasonKeyRequired)
	if e.Details["field"] != "/key" {
		t.Errorf("another provider: %+v", e.Details)
	}
	for body, field := range map[string]string{
		`{"enabled":"yes"}`: "/enabled", `{"label":""}`: "/label", `{"key":""}`: "/key", `{"model":null}`: "/model",
		`{"max_output_tokens":"many"}`: "/max_output_tokens", `{"base_url":"https://x.example"}`: "/base_url",
	} {
		if a := h.admin("PATCH", "admin/school-plan/offers/fast", body); a.code != 400 || a.refusal(t).Details["field"] != field {
			t.Errorf("PATCH %s: %d %s", body, a.code, a.body)
		}
	}
	h.noSchoolKeys(skipped)
}

// With a quota of the plan's in dollars, an offer the price table does
// not price is refused: it could not be held to it.
func TestSchoolPlanAdminNeedsAPrice(t *testing.T) {
	rt := schoolRuntime()
	two := 2.0
	rt.School.PerDay.USD = &two
	h, _, _ := newModelWorld(t, rt)
	e := wantRefused(t, h.admin("POST", "admin/school-plan/offers", strings.Replace(fastOffer, "deepseek-chat", "deepseek-reasoner", 1)), 422,
		CodeFailedPrecondition, ReasonOfferNotPriced)
	if e.Details["field"] != "/model" {
		t.Errorf("field %v", e.Details["field"])
	}
	if a := h.admin("POST", "admin/school-plan/offers", fastOffer); a.code != 201 {
		t.Errorf("a priced offer: %d %s", a.code, a.body)
	}
}

// An offer's key trials are held to the allowance of keys/test.
func TestSchoolPlanAdminKeyTrialsAreLimited(t *testing.T) {
	h, _, _ := newModelWorld(t, schoolRuntime())
	h.s.keyTest.reset(Rate{PerMinute: 6, Burst: 1})
	if a := h.admin("POST", "admin/school-plan/offers", fastOffer); a.code != 201 {
		t.Fatalf("the first: %d %s", a.code, a.body)
	}
	a := h.admin("PATCH", "admin/school-plan/offers/fast", `{"key":"`+fastKey2+`"}`)
	wantRefused(t, a, 429, CodeRateLimited, ReasonRateLimited)
	if a.header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if a := h.admin("PATCH", "admin/school-plan/offers/fast", `{"key":"`+fastKey2+`","skip_key_test":true}`); a.code != 200 {
		t.Errorf("a key whose trial is skipped: %d %s", a.code, a.body)
	}
}

// PUT /admin/school-plan/quotas sets the plan's quotas in place of
// runtime.yaml's, per_day null for no ceiling: owners' views, GET /models
// and the usage say them, and the registry puts them in force; DELETE
// takes runtime.yaml's again. Each is audited, but a DELETE with nothing
// set, which writes nothing.
func TestSchoolPlanQuotas(t *testing.T) {
	rt := schoolRuntime()
	h, _, fh := newModelWorld(t, rt)
	ctx := context.Background()
	var plan SchoolPlan
	put := h.admin("PUT", "admin/school-plan/quotas", `{"per_owner_day":50,"per_asker_day":5,"per_day":400}`)
	put.decode(t, &plan)
	four := 400
	if put.code != 200 || plan.Quotas != (SchoolPlanLimits{PerOwnerDay: 50, PerAskerDay: 5, PerDay: plan.Quotas.PerDay}) || plan.Quotas.PerDay == nil ||
		*plan.Quotas.PerDay != four || plan.QuotaDefaults != (SchoolPlanLimits{PerOwnerDay: 3, PerAskerDay: 20}) || !plan.QuotasSet ||
		plan.QuotasUpdatedBy == nil || *plan.QuotasUpdatedBy != ken || len(plan.Offers) != 2 {
		t.Fatalf("%d %s", put.code, put.body)
	}
	var m Models
	h.call("GET", "models", h.yuki, "").decode(t, &m)
	if m.SchoolKey.Limits != (SchoolLimits{PerOwnerDay: 50, PerAskerDay: 5}) {
		t.Errorf("GET /models: %+v", m.SchoolKey.Limits)
	}
	var usage SchoolPlanUsage
	h.admin("GET", "admin/school-plan/usage", "").decode(t, &usage)
	if usage.Limits.PerOwnerDay != 50 || usage.Limits.PerDay == nil || *usage.Limits.PerDay != 400 {
		t.Errorf("the usage's limits: %+v", usage.Limits)
	}
	cfg, _, err := registry.Build(ctx, fh.YAML(), h.st, registry.Options{CoreBaseURL: h.srv.URL})
	h.ok(err)
	if sc := cfg.Runtime.School; *sc.OwnerQuota().Answers != 50 || *sc.AskerQuota().Answers != 5 || *sc.PerDay.Answers != 400 {
		t.Errorf("the plan in force: %+v", sc)
	}
	if ev := h.events("school_quotas.update"); len(ev) != 1 || ev[0].TargetID != store.SettingSchoolQuotas ||
		string(ev[0].Detail) != `{"per_asker_day":5,"per_asker_day_usd":null,"per_day":400,"per_day_usd":null,"per_owner_day":50,"per_owner_day_usd":null}` {
		t.Errorf("the audit: %+v", ev)
	}

	h.admin("PUT", "admin/school-plan/quotas", `{"per_owner_day":50,"per_asker_day":5,"per_day":null}`).decode(t, &plan)
	if plan.Quotas.PerDay != nil {
		t.Errorf("per_day null: %+v", plan.Quotas)
	}
	for _, tc := range []struct{ body, reason, field string }{
		{`{"per_owner_day":50,"per_asker_day":5}`, ReasonMissingField, "/per_day"},
		{`{"per_asker_day":5,"per_day":null}`, ReasonMissingField, "/per_owner_day"},
		{`{"per_owner_day":0,"per_asker_day":5,"per_day":null}`, ReasonInvalidField, "/per_owner_day"},
		{`{"per_owner_day":null,"per_asker_day":5,"per_day":null}`, ReasonInvalidField, "/per_owner_day"},
		{`{"per_owner_day":1.5,"per_asker_day":5,"per_day":null}`, ReasonInvalidField, "/per_owner_day"},
		{`{"per_owner_day":5,"per_asker_day":"5","per_day":null}`, ReasonInvalidField, "/per_asker_day"},
		{`{"per_owner_day":5,"per_asker_day":5,"per_day":1000001}`, ReasonInvalidField, "/per_day"},
		{`{"per_owner_day":5,"per_asker_day":5,"per_day":null,"per_week":2}`, ReasonUnknownField, "/per_week"},
		{`{"per_owner_day":5,"per_asker_day":5,"per_day":null,"per_owner_day_usd":0}`, ReasonInvalidField, "/per_owner_day_usd"},
		{`{"per_owner_day":5,"per_asker_day":5,"per_day":null,"per_asker_day_usd":"-1"}`, ReasonInvalidField, "/per_asker_day_usd"},
		{`{"per_owner_day":5,"per_asker_day":5,"per_day":null,"per_day_usd":"0.0000001"}`, ReasonInvalidField, "/per_day_usd"},
		{`{"per_owner_day":5,"per_asker_day":5,"per_day":null,"per_day_usd":1e3}`, ReasonInvalidField, "/per_day_usd"},
		{`{"per_owner_day":5,"per_asker_day":5,"per_day":null,"per_day_usd":"1000000.01"}`, ReasonInvalidField, "/per_day_usd"},
		{`{"per_owner_day":5,"per_asker_day":5,"per_day":null,"per_day_usd":"two"}`, ReasonInvalidField, "/per_day_usd"},
		{`{"per_owner_day":5,"per_asker_day":5,"per_day":null,"per_day_usd":true}`, ReasonInvalidField, "/per_day_usd"},
	} {
		e := wantRefused(t, h.admin("PUT", "admin/school-plan/quotas", tc.body), 400, CodeInvalidArgument, tc.reason)
		if e.Details["field"] != tc.field {
			t.Errorf("%s: field %v", tc.body, e.Details["field"])
		}
	}

	reset := h.admin("DELETE", "admin/school-plan/quotas", "")
	reset.decode(t, &plan)
	if reset.code != 200 || plan.QuotasSet || plan.Quotas != plan.QuotaDefaults || plan.QuotasUpdatedAt != nil {
		t.Errorf("reset: %d %s", reset.code, reset.body)
	}
	if a := h.admin("DELETE", "admin/school-plan/quotas", ""); a.code != 200 {
		t.Errorf("reset again: %d %s", a.code, a.body)
	}
	if ev := h.events("school_quotas.reset"); len(ev) != 1 || ev[0].Outcome != "ok" {
		t.Errorf("the resets' audit: %+v", ev)
	}
	h.call("GET", "models", h.yuki, "").decode(t, &m)
	if m.SchoolKey.Limits != (SchoolLimits{PerOwnerDay: 3, PerAskerDay: config.DefaultPerAskerDay}) {
		t.Errorf("GET /models after the reset: %+v", m.SchoolKey.Limits)
	}
}
