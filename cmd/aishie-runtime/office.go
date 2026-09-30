package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// newOffice is the worker's conversion of Office files (OFFICE_PDF*,
// PDF_PART_PAGES; docs/deploying.md), which runs until ctx ends: off with
// OFFICE_PDF=off; with OFFICE_PDF=auto, the default, on when LibreOffice
// (soffice) and prlimit are installed, as they are in the image, and off
// otherwise, saying why; with OFFICE_PDF=on, an error without them, and the
// runtime does not start. PDFs are cut into parts of their pages wherever
// poppler's pdftocairo, pdfseparate and pdfunite are, whatever OFFICE_PDF
// says, and given whole where they are not.
func newOffice(ctx context.Context, env config.Env, m *metrics.Metrics, log *slog.Logger) (*office.Service, error) {
	cfg := env.Office.WithDefaults()
	o := office.ServiceOptions{Config: cfg, Metrics: m, Log: log}
	if p, err := office.NewPager(cfg); err != nil {
		log.Warn("PDFs are given to the models that take files whole, not in parts of their pages; the image has what it takes", "why", err.Error())
	} else {
		o.Pager = p
	}
	if cfg.Mode == office.ModeOff {
		o.Off = "it is turned off"
		log.Info("the conversion of Office files is off (OFFICE_PDF=off): presentations and documents are given as the runtime's text of them")
		return office.NewService(ctx, o), nil
	}
	c, err := office.NewConverter(ctx, cfg)
	switch {
	case err != nil && cfg.Mode == office.ModeOn:
		return nil, err
	case errors.Is(err, office.ErrUnavailable):
		o.Off = "LibreOffice is not installed"
		log.Warn("the conversion of Office files is off: presentations and documents are given as the runtime's text of them, and older "+
			"Office files not at all; the image has what it needs, and OFFICE_PDF=on refuses to start without it", "why", err.Error())
		return office.NewService(ctx, o), nil
	case err != nil:
		return nil, err
	}
	o.Converter = c
	return office.NewService(ctx, o), nil
}
