package ocr

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

// fakeRecognizer recognizes by a test's function, and counts its runs.
type fakeRecognizer struct {
	runs atomic.Int32
	fn   func(ctx context.Context, data []byte, progress func(done, of int)) (*Result, error)
}

func (f *fakeRecognizer) Recognize(ctx context.Context, data []byte, _ Kind, _ int, progress func(done, of int)) (*Result, error) {
	f.runs.Add(1)
	return f.fn(ctx, data, progress)
}

func (*fakeRecognizer) Describe() string { return "fake 1.0 chi_sim+chi_tra+eng 300dpi" }

func sumOf(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// gate is a recognizer that recognizes "## Page 1\n"+data once released,
// telling its progress first.
func gate() (*fakeRecognizer, chan struct{}) {
	release := make(chan struct{})
	return &fakeRecognizer{fn: func(ctx context.Context, data []byte, progress func(done, of int)) (*Result, error) {
		progress(1, 3)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &Result{Text: "## Page 1\n" + string(data), Sections: []Section{{N: 1}}, Pages: 1, Of: 1}, nil
	}}, release
}

func newTestService(t *testing.T, ctx context.Context, rec Recognizer, cfg Config, st Store) (*Service, *metrics.Metrics) {
	t.Helper()
	if st == nil {
		st = memstore.New()
	}
	m := metrics.New(prometheus.NewRegistry())
	s := NewService(ctx, ServiceOptions{Recognizer: rec, Config: cfg, Store: st, Holder: "w1", Metrics: m})
	t.Cleanup(s.Wait)
	return s, m
}

// bytesOf is a file's data, counting how often it is fetched.
func bytesOf(text string, fetched *atomic.Int32) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) {
		fetched.Add(1)
		return []byte(text), nil
	}
}

// TestServiceRecognizesOnceInTheBackground: the first question about a
// file starts its recognition, fetching it once, and is told it is pending
// with its progress; a question meanwhile is told the same, fetching
// nothing; once done, the text is kept and every later question reads it,
// with no run and no fetch.
func TestServiceRecognizesOnceInTheBackground(t *testing.T) {
	rec, release := gate()
	st := memstore.New()
	s, m := newTestService(t, t.Context(), rec, Config{Wait: -1}, st)
	var fetched atomic.Int32
	sum := sumOf("a")
	first := s.Text(t.Context(), sum, PDF, 3, bytesOf("期中考試", &fetched))
	if first.Status != StatusPending {
		t.Fatalf("the first question: %+v", first)
	}
	eventually(t, "progress", func() bool { return s.Text(t.Context(), sum, PDF, 3, bytesOf("x", &fetched)).Done == 1 })
	if st := s.Text(t.Context(), sum, PDF, 3, bytesOf("x", &fetched)); st.Status != StatusPending || st.Of != 3 {
		t.Errorf("a question meanwhile: %+v", st)
	}
	close(release)
	eventually(t, "the text kept", func() bool {
		_, err := st.OCRText(t.Context(), sum)
		return err == nil
	})
	done := s.Text(t.Context(), sum, PDF, 3, bytesOf("x", &fetched))
	if done.Status != StatusDone || done.Text.Text != "## Page 1\n期中考試" || done.Text.Engine != rec.Describe() ||
		len(done.Text.Sections) != 1 || done.Text.Kind != store.OCRPDF {
		t.Errorf("done: %+v %+v", done, done.Text)
	}
	if rec.runs.Load() != 1 || fetched.Load() != 1 {
		t.Errorf("%d runs and %d fetches, want one of each", rec.runs.Load(), fetched.Load())
	}
	if n := testutil.ToFloat64(m.OCRJobs.WithLabelValues("pdf", "done")); n != 1 {
		t.Errorf("ocr_jobs_total{pdf, done} = %v", n)
	}
	for result, want := range map[string]float64{"started": 1, "in_progress": 2, "done": 1} {
		if n := testutil.ToFloat64(m.OCRRequests.WithLabelValues(result)); n < want {
			t.Errorf("ocr_requests_total{%s} = %v, want at least %v", result, n, want)
		}
	}
	if held, _ := st.AcquireLease(t.Context(), leaseName(sum), "w2", time.Minute); !held {
		t.Error("the file's lease was not let go")
	}
}

