package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/api"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chatwire"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/httpserver"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/netguard"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	runtimeRunner "github.com/AIShie-Education/AIShie-Agent-Runtime/internal/runner"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/version"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/webauth"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/worker"
)

// stopMargin is how much longer than SHUTDOWN_GRACE run waits for the
// worker to stop before it gives up on it.
const stopMargin = 5 * time.Second

// cmdRun is `aishie-runtime run`: the worker, and its HTTP endpoints,
// until SIGINT or SIGTERM, after which answers in progress are given
// SHUTDOWN_GRACE (and a second SIGINT or SIGTERM stops it at once). It loads
// YAML and, with the store in PostgreSQL, registry-hosted agents whenever the
// registry changes (LISTEN aishie_registry and a poll), supervises the loopback
// TypeScript runner, and serves AR-owned chat. SIGHUP reads the YAML again; a
// configuration that does not load is logged, and the one running stays.
func cmdRun(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, sigs <-chan os.Signal) int {
	if len(args) > 0 {
		return usageError(stderr, "run takes no arguments; it is configured by its environment")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
	prices := h.table()
	h.mu.Unlock()
	if err != nil {
		log.Error("the registry of hosted agents could not be read: the YAML agents start, and it is read again at the next poll", "err", err)
		rev = -1
	}
	if prices == nil {
		log.Warn("no price table (PRICES, the runtime's prices_ref, or the site's prices): the costs of model calls are unknown, and recorded as zero")
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	hostedClient, err := netguard.Client(client)
	if err != nil {
		log.Error("the hosted agents' model client", "err", err)
		return exitFailure
	}
	_ = metrics.New(reg) // retained for the management collectors and chat composition root.
	managedRunner, err := runtimeRunner.Start(ctx, runtimeRunner.ProcessOptions{URL: env.RunnerURL, Port: env.RunnerPort,
		Bin: env.RunnerBin, Entry: env.RunnerEntry, Log: log})
	if err != nil {
		log.Error("the TypeScript runner", "err", err)
		return exitFailure
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), env.ShutdownGrace+stopMargin)
		defer closeCancel()
		if err := managedRunner.Close(closeCtx); err != nil {
			log.Error("the TypeScript runner did not stop", "err", err)
		}
	}()
	credential := serviceCredential(env, res)
	if len(cfg.Agents) > 0 || (h.pg != nil && env.CoreBaseURL != "") {
		if _, err := credential(ctx); err != nil {
			log.Error("the runtime's own credential in Core cannot be read (CORE_SERVICE_CREDENTIAL): agents cannot be prepared until it can, "+
				"and it is read again at each try", "ref", env.CoreServiceCredential, "err", err)
		}
	}
	bucket := serviceBucket()
	sup, err := worker.NewSupervisor(worker.Options{Config: cfg, Env: env, Store: st, Log: log, Prices: prices, WorkerID: env.WorkerID})
	if err != nil {
		log.Error("the worker", "err", err)
		return exitFailure
	}
	tokens := newTokenManager(st, res, v, credential, client, bucket, log, sup.WorkerID())
	h.tokens = tokens
	if err := tokens.Sync(ctx, cfg); err != nil {
		log.Warn("initial agent token reconciliation was incomplete; it will retry", "err", redact.String(err.Error()))
	}
	go tokens.Run(ctx)
	defer tokens.Close()
	srv := httpserver.New(env.HTTPAddr, sup, reg, st, log)
	if err := srv.Listen(); err != nil {
		log.Error("HTTP_ADDR cannot be listened on", "addr", env.HTTPAddr, "err", err)
		return exitFailure
	}
	coreRuntime := runtimeService(env, client, credential, bucket)
	var chatService *chat.Service
	if coreRuntime != nil {
		wiring, wireErr := chatwire.New(chatwire.Options{Config: sup.Config, Store: st, Secrets: res,
			OperatorToken: tokens.OperatorToken, HTTPClient: client, PriceTable: h.currentPrices, Location: sup.Location(), Log: log})
		if wireErr != nil {
			log.Error("the chat wiring", "err", wireErr)
			return exitFailure
		}
		chatService, err = chat.NewService(chat.Options{Store: st,
			Access: chat.CoreAccess{Core: coreRuntime, Candidates: wiring}, Decisions: coreRuntime,
			Generator:  &chat.RunnerGenerator{Preparer: wiring, Runner: managedRunner.Client(), Usage: wiring},
			PriceTable: h.currentPrices, Location: sup.Location, Log: log, WorkerID: sup.WorkerID()})
		if err != nil {
			log.Error("the chat service", "err", err)
			return exitFailure
		}
		chatService.Run(ctx)
		defer chatService.Close()
	}
	apiSrv, err := newAPI(env, apiDeps{client: client, models: hostedClient, st: st, vault: v, actors: sup, hosting: h, location: sup.Location,
		runtime: coreRuntime, chat: chatService}, reg, log)
	if err != nil {
		log.Error("the API", "err", err)
		return exitFailure
	}
	apiAddr := ""
	if apiSrv != nil {
		if err := apiSrv.Listen(); err != nil {
			log.Error("API_ADDR cannot be listened on", "addr", env.APIAddr, "err", err)
			return exitFailure
		}
		apiAddr = apiSrv.Addr()
	}
	log.Info("aishie-runtime started", "version", version.Version, "commit", version.Commit, "worker", sup.WorkerID(),
		"addr", srv.Addr(), "api", apiAddr, "agents", len(cfg.Agents), "hosted", len(h.hosted), "registry", h.pg != nil,
		"store", kind, "prices", l.pricesPath, "kek", kekID(v), "runner", managedRunner.URL())
	warnNoAgents(log, cfg)

	supDone := make(chan struct{})
	go func() {
		defer close(supDone)
		if err := sup.Run(ctx); err != nil {
			log.Error("the worker stopped", "err", err)
		}
	}()
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Serve(ctx) }()
	apiDone := make(chan error, 1)
	if apiSrv != nil {
		go func() { apiDone <- apiSrv.Serve(ctx) }()
	}
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
		case err := <-apiDone:
			log.Error("the API stopped", "err", err)
			apiDone <- err
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
	if apiSrv != nil {
		if err := <-apiDone; err != nil && code == exitOK {
			log.Error("the API did not stop cleanly", "err", err)
			code = exitFailure
		}
	}
	log.Info("aishie-runtime stopped")
	return code
}

