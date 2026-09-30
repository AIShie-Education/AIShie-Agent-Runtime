package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/transcribe"
)

// transcribeCheckTimeout bounds the reading of Core's catalogue that
// TRANSCRIBE=on, and check, do before the transcriber starts.
const transcribeCheckTimeout = 30 * time.Second

// newTranscriber is the worker's transcriber (TRANSCRIBE; docs/deploying.md,
// docs/design.md §12), which the site's setting is put in force in and run
// runs: off with TRANSCRIBE=off, or without the store in PostgreSQL, the
// key that seals secrets or CORE_BASE_URL, saying why; with
// TRANSCRIBE=auto, the default, it runs whenever the site's administrators
// turn it on; with TRANSCRIBE=on, an error where it cannot run (those
// missing, or a Core from before the transcription service, or one that
// does not answer), and the runtime does not start.
func newTranscriber(ctx context.Context, env config.Env, o transcribe.Options) (*transcribe.Service, error) {
	o.Mode, o.CoreBaseURL, o.PartPages = env.Transcribe, env.CoreBaseURL, env.PDFPartPages
	t := transcribe.New(o)
	st := t.Status()
	switch {
	case env.Transcribe == config.TranscribeOff:
		o.Log.Info("the transcriber is off (TRANSCRIBE=off): no text version is transcribed here, whatever the site's settings say")
	case !st.Available && env.Transcribe == config.TranscribeOn:
		return nil, errors.New(st.Detail)
	case !st.Available:
		o.Log.Info("the transcriber cannot run here, whatever the site's settings say", "why", st.Detail)
	case env.Transcribe == config.TranscribeOn:
		ctx, cancel := context.WithTimeout(ctx, transcribeCheckTimeout)
		defer cancel()
		st, err := t.Check(ctx)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%s", redact.String(err.Error()))
		case !st.Available:
			return nil, errors.New(st.Detail)
		}
	}
	return t, nil
}

// transcriberState is where the transcriber stands, for the log line of
// the start.
func transcriberState(st transcribe.Status) string {
	switch {
	case !st.Available:
		return "off: " + st.Detail
	case st.Enabled:
		return "on"
	}
	return "off in the site's settings"
}

// checkTranscriber is what check says of the transcriber: whether it may
// run here, whether the site turns it on, and with which offer of the
// plan in force; Core's catalogue is read where TRANSCRIBE=on or the site
// turns it on. st is the store in PostgreSQL, nil where there is none.
// err is TRANSCRIBE=on where it cannot run: check fails.
func checkTranscriber(ctx context.Context, env config.Env, cfg *config.Config, st store.Store, client *http.Client) (string, error) {
	t := transcribe.New(transcribe.Options{Mode: env.Transcribe, Keeps: st != nil && env.KMSKeyID != "", Store: st,
		CoreBaseURL: env.CoreBaseURL, CoreHTTP: client, Log: slog.New(slog.DiscardHandler)})
	status := t.Status()
	site := cfg.Runtime.Site.Transcription
	switch {
	case env.Transcribe == config.TranscribeOff:
		return "off (TRANSCRIBE=off)", nil
	case !status.Available && env.Transcribe == config.TranscribeOn:
		return "", errors.New(status.Detail)
	case !status.Available:
		return "off: " + status.Detail, nil
	case env.Transcribe == config.TranscribeOn || site.Enabled:
		ctx, cancel := context.WithTimeout(ctx, transcribeCheckTimeout)
		defer cancel()
		status, err := t.Check(ctx)
		switch {
		case err != nil && env.Transcribe == config.TranscribeOn:
			return "", fmt.Errorf("%s", redact.String(err.Error()))
		case err != nil:
			return "Core's catalogue could not be read: " + redact.String(err.Error()), nil
		case !status.Available && env.Transcribe == config.TranscribeOn:
			return "", errors.New(status.Detail)
		case !status.Available:
			return "off: " + status.Detail, nil
		}
	}
	if !site.Enabled {
		return "off in the site's settings; the site's administrators turn it on", nil
	}
	offer := "no offer chosen: it claims nothing"
	if site.Offer != "" {
		offer = fmt.Sprintf("the offer %q is not in the plan: it claims nothing", site.Offer)
		for _, o := range cfg.Runtime.School.Offers {
			if o.ID == site.Offer {
				offer = fmt.Sprintf("with the plan's offer %q (%s)", o.ID, o.Model)
			}
		}
	}
	quota := "no daily limit of pages"
	if site.PerDayPages != nil {
		quota = fmt.Sprintf("%d pages a day", *site.PerDayPages)
	}
	return fmt.Sprintf("on in the site's settings, %s; %d at once, %d pages a document at most, %s", offer, site.Slots(), site.Pages(), quota), nil
}
