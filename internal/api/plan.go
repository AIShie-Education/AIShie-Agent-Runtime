package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/probe"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// The school's plan as the runtime's administrators manage it (D8, D4;
// docs/design.md §11.5): GET /admin/school-plan, the offers of
// runtime.yaml (source config, which only the operator changes) and of
// the site (source site), and the quotas; POST, PATCH and DELETE of the
// site's offers under /admin/school-plan/offers; PUT and DELETE of the
// quotas. An offer is made as an owner's own model is (OwnModelChoice), at
// a provider's own endpoint, with the school's key, which is tried with
// the model before it is kept (unless skip_key_test), sealed under the
// tenant school, and never shown again but as its hint. What changes is
// kept in the store, and every worker puts it in force as it reads the
// registry again, without a restart.

// Where an offer of the plan comes from.
const (
	SourceConfig = "config"
	SourceSite   = "site"
)

// An offer's status in the plan: offered, or why not.
const (
	OfferOffered  = "offered"
	OfferDisabled = "disabled"
)

// The key of a site's offer: tried with its model when it was given, or
// not (skip_key_test, or its model changed since).
const (
	KeyTested   = "tested"
	KeyUntested = "untested"
)

// SchoolPlan is GET /admin/school-plan's answer, and PUT and DELETE
// /admin/school-plan/quotas': every offer, runtime.yaml's first as it
// lists them, then the site's by id; the quotas in force in answers a UTC
// day (per_day null for no ceiling), runtime.yaml's (with the built-in
// defaults) beside them, and whether the site sets them, when and by whom.
type SchoolPlan struct {
	Offers          []PlanOffer      `json:"offers"`
	Quotas          SchoolPlanLimits `json:"quotas"`
	QuotaDefaults   SchoolPlanLimits `json:"quota_defaults"`
	QuotasSet       bool             `json:"quotas_set"`
	QuotasUpdatedAt *time.Time       `json:"quotas_updated_at"`
	QuotasUpdatedBy *string          `json:"quotas_updated_by"`
}

// PlanOffer is one offer as the administrators read it: its id and label;
// its model as an owner's is chosen (provider, adapter, model, and the
// endpoint, resource or region the provider's offer takes, null where it
// takes none or it is not one of the provider's), with base_url, null for
// the adapter's own; whether it is turned on, and its status in the plan
// (offered, disabled, id_taken where an offer of runtime.yaml's has its
// id, model_not_allowed where runtime.yaml's model lists do not allow it);
// whether the price table prices it today; how many hosted agents are on
// it; and, of a site's offer, its key's hint and whether the key was
// tried with its model, its version (the ETag), and when and by whom it
// was made and last changed. Of runtime.yaml's, those are null, and
// enabled is true: only the operator changes them.
type PlanOffer struct {
	ID              string     `json:"id"`
	Source          string     `json:"source"`
	Label           string     `json:"label"`
	Provider        string     `json:"provider"`
	Adapter         string     `json:"adapter"`
	Model           string     `json:"model"`
	Endpoint        *string    `json:"endpoint"`
	Resource        *string    `json:"resource"`
	Region          *string    `json:"region"`
	BaseURL         *string    `json:"base_url"`
	MaxOutputTokens *int       `json:"max_output_tokens"`
	ReasoningEffort *string    `json:"reasoning_effort"`
	Enabled         bool       `json:"enabled"`
	Status          string     `json:"status"`
	Priced          bool       `json:"priced"`
	Agents          int        `json:"agents"`
	KeyHint         *string    `json:"key_hint"`
	KeyStatus       *string    `json:"key_status"`
	Version         *int       `json:"version"`
	CreatedAt       *time.Time `json:"created_at"`
	CreatedBy       *string    `json:"created_by"`
	UpdatedAt       *time.Time `json:"updated_at"`
	UpdatedBy       *string    `json:"updated_by"`
}

// OfferDeleted is DELETE /admin/school-plan/offers/{id}'s answer: the
// offer deleted, and how many hosted agents were on it, which now run on
// their owners' own models behind it, or, with none, are not run
// (offer_withdrawn).
type OfferDeleted struct {
	Deleted struct {
		ID string `json:"id"`
	} `json:"deleted"`
	Agents int `json:"agents"`
}

// Bounds of the quotas an administrator sets, in answers a UTC day.
const (
	minQuota = 1
	maxQuota = 1_000_000
)

