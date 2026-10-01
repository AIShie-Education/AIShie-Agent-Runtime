package ocr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Recognizer reads the text of a file: an Engine, or a test's.
type Recognizer interface {
	Recognize(ctx context.Context, data []byte, kind Kind, pages int, progress func(done, of int)) (*Result, error)
	// Describe names it, for what a text records of how it was made.
	Describe() string
}

// Store is what the service keeps texts in, and takes the lease of a file
// being recognized with, so that two workers do not recognize one file at
// once.
type Store interface {
	store.OCRTexts
	store.Leases
}

// Status is where the OCR of a file stands, as Text finds it.
type Status string

// The statuses.
const (
	// StatusDone: recognized; State.Text is what was, perhaps nothing.
	StatusDone Status = "done"
	// StatusPending: being recognized, here or by another worker, or
	// waiting for its turn: ask again later.
	StatusPending Status = "pending"
	// StatusFailed: OCR could not read it; State.Why says why. Kept for
	// FailedRetention, and tried again after.
	StatusFailed Status = "failed"
	// StatusBusy: not started, the queue being full, or the file or the
	// store not to be read just now: ask again later.
	StatusBusy Status = "busy"
	// StatusOff: there is no OCR here; State.Why says why.
	StatusOff Status = "off"
)

// State is what Text found.
type State struct {
	Status Status
	// Text is the text recognized, when Status is StatusDone.
	Text *store.OCRText
	// Done and Of are the pages recognized so far, of how many will be,
	// while pending here; both 0 when not known.
	Done, Of int
	// Why says, in words for the model, why it failed or is off.
	Why string
}

// How long a text is kept in the store, and a failure: a failure is tried
// again once it goes, a text recognized again.
const (
	TextRetention   = 180 * 24 * time.Hour
	FailedRetention = 24 * time.Hour
)

// Service is OCR for a worker process: at most Config.Concurrency files
// recognized at once, the others waiting their turn, at most Config.Queue
// of them; each file recognized once, in the background, and its text
// kept in the store by its checksum, where every worker reads it after.
// A nil *Service, or one without a Recognizer, is off. Within what the
// environment allows, the site's setting (Set) turns it off and on and
// chooses its languages.
type Service struct {
	// base is the recognizer the environment gives, in its languages.
	base   Recognizer
	why    string
	reason string
	detail string
	cfg    Config
	store  Store
	holder string
	m      *metrics.Metrics
	log    *slog.Logger
	now    func() time.Time
	// after starts the timer a question waits on (await), and returns
	// what it fires on and what stops it: time's, or a test's, which reads
	// how long it was set for and fires it when the test says.
	after func(time.Duration) (<-chan time.Time, func() bool)

	// ctx is the process's: its end cancels every job, whose outcome is
	// then not kept.
	ctx context.Context
	sem chan struct{}

	mu sync.Mutex
	// rec is base in the site's languages, and off the site's turning it
	// off (Set).
	rec  Recognizer
	off  bool
	jobs map[string]*job
	wg   sync.WaitGroup
}

// job is one file being recognized, or waiting to be, by rec.
type job struct {
	rec      Recognizer
	done     chan struct{}
	pages    int
	progress [2]int
	state    State
}

// Why OCR does not run here at all (Capability.Reason): the environment is
// the ceiling of the site's setting.
const (
	// ReasonTurnedOff: OCR=off.
	ReasonTurnedOff = "operator_off"
	// ReasonNotInstalled: its programs, or the languages of
	// OCR_LANGUAGES, are not installed.
	ReasonNotInstalled = "not_installed"
)

// ServiceOptions are what a Service is made of. Recognizer nil makes one
// that is off, saying Off.
type ServiceOptions struct {
	Recognizer Recognizer
	// Off is why there is no OCR, for the model and the logs; OffReason
	// the same as a code (ReasonTurnedOff, ReasonNotInstalled), and
	// OffDetail what is missing, for the site's administrators.
	Off       string
	OffReason string
	OffDetail string
	Config    Config
	Store     Store
	Holder    string
	Metrics   *metrics.Metrics
	Log       *slog.Logger
	Now       func() time.Time
}

