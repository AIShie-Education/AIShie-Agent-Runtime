package main

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/transcribe"
)

// TestCheckTranscriber: check says whether the transcriber may run here,
// and why not; with TRANSCRIBE=on, a runtime where it cannot does not
// pass.
func TestCheckTranscriber(t *testing.T) {
	examples := []string{"CONFIG", "../../examples/runtime.yaml,../../examples/agents"}
	code, out, errs := runCmd(t, env(append(examples, "TRANSCRIBE", "off")...), "check")
	if code != exitOK || !strings.Contains(out, "transcriber: off (TRANSCRIBE=off)") {
		t.Errorf("TRANSCRIBE=off: %d\n%s%s", code, out, errs)
	}
	code, out, errs = runCmd(t, env(examples...), "check")
	if code != exitOK || !strings.Contains(out, "transcriber: off: the transcriber needs DATABASE_URL (the store in PostgreSQL), KMS_KEY_ID and CORE_BASE_URL") {
		t.Errorf("TRANSCRIBE=auto, without its store: %d\n%s%s", code, out, errs)
	}
	code, out, errs = runCmd(t, env(append(examples, "TRANSCRIBE", "on")...), "check")
	if code != exitFailure || !strings.Contains(errs, "TRANSCRIBE=on, and the transcriber cannot run here: the transcriber needs DATABASE_URL") {
		t.Errorf("TRANSCRIBE=on, without its store: %d\n%s%s", code, out, errs)
	}
	code, out, errs = runCmd(t, env(append(examples, "TRANSCRIBE", "sometimes")...), "check")
	if code != exitFailure || !strings.Contains(errs, `TRANSCRIBE: "sometimes" is not auto, on or off`) {
		t.Errorf("TRANSCRIBE=sometimes: %d\n%s%s", code, out, errs)
	}
}

// TestTranscriberWithTheRegistry: with the store, its key and a Core with
// the service, TRANSCRIBE=on passes check, which says whether the site
// turns the transcriber on and with which offer; each build puts the
// site's setting in force in the worker's transcriber, with the plan's
// offer it names, until the plan no longer offers it.
func TestTranscriberWithTheRegistry(t *testing.T) {
	w := newRegistryWorld(t)
	w.siteOffer(t, "fast", "deepseek-chat")
	getenv := env("CONFIG", t.TempDir(), "DATABASE_URL", w.dbURL, "CORE_BASE_URL", w.coreURL, "KMS_KEY_ID", w.kms, "TRANSCRIBE", "on")
	code, out, errs := runCmd(t, getenv, "check")
	if code != exitOK || !strings.Contains(out, "transcriber: off in the site's settings; the site's administrators turn it on") {
		t.Errorf("check, the site's setting unset: %d\n%s%s", code, out, errs)
	}
	put := func(v string) {
		t.Helper()
		if err := w.st.PutSiteSetting(t.Context(), store.SiteSetting{Name: store.SettingTranscription, Value: json.RawMessage(v)}); err != nil {
			t.Fatal(err)
		}
	}
	put(`{"enabled": true, "offer": "fast", "per_day_pages": 100}`)
	code, out, errs = runCmd(t, getenv, "check")
	if want := `transcriber: on in the site's settings, with the plan's offer "fast" (deepseek-chat); 2 at once, 300 pages a document at ` +
		`most, 100 pages a day`; code != exitOK || !strings.Contains(out, want) {
		t.Errorf("check, turned on: %d\n%s%s", code, out, errs)
	}

	h := &hosting{env: config.Env{CoreBaseURL: w.coreURL}, pg: w.st, log: slog.New(slog.DiscardHandler), yaml: &config.Config{Dir: "/etc/aishie"}}
	tr := transcribe.New(transcribe.Options{Store: w.st, Keeps: true, CoreBaseURL: w.coreURL})
	h.setTranscriber(tr)
	build := func() transcribe.Setting {
		t.Helper()
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, _, err := h.build(t.Context()); err != nil {
			t.Fatal(err)
		}
		return tr.Setting()
	}
	if st := build(); !st.Site.Enabled || st.Offer == nil || st.Offer.ID != "fast" || !st.Offer.Site || *st.Site.PerDayPages != 100 ||
		st.Dir != "/etc/aishie" {
		t.Errorf("the setting in force: %+v", st)
	}
	if err := w.st.DeleteSchoolOffer(t.Context(), "fast", 0); err != nil {
		t.Fatal(err)
	}
	if st := build(); !st.Site.Enabled || st.Site.Offer != "fast" || st.Offer != nil {
		t.Errorf("the offer withdrawn: %+v", st)
	}
	put(`{"enabled": false, "offer": "fast"}`)
	if st := build(); st.Site.Enabled {
		t.Errorf("turned off: %+v", st)
	}
}