// modelView is where a model is, as an owner's choice says it: the
// provider's endpoint choice or Azure resource its base_url is, and its
// region; nil for each it has not.
func modelView(provider, baseURL, region string) (endpoint, resource, reg, base *string) {
	if offer, ok := registry.OfferOf(provider); ok {
		if e, r, ok := offer.ChoiceOf(baseURL); ok {
			endpoint, resource = strPtr(e), strPtr(r)
		}
	}
	return endpoint, resource, strPtr(region), strPtr(baseURL)
}

// agentsOn counts the hosted agents on each offer of the plan, by the
// offer's id their rows name.
func (s *Server) agentsOn(ctx context.Context) (map[string]int, error) {
	rows, err := s.o.Store.HostedAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, r := range rows {
		if _, school := modelSlots(r.Settings); school != nil {
			out[school.Offer]++
		}
	}
	return out, nil
}

// configOfferView is an offer of runtime.yaml's as the administrators
// read it.
func (s *Server) configOfferView(o config.SchoolOffer, agents int, now time.Time) PlanOffer {
	m := o.AsModel()
	v := PlanOffer{ID: o.ID, Source: SourceConfig, Label: o.Label, Provider: m.EffectiveProvider(), Adapter: o.Adapter, Model: o.Model,
		Enabled: true, Status: OfferOffered, Agents: agents, ReasoningEffort: strPtr(o.Reasoning.Effort)}
	v.Endpoint, v.Resource, v.Region, v.BaseURL = modelView(v.Provider, o.BaseURL, o.Region)
	if n := o.Params.MaxOutputTokens; n > 0 {
		v.MaxOutputTokens = &n
	}
	_, v.Priced = s.prices().Lookup(v.Provider, v.Model, now)
	return v
}

// siteOfferView is the site's offer o as the administrators read it, its
// status in the plan of rt.
func (s *Server) siteOfferView(o store.SchoolOffer, rt config.Runtime, agents int, now time.Time) PlanOffer {
	v := PlanOffer{ID: o.ID, Source: SourceSite, Label: o.Label, Provider: o.Provider, Adapter: o.Adapter, Model: o.Model,
		Enabled: o.Enabled, Status: OfferOffered, Agents: agents, ReasoningEffort: strPtr(o.ReasoningEffort), KeyHint: strPtr(o.KeyHint)}
	v.Endpoint, v.Resource, v.Region, v.BaseURL = modelView(o.Provider, o.BaseURL, o.Region)
	if n := o.MaxOutputTokens; n > 0 {
		v.MaxOutputTokens = &n
	}
	switch why := rt.Withheld(registry.SiteOffer(o)); {
	case !o.Enabled:
		v.Status = OfferDisabled
	case why != "":
		v.Status = why
	}
	_, v.Priced = s.prices().Lookup(v.Provider, v.Model, now)
	status := KeyUntested
	if o.KeyTested {
		status = KeyTested
	}
	version, created, updated := o.Version, o.CreatedAt.UTC(), o.UpdatedAt.UTC()
	v.KeyStatus, v.Version, v.CreatedAt, v.CreatedBy, v.UpdatedAt, v.UpdatedBy = &status, &version, &created, strPtr(o.CreatedBy), &updated,
		strPtr(o.UpdatedBy)
	return v
}

// schoolPlan is the plan as GET /admin/school-plan answers it.
func (s *Server) schoolPlan(ctx context.Context) (*SchoolPlan, error) {
	eff, err := s.effective(ctx)
	if err != nil {
		return nil, err
	}
	offers, err := s.o.Store.SchoolOffers(ctx)
	if err != nil {
		return nil, err
	}
	quotas, err := s.siteSetting(ctx, store.SettingSchoolQuotas)
	if err != nil {
		return nil, err
	}
	agents, err := s.agentsOn(ctx)
	if err != nil {
		return nil, err
	}
	now := s.o.Now()
	out := &SchoolPlan{Offers: []PlanOffer{}, Quotas: limitsOf(eff.Runtime.School), QuotaDefaults: limitsOf(s.yaml().Runtime.School),
		QuotasSet: eff.Runtime.Site.Quotas != nil}
	for _, o := range s.yaml().Runtime.School.Offers {
		out.Offers = append(out.Offers, s.configOfferView(o, agents[o.ID], now))
	}
	for _, o := range offers {
		out.Offers = append(out.Offers, s.siteOfferView(o, eff.Runtime, agents[o.ID], now))
	}
	if quotas != nil {
		at := quotas.UpdatedAt.UTC()
		out.QuotasUpdatedAt, out.QuotasUpdatedBy = &at, strPtr(quotas.UpdatedBy)
	}
	return out, nil
}

