package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// newOCR is the worker's OCR (OCR, OCR_*; docs/deploying.md), which runs
// until ctx ends: off with OCR=off; with OCR=auto, the default, on when
// tesseract (with its languages), pdftoppm and prlimit are installed, as
// they are in the image, and off otherwise, saying why; with OCR=on, an
// error without them, and the runtime does not start. Where it runs, the
// site's setting turns it off and on and chooses its languages
// (hosting.applyOCR).
func newOCR(ctx context.Context, env config.Env, st store.Store, m *metrics.Metrics, log *slog.Logger) (*ocr.Service, error) {
	cfg := env.OCR.WithDefaults()
	o := ocr.ServiceOptions{Config: cfg, Store: st, Holder: env.WorkerID, Metrics: m, Log: log}
	if cfg.Mode == ocr.ModeOff {
		o.Off, o.OffReason = "it is turned off", ocr.ReasonTurnedOff
		log.Info("OCR is off (OCR=off): scanned PDFs and images are not read for the models that cannot take the files")
		return ocr.NewService(ctx, o), nil
	}
	e, err := ocr.NewEngine(ctx, cfg)
	switch {
	case err != nil && cfg.Mode == ocr.ModeOn:
		return nil, err
	case errors.Is(err, ocr.ErrUnavailable):
		o.Off, o.OffReason, o.OffDetail = "its programs are not installed", ocr.ReasonNotInstalled, err.Error()
		log.Warn("OCR is off: scanned PDFs and images are not read for the models that cannot take the files; "+
			"the image has what it needs, and OCR=on refuses to start without it", "why", err.Error())
		return ocr.NewService(ctx, o), nil
	case err != nil:
		return nil, err
	}
	e.Observe(func(step string, took time.Duration) { m.OCRPageSeconds.WithLabelValues(step).Observe(took.Seconds()) })
	o.Recognizer = e
	return ocr.NewService(ctx, o), nil
}
