package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/transcribe"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// transcribeRuntime is a school's plan of three offers: one whose model
// takes PDFs and is priced, one whose model takes PDFs and is not, and one
// whose model takes no files.
func transcribeRuntime() config.Runtime {
	return config.Runtime{School: config.School{Offers: []config.SchoolOffer{
		{ID: "mini", Label: "School AI (mini)", Adapter: "openai_chat", Model: "gpt-4.1-mini", KeyRef: "secret://school/keys/openai"},
		{ID: "nano", Label: "School AI (nano)", Adapter: "openai_chat", Model: "gpt-4.1-nano", KeyRef: "secret://school/keys/openai"},
		{ID: "chat", Label: "School AI (text)", Adapter: "openai_chat", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com",
			KeyRef: "secret://school/keys/deepseek"},
	}}}
}

// newTranscribeWorld is a model world whose worker has a transcriber, on
// the API's store, against the fake Core, which has the transcription
// service; mode is TRANSCRIBE.
func newTranscribeWorld(t *testing.T, mode string) (*hostWorld, *transcribe.Service) {
	t.Helper()
	return newTranscribeWorldOf(t, mode, fakecore.Options{})
}

// newTranscribeWorldOf is a transcribe world against a fake Core of o.
func newTranscribeWorldOf(t *testing.T, mode string, o fakecore.Options) (*hostWorld, *transcribe.Service) {
	t.Helper()
	h, _, _ := newModelWorldOf(t, transcribeRuntime(), o)
	tr := transcribe.New(transcribe.Options{Mode: mode, Store: h.st, Keeps: true, CoreBaseURL: h.srv.URL, CoreHTTP: h.srv.Client(),
		Now: h.clock})
	h.s.o.Transcriber = tr
	return h, tr
}

// transcriptionOf is the transcription member of an answer of the
// settings, or the answer itself where it is the member.
func transcriptionOf(t *testing.T, a answer) TranscriptionSettings {
	t.Helper()
	var s Settings
	a.decode(t, &s)
	if s.Transcription.State == "" {
		a.decode(t, &s.Transcription)
	}
	return s.Transcription
}

// noServiceToken fails when anything the API said, logged or audited
// holds the token.
func (h *hostWorld) noServiceToken(token string, answers ...answer) {
	h.t.Helper()
	var b strings.Builder
	for _, a := range answers {
		b.WriteString(a.body)
	}
	b.WriteString(h.log.String())
	events, err := h.st.AuditEvents(context.Background(), at.AddDate(-1, 0, 0), 1000)
	h.ok(err)
	for _, e := range events {
		b.Write(e.Detail)
	}
	// aissvc_, the public prefix of 12, _, the secret.
	secret := token[len("aissvc_")+13:]
	if strings.Contains(b.String(), token) || strings.Contains(b.String(), secret[:12]) {
		h.t.Errorf("the service's token was shown, logged or audited")
	}
}