// apiDeps are what the management API shares: the egress and OpenRouter
// clients, store, vault, actor/configuration views and Core runtime service.
type apiDeps struct {
	client   *http.Client
	models   *http.Client
	st       store.Store
	vault    *vault.Vault
	actors   api.Actors
	hosting  api.Hosting
	runtime  *core.RuntimeService
	chat     api.ChatService
	location func() *time.Location
}

// Core's agent_runtime service allows its one actor, the runtime, 600 calls
// a minute in bursts of 100, every worker's and the API's together:
// serviceRate is what one process makes at most, so that two share it, and
// hosting every agent at once (the first start after an upgrade, which
// issues each a new token) never runs past it.
const (
	serviceRate  = 300
	serviceBurst = 50
)

// serviceBucket paces the calls this process makes of Core's agent_runtime
// service, the worker's and the API's together.
func serviceBucket() *ratelimit.Bucket { return ratelimit.New(serviceRate, serviceBurst) }

// serviceCredential is the runtime's own credential in Core
// (CORE_SERVICE_CREDENTIAL, secret://core/agent_runtime by default),
// read at each use: one the operator puts in its place is taken at the
// next call, with no restart. It is never logged.
func serviceCredential(env config.Env, res secrets.Resolver) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		return res.Resolve(ctx, env.CoreServiceCredential, "")
	}
}

// runtimeService is the API's client of Core's agent_runtime service at
// CORE_BASE_URL, nil without one: each call made once, for a person
// waiting, paced with the worker's.
func runtimeService(env config.Env, client *http.Client, credential func(context.Context) (string, error), bucket *ratelimit.Bucket) *core.RuntimeService {
	if env.CoreBaseURL == "" {
		return nil
	}
	hc := *client
	if hc.Timeout == 0 {
		hc.Timeout = core.DefaultTimeout
	}
	return core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: env.CoreBaseURL, Credential: credential,
		HTTPClient: &hc, Bucket: bucket, Once: true}))
}

// newAPI is the JSON API for the front end (docs/design.md §11.4), or nil
// when API_ADDR is not set: it takes the assertions Core at CORE_BASE_URL
// makes for API_AUDIENCE, checked against CORE_ASSERTION_KEY when it is
// pinned, and otherwise against the keys Core publishes, fetched through
// the egress client. It keeps what it is given in the store, seals the
// owners' keys with the vault, asks Core about an agent with the
// runtime's own credential, asks the supervisor which agents it runs,
// reads the configuration in force, and counts on reg.
func newAPI(env config.Env, d apiDeps, reg prometheus.Registerer, log *slog.Logger) (*api.Server, error) {
	if env.APIAddr == "" {
		return nil, nil
	}
	var keys webauth.Keys = webauth.NewRemoteKeys(env.CoreBaseURL, d.client)
	if env.CoreAssertionKey != "" {
		pinned, err := webauth.ParsePinnedKey(env.CoreAssertionKey)
		if err != nil {
			return nil, fmt.Errorf("CORE_ASSERTION_KEY: %w", err)
		}
		keys = pinned
	}
	proxies, err := api.ParseProxies(env.APITrustedProxies)
	if err != nil {
		return nil, err
	}
	return api.New(api.Options{
		Addr:           env.APIAddr,
		Verifier:       &webauth.Verifier{Keys: keys, Issuer: env.CoreBaseURL, Audience: env.APIAudience},
		Store:          d.st,
		Chat:           d.chat,
		CoreBaseURL:    env.CoreBaseURL,
		CoreHTTP:       d.client,
		Vault:          d.vault,
		Runtime:        d.runtime,
		Actors:         d.actors,
		Hosting:        d.hosting,
		Allowlist:      env.CoreBaseURLAllowlist,
		ModelHTTP:      d.models,
		Runner:         runtimeRunner.NewClient(env.RunnerAddress(), nil),
		AdminActorIDs:  env.AdminActorIDs,
		TrustedProxies: proxies,
		Registerer:     reg,
		Log:            log,
		Location:       d.location,
	}), nil
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
