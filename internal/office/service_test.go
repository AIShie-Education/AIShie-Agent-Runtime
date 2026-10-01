package office

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
)

// stubConverter converts as a test says: after release is closed (nil: at
// once), with out or err.
type stubConverter struct {
	calls   atomic.Int32
	release chan struct{}
	out     *Output
	err     error
}

func (c *stubConverter) Convert(ctx context.Context, data []byte, f Format, to Target) (*Output, error) {
	c.calls.Add(1)
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.out, nil
}

func (c *stubConverter) Describe() string { return "LibreOffice 0.0 stub" }

// stubPager cuts as a test says, and counts its cuts.
type stubPager struct {
	ranges, picks atomic.Int32
	err           error
}

func (p *stubPager) Range(_ context.Context, pdf []byte, first, last int) ([]byte, error) {
	p.ranges.Add(1)
	return []byte("range"), p.err
}

func (p *stubPager) Pick(_ context.Context, pdf []byte, pages []int) ([]byte, error) {
	p.picks.Add(1)
	return []byte("picked"), p.err
}

const sum1, sum2, sum3 = "sha256:01", "sha256:02", "sha256:03"

var deck = Format{"pptx", Slides, true}

func newTestService(t *testing.T, conv Converting, pager Paging, cfg Config) (*Service, *metrics.Metrics, context.CancelFunc) {
	t.Helper()
	m := metrics.New(prometheus.NewRegistry())
	ctx, cancel := context.WithCancel(context.Background())
	s := NewService(ctx, ServiceOptions{Converter: conv, Pager: pager, Config: cfg, Metrics: m})
	t.Cleanup(func() { cancel(); s.Wait() })
	return s, m, cancel
}

