package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// fakeOCR is the worker's OCR as the API reads it.
type fakeOCR struct{ c ocr.Capability }

func (f fakeOCR) Capability() ocr.Capability { return f.c }

// installed is OCR that runs here, in three languages installed and two
// by default.
var installed = fakeOCR{ocr.Capability{Available: true, Installed: []string{"chi_sim", "chi_tra", "eng"}, Default: []string{"chi_tra", "eng"}}}

// adminCall sends a request of one of the runtime's administrators, Ken,
// or, with role "", of Ken as no administrator.
func (f *fixture) adminCall(t *testing.T, role, method, path, body string, headers ...string) answer {
	t.Helper()
	r := req{method: method, path: Prefix + path, token: f.core.assert(t, claims(ken, role, nil)), headers: headers}
	if body != "" {
		r.body = strings.NewReader(body)
		r.headers = append(r.headers, "Content-Type", "application/json")
	}
	return f.send(r)
}

// auditOf are the store's audit events of action.
func (f *fixture) auditOf(t *testing.T, action string) []store.AuditEvent {
	t.Helper()
	all, err := f.st.AuditEvents(context.Background(), at.AddDate(-1, 0, 0), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AuditEvent
	for _, e := range all {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// GET and PATCH /admin/settings: the runtime's administrators alone read
// and change OCR's setting; by default OCR is on, in the environment's
// languages; turned off, or in other languages installed, it is kept in
// the store, moving the registry's revision on so that every worker takes
// it, and audited; null takes the environment's languages again; a patch
// that changes nothing writes nothing.
func TestAdminSettings(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.OCR = installed })
	ctx := context.Background()
	for _, role := range []string{"", "instructor"} {
		wantRefused(t, f.adminCall(t, role, "GET", "admin/settings", ""), 403, CodeForbidden, ReasonNotAdmin)
		wantRefused(t, f.adminCall(t, role, "PATCH", "admin/settings", `{"ocr":{"enabled":false}}`), 403, CodeForbidden, ReasonNotAdmin)
	}
	if ev := f.auditOf(t, "settings.update"); len(ev) != 2 || ev[0].Outcome != ReasonNotAdmin || ev[0].TargetType != "site_setting" ||
		ev[0].TargetID != "ocr" || ev[0].ActorID != ken {
		t.Errorf("the refusals' audit: %+v", ev)
	}
	var got Settings
	a := f.adminCall(t, "admin", "GET", "admin/settings", "")
	wantSecured(t, a, "no-store")
	a.decode(t, &got)
	if a.code != 200 || !strings.Contains(a.body, `"unavailable_reason":null`) || !strings.Contains(a.body, `"updated_at":null`) {
		t.Fatalf("%d %s", a.code, a.body)
	}
	o := got.OCR
	if !o.Available || !o.Enabled || strings.Join(o.Languages, "+") != "chi_tra+eng" || strings.Join(o.DefaultLanguages, "+") != "chi_tra+eng" ||
		strings.Join(o.AvailableLanguages, " ") != "chi_sim chi_tra eng" {
		t.Errorf("the defaults: %+v", o)
	}

	rev, err := f.st.RegistryRev(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a = f.adminCall(t, "admin", "PATCH", "admin/settings", `{"ocr":{"enabled":false}}`)
	a.decode(t, &got)
	if a.code != 200 || got.OCR.Enabled || got.OCR.UpdatedBy == nil || *got.OCR.UpdatedBy != ken || got.OCR.UpdatedAt == nil || !got.OCR.UpdatedAt.Equal(at) {
		t.Fatalf("turned off: %d %s", a.code, a.body)
	}
	if r, _ := f.st.RegistryRev(ctx); r <= rev {
		t.Errorf("the revision did not move on: %d, was %d", r, rev)
	}
	a = f.adminCall(t, "admin", "PATCH", "admin/settings", `{"ocr":{"enabled":true,"languages":["eng","chi_sim"]}}`)
	a.decode(t, &got)
	if a.code != 200 || !got.OCR.Enabled || strings.Join(got.OCR.Languages, "+") != "eng+chi_sim" {
		t.Fatalf("in other languages: %d %s", a.code, a.body)
	}
	settings, err := f.st.SiteSettings(ctx)
	if err != nil || len(settings) != 1 || settings[0].Name != store.SettingOCR || settings[0].UpdatedBy != ken {
		t.Fatalf("the store: %+v %v", settings, err)
	}
	var stored map[string]any
	if json.Unmarshal(settings[0].Value, &stored) != nil || stored["enabled"] != true || len(stored["languages"].([]any)) != 2 {
		t.Errorf("stored: %s", settings[0].Value)
	}
	f.adminCall(t, "admin", "GET", "admin/settings", "").decode(t, &got)
	if strings.Join(got.OCR.Languages, "+") != "eng+chi_sim" {
		t.Errorf("read back: %+v", got.OCR)
	}
	ev := f.auditOf(t, "settings.update")
	if len(ev) != 4 || ev[3].Outcome != "ok" || !strings.Contains(string(ev[3].Detail), `"changed":["ocr.enabled","ocr.languages"]`) ||
		!strings.Contains(string(ev[3].Detail), `"languages":["eng","chi_sim"]`) {
		t.Errorf("the audit: %+v", ev)
	}

	// Nothing changed: nothing written, nothing audited.
	rev, _ = f.st.RegistryRev(ctx)
	for _, body := range []string{`{}`, `{"ocr":{}}`, `{"ocr":{"enabled":true}}`, `{"ocr":{"languages":["eng","chi_sim"]}}`} {
		if a := f.adminCall(t, "admin", "PATCH", "admin/settings", body); a.code != 200 {
			t.Errorf("%s: %d %s", body, a.code, a.body)
		}
	}
	if r, _ := f.st.RegistryRev(ctx); r != rev || len(f.auditOf(t, "settings.update")) != 4 {
		t.Errorf("a patch of nothing wrote: rev %d, was %d", r, rev)
	}

	// null: the environment's languages again.
	f.adminCall(t, "admin", "PATCH", "admin/settings", `{"ocr":{"languages":null}}`).decode(t, &got)
	if strings.Join(got.OCR.Languages, "+") != "chi_tra+eng" || !got.OCR.Enabled {
		t.Errorf("languages null: %+v", got.OCR)
	}
}

// PATCH /admin/settings refuses what is not a setting, at its pointer, and
// writes nothing for it: a language not installed here, one named twice,
// none, a list that is none, an enabled that is not true or false, and
// members it does not take.
func TestAdminSettingsRefuses(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.OCR = installed })
	for _, tc := range []struct {
		body, reason, field string
	}{
		{`{"ocr":{"languages":["jpn"]}}`, ReasonInvalidField, "/ocr/languages/0"},
		{`{"ocr":{"languages":["eng","osd"]}}`, ReasonInvalidField, "/ocr/languages/1"},
		{`{"ocr":{"languages":["eng","eng"]}}`, ReasonInvalidField, "/ocr/languages/1"},
		{`{"ocr":{"languages":[]}}`, ReasonInvalidField, "/ocr/languages"},
		{`{"ocr":{"languages":"eng"}}`, ReasonInvalidField, "/ocr/languages"},
		{`{"ocr":{"languages":[1]}}`, ReasonInvalidField, "/ocr/languages"},
		{`{"ocr":{"enabled":"yes"}}`, ReasonInvalidField, "/ocr/enabled"},
		{`{"ocr":{"enabled":null}}`, ReasonInvalidField, "/ocr/enabled"},
		{`{"ocr":{"dpi":300}}`, ReasonUnknownField, "/ocr/dpi"},
		{`{"ocr":true}`, ReasonInvalidField, "/ocr"},
		{`{"school":{}}`, ReasonUnknownField, "/school"},
	} {
		e := wantRefused(t, f.adminCall(t, "admin", "PATCH", "admin/settings", tc.body), 400, CodeInvalidArgument, tc.reason)
		if e.Details["field"] != tc.field {
			t.Errorf("%s: field %v, want %s", tc.body, e.Details["field"], tc.field)
		}
	}
	if s, _ := f.st.SiteSettings(context.Background()); len(s) != 0 {
		t.Errorf("a refused patch wrote: %+v", s)
	}
}