// NewService makes the worker's OCR, which runs until ctx ends.
func NewService(ctx context.Context, o ServiceOptions) *Service {
	cfg := o.Config.WithDefaults()
	s := &Service{base: o.Recognizer, rec: o.Recognizer, why: o.Off, reason: o.OffReason, detail: o.OffDetail, cfg: cfg, store: o.Store,
		holder: o.Holder, m: o.Metrics, log: o.Log, now: o.Now, ctx: ctx, sem: make(chan struct{}, cfg.Concurrency), jobs: map[string]*job{}}
	if s.reason == "" {
		s.reason = ReasonNotInstalled
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.after = func(d time.Duration) (<-chan time.Time, func() bool) {
		t := time.NewTimer(d)
		return t.C, t.Stop
	}
	if s.why == "" {
		s.why = "OCR is off here"
	}
	return s
}

// Available reports whether s recognizes anything, and if not, why.
func (s *Service) Available() (bool, string) {
	_, why := s.current()
	return why == "", why
}

// current is the recognizer in force, or, when there is none, why.
func (s *Service) current() (Recognizer, string) {
	if s == nil {
		return nil, "OCR is off here"
	}
	if s.base == nil {
		return nil, s.why
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.off {
		return nil, "it is turned off"
	}
	return s.rec, ""
}

// Setting is the site's OCR setting (docs/design.md §11.5): whether OCR
// runs, and in which of the languages installed, tesseract's joined by +,
// "" for the environment's (OCR_LANGUAGES).
type Setting struct {
	Enabled   bool
	Languages string
}

// Multilingual is a Recognizer that recognizes in other languages than its
// own: an Engine.
type Multilingual interface {
	Recognizer
	// InLanguages is it recognizing in languages instead, or an error
	// when it has not got one of them.
	InLanguages(languages string) (Recognizer, error)
	// Installed are the languages it may recognize in.
	Installed() []string
}

// Set puts the site's setting in force for the files asked about after
// it: turned off, no file is recognized, and none is read from what was
// kept, as with OCR=off; in other languages, a file whose kept text was
// recognized in others is recognized again. Languages the recognizer has
// not got are refused, and the setting before is kept. With no OCR here
// (OCR=off, or its programs missing), it changes nothing: the environment
// is its ceiling.
func (s *Service) Set(st Setting) error {
	if s == nil || s.base == nil {
		return nil
	}
	rec := s.base
	if st.Languages != "" && st.Languages != s.cfg.Languages {
		m, ok := s.base.(Multilingual)
		if !ok {
			return fmt.Errorf("%w: it recognizes in %s alone", ErrUnavailable, s.cfg.Languages)
		}
		var err error
		if rec, err = m.InLanguages(st.Languages); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec, s.off = rec, !st.Enabled
	return nil
}

// Languages are the languages the files asked about now are recognized
// in, as a text kept records them; "" when OCR does not run.
func (s *Service) Languages() string {
	rec, _ := s.current()
	if rec == nil {
		return ""
	}
	return languagesOf(rec.Describe())
}

// Capability is what OCR can do here, as the site's administrators are
// shown it: whether the environment lets it run, and if not, why (Reason,
// Detail); the languages installed; and the environment's, the default.
type Capability struct {
	Available bool
	Reason    string
	Detail    string
	Installed []string
	Default   []string
}

// Capability says what OCR can do here, whatever the site's setting.
func (s *Service) Capability() Capability {
	if s == nil {
		return Capability{Reason: ReasonNotInstalled}
	}
	c := Capability{Available: s.base != nil, Default: strings.Split(s.cfg.Languages, "+")}
	if !c.Available {
		c.Reason, c.Detail = s.reason, s.detail
		return c
	}
	if m, ok := s.base.(Multilingual); ok {
		c.Installed = m.Installed()
	} else {
		c.Installed = c.Default
	}
	return c
}

// Wait waits for every job to end, as the process stops.
func (s *Service) Wait() {
	if s != nil {
		s.wg.Wait()
	}
}

// Text is the OCR of the file whose checksum is sum (sha256:<hex>), of
// kind, with pages pages when a PDF's are known: what the store keeps of
// it, or the job recognizing it now. When there is neither, it takes the
// file's lease and starts a job, with the bytes data gives, and waits for
// it Config.Wait at most, never past ctx's deadline less a margin for the
// answer to go on: a small file is answered at once, and a long one is
// said to be pending, for the model to ask again; asked again while it is
// recognized here, it waits as long again. A file another worker is
// recognizing is pending at once.
func (s *Service) Text(ctx context.Context, sum string, kind Kind, pages int, data func(context.Context) ([]byte, error)) State {
	rec, why := s.current()
	if rec == nil {
		s.count("off")
		return State{Status: StatusOff, Why: why}
	}
	s.mu.Lock()
	if j, ok := s.jobs[sum]; ok {
		s.mu.Unlock()
		s.count("in_progress")
		// Asked again while it is recognized here: wait as the first
		// question did, so that a model asking at once is not answered
		// at once, again and again.
		return s.await(ctx, j)
	}
	s.mu.Unlock()

	kept, err := s.store.OCRText(ctx, sum)
	switch {
	case err == nil && languagesOf(kept.Engine) != languagesOf(rec.Describe()):
		// Recognized in other languages than the site's now: recognized
		// again, and kept in their place.
	case err == nil:
		s.count(kept.Status)
		return keptState(kept)
	case !errors.Is(err, store.ErrNotFound):
		s.count("busy")
		return State{Status: StatusBusy, Why: "the runtime could not read what it keeps of it"}
	}

	j, st := s.start(ctx, rec, sum, kind, pages, data)
	if j == nil {
		return st
	}
	return s.await(ctx, j)
}

// start starts a job for the file, by rec, or says why it did not: the
// queue is full, another worker holds the file's lease, the file could not
// be read.
func (s *Service) start(ctx context.Context, rec Recognizer, sum string, kind Kind, pages int, data func(context.Context) ([]byte, error)) (*job, State) {
	s.mu.Lock()
	if j, ok := s.jobs[sum]; ok {
		st := j.stateLocked()
		s.mu.Unlock()
		s.count("in_progress")
		return nil, st
	}
	if len(s.jobs) >= s.cfg.Concurrency+s.cfg.Queue {
		s.mu.Unlock()
		s.count("busy")
		return nil, State{Status: StatusBusy, Why: "the runtime is recognizing other files just now"}
	}
	j := &job{rec: rec, done: make(chan struct{}), pages: pages}
	s.jobs[sum] = j
	s.mu.Unlock()

	forget := func() {
		s.mu.Lock()
		delete(s.jobs, sum)
		s.mu.Unlock()
	}
	held, err := s.store.AcquireLease(ctx, leaseName(sum), s.holder, s.cfg.Timeout+time.Minute)
	if err != nil || !held {
		forget()
		if err != nil {
			s.count("busy")
			return nil, State{Status: StatusBusy, Why: "the runtime could not take the file to recognize it"}
		}
		s.count("in_progress")
		return nil, State{Status: StatusPending}
	}
	bytes, err := data(ctx)
	if err != nil {
		forget()
		s.release(sum)
		s.count("busy")
		return nil, State{Status: StatusBusy, Why: "the file could not be fetched to recognize it"}
	}
	s.count("started")
	s.wg.Add(1)
	go s.run(sum, kind, bytes, j)
	return j, State{}
}

// await waits for j as Text says, and says where it stands.
func (s *Service) await(ctx context.Context, j *job) State {
	wait := s.cfg.Wait
	if dl, ok := ctx.Deadline(); ok {
		// Leave the answer most of its time: what is left, less a margin.
		wait = min(wait, dl.Sub(s.now())/2)
	}
	if wait > 0 {
		fired, stop := s.after(wait)
		defer stop()
		select {
		case <-j.done:
		case <-fired:
		case <-ctx.Done():
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return j.stateLocked()
}

// stateLocked is where j stands; s.mu held.
func (j *job) stateLocked() State {
	select {
	case <-j.done:
		return j.state
	default:
	}
	return State{Status: StatusPending, Done: j.progress[0], Of: j.progress[1]}
}

// run recognizes one file, in its turn, keeps what came of it, and lets
// its lease go.
func (s *Service) run(sum string, kind Kind, data []byte, j *job) {
	defer s.wg.Done()
	var st State
	defer func() {
		s.mu.Lock()
		j.state = st
		delete(s.jobs, sum)
		s.mu.Unlock()
		close(j.done)
	}()
	defer s.release(sum)

	s.gauges(1, 0)
	select {
	case s.sem <- struct{}{}:
	case <-s.ctx.Done():
		s.gauges(-1, 0)
		s.jobCount(kind, "cancelled")
		st = State{Status: StatusBusy, Why: "the runtime was stopping"}
		return
	}
	s.gauges(-1, 1)
	defer func() { <-s.sem; s.gauges(0, -1) }()

	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.Timeout)
	defer cancel()
	start := s.now()
	res, err := j.rec.Recognize(ctx, data, kind, j.pages, func(done, of int) {
		s.mu.Lock()
		j.progress = [2]int{done, of}
		s.mu.Unlock()
	})
	took := s.now().Sub(start)
	if s.m != nil {
		s.m.OCRJobSeconds.WithLabelValues(string(kind)).Observe(took.Seconds())
	}
	if s.ctx.Err() != nil {
		// Stopped with the process: nothing is kept, and the next worker
		// recognizes it again.
		s.jobCount(kind, "cancelled")
		st = State{Status: StatusBusy, Why: "the runtime was stopping"}
		s.log.Info("ocr: a file's recognition stopped with the runtime", "sum", short(sum), "kind", kind)
		return
	}
	t := store.OCRText{Sum: sum, Kind: string(kind), Engine: j.rec.Describe(), DurationMS: took.Milliseconds(), CreatedAt: s.now()}
	outcome := "done"
	switch {
	case err == nil:
		t.Status, t.Text, t.Pages, t.PagesOf, t.Notes = store.OCRDone, res.Text, res.Pages, res.Of, res.Notes
		for _, sec := range res.Sections {
			t.Sections = append(t.Sections, store.OCRSection{N: sec.N, Offset: sec.Offset})
		}
		if res.Pages == res.Empty {
			outcome = "empty"
		}
		s.pageCount(kind, "text", res.Pages-res.Empty-res.Failed)
		s.pageCount(kind, "empty", res.Empty)
		s.pageCount(kind, "failed", res.Failed)
	case errors.Is(err, ErrTooLarge):
		t.Status, t.Reason, outcome = store.OCRFailed, "too_large", "too_large"
	case errors.Is(err, context.DeadlineExceeded):
		t.Status, t.Reason, outcome = store.OCRFailed, "timeout", "timeout"
	case errors.Is(err, ErrMalformed):
		t.Status, t.Reason, outcome = store.OCRFailed, "malformed", "failed"
	default:
		t.Status, t.Reason, outcome = store.OCRFailed, "failed", "failed"
	}
	s.jobCount(kind, outcome)
	s.log.Info("ocr: a file was recognized", "sum", short(sum), "kind", kind, "outcome", outcome, "pages", t.Pages,
		"of", t.PagesOf, "chars", len(t.Text), "ms", took.Milliseconds())
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(s.ctx), 10*time.Second)
	defer bcancel()
	if err := s.store.PutOCRText(bctx, t); err != nil {
		s.log.Warn("ocr: a file's text not kept", "sum", short(sum), "err", err)
	}
	st = keptState(t)
}

// keptState is the state of a text the store keeps.
func keptState(t store.OCRText) State {
	if t.Status == store.OCRDone {
		return State{Status: StatusDone, Text: &t}
	}
	return State{Status: StatusFailed, Why: failureWords(t.Reason)}
}

// failureWords says a kept failure's reason to the model.
func failureWords(reason string) string {
	switch reason {
	case "too_large":
		return "it is larger than the runtime recognizes"
	case "timeout":
		return "recognizing it took longer than the runtime allows"
	case "malformed":
		return "its pages could not be rendered or recognized"
	}
	return "the OCR programs failed on it"
}

func (s *Service) release(sum string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	if err := s.store.ReleaseLease(ctx, leaseName(sum), s.holder); err != nil {
		s.log.Warn("ocr: a file's lease not let go", "sum", short(sum), "err", err)
	}
}

// leaseName is the lease of a file being recognized.
func leaseName(sum string) string { return "ocr:" + sum }

// short is a sum as logs name it: enough to tell files apart.
func short(sum string) string {
	if len(sum) > 19 {
		return sum[:19]
	}
	return sum
}

func (s *Service) count(result string) {
	if s != nil && s.m != nil {
		s.m.OCRRequests.WithLabelValues(result).Inc()
	}
}

func (s *Service) jobCount(kind Kind, outcome string) {
	if s.m != nil {
		s.m.OCRJobs.WithLabelValues(string(kind), outcome).Inc()
	}
}

func (s *Service) pageCount(kind Kind, outcome string, n int) {
	if s.m != nil && n > 0 {
		s.m.OCRPages.WithLabelValues(string(kind), outcome).Add(float64(n))
	}
}

// gauges moves the gauges of the files waiting and running.
func (s *Service) gauges(waiting, running float64) {
	if s.m != nil {
		s.m.OCRWaiting.Add(waiting)
		s.m.OCRRunning.Add(running)
	}
}

// String is the service as the start's log line says it.
func (s *Service) String() string {
	rec, why := s.current()
	if rec == nil {
		return "off: " + why
	}
	return fmt.Sprintf("%s, %d at once, %d pages a file at most", rec.Describe(), s.cfg.Concurrency, s.cfg.MaxPages)
}