// GET and PATCH /admin/settings' transcription: the runtime's
// administrators alone read and change it; off by default, with its
// defaults; turned on, it says what it lacks (the credential), then which
// offer; an offer the plan has not, or whose model takes no files, is
// refused, as are limits out of their bounds; the change is kept, moving
// the registry's revision on, and audited as transcription_settings.update;
// where the environment does not let it run, it is not turned on.
func TestTranscriptionSettings(t *testing.T) {
	h, _ := newTranscribeWorld(t, config.TranscribeAuto)
	ctx := context.Background()
	for _, role := range []string{"", "instructor"} {
		wantRefused(t, h.adminCall(t, role, "PATCH", "admin/settings", `{"transcription":{"enabled":true}}`), 403, CodeForbidden, ReasonNotAdmin)
		wantRefused(t, h.adminCall(t, role, "PUT", "admin/transcription/credential", `{"token":"x"}`), 403, CodeForbidden, ReasonNotAdmin)
		wantRefused(t, h.adminCall(t, role, "DELETE", "admin/transcription/credential", ""), 403, CodeForbidden, ReasonNotAdmin)
		wantRefused(t, h.adminCall(t, role, "GET", "admin/transcription/jobs", ""), 403, CodeForbidden, ReasonNotAdmin)
	}
	// Refused before its body is read, a patch is audited as the route's.
	if ev := h.auditOf(t, "settings.update"); len(ev) != 2 || ev[0].Outcome != ReasonNotAdmin {
		t.Errorf("the refusals' audit: %+v", ev)
	}
	if ev := h.auditOf(t, "transcription_credential.delete"); len(ev) != 2 || ev[0].Outcome != ReasonNotAdmin {
		t.Errorf("the refusals' audit: %+v", ev)
	}

	a := h.admin("GET", "admin/settings", "")
	wantSecured(t, a, "no-store")
	v := transcriptionOf(t, a)
	if a.code != 200 || !v.Available || v.Enabled || v.Offer != nil || v.OfferStatus != nil || v.MaxPages != 300 || v.PerDayPages != nil ||
		v.Concurrency != 2 || v.Credential.Status != CredentialNone || v.State != TranscriptionOff || v.BlockedReason != nil ||
		v.Today != (TranscriptionToday{CostUSD: "0.000000"}) || v.UpdatedAt != nil ||
		!strings.Contains(a.body, `"transcription":{"available":true,"unavailable_reason":null,"unavailable_detail":null,"enabled":false,"offer":null`) {
		t.Fatalf("the defaults: %d %s", a.code, a.body)
	}

	rev, err := h.st.RegistryRev(ctx)
	h.ok(err)
	a = h.admin("PATCH", "admin/settings", `{"transcription":{"enabled":true}}`)
	if v = transcriptionOf(t, a); a.code != 200 || !v.Enabled || v.State != TranscriptionBlocked || *v.BlockedReason != transcribe.BlockedNoCredential ||
		v.UpdatedAt == nil || *v.UpdatedBy != ken {
		t.Fatalf("turned on: %d %s", a.code, a.body)
	}
	if next, _ := h.st.RegistryRev(ctx); next <= rev {
		t.Error("the registry's revision did not move")
	}
	ev := h.auditOf(t, "transcription_settings.update")
	if len(ev) != 1 || ev[0].Outcome != "ok" || ev[0].TargetType != "site_setting" || !strings.Contains(string(ev[0].Detail), `"changed":["transcription.enabled"]`) {
		t.Errorf("the change's audit: %+v", ev)
	}
	if len(h.auditOf(t, "settings.update")) != 2 {
		t.Error("a change of the transcriber's alone is audited as OCR's")
	}

	for _, c := range []struct {
		body, field, reason string
		status              int
	}{
		{`{"transcription":{"enabled":null}}`, "/transcription/enabled", ReasonInvalidField, 400},
		{`{"transcription":{"offer":"nope"}}`, "/transcription/offer", ReasonInvalidField, 400},
		{`{"transcription":{"offer":7}}`, "/transcription/offer", ReasonInvalidField, 400},
		{`{"transcription":{"offer":"chat"}}`, "/transcription/offer", ReasonOfferNoFileInput, 422},
		{`{"transcription":{"max_pages":0}}`, "/transcription/max_pages", ReasonInvalidField, 400},
		{`{"transcription":{"max_pages":5001}}`, "/transcription/max_pages", ReasonInvalidField, 400},
		{`{"transcription":{"per_day_pages":1000001}}`, "/transcription/per_day_pages", ReasonInvalidField, 400},
		{`{"transcription":{"concurrency":9}}`, "/transcription/concurrency", ReasonInvalidField, 400},
		{`{"transcription":{"concurrency":1.5}}`, "/transcription/concurrency", ReasonInvalidField, 400},
		{`{"transcription":{"model":"x"}}`, "/transcription/model", ReasonUnknownField, 400},
		{`{"transcription":true}`, "/transcription", ReasonInvalidField, 400},
	} {
		code := CodeInvalidArgument
		if c.status == 422 {
			code = CodeFailedPrecondition
		}
		if e := wantRefused(t, h.admin("PATCH", "admin/settings", c.body), c.status, code, c.reason); e.Details["field"] != c.field {
			t.Errorf("%s: %+v", c.body, e)
		}
	}

	a = h.admin("PATCH", "admin/settings", `{"transcription":{"offer":"mini","max_pages":50,"per_day_pages":1000,"concurrency":4}}`)
	if v = transcriptionOf(t, a); a.code != 200 || *v.Offer != "mini" || *v.OfferStatus != OfferStatusOK || v.MaxPages != 50 || *v.PerDayPages != 1000 ||
		v.Concurrency != 4 {
		t.Fatalf("an offer and limits: %d %s", a.code, a.body)
	}
	site, err := h.st.SiteSettings(ctx)
	h.ok(err)
	for _, s := range site {
		if s.Name == store.SettingTranscription &&
			string(s.Value) != `{"concurrency": 4, "enabled": true, "max_pages": 50, "offer": "mini", "per_day_pages": 1000}` &&
			string(s.Value) != `{"enabled":true,"offer":"mini","max_pages":50,"per_day_pages":1000,"concurrency":4}` {
			t.Errorf("as stored: %s", s.Value)
		}
	}
	a = h.admin("PATCH", "admin/settings", `{"transcription":{"offer":"nano","max_pages":null,"per_day_pages":null,"concurrency":null}}`)
	if v = transcriptionOf(t, a); *v.OfferStatus != OfferStatusNotPriced || v.MaxPages != 300 || v.PerDayPages != nil || v.Concurrency != 2 {
		t.Errorf("an offer not priced, the limits' defaults: %s", a.body)
	}
	// A patch that changes nothing writes and audits nothing.
	n := len(h.auditOf(t, "transcription_settings.update"))
	if a = h.admin("PATCH", "admin/settings", `{"transcription":{"offer":"nano"}}`); a.code != 200 || len(h.auditOf(t, "transcription_settings.update")) != n {
		t.Errorf("no change: %d, audited", a.code)
	}
	// A patch of both: each its own event.
	a = h.admin("PATCH", "admin/settings", `{"ocr":{"enabled":false},"transcription":{"offer":"mini"}}`)
	if a.code != 200 || len(h.auditOf(t, "settings.update")) != 3 || len(h.auditOf(t, "transcription_settings.update")) != n+1 {
		t.Errorf("both: %d %s", a.code, a.body)
	}

	// The site's offer, turned off: kept, and the transcriber says so.
	key, err := h.v.Seal(ctx, store.Secret{ID: vault.NewSecretID(), TenantID: store.SchoolTenantID, Kind: store.SecretModelKey},
		"sk-school-0123456789abcdefghij")
	h.ok(err)
	_, err = h.st.CreateSchoolOffer(ctx, store.SchoolOffer{ID: "site-gemini", Label: "Site Gemini", Adapter: "gemini", Provider: "gemini",
		Model: "gemini-2.5-flash-lite", Enabled: false, KeySecretID: key.ID, KeyHint: key.Hint, CreatedBy: "admin"}, key)
	h.ok(err)
	a = h.admin("PATCH", "admin/settings", `{"transcription":{"offer":"site-gemini"}}`)
	if v = transcriptionOf(t, a); a.code != 200 || *v.OfferStatus != OfferStatusDisabled || v.State != TranscriptionBlocked ||
		*v.BlockedReason != transcribe.BlockedNoCredential {
		t.Errorf("a site's offer turned off: %d %s", a.code, a.body)
	}

	// TRANSCRIBE=off: not available, and not turned on.
	off, _ := newTranscribeWorld(t, config.TranscribeOff)
	a = off.admin("GET", "admin/settings", "")
	if v = transcriptionOf(t, a); v.Available || *v.UnavailableReason != transcribe.ReasonOperatorOff || *v.UnavailableDetail != "TRANSCRIBE=off" ||
		v.State != TranscriptionOff {
		t.Errorf("TRANSCRIBE=off: %s", a.body)
	}
	e := wantRefused(t, off.admin("PATCH", "admin/settings", `{"transcription":{"enabled":true}}`), 422, CodeFailedPrecondition,
		ReasonTranscriptionUnavailable)
	if e.Details["field"] != "/transcription/enabled" {
		t.Errorf("TRANSCRIBE=off, turned on: %+v", e)
	}
	if a = off.admin("PATCH", "admin/settings", `{"transcription":{"enabled":false,"offer":"mini"}}`); a.code != 200 {
		t.Errorf("TRANSCRIBE=off, set off: %d %s", a.code, a.body)
	}
	// No transcriber at all (an API without the worker's).
	none := newFixture(t, nil)
	a = none.adminCall(t, "admin", "GET", "admin/settings", "")
	if v = transcriptionOf(t, a); a.code != 200 || v.Available || *v.UnavailableReason != transcribe.ReasonOperatorOff {
		t.Errorf("no transcriber: %d %s", a.code, a.body)
	}
}

