package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/transcribe"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// The transcriber (package transcribe; docs/design.md §12) as the
// runtime's administrators alone read and set it (D4; docs/design.md
// §11.5): GET and PATCH /admin/settings' transcription, whether it runs,
// with which offer of the school's plan and within which limits; PUT and
// DELETE /admin/transcription/credential, the service credential it claims
// with, issued in Core by an administrator and handed over here, tried
// with Core before it is kept (unless skip_test), sealed under the tenant
// site, and never shown again but as its hint; and GET
// /admin/transcription/jobs, the runtime's record of what it did. What
// changes is kept in the store, and every worker puts it in force as it
// reads the registry again, without a restart. The operator's environment
// is the ceiling (TRANSCRIBE, and what the transcriber needs): where it
// cannot run, it is not turned on.

// Transcriber is what the API reads of the worker's transcriber:
// *transcribe.Service.
type Transcriber interface {
	// Status is where it stands on this worker; Check the same, having
	// read Core's catalogue when it was not read.
	Status() transcribe.Status
	Check(ctx context.Context) (transcribe.Status, error)
	// Setting is the site's setting in force, as the worker last built
	// it; Blocked why a setting would claim nothing now.
	Setting() transcribe.Setting
	Blocked(ctx context.Context, st transcribe.Setting) string
}

// TranscriptionSettings are the transcriber as the site sets it, and where
// it stands. Available is whether the environment lets it run at all, as
// this runtime finds it; when not, UnavailableReason says why
// (transcribe.ReasonOperatorOff, transcribe.ReasonCoreTooOld) and
// UnavailableDetail what is missing, and it is not turned on. Enabled,
// Offer, MaxPages, PerDayPages and Concurrency are the site's setting, or
// its defaults (off, no offer, 300 pages, no daily limit, 2 at once);
// OfferStatus is where the offer stands in the plan in force (null for
// none); Credential the service credential; State what it does now, and
// BlockedReason why it claims nothing while on (a transcribe.Blocked…
// reason); Today the UTC day's work so far; UpdatedAt and UpdatedBy the
// setting's last write, null for none.
type TranscriptionSettings struct {
	Available         bool                    `json:"available"`
	UnavailableReason *string                 `json:"unavailable_reason"`
	UnavailableDetail *string                 `json:"unavailable_detail"`
	Enabled           bool                    `json:"enabled"`
	Offer             *string                 `json:"offer"`
	OfferStatus       *string                 `json:"offer_status"`
	MaxPages          int                     `json:"max_pages"`
	PerDayPages       *int                    `json:"per_day_pages"`
	Concurrency       int                     `json:"concurrency"`
	Credential        TranscriptionCredential `json:"credential"`
	State             string                  `json:"state"`
	BlockedReason     *string                 `json:"blocked_reason"`
	Today             TranscriptionToday      `json:"today"`
	UpdatedAt         *time.Time              `json:"updated_at"`
	UpdatedBy         *string                 `json:"updated_by"`
}

// TranscriptionCredential is the service credential as the administrators
// read it: its status (none, ok: Core took it; untested: given with
// skip_test, and not yet used; rejected: Core refused it), its hint (the
// token's public prefix), Core's id of it when it was given, when and by
// whom it was given, when Core last took it, and why it was refused.
type TranscriptionCredential struct {
	Status       string     `json:"status"`
	Hint         *string    `json:"hint"`
	CredentialID *string    `json:"credential_id"`
	SetAt        *time.Time `json:"set_at"`
	SetBy        *string    `json:"set_by"`
	LastOKAt     *time.Time `json:"last_ok_at"`
	LastError    *string    `json:"last_error"`
}

// TranscriptionToday is the UTC day's work so far, from the runtime's
// record of its jobs: the pages sent to the model, the documents done,
// failed and skipped, and what their model calls cost.
type TranscriptionToday struct {
	Pages     int    `json:"pages"`
	Documents int    `json:"documents"`
	Failed    int    `json:"failed"`
	Skipped   int    `json:"skipped"`
	CostUSD   string `json:"cost_usd"`
}

