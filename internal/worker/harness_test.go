package worker

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.yaml.in/yaml/v3"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
)

// The worker's tests run it whole: a Supervisor over the fake Core (served
// on loopback), with scripted models, on memstore (pgstore where two
// workers must share leases), at intervals of milliseconds.

// modelKey is the provider key every test agent's key_ref resolves to: no
// log line may hold it.
const modelKey = "sk-test-worker-0123456789abcdefghij"

// world is one course in the fake Core, as the handout's examples have it:
// Sato (an instructor, who owns the course's tutor), Mori (a second
// instructor, who decides the tutor's proposals: nobody decides their own
// agent's), and two students, Yuki and Ken.
type world struct {
	t   *testing.T
	fc  *fakecore.Core
	srv *httptest.Server
	co  fakecore.Course

	sato, mori         fakecore.Actor
	satoSeat, moriSeat fakecore.Member
	students           []fakecore.Actor
	studentSeats       []fakecore.Member

	dir string
	env sync.Map // the environment secrets are read from
	// catalogueFetches counts GET /v1/tools.
	catalogueFetches atomic.Int32

	logs *logBuffer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return newWorldWith(t, fakecore.Options{})
}

func newWorldWith(t *testing.T, o fakecore.Options) *world {
	t.Helper()
	fc := fakecore.New(o)
	w := &world{t: t, fc: fc, dir: t.TempDir(), logs: &logBuffer{}}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tools" {
			w.catalogueFetches.Add(1)
		}
		fc.Handler().ServeHTTP(rw, r)
	}))
	t.Cleanup(w.srv.Close)
	w.co = fc.AddCourse("CS101")
	w.sato = fc.AddPerson("Sato")
	w.satoSeat = w.must(fc.Seat(w.sato.ID, w.co.ID, fakecore.SeatOptions{Preset: "instructor"}))
	w.mori = fc.AddPerson("Mori")
	w.moriSeat = w.must(fc.Seat(w.mori.ID, w.co.ID, fakecore.SeatOptions{Preset: "instructor"}))
	for _, name := range []string{"Yuki", "Ken"} {
		p := fc.AddPerson(name)
		w.students = append(w.students, p)
		w.studentSeats = append(w.studentSeats, w.must(fc.Seat(p.ID, w.co.ID, fakecore.SeatOptions{Preset: "student"})))
	}
	w.env.Store("MODEL_KEY", modelKey)
	return w
}

func (w *world) must(m fakecore.Member, err error) fakecore.Member {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
	return m
}

func (w *world) ok(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) getenv(k string) string {
	v, _ := w.env.Load(k)
	s, _ := v.(string)
	return s
}

// agent is an agent of the world's, seated.
type agent struct {
	id    string
	actor fakecore.Actor
	seat  fakecore.Member
	// owner is the person who owns it in Core.
	owner fakecore.Actor
}

// ownAgent seats a student's own agent (preset delegate) under student i.
func (w *world) ownAgent(id string, i int) agent {
	w.t.Helper()
	a, err := w.fc.AddAgent(w.students[i].Name+"'s helper", w.students[i].ID)
	w.ok(err)
	m := w.must(w.fc.Seat(a.ID, w.co.ID, fakecore.SeatOptions{Preset: "delegate", Principal: w.studentSeats[i].ID}))
	w.env.Store(tokenVar(id), a.Token)
	return agent{id: id, actor: a, seat: m, owner: w.students[i]}
}

// tutor seats Sato's course tutor (preset course_tutor), answering every
// student.
func (w *world) tutor(id string) agent {
	w.t.Helper()
	a, err := w.fc.AddAgent("CS101 Tutor", w.sato.ID)
	w.ok(err)
	m := w.must(w.fc.Seat(a.ID, w.co.ID, fakecore.SeatOptions{Preset: "course_tutor", Principal: w.satoSeat.ID}))
	w.env.Store(tokenVar(id), a.Token)
	return agent{id: id, actor: a, seat: m, owner: w.sato}
}

func tokenVar(id string) string {
	return "TOKEN_" + strings.ToUpper(strings.NewReplacer("-", "_").Replace(id))
}