// PUT and DELETE /admin/transcription/credential: a service token of
// Core's is tried with Core by a call that claims nothing, sealed under
// the tenant site, and kept, the transcriber then running; a token of
// another shape is refused, one Core refuses is not kept, nor one Core
// cannot be asked about; skip_test keeps it untested; forgotten, the
// transcriber is blocked again. The token is never shown, logged or
// audited; its hint is.
func TestTranscriptionCredential(t *testing.T) {
	h, _ := newTranscribeWorld(t, config.TranscribeAuto)
	ctx := context.Background()
	h.admin("PATCH", "admin/settings", `{"transcription":{"enabled":true,"offer":"mini"}}`)
	tok := h.fc.IssueServiceToken("runtime")
	var answers []answer
	for _, c := range []struct {
		body, field, reason string
	}{
		{`{}`, "/token", ReasonMissingField},
		{`{"token":"sk-proj-0123456789"}`, "/token", ReasonInvalidField},
		{`{"token":"` + h.helper.Token + `"}`, "/token", ReasonInvalidField},
		{`{"token":"aissvc_short"}`, "/token", ReasonInvalidField},
		{`{"token":"` + tok.Token + `","credential_id":""}`, "/credential_id", ReasonInvalidField},
	} {
		a := h.admin("PUT", "admin/transcription/credential", c.body)
		answers = append(answers, a)
		if e := wantRefused(t, a, 400, CodeInvalidArgument, c.reason); e.Details["field"] != c.field {
			t.Errorf("%s: %+v", c.body, e)
		}
	}
	a := h.admin("PUT", "admin/transcription/credential", `{"token":"`+tok.Token+`","credential_id":"`+tok.CredentialID+`"}`)
	answers = append(answers, a)
	v := transcriptionOf(t, a)
	if a.code != 200 || v.Credential.Status != CredentialOK || v.Credential.Hint == nil || !strings.HasPrefix(*v.Credential.Hint, "aissvc_") ||
		*v.Credential.CredentialID != tok.CredentialID || *v.Credential.SetBy != ken || v.Credential.LastOKAt == nil || v.State != TranscriptionRunning ||
		v.BlockedReason != nil {
		t.Fatalf("a credential: %d %s", a.code, a.body)
	}
	cred, err := h.st.TranscriptionCredential(ctx)
	h.ok(err)
	if sec := h.secretOf(cred.SecretID); sec.TenantID != store.SiteTenantID || sec.Kind != store.SecretCoreToken {
		t.Errorf("the credential's secret: %+v", sec)
	} else if opened, err := h.v.Open(ctx, &sec); err != nil || opened != tok.Token {
		t.Errorf("the credential is not sealed as the site's: %v", err)
	}
	ev := h.auditOf(t, "transcription_credential.set")
	if last := ev[len(ev)-1]; last.Outcome != "ok" || last.TargetID != tok.CredentialID || !strings.Contains(string(last.Detail), `"hint":"aissvc_`) {
		t.Errorf("the audit: %+v", last)
	}
	// The credential was tried by a renewal, which claims nothing, and
	// nothing else.
	for _, c := range h.fc.Calls() {
		if strings.HasPrefix(c.Tool, "document_text_") && c.Tool != core.ToolTextRenew {
			t.Errorf("the credential's test called %s", c.Tool)
		}
	}

	// Revoked in Core: refused (401), and the one kept stays.
	old := h.fc.IssueServiceToken("old")
	h.ok(h.fc.RevokeServiceToken(old.CredentialID))
	a = h.admin("PUT", "admin/transcription/credential", `{"token":"`+old.Token+`"}`)
	answers = append(answers, a)
	if e := wantRefused(t, a, 422, CodeFailedPrecondition, ReasonCredentialRejected); e.Details["status"] != float64(401) {
		t.Errorf("revoked: %+v", e)
	}
	if now, _ := h.st.TranscriptionCredential(ctx); now.SecretID != cred.SecretID {
		t.Error("a credential Core refused replaced the one kept")
	}
	// skip_test: kept untried.
	a = h.admin("PUT", "admin/transcription/credential", `{"token":"`+old.Token+`","skip_test":true}`)
	answers = append(answers, a)
	if v = transcriptionOf(t, a); a.code != 200 || v.Credential.Status != CredentialUntested || v.Credential.LastOKAt != nil {
		t.Errorf("skip_test: %d %s", a.code, a.body)
	}
	if _, err := h.st.Secret(ctx, cred.SecretID); err == nil {
		t.Error("the credential replaced is still kept")
	}
	// Refused by the transcriber since: it says so.
	now, err := h.st.TranscriptionCredential(ctx)
	h.ok(err)
	h.ok(h.st.NoteTranscriptionCredential(ctx, now.SecretID, false, at, "Core answered 401 (revoked or expired)"))
	a = h.admin("GET", "admin/settings", "")
	if v = transcriptionOf(t, a); v.Credential.Status != CredentialRejected || *v.Credential.LastError != "Core answered 401 (revoked or expired)" ||
		v.State != TranscriptionBlocked || *v.BlockedReason != transcribe.BlockedCredentialRejected {
		t.Errorf("rejected: %s", a.body)
	}
	// Core cannot be reached: nothing kept.
	url := h.s.o.CoreBaseURL
	h.s.o.CoreBaseURL = "http://127.0.0.1:1"
	a = h.admin("PUT", "admin/transcription/credential", `{"token":"`+tok.Token+`"}`)
	answers = append(answers, a)
	wantRefused(t, a, 503, CodeUnavailable, ReasonCoreUnavailable)
	h.s.o.CoreBaseURL = url

	a = h.admin("DELETE", "admin/transcription/credential", "")
	answers = append(answers, a)
	if v = transcriptionOf(t, a); a.code != 200 || v.Credential.Status != CredentialNone || *v.BlockedReason != transcribe.BlockedNoCredential {
		t.Errorf("forgotten: %d %s", a.code, a.body)
	}
	if _, err := h.st.Secret(ctx, now.SecretID); err == nil {
		t.Error("the credential forgotten is still kept")
	}
	if a = h.admin("DELETE", "admin/transcription/credential", ""); a.code != 200 || len(h.auditOf(t, "transcription_credential.delete")) != 1 {
		t.Errorf("forgotten again: %d, audited %d times", a.code, len(h.auditOf(t, "transcription_credential.delete")))
	}
	h.noServiceToken(tok.Token, answers...)
	h.noServiceToken(old.Token, answers...)
}

