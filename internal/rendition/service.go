package rendition

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
)

// Converter makes a file's PDF rendition: an *office.Converter, with the
// renditions' timeout, in the sandbox every conversion runs in.
type Converter interface {
	Rendition(ctx context.Context, data []byte, f office.Format, maxBytes int64) (*office.Output, error)
	Describe() string
}

// Options configure a Service.
type Options struct {
	// Config is RENDITIONS*.
	Config Config
	// Converter converts; nil where LibreOffice does not run here, and
	// Unavailable says why.
	Converter   Converter
	Unavailable string
	// CoreBaseURL is CORE_BASE_URL: the Core whose queue it works.
	CoreBaseURL string
	// CoreHTTP carries its calls to Core, its fetches of the files and
	// its PUTs of the PDFs: the egress client. http.DefaultClient when nil.
	CoreHTTP *http.Client
	// Credential is the runtime's own credential in Core, the agent_runtime
	// service's (CORE_SERVICE_CREDENTIAL), read at each call: one put in
	// its place is taken at the next, without a restart.
	Credential func(ctx context.Context) (string, error)
	// Bucket paces the calls, with every other call of the agent_runtime
	// service this process makes (hosting's, the API's).
	Bucket *ratelimit.Bucket
	// MaxFileBytes is the largest file fetched; MaxFileBytes when 0.
	MaxFileBytes int64
	// Metrics take its counts; nil counts nothing.
	Metrics *metrics.Metrics
	// Log is the runtime's logger (through redact); nil discards.
	Log *slog.Logger
	// Now is the clock; nil is time.Now.
	Now    func() time.Time
	Timing Timing
}

// Timing is how the worker paces itself; a zero field is its default.
type Timing struct {
	// Wait is how long a claim waits for a file to be queued (wait_s).
	Wait time.Duration
	// Backoff and BackoffMax bound the pause after a call to Core that
	// failed.
	Backoff, BackoffMax time.Duration
	// Blocked is how often a worker whose credential is missing or
	// refused, or whose site keeps no files, tries again; Recheck how
	// often it reads the catalogue of a Core too old.
	Blocked, Recheck time.Duration
	// Renew is how often a claim is renewed while its file is converted:
	// every half of its lease when 0, as Core asks.
	Renew time.Duration
}

// Defaults of Timing.
const (
	DefaultWait       = 25 * time.Second
	DefaultBackoff    = time.Second
	DefaultBackoffMax = time.Minute
	DefaultBlocked    = time.Minute
	DefaultRecheck    = 10 * time.Minute
)

func (t Timing) withDefaults() Timing {
	for _, f := range []struct {
		v   *time.Duration
		def time.Duration
	}{{&t.Wait, DefaultWait}, {&t.Backoff, DefaultBackoff}, {&t.BackoffMax, DefaultBackoffMax}, {&t.Blocked, DefaultBlocked},
		{&t.Recheck, DefaultRecheck}} {
		if *f.v <= 0 {
			*f.v = f.def
		}
	}
	return t
}

// Why the worker does not run here (Status.Reason).
const (
	// ReasonOperatorOff: RENDITIONS=off, or what it needs is not there:
	// LibreOffice (OFFICE_PDF off, or soffice or prlimit not installed),
	// or CORE_BASE_URL.
	ReasonOperatorOff = "operator_off"
	// ReasonCoreTooOld: Core's catalogue has no renditions: a Core from
	// before AIShie-Core's migration 0026.
	ReasonCoreTooOld = "core_too_old"
)

// Why the worker, running, claims nothing now (Status.Blocked).
const (
	BlockedNoCredential       = "no_credential"       // #nosec G101 -- a reason's code, not a credential.
	BlockedCredentialRejected = "credential_rejected" // #nosec G101 -- a reason's code, not a credential.
	BlockedNoFileStorage      = "no_file_storage"
)

// Status is where the worker stands in this process.
type Status struct {
	// Available is whether it runs here, and Reason and Detail why not,
	// in English for operators.
	Available      bool
	Reason, Detail string
	// Blocked is why it claims nothing now, "" when nothing stops it.
	Blocked string
	// Inflight are the files it converts now.
	Inflight int
}

// Service is the renditions worker of one process: it claims from Core's
// queue as many files as it converts at once, and converts each. Its
// methods are safe for concurrent use.
type Service struct {
	o   Options
	cfg Config
	t   Timing
	now func() time.Time
	log *slog.Logger
	hc  *http.Client

	mu sync.Mutex
	// wake is closed, and replaced, when a job ends.
	wake     chan struct{}
	avail    availability
	client   *core.RuntimeService
	blocked  string
	inflight int

	jobs sync.WaitGroup
}

