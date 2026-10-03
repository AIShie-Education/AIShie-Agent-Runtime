// Package api serves the runtime's JSON API for AIShie-Frontend (the
// product owner's D1; its contract is docs/design.md §11.4 and the M2 API
// contract it follows). It listens on a listener of its own, API_ADDR,
// behind Caddy at /runtime/api/ on Core's origin with the Cookie header
// stripped, and serves /runtime/api/v1/ and nothing else: /healthz,
// /metrics and /status are HTTP_ADDR's alone.
//
// A person is who Core's assertion says (package webauth, D2): the front
// end sends it as a bearer token on every call. Nothing ambient is taken,
// no cookie and no session, so a page elsewhere cannot make a browser call
// it as someone; cross-origin requests that change anything are refused
// all the same. Every answer is JSON, is not cached (GET /info aside), and
// may run nothing; a refusal is Core's envelope, {"error": {"code",
// "message", "details": {"reason", …}}}.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/netguard"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/version"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/webauth"
)

// Prefix is where every route of the API is.
const Prefix = "/runtime/api/v1/"

// MaxBodyBytes bounds a request's body.
const MaxBodyBytes = 64 << 10

// Options configure a Server.
type Options struct {
	// Addr is where it listens (API_ADDR).
	Addr string
	// Verifier checks the assertions people bring; its Issuer and
	// Audience are what GET /info says.
	Verifier *webauth.Verifier
	// Store is the runtime's store: the registry, the people, the audit.
	Store store.Store
	// CoreBaseURL is the Core hosted agents run at (CORE_BASE_URL): the
	// one Core the API asks about an agent.
	CoreBaseURL string
	// CoreHTTP carries the calls to Core: the egress client, bounded by
	// core.DefaultTimeout when it has no timeout of its own.
	// http.DefaultClient when nil.
	CoreHTTP *http.Client
	// Vault seals the keys people give; a hosted agent is hosted only
	// with one, since the worker seals the token it is issued with it too.
	Vault *vault.Vault
	// Runtime is the runtime's client of Core's agent_runtime service,
	// with its own credential (CORE_SERVICE_CREDENTIAL): whether a person
	// owns an agent and may host it, and revoking its token as its hosting
	// ends. Nil when the runtime has no credential: nothing is hosted.
	Runtime *core.RuntimeService
	// Actors says which of the worker's agents runs as a Core actor (the
	// supervisor); nil knows of none.
	Actors Actors
	// Hosting is the configuration the runtime runs: the operator's YAML
	// and the price file's table. Nil is none of either.
	Hosting Hosting
	// OCR is the worker's OCR, which the site's settings turn off and on
	// and give its languages (admin/settings); nil is none here.
	OCR OCR
	// Transcriber is the worker's transcriber, which the site's settings
	// turn on and off (admin/settings) and whose credential and jobs the
	// administrators manage (admin/transcription); nil is none here.
	Transcriber Transcriber
	// Allowlist is CORE_BASE_URL_ALLOWLIST, which a change's dry run holds
	// CORE_BASE_URL to, as the registry does.
	Allowlist []string
	// ModelHTTP carries keys/test's call to a provider: the hosted-model
	// client (package netguard), which connects to public addresses alone
	// and follows no redirect. netguard.Client(CoreHTTP) when nil.
	ModelHTTP *http.Client
	// NewAdapter builds keys/test's adapter; providers.New when nil.
	NewAdapter func(llm.Config) (llm.Adapter, error)
	// OpenRouterBaseURL is OpenRouter's API, which the list of a model's
	// upstream providers is read from over ModelHTTP;
	// openrouter.DefaultBaseURL when empty. For tests.
	OpenRouterBaseURL string
	// AdminActorIDs, when not empty, narrow the runtime's administrators
	// to those of Core's it names (ADMIN_ACTOR_IDS), in lower case.
	AdminActorIDs []string
	// TrustedProxies are the proxies whose X-Forwarded-For names the
	// client (API_TRUSTED_PROXIES).
	TrustedProxies []*net.IPNet
	// Registerer takes the API's metrics; nil keeps them to the server.
	Registerer prometheus.Registerer
	// Log is the runtime's logger; nil discards.
	Log *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Actors says which agent of the worker's runs as a Core actor at a Core:
// worker.Supervisor.ActorAgent.
type Actors interface {
	ActorAgent(baseURL, actorID string) (agentID string, hosted, ok bool)
}

// Hosting is the configuration the runtime runs, as the API reads it.
type Hosting interface {
	// YAML is the operator's configuration as last loaded: its runtime
	// settings, and its agents.
	YAML() *config.Config
	// Prices is the price file's table, nil for none; the site's rows
	// are put before it as the store has them (pricesOf).
	Prices() *pricing.Table
}

// OCR is what the API reads of the worker's OCR: *ocr.Service.
type OCR interface {
	Capability() ocr.Capability
}

// Server is the API.
type Server struct {
	o        Options
	mux      *http.ServeMux
	handler  http.Handler
	m        *metrics
	coreHTTP *http.Client
	// modelHTTP carries keys/test's calls: Options.ModelHTTP, or the
	// guarded client made from CoreHTTP.
	modelHTTP *http.Client
	cats      catalogueCache
	// openRouter reads, and keeps, OpenRouter's lists of a model's
	// upstream providers.
	openRouter *openrouter.Catalogue

	perIP, failures, general, token, keyTest *limiter
	keyDay                                   *dailyLimiter

	seenMu sync.Mutex
	// seen is when each person was last recorded (PutPerson), so that
	// they are recorded at most every personEvery.
	seen map[string]time.Time

	mu  sync.Mutex
	srv *http.Server
	ln  net.Listener
}

// New makes the API's server. It listens on nothing until Listen or Serve.
func New(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Registerer == nil {
		o.Registerer = prometheus.NewRegistry()
	}
	admins := make([]string, len(o.AdminActorIDs))
	for i, id := range o.AdminActorIDs {
		admins[i] = strings.ToLower(id)
	}
	o.AdminActorIDs = admins
	modelHTTP := o.ModelHTTP
	if modelHTTP == nil {
		c, err := netguard.Client(o.CoreHTTP)
		if err != nil {
			// A transport it cannot guard: the default one, guarded.
			c, _ = netguard.Client(nil)
		}
		modelHTTP = c
	}
	s := &Server{
		o: o, mux: http.NewServeMux(), m: newMetrics(o.Registerer), coreHTTP: coreClient(o.CoreHTTP), modelHTTP: modelHTTP,
		perIP: newLimiter(RatePerIP), failures: newLimiter(RateFailures), general: newLimiter(RateGeneral),
		token: newLimiter(RateToken), keyTest: newLimiter(RateKeyTest), keyDay: newDailyLimiter(KeyTestsPerDay),
		seen: map[string]time.Time{},
		openRouter: openrouter.NewCatalogue(openrouter.CatalogueOptions{BaseURL: o.OpenRouterBaseURL, Client: modelHTTP,
			UserAgent: "aishie-runtime/" + version.Version, Now: o.Now}),
	}
	s.mux.Handle("GET "+Prefix+"info", s.public(s.info))
	s.mux.Handle("GET "+Prefix+"me", s.authed(s.me))
	s.mux.Handle("POST "+Prefix+"agents/inspect", s.authedBody(s.limited(s.token, s.audited("agent.inspect", s.inspect))))
	s.mux.Handle("POST "+Prefix+"agents", s.authedBody(s.limited(s.token, s.audited("agent.host", s.host))))
	s.mux.Handle("GET "+Prefix+"agents", s.authed(s.list))
	s.mux.Handle("GET "+Prefix+"agents/{id}", s.authed(s.get))
	s.mux.Handle("POST "+Prefix+"agents/{id}/token", s.authed(s.limited(s.token, s.audited("agent.token_renew", s.renewToken))))
	s.mux.Handle("POST "+Prefix+"agents/{id}/pause", s.authed(s.audited("agent.pause", s.pause(true))))
	s.mux.Handle("POST "+Prefix+"agents/{id}/resume", s.authed(s.audited("agent.resume", s.pause(false))))
	s.mux.Handle("DELETE "+Prefix+"agents/{id}", s.authed(s.audited("agent.delete", s.remove)))
	s.mux.Handle("PATCH "+Prefix+"agents/{id}", s.authedBody(s.audited("agent.update", s.update)))
	s.mux.Handle("GET "+Prefix+"models", s.authed(s.models))
	s.mux.Handle("POST "+Prefix+"keys/test", s.authedBody(s.limitedKeyTest(s.audited("key.test", s.testKey))))
	s.mux.Handle("GET "+Prefix+"admin/school-plan/usage", s.authed(s.schoolPlanUsage))
	s.mux.Handle("GET "+Prefix+"admin/settings", s.authed(s.getSettings))
	s.mux.Handle("PATCH "+Prefix+"admin/settings", s.authedBody(s.audited("settings.update", s.patchSettings)))
	s.mux.Handle("GET "+Prefix+"admin/school-plan", s.authed(s.getSchoolPlan))
	s.mux.Handle("POST "+Prefix+"admin/school-plan/offers", s.authedBody(s.audited("school_offer.create", s.createOffer)))
	s.mux.Handle("GET "+Prefix+"admin/school-plan/offers/{id}", s.authed(s.getOffer))
	s.mux.Handle("PATCH "+Prefix+"admin/school-plan/offers/{id}", s.authedBody(s.audited("school_offer.update", s.updateOffer)))
	s.mux.Handle("DELETE "+Prefix+"admin/school-plan/offers/{id}", s.authed(s.audited("school_offer.delete", s.deleteOffer)))
	s.mux.Handle("GET "+Prefix+"admin/openrouter/endpoints", s.authedBody(s.openRouterEndpoints))
	s.mux.Handle("PUT "+Prefix+"admin/school-plan/quotas", s.authedBody(s.audited("school_quotas.update", s.putQuotas)))
	s.mux.Handle("DELETE "+Prefix+"admin/school-plan/quotas", s.authed(s.audited("school_quotas.reset", s.resetQuotas)))
	s.mux.Handle("GET "+Prefix+"admin/prices", s.authed(s.getPrices))
	s.mux.Handle("POST "+Prefix+"admin/prices", s.authedBody(s.audited("price.create", s.createPrice)))
	s.mux.Handle("GET "+Prefix+"admin/prices/{id}", s.authed(s.getPrice))
	s.mux.Handle("PATCH "+Prefix+"admin/prices/{id}", s.authedBody(s.audited("price.update", s.updatePrice)))
	s.mux.Handle("DELETE "+Prefix+"admin/prices/{id}", s.authed(s.audited("price.delete", s.deletePrice)))
	s.mux.Handle("GET "+Prefix+"admin/tenants", s.authedBody(s.listTenants))
	s.mux.Handle("GET "+Prefix+"admin/tenants/{tenant_id}", s.authed(s.getTenant))
	s.mux.Handle("PUT "+Prefix+"admin/tenants/{tenant_id}", s.authedBody(s.audited("tenant_quota.update", s.putTenant)))
	s.mux.Handle("DELETE "+Prefix+"admin/tenants/{tenant_id}", s.authed(s.audited("tenant_quota.reset", s.resetTenant)))
	s.mux.Handle("GET "+Prefix+"admin/agent-budgets", s.authed(s.getAgentBudgets))
	s.mux.Handle("PUT "+Prefix+"admin/agent-budgets", s.authedBody(s.audited("agent_budgets.update", s.putAgentBudgets)))
	s.mux.Handle("DELETE "+Prefix+"admin/agent-budgets", s.authed(s.audited("agent_budgets.reset", s.resetAgentBudgets)))
	s.mux.Handle("GET "+Prefix+"admin/costs", s.authedBody(s.costs))
	s.mux.Handle("PUT "+Prefix+"admin/transcription/credential", s.authedBody(s.audited("transcription_credential.set", s.putCredential)))
	s.mux.Handle("DELETE "+Prefix+"admin/transcription/credential", s.authed(s.audited("transcription_credential.delete", s.deleteCredential)))
	s.mux.Handle("GET "+Prefix+"admin/transcription/jobs", s.authedBody(s.jobs))

	guard := http.NewCrossOriginProtection()
	guard.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, Error{Code: CodeForbidden, Reason: ReasonCrossOrigin,
			Message: "a request from another origin that would change something is refused"})
	}))
	s.handler = s.logged(s.recovered(secured(s.addressed(guard.Handler(s.routed(s.mux))))))
	return s
}