// ask has student i ask agent ag a question, and returns the conversation
// and the message.
func (w *world) ask(i int, ag agent, body string) (string, string) {
	w.t.Helper()
	cv, m, err := w.fc.Ask(w.co.ID, w.studentSeats[i].ID, ag.seat.ID, body)
	w.ok(err)
	return cv.ID, m.ID
}

// fastPolling are polling settings for tests: milliseconds, and an
// allowance so large that the rate share's floor never binds.
func fastPolling() map[string]any {
	return map[string]any{
		"inbox_hot_s": 0.01, "hot_window_s": 1, "inbox_idle_s": 0.02, "inbox_max_s": 0.05,
		"events_s": 0.03, "memberships_s": 0.3, "jitter": 0.25, "max_rate_share": 0.3,
		"assumed_core_rate_per_min": 600000, "assumed_core_burst": 10000,
	}
}

// agentDoc is an agent's YAML document for a test: model names the
// scripted model, over is merged into the agent's settings, courses are
// its per-course settings.
func (w *world) agentDoc(id, model string, over map[string]any, courses map[string]any) map[string]any {
	a := map[string]any{
		"id": id, "display_name": "Agent " + id,
		"core":    map[string]any{"base_url": w.srv.URL, "token_ref": "env://" + tokenVar(id)},
		"model":   map[string]any{"adapter": "openai_chat", "model": model, "key_ref": "env://MODEL_KEY", "params": map[string]any{"max_output_tokens": 500}},
		"polling": fastPolling(),
		"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 10}},
	}
	a = mergeMaps(a, over)
	doc := map[string]any{"agent": a}
	if courses != nil {
		doc["courses"] = courses
	}
	return doc
}

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

// config writes docs (agents, and a runtime document when rt is set) and
// loads them as the binary does.
func (w *world) config(rt map[string]any, docs ...map[string]any) *config.Config {
	w.t.Helper()
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	if rt != nil {
		w.ok(enc.Encode(map[string]any{"runtime": rt}))
	}
	for _, d := range docs {
		w.ok(enc.Encode(d))
	}
	w.ok(enc.Close())
	path := filepath.Join(w.dir, fmt.Sprintf("agents-%d.yaml", time.Now().UnixNano()))
	w.ok(os.WriteFile(path, b.Bytes(), 0o600))
	cfg, err := config.Load(path)
	if err != nil {
		w.t.Fatalf("config: %v\n%s", err, b.String())
	}
	return cfg
}

// models are scripted models by name, as NewAdapter builds them.
type models map[string]*scripted.Adapter

// worker is a running Supervisor and what it runs on.
type worker struct {
	w      *world
	sup    *Supervisor
	st     *recordingStore
	reg    *prometheus.Registry
	m      *metrics.Metrics
	cancel context.CancelFunc
	done   chan struct{}
}

// workerOpts are a test's changes to a worker's options.
type workerOpts struct {
	id     string
	store  store.Store
	prices *pricing.Table
	edit   func(*Options)
	log    *slog.Logger
}