// The credential's try names no version's file, as it names no version:
// a nil uuid, where Core's renewal takes file_id (it requires it since
// AIShie-Core #54), and none where it does not (a Core from before #49,
// which refuses it). The token passes either way.
func TestTranscriptionCredentialTriedOfNoFile(t *testing.T) {
	for name, o := range map[string]fakecore.Options{"files": {}, "one_file_a_version": {WithoutFiles: true}} {
		t.Run(name, func(t *testing.T) {
			h, _ := newTranscribeWorldOf(t, config.TranscribeAuto, o)
			h.admin("PATCH", "admin/settings", `{"transcription":{"enabled":true,"offer":"mini"}}`)
			tok := h.fc.IssueServiceToken("runtime")
			a := h.admin("PUT", "admin/transcription/credential", `{"token":"`+tok.Token+`"}`)
			if v := transcriptionOf(t, a); a.code != 200 || v.Credential.Status != CredentialOK {
				t.Fatalf("the credential: %d %s", a.code, a.body)
			}
			tried := 0
			for _, c := range h.fc.Calls() {
				if c.Tool != core.ToolTextRenew {
					continue
				}
				tried++
				var args map[string]any
				if err := json.Unmarshal(c.Args, &args); err != nil {
					t.Fatal(err)
				}
				file, named := args["file_id"]
				if named != !o.WithoutFiles || named && file != uuid.Nil.String() || args["version_id"] != uuid.Nil.String() {
					t.Errorf("the try: %s", c.Args)
				}
			}
			if tried != 1 {
				t.Errorf("tried %d times", tried)
			}
		})
	}
}