// TestServiceWaits: a file recognized within Config.Wait is answered at
// once; the wait never passes half of what the question's context has
// left, and a question asked again while the file is recognized waits as
// long again. How long each question waits is read off the timer it sets,
// which the test holds, on a clock that stands still: what the test
// checks is what the service decided, never how long the machine took.
func TestServiceWaits(t *testing.T) {
	quick := &fakeRecognizer{fn: func(context.Context, []byte, func(int, int)) (*Result, error) {
		return &Result{Text: "看板", Pages: 1, Of: 1}, nil
	}}
	s, _ := newTestService(t, t.Context(), quick, Config{Wait: 5 * time.Second}, nil)
	// Its timer fires only long after the job's end, which is to answer.
	waits := heldTimers(s, func() <-chan time.Time { return time.After(30 * time.Second) })
	var fetched atomic.Int32
	if st := s.Text(t.Context(), sumOf("b"), Image, 0, bytesOf("img", &fetched)); st.Status != StatusDone || st.Text.Text != "看板" {
		t.Errorf("a quick file: %+v", st)
	}
	if w := waits(); !slices.Equal(w, []time.Duration{5 * time.Second}) {
		t.Errorf("a quick file set its wait for %v, want Config.Wait, ended by the job's end", w)
	}

	// A file recognized until the test is done with it, which says when
	// it has told its progress.
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	slow := &fakeRecognizer{fn: func(ctx context.Context, data []byte, progress func(done, of int)) (*Result, error) {
		progress(1, 3)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &Result{Text: string(data), Pages: 1, Of: 1}, nil
	}}
	s, _ = newTestService(t, t.Context(), slow, Config{Wait: 5 * time.Second}, nil)
	now := time.Now()
	s.now = func() time.Time { return now }
	// Its timers fire at once: the job is not done by then.
	waits = heldTimers(s, func() <-chan time.Time {
		fired := make(chan time.Time, 1)
		fired <- now
		return fired
	})
	ctx, cancel := context.WithDeadline(t.Context(), now.Add(200*time.Millisecond))
	defer cancel()
	if st := s.Text(ctx, sumOf("c"), PDF, 3, bytesOf("slow", &fetched)); st.Status != StatusPending {
		t.Errorf("a slow file: %+v", st)
	}
	<-started
	// Asked again while it is recognized here, the question waits as the
	// first did, not answered at once.
	ctx, cancel = context.WithDeadline(t.Context(), now.Add(200*time.Millisecond))
	defer cancel()
	if st := s.Text(ctx, sumOf("c"), PDF, 3, bytesOf("slow", &fetched)); st.Status != StatusPending || st.Done != 1 {
		t.Errorf("the slow file asked again: %+v", st)
	}
	if w := waits(); !slices.Equal(w, []time.Duration{100 * time.Millisecond, 100 * time.Millisecond}) {
		t.Errorf("questions with 200 ms left set their waits for %v, want half of it each, the second as the first", w)
	}
	if fetched.Load() != 2 {
		t.Errorf("fetched %d times, want once a file", fetched.Load())
	}
}

// heldTimers has s's questions wait on timers the test holds: each fires
// on what fire gives it, and the waits they were set for are recorded,
// which the func returned reads.
func heldTimers(s *Service, fire func() <-chan time.Time) func() []time.Duration {
	var mu sync.Mutex
	var waits []time.Duration
	s.after = func(d time.Duration) (<-chan time.Time, func() bool) {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return fire(), func() bool { return true }
	}
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(waits)
	}
}

