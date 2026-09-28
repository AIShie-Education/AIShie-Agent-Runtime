package ocr

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
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
// left.
func TestServiceWaits(t *testing.T) {
	quick := &fakeRecognizer{fn: func(context.Context, []byte, func(int, int)) (*Result, error) {
		return &Result{Text: "看板", Pages: 1, Of: 1}, nil
	}}
	s, _ := newTestService(t, t.Context(), quick, Config{Wait: 5 * time.Second}, nil)
	var fetched atomic.Int32
	if st := s.Text(t.Context(), sumOf("b"), Image, 0, bytesOf("img", &fetched)); st.Status != StatusDone || st.Text.Text != "看板" {
		t.Errorf("a quick file: %+v", st)
	}

	rec, release := gate()
	defer close(release)
	s, _ = newTestService(t, t.Context(), rec, Config{Wait: 5 * time.Second}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if st := s.Text(ctx, sumOf("c"), PDF, 3, bytesOf("slow", &fetched)); st.Status != StatusPending {
		t.Errorf("a slow file: %+v", st)
	}
	if took := time.Since(start); took > 150*time.Millisecond {
		t.Errorf("waited %s of a question that had 200 ms", took)
	}
	// Asked again while it is recognized here, the question waits as the
	// first did, not answered at once.
	ctx, cancel = context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	if st := s.Text(ctx, sumOf("c"), PDF, 3, bytesOf("slow", &fetched)); st.Status != StatusPending || st.Done != 1 {
		t.Errorf("the slow file asked again: %+v", st)
	}
	if took := time.Since(start); took < 80*time.Millisecond || took > 150*time.Millisecond {
		t.Errorf("asked again, waited %s of a question that had 200 ms, want about half", took)
	}
	if fetched.Load() != 2 {
		t.Errorf("fetched %d times, want once a file", fetched.Load())
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
