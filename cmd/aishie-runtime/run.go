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
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/worker"
)

// stopMargin is how much longer than SHUTDOWN_GRACE run waits for the
// worker to stop before it gives up on it.
const stopMargin = 5 * time.Second

// cmdRun is `aishie-runtime run`: the worker, and its HTTP endpoints,
// until SIGINT or SIGTERM, after which answers in progress are given
// SHUTDOWN_GRACE (and a second SIGINT or SIGTERM stops it at once). SIGHUP
// reads the configuration again; a configuration that does not load is
// logged, and the one running stays.
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

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	sup, err := worker.NewSupervisor(worker.Options{
		Config: l.cfg, Env: env, Store: st, Metrics: metrics.New(reg), Log: log,
		Secrets: secrets.Resolver{Dir: env.SecretsDir}, Prices: l.prices, HTTPClient: client, WorkerID: env.WorkerID,
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
		"addr", srv.Addr(), "agents", len(l.cfg.Agents), "store", kind, "prices", l.pricesPath)

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

	code := exitOK
wait:
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				reload(env, sup, log)
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
stopping:
	for {
		select {
		case <-supDone:
			break stopping
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

// reload reads the configuration and the price table again, and gives
// them to the worker; what does not load is logged, and changes nothing.
func reload(env config.Env, sup *worker.Supervisor, log *slog.Logger) {
	l, err := load(env)
	if err != nil {
		log.Error("SIGHUP: the configuration does not load; the one running stays", "problems", problemsText(err))
		return
	}
	sup.SetPrices(l.prices)
	sup.Reload(l.cfg)
	log.Info("SIGHUP: the configuration was read again", "agents", len(l.cfg.Agents), "prices", l.pricesPath)
}
