package e2e

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.yaml.in/yaml/v3"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/netguard"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/worker"
)

// The runtime runs in-process, built as the binary's run command builds it
// but from parts a test can watch: its configuration YAML in a file,
// loaded by config.Load; its log in a buffer, through redact as the binary
// logs; its metrics in a registry of its own; its state in a memstore; and
// its model a fakellm server behind the real openai_chat adapter.

// newModel starts a scripted OpenAI Chat server for t, answering with r,
// and closes it when t ends. t's context is done before the server is
// closed, so a responder that waits on it lets the server close.
func newModel(t *testing.T, r fakellm.Responder) *fakellm.Server {
	t.Helper()
	s := fakellm.New(r).Start()
	t.Cleanup(s.Close)
	return s
}

// polling are the tests' polling settings: a question is noticed within a
// second or so, and each agent stays well inside Core's default limit of
// 600 calls a minute (§7.3: inbox at most twice a second, events and seats
// every 2 s, 180 calls a minute in all). Where Core offers wait_s, each
// inbox call waits a second for a question, the least Core takes, not the
// default 25: what is no news to Core (a seat's level changed, say) is seen
// within a second, and settle's inbox polls take a second each.
// longPollPickup waits the default.
func polling() map[string]any {
	return map[string]any{
		"inbox_hot_s": 0.5, "hot_window_s": 10, "inbox_idle_s": 0.5, "inbox_max_s": 1,
		"events_s": 2, "memberships_s": 2, "jitter": 0.25, "long_poll_wait_s": 1,
	}
}

// agentConf is one agent of a test's configuration: its id, the seat whose
// token it connects with, and settings merged over the tests' own.
type agentConf struct {
	id   string
	seat agentSeat
	over map[string]any
}

