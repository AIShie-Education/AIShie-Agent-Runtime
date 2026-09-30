package transcribe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/providers"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Resolver resolves a reference to a secret: secrets.Resolver, whose
// sealed:// references the vault opens.
type Resolver interface {
	Resolve(ctx context.Context, ref, baseDir string) (string, error)
}

// Office converts Office files to PDF and cuts and draws ranges of a
// PDF's pages: the worker's *office.Service. Nil converts, cuts and draws
// nothing.
type Office interface {
	Available() (bool, string)
	Cuts() bool
	Draws() bool
	Convert(ctx context.Context, sum string, f office.Format, to office.Target, data []byte) office.State
	Range(ctx context.Context, sum string, pdf []byte, first, last int) ([]byte, error)
	Images(ctx context.Context, pdf []byte, first, last, dpi int) ([][]byte, error)
}

// Options configure a Service.
type Options struct {
	// Mode is TRANSCRIBE: config.TranscribeAuto, On or Off.
	Mode string
	// Store keeps the lease, the credential, the jobs and the ledger.
	Store store.Store
	// Keeps says the store is PostgreSQL and a vault opens its sealed
	// secrets: what the site's settings and credential are kept in.
	// Without it the transcriber does not run (ReasonOperatorOff).
	Keeps bool
	// CoreBaseURL is CORE_BASE_URL: the Core whose queue it works.
	CoreBaseURL string
	// CoreHTTP carries its calls to Core and its fetches of the files: the
	// egress client. http.DefaultClient when nil.
	CoreHTTP *http.Client
	// Secrets resolve the offer's key and open the credential.
	Secrets Resolver
	// ModelHTTP carries the calls to an offer of runtime.yaml's, whose
	// endpoint is the operator's; HostedHTTP those to an offer the site
	// made, at a provider's own endpoint, over the hosted-model client
	// (netguard). Each is http.DefaultClient when nil.
	ModelHTTP, HostedHTTP *http.Client
	// NewAdapter builds the offer's adapter; providers.New when nil.
	NewAdapter func(llm.Config) (llm.Adapter, error)
	// Office converts, cuts and draws; nil does none of them.
	Office Office
	// PartPages is how many pages one call is given (PDF_PART_PAGES);
	// office.DefaultPartPages when 0.
	PartPages int
	// DocLimits bound the reading of a PDF's pages and a deck's notes.
	DocLimits doctext.Limits
	// Metrics take its counts; nil counts nothing.
	Metrics *metrics.Metrics
	// Log is the runtime's logger (through redact); nil discards.
	Log *slog.Logger
	// Holder names this worker in the lease and the jobs.
	Holder string
	// Now is the clock; nil is time.Now.
	Now    func() time.Time
	Timing Timing
}

// Timing is how the transcriber paces itself; a zero field is its default.
type Timing struct {
	// LeaderTTL is the lease "transcriber"'s, renewed every third of it;
	// Standby how often a worker that does not hold it tries for it.
	LeaderTTL, Standby time.Duration
	// Lease is each claim's in Core, renewed every third of it.
	Lease time.Duration
	// Wait is how long a call to the queue waits for a version (wait_s).
	Wait time.Duration
	// Backoff and BackoffMax bound the pause after a call to Core that
	// failed; ModelBackoff the pause before a model call is tried again.
	Backoff, BackoffMax, ModelBackoff time.Duration
	// Blocked is how often a transcriber that can claim nothing looks
	// again; Recheck how often it reads the catalogue of a Core too old.
	Blocked, Recheck time.Duration
}

// Defaults of Timing.
const (
	DefaultLeaderTTL    = time.Minute
	DefaultStandby      = 20 * time.Second
	DefaultLease        = 10 * time.Minute
	DefaultWait         = 25 * time.Second
	DefaultBackoff      = time.Second
	DefaultBackoffMax   = time.Minute
	DefaultModelBackoff = 2 * time.Second
	DefaultBlocked      = time.Minute
	DefaultRecheck      = 10 * time.Minute
)

