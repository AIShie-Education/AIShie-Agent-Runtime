package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/worker"
)

// hosting is the configuration run puts in force: the YAML last loaded
// and, when the registry is on, the hosted agents, rebuilt when either
// changes (docs/design.md §11.2). The registry is on only with the store in
// PostgreSQL, where hosted agents are kept.
type hosting struct {
	env config.Env
	// pg is the store, nil when the registry is off.
	pg  *pgstore.Store
	log *slog.Logger

	mu     sync.Mutex
	yaml   *config.Config
	prices *pricing.Table
	// hosted and rejected are the registry's agents as last built, kept
	// for a reload when the registry cannot be read.
	hosted   []*config.Agent
	rejected []config.Rejection
	// reported are the agents last reported not run.
	reported []string
}

// registryTimeout bounds each build's read of the registry, so that a
// database that does not answer holds up neither SIGHUP, whose reload
// waits for the build (and the signals after it wait for the reload), nor
// the start.
var registryTimeout = registry.DefaultTimeout

// options are the registry's settings from the environment.
func (h *hosting) options() registry.Options {
	return registry.Options{CoreBaseURL: h.env.CoreBaseURL, Allowlist: h.env.CoreBaseURLAllowlist}
}

// build is YAML ∪ registry, and the registry's revision it was built from:
// the YAML alone, at revision 0, when the registry is off. A hosted agent
// with a quota in dollars that no price holds is not run. When the
// registry cannot be read, it is the YAML with the hosted agents as they
// were last built, and the error. Called with mu held.
func (h *hosting) build(ctx context.Context) (*config.Config, int64, error) {
	if h.pg == nil {
		return h.yaml, 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	cfg, rev, err := registry.Build(ctx, h.yaml, h.pg, h.options())
	if err != nil {
		return h.withLastHosted(), 0, err
	}
	kept := cfg.Agents[:0]
	for _, a := range cfg.Agents {
		if a.Hosted != nil {
			if p := usdWithoutPrices(&config.Config{Runtime: cfg.Runtime, Agents: []*config.Agent{a}}, h.prices, time.Now()); len(p) > 0 {
				cfg.Rejected = append(cfg.Rejected, config.Rejection{AgentID: a.ID, Source: registry.SourceName(a.ID), Err: errors.New(p[0]),
					Reason: store.ReasonSettingsRejected, Version: a.Hosted.Version})
				continue
			}
		}
		kept = append(kept, a)
	}
	cfg.Agents = kept
	h.hosted, h.rejected = nil, cfg.Rejected
	for _, a := range cfg.Agents {
		if a.Hosted != nil {
			h.hosted = append(h.hosted, a)
		}
	}
	h.report(cfg)
	return cfg, rev, nil
}

// withLastHosted is the YAML with the hosted agents as last built, less
// any whose id a YAML agent now has. Called with mu held.
func (h *hosting) withLastHosted() *config.Config {
	cfg := &config.Config{Runtime: h.yaml.Runtime, Dir: h.yaml.Dir, Agents: slices.Clone(h.yaml.Agents), Rejected: h.rejected}
	ids := map[string]bool{}
	for _, a := range h.yaml.Agents {
		ids[a.ID] = true
	}
	for _, a := range h.hosted {
		if !ids[a.ID] {
			cfg.Agents = append(cfg.Agents, a)
		}
	}
	return cfg
}

// report logs the hosted agents that run and those that are not run, when
// the latter change: their ids only, since what is wrong may quote what
// their owners wrote, and is in each one's state.
func (h *hosting) report(cfg *config.Config) {
	var rejected []string
	for _, r := range cfg.Rejected {
		rejected = append(rejected, r.AgentID)
	}
	if slices.Equal(rejected, h.reported) {
		return
	}
	h.reported = rejected
	if len(rejected) > 0 {
		h.log.Warn("hosted agents not run: their configuration does not pass; each one's state says why", "agents", rejected)
	}
}

// reload is SIGHUP: the YAML and the price table read again, and put in
// force with the registry. What does not load is logged, and changes
// nothing.
func (h *hosting) reload(ctx context.Context, sup *worker.Supervisor) {
	l, err := load(h.env)
	if err != nil {
		h.log.Error("SIGHUP: the configuration does not load; the one running stays", "problems", problemsText(err))
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.yaml, h.prices = l.cfg, l.prices
	cfg, _, err := h.build(ctx)
	if err != nil {
		h.log.Warn("SIGHUP: the registry of hosted agents could not be read; they run as they were", "err", err)
	}
	sup.SetPrices(l.prices)
	sup.Reload(cfg)
	h.log.Info("SIGHUP: the configuration was read again", "agents", len(cfg.Agents), "hosted", len(h.hosted), "prices", l.pricesPath)
	warnNoAgents(h.log, cfg)
}

// update is the registry's change put in force: the YAML as it is, and the
// hosted agents as they now are. It returns the revision it was built
// from.
func (h *hosting) update(ctx context.Context, sup *worker.Supervisor) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cfg, rev, err := h.build(ctx)
	if err != nil {
		return 0, err
	}
	sup.Update(cfg)
	h.log.Info("the registry of hosted agents changed", "rev", rev, "hosted", len(h.hosted), "not_run", len(cfg.Rejected))
	return rev, nil
}

// watch runs the registry's watcher until ctx is done, from rev: LISTEN on
// a connection of its own, and the poll.
func (h *hosting) watch(ctx context.Context, sup *worker.Supervisor, rev int64) {
	w := &registry.Watcher{
		Rev: h.pg.RegistryRev, Listen: h.pg.ListenRegistry, Log: h.log,
		Rebuild: func(ctx context.Context) (int64, error) { return h.update(ctx, sup) },
	}
	w.Run(ctx, rev)
}