// secretOf is the store's secret id.
func (h *hostWorld) secretOf(id string) store.Secret {
	h.t.Helper()
	s, err := h.st.Secret(context.Background(), id)
	h.ok(err)
	return *s
}

// The day's pages spent, the transcriber is blocked (quota_exhausted), and
// today's numbers say what it did.
func TestTranscriptionToday(t *testing.T) {
	h, _ := newTranscribeWorld(t, config.TranscribeAuto)
	ctx := context.Background()
	h.admin("PATCH", "admin/settings", `{"transcription":{"enabled":true,"offer":"mini","per_day_pages":10}}`)
	tok := h.fc.IssueServiceToken("runtime")
	h.admin("PUT", "admin/transcription/credential", `{"token":"`+tok.Token+`"}`)
	cost := int64(1_500_000)
	for i, status := range []string{store.JobDone, store.JobDone, store.JobFailed, store.JobSkipped} {
		pages := 5
		finished := at
		_, err := h.st.PutTranscriptionJob(ctx, store.TranscriptionJob{ID: fmt.Sprintf("trj_%d", i), VersionID: fmt.Sprintf("v%d", i), DocumentID: "d",
			CourseID: "c", LeaseID: "l", Status: status, ContentType: "application/pdf", ByteSize: 100, Pages: &pages, PagesSent: pages * (1 - i/2),
			Offer: "mini", Model: "gpt-4.1-mini", ModelCalls: 1, CostPUSD: &cost, Worker: "w", StartedAt: at.Add(-time.Minute), HeartbeatAt: at,
			FinishedAt: &finished})
		h.ok(err)
	}
	a := h.admin("GET", "admin/settings", "")
	if v := transcriptionOf(t, a); v.Today != (TranscriptionToday{Pages: 10, Documents: 2, Failed: 1, Skipped: 1, CostUSD: "0.000006"}) ||
		v.State != TranscriptionBlocked || *v.BlockedReason != transcribe.BlockedQuotaExhausted {
		t.Errorf("today: %s", a.body)
	}
}

