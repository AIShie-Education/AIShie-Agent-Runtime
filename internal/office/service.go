package office

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
)

// Converting converts a file: a *Converter, or a test's.
type Converting interface {
	Convert(ctx context.Context, data []byte, f Format, to Target) (*Output, error)
	// Describe names it, for the start's log line.
	Describe() string
}

// Paging cuts pages from a PDF: a *Pager, or a test's.
type Paging interface {
	Range(ctx context.Context, pdf []byte, first, last int) ([]byte, error)
	Pick(ctx context.Context, pdf []byte, pages []int) ([]byte, error)
}

// Status is where a file's conversion stands, as Convert finds it.
type Status string

// The statuses.
const (
	// StatusDone: converted; State.Out is the file made.
	StatusDone Status = "done"
	// StatusPending: being converted, or waiting for its turn: ask again
	// later.
	StatusPending Status = "pending"
	// StatusFailed: LibreOffice could not convert it; State.Why says why.
	// Kept for FailedRetention, and tried again after.
	StatusFailed Status = "failed"
	// StatusBusy: not started, the queue being full: ask again later.
	StatusBusy Status = "busy"
	// StatusOff: there is no conversion here; State.Why says why.
	StatusOff Status = "off"
)

// State is what Convert found.
type State struct {
	Status Status
	// Out is the file made, when Status is StatusDone.
	Out *Output
	// Why says, in words for the model, why it failed or is off.
	Why string
}

// FailedRetention is how long a failed conversion is kept, so that a file
// LibreOffice cannot convert is not tried at every question.
const FailedRetention = time.Hour

// Service converts Office files for a worker process: at most
// Config.Concurrency at once, the others waiting their turn, at most
// Config.Queue of them; each file converted once, in the background, and
// what was made kept in memory by the file's checksum, within
// Config.CacheBytes, for every agent of the worker. It cuts ranges of pages
// from PDFs too, a few at once, and keeps them as well. A nil *Service
// converts and cuts nothing.
//
// What is made is kept in memory, not in the store or on disk: a PDF is
// megabytes, which PostgreSQL would keep, back up and replicate for every
// deck, while making it again takes seconds, once a worker; a disk the
// runtime's container may not keep, or have room on.
type Service struct {
	conv  Converting
	why   string
	pager Paging
	cfg   Config
	m     *metrics.Metrics
	log   *slog.Logger
	now   func() time.Time

	// ctx is the process's: its end cancels every job, whose outcome is
	// then not kept.
	ctx  context.Context
	sem  chan struct{}
	cuts chan struct{}

	mu    sync.Mutex
	jobs  map[string]*job
	cache *cache
	wg    sync.WaitGroup
}

// job is one file being converted, or waiting to be.
type job struct {
	done  chan struct{}
	state State
}

// ServiceOptions are what a Service is made of. Converter nil makes one
// that converts nothing, saying Off; Pager nil one that cuts nothing.
type ServiceOptions struct {
	Converter Converting
	Off       string
	Pager     Paging
	Config    Config
	Metrics   *metrics.Metrics
	Log       *slog.Logger
	Now       func() time.Time
}

// maxCuts is how many cuts of pages run at once: they take a second or two.
const maxCuts = 2

// NewService makes the worker's conversions, which run until ctx ends.
func NewService(ctx context.Context, o ServiceOptions) *Service {
	cfg := o.Config.WithDefaults()
	s := &Service{conv: o.Converter, why: o.Off, pager: o.Pager, cfg: cfg, m: o.Metrics, log: o.Log, now: o.Now, ctx: ctx,
		sem: make(chan struct{}, cfg.Concurrency), cuts: make(chan struct{}, maxCuts), jobs: map[string]*job{}, cache: newCache(cfg.CacheBytes)}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.why == "" {
		s.why = "the conversion of Office files is off here"
	}
	return s
}

// Available reports whether s converts files, and if not, why.
func (s *Service) Available() (bool, string) {
	if s == nil {
		return false, "the conversion of Office files is off here"
	}
	if s.conv == nil {
		return false, s.why
	}
	return true, ""
}