// availability is what the environment and Core allow.
type availability struct {
	ok             bool
	reason, detail string
	// checked is when Core's catalogue was last read and found too old.
	checked time.Time
}

// New makes the worker. It does nothing until Run.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.CoreHTTP == nil {
		o.CoreHTTP = http.DefaultClient
	}
	// Core's calls, the files' fetches and the PDFs' PUTs follow no
	// redirect, and the calls end within core.DefaultTimeout but for the
	// claim that waits, which the transport gives longer.
	hc := *o.CoreHTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = MaxFileBytes
	}
	cfg := o.Config.WithDefaults()
	if o.Timing.Renew <= 0 {
		o.Timing.Renew = cfg.Lease / 2
	}
	s := &Service{o: o, cfg: cfg, t: o.Timing.withDefaults(), now: o.Now, log: o.Log.With("module", "renditions"), hc: &hc,
		wake: make(chan struct{})}
	s.avail = s.environment()
	return s
}

// environment is what the operator's environment allows, before Core is
// asked.
func (s *Service) environment() availability {
	switch {
	case s.cfg.Mode == ModeOff:
		return availability{reason: ReasonOperatorOff, detail: "RENDITIONS=off"}
	case s.o.Converter == nil:
		why := s.o.Unavailable
		if why == "" {
			why = "LibreOffice does not convert here"
		}
		return availability{reason: ReasonOperatorOff, detail: "no PDF renditions are made here: " + why}
	case s.o.CoreBaseURL == "":
		return availability{reason: ReasonOperatorOff, detail: "no PDF renditions are made here: CORE_BASE_URL is not set"}
	case s.o.Credential == nil:
		return availability{reason: ReasonOperatorOff, detail: "no PDF renditions are made here: the runtime has no credential of its own in Core"}
	}
	return availability{ok: true}
}

// Status is where the worker stands now.
func (s *Service) Status() Status {
	if s == nil {
		return Status{Reason: ReasonOperatorOff, Detail: "no PDF renditions are made here"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Available: s.avail.ok, Reason: s.avail.reason, Detail: s.avail.detail, Blocked: s.blocked, Inflight: s.inflight}
}

// Check reads Core's catalogue, as Run does first, and says where the
// worker stands: what run says at its start, and refuses to start with
// RENDITIONS=on where it cannot run. A Core that cannot be reached is no
// answer: err says so, and the status is the environment's.
func (s *Service) Check(ctx context.Context) (Status, error) {
	if !s.environment().ok {
		return s.Status(), nil
	}
	_, err := s.service(ctx)
	return s.Status(), err
}

// String is the worker as the start's log line says it.
func (s *Service) String() string {
	st := s.Status()
	if !st.Available {
		return "off: " + st.Detail
	}
	return fmt.Sprintf("on, %d at once, each in %s at most, with %s", s.cfg.Concurrency, s.cfg.Timeout, s.o.Converter.Describe())
}

// service is the runtime's client of Core's renditions: made once Core's
// catalogue is read and offers them; nil, and the worker unavailable
// (core_too_old), when it does not, read again Recheck after.
func (s *Service) service(ctx context.Context) (*core.RuntimeService, error) {
	s.mu.Lock()
	client, avail := s.client, s.avail
	s.mu.Unlock()
	if client != nil {
		return client, nil
	}
	if !avail.ok && !avail.checked.IsZero() && s.now().Sub(avail.checked) < s.t.Recheck {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cat, err := core.FetchCatalogue(ctx, s.hc, s.o.CoreBaseURL)
	if err != nil {
		return nil, fmt.Errorf("renditions: Core's catalogue: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !core.HasRenditions(cat) || !core.HasRuntimeService(cat) {
		if s.avail.reason != ReasonCoreTooOld {
			s.log.Warn("Core is too old for PDF renditions: it has none (agent_runtime.rendition_*); its catalogue is read again every "+
				s.t.Recheck.String(), "core", s.o.CoreBaseURL)
		}
		s.avail = availability{reason: ReasonCoreTooOld, checked: s.now(),
			detail: "Core has no PDF renditions (agent_runtime.rendition_*): it is older than AIShie-Core's migration 0026"}
		return nil, nil
	}
	hc := *s.hc
	if hc.Timeout == 0 {
		hc.Timeout = core.DefaultTimeout
	}
	s.avail = availability{ok: true}
	s.client = core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: s.o.CoreBaseURL, Credential: s.o.Credential,
		HTTPClient: &hc, Bucket: s.o.Bucket, Once: true, Catalogue: cat}))
	return s.client, nil
}

// forget drops the client, so that Core's catalogue is read again: Core
// said it has no renditions after all.
func (s *Service) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client = nil
}