// GET /admin/transcription/jobs: the runtime's record of the jobs, newest
// first, a page at a time, of one status or all; ids, counts and costs.
func TestTranscriptionJobs(t *testing.T) {
	h, _ := newTranscribeWorld(t, config.TranscribeAuto)
	ctx := context.Background()
	cost := int64(2_000_000)
	for i := range 5 {
		j := store.TranscriptionJob{ID: fmt.Sprintf("trj_%d", i), VersionID: fmt.Sprintf("v%d", i), DocumentID: "d", CourseID: "c", LeaseID: "l",
			Status: store.JobDone, ContentType: "application/pdf", ByteSize: 100, Offer: "mini", Model: "gpt-4.1-mini", Worker: "w",
			StartedAt: at.Add(time.Duration(i) * time.Minute), HeartbeatAt: at}
		switch i {
		case 1:
			j.Status, j.Reason = store.JobSkipped, transcribe.ReasonTooManyPages
		case 4:
			pages := 3
			j.Pages, j.PagesSent, j.ModelCalls, j.CostPUSD, j.InputTokens, j.OutputTokens = &pages, 3, 1, &cost, 900, 300
			j.FinishedAt = &at
			// A file of a version of several (AIShie-Core #49).
			j.FileID, j.Position = "f2", 2
		}
		_, err := h.st.PutTranscriptionJob(ctx, j)
		h.ok(err)
	}
	var l JobList
	a := h.admin("GET", "admin/transcription/jobs", "")
	wantSecured(t, a, "no-store")
	a.decode(t, &l)
	if a.code != 200 || len(l.Jobs) != 5 || l.Next != nil || l.Jobs[0].ID != "trj_4" || *l.Jobs[0].CostUSD != "0.000002" || *l.Jobs[0].Pages != 3 ||
		*l.Jobs[0].InputTokens != 900 || l.Jobs[0].FinishedAt == nil || l.Jobs[1].CostUSD != nil || l.Jobs[1].InputTokens != nil ||
		l.Jobs[1].Pages != nil || !strings.Contains(a.body, `"reason":null`) || *l.Jobs[0].FileID != "f2" || *l.Jobs[0].Position != 2 ||
		l.Jobs[1].FileID != nil || l.Jobs[1].Position != nil || !strings.Contains(a.body, `"file_id":null`) {
		t.Fatalf("all: %d %s", a.code, a.body)
	}
	a = h.admin("GET", "admin/transcription/jobs?limit=2", "")
	a.decode(t, &l)
	if len(l.Jobs) != 2 || l.Next == nil {
		t.Fatalf("a page: %s", a.body)
	}
	a = h.admin("GET", "admin/transcription/jobs?limit=2&after="+*l.Next, "")
	a.decode(t, &l)
	if len(l.Jobs) != 2 || l.Jobs[0].ID != "trj_2" || l.Next == nil {
		t.Errorf("the next page: %s", a.body)
	}
	a = h.admin("GET", "admin/transcription/jobs?status=skipped", "")
	a.decode(t, &l)
	if len(l.Jobs) != 1 || *l.Jobs[0].Reason != transcribe.ReasonTooManyPages {
		t.Errorf("skipped: %s", a.body)
	}
	for _, q := range []string{"limit=0", "limit=201", "status=lost", "after=x", "after=0", "sort=asc"} {
		reason := ReasonInvalidField
		if strings.HasPrefix(q, "sort") {
			reason = ReasonUnknownParameter
		}
		wantRefused(t, h.admin("GET", "admin/transcription/jobs?"+q, ""), 400, CodeInvalidArgument, reason)
	}
}

