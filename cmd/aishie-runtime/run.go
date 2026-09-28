package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/httpserver"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/worker"
)

// stopMargin is how much longer than SHUTDOWN_GRACE run waits for the
// worker to stop before it gives up on it.
const stopMargin = 5 * time.Second

// cmdRun is `aishie-runtime run`: the worker, and its HTTP endpoints,
// until SIGINT or SIGTERM, after which answers in progress are given
// SHUTDOWN_GRACE (and a second SIGINT or SIGTERM stops it at once). It runs
// the YAML agents and, with the store in PostgreSQL, the registry's hosted
// agents, put in force again whenever the registry changes (LISTEN
// aishie_registry, and a poll). SIGHUP reads the YAML again; a
// configuration that does not load is logged, and the one running stays.
func cmdRun(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, sigs <-chan os.Signal) int {
	if len(args) > 0 {
		return usageError(stderr, "run takes no arguments; it is configured by its environment")
	}
	env, err := config.FromEnv(getenv)
	if err != nil {
		return failure(stderr, "the environment:\n%s", problemsText(err))
	}
	log, err := newLogger(env, stderr)
	if err != nil {
		return failure(stderr, "%v", err)
	}
	l, err := load(env)
	if err != nil {
		log.Error("the configuration does not load; see `aishie-runtime check`", "problems", problemsText(err))
		return exitFailure
	}
	if l.prices == nil {
		log.Warn("no price table (PRICES, or the runtime's prices_ref): the costs of model calls are unknown, and recorded as zero")
	}
	client, err := egressClient(env)
	if err != nil {
		log.Error("the egress client", "err", err)
		return exitFailure
	}
	st, kind, err := openStore(ctx, env, log)
	if err != nil {
		log.Error("the store", "err", err)
		return exitFailure
	}
	defer func() { _ = st.Close() }()
	res, v, err := resolver(env, st)
	if err != nil {
		log.Error("the sealed secrets' keyring cannot be used", "err", err)
		return exitFailure
	}
	h := &hosting{env: env, log: log, yaml: l.cfg, prices: l.prices}
	if pg, ok := st.(*pgstore.Store); ok {
		h.pg = pg
		if env.CoreBaseURL == "" {
			log.Warn("CORE_BASE_URL is not set: no hosted agent runs, and each one's state says so")
		}
	} else {
		log.Info("the registry of hosted agents is off: it is kept in PostgreSQL, which DATABASE_URL names")
	}
	h.mu.Lock()
	cfg, rev, err := h.build(ctx)
	h.mu.Unlock()
	if err != nil {
		log.Error("the registry of hosted agents could not be read: the YAML agents start, and it is read again at the next poll", "err", err)
		rev = -1
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	sup, err := worker.NewSupervisor(worker.Options{
		Config: cfg, Env: env, Store: st, Metrics: metrics.New(reg), Log: log,
		Secrets: res, Prices: l.prices, HTTPClient: client, WorkerID: env.WorkerID,
	})
	if err != nil {
		log.Error("the worker", "err", err)
		return exitFailure
	}
	srv := httpserver.New(env.HTTPAddr, sup, reg, st, log)
	if err := srv.Listen(); err != nil {
		log.Error("HTTP_ADDR cannot be listened on", "addr", env.HTTPAddr, "err", err)
		return exitFailure
	}
	log.Info("aishie-runtime started", "version", version.Version, "commit", version.Commit, "worker", sup.WorkerID(),
		"addr", srv.Addr(), "agents", len(cfg.Agents), "hosted", len(h.hosted), "registry", h.pg != nil,
		"store", kind, "prices", l.pricesPath, "kek", kekID(v))
	warnNoAgents(log, cfg)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	supDone := make(chan struct{})
	go func() {
		defer close(supDone)
		if err := sup.Run(ctx); err != nil {
			log.Error("the worker stopped", "err", err)
		}
	}()
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Serve(ctx) }()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		if h.pg != nil {
			h.watch(ctx, sup, rev)
		}
	}()

	code := exitOK
wait:
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				h.reload(ctx, sup)
				continue
			}
			log.Info("stopping", "signal", sig.String(), "grace", env.ShutdownGrace.String())
			break wait
		case err := <-srvDone:
			log.Error("the HTTP server stopped", "err", err)
			srvDone <- err
			code = exitFailure
			break wait
		case <-ctx.Done():
			break wait
		}
	}
	cancel()
	deadline := time.NewTimer(env.ShutdownGrace + stopMargin)
	defer deadline.Stop()
	for supDone != nil || watchDone != nil {
		select {
		case <-supDone:
			supDone = nil
		case <-watchDone:
			watchDone = nil
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				continue
			}
			// Answers still in progress are cut short; what they wrote
			// ahead is sent again by the worker that runs them next.
			log.Error("stopping at once: a second signal", "signal", sig.String())
			return exitFailure
		case <-deadline.C:
			log.Error("the worker did not stop within SHUTDOWN_GRACE", "grace", env.ShutdownGrace.String())
			return exitFailure
		}
	}
	if err := <-srvDone; err != nil && code == exitOK {
		log.Error("the HTTP server did not stop cleanly", "err", err)
		code = exitFailure
	}
	log.Info("aishie-runtime stopped")
	return code
}

// noAgentsNote says what a runtime with no agent does, and how one is added.
const noAgentsNote = "no agent is configured: the runtime starts and waits. Add an agent's YAML to CONFIG, " +
	"check it (aishie-runtime check --live), and send the runtime SIGHUP (in the compose stack, " +
	"aishie compose kill -s HUP runtime; deployed with aishie-runtime-deploy, docker kill -s HUP aishie-runtime)"

// warnNoAgents logs, at start and on every reload, that there is no agent:
// the runtime is healthy and does nothing, which is right before the first
// agent and a mistake after the last, so it is said each time.
func warnNoAgents(log *slog.Logger, cfg *config.Config) {
	if len(cfg.Agents) == 0 {
		log.Warn(noAgentsNote)
	}
}