// Cuts reports whether s cuts ranges of pages from PDFs.
func (s *Service) Cuts() bool { return s != nil && s.pager != nil }

// Wait waits for every job to end, as the process stops.
func (s *Service) Wait() {
	if s != nil {
		s.wg.Wait()
	}
}

// Convert is the file whose checksum is sum (sha256:<hex>), data, of format
// f, converted to target: what is kept of it, or the job converting it
// now, or a job started for it. The question waits for the job at most the
// configured timeout, and never past half the time ctx has left, so that
// a file converted in seconds is given at once and a slow one is said to
// be pending, for the model to ask again; its job goes on meanwhile.
func (s *Service) Convert(ctx context.Context, sum string, f Format, to Target, data []byte) State {
	if ok, why := s.Available(); !ok {
		s.count("off")
		return State{Status: StatusOff, Why: why}
	}
	key := string(to) + "\x00" + sum
	s.mu.Lock()
	if e := s.cache.get(key, s.now()); e != nil {
		s.mu.Unlock()
		if e.out != nil {
			s.count("cached")
			return State{Status: StatusDone, Out: e.out}
		}
		s.count("failed")
		return State{Status: StatusFailed, Why: e.why}
	}
	if j, ok := s.jobs[key]; ok {
		s.mu.Unlock()
		s.count("in_progress")
		return s.await(ctx, j)
	}
	if len(s.jobs) >= s.cfg.Concurrency+s.cfg.Queue {
		s.mu.Unlock()
		s.count("busy")
		return State{Status: StatusBusy, Why: "the runtime is converting other files just now"}
	}
	j := &job{done: make(chan struct{})}
	s.jobs[key] = j
	s.mu.Unlock()
	s.count("started")
	s.wg.Add(1)
	go s.run(key, sum, f, to, data, j)
	return s.await(ctx, j)
}

// await waits for j as Convert says, and says where it stands.
func (s *Service) await(ctx context.Context, j *job) State {
	wait := s.cfg.Timeout
	if dl, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(dl)/2)
	}
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-j.done:
		case <-t.C:
		case <-ctx.Done():
		}
	}
	select {
	case <-j.done:
		return j.state
	default:
		return State{Status: StatusPending}
	}
}

// run converts one file, in its turn, and keeps what came of it.
func (s *Service) run(key, sum string, f Format, to Target, data []byte, j *job) {
	defer s.wg.Done()
	var st State
	defer func() {
		s.mu.Lock()
		j.state = st
		delete(s.jobs, key)
		s.mu.Unlock()
		close(j.done)
	}()
	s.gauges(1, 0)
	select {
	case s.sem <- struct{}{}:
	case <-s.ctx.Done():
		s.gauges(-1, 0)
		s.jobCount(to, "cancelled")
		st = State{Status: StatusBusy, Why: "the runtime was stopping"}
		return
	}
	s.gauges(-1, 1)
	defer func() { <-s.sem; s.gauges(0, -1) }()

	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout+10*time.Second)
	defer cancel()
	start := s.now()
	out, err := s.conv.Convert(ctx, data, f, to)
	took := s.now().Sub(start)
	if s.m != nil {
		s.m.OfficeJobSeconds.WithLabelValues(string(to)).Observe(took.Seconds())
	}
	if s.ctx.Err() != nil {
		s.jobCount(to, "cancelled")
		st = State{Status: StatusBusy, Why: "the runtime was stopping"}
		s.log.Info("office: a file's conversion stopped with the runtime", "sum", short(sum), "from", f.Ext, "to", to)
		return
	}
	e := &entry{key: key, out: out}
	outcome := "done"
	st = State{Status: StatusDone, Out: out}
	if err != nil {
		outcome, e.why = failure(err)
		e.out, e.expires = nil, s.now().Add(FailedRetention)
		st = State{Status: StatusFailed, Why: e.why}
	}
	s.jobCount(to, outcome)
	pages, size := 0, 0
	if out != nil {
		pages, size = out.Pages, len(out.Data)
	}
	s.log.Info("office: a file was converted", "sum", short(sum), "from", f.Ext, "to", to, "outcome", outcome,
		"pages", pages, "bytes", size, "ms", took.Milliseconds())
	s.mu.Lock()
	s.cache.put(e)
	s.mu.Unlock()
}