// TestServiceConvertsOnce: questions about one file at once share one
// conversion; a later one is given what was kept; the file's other
// targets are other conversions.
func TestServiceConvertsOnce(t *testing.T) {
	conv := &stubConverter{release: make(chan struct{}), out: &Output{Data: []byte("%PDF"), Pages: 3}}
	s, m, _ := newTestService(t, conv, nil, Config{})
	var wg sync.WaitGroup
	states := make([]State, 3)
	for i := range states {
		wg.Add(1)
		go func() {
			defer wg.Done()
			states[i] = s.Convert(context.Background(), sum1, deck, ToPDF, []byte("deck"))
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(conv.release)
	wg.Wait()
	for i, st := range states {
		if st.Status != StatusDone || st.Out.Pages != 3 {
			t.Errorf("question %d: %+v", i, st)
		}
	}
	if st := s.Convert(context.Background(), sum1, deck, ToPDF, []byte("deck")); st.Status != StatusDone || string(st.Out.Data) != "%PDF" {
		t.Errorf("kept: %+v", st)
	}
	if n := conv.calls.Load(); n != 1 {
		t.Errorf("converted %d times", n)
	}
	if st := s.Convert(context.Background(), sum1, deck, ToPPTX, []byte("deck")); st.Status != StatusDone || conv.calls.Load() != 2 {
		t.Errorf("its PowerPoint form: %+v, %d conversions", st, conv.calls.Load())
	}
	// Five questions: the three at once (one started, the others in
	// progress, or kept should one come after), the one after, kept, and
	// the PowerPoint form's, started.
	requests := func(result string) float64 { return testutil.ToFloat64(m.OfficeRequests.WithLabelValues(result)) }
	if requests("started") != 2 || requests("cached") < 1 || requests("started")+requests("in_progress")+requests("cached") != 5 {
		t.Errorf("started %v, in progress %v, cached %v", requests("started"), requests("in_progress"), requests("cached"))
	}
	if got := testutil.ToFloat64(m.OfficeJobs.WithLabelValues("pdf", "done")); got != 1 {
		t.Errorf("office_conversions_total done %v", got)
	}
}

// TestServicePending: a question waits for its file at most half the time
// its context has left; past it, the file is pending, and its conversion
// goes on, for a later question to find. How long the question waits is
// read off the timer it sets, which the test holds and fires at once, on a
// clock that stands still: what the test checks is what the service
// decided, never how long the machine took.
func TestServicePending(t *testing.T) {
	conv := &stubConverter{release: make(chan struct{}), out: &Output{Data: []byte("%PDF"), Pages: 1}}
	s, _, _ := newTestService(t, conv, nil, Config{})
	now := time.Now()
	s.now = func() time.Time { return now }
	var mu sync.Mutex
	var waits []time.Duration
	s.after = func(d time.Duration) (<-chan time.Time, func() bool) {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		fired := make(chan time.Time, 1)
		fired <- now
		return fired, func() bool { return true }
	}
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(2*time.Second))
	defer cancel()
	if st := s.Convert(ctx, sum1, deck, ToPDF, []byte("deck")); st.Status != StatusPending {
		t.Errorf("a slow file: %+v", st)
	}
	mu.Lock()
	if !slices.Equal(waits, []time.Duration{time.Second}) {
		t.Errorf("a question with 2 s left set its wait for %v, want half of it", waits)
	}
	mu.Unlock()
	close(conv.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := s.Convert(context.Background(), sum1, deck, ToPDF, []byte("deck"))
		if st.Status == StatusDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never converted: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if conv.calls.Load() != 1 {
		t.Errorf("converted %d times", conv.calls.Load())
	}
}

// TestServiceFailureKept: a file LibreOffice fails on is said so, and kept
// so for FailedRetention, then tried again.
func TestServiceFailureKept(t *testing.T) {
	conv := &stubConverter{err: ErrMalformed}
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	m := metrics.New(prometheus.NewRegistry())
	s := NewService(context.Background(), ServiceOptions{Converter: conv, Metrics: m, Now: func() time.Time { return now }})
	st := s.Convert(context.Background(), sum1, deck, ToPDF, nil)
	if st.Status != StatusFailed || st.Why != "LibreOffice could not open it: it is damaged, password-protected, or not the kind of file it says it is" {
		t.Errorf("a file that does not convert: %+v", st)
	}
	if st := s.Convert(context.Background(), sum1, deck, ToPDF, nil); st.Status != StatusFailed || conv.calls.Load() != 1 {
		t.Errorf("asked again: %+v, %d conversions", st, conv.calls.Load())
	}
	now = now.Add(FailedRetention + time.Second)
	if st := s.Convert(context.Background(), sum1, deck, ToPDF, nil); st.Status != StatusFailed || conv.calls.Load() != 2 {
		t.Errorf("past its retention: %+v, %d conversions", st, conv.calls.Load())
	}
	conv.err = ErrTimeout
	if st := s.Convert(context.Background(), sum2, deck, ToPDF, nil); st.Why != "converting it took longer than the runtime allows" {
		t.Errorf("a timeout: %+v", st)
	}
	if got := testutil.ToFloat64(m.OfficeJobs.WithLabelValues("pdf", "timeout")); got != 1 {
		t.Errorf("timeouts counted %v", got)
	}
	s.Wait()
}

// TestServiceBusy: past Concurrency+Queue files, a file is not started,
// and said busy; one converts at a time.
func TestServiceBusy(t *testing.T) {
	conv := &stubConverter{release: make(chan struct{}), out: &Output{Data: []byte("%PDF")}}
	s, m, _ := newTestService(t, conv, nil, Config{Concurrency: 1, Queue: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	for _, sum := range []string{sum1, sum2} {
		if st := s.Convert(ctx, sum, deck, ToPDF, nil); st.Status != StatusPending {
			t.Errorf("%s: %+v", sum, st)
		}
	}
	if st := s.Convert(ctx, sum3, deck, ToPDF, nil); st.Status != StatusBusy || st.Why == "" {
		t.Errorf("past the queue: %+v", st)
	}
	if n := conv.calls.Load(); n != 1 {
		t.Errorf("%d converting at once", n)
	}
	if got := testutil.ToFloat64(m.OfficeWaiting); got != 1 {
		t.Errorf("waiting %v", got)
	}
	close(conv.release)
}

// TestServiceStopping: the process stopping cancels the conversions, which
// are not kept.
func TestServiceStopping(t *testing.T) {
	conv := &stubConverter{release: make(chan struct{}), out: &Output{Data: []byte("%PDF")}}
	s, m, stop := newTestService(t, conv, nil, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = s.Convert(ctx, sum1, deck, ToPDF, nil)
	stop()
	s.Wait()
	if got := testutil.ToFloat64(m.OfficeJobs.WithLabelValues("pdf", "cancelled")); got != 1 {
		t.Errorf("cancelled %v", got)
	}
	s.mu.Lock()
	kept := s.cache.lru.Len()
	s.mu.Unlock()
	if kept != 0 {
		t.Errorf("%d kept of a stopped conversion", kept)
	}
}

func TestServiceOff(t *testing.T) {
	var none *Service
	if ok, why := none.Available(); ok || why == "" || none.Cuts() {
		t.Error("a nil service converts")
	}
	s := NewService(context.Background(), ServiceOptions{Off: "LibreOffice is not installed"})
	if st := s.Convert(context.Background(), sum1, deck, ToPDF, nil); st.Status != StatusOff || st.Why != "LibreOffice is not installed" {
		t.Errorf("off: %+v", st)
	}
	if _, err := s.Range(context.Background(), sum1, nil, 1, 2); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a range with no pager: %v", err)
	}
	if s.String() != "off: LibreOffice is not installed; PDFs given whole" {
		t.Errorf("said as %q", s.String())
	}
}

// TestServiceCuts: a range is cut once and kept; picked pages are cut every
// time; a pager's failure is its caller's.
func TestServiceCuts(t *testing.T) {
	p := &stubPager{}
	s, m, _ := newTestService(t, nil, p, Config{})
	for range 2 {
		b, err := s.Range(context.Background(), sum1, []byte("%PDF"), 1, 10)
		if err != nil || string(b) != "range" {
			t.Fatalf("range: %q %v", b, err)
		}
	}
	if _, err := s.Range(context.Background(), sum1, []byte("%PDF"), 11, 20); err != nil || p.ranges.Load() != 2 {
		t.Errorf("another range: %v, %d cut", err, p.ranges.Load())
	}
	for range 2 {
		if b, err := s.Pick(context.Background(), []byte("%PDF"), []int{2, 5}); err != nil || string(b) != "picked" {
			t.Fatalf("pick: %q %v", b, err)
		}
	}
	if p.picks.Load() != 2 {
		t.Errorf("picked %d times", p.picks.Load())
	}
	if got := testutil.ToFloat64(m.OfficeCuts.WithLabelValues("range", "cached")); got != 1 {
		t.Errorf("ranges cached %v", got)
	}
	p.err = ErrMalformed
	if _, err := s.Range(context.Background(), sum2, []byte("x"), 1, 2); !errors.Is(err, ErrMalformed) {
		t.Errorf("a failed cut: %v", err)
	}
	if s.String() != "off: the conversion of Office files is off here; PDFs cut into ranges of pages" {
		t.Errorf("said as %q", s.String())
	}
}

// TestCacheBound: the cache keeps at most its bound, the entry used least
// recently going first, and nothing larger than the whole.
func TestCacheBound(t *testing.T) {
	c := newCache(1000)
	now := time.Now()
	c.put(&entry{key: "a", out: &Output{Data: make([]byte, 300)}})
	c.put(&entry{key: "b", out: &Output{Data: make([]byte, 300)}})
	_ = c.get("a", now)
	c.put(&entry{key: "c", out: &Output{Data: make([]byte, 300)}})
	if c.get("b", now) != nil || c.get("a", now) == nil || c.get("c", now) == nil || c.size > 1000 {
		t.Errorf("kept %v, %d bytes", c.byKey, c.size)
	}
	c.put(&entry{key: "d", out: &Output{Data: make([]byte, 2000)}})
	if c.get("d", now) != nil {
		t.Error("an entry past the whole bound was kept")
	}
	c.put(&entry{key: "e", why: "no", expires: now.Add(time.Minute)})
	if c.get("e", now) == nil || c.get("e", now.Add(2*time.Minute)) != nil {
		t.Error("a failure is not kept for its time alone")
	}
}

// TestServiceTakesRendition: Core's PDF of a file is fetched once, its
// pages counted, kept by the file's checksum as a conversion is, and found
// by a conversion of the file after, LibreOffice never run; with or
// without LibreOffice here, and whatever a conversion of the file said
// before. One of more pages than a PDF made here has is cut to so many,
// and said to be; where nothing cuts it, it is not taken, nor one that is
// not a PDF, nor one not fetched, and the caller converts the file itself:
// nothing of them is kept as the file's PDF, and they are remembered for
// RenditionRetention, not fetched again until then, but for a fetch
// cancelled, which the next question tries again.
func TestServiceTakesRendition(t *testing.T) {
	three := doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"1"}}, doctexttest.PDFPage{Lines: []string{"2"}}, doctexttest.PDFPage{Lines: []string{"3"}})
	conv := &stubConverter{err: ErrMalformed}
	p := &stubPager{}
	s, m, _ := newTestService(t, conv, p, Config{MaxPages: 2})
	fetches := 0
	give := func(data []byte, err error) func(context.Context) ([]byte, error) {
		return func(context.Context) ([]byte, error) { fetches++; return data, err }
	}
	requests := func(result string) float64 { return testutil.ToFloat64(m.OfficeRequests.WithLabelValues(result)) }

	// LibreOffice failed on the file here; Core made its PDF all the same.
	if st := s.Convert(context.Background(), sum1, deck, ToPDF, nil); st.Status != StatusFailed {
		t.Fatalf("the conversion here: %+v", st)
	}
	out, err := s.TakeRendition(context.Background(), sum1, give(three, nil))
	if err != nil || string(out.Data) != "range" || out.Pages != 2 || !out.Capped || !out.Rendition || p.ranges.Load() != 1 {
		t.Fatalf("a PDF of 3 pages past 2: %+v %v, %d cut", out, err, p.ranges.Load())
	}
	if again, err := s.TakeRendition(context.Background(), sum1, give(nil, errors.New("not again"))); err != nil || again != out || fetches != 1 {
		t.Errorf("asked again: %+v %v, fetched %d times", again, err, fetches)
	}
	if st := s.Convert(context.Background(), sum1, deck, ToPDF, nil); st.Status != StatusDone || st.Out != out || conv.calls.Load() != 1 {
		t.Errorf("a conversion after: %+v, LibreOffice run %d times", st, conv.calls.Load())
	}
	if requests("rendition") != 1 || requests("cached") != 2 {
		t.Errorf("rendition %v, cached %v", requests("rendition"), requests("cached"))
	}

	off := NewService(context.Background(), ServiceOptions{Off: "LibreOffice is not installed", Config: Config{MaxPages: 3}})
	if out, err := off.TakeRendition(context.Background(), sum2, give(three, nil)); err != nil || string(out.Data) != string(three) || out.Pages != 3 || out.Capped {
		t.Errorf("without LibreOffice: %+v %v", out, err)
	}

	now := time.Now()
	clock := func() time.Time { return now }
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		what  string
		s     *Service
		ctx   context.Context
		fetch func(context.Context) ([]byte, error)
	}{
		{"past the pages, and nothing to cut it", NewService(context.Background(), ServiceOptions{Config: Config{MaxPages: 2}}), context.Background(), give(three, nil)},
		{"not a PDF", s, context.Background(), give([]byte("<html>expired</html>"), nil)},
		{"not fetched", s, context.Background(), give(nil, errors.New(`Get "https://files.example/rendition.pdf?X-Amz-Signature=5ecre7": EOF`))},
		{"cancelled", s, cancelled, give(nil, context.Canceled)},
	}
	for i, tc := range cases {
		sum := sum3 + string(rune('a'+i))
		var logs strings.Builder
		tc.s.log, tc.s.now = slog.New(slog.NewJSONHandler(&logs, nil)), clock
		before := fetches
		out, err := tc.s.TakeRendition(tc.ctx, sum, tc.fetch)
		if out != nil || !errors.Is(err, ErrRendition) {
			t.Errorf("%s: %+v %v", tc.what, out, err)
		}
		for _, said := range []string{err.Error(), logs.String()} {
			if strings.Contains(said, "files.example") || strings.Contains(said, "Signature") {
				t.Errorf("%s: the URL is said: %s", tc.what, said)
			}
		}
		tc.s.mu.Lock()
		kept := tc.s.cache.get(string(ToPDF)+"\x00"+sum, now)
		tc.s.mu.Unlock()
		if kept != nil {
			t.Errorf("%s: kept %+v", tc.what, kept)
		}
		// Asked again a moment later, and once its time is up.
		out, again := tc.s.TakeRendition(context.Background(), sum, tc.fetch)
		if out != nil || !errors.Is(again, ErrRendition) || again.Error() != err.Error() && tc.what != "cancelled" {
			t.Errorf("%s, asked again: %+v %v, first %v", tc.what, out, again, err)
		}
		if want := map[bool]int{true: 2, false: 1}[tc.what == "cancelled"]; fetches-before != want {
			t.Errorf("%s: fetched %d times, asked twice", tc.what, fetches-before)
		}
		now = now.Add(RenditionRetention + time.Second)
		_, _ = tc.s.TakeRendition(context.Background(), sum, tc.fetch)
		if want := map[bool]int{true: 3, false: 2}[tc.what == "cancelled"]; fetches-before != want {
			t.Errorf("%s: fetched %d times, the last past its time", tc.what, fetches-before)
		}
	}
	if requests("rendition_failed") != 9 {
		t.Errorf("rendition_failed %v", requests("rendition_failed"))
	}
	var none *Service
	if _, err := none.TakeRendition(context.Background(), sum1, give(three, nil)); !errors.Is(err, ErrRendition) {
		t.Errorf("a nil service: %v", err)
	}
}