// What the transcriber does now (TranscriptionSettings.State).
const (
	TranscriptionOff     = "off"
	TranscriptionRunning = "running"
	TranscriptionStandby = "standby"
	TranscriptionBlocked = "blocked"
)

// Where the transcriber's offer stands in the plan in force
// (TranscriptionSettings.OfferStatus).
const (
	OfferStatusOK          = "ok"
	OfferStatusNotFound    = "not_found"
	OfferStatusDisabled    = "disabled"
	OfferStatusNoFileInput = "no_file_input"
	OfferStatusNotPriced   = "not_priced"
)

// The service credential's status (TranscriptionCredential.Status).
const (
	CredentialNone     = "none"
	CredentialOK       = "ok"
	CredentialUntested = "untested"
	CredentialRejected = "rejected"
)

// checkTimeout bounds the reading of Core's catalogue a view of the
// settings may make, where the transcriber has not read it.
const checkTimeout = 3 * time.Second

// transcriberStatus is where the transcriber stands, Core's catalogue
// read where it was not: none runs without the worker's.
func (s *Server) transcriberStatus(ctx context.Context) transcribe.Status {
	if s.o.Transcriber == nil {
		return transcribe.Status{Reason: transcribe.ReasonOperatorOff, Detail: "no transcriber runs here"}
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	st, err := s.o.Transcriber.Check(ctx)
	if err != nil {
		s.o.Log.Warn("Core's catalogue could not be read for the transcriber", "err", err)
	}
	return st
}

// storedTranscription is the site's transcription setting as stored, its
// zero value (off) when none is, and the row it came from.
func (s *Server) storedTranscription(ctx context.Context) (config.SiteTranscription, *store.SiteSetting, error) {
	var t config.SiteTranscription
	row, err := s.siteSetting(ctx, store.SettingTranscription)
	if err != nil || row == nil {
		return t, row, err
	}
	if !registry.DecodeSetting(row.Value, &t) {
		t = config.SiteTranscription{}
	}
	return t, row, nil
}

// transcriptionOffer is the offer id of the plan in force eff, and where it
// stands: not found where the plan has no offer of that id, disabled where
// the site's offer of it is turned off or withheld from the plan, of a
// model that takes no files, of a model the price table in force does not
// price today (which a ceiling in dollars cannot hold: a warning), or ok.
// siteOffers are the site's offers, on and off.
func (s *Server) transcriptionOffer(eff *config.Config, siteOffers []store.SchoolOffer, id string) (*config.SchoolOffer, string) {
	if id == "" {
		return nil, ""
	}
	var offer *config.SchoolOffer
	for _, o := range eff.Runtime.School.Offers {
		if o.ID == id {
			offer = &o
			break
		}
	}
	if offer == nil {
		for _, o := range siteOffers {
			if o.ID == id {
				return nil, OfferStatusDisabled
			}
		}
		return nil, OfferStatusNotFound
	}
	m := offer.AsModel()
	if transcribe.InputOf(m) == transcribe.InputNone {
		return offer, OfferStatusNoFileInput
	}
	if _, ok := s.pricesOf(eff).Lookup(m.EffectiveProvider(), m.Model, s.o.Now()); !ok {
		return offer, OfferStatusNotPriced
	}
	return offer, OfferStatusOK
}

// transcriptionView is the transcriber as its administrators read it: the
// site's setting t, as the row stored it, in the plan in force.
func (s *Server) transcriptionView(ctx context.Context, t config.SiteTranscription, row *store.SiteSetting) (TranscriptionSettings, error) {
	eff, err := s.effective(ctx)
	if err != nil {
		return TranscriptionSettings{}, err
	}
	siteOffers, err := s.o.Store.SchoolOffers(ctx)
	if err != nil {
		return TranscriptionSettings{}, err
	}
	cred, err := s.o.Store.TranscriptionCredential(ctx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return TranscriptionSettings{}, err
	}
	day, err := s.o.Store.TranscriptionDay(ctx, store.UTCDay(s.o.Now()))
	if err != nil {
		return TranscriptionSettings{}, err
	}
	st := s.transcriberStatus(ctx)
	v := TranscriptionSettings{Available: st.Available, Enabled: t.Enabled, Offer: strPtr(t.Offer), MaxPages: t.Pages(),
		PerDayPages: clonePtr(t.PerDayPages), Concurrency: t.Slots(), Credential: credentialView(cred), State: TranscriptionOff,
		Today: TranscriptionToday{Pages: day.Pages, Documents: day.Documents, Failed: day.Failed, Skipped: day.Skipped,
			CostUSD: costUSD(day.CostPUSD)}}
	if !st.Available {
		v.UnavailableReason, v.UnavailableDetail = strPtr(st.Reason), strPtr(st.Detail)
	}
	offer, status := s.transcriptionOffer(eff, siteOffers, t.Offer)
	v.OfferStatus = strPtr(status)
	if row != nil {
		at := row.UpdatedAt.UTC()
		v.UpdatedAt, v.UpdatedBy = &at, strPtr(row.UpdatedBy)
	}
	if !v.Available || !v.Enabled {
		return v, nil
	}
	if status == OfferStatusNoFileInput {
		offer = nil
	}
	why := s.o.Transcriber.Blocked(ctx, transcribe.Setting{Site: t, Offer: offer, PerDayUSD: eff.Runtime.School.PerDay.USD})
	switch {
	case why != "":
		v.State, v.BlockedReason = TranscriptionBlocked, &why
	case st.Standby:
		v.State = TranscriptionStandby
	default:
		v.State = TranscriptionRunning
	}
	return v, nil
}

// credentialView is the credential c as the administrators read it: none
// for nil.
func credentialView(c *store.TranscriptionCredential) TranscriptionCredential {
	if c == nil {
		return TranscriptionCredential{Status: CredentialNone}
	}
	v := TranscriptionCredential{Status: CredentialUntested, Hint: strPtr(c.Hint), CredentialID: strPtr(c.CredentialID),
		SetBy: strPtr(c.SetBy), LastError: strPtr(c.LastError)}
	set := c.SetAt.UTC()
	v.SetAt = &set
	if c.LastOKAt != nil {
		ok := c.LastOKAt.UTC()
		v.LastOKAt = &ok
	}
	switch {
	case c.RejectedAt != nil:
		v.Status = CredentialRejected
	case c.Tested || c.LastOKAt != nil:
		v.Status = CredentialOK
	}
	return v
}

// transcriptionRunning is GET /info's features.transcription: the
// transcriber of this worker runs, or stands by, as the site's setting in
// force says, and nothing stops it.
func (s *Server) transcriptionRunning(ctx context.Context) bool {
	if s.o.Transcriber == nil {
		return false
	}
	set := s.o.Transcriber.Setting()
	return s.o.Transcriber.Status().Available && set.Site.Enabled && s.o.Transcriber.Blocked(ctx, set) == ""
}

// transcriptionPatch is PATCH /admin/settings' transcription, a
// merge-patch: enabled true or false; offer, an offer's id of the plan, or
// null for none; max_pages, 1 to 5000; per_day_pages, 1 to 1,000,000, or
// null for no limit; concurrency, 1 to 8. null takes max_pages' and
// concurrency's defaults again.
type transcriptionPatch struct {
	Enabled     json.RawMessage `json:"enabled"`
	Offer       json.RawMessage `json:"offer"`
	MaxPages    json.RawMessage `json:"max_pages"`
	PerDayPages json.RawMessage `json:"per_day_pages"`
	Concurrency json.RawMessage `json:"concurrency"`
}

// errTranscriptionUnavailable refuses to turn on a transcriber that cannot
// run here.
func errTranscriptionUnavailable(st transcribe.Status, field string) *Error {
	msg := "the transcriber cannot run on this runtime (" + st.Reason + "): " + st.Detail
	return &Error{Code: CodeFailedPrecondition, Reason: ReasonTranscriptionUnavailable, Message: msg, Details: map[string]any{"field": field}}
}

// readTranscriptionPatch is the transcription setting cur with the patch
// raw over it, and the members it changed; the environment's ceiling
// holds: where the transcriber cannot run, it is not turned on.
func (s *Server) readTranscriptionPatch(ctx context.Context, raw json.RawMessage, cur config.SiteTranscription) (config.SiteTranscription,
	[]string, *Error, error) {
	next := cur
	var changed []string
	if raw == nil {
		return next, nil, nil, nil
	}
	var p transcriptionPatch
	if e := decodeMember(raw, &p, "/transcription"); e != nil {
		return next, nil, e, nil
	}
	if p.Enabled != nil {
		var on bool
		if isNull(p.Enabled) || json.Unmarshal(p.Enabled, &on) != nil {
			return next, nil, fieldError(CodeInvalidArgument, ReasonInvalidField, "/transcription/enabled", "enabled is true or false"), nil
		}
		if st := s.transcriberStatus(ctx); on && !st.Available {
			return next, nil, errTranscriptionUnavailable(st, "/transcription/enabled"), nil
		}
		if cur.Enabled != on {
			changed = append(changed, "transcription.enabled")
		}
		next.Enabled = on
	}
	if p.Offer != nil {
		var id string
		if !isNull(p.Offer) && (json.Unmarshal(p.Offer, &id) != nil || !store.IsOfferID(id)) {
			return next, nil, fieldError(CodeInvalidArgument, ReasonInvalidField, "/transcription/offer", "offer is an offer's id, or null"), nil
		}
		if id != "" {
			e, err := s.transcriptionOfferOf(ctx, id)
			if e != nil || err != nil {
				return next, nil, e, err
			}
		}
		if cur.Offer != id {
			changed = append(changed, "transcription.offer")
		}
		next.Offer = id
	}
	for _, m := range []struct {
		raw      json.RawMessage
		name     string
		lo, hi   int
		into     *int
		nullable bool
	}{
		{p.MaxPages, "max_pages", 1, config.MaxTranscribeMaxPages, &next.MaxPages, false},
		{p.Concurrency, "concurrency", 1, config.MaxTranscribeConcurrency, &next.Concurrency, false},
	} {
		if m.raw == nil {
			continue
		}
		n, e := boundedInt(m.raw, "/transcription/"+m.name, m.lo, m.hi)
		if e != nil {
			return next, nil, e, nil
		}
		v := 0
		if n != nil {
			v = *n
		}
		if *m.into != v {
			changed = append(changed, "transcription."+m.name)
		}
		*m.into = v
	}
	if p.PerDayPages != nil {
		n, e := boundedInt(p.PerDayPages, "/transcription/per_day_pages", 1, config.MaxTranscribePerDayPages)
		if e != nil {
			return next, nil, e, nil
		}
		if (cur.PerDayPages == nil) != (n == nil) || n != nil && *cur.PerDayPages != *n {
			changed = append(changed, "transcription.per_day_pages")
		}
		next.PerDayPages = n
	}
	return next, changed, nil, nil
}

// transcriptionOfferOf refuses an offer the transcriber may not be given:
// one the plan has not, runtime.yaml's or the site's, on or off
// (invalid_field), or one whose model takes no files (offer_no_file_input).
func (s *Server) transcriptionOfferOf(ctx context.Context, id string) (*Error, error) {
	siteOffers, err := s.o.Store.SchoolOffers(ctx)
	if err != nil {
		return nil, err
	}
	var m *config.Model
	for _, o := range s.yaml().Runtime.School.Offers {
		if o.ID == id {
			mm := o.AsModel()
			m = &mm
			break
		}
	}
	for _, o := range siteOffers {
		if m == nil && o.ID == id {
			mm := registry.SiteOffer(o).AsModel()
			m = &mm
		}
	}
	switch {
	case m == nil:
		return fieldError(CodeInvalidArgument, ReasonInvalidField, "/transcription/offer", "the school's plan has no offer of this id"), nil
	case transcribe.InputOf(*m) == transcribe.InputNone:
		return &Error{Code: CodeFailedPrecondition, Reason: ReasonOfferNoFileInput,
			Message: "the offer's model takes neither PDFs nor pictures: it cannot transcribe", Details: map[string]any{"field": "/transcription/offer"}}, nil
	}
	return nil, nil
}

// boundedInt is raw as a whole number from lo to hi, or nil for null,
// having refused anything else at field.
func boundedInt(raw json.RawMessage, field string, lo, hi int) (*int, *Error) {
	if isNull(raw) {
		return nil, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n < lo || n > hi {
		return nil, fieldError(CodeInvalidArgument, ReasonInvalidField, field, "a whole number from "+strconv.Itoa(lo)+" to "+strconv.Itoa(hi)+", or null")
	}
	return &n, nil
}

// credentialRequest is PUT /admin/transcription/credential's body: the
// service's token, Core's id of its credential (for display, and to match
// Core's list), and whether to keep it without trying it with Core.
type credentialRequest struct {
	Token        string  `json:"token"`
	CredentialID *string `json:"credential_id"`
	SkipTest     bool    `json:"skip_test"`
}

// maxCredentialID bounds the id of Core's credential kept beside the token.
const maxCredentialID = 128

// serviceToken says whether token has the shape of Core's service token:
// aissvc_, a public prefix of 12, _, the secret.
func serviceToken(token string) bool {
	return strings.HasPrefix(token, core.ServiceTokenPrefix) && len(token) <= 512 && !strings.ContainsAny(token, " \t\r\n") &&
		vault.Hint(store.SecretCoreToken, token) != vault.Ellipsis
}

// putCredential is PUT /admin/transcription/credential: the service's
// token, tried with Core by a call that claims nothing (unless skip_test),
// sealed, and kept in place of the one before; the transcriber takes it
// without a restart.
func (s *Server) putCredential(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("transcription_credential", "")
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	var req credentialRequest
	if !readBody(w, r, &req) {
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	switch {
	case req.Token == "":
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonMissingField, "/token", "the service's token is required"))
		return
	case !serviceToken(req.Token):
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "/token", "not a token of Core's transcription service (aissvc_…)"))
		return
	case req.CredentialID != nil && (*req.CredentialID == "" || len(*req.CredentialID) > maxCredentialID ||
		strings.ContainsFunc(*req.CredentialID, func(r rune) bool { return r <= ' ' || r > '~' })):
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "/credential_id", "Core's id of the credential"))
		return
	}
	hint := vault.Hint(store.SecretCoreToken, req.Token)
	au.detail["hint"], au.detail["tested"] = hint, !req.SkipTest
	if req.CredentialID != nil {
		au.target("transcription_credential", *req.CredentialID)
		au.detail["credential_id"] = *req.CredentialID
	}
	if s.o.Vault == nil {
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime cannot keep credentials now"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), inspectTimeout)
	defer cancel()
	if !req.SkipTest && !s.testCredential(ctx, w, req.Token) {
		return
	}
	sec, err := s.o.Vault.Seal(ctx, store.Secret{ID: vault.NewSecretID(), TenantID: store.SiteTenantID, Kind: store.SecretCoreToken,
		CreatedBy: c.ActorID}, req.Token)
	if err != nil {
		s.o.Log.Error("the transcriber's credential could not be sealed", "err", err)
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime could not keep the credential"})
		return
	}
	now := s.o.Now().UTC()
	cred := store.TranscriptionCredential{SecretID: sec.ID, Hint: sec.Hint, Tested: !req.SkipTest, SetBy: c.ActorID, SetAt: now}
	if req.CredentialID != nil {
		cred.CredentialID = *req.CredentialID
	}
	if err := s.o.Store.PutTranscriptionCredential(ctx, cred, sec); err != nil {
		s.storeUnavailable(w, "the transcriber's credential kept", err)
		return
	}
	if !req.SkipTest {
		// Core took it just now.
		if err := s.o.Store.NoteTranscriptionCredential(ctx, sec.ID, true, now, ""); err != nil {
			s.o.Log.Warn("Core's taking of the transcriber's credential could not be noted", "err", err)
		}
	}
	s.writeTranscription(ctx, w)
}