// Handler serves the API.
func (s *Server) Handler() http.Handler { return s.handler }

// Listen binds the server's address; Addr says what it bound.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.o.Addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.srv = &http.Server{Handler: s.handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: time.Minute, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
		ErrorLog: slog.NewLogLogger(s.o.Log.Handler(), slog.LevelWarn)}
	s.mu.Unlock()
	return nil
}

// Addr is the address the server listens on, once Listen has bound it.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return s.o.Addr
	}
	return s.ln.Addr().String()
}

// shutdownGrace is how long requests in progress are given when the server
// stops.
const shutdownGrace = 2 * time.Second

// Serve serves until ctx is done, then shuts down, giving requests in
// progress shutdownGrace. It listens first if Listen was not called.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	bound := s.ln != nil
	s.mu.Unlock()
	if !bound {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	srv, ln := s.srv, s.ln
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(shut)
	if errors.Is(err, context.DeadlineExceeded) {
		err = srv.Close()
	}
	if serveErr := <-done; !errors.Is(serveErr, http.ErrServerClosed) && err == nil {
		err = serveErr
	}
	return err
}

// adminRoles are Core's platform roles the runtime takes as its
// administrators' (the product owner's D4).
var adminRoles = []string{"root", "admin"}

// isAdmin reports whether the person the claims name is one of the
// runtime's administrators: an administrator of Core's, as Core said when
// it made the assertion (at most its lifetime ago), and one AdminActorIDs
// names when it names any.
func (s *Server) isAdmin(c *webauth.Claims) bool {
	if !slices.Contains(adminRoles, c.PlatformRole) {
		return false
	}
	return len(s.o.AdminActorIDs) == 0 || slices.Contains(s.o.AdminActorIDs, c.Subject)
}

// prices is the price file's table, nil for none: the table in force is
// it with the site's rows (pricesOf).
func (s *Server) prices() *pricing.Table {
	if s.o.Hosting == nil {
		return nil
	}
	return s.o.Hosting.Prices()
}

// yaml is the operator's configuration in force: none, an empty one.
func (s *Server) yaml() *config.Config {
	if s.o.Hosting == nil {
		return &config.Config{}
	}
	if c := s.o.Hosting.YAML(); c != nil {
		return c
	}
	return &config.Config{}
}

// effective is the operator's configuration with the site's settings in
// force (registry.ReadSite, registry.WithSite), as the registry builds the
// configuration the worker runs: read from the store at each request, so
// that the request after an administrator's change sees it.
func (s *Server) effective(ctx context.Context) (*config.Config, error) {
	site, err := registry.ReadSite(ctx, s.o.Store)
	if err != nil {
		return nil, err
	}
	return registry.WithSite(s.yaml(), site), nil
}

// plan is the school's plan in force: runtime.yaml's, with the site's
// offers and quotas (effective).
func (s *Server) plan(ctx context.Context) (config.School, error) {
	eff, err := s.effective(ctx)
	if err != nil {
		return config.School{}, err
	}
	return eff.Runtime.School, nil
}