// writeConfig writes the agents' configuration, one YAML document each, as
// an operator would, and returns its path. Every agent reaches Core at the
// world's base URL with its token from the environment (env://), and its
// model at m, as an OpenAI-compatible server, with the world's key.
func (w *world) writeConfig(t testing.TB, m *fakellm.Server, agents ...agentConf) string {
	t.Helper()
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	for _, a := range agents {
		doc := map[string]any{
			"id": a.id, "display_name": "Agent " + a.id,
			"core": map[string]any{"base_url": w.api.base, "token_ref": "env://" + a.seat.tokenVar},
			"model": map[string]any{
				"adapter": "openai_chat", "provider": "openai_compatible", "model": "e2e-model", "base_url": m.URL(),
				"key_ref": "env://" + w.modelKeyVar, "params": map[string]any{"max_output_tokens": 500},
			},
			"polling": polling(),
			"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 60}},
		}
		if err := enc.Encode(map[string]any{"agent": mergeMaps(doc, a.over)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mergeMaps is base with over merged in, maps key by key.
func mergeMaps(base, over map[string]any) map[string]any {
	out := maps.Clone(base)
	for k, v := range over {
		if om, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = mergeMaps(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// runtimeConf is how a test runs the runtime.
type runtimeConf struct {
	// workerID names the worker in leases; "w1" when empty.
	workerID string
	agents   []agentConf
	// tag, when set, goes to the model with every call as the header
	// X-E2E-Worker, so that a test running two workers can tell their
	// model calls apart.
	tag string
	// office, when set, converts the Office files the agents read (the
	// worker's, as run makes it); none, they are given as their text.
	office *office.Service
}

// tagHeader is the header runtimeConf.tag is sent in.
const tagHeader = "X-E2E-Worker"

// instance is a Supervisor running for a test, and what the test watches it
// through.
type instance struct {
	t   *testing.T
	sup *worker.Supervisor
	reg *prometheus.Registry
	st  store.Store
	// log is what the runtime logged, through redact; raw is the same
	// before redaction, only ever searched for secrets, never shown.
	log, raw *logBuffer
}

// startRuntime runs the runtime with rc's agents, their model m, until t
// ends.
func (w *world) startRuntime(t *testing.T, m *fakellm.Server, rc runtimeConf) *instance {
	t.Helper()
	if rc.workerID == "" {
		rc.workerID = "w1"
	}
	cfg, err := config.Load(w.writeConfig(t, m, rc.agents...))
	if err != nil {
		t.Fatalf("the configuration: %v", err)
	}
	rt := &instance{t: t, reg: prometheus.NewRegistry(), st: memstore.New(), log: &logBuffer{}, raw: &logBuffer{}}
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	logger := slog.New(teeHandler{
		redact.NewHandler(slog.NewJSONHandler(rt.log, opts), nil),
		slog.NewJSONHandler(rt.raw, opts),
	})
	name := t.Name() + " " + rc.workerID
	w.addLog(name, rt.log.String)
	w.addLog(name+" (before redaction)", rt.raw.String)

	tr := http.DefaultTransport.(*http.Transport).Clone()
	var rtp http.RoundTripper = tr
	if rc.tag != "" {
		u, err := url.Parse(m.URL())
		if err != nil {
			t.Fatal(err)
		}
		rtp = tagged{next: tr, host: u.Host, tag: rc.tag}
	}
	var conv toolset.Office
	if rc.office != nil {
		conv = rc.office
	}
	rt.sup, err = worker.NewSupervisor(worker.Options{
		Config: cfg, Store: rt.st, Metrics: metrics.New(rt.reg), Log: logger, WorkerID: rc.workerID, Office: conv,
		HTTPClient: &http.Client{Transport: rtp},
		// No hosted agent runs here; the hosted-model client follows no
		// redirect, as in production.
		HostedHTTPClient: netguard.NoRedirects(&http.Client{Transport: rtp}),
		CoreRetry:        core.RetryOptions{Base: 50 * time.Millisecond, Max: time.Second},
		Timing: worker.Timing{
			LeaseEvery: time.Second, LeaseTTL: 5 * time.Second, Restart: 100 * time.Millisecond, RestartMax: time.Second,
			ModelBackoff: 50 * time.Millisecond, ModelBackoffMax: 500 * time.Millisecond,
			HoldBack: time.Second, HoldBackMax: 5 * time.Second, RetryLater: time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := rt.sup.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the supervisor did not stop within 30 s")
		}
		tr.CloseIdleConnections()
		if t.Failed() {
			t.Logf("the log of worker %s (redacted):\n%s", rc.workerID, tail(rt.log.String(), 200))
		}
	})
	return rt
}

// tagged sends tag with every request to the model's host.
type tagged struct {
	next      http.RoundTripper
	host, tag string
}

func (tg tagged) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == tg.host {
		r = r.Clone(r.Context())
		r.Header.Set(tagHeader, tg.tag)
	}
	return tg.next.RoundTrip(r)
}

// metric is the value of the metric name, summed over the series whose
// labels include labels: a counter's, a gauge's, or a histogram's count.
func (rt *instance) metric(name string, labels map[string]string) float64 {
	rt.t.Helper()
	mfs, err := rt.reg.Gather()
	if err != nil {
		rt.t.Fatal(err)
	}
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	series:
		for _, m := range mf.GetMetric() {
			have := map[string]string{}
			for _, lp := range m.GetLabel() {
				have[lp.GetName()] = lp.GetValue()
			}
			for k, v := range labels {
				if have[k] != v {
					continue series
				}
			}
			switch {
			case m.Counter != nil:
				sum += m.GetCounter().GetValue()
			case m.Gauge != nil:
				sum += m.GetGauge().GetValue()
			case m.Histogram != nil:
				sum += float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return sum
}

// inboxPolls is how often the agent has asked conversation_inbox.
func (rt *instance) inboxPolls(agent string) float64 {
	return rt.metric("inbox_polls_total", map[string]string{"agent": agent})
}

// coreCalls is how often the runtime has called tool, however it came
// back.
func (rt *instance) coreCalls(tool string) float64 {
	return rt.metric("core_calls_total", map[string]string{"tool": tool})
}

// waitPolling waits until the agent's seat has polled its inbox, or has a
// call waiting for news: the runtime has connected, read its seats and
// started.
func (rt *instance) waitPolling(agent string) {
	rt.t.Helper()
	eventually(rt.t, 60*time.Second, "the first inbox poll of "+agent, func() bool {
		return rt.inboxPolls(agent) > 0 || rt.longPolls(agent) > 0
	})
}

// longPolls is how many of the agent's calls wait for news now.
func (rt *instance) longPolls(agent string) float64 {
	return rt.metric("long_polls", map[string]string{"agent": agent})
}

// histogramSum is the sum of the histogram name's samples, over the series
// whose labels include labels.
func (rt *instance) histogramSum(name string, labels map[string]string) float64 {
	rt.t.Helper()
	mfs, err := rt.reg.Gather()
	if err != nil {
		rt.t.Fatal(err)
	}
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	series:
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if v, ok := labels[lp.GetName()]; ok && v != lp.GetValue() {
					continue series
				}
			}
			if m.Histogram != nil {
				sum += m.GetHistogram().GetSampleSum()
			}
		}
	}
	return sum
}

// seat is the agent's seat in course as the supervisor shows it, and
// whether it runs.
func (rt *instance) seat(agent, course string) (worker.SeatStatus, bool) {
	for _, a := range rt.sup.Status() {
		if a.AgentID != agent {
			continue
		}
		for _, s := range a.Seats {
			if s.CourseID == course {
				return s, true
			}
		}
	}
	return worker.SeatStatus{}, false
}

// attempt is the attempt the runtime stored under key, nil when there is
// none.
func (rt *instance) attempt(agent, key string) *store.Attempt {
	rt.t.Helper()
	at, err := rt.st.Attempt(context.Background(), agent, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		rt.t.Fatal(err)
	}
	return at
}

// settled is the attempt under key once the runtime has finished recording
// it: Core may show the answer a moment before the runtime's store says
// posted, since the runtime writes its own record after Core answers. It
// waits for the attempt to leave sending, up to answerWait, and returns it as
// it then stands (nil if there is none).
func (rt *instance) settled(agent, key string) *store.Attempt {
	rt.t.Helper()
	var at *store.Attempt
	within(answerWait, func() bool {
		at = rt.attempt(agent, key)
		return at != nil && at.State != store.AttemptSending
	})
	return at
}

// teeHandler writes every record to both its handlers: the log as the
// binary writes it, and the same before redaction.
type teeHandler struct{ a, b slog.Handler }

func (h teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.a.Enabled(ctx, l) || h.b.Enabled(ctx, l)
}

func (h teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, x := range []slog.Handler{h.a, h.b} {
		if x.Enabled(ctx, r.Level) {
			errs = append(errs, x.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (h teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return teeHandler{h.a.WithAttrs(as), h.b.WithAttrs(as)}
}

func (h teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{h.a.WithGroup(name), h.b.WithGroup(name)}
}

// logBuffer collects log lines from many goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// tail is s's last n lines.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