func (t Timing) withDefaults() Timing {
	for _, f := range []struct {
		v   *time.Duration
		def time.Duration
	}{{&t.LeaderTTL, DefaultLeaderTTL}, {&t.Standby, DefaultStandby}, {&t.Lease, DefaultLease}, {&t.Wait, DefaultWait},
		{&t.Backoff, DefaultBackoff}, {&t.BackoffMax, DefaultBackoffMax}, {&t.ModelBackoff, DefaultModelBackoff},
		{&t.Blocked, DefaultBlocked}, {&t.Recheck, DefaultRecheck}} {
		if *f.v <= 0 {
			*f.v = f.def
		}
	}
	return t
}

// JobRetention is how long the record of a job is kept.
const JobRetention = 90 * 24 * time.Hour

// Setting is the site's setting of the transcriber, and what of the plan
// in force it needs, as each build of the registry puts it (Set).
type Setting struct {
	Site config.SiteTranscription
	// Offer is the plan's offer in force Site.Offer names, nil when it
	// names none, or the plan no longer offers it.
	Offer *config.SchoolOffer
	// Dir is where the runtime's relative references resolve: the
	// offer's key_ref, for one of runtime.yaml's.
	Dir string
	// PerDayUSD is the plan's ceiling across the school's key, a UTC day,
	// which the transcriber's costs count against: nil for none.
	PerDayUSD *float64
	// Prices is the price table in force.
	Prices *pricing.Table
}

// SettingOf is the setting in force of the runtime's settings rt, the
// site's in them (config.Runtime.WithSite): the site's, with the plan's
// offer it names, none where the plan does not offer it, and the plan's
// ceiling across the school's key; dir is where runtime.yaml's references
// resolve, and prices the price table in force.
func SettingOf(rt config.Runtime, dir string, prices *pricing.Table) Setting {
	st := Setting{Site: rt.Site.Transcription, Dir: dir, PerDayUSD: rt.School.PerDay.USD, Prices: prices}
	for _, o := range rt.School.Offers {
		if o.ID == st.Site.Offer {
			st.Offer = &o
			break
		}
	}
	return st
}

// Status is where the transcriber stands on this worker.
type Status struct {
	// Available is whether it may run here, and Reason and Detail why
	// not (ReasonOperatorOff, ReasonCoreTooOld), in English for
	// administrators.
	Available      bool
	Reason, Detail string
	// Enabled is the site's switch.
	Enabled bool
	// Leader is this worker holding the lease, the one that claims;
	// Standby another worker holding it.
	Leader, Standby bool
	// Inflight are the versions this worker works on now.
	Inflight int
}

// Service is the transcriber of one worker process. Its methods are safe
// for concurrent use.
type Service struct {
	o   Options
	t   Timing
	now func() time.Time
	log *slog.Logger

	mu  sync.Mutex
	set Setting
	// wake is closed, and replaced, when the setting changes, a job ends,
	// or Stop is called.
	wake chan struct{}
	// avail is what the environment and Core's catalogue allow.
	avail   availability
	cat     *core.Catalogue
	leader  bool
	standby bool
	// inflight are the jobs working; reserved the pages they may send to
	// the model today (the daily quota), of reservedOn's day.
	inflight   int
	reserved   int
	reservedOn time.Time
	// rejected is the credential Core refused (401), which is not tried
	// again; noted is when Core last took one, noted in the store at most
	// once a minute.
	rejected string
	noted    time.Time
	// client is the service's client, of the credential clientFor; model
	// the offer's adapter, of the offer modelFor.
	client    *core.Service
	clientFor string
	model     *model
	modelFor  config.SchoolOffer

	jobs sync.WaitGroup
}

// availability is what the environment and Core allow.
type availability struct {
	ok             bool
	reason, detail string
	// known is whether Core's catalogue has been read, and checked when.
	known   bool
	checked time.Time
}

// New makes a transcriber. It does nothing until Run.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewAdapter == nil {
		o.NewAdapter = providers.New
	}
	if o.CoreHTTP == nil {
		o.CoreHTTP = http.DefaultClient
	}
	if o.PartPages <= 0 {
		o.PartPages = office.DefaultPartPages
	}
	if o.Mode == "" {
		o.Mode = config.TranscribeAuto
	}
	s := &Service{o: o, t: o.Timing.withDefaults(), now: o.Now, log: o.Log.With("module", "transcribe"), wake: make(chan struct{})}
	s.avail = s.environment()
	return s
}