// limitsOf are sc's quotas in answers.
func limitsOf(sc config.School) SchoolPlanLimits {
	return SchoolPlanLimits{PerOwnerDay: *sc.OwnerQuota().Answers, PerAskerDay: *sc.AskerQuota().Answers, PerDay: sc.PerDay.Answers}
}

// writePlan answers the plan, or that the store cannot be read.
func (s *Server) writePlan(ctx context.Context, w http.ResponseWriter) {
	plan, err := s.schoolPlan(ctx)
	if err != nil {
		s.storeUnavailable(w, "the school's plan", err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) getSchoolPlan(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	s.writePlan(ctx, w)
}

// writeOffer answers the site's offer o, with its ETag.
func (s *Server) writeOffer(ctx context.Context, w http.ResponseWriter, status int, o *store.SchoolOffer) {
	eff, err := s.effective(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	agents, err := s.agentsOn(ctx)
	if err != nil {
		s.storeUnavailable(w, "the hosted agents", err)
		return
	}
	w.Header().Set("ETag", etag(o.Version))
	writeJSON(w, status, s.siteOfferView(*o, eff.Runtime, agents[o.ID], s.o.Now()))
}

// getOffer is GET /admin/school-plan/offers/{id}: an offer as GET
// /admin/school-plan lists it, a site's with its ETag. The site's is the
// one PATCH and DELETE change, and is given where runtime.yaml has an
// offer of its id too.
func (s *Server) getOffer(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	id := r.PathValue("id")
	o, err := s.o.Store.SchoolOffer(ctx, id)
	switch {
	case err == nil:
		s.writeOffer(ctx, w, http.StatusOK, o)
		return
	case !errors.Is(err, store.ErrNotFound):
		s.storeUnavailable(w, "an offer", err)
		return
	}
	yo, ok := s.yaml().Runtime.School.OfferOf(id)
	if !ok {
		WriteError(w, Error{Code: CodeNotFound, Reason: ReasonOfferNotFound, Message: "the plan has no offer of this id"})
		return
	}
	agents, err := s.agentsOn(ctx)
	if err != nil {
		s.storeUnavailable(w, "the hosted agents", err)
		return
	}
	writeJSON(w, http.StatusOK, s.configOfferView(yo, agents[id], s.o.Now()))
}

// offerRequest is POST /admin/school-plan/offers' body: the offer's id,
// its label, its model as an owner's is chosen, whether it is turned on
// (true unless given), and the school's key, tried with the model before
// it is kept unless skip_key_test.
type offerRequest struct {
	OwnModelChoice
	ID          string `json:"id"`
	Label       string `json:"label"`
	Enabled     *bool  `json:"enabled"`
	Key         string `json:"key"`
	SkipKeyTest bool   `json:"skip_key_test"`
}

// labelError refuses a label that is not one line of 1 to
// config.MaxOfferLabel characters.
func labelError(label, field string) *Error {
	switch n := utf8.RuneCountInString(label); {
	case strings.TrimSpace(label) == "":
		return fieldError(CodeInvalidArgument, ReasonMissingField, field, "a label is required: what owners are shown")
	case n > config.MaxOfferLabel || strings.ContainsAny(label, "\r\n") || !utf8.ValidString(label):
		return fieldError(CodeInvalidArgument, ReasonInvalidField, field, "a label is one line of at most 80 characters")
	}
	return nil
}

// offerOf is the section sec as the site's offer id, labelled label.
func offerOf(id, label string, sec *modelSection) store.SchoolOffer {
	o := store.SchoolOffer{ID: id, Label: label, Adapter: sec.Adapter, Provider: sec.Provider, Model: sec.Model, BaseURL: sec.BaseURL,
		Region: sec.Region}
	if sec.Params != nil {
		o.MaxOutputTokens = sec.Params.MaxOutputTokens
	}
	if sec.Reasoning != nil {
		o.ReasoningEffort = sec.Reasoning.Effort
	}
	return o
}

// allowedOffer refuses an offer the plan in force eff would not offer for
// its model (runtime.denied_models, runtime.allowed_models), or could not
// hold to its quotas in dollars, the price table not pricing it. The
// field is the model's.
func (s *Server) allowedOffer(eff *config.Config, o store.SchoolOffer, field string) *Error {
	if eff.Runtime.Withheld(registry.SiteOffer(o)) == config.WithheldModelNotAllowed {
		return &Error{Code: CodeFailedPrecondition, Reason: ReasonModelDenied,
			Message: "the school's model lists do not allow this model on the school's key (runtime.yaml's allowed_models, denied_models)",
			Details: map[string]any{"field": field}}
	}
	if _, priced := s.prices().Lookup(o.Provider, o.Model, s.o.Now()); eff.Runtime.School.USD() && !priced {
		return &Error{Code: CodeFailedPrecondition, Reason: ReasonOfferNotPriced,
			Message: "the plan has a quota in dollars, and the price table has no price for this model", Details: map[string]any{"field": field}}
	}
	return nil
}

// tryOfferKey tries key with the offer's model, as keys/test does, within
// the caller's allowance of key tests: a trial the key does not pass (a
// key refused, a model not found, a provider not reached) is refused, 422
// key_test_failed, with what the provider said. It reports whether the
// request may go on, having answered when not.
func (s *Server) tryOfferKey(ctx context.Context, w http.ResponseWriter, c *Caller, au *auditing, sec *modelSection, key string) bool {
	now := s.o.Now()
	if ok, wait := s.keyTest.take(c.ActorID, now); !ok {
		writeRateLimited(w, wait)
		return false
	}
	if ok, wait := s.keyDay.take(c.ActorID, now); !ok {
		writeRateLimited(w, wait)
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, probe.TrialTimeout+5*time.Second)
	defer cancel()
	tr, err := probe.TryModel(ctx, sec.config(), key, s.modelHTTP, s.o.NewAdapter)
	if err != nil {
		s.o.Log.Warn("an offer's key: the model's adapter could not be built", "provider", sec.Provider, "adapter", sec.Adapter)
		tr = probe.Trial{Result: probe.ResultUnreachable}
	}
	au.detail["key_test"] = tr.Result
	if tr.Result == probe.ResultOK || tr.Result == probe.ResultKeyAccepted {
		return true
	}
	details := map[string]any{"field": "/key", "result": tr.Result, "http_status": nil, "provider_code": nil}
	if tr.HTTPStatus != 0 {
		details["http_status"] = tr.HTTPStatus
	}
	if tr.ProviderCode != "" {
		details["provider_code"] = tr.ProviderCode
	}
	WriteError(w, Error{Code: CodeFailedPrecondition, Reason: ReasonKeyTestFailed,
		Message: "the key did not pass a trial of the model (" + tr.Result + "): give another, or skip_key_test", Details: details})
	return false
}

// sealOfferKey seals key as the school's, for the offer, answering when
// it cannot be.
func (s *Server) sealOfferKey(ctx context.Context, w http.ResponseWriter, c *Caller, key string) (*store.Secret, bool) {
	if s.o.Vault == nil {
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime cannot keep keys now"})
		return nil, false
	}
	sec, err := s.o.Vault.Seal(ctx, store.Secret{ID: vault.NewSecretID(), TenantID: store.SchoolTenantID, Kind: store.SecretModelKey,
		CreatedBy: c.ActorID}, key)
	if err != nil {
		s.o.Log.Error("a school's key could not be sealed", "err", err)
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime could not keep the key"})
		return nil, false
	}
	return &sec, true
}

// errOfferExists is an offer made with an id the plan has.
func errOfferExists(source string) Error {
	return Error{Code: CodeConflict, Reason: ReasonOfferExists, Message: "the plan has an offer of this id",
		Details: map[string]any{"field": "/id", "source": source}}
}

// createOffer is POST /admin/school-plan/offers.
func (s *Server) createOffer(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	var req offerRequest
	if !readBody(w, r, &req) {
		return
	}
	var e *Error
	switch {
	case req.ID == "":
		e = fieldError(CodeInvalidArgument, ReasonMissingField, "/id", "an id is required: what owners choose the offer by")
	case !store.IsOfferID(req.ID):
		e = fieldError(CodeInvalidArgument, ReasonInvalidField, "/id", "an id is letters, digits, '_' and '-', at most 64")
	}
	if e == nil {
		au.target("school_offer", req.ID)
		e = labelError(req.Label, "/label")
	}
	if e == nil {
		e = req.shape("")
	}
	if e == nil && req.Key == "" {
		e = fieldError(CodeInvalidArgument, ReasonMissingField, "/key", "the school's key is required")
	}
	if e == nil {
		e = keyMalformed(req.Key, "/key")
	}
	if e != nil {
		WriteError(w, *e)
		return
	}
	au.detail["key_hint"] = vault.Hint(store.SecretModelKey, req.Key)
	sec, e := req.section("")
	if e != nil {
		WriteError(w, *e)
		return
	}
	au.detail["provider"], au.detail["adapter"], au.detail["model"] = sec.Provider, sec.Adapter, sec.Model
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	eff, err := s.effective(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	o := offerOf(req.ID, req.Label, sec)
	o.Enabled, o.CreatedBy, o.UpdatedBy = req.Enabled == nil || *req.Enabled, c.ActorID, c.ActorID
	if eff.Runtime.Withheld(registry.SiteOffer(o)) == config.WithheldIDTaken {
		WriteError(w, errOfferExists(SourceConfig))
		return
	}
	if e := s.allowedOffer(eff, o, "/model"); e != nil {
		WriteError(w, *e)
		return
	}
	switch _, err := s.o.Store.SchoolOffer(ctx, o.ID); {
	case err == nil:
		WriteError(w, errOfferExists(SourceSite))
		return
	case !errors.Is(err, store.ErrNotFound):
		s.storeUnavailable(w, "the school's offers", err)
		return
	}
	if !req.SkipKeyTest && !s.tryOfferKey(ctx, w, c, au, sec, req.Key) {
		return
	}
	key, ok := s.sealOfferKey(ctx, w, c, req.Key)
	if !ok {
		return
	}
	o.KeySecretID, o.KeyHint, o.KeyTested = key.ID, key.Hint, !req.SkipKeyTest
	o.CreatedAt = s.o.Now().UTC()
	created, err := s.o.Store.CreateSchoolOffer(ctx, o, *key)
	switch {
	case errors.Is(err, store.ErrExists):
		WriteError(w, errOfferExists(SourceSite))
		return
	case err != nil:
		s.storeUnavailable(w, "an offer made", err)
		return
	}
	au.detail["enabled"], au.detail["version"] = created.Enabled, created.Version
	if req.SkipKeyTest {
		au.detail["key_test"] = "skipped"
	}
	s.writeOffer(ctx, w, http.StatusCreated, created)
}

// offerPatch is PATCH /admin/school-plan/offers/{id}'s body, a
// merge-patch: a member left out is kept as it is. The model's members
// are an owner's choice's; max_output_tokens and reasoning_effort null
// are the runtime's defaults. key is a new key of the school's, tried
// with the model unless skip_key_test; one is needed to move the offer to
// another provider.
type offerPatch struct {
	Label           json.RawMessage `json:"label"`
	Enabled         json.RawMessage `json:"enabled"`
	Provider        json.RawMessage `json:"provider"`
	Adapter         json.RawMessage `json:"adapter"`
	Model           json.RawMessage `json:"model"`
	Endpoint        json.RawMessage `json:"endpoint"`
	Resource        json.RawMessage `json:"resource"`
	Region          json.RawMessage `json:"region"`
	MaxOutputTokens json.RawMessage `json:"max_output_tokens"`
	ReasoningEffort json.RawMessage `json:"reasoning_effort"`
	Key             json.RawMessage `json:"key"`
	SkipKeyTest     json.RawMessage `json:"skip_key_test"`
}

// patchString reads a member that is a string into *into, when given:
// null only where nullable, as "".
func patchString(raw json.RawMessage, into *string, field string, nullable bool) *Error {
	switch {
	case raw == nil:
		return nil
	case isNull(raw) && nullable:
		*into = ""
		return nil
	case json.Unmarshal(raw, into) != nil || isNull(raw):
		return fieldError(CodeInvalidArgument, ReasonInvalidField, field, "must be text")
	}
	return nil
}

// patchBool reads a member that is true or false into *into, when given.
func patchBool(raw json.RawMessage, into *bool, field string) *Error {
	if raw != nil && (isNull(raw) || json.Unmarshal(raw, into) != nil) {
		return fieldError(CodeInvalidArgument, ReasonInvalidField, field, "must be true or false")
	}
	return nil
}

// siteOffer is the site's offer id, answering when there is none: an
// offer of runtime.yaml's is the operator's to change (403
// offer_read_only), and any other 404 offer_not_found.
func (s *Server) siteOffer(ctx context.Context, w http.ResponseWriter, id string) *store.SchoolOffer {
	o, err := s.o.Store.SchoolOffer(ctx, id)
	switch {
	case err == nil:
		return o
	case !errors.Is(err, store.ErrNotFound):
		s.storeUnavailable(w, "an offer", err)
	default:
		if _, ok := s.yaml().Runtime.School.OfferOf(id); ok {
			WriteError(w, Error{Code: CodeForbidden, Reason: ReasonOfferReadOnly,
				Message: "this offer is runtime.yaml's: only the runtime's operator changes it"})
			return nil
		}
		WriteError(w, Error{Code: CodeNotFound, Reason: ReasonOfferNotFound, Message: "the plan has no offer of this id"})
	}
	return nil
}

// offerMismatch is a write of an offer at a version it has left.
func offerMismatch(current int) Error {
	return Error{Code: CodeVersionMismatch, Reason: ReasonVersionMismatch,
		Message: "the offer has changed since that version was read: read it again", Details: map[string]any{"current_version": current}}
}

// updateOffer is PATCH /admin/school-plan/offers/{id}.
func (s *Server) updateOffer(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("school_offer", r.PathValue("id"))
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	version, named, bad := ifMatch(r)
	if bad != nil {
		WriteError(w, *bad)
		return
	}
	var req offerPatch
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	cur := s.siteOffer(ctx, w, r.PathValue("id"))
	if cur == nil {
		return
	}
	if named && version != cur.Version {
		WriteError(w, offerMismatch(cur.Version))
		return
	}
	next, choice, key, skip, e := readOfferPatch(req, *cur)
	if e == nil {
		e = choice.shape("")
	}
	var sec *modelSection
	if e == nil {
		sec, e = choice.section("")
	}
	if e == nil && key != "" {
		e = keyMalformed(key, "/key")
	}
	if e != nil {
		WriteError(w, *e)
		return
	}
	moved := sec.Provider != cur.Provider
	o := offerOf(cur.ID, next.Label, sec)
	o.Enabled, o.KeySecretID, o.KeyHint, o.KeyTested = next.Enabled, cur.KeySecretID, cur.KeyHint, cur.KeyTested
	o.Version, o.CreatedBy, o.UpdatedBy = cur.Version, cur.CreatedBy, c.ActorID
	var changed []string
	for _, f := range []struct {
		name string
		same bool
	}{
		{"label", o.Label == cur.Label}, {"enabled", o.Enabled == cur.Enabled},
		{"model", o.Provider == cur.Provider && o.Adapter == cur.Adapter && o.Model == cur.Model && o.BaseURL == cur.BaseURL && o.Region == cur.Region &&
			o.MaxOutputTokens == cur.MaxOutputTokens && o.ReasoningEffort == cur.ReasoningEffort},
		{"key", key == ""},
	} {
		if !f.same {
			changed = append(changed, f.name)
		}
	}
	if len(changed) == 0 {
		au.skip = true
		s.writeOffer(ctx, w, http.StatusOK, cur)
		return
	}
	if key != "" {
		au.detail["key_hint"] = vault.Hint(store.SecretModelKey, key)
	}
	au.detail["provider"], au.detail["adapter"], au.detail["model"], au.detail["changed"] = o.Provider, o.Adapter, o.Model, changed
	if moved && key == "" {
		WriteError(w, *fieldError(CodeFailedPrecondition, ReasonKeyRequired, "/key", "another provider's model needs a key of that provider's"))
		return
	}
	// An offer the model lists came to deny since may still be relabelled,
	// turned off or given another key; not given another model of theirs,
	// nor turned on.
	if slices.Contains(changed, "model") || o.Enabled && !cur.Enabled {
		eff, err := s.effective(ctx)
		if err != nil {
			s.storeUnavailable(w, "the site's settings", err)
			return
		}
		if e := s.allowedOffer(eff, o, "/model"); e != nil {
			WriteError(w, *e)
			return
		}
	}
	var secrets []store.Secret
	if key != "" {
		if !skip && !s.tryOfferKey(ctx, w, c, au, sec, key) {
			return
		}
		sealed, ok := s.sealOfferKey(ctx, w, c, key)
		if !ok {
			return
		}
		o.KeySecretID, o.KeyHint, o.KeyTested = sealed.ID, sealed.Hint, !skip
		secrets = append(secrets, *sealed)
		if skip {
			au.detail["key_test"] = "skipped"
		}
	} else if o.Provider != cur.Provider || o.Adapter != cur.Adapter || o.Model != cur.Model || o.BaseURL != cur.BaseURL || o.Region != cur.Region {
		// The key was tried with another model.
		o.KeyTested = false
	}
	updated, err := s.o.Store.UpdateSchoolOffer(ctx, o, secrets...)
	switch {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, Error{Code: CodeNotFound, Reason: ReasonOfferNotFound, Message: "the plan has no offer of this id"})
		return
	case errors.Is(err, store.ErrConflict):
		current := cur.Version
		if again, err := s.o.Store.SchoolOffer(ctx, cur.ID); err == nil {
			current = again.Version
		}
		WriteError(w, offerMismatch(current))
		return
	case err != nil:
		s.storeUnavailable(w, "an offer updated", err)
		return
	}
	au.detail["enabled"], au.detail["version"] = updated.Enabled, updated.Version
	s.writeOffer(ctx, w, http.StatusOK, updated)
}

// readOfferPatch is the offer cur with the patch req over it: its label
// and whether it is on, its model as an owner's choice (the members of
// cur's provider kept only while the provider is), and the key given, if
// any, with whether its trial is skipped.
func readOfferPatch(req offerPatch, cur store.SchoolOffer) (next store.SchoolOffer, choice OwnModelChoice, key string, skip bool, e *Error) {
	next = cur
	choice = OwnModelChoice{Provider: cur.Provider, Adapter: cur.Adapter, Model: cur.Model}
	endpoint, resource, region, _ := modelView(cur.Provider, cur.BaseURL, cur.Region)
	for _, p := range []struct {
		from *string
		into *string
	}{{endpoint, &choice.Endpoint}, {resource, &choice.Resource}, {region, &choice.Region}} {
		if p.from != nil {
			*p.into = *p.from
		}
	}
	if cur.MaxOutputTokens > 0 {
		n := cur.MaxOutputTokens
		choice.MaxOutputTokens = &n
	}
	if cur.ReasoningEffort != "" {
		effort := cur.ReasoningEffort
		choice.ReasoningEffort = &effort
	}
	var provider string
	if e = patchString(req.Provider, &provider, "/provider", false); e != nil {
		return
	}
	if req.Provider != nil && provider != cur.Provider {
		// Another provider's: its endpoint, adapter and model are its own.
		choice = OwnModelChoice{Provider: provider, MaxOutputTokens: choice.MaxOutputTokens, ReasoningEffort: choice.ReasoningEffort}
	}
	for _, f := range []struct {
		raw   json.RawMessage
		into  *string
		field string
	}{
		{req.Label, &next.Label, "/label"}, {req.Adapter, &choice.Adapter, "/adapter"}, {req.Model, &choice.Model, "/model"},
		{req.Endpoint, &choice.Endpoint, "/endpoint"}, {req.Resource, &choice.Resource, "/resource"}, {req.Region, &choice.Region, "/region"},
		{req.Key, &key, "/key"},
	} {
		nullable := f.field == "/endpoint" || f.field == "/resource" || f.field == "/region" || f.field == "/adapter"
		if e = patchString(f.raw, f.into, f.field, nullable); e != nil {
			return
		}
	}
	if req.Label != nil {
		if e = labelError(next.Label, "/label"); e != nil {
			return
		}
	}
	if req.Key != nil && key == "" {
		e = fieldError(CodeInvalidArgument, ReasonMissingField, "/key", "a key is text; leave it out to keep the one stored")
		return
	}
	if e = patchBool(req.Enabled, &next.Enabled, "/enabled"); e != nil {
		return
	}
	if e = patchBool(req.SkipKeyTest, &skip, "/skip_key_test"); e != nil {
		return
	}
	switch {
	case req.MaxOutputTokens == nil:
	case isNull(req.MaxOutputTokens):
		choice.MaxOutputTokens = nil
	default:
		var n int
		if json.Unmarshal(req.MaxOutputTokens, &n) != nil {
			e = fieldError(CodeInvalidArgument, ReasonInvalidField, "/max_output_tokens", "max_output_tokens is from 256 to 32000, or null")
			return
		}
		choice.MaxOutputTokens = &n
	}
	switch {
	case req.ReasoningEffort == nil:
	case isNull(req.ReasoningEffort):
		choice.ReasoningEffort = nil
	default:
		var effort string
		if json.Unmarshal(req.ReasoningEffort, &effort) != nil {
			e = fieldError(CodeInvalidArgument, ReasonInvalidField, "/reasoning_effort", "reasoning_effort is minimal, low, medium or high, or null")
			return
		}
		choice.ReasoningEffort = &effort
	}
	return
}

// deleteOffer is DELETE /admin/school-plan/offers/{id}: the site's offer
// and its key destroyed, at the version If-Match names when it names one.
// The hosted agents on it run on their owners' models behind it, or are
// not run (offer_withdrawn).
func (s *Server) deleteOffer(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("school_offer", r.PathValue("id"))
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	version, named, bad := ifMatch(r)
	if bad != nil {
		WriteError(w, *bad)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	cur := s.siteOffer(ctx, w, r.PathValue("id"))
	if cur == nil {
		return
	}
	agents, err := s.agentsOn(ctx)
	if err != nil {
		s.storeUnavailable(w, "the hosted agents", err)
		return
	}
	switch err := s.o.Store.DeleteSchoolOffer(ctx, cur.ID, heldTo(version, named)); {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, Error{Code: CodeNotFound, Reason: ReasonOfferNotFound, Message: "the plan has no offer of this id"})
		return
	case errors.Is(err, store.ErrConflict):
		WriteError(w, offerMismatch(cur.Version))
		return
	case err != nil:
		s.storeUnavailable(w, "an offer deleted", err)
		return
	}
	au.detail["provider"], au.detail["model"], au.detail["agents"] = cur.Provider, cur.Model, agents[cur.ID]
	var out OfferDeleted
	out.Deleted.ID, out.Agents = cur.ID, agents[cur.ID]
	writeJSON(w, http.StatusOK, out)
}

// quotasRequest is PUT /admin/school-plan/quotas' body: every quota, in
// answers a UTC day, per_day null for no ceiling.
type quotasRequest struct {
	PerOwnerDay json.RawMessage `json:"per_owner_day"`
	PerAskerDay json.RawMessage `json:"per_asker_day"`
	PerDay      json.RawMessage `json:"per_day"`
}

// quota reads a quota's member: from minQuota to maxQuota, or, where
// nullable, null for none.
func quota(raw json.RawMessage, field string, nullable bool) (*int, *Error) {
	if raw == nil {
		return nil, fieldError(CodeInvalidArgument, ReasonMissingField, field, "every quota is given: per_owner_day, per_asker_day and per_day")
	}
	if isNull(raw) && nullable {
		return nil, nil
	}
	var n int
	if isNull(raw) || json.Unmarshal(raw, &n) != nil || n < minQuota || n > maxQuota {
		msg := "a quota is a whole number of answers from 1 to 1000000"
		if nullable {
			msg += ", or null for none"
		}
		return nil, fieldError(CodeInvalidArgument, ReasonInvalidField, field, msg)
	}
	return &n, nil
}

// putQuotas is PUT /admin/school-plan/quotas: the site's quotas, in place
// of runtime.yaml's.
func (s *Server) putQuotas(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("site_setting", store.SettingSchoolQuotas)
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	var req quotasRequest
	if !readBody(w, r, &req) {
		return
	}
	owner, e := quota(req.PerOwnerDay, "/per_owner_day", false)
	var asker, day *int
	if e == nil {
		asker, e = quota(req.PerAskerDay, "/per_asker_day", false)
	}
	if e == nil {
		day, e = quota(req.PerDay, "/per_day", true)
	}
	if e != nil {
		WriteError(w, *e)
		return
	}
	q := config.SiteQuotas{PerOwnerDay: *owner, PerAskerDay: *asker, PerDay: day}
	value, err := json.Marshal(q)
	if err != nil {
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the quotas could not be written"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	if err := s.o.Store.PutSiteSetting(ctx, store.SiteSetting{Name: store.SettingSchoolQuotas, Value: value, UpdatedBy: c.ActorID,
		UpdatedAt: s.o.Now().UTC()}); err != nil {
		s.storeUnavailable(w, "the school's quotas written", err)
		return
	}
	au.detail["per_owner_day"], au.detail["per_asker_day"], au.detail["per_day"] = q.PerOwnerDay, q.PerAskerDay, q.PerDay
	s.writePlan(ctx, w)
}

// resetQuotas is DELETE /admin/school-plan/quotas: runtime.yaml's quotas
// again. With none of the site's set, nothing is written or audited.
func (s *Server) resetQuotas(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("site_setting", store.SettingSchoolQuotas)
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	set, err := s.siteSetting(ctx, store.SettingSchoolQuotas)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	if set == nil {
		au.skip = true
	} else if err := s.o.Store.DeleteSiteSetting(ctx, store.SettingSchoolQuotas); err != nil {
		s.storeUnavailable(w, "the school's quotas unset", err)
		return
	}
	s.writePlan(ctx, w)
}