// testCredential tries token with Core by a call that claims nothing: the
// renewal of a claim no version has (a nil uuid, a lease of chance), which
// Core answers 404 or 409 to a token of the service's; 401 is a token Core
// does not take, and 403 service_only one that is not the service's. It
// answers a refusal, and reports whether the token passed.
func (s *Server) testCredential(ctx context.Context, w http.ResponseWriter, token string) bool {
	unavailable := func(msg string) bool {
		WriteError(w, Error{Code: CodeUnavailable, Reason: ReasonCoreUnavailable, Message: msg})
		return false
	}
	rejected := func(details map[string]any, msg string) bool {
		WriteError(w, Error{Code: CodeFailedPrecondition, Reason: ReasonCredentialRejected, Message: msg, Details: details})
		return false
	}
	if s.o.CoreBaseURL == "" {
		WriteError(w, *errTranscriptionUnavailable(transcribe.Status{Reason: transcribe.ReasonOperatorOff, Detail: "CORE_BASE_URL is not set"},
			"/token"))
		return false
	}
	cat, err := s.catalogue(ctx)
	if err != nil {
		s.o.Log.Warn("Core's catalogue could not be read", "err", err)
		return unavailable("Core cannot be reached to try the credential")
	}
	if !core.HasService(cat) {
		WriteError(w, *errTranscriptionUnavailable(transcribe.Status{Reason: transcribe.ReasonCoreTooOld,
			Detail: "Core has no transcription service"}, "/token"))
		return false
	}
	svc := core.NewService(core.NewRESTCaller(core.RESTOptions{BaseURL: s.o.CoreBaseURL, Token: token, Catalogue: cat, HTTPClient: s.coreHTTP}))
	_, err = svc.Renew(ctx, core.Claim{VersionID: uuid.Nil.String(), LeaseID: uuid.NewString()}, core.MinLease)
	var se *core.ServiceError
	switch {
	case err == nil, core.IsNotFound(err), errors.As(err, &se) && se.Code == core.CodeConflict:
		return true
	case errors.Is(err, core.ErrUnauthenticated):
		return rejected(map[string]any{"status": http.StatusUnauthorized}, "Core answered 401: the credential is revoked, expired or not Core's")
	case core.IsReason(err, core.ReasonServiceOnly):
		return rejected(map[string]any{"status": http.StatusForbidden, "core_reason": core.ReasonServiceOnly},
			"Core answered 403 service_only: the token is not the transcription service's")
	case errors.As(err, &se):
		return rejected(map[string]any{"core_code": se.Code, "core_reason": se.Reason}, "Core refused the credential")
	}
	s.o.Log.Warn("the transcriber's credential could not be tried with Core", "err", err)
	return unavailable("Core cannot be reached to try the credential")
}

