package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/rendition"
)

// renditionsCheckTimeout bounds the reading of Core's catalogue that
// RENDITIONS=on does before the worker starts.
const renditionsCheckTimeout = transcribeCheckTimeout

// newRenditions is the worker's renditions (RENDITIONS*; docs/deploying.md,
// docs/design.md §13), which run runs: the PDF of every Office and
// OpenDocument file Core keeps, made once by LibreOffice (conv, with
// RENDITIONS_TIMEOUT, in the sandbox of every conversion; nil where
// OFFICE_PDF is off or LibreOffice is not installed, as svc says), with the
// runtime's own credential in Core (credential, read at each call, its
// calls paced with the rest of the service's by bucket). Off with
// RENDITIONS=off, without LibreOffice or without CORE_BASE_URL, saying
// why; with RENDITIONS=auto, the default, on wherever it can run; with
// RENDITIONS=on, an error where it cannot (those missing, or a Core
// without renditions, or one that does not answer), and the runtime does
// not start. A credential missing or refused is no reason not to start:
// the worker claims nothing, says why, and tries it again.
func newRenditions(ctx context.Context, env config.Env, conv *office.Converter, svc *office.Service, client *http.Client,
	credential func(context.Context) (string, error), bucket *ratelimit.Bucket, m *metrics.Metrics, log *slog.Logger) (*rendition.Service, error) {
	cfg := env.Renditions.WithDefaults()
	o := rendition.Options{Config: cfg, CoreBaseURL: env.CoreBaseURL, CoreHTTP: client, Credential: credential, Bucket: bucket, Metrics: m, Log: log}
	if conv != nil {
		o.Converter = conv.WithTimeout(cfg.Timeout)
	} else if _, why := svc.Available(); why != "" {
		o.Unavailable = "LibreOffice does not convert here (OFFICE_PDF): " + why
	}
	r := rendition.New(o)
	st := r.Status()
	switch {
	case cfg.Mode == rendition.ModeOff:
		log.Info("no PDF renditions are made here (RENDITIONS=off): Office files Core keeps wait for another runtime to convert them")
	case !st.Available && cfg.Mode == rendition.ModeOn:
		return nil, errors.New(st.Detail)
	case !st.Available:
		log.Info("no PDF renditions are made here", "why", st.Detail)
	case cfg.Mode == rendition.ModeOn:
		ctx, cancel := context.WithTimeout(ctx, renditionsCheckTimeout)
		defer cancel()
		st, err := r.Check(ctx)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%s", redact.String(err.Error()))
		case !st.Available:
			return nil, errors.New(st.Detail)
		}
	}
	return r, nil
}

// checkRenditions is what check says of the renditions: whether they are
// made here, with which LibreOffice, how many at once and in how long;
// Core's catalogue is read where RENDITIONS=on. err is RENDITIONS=on where
// they cannot be made: check fails.
func checkRenditions(ctx context.Context, env config.Env, conv *office.Converter, why string, client *http.Client) (string, error) {
	cfg := env.Renditions.WithDefaults()
	o := rendition.Options{Config: cfg, CoreBaseURL: env.CoreBaseURL, CoreHTTP: client, Log: slog.New(slog.DiscardHandler),
		Credential: func(context.Context) (string, error) { return "", errors.New("check reads no credential") }}
	if conv != nil {
		o.Converter = conv.WithTimeout(cfg.Timeout)
	} else {
		o.Unavailable = why
	}
	r := rendition.New(o)
	st := r.Status()
	switch {
	case cfg.Mode == rendition.ModeOff:
		return "off (RENDITIONS=off)", nil
	case !st.Available && cfg.Mode == rendition.ModeOn:
		return "", errors.New(st.Detail)
	case !st.Available:
		return "off: " + st.Detail, nil
	case cfg.Mode == rendition.ModeOn:
		ctx, cancel := context.WithTimeout(ctx, renditionsCheckTimeout)
		defer cancel()
		st, err := r.Check(ctx)
		switch {
		case err != nil:
			return "", fmt.Errorf("%s", redact.String(err.Error()))
		case !st.Available:
			return "", errors.New(st.Detail)
		}
	}
	return r.String() + ", with the runtime's own credential in Core (CORE_SERVICE_CREDENTIAL)", nil
}
