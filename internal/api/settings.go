package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// GET and PATCH /admin/settings: the site's settings, which the runtime's
// administrators alone read and change (D4; docs/design.md §11.5), within
// the ceiling the operator's environment sets: whether OCR runs, and in
// which of the languages installed; and whether the transcriber runs, and
// how (transcription.go). A change is kept in the store, and every worker
// puts it in force as it reads the registry again, without a restart.

// Settings is the answer of GET and PATCH /admin/settings.
type Settings struct {
	OCR           OCRSettings           `json:"ocr"`
	Transcription TranscriptionSettings `json:"transcription"`
}

// OCRSettings are OCR as the site sets it. Available is whether the
// environment lets it run at all, as this runtime finds it; when not,
// UnavailableReason says why (ocr.ReasonTurnedOff, OCR=off;
// ocr.ReasonNotInstalled, its programs or languages missing) and
// UnavailableDetail what is missing, and the site's setting changes
// nothing. Enabled and Languages are the site's setting, or its defaults
// (on, in the environment's languages, DefaultLanguages); OCR runs when it
// is available and enabled. AvailableLanguages are those tesseract has
// here, which Languages are chosen from. UpdatedAt and UpdatedBy are the
// setting's last write, null for none.
type OCRSettings struct {
	Available          bool       `json:"available"`
	UnavailableReason  *string    `json:"unavailable_reason"`
	UnavailableDetail  *string    `json:"unavailable_detail"`
	Enabled            bool       `json:"enabled"`
	Languages          []string   `json:"languages"`
	DefaultLanguages   []string   `json:"default_languages"`
	AvailableLanguages []string   `json:"available_languages"`
	UpdatedAt          *time.Time `json:"updated_at"`
	UpdatedBy          *string    `json:"updated_by"`
}

// maxOCRLanguages bounds the languages OCR is given at once: each is a
// model tesseract runs every page through.
const maxOCRLanguages = 8

// ocrCapability is what OCR can do here, none without the worker's.
func (s *Server) ocrCapability() ocr.Capability {
	if s.o.OCR == nil {
		return ocr.Capability{Reason: ocr.ReasonNotInstalled}
	}
	return s.o.OCR.Capability()
}