// TestServiceTurns: at most Concurrency files at once, the others waiting
// their turn, and past Queue of them, busy.
func TestServiceTurns(t *testing.T) {
	var running, most atomic.Int32
	release := make(chan struct{})
	rec := &fakeRecognizer{fn: func(ctx context.Context, data []byte, _ func(int, int)) (*Result, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		return &Result{Text: string(data), Pages: 1, Of: 1}, nil
	}}
	s, m := newTestService(t, t.Context(), rec, Config{Wait: -1, Concurrency: 1, Queue: 2}, nil)
	var fetched atomic.Int32
	for _, c := range []string{"1", "2", "3"} {
		if st := s.Text(t.Context(), sumOf(c), PDF, 1, bytesOf(c, &fetched)); st.Status != StatusPending {
			t.Fatalf("file %s: %+v", c, st)
		}
	}
	if st := s.Text(t.Context(), sumOf("4"), PDF, 1, bytesOf("4", &fetched)); st.Status != StatusBusy {
		t.Errorf("a fourth file past the queue: %+v", st)
	}
	eventually(t, "one running, two waiting", func() bool {
		return testutil.ToFloat64(m.OCRRunning) == 1 && testutil.ToFloat64(m.OCRWaiting) == 2
	})
	close(release)
	s.Wait()
	if most.Load() != 1 {
		t.Errorf("%d files recognized at once, want 1", most.Load())
	}
	if fetched.Load() != 3 {
		t.Errorf("fetched %d files, want the three started", fetched.Load())
	}
}

// TestServiceElsewhere: a file another worker holds the lease of is
// pending, and neither fetched nor run here.
func TestServiceElsewhere(t *testing.T) {
	rec, release := gate()
	defer close(release)
	st := memstore.New()
	sum := sumOf("d")
	if held, err := st.AcquireLease(t.Context(), leaseName(sum), "w2", time.Minute); !held || err != nil {
		t.Fatal(held, err)
	}
	s, _ := newTestService(t, t.Context(), rec, Config{Wait: -1}, st)
	var fetched atomic.Int32
	if got := s.Text(t.Context(), sum, PDF, 1, bytesOf("x", &fetched)); got.Status != StatusPending || fetched.Load() != 0 || rec.runs.Load() != 0 {
		t.Errorf("a file another worker recognizes: %+v, %d fetches", got, fetched.Load())
	}
}

// TestServiceFailures: a failure is kept, with why, and read as failed
// after without running again; a file that cannot be fetched, or a store
// that cannot be read, is busy and keeps nothing; a service off says why.
func TestServiceFailures(t *testing.T) {
	st := memstore.New()
	failing := &fakeRecognizer{fn: func(context.Context, []byte, func(int, int)) (*Result, error) {
		return nil, ErrTooLarge
	}}
	s, m := newTestService(t, t.Context(), failing, Config{Wait: time.Second}, st)
	var fetched atomic.Int32
	if got := s.Text(t.Context(), sumOf("e"), Image, 0, bytesOf("x", &fetched)); got.Status != StatusFailed ||
		got.Why != "it is larger than the runtime recognizes" {
		t.Errorf("a file too large: %+v", got)
	}
	if got := s.Text(t.Context(), sumOf("e"), Image, 0, bytesOf("x", &fetched)); got.Status != StatusFailed || failing.runs.Load() != 1 {
		t.Errorf("asked again: %+v, %d runs", got, failing.runs.Load())
	}
	if kept, err := st.OCRText(t.Context(), sumOf("e")); err != nil || kept.Reason != "too_large" || kept.Status != store.OCRFailed {
		t.Errorf("kept %+v %v", kept, err)
	}
	if n := testutil.ToFloat64(m.OCRJobs.WithLabelValues("image", "too_large")); n != 1 {
		t.Errorf("ocr_jobs_total{image, too_large} = %v", n)
	}

	noFetch := func(context.Context) ([]byte, error) { return nil, errors.New("HTTP 403") }
	if got := s.Text(t.Context(), sumOf("f"), PDF, 1, noFetch); got.Status != StatusBusy {
		t.Errorf("a file not fetched: %+v", got)
	}
	if held, _ := st.AcquireLease(t.Context(), leaseName(sumOf("f")), "w2", time.Minute); !held {
		t.Error("the lease of a file not fetched was kept")
	}

	broken := &brokenStore{Store: memstore.New()}
	s, _ = newTestService(t, t.Context(), failing, Config{}, broken)
	if got := s.Text(t.Context(), sumOf("g"), PDF, 1, bytesOf("x", &fetched)); got.Status != StatusBusy {
		t.Errorf("a store that cannot be read: %+v", got)
	}

	off := NewService(t.Context(), ServiceOptions{Off: "tesseract not installed"})
	if got := off.Text(t.Context(), sumOf("h"), PDF, 1, bytesOf("x", &fetched)); got.Status != StatusOff || got.Why != "tesseract not installed" {
		t.Errorf("off: %+v", got)
	}
	var none *Service
	if ok, why := none.Available(); ok || why == "" {
		t.Errorf("a nil service is available: %v %q", ok, why)
	}
}