// Run is the worker, until ctx ends: while Core offers renditions, it
// claims as many files as it has slots free (Config.Concurrency), waiting
// for one to be queued when none is (wait_s), and converts each in the
// background. Several processes may run it against one Core: Core never
// gives one claim's file to another, and each process converts as many at
// once as its own slots. When ctx ends, the files in progress are dropped
// with it: their claims lapse in Core, which gives them out again.
func (s *Service) Run(ctx context.Context) {
	defer s.jobs.Wait()
	if !s.environment().ok {
		<-ctx.Done()
		return
	}
	backoff := s.t.Backoff
	for ctx.Err() == nil {
		wake := s.wakeChan()
		svc, err := s.service(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("Core's catalogue could not be read; the renditions worker tries again", "err", errText(err), "in", backoff.String())
			sleep(ctx, nil, backoff)
			backoff = min(backoff*2, s.t.BackoffMax)
			continue
		case svc == nil:
			sleep(ctx, nil, s.t.Recheck)
			continue
		}
		free := s.cfg.Concurrency - s.working()
		if free <= 0 {
			sleep(ctx, wake, 0)
			continue
		}
		claimed, err := svc.ClaimRenditions(ctx, free, s.cfg.Lease, s.t.Wait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d := s.claimFailed(err, backoff)
			sleep(ctx, nil, d)
			backoff = min(backoff*2, s.t.BackoffMax)
			continue
		}
		backoff = s.t.Backoff
		s.unblock()
		for _, c := range claimed {
			s.start(ctx, svc, c)
		}
	}
}

// wakeChan is the channel closed when a job next ends.
func (s *Service) wakeChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wake
}

// working is how many files are converted now.
func (s *Service) working() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight
}

// block records why nothing is claimed now, logging it when it changes.
func (s *Service) block(why, detail string) {
	s.mu.Lock()
	was := s.blocked
	s.blocked = why
	s.mu.Unlock()
	if was != why {
		s.log.Error("no PDF rendition is made until this is put right; the queue is tried again every "+s.t.Blocked.String(),
			"blocked", why, "why", detail)
	}
}

// unblock records that Core took a claim.
func (s *Service) unblock() {
	s.mu.Lock()
	was := s.blocked
	s.blocked = ""
	s.mu.Unlock()
	if was != "" {
		s.log.Info("the renditions worker claims again", "was", was)
	}
}

// claimFailed says what a claim that failed means, and how long to wait
// before the next: a credential missing or refused, or a site that keeps
// no files, waits Blocked, as Core too old waits Recheck; a 429 waits as
// Core says; anything else backs off.
func (s *Service) claimFailed(err error, backoff time.Duration) time.Duration {
	var rl *core.RateLimitedError
	var te *core.TransientError
	var ce *core.CredentialError
	reason, d := "refused", backoff
	switch {
	case errors.As(err, &ce):
		reason, d = BlockedNoCredential, s.t.Blocked
		s.block(BlockedNoCredential, "the runtime's own credential in Core (CORE_SERVICE_CREDENTIAL) cannot be read: "+errText(err))
	case core.CredentialRefused(err):
		reason, d = BlockedCredentialRejected, s.t.Blocked
		s.block(BlockedCredentialRejected, "Core refuses the runtime's own credential (CORE_SERVICE_CREDENTIAL): "+errText(err)+
			"; put the agent_runtime service's credential in its place")
	case core.IsReason(err, core.ReasonNoFileStorage):
		reason, d = BlockedNoFileStorage, s.t.Recheck
		s.block(BlockedNoFileStorage, "the site keeps no files")
	case core.IsReason(err, core.ReasonCoreTooOld):
		s.forget()
		d = s.t.Backoff
	case errors.As(err, &rl):
		reason, d = "rate_limited", rl.RetryAfter
	case errors.As(err, &te):
		reason = "unreachable"
	}
	if s.o.Metrics != nil {
		s.o.Metrics.RenditionClaimErrors.WithLabelValues(reason).Inc()
	}
	if reason == "rate_limited" || reason == "unreachable" || reason == "refused" {
		s.log.Warn("Core's queue of renditions could not be read", "reason", reason, "err", errText(err), "in", d.String())
	}
	if d <= 0 {
		d = s.t.Backoff
	}
	return d + time.Duration(rand.Int64N(int64(d)/4+1)) //nolint:gosec // jitter, not a secret.
}

// sleep waits d, or until wake is closed or ctx ends; d of 0 is for ever.
func sleep(ctx context.Context, wake <-chan struct{}, d time.Duration) {
	var timer <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ctx.Done():
	case <-wake:
	case <-timer:
	}
}

// errText is err's text, redacted, or "" for none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return redact.String(err.Error())
}