// siteSetting is the site's setting name as stored, nil when it is not
// set.
func (s *Server) siteSetting(ctx context.Context, name string) (*store.SiteSetting, error) {
	all, err := s.o.Store.SiteSettings(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, nil
}

// storedOCR is the site's OCR setting as stored, its zero value when none
// is, and the row it came from.
func (s *Server) storedOCR(ctx context.Context) (config.SiteOCR, *store.SiteSetting, error) {
	var o config.SiteOCR
	row, err := s.siteSetting(ctx, store.SettingOCR)
	if err != nil || row == nil {
		return o, row, err
	}
	if !registry.DecodeSetting(row.Value, &o) {
		o = config.SiteOCR{}
	}
	return o, row, nil
}

// settingsView is the site's settings as its administrators read them:
// OCR's o, as the row stored it, and the transcriber's t, as its row
// trow stored it.
func (s *Server) settingsView(ctx context.Context, o config.SiteOCR, row *store.SiteSetting, t config.SiteTranscription,
	trow *store.SiteSetting) (Settings, error) {
	tv, err := s.transcriptionView(ctx, t, trow)
	if err != nil {
		return Settings{}, err
	}
	return Settings{OCR: s.ocrView(o, row), Transcription: tv}, nil
}

// ocrView is OCR's setting as its administrators read it.
func (s *Server) ocrView(o config.SiteOCR, row *store.SiteSetting) OCRSettings {
	c := s.ocrCapability()
	v := OCRSettings{Available: c.Available, Enabled: o.Enabled == nil || *o.Enabled, Languages: o.Languages,
		DefaultLanguages: nonNil(c.Default), AvailableLanguages: nonNil(c.Installed)}
	if len(v.DefaultLanguages) == 0 {
		v.DefaultLanguages = strings.Split(ocr.DefaultLanguages, "+")
	}
	if len(v.Languages) == 0 {
		v.Languages = v.DefaultLanguages
	}
	if !c.Available {
		v.UnavailableReason = strPtr(c.Reason)
		v.UnavailableDetail = strPtr(c.Detail)
	}
	if row != nil {
		at := row.UpdatedAt.UTC()
		v.UpdatedAt, v.UpdatedBy = &at, strPtr(row.UpdatedBy)
	}
	return v
}

// nonNil is xs, or an empty list for none, as JSON's [] rather than null.
func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// strPtr is x, or nil for "".
func strPtr(x string) *string {
	if x == "" {
		return nil
	}
	return &x
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	o, row, err := s.storedOCR(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	t, trow, err := s.storedTranscription(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	s.writeSettings(ctx, w, o, row, t, trow)
}

// writeSettings answers the site's settings.
func (s *Server) writeSettings(ctx context.Context, w http.ResponseWriter, o config.SiteOCR, row *store.SiteSetting,
	t config.SiteTranscription, trow *store.SiteSetting) {
	v, err := s.settingsView(ctx, o, row, t, trow)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// settingsPatch is PATCH /admin/settings' body, a merge-patch: a member
// left out is kept as it is.
type settingsPatch struct {
	OCR           json.RawMessage `json:"ocr"`
	Transcription json.RawMessage `json:"transcription"`
}

// ocrPatch is its ocr: enabled true or false; languages, those installed
// in the order tesseract is to take them, or null for the environment's.
type ocrPatch struct {
	Enabled   json.RawMessage `json:"enabled"`
	Languages json.RawMessage `json:"languages"`
}

// patchSettings is PATCH /admin/settings: OCR's setting, audited as
// settings.update, and the transcriber's, as transcription_settings.update
// (the route's own event, where the patch has no ocr).
func (s *Server) patchSettings(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("site_setting", store.SettingOCR)
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	var req settingsPatch
	if !readBody(w, r, &req) {
		return
	}
	if req.OCR == nil && req.Transcription != nil {
		au.action = "transcription_settings.update"
		au.target("site_setting", store.SettingTranscription)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	cur, row, err := s.storedOCR(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	tcur, trow, err := s.storedTranscription(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	next, changed, e := s.readOCRPatch(req.OCR, cur)
	if e != nil {
		WriteError(w, *e)
		return
	}
	tnext, tchanged, e, err := s.readTranscriptionPatch(ctx, req.Transcription, tcur)
	switch {
	case err != nil:
		s.storeUnavailable(w, "the school's plan", err)
		return
	case e != nil:
		WriteError(w, *e)
		return
	}
	if len(changed) > 0 {
		put, ok := s.putSetting(ctx, w, c, store.SettingOCR, next)
		if !ok {
			return
		}
		cur, row = next, put
		au.detail["changed"] = changed
		au.detail["enabled"] = next.Enabled == nil || *next.Enabled
		au.detail["languages"] = next.Languages
	}
	if len(tchanged) > 0 {
		put, ok := s.putSetting(ctx, w, c, store.SettingTranscription, tnext)
		if !ok {
			return
		}
		tcur, trow = tnext, put
		detail := map[string]any{"changed": tchanged, "enabled": tnext.Enabled, "offer": tnext.Offer, "max_pages": tnext.Pages(),
			"per_day_pages": tnext.PerDayPages, "concurrency": tnext.Slots()}
		if au.action != "" {
			au.detail = detail
		} else {
			s.Audit(ctx, r, store.AuditEvent{Action: "transcription_settings.update", TargetType: "site_setting",
				TargetID: store.SettingTranscription, Outcome: "ok", Detail: mustJSON(detail)})
		}
	}
	// The route's own event is of OCR's change, or of the transcriber's
	// where the patch has no ocr: none where it changed nothing.
	au.skip = len(changed) == 0 && (au.action == "" || len(tchanged) == 0)
	s.writeSettings(ctx, w, cur, row, tcur, trow)
}

// putSetting keeps the site's setting name as value, written by c now,
// answering when it cannot be.
func (s *Server) putSetting(ctx context.Context, w http.ResponseWriter, c *Caller, name string, value any) (*store.SiteSetting, bool) {
	b, err := json.Marshal(value)
	if err != nil {
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the setting could not be written"})
		return nil, false
	}
	put := store.SiteSetting{Name: name, Value: b, UpdatedBy: c.ActorID, UpdatedAt: s.o.Now().UTC()}
	if err := s.o.Store.PutSiteSetting(ctx, put); err != nil {
		s.storeUnavailable(w, "the site's settings written", err)
		return nil, false
	}
	return &put, true
}

// readOCRPatch is the OCR setting cur with the patch raw over it, and the
// members it changed; the environment's ceiling holds: where OCR cannot
// run, it is not turned on, and no languages are chosen.
func (s *Server) readOCRPatch(raw json.RawMessage, cur config.SiteOCR) (config.SiteOCR, []string, *Error) {
	next := cur
	var changed []string
	if raw == nil {
		return next, nil, nil
	}
	var p ocrPatch
	if e := decodeMember(raw, &p, "/ocr"); e != nil {
		return next, nil, e
	}
	c := s.ocrCapability()
	unavailable := func(field string) *Error {
		msg := "OCR cannot run on this runtime (" + c.Reason + "): the operator's environment turns it off, or lacks its programs"
		return &Error{Code: CodeFailedPrecondition, Reason: ReasonOCRUnavailable, Message: msg, Details: map[string]any{"field": field}}
	}
	if p.Enabled != nil {
		var on bool
		if isNull(p.Enabled) || json.Unmarshal(p.Enabled, &on) != nil {
			return next, nil, fieldError(CodeInvalidArgument, ReasonInvalidField, "/ocr/enabled", "enabled is true or false")
		}
		if on && !c.Available {
			return next, nil, unavailable("/ocr/enabled")
		}
		if (cur.Enabled == nil || *cur.Enabled) != on {
			changed = append(changed, "ocr.enabled")
		}
		next.Enabled = &on
	}
	if p.Languages != nil {
		var langs []string
		switch {
		case isNull(p.Languages):
		case !c.Available:
			return next, nil, unavailable("/ocr/languages")
		case !bytes.HasPrefix(bytes.TrimSpace(p.Languages), []byte("[")) || json.Unmarshal(p.Languages, &langs) != nil:
			return next, nil, fieldError(CodeInvalidArgument, ReasonInvalidField, "/ocr/languages", "languages is a list of names, or null")
		case len(langs) == 0 || len(langs) > maxOCRLanguages:
			return next, nil, fieldError(CodeInvalidArgument, ReasonInvalidField, "/ocr/languages", "from 1 to 8 languages, or null")
		}
		for i, l := range langs {
			at := "/ocr/languages/" + strconv.Itoa(i)
			switch {
			case !slices.Contains(c.Installed, l):
				return next, nil, fieldError(CodeInvalidArgument, ReasonInvalidField, at, "not a language installed here: see available_languages")
			case slices.Index(langs, l) != i:
				return next, nil, fieldError(CodeInvalidArgument, ReasonInvalidField, at, "a language named twice")
			}
		}
		if !slices.Equal(cur.Languages, langs) {
			changed = append(changed, "ocr.languages")
		}
		next.Languages = langs
	}
	return next, changed, nil
}