type brokenStore struct{ *memstore.Store }

func (*brokenStore) OCRText(context.Context, string) (store.OCRText, error) {
	return store.OCRText{}, errors.New("connection refused")
}

// TestServiceStops: the process stopping cancels the recognition in
// progress and the files waiting, keeps nothing of them, and lets their
// leases go, for the next worker to recognize them.
func TestServiceStops(t *testing.T) {
	rec, release := gate()
	defer close(release)
	st := memstore.New()
	ctx, cancel := context.WithCancel(t.Context())
	s, m := newTestService(t, ctx, rec, Config{Wait: -1, Concurrency: 1}, st)
	var fetched atomic.Int32
	for _, c := range []string{"i", "j"} {
		if got := s.Text(t.Context(), sumOf(c), PDF, 1, bytesOf(c, &fetched)); got.Status != StatusPending {
			t.Fatalf("file %s: %+v", c, got)
		}
	}
	cancel()
	s.Wait()
	for _, c := range []string{"i", "j"} {
		if _, err := st.OCRText(t.Context(), sumOf(c)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("file %s was kept: %v", c, err)
		}
		if held, _ := st.AcquireLease(t.Context(), leaseName(sumOf(c)), "w2", time.Minute); !held {
			t.Errorf("file %s's lease was kept", c)
		}
	}
	if n := testutil.ToFloat64(m.OCRJobs.WithLabelValues("pdf", "cancelled")); n != 2 {
		t.Errorf("ocr_jobs_total{pdf, cancelled} = %v", n)
	}
}

// eventually waits for cond, failing the test after a deadline.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// multilingual is a fake recognizer in the languages it is given, of
// those installed.
type multilingual struct {
	*fakeRecognizer
	langs string
}

func (m multilingual) Describe() string { return "fake 1.0 " + m.langs + " 300dpi" }

func (m multilingual) Installed() []string { return []string{"chi_sim", "chi_tra", "eng"} }

func (m multilingual) InLanguages(l string) (Recognizer, error) {
	for _, x := range strings.Split(l, "+") {
		if x != "chi_sim" && x != "chi_tra" && x != "eng" {
			return nil, ErrUnavailable
		}
	}
	return multilingual{m.fakeRecognizer, l}, nil
}