// environment is what the operator's environment allows, before Core is
// asked.
func (s *Service) environment() availability {
	switch {
	case s.o.Mode == config.TranscribeOff:
		return availability{reason: ReasonOperatorOff, detail: "TRANSCRIBE=off"}
	case !s.o.Keeps || s.o.Store == nil || s.o.CoreBaseURL == "":
		return availability{reason: ReasonOperatorOff,
			detail: "the transcriber needs DATABASE_URL (the store in PostgreSQL), KMS_KEY_ID and CORE_BASE_URL"}
	}
	return availability{ok: true}
}

// Set puts the site's setting s in force, from the next claim on: work in
// progress goes on as it began.
func (s *Service) Set(st Setting) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.set.Site
	s.set = st
	s.poke()
	if was != st.Site {
		s.log.Info("the transcriber's setting", "enabled", st.Site.Enabled, "offer", st.Site.Offer, "offered", st.Offer != nil,
			"max_pages", st.Site.Pages(), "concurrency", st.Site.Slots(), "per_day_pages", st.Site.PerDayPages)
	}
}

// poke wakes Run. Called with mu held.
func (s *Service) poke() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// Status is where the transcriber stands on this worker now.
func (s *Service) Status() Status {
	if s == nil {
		return Status{Reason: ReasonOperatorOff, Detail: "no transcriber runs here"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Available: s.avail.ok, Reason: s.avail.reason, Detail: s.avail.detail, Enabled: s.set.Site.Enabled,
		Leader: s.leader, Standby: s.standby, Inflight: s.inflight}
}

// Check reads Core's catalogue, as Run does first, and says where the
// transcriber stands: what run and check say at the start, and refuse to
// start with TRANSCRIBE=on where it cannot run. A Core that cannot be
// reached is no answer: err says so, and the status is the environment's.
func (s *Service) Check(ctx context.Context) (Status, error) {
	if !s.environment().ok {
		return s.Status(), nil
	}
	_, err := s.catalogue(ctx)
	return s.Status(), err
}

// catalogue is Core's catalogue, read now, when it was never read or Core
// was too old at the last reading more than Recheck ago; it records what
// Core allows.
func (s *Service) catalogue(ctx context.Context) (*core.Catalogue, error) {
	s.mu.Lock()
	cat, avail := s.cat, s.avail
	s.mu.Unlock()
	if cat != nil && avail.ok {
		return cat, nil
	}
	if avail.known && s.now().Sub(avail.checked) < s.t.Recheck {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cat, err := core.FetchCatalogue(ctx, s.o.CoreHTTP, s.o.CoreBaseURL)
	if err != nil {
		return nil, fmt.Errorf("transcribe: Core's catalogue: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.avail = availability{ok: true, known: true, checked: s.now()}
	if !core.HasService(cat) {
		s.avail = availability{reason: ReasonCoreTooOld, known: true, checked: s.now(),
			detail: "Core has no transcription service (document_text.queue): it is older than AIShie-Core #43"}
		s.log.Warn("Core is too old for the transcriber: it has no transcription service; its catalogue is read again every "+
			s.t.Recheck.String(), "core", s.o.CoreBaseURL)
		return nil, nil
	}
	s.cat = cat
	return cat, nil
}

// snapshot is the setting in force, and the channel that says it changed.
func (s *Service) snapshot() (Setting, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set, s.wake
}

// role records whether this worker leads or stands by.
func (s *Service) role(leader, standby bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leader, s.standby = leader, standby
}

// Run is the transcriber, until ctx ends: while the site turns it on and
// Core offers the service, this worker takes the lease "transcriber" or
// stands by for it, and the worker holding it claims (lead). When ctx
// ends, the jobs in progress end with it: their claims lapse in Core, and
// the versions are claimed again.
func (s *Service) Run(ctx context.Context) {
	defer s.jobs.Wait()
	defer s.release()
	if !s.environment().ok {
		<-ctx.Done()
		return
	}
	backoff := s.t.Backoff
	for ctx.Err() == nil {
		st, wake := s.snapshot()
		if !st.Site.Enabled {
			s.role(false, false)
			s.release()
			sleep(ctx, wake, 0)
			continue
		}
		cat, err := s.catalogue(ctx)
		switch {
		case err != nil:
			s.log.Warn("Core's catalogue could not be read; the transcriber tries again", "err", redact.String(err.Error()), "in", backoff.String())
			sleep(ctx, wake, backoff)
			backoff = min(backoff*2, s.t.BackoffMax)
			continue
		case cat == nil:
			s.role(false, false)
			sleep(ctx, wake, s.t.Recheck)
			continue
		}
		backoff = s.t.Backoff
		ok, err := s.o.Store.AcquireLease(ctx, LeaseName, s.o.Holder, s.t.LeaderTTL)
		if err != nil || !ok {
			if err != nil && ctx.Err() == nil {
				s.log.Warn("the transcriber's lease could not be taken", "err", redact.String(err.Error()))
			}
			s.role(false, err == nil)
			sleep(ctx, wake, s.t.Standby)
			continue
		}
		s.role(true, false)
		s.log.Info("this worker is the transcriber", "holder", s.o.Holder)
		s.lead(ctx, cat)
		s.role(false, false)
	}
}

// release gives up the lease "transcriber", if this worker holds it.
func (s *Service) release() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.o.Store.ReleaseLease(ctx, LeaseName, s.o.Holder)
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

// lead is the claiming, while this worker holds the lease and the site
// keeps the transcriber on: the lease renewed every third of its time,
// jobs a worker left working ended as interrupted, old ones pruned, and
// versions claimed as slots come free. A lease lost stops the claiming;
// jobs in progress go on, under their claims in Core.
func (s *Service) lead(run context.Context, cat *core.Catalogue) {
	ctx, cancel := context.WithCancel(run)
	defer cancel()
	go func() {
		t := time.NewTicker(s.t.LeaderTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			ok, err := s.o.Store.AcquireLease(ctx, LeaseName, s.o.Holder, s.t.LeaderTTL)
			if ctx.Err() != nil {
				return
			}
			if err != nil || !ok {
				s.log.Warn("this worker lost the transcriber's lease: it claims no more", "held", ok, "err", errText(err))
				cancel()
				return
			}
		}
	}()
	s.housekeep(ctx)
	kept := s.now()
	backoff := s.t.Backoff
	for ctx.Err() == nil {
		st, wake := s.snapshot()
		if !st.Site.Enabled {
			s.release()
			return
		}
		if s.now().Sub(kept) > time.Hour {
			s.housekeep(ctx)
			kept = s.now()
		}
		svc, m, why := s.ready(ctx, cat, st)
		if why != "" {
			sleep(ctx, wake, s.t.Blocked)
			continue
		}
		free := st.Site.Slots() - s.working()
		if free <= 0 {
			sleep(ctx, wake, s.t.Blocked)
			continue
		}
		claimed, err := svc.Queue(ctx, free, s.t.Lease, s.t.Wait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d := s.claimFailed(ctx, err, backoff)
			sleep(ctx, wake, d)
			backoff = min(backoff*2, s.t.BackoffMax)
			continue
		}
		backoff = s.t.Backoff
		s.tookCredential(ctx)
		for _, c := range claimed {
			s.start(run, svc, m, st, c)
		}
	}
}

// housekeep ends as interrupted the jobs still working that no claim has
// held for longer than a lease and a minute (a worker stopped part way),
// and prunes the jobs older than JobRetention.
func (s *Service) housekeep(ctx context.Context) {
	now := s.now()
	if n, err := s.o.Store.InterruptTranscriptionJobs(ctx, now.Add(-s.t.Lease-time.Minute), now); err != nil {
		s.log.Warn("the jobs left working could not be ended", "err", redact.String(err.Error()))
	} else if n > 0 {
		s.log.Info("jobs a worker left working ended as interrupted", "jobs", n)
	}
	if n, err := s.o.Store.PruneTranscriptionJobs(ctx, now.Add(-JobRetention)); err != nil {
		s.log.Warn("old jobs could not be pruned", "err", redact.String(err.Error()))
	} else if n > 0 {
		s.log.Info("old jobs pruned", "jobs", n)
	}
}

// working is how many jobs work now.
func (s *Service) working() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight
}

// ready is the service's client and the offer's model to claim with, or
// why none is claimed now (a Blocked… reason, blockedBy's, or the offer's
// model that cannot be made, or takes no files as its adapter says).
func (s *Service) ready(ctx context.Context, cat *core.Catalogue, st Setting) (*core.Service, *model, string) {
	cred, why := s.blockedBy(ctx, st)
	if why != "" {
		return nil, nil, why
	}
	svc, err := s.clientOf(ctx, cat, cred.SecretID)
	if err != nil {
		s.log.Warn("the transcriber's credential could not be opened", "err", redact.String(err.Error()))
		return nil, nil, BlockedNoCredential
	}
	m, err := s.modelOf(ctx, st)
	if err != nil {
		s.log.Warn("the offer's model could not be made", "offer", st.Offer.ID, "err", redact.String(err.Error()))
		return nil, nil, BlockedOfferUnavailable
	}
	if m.input == InputNone {
		return nil, nil, BlockedOfferUnavailable
	}
	return svc, m, ""
}

// blockedBy is the credential to claim with, or why nothing is claimed
// now (a Blocked… reason): no credential, or the one Core refused; no
// offer, or one withdrawn or of a model that takes no files; the day's
// pages, or the plan's dollars across the school's key, spent.
func (s *Service) blockedBy(ctx context.Context, st Setting) (*store.TranscriptionCredential, string) {
	cred, err := s.o.Store.TranscriptionCredential(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, BlockedNoCredential
	case err != nil:
		s.log.Warn("the transcriber's credential could not be read", "err", redact.String(err.Error()))
		return nil, BlockedNoCredential
	case cred.RejectedAt != nil:
		return nil, BlockedCredentialRejected
	}
	s.mu.Lock()
	rejected := s.rejected == cred.SecretID
	s.mu.Unlock()
	switch {
	case rejected:
		return nil, BlockedCredentialRejected
	case st.Offer == nil && st.Site.Offer == "":
		return nil, BlockedNoOffer
	case st.Offer == nil || InputOf(st.Offer.AsModel()) == InputNone:
		return nil, BlockedOfferUnavailable
	}
	if spent, err := s.dayFull(ctx, st); err != nil || spent {
		return nil, BlockedQuotaExhausted
	}
	return cred, ""
}

// Blocked is why the transcriber, turned on with the setting st (the one
// in force, Setting, or one about to be), would claim nothing now (a
// Blocked… reason), or "" when nothing stops it, or st turns it off.
func (s *Service) Blocked(ctx context.Context, st Setting) string {
	if s == nil || s.o.Store == nil || !st.Site.Enabled {
		return ""
	}
	_, why := s.blockedBy(ctx, st)
	return why
}

// Setting is the site's setting in force.
func (s *Service) Setting() Setting {
	st, _ := s.snapshot()
	return st
}

// clientOf is the service's client with the credential secretID, made
// once for it.
func (s *Service) clientOf(ctx context.Context, cat *core.Catalogue, secretID string) (*core.Service, error) {
	s.mu.Lock()
	if s.client != nil && s.clientFor == secretID {
		defer s.mu.Unlock()
		return s.client, nil
	}
	s.mu.Unlock()
	token, err := s.o.Secrets.Resolve(ctx, secrets.SchemeSealed+secretID, "")
	if err != nil {
		return nil, err
	}
	c := core.NewService(core.NewRESTCaller(core.RESTOptions{BaseURL: s.o.CoreBaseURL, Token: token, Catalogue: cat,
		HTTPClient: s.o.CoreHTTP}))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client, s.clientFor = c, secretID
	return c, nil
}

// dayFull reports whether the day's quotas leave no room for a page more:
// the site's pages a day (the jobs' pages since the start of the UTC day,
// and those the jobs in progress hold), and the plan's dollars across the
// school's key.
func (s *Service) dayFull(ctx context.Context, st Setting) (bool, error) {
	day := store.UTCDay(s.now())
	if limit := st.Site.PerDayPages; limit != nil {
		d, err := s.o.Store.TranscriptionDay(ctx, day)
		if err != nil {
			return false, err
		}
		if d.Pages+s.held(day) >= *limit {
			return true, nil
		}
	}
	if usd := st.PerDayUSD; usd != nil {
		spent, err := s.o.Store.Spend(ctx, store.SpendScope{KeySource: config.KeySchool}, day)
		if err != nil {
			return false, err
		}
		if spent.CostPUSD >= pricing.PUSD(*usd) {
			return true, nil
		}
	}
	return false, nil
}

// held are the pages the jobs in progress hold today.
func (s *Service) held(day time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reservedOn.Equal(day) {
		return 0
	}
	return s.reserved
}

// reserve holds pages of the day's quota for a job, and reports whether
// they fit: nothing is held when they do not, and every page fits where
// the site sets no quota.
func (s *Service) reserve(ctx context.Context, st Setting, pages int) (bool, error) {
	if st.Site.PerDayPages == nil {
		return true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	day := store.UTCDay(s.now())
	if !s.reservedOn.Equal(day) {
		s.reservedOn, s.reserved = day, 0
	}
	d, err := s.o.Store.TranscriptionDay(ctx, day)
	if err != nil {
		return false, err
	}
	if d.Pages+s.reserved+pages > *st.Site.PerDayPages {
		return false, nil
	}
	s.reserved += pages
	return true, nil
}

// unreserve gives back pages a job held, on the day it held them.
func (s *Service) unreserve(on time.Time, pages int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservedOn.Equal(on) {
		s.reserved = max(s.reserved-pages, 0)
	}
}

// claimFailed says what a call to the queue that failed means, and how
// long to wait before the next: a credential Core refuses is not tried
// again until another is given; a 429 waits as Core says.
func (s *Service) claimFailed(ctx context.Context, err error, backoff time.Duration) time.Duration {
	var rl *core.RateLimitedError
	var te *core.TransientError
	reason := "refused"
	d := backoff
	switch {
	case errors.Is(err, core.ErrUnauthenticated):
		reason = "unauthenticated"
		s.rejectCredential(ctx, "Core answered 401: the credential was revoked or has expired")
		d = 0
	case core.IsReason(err, core.ReasonServiceOnly):
		s.rejectCredential(ctx, "Core answered 403 service_only: the token is not the transcription service's")
		d = 0
	case errors.As(err, &rl):
		reason, d = "rate_limited", rl.RetryAfter
	case errors.As(err, &te):
		reason = "unreachable"
	}
	if s.o.Metrics != nil {
		s.o.Metrics.TranscribeClaimErrors.WithLabelValues(reason).Inc()
	}
	s.log.Warn("the transcription queue could not be read", "reason", reason, "err", redact.String(err.Error()), "in", d.String())
	if d <= 0 {
		d = s.t.Blocked
	}
	return d + time.Duration(rand.Int64N(int64(d)/4+1)) //nolint:gosec // jitter, not a secret.
}

// rejectCredential records that Core refused the credential in use: the
// transcriber claims nothing until another is given.
func (s *Service) rejectCredential(ctx context.Context, why string) {
	s.mu.Lock()
	id := s.clientFor
	s.rejected = id
	s.mu.Unlock()
	if id == "" {
		return
	}
	s.log.Error("Core refused the transcriber's credential: it claims nothing until the site's administrators give another", "why", why)
	if err := s.o.Store.NoteTranscriptionCredential(ctx, id, false, s.now(), why); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("Core's refusal of the credential could not be noted", "err", redact.String(err.Error()))
	}
}

// tookCredential notes, at most once a minute, that Core took the
// credential in use.
func (s *Service) tookCredential(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	id := s.clientFor
	due := now.Sub(s.noted) >= time.Minute
	if due {
		s.noted = now
	}
	s.mu.Unlock()
	if !due || id == "" {
		return
	}
	if err := s.o.Store.NoteTranscriptionCredential(ctx, id, true, now, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("Core's taking of the credential could not be noted", "err", redact.String(err.Error()))
	}
}

// errText is err's text, redacted, or "" for none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return redact.String(err.Error())
}