// start runs a supervisor of cfg with models, until the test ends.
func (w *world) start(cfg *config.Config, ms models, wo workerOpts) *worker {
	w.t.Helper()
	if wo.id == "" {
		wo.id = "w1"
	}
	base := wo.store
	if base == nil {
		base = memstore.New()
	}
	st := &recordingStore{Store: base}
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	log := wo.log
	if log == nil {
		log = slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	o := Options{
		Config: cfg, Store: st, Metrics: m, Log: log, WorkerID: wo.id,
		Secrets:    secrets.Resolver{Getenv: w.getenv},
		Prices:     wo.prices,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		NewAdapter: func(c llm.Config) (llm.Adapter, error) {
			if a, ok := ms[c.Model]; ok {
				return a, nil
			}
			return nil, fmt.Errorf("no scripted model %q", c.Model)
		},
		CoreRetry: core.RetryOptions{Base: time.Millisecond, Max: 5 * time.Millisecond},
		Timing: Timing{
			LeaseEvery: 20 * time.Millisecond, LeaseTTL: time.Second, Housekeeping: 50 * time.Millisecond,
			Restart: 10 * time.Millisecond, RestartMax: 50 * time.Millisecond,
			ModelBackoff: time.Millisecond, ModelBackoffMax: 5 * time.Millisecond,
			HoldBack: 100 * time.Millisecond, HoldBackMax: 400 * time.Millisecond, RetryLater: 30 * time.Millisecond,
		},
	}
	if wo.edit != nil {
		wo.edit(&o)
	}
	sup, err := NewSupervisor(o)
	w.ok(err)
	ctx, cancel := context.WithCancel(context.Background())
	wk := &worker{w: w, sup: sup, st: st, reg: reg, m: m, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(wk.done)
		if err := sup.Run(ctx); err != nil {
			w.t.Errorf("Run: %v", err)
		}
	}()
	w.t.Cleanup(wk.stop)
	return wk
}

// stop stops the worker and waits for Run to return.
func (wk *worker) stop() {
	wk.cancel()
	select {
	case <-wk.done:
	case <-time.After(20 * time.Second):
		wk.w.t.Error("the supervisor did not stop within 20 s")
	}
}

// eventually waits for cond, failing the test after a deadline.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// answers are the agent's messages in conv.
func (w *world) answers(conv string) []fakecore.MessageRecord { return w.fc.Answers(conv) }

// waitAnswers waits for n answers in conv.
func (w *world) waitAnswers(conv string, n int) []fakecore.MessageRecord {
	w.t.Helper()
	eventually(w.t, fmt.Sprintf("%d answers in %s", n, conv), func() bool { return len(w.answers(conv)) >= n })
	return w.answers(conv)
}

// calls are the fake's calls made by actor, of tool when tool is not "".
func (w *world) calls(actor, tool string) []fakecore.Call {
	var out []fakecore.Call
	for _, c := range w.fc.Calls() {
		if c.ActorID == actor && (tool == "" || c.Tool == tool) {
			out = append(out, c)
		}
	}
	return out
}

// state is the agent's state in the store.
func (wk *worker) state(id string) store.AgentState {
	sts, err := wk.st.AgentStates(context.Background())
	if err != nil {
		wk.w.t.Fatal(err)
	}
	for _, s := range sts {
		if s.AgentID == id {
			return s
		}
	}
	return store.AgentState{}
}

func (wk *worker) waitState(id, state string) store.AgentState {
	wk.w.t.Helper()
	eventually(wk.w.t, "agent "+id+" "+state, func() bool { return wk.state(id).State == state })
	return wk.state(id)
}

// recordingStore is a store that also keeps the ledger's rows, for tests
// to look at.
type recordingStore struct {
	store.Store
	mu      sync.Mutex
	calls   []store.LLMCall
	records []store.AnswerRecord
}

func (s *recordingStore) RecordLLMCall(ctx context.Context, c store.LLMCall) error {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	return s.Store.RecordLLMCall(ctx, c)
}

func (s *recordingStore) RecordAnswer(ctx context.Context, a store.AnswerRecord) error {
	s.mu.Lock()
	s.records = append(s.records, a)
	s.mu.Unlock()
	return s.Store.RecordAnswer(ctx, a)
}

func (s *recordingStore) ledger() ([]store.LLMCall, []store.AnswerRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls), slices.Clone(s.records)
}

// outcomes are the answers' outcomes recorded for conv, in order.
func (s *recordingStore) outcomes(conv string) []string {
	_, recs := s.ledger()
	var out []string
	for _, r := range recs {
		if r.ConversationID == conv {
			out = append(out, r.Outcome)
		}
	}
	return out
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

// lastRequest is the scripted model's last request.
func lastRequest(t *testing.T, m *scripted.Adapter) *llm.Request {
	t.Helper()
	reqs := m.Requests()
	if len(reqs) == 0 {
		t.Fatal("the model was never called")
	}
	return reqs[len(reqs)-1]
}

// requestText is everything a request says, system prompt and messages.
func requestText(r *llm.Request) string {
	var b strings.Builder
	b.WriteString(r.System)
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			b.WriteString("\n")
			b.WriteString(p.Text)
			b.WriteString(p.Content)
		}
	}
	return b.String()
}

// counter is the value of the metric name whose labels include labels,
// summed over the series that match: a counter's, a gauge's, or a
// histogram's count.
func counter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
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

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
