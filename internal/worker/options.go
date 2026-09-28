package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/providers"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/netguard"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// Options are what a Supervisor is built from. Every dependency can be
// given, so that tests run on the fake Core and the scripted model with
// millisecond intervals; a zero field is the production default.
type Options struct {
	// Config is the agents and the runtime's settings, loaded and
	// validated. Required.
	Config *config.Config
	// Env is the process's settings: the worker id and the shutdown grace
	// are read from it.
	Env config.Env
	// Store is the runtime's state. Required.
	Store store.Store
	// Metrics are counted into; a registry of its own when nil.
	Metrics *metrics.Metrics
	// Log is where the worker logs: ids, counts, codes and timings, never
	// what anyone wrote. Nothing is logged when nil.
	Log *slog.Logger
	// Secrets resolves token_ref and key_ref, a relative file:// path from
	// the agent's directory, as an agent starts and nowhere else: a
	// secrets.Resolver, whose Sealed opens sealed:// references through the
	// vault. A resolver of the environment and files alone when nil.
	Secrets SecretResolver
	// Prices cost each model call; nil leaves costs unknown (zero).
	Prices *pricing.Table
	// HTTPClient carries every call out (the egress proxy's): to Core, to
	// the providers of YAML agents' models, to Core's file downloads. Calls
	// to Core are bounded by core.DefaultTimeout when it has no timeout of
	// its own; model calls by their context. http.DefaultClient when nil.
	HTTPClient *http.Client
	// HostedHTTPClient carries a hosted agent's model calls: connections
	// to public addresses alone, and no redirect followed (package
	// netguard), since the owner, not the operator, chose the model. Calls
	// to Core keep HTTPClient: Core may well be at a private address. When
	// nil it is netguard.Client(HTTPClient), which needs HTTPClient's
	// transport to be an *http.Transport (or none).
	HostedHTTPClient *http.Client
	// NewAdapter builds a model adapter; providers.New when nil.
	NewAdapter func(llm.Config) (llm.Adapter, error)
	// NewCaller makes an agent's connection to Core. When nil it is MCP or
	// REST as core.transport says, under the agent's rate limit (a bucket at
	// ratelimit.CoreShare of assumed_core_rate_per_min and
	// assumed_core_burst), retried (core.Retrying, whose 429s slow the
	// agent's polling for Slowdown) and counted (Metrics.CoreCaller).
	// Whatever it returns, the worker also reads an envelope of status
	// error and code unauthenticated as a 401.
	NewCaller func(a *config.Agent, token string, cat *core.Catalogue) (core.Caller, error)
	// CoreRetry configures the default NewCaller's retries (its
	// OnRateLimited is the worker's own); the zero value is the handout's.
	CoreRetry core.RetryOptions
	// Now is the clock of timestamps, days and deadlines; time.Now when
	// nil. Waits are timers of real time.
	Now func() time.Time
	// Rand is a number in [0, 1), for jitter, safe for concurrent use;
	// math/rand/v2's when nil.
	Rand func() float64
	// WorkerID names this process in leases; Env.WorkerID, else the host
	// name and process id, when empty.
	WorkerID string
	// Timing is the supervisor's own intervals.
	Timing Timing
}

// SecretResolver resolves references to secrets (package secrets): ref,
// with a relative file:// path from baseDir.
type SecretResolver interface {
	Resolve(ctx context.Context, ref, baseDir string) (string, error)
}

// Timing is how often the supervisor does what it does, and how long it
// waits after a failure. A zero field is its default.
type Timing struct {
	// LeaseEvery is how often agent leases are taken or renewed: 10 s.
	LeaseEvery time.Duration
	// LeaseTTL is how long an agent lease lasts unrenewed: 30 s. A dead
	// worker's agents are taken up by another within it.
	LeaseTTL time.Duration
	// Housekeeping is how often the memory of seats gone is purged: 1 h.
	Housekeeping time.Duration
	// Restart is the first wait before an agent that failed is started
	// again, doubling to RestartMax: 1 s and 5 min.
	Restart, RestartMax time.Duration
	// ModelBackoff is the first wait before a provider's retryable error
	// is tried again, doubling to ModelBackoffMax, within the answer's wall
	// clock: 1 s and 10 s.
	ModelBackoff, ModelBackoffMax time.Duration
	// HoldBack is how long a conversation whose providers all failed is
	// held back, doubling with each failure to HoldBackMax: 1 min and 10 min.
	HoldBack, HoldBackMax time.Duration
	// RetryLater is how long a conversation is held back when Core could
	// not be reached for its answer: 10 s.
	RetryLater time.Duration
}

// Defaults of Timing.
const (
	DefaultLeaseEvery      = 10 * time.Second
	DefaultLeaseTTL        = 30 * time.Second
	DefaultHousekeeping    = time.Hour
	DefaultRestart         = time.Second
	DefaultRestartMax      = 5 * time.Minute
	DefaultModelBackoff    = time.Second
	DefaultModelBackoffMax = 10 * time.Second
	DefaultHoldBack        = time.Minute
	DefaultHoldBackMax     = 10 * time.Minute
	DefaultRetryLater      = 10 * time.Second
)

func (t Timing) withDefaults() Timing {
	set := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	set(&t.LeaseEvery, DefaultLeaseEvery)
	set(&t.LeaseTTL, DefaultLeaseTTL)
	set(&t.Housekeeping, DefaultHousekeeping)
	set(&t.Restart, DefaultRestart)
	set(&t.RestartMax, DefaultRestartMax)
	set(&t.ModelBackoff, DefaultModelBackoff)
	set(&t.ModelBackoffMax, DefaultModelBackoffMax)
	set(&t.HoldBack, DefaultHoldBack)
	set(&t.HoldBackMax, DefaultHoldBackMax)
	set(&t.RetryLater, DefaultRetryLater)
	return t
}

// withDefaults checks o and fills in what it leaves out.
func (o Options) withDefaults() (Options, error) {
	var errs []error
	if o.Config == nil {
		errs = append(errs, errors.New("worker: no configuration"))
	}
	if o.Store == nil {
		errs = append(errs, errors.New("worker: no store"))
	}
	if err := errors.Join(errs...); err != nil {
		return o, err
	}
	if o.Metrics == nil {
		o.Metrics = metrics.New(prometheus.NewRegistry())
	}
	if o.Secrets == nil {
		o.Secrets = secrets.Resolver{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.HTTPClient == nil {
		o.HTTPClient = http.DefaultClient
	}
	if o.HostedHTTPClient == nil {
		c, err := netguard.Client(o.HTTPClient)
		if err != nil {
			return o, fmt.Errorf("worker: hosted agents' model calls: %w; give HostedHTTPClient", err)
		}
		o.HostedHTTPClient = c
	}
	if o.NewAdapter == nil {
		o.NewAdapter = providers.New
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Float64
	}
	if o.WorkerID == "" {
		o.WorkerID = o.Env.WorkerID
	}
	if o.WorkerID == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "worker"
		}
		o.WorkerID = host + "-" + strconv.Itoa(os.Getpid())
	}
	o.Timing = o.Timing.withDefaults()
	return o, nil
}

// coreClient is the HTTP client calls to Core go through: the egress
// client, bounded by core.DefaultTimeout when it has no timeout of its own,
// so that a connection that hangs does not hold a poller for ever.
func coreClient(c *http.Client) *http.Client {
	if c.Timeout > 0 {
		return c
	}
	cp := *c
	cp.Timeout = core.DefaultTimeout
	return &cp
}