// GET /admin/costs: the transcriber's model calls are a line of their own
// kind, of no agent (the group transcription) and no tenant (the group
// site), on the school's key.
func TestCostsOfTranscription(t *testing.T) {
	h, _ := newTranscribeWorld(t, config.TranscribeAuto)
	ctx := context.Background()
	h.ok(h.st.RecordLLMCall(ctx, store.LLMCall{ID: "a1", At: at, TenantID: "t_ops", AgentID: "agt_ops", CourseID: "c", ConversationID: "x",
		MessageID: "m", Provider: "openai", Model: "gpt-4.1-mini", Input: 1000, Output: 100, CostPUSD: 1_000_000, KeySource: config.KeySchool}))
	h.ok(h.st.RecordLLMCall(ctx, store.LLMCall{ID: "t1", At: at, Kind: store.CallTranscription, Provider: "openai", Model: "gpt-4.1-mini",
		Input: 3000, Output: 900, CostPUSD: 2_000_000, KeySource: config.KeySchool}))
	var r CostReport
	h.admin("GET", "admin/costs?group=total", "").decode(t, &r)
	if r.Total.CostUSD != "0.000003" || len(r.Total.Lines) != 2 || r.Total.Lines[0].Kind != CostKindModelCalls || r.Total.Lines[1].Kind != CostKindTranscription ||
		r.Total.Lines[1].Calls != 1 || r.Total.Lines[1].CostUSD != "0.000002" || r.Total.Lines[1].Tokens.Input != 3000 {
		t.Errorf("the total: %+v", r.Total)
	}
	a := h.admin("GET", "admin/costs?group=agent", "")
	a.decode(t, &r)
	var seen bool
	for _, g := range r.Rows {
		if g.Key == store.CostKeyTranscription {
			seen = true
			if g.AgentID != nil || g.AgentName != nil || g.TenantID != nil || len(g.Lines) != 1 || g.Lines[0].Kind != CostKindTranscription {
				t.Errorf("by agent: %+v", g)
			}
		}
	}
	if !seen || len(r.Rows) != 2 {
		t.Errorf("by agent: %s", a.body)
	}
	a = h.admin("GET", "admin/costs?group=tenant", "")
	a.decode(t, &r)
	seen = false
	for _, g := range r.Rows {
		if g.Key == store.CostKeySite {
			seen = true
			if g.TenantID != nil || g.OwnerActorID != nil || len(g.Lines) != 1 || g.Lines[0].Kind != CostKindTranscription {
				t.Errorf("by tenant: %+v", g)
			}
		}
	}
	if !seen {
		t.Errorf("by tenant: %s", a.body)
	}
	h.admin("GET", "admin/costs?group=key_source", "").decode(t, &r)
	if len(r.Rows) != 1 || *r.Rows[0].KeySource != config.KeySchool || len(r.Rows[0].Lines) != 2 {
		t.Errorf("by key source: %+v", r.Rows)
	}
}

// GET /info's features.transcription: the transcriber of this worker, as
// the site's setting in force turns it on, with nothing stopping it.
func TestInfoTranscription(t *testing.T) {
	h, tr := newTranscribeWorld(t, config.TranscribeAuto)
	feature := func() bool {
		t.Helper()
		var info struct {
			Features map[string]bool `json:"features"`
		}
		a := h.get(Prefix+"info", "")
		a.decode(t, &info)
		if _, ok := info.Features["transcription"]; !ok {
			t.Fatalf("no features.transcription: %s", a.body)
		}
		return info.Features["transcription"]
	}
	if feature() {
		t.Error("off, and on in /info")
	}
	offer := transcribeRuntime().School.Offers[0]
	tr.Set(transcribe.Setting{Site: config.SiteTranscription{Enabled: true, Offer: "mini"}, Offer: &offer})
	if feature() {
		t.Error("with no credential, and on in /info")
	}
	tok := h.fc.IssueServiceToken("runtime")
	if a := h.admin("PUT", "admin/transcription/credential", `{"token":"`+tok.Token+`"}`); a.code != 200 {
		t.Fatalf("%d %s", a.code, a.body)
	}
	if !feature() {
		t.Error("running, and off in /info")
	}
}