// deleteCredential is DELETE /admin/transcription/credential: the
// credential forgotten, and the transcriber claims nothing more; the
// front end revokes it in Core.
func (s *Server) deleteCredential(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("transcription_credential", "")
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	cur, err := s.o.Store.TranscriptionCredential(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		au.skip = true
		s.writeTranscription(ctx, w)
		return
	case err != nil:
		s.storeUnavailable(w, "the transcriber's credential", err)
		return
	}
	if cur.CredentialID != "" {
		au.target("transcription_credential", cur.CredentialID)
		au.detail["credential_id"] = cur.CredentialID
	}
	au.detail["hint"] = cur.Hint
	if err := s.o.Store.DeleteTranscriptionCredential(ctx); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.storeUnavailable(w, "the transcriber's credential forgotten", err)
		return
	}
	s.writeTranscription(ctx, w)
}

// writeTranscription answers the transcriber's settings as they now are.
func (s *Server) writeTranscription(ctx context.Context, w http.ResponseWriter) {
	t, row, err := s.storedTranscription(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	v, err := s.transcriptionView(ctx, t, row)
	if err != nil {
		s.storeUnavailable(w, "the transcriber's settings", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// JobList is GET /admin/transcription/jobs' answer: a page of jobs, newest
// first, and the cursor of the next page (after), null after the last.
type JobList struct {
	Jobs []TranscriptionJob `json:"jobs"`
	Next *string            `json:"next"`
}

// TranscriptionJob is what the transcriber did with one claim of a
// version: ids, never a title or any text; its status (working, done,
// failed, skipped, or dropped: the claim lost, staff wrote the text, the
// version gone, or the worker stopped) and why; the file's type and size,
// its pages (null before they are counted), the offer and model that read
// it (null for a text file's own text), the cost of its calls (null where
// no price held them) and their tokens (null for no call), and when it
// started and ended.
type TranscriptionJob struct {
	ID           string     `json:"id"`
	VersionID    string     `json:"version_id"`
	DocumentID   string     `json:"document_id"`
	CourseID     string     `json:"course_id"`
	Status       string     `json:"status"`
	Reason       *string    `json:"reason"`
	Backfill     bool       `json:"backfill"`
	Attempt      int        `json:"attempt"`
	ContentType  string     `json:"content_type"`
	ByteSize     int64      `json:"byte_size"`
	Pages        *int       `json:"pages"`
	PagesSent    int        `json:"pages_sent"`
	Offer        *string    `json:"offer"`
	Model        *string    `json:"model"`
	ModelCalls   int        `json:"model_calls"`
	CostUSD      *string    `json:"cost_usd"`
	InputTokens  *int64     `json:"input_tokens"`
	OutputTokens *int64     `json:"output_tokens"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

// Bounds of GET /admin/transcription/jobs' page.
const (
	defaultJobPage = 50
	maxJobPage     = 200
)

// jobs is GET /admin/transcription/jobs: a page of the transcriber's jobs,
// newest first, of one status (done, failed, skipped, working, dropped)
// or all; after is the next of the page before.
func (s *Server) jobs(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	p, ok := queryOf(w, r, "after", "limit", "status")
	if !ok {
		return
	}
	refuse := func(field, msg string) {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, field, msg))
	}
	q := store.JobQuery{Limit: defaultJobPage}
	if v, given := p["limit"]; given {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxJobPage {
			refuse("limit", "limit is from 1 to 200")
			return
		}
		q.Limit = n
	}
	if v, given := p["status"]; given {
		if !store.JobStatusKnown(v) {
			refuse("status", "status is working, done, failed, skipped or dropped")
			return
		}
		q.Status = v
	}
	if v, given := p["after"]; given {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			refuse("after", "after is the next of a page before")
			return
		}
		q.Before = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	page := q
	page.Limit++
	jobs, err := s.o.Store.TranscriptionJobs(ctx, page)
	if err != nil {
		s.storeUnavailable(w, "the transcriber's jobs", err)
		return
	}
	out := JobList{Jobs: []TranscriptionJob{}}
	more := len(jobs) > q.Limit
	if more {
		jobs = jobs[:q.Limit]
	}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, jobView(j))
	}
	if more {
		next := strconv.FormatInt(jobs[len(jobs)-1].Seq, 10)
		out.Next = &next
	}
	writeJSON(w, http.StatusOK, out)
}

// jobView is the job j as the administrators read it.
func jobView(j store.TranscriptionJob) TranscriptionJob {
	v := TranscriptionJob{ID: j.ID, VersionID: j.VersionID, DocumentID: j.DocumentID, CourseID: j.CourseID, Status: j.Status,
		Reason: strPtr(j.Reason), Backfill: j.Backfill, Attempt: j.Attempt, ContentType: j.ContentType, ByteSize: j.ByteSize,
		Pages: clonePtr(j.Pages), PagesSent: j.PagesSent, Offer: strPtr(j.Offer), Model: strPtr(j.Model), ModelCalls: j.ModelCalls,
		StartedAt: j.StartedAt.UTC()}
	if j.CostPUSD != nil {
		usd := costUSD(*j.CostPUSD)
		v.CostUSD = &usd
	}
	if j.ModelCalls > 0 {
		in, out := j.InputTokens, j.OutputTokens
		v.InputTokens, v.OutputTokens = &in, &out
	}
	if j.FinishedAt != nil {
		at := j.FinishedAt.UTC()
		v.FinishedAt = &at
	}
	return v
}