// TestServiceTakesTheSitesSetting: turned off by the site, no file is
// recognized and no kept text read, as with OCR=off, and turned on again it
// reads them; in other languages, a file whose text was kept in others is
// recognized again, in these, and kept in its place; languages not
// installed are refused, keeping the setting before. Capability says what
// the environment allows.
func TestServiceTakesTheSitesSetting(t *testing.T) {
	st := memstore.New()
	base := &fakeRecognizer{fn: func(_ context.Context, data []byte, _ func(done, of int)) (*Result, error) {
		return &Result{Text: string(data), Pages: 1, Of: 1}, nil
	}}
	rec := multilingual{base, "chi_sim+chi_tra+eng"}
	s, _ := newTestService(t, t.Context(), rec, Config{Wait: time.Second}, st)
	var fetched atomic.Int32
	sum := sumOf("b")
	if got := s.Text(t.Context(), sum, Image, 0, bytesOf("第一章", &fetched)); got.Status != StatusDone || got.Text.Text != "第一章" {
		t.Fatalf("first: %+v", got)
	}
	if c := s.Capability(); !c.Available || strings.Join(c.Installed, " ") != "chi_sim chi_tra eng" || strings.Join(c.Default, "+") != "chi_sim+chi_tra+eng" {
		t.Errorf("Capability = %+v", c)
	}

	if err := s.Set(Setting{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if ok, why := s.Available(); ok || why != "it is turned off" {
		t.Errorf("turned off: Available = %v, %q", ok, why)
	}
	if got := s.Text(t.Context(), sum, Image, 0, bytesOf("第一章", &fetched)); got.Status != StatusOff {
		t.Errorf("turned off, a kept text: %+v", got)
	}
	if err := s.Set(Setting{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := s.Text(t.Context(), sum, Image, 0, bytesOf("第一章", &fetched)); got.Status != StatusDone || base.runs.Load() != 1 {
		t.Errorf("on again, the kept text: %+v, %d runs", got, base.runs.Load())
	}

	// In English alone: recognized again, and kept in its place.
	if err := s.Set(Setting{Enabled: true, Languages: "eng"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Text(t.Context(), sum, Image, 0, bytesOf("Chapter one", &fetched)); got.Status != StatusDone || got.Text.Text != "Chapter one" ||
		base.runs.Load() != 2 || fetched.Load() != 2 {
		t.Fatalf("in other languages: %+v, %d runs, %d fetched", got, base.runs.Load(), fetched.Load())
	}
	kept, err := st.OCRText(t.Context(), sum)
	if err != nil || kept.Engine != "fake 1.0 eng 300dpi" {
		t.Errorf("kept: %+v %v", kept, err)
	}
	if got := s.Text(t.Context(), sum, Image, 0, bytesOf("x", &fetched)); got.Status != StatusDone || base.runs.Load() != 2 {
		t.Errorf("in the same languages again: %+v, %d runs", got, base.runs.Load())
	}

	if err := s.Set(Setting{Enabled: false, Languages: "jpn"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a language not installed: %v", err)
	}
	if ok, _ := s.Available(); !ok || !strings.Contains(s.String(), " eng ") {
		t.Errorf("a refused setting changed it: %s", s.String())
	}
	// The environment's languages again, by naming none.
	if err := s.Set(Setting{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.String(), "chi_sim+chi_tra+eng") {
		t.Errorf("the environment's languages: %s", s.String())
	}
}

// TestServiceOffByTheEnvironment: with no recognizer, the site's setting
// changes nothing, and Capability says why.
func TestServiceOffByTheEnvironment(t *testing.T) {
	s := NewService(t.Context(), ServiceOptions{Off: "it is turned off", OffReason: ReasonTurnedOff, Store: memstore.New()})
	if err := s.Set(Setting{Enabled: true, Languages: "eng"}); err != nil {
		t.Fatal(err)
	}
	if ok, why := s.Available(); ok || why != "it is turned off" {
		t.Errorf("Available = %v, %q", ok, why)
	}
	if c := s.Capability(); c.Available || c.Reason != ReasonTurnedOff || len(c.Installed) != 0 {
		t.Errorf("Capability = %+v", c)
	}
	missing := NewService(t.Context(), ServiceOptions{Off: "its programs are not installed", OffDetail: "tesseract not installed", Store: memstore.New()})
	if c := missing.Capability(); c.Available || c.Reason != ReasonNotInstalled || c.Detail != "tesseract not installed" {
		t.Errorf("Capability = %+v", c)
	}
	var none *Service
	if c := none.Capability(); c.Available || c.Reason != ReasonNotInstalled {
		t.Errorf("a nil service's Capability = %+v", c)
	}
	// A recognizer of one language set cannot be given another.
	fixed := NewService(t.Context(), ServiceOptions{Recognizer: &fakeRecognizer{}, Store: memstore.New()})
	if err := fixed.Set(Setting{Enabled: true, Languages: "eng"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("another language of a recognizer of one: %v", err)
	}
	if err := fixed.Set(Setting{Enabled: true, Languages: DefaultLanguages}); err != nil {
		t.Errorf("its own languages, named: %v", err)
	}
}