// failure is a failed conversion's outcome, as counted, and why, in words
// for the model.
func failure(err error) (outcome, why string) {
	switch {
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout", "converting it took longer than the runtime allows"
	case errors.Is(err, ErrTooLarge):
		return "too_large", "what LibreOffice made of it is larger than the runtime keeps"
	case errors.Is(err, ErrMalformed):
		return "failed", "LibreOffice could not open it: it is damaged, password-protected, or not the kind of file it says it is"
	}
	return "failed", "LibreOffice failed on it"
}

// Range is pages first to last of pdf, whose checksum is sum, as a PDF of
// their own: kept, or cut now, a few at once, within ctx. Its error is the
// pager's, or ErrUnavailable when s cuts nothing.
func (s *Service) Range(ctx context.Context, sum string, pdf []byte, first, last int) ([]byte, error) {
	if !s.Cuts() {
		return nil, ErrUnavailable
	}
	key := "range\x00" + sum + "\x00" + strconv.Itoa(first) + "-" + strconv.Itoa(last)
	s.mu.Lock()
	if e := s.cache.get(key, s.now()); e != nil && e.out != nil {
		s.mu.Unlock()
		s.cutCount("range", "cached")
		return e.out.Data, nil
	}
	s.mu.Unlock()
	b, err := s.cut(ctx, "range", func() ([]byte, error) { return s.pager.Range(ctx, pdf, first, last) })
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache.put(&entry{key: key, out: &Output{Data: b}})
	s.mu.Unlock()
	return b, nil
}

// Pick is the pages of pdf given, in order, as a PDF of their own, for
// OCR: cut now, never kept, as OCR keeps what it reads of them.
func (s *Service) Pick(ctx context.Context, pdf []byte, pages []int) ([]byte, error) {
	if !s.Cuts() {
		return nil, ErrUnavailable
	}
	return s.cut(ctx, "pick", func() ([]byte, error) { return s.pager.Pick(ctx, pdf, pages) })
}

// cut runs one cut in its turn, and counts it.
func (s *Service) cut(ctx context.Context, op string, f func() ([]byte, error)) ([]byte, error) {
	select {
	case s.cuts <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.cuts }()
	start := s.now()
	b, err := f()
	if s.m != nil {
		s.m.OfficeCutSeconds.WithLabelValues(op).Observe(s.now().Sub(start).Seconds())
	}
	if err != nil {
		s.cutCount(op, "failed")
		return nil, err
	}
	s.cutCount(op, "done")
	return b, nil
}

// short is a sum as logs name it: enough to tell files apart.
func short(sum string) string {
	if len(sum) > 19 {
		return sum[:19]
	}
	return sum
}

func (s *Service) count(result string) {
	if s != nil && s.m != nil {
		s.m.OfficeRequests.WithLabelValues(result).Inc()
	}
}

func (s *Service) jobCount(to Target, outcome string) {
	if s.m != nil {
		s.m.OfficeJobs.WithLabelValues(string(to), outcome).Inc()
	}
}

func (s *Service) cutCount(op, outcome string) {
	if s.m != nil {
		s.m.OfficeCuts.WithLabelValues(op, outcome).Inc()
	}
}

// gauges moves the gauges of the files waiting and running.
func (s *Service) gauges(waiting, running float64) {
	if s.m != nil {
		s.m.OfficeWaiting.Add(waiting)
		s.m.OfficeRunning.Add(running)
	}
}

// String is the service as the start's log line says it.
func (s *Service) String() string {
	cuts := "PDFs given whole"
	if s.Cuts() {
		cuts = "PDFs cut into ranges of pages"
	}
	if ok, why := s.Available(); !ok {
		return "off: " + why + "; " + cuts
	}
	return fmt.Sprintf("%s, %d at once, %d pages a file at most; %s", s.conv.Describe(), s.cfg.Concurrency, s.cfg.MaxPages, cuts)
}
