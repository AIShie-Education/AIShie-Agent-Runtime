package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/worker"
)

// fixture is a supervisor running a student's own agent on the fake Core,
// its store and its registry.
type fixture struct {
	sup     *worker.Supervisor
	st      store.Store
	reg     *prometheus.Registry
	token   string
	seat    string
	stopped chan struct{}
	cancel  context.CancelFunc
}

func newFixture(t *testing.T, st store.Store) *fixture {
	t.Helper()
	fc := fakecore.New(fakecore.Options{})
	srv := httptest.NewServer(fc.Handler())
	t.Cleanup(srv.Close)
	co := fc.AddCourse("CS101")
	yuki := fc.AddPerson("Yuki")
	seat, err := fc.Seat(yuki.ID, co.ID, fakecore.SeatOptions{Preset: "student"})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := fc.AddAgent("Yuki's helper", yuki.ID)
	if err != nil {
		t.Fatal(err)
	}
	own, err := fc.Seat(ag.ID, co.ID, fakecore.SeatOptions{Preset: "delegate", Principal: seat.ID})
	if err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf(`agent:
  id: yuki-helper
  display_name: "Yuki's helper"
  core: {base_url: %q, token_ref: env://TOKEN}
  model: {adapter: openai_chat, model: m1, key_ref: env://KEY}
  polling: {inbox_hot_s: 0.01, inbox_idle_s: 0.02, inbox_max_s: 0.05, events_s: 0.03, memberships_s: 0.3, assumed_core_rate_per_min: 600000}
`, srv.URL)
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TOKEN": ag.Token, "KEY": "sk-test-0123456789abcdef"}
	reg := prometheus.NewRegistry()
	sup, err := worker.NewSupervisor(worker.Options{
		Config: cfg, Store: st, Metrics: metrics.New(reg), WorkerID: "w1",
		Secrets:    secrets.Resolver{Getenv: func(k string) string { return env[k] }},
		NewAdapter: func(llm.Config) (llm.Adapter, error) { return scripted.New(), nil },
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{sup: sup, st: st, reg: reg, token: ag.Token, seat: own.ID, stopped: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() { defer close(f.stopped); _ = sup.Run(ctx) }()
	t.Cleanup(f.stop)
	return f
}

func (f *fixture) stop() {
	f.cancel()
	<-f.stopped
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHealthz(t *testing.T) {
	f := newFixture(t, memstore.New())
	s := New("127.0.0.1:0", f.sup, f.reg, f.st, nil)
	waitFor(t, "the supervisor running", f.sup.Running)
	code, body := get(t, s.Handler(), "/healthz")
	var h health
	if err := json.Unmarshal([]byte(body), &h); err != nil || code != http.StatusOK || h.Status != "ok" ||
		h.Version != version.Version || h.Commit != version.Commit {
		t.Errorf("healthz: %d %s", code, body)
	}
	f.stop()
	if code, body := get(t, s.Handler(), "/healthz"); code != http.StatusServiceUnavailable || !strings.Contains(body, `"unavailable"`) {
		t.Errorf("healthz with the worker stopped: %d %s", code, body)
	}
}

// brokenStore cannot be reached.
type brokenStore struct{ store.Store }

func (brokenStore) AgentStates(context.Context) ([]store.AgentState, error) {
	return nil, errors.New("connection refused")
}

func TestHealthzWithoutTheStore(t *testing.T) {
	f := newFixture(t, memstore.New())
	waitFor(t, "the supervisor running", f.sup.Running)
	s := New("127.0.0.1:0", f.sup, f.reg, brokenStore{f.st}, nil)
	code, body := get(t, s.Handler(), "/healthz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"status": "unavailable"`) || !strings.Contains(body, version.Version) {
		t.Errorf("healthz: %d %s", code, body)
	}
}

func TestMetrics(t *testing.T) {
	f := newFixture(t, memstore.New())
	s := New("127.0.0.1:0", f.sup, f.reg, f.st, nil)
	waitFor(t, "an inbox poll counted", func() bool {
		_, body := get(t, s.Handler(), "/metrics")
		return strings.Contains(body, "inbox_polls_total{") && strings.Contains(body, `core_calls_total{error_code="",status="executed",tool="conversation_inbox"}`)
	})
	_, body := get(t, s.Handler(), "/metrics")
	for _, want := range []string{"agents{state=\"running\"} 1", "presence_gap_seconds{agent=\"yuki-helper\"}"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

func TestStatus(t *testing.T) {
	f := newFixture(t, memstore.New())
	s := New("127.0.0.1:0", f.sup, f.reg, f.st, nil)
	var st Status
	waitFor(t, "the agent's seat in /status", func() bool {
		code, body := get(t, s.Handler(), "/status")
		if code != http.StatusOK || json.Unmarshal([]byte(body), &st) != nil {
			return false
		}
		return len(st.Agents) == 1 && len(st.Agents[0].Seats) == 1 && st.Agents[0].Seats[0].LastPoll != nil
	})
	a := st.Agents[0]
	if a.AgentID != "yuki-helper" || a.State != store.AgentRunning || !a.RunHere || !a.Configured || a.Worker != "w1" ||
		a.CatalogueHash != worker.SnapshotCatalogueHash || a.Seats[0].MemberID != f.seat || a.Seats[0].CourseCode != "CS101" ||
		!a.Seats[0].Answering {
		t.Errorf("status %+v", a)
	}
	if st.Worker != "w1" || st.Version != version.Version {
		t.Errorf("status %+v", st)
	}
	_, body := get(t, s.Handler(), "/status")
	if strings.Contains(body, f.token) || strings.Contains(body, "ais_") || strings.Contains(body, "sk-test") {
		t.Error("/status holds a secret")
	}
}

func TestServe(t *testing.T) {
	f := newFixture(t, memstore.New())
	s := New("127.0.0.1:0", f.sup, f.reg, f.st, nil)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	waitFor(t, "the supervisor running", f.sup.Running)
	resp, err := http.Get("http://" + s.Addr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
	if resp, err := http.Post("http://"+s.Addr()+"/healthz", "text/plain", nil); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST /healthz: %d", resp.StatusCode)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
}