// Where the environment does not let OCR run, the administrators are
// told why, and cannot turn it on or choose its languages; turning it off
// is kept, for when the operator lets it run.
func TestAdminSettingsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ocr    OCR
		reason string
		detail string
	}{
		{"OCR=off", fakeOCR{ocr.Capability{Reason: ocr.ReasonTurnedOff, Default: []string{"chi_sim", "chi_tra", "eng"}}}, ocr.ReasonTurnedOff, ""},
		{"its programs missing", fakeOCR{ocr.Capability{Reason: ocr.ReasonNotInstalled, Detail: "ocr: not available: tesseract not installed",
			Default: []string{"chi_sim", "chi_tra", "eng"}}}, ocr.ReasonNotInstalled, "ocr: not available: tesseract not installed"},
		{"no OCR given the API", nil, ocr.ReasonNotInstalled, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(o *Options) { o.OCR = tc.ocr })
			var got Settings
			a := f.adminCall(t, "admin", "GET", "admin/settings", "")
			a.decode(t, &got)
			o := got.OCR
			if a.code != 200 || o.Available || o.UnavailableReason == nil || *o.UnavailableReason != tc.reason ||
				(tc.detail == "") != (o.UnavailableDetail == nil) || len(o.AvailableLanguages) != 0 ||
				!strings.Contains(a.body, `"available_languages":[]`) || strings.Join(o.Languages, "+") != "chi_sim+chi_tra+eng" {
				t.Fatalf("%d %s", a.code, a.body)
			}
			for body, field := range map[string]string{`{"ocr":{"enabled":true}}`: "/ocr/enabled", `{"ocr":{"languages":["eng"]}}`: "/ocr/languages"} {
				e := wantRefused(t, f.adminCall(t, "admin", "PATCH", "admin/settings", body), 422, CodeFailedPrecondition, ReasonOCRUnavailable)
				if e.Details["field"] != field {
					t.Errorf("%s: field %v", body, e.Details["field"])
				}
			}
			a = f.adminCall(t, "admin", "PATCH", "admin/settings", `{"ocr":{"enabled":false,"languages":null}}`)
			a.decode(t, &got)
			if a.code != 200 || got.OCR.Enabled || got.OCR.Available {
				t.Errorf("turned off: %d %s", a.code, a.body)
			}
		})
	}
}
