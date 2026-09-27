// Package httpserver serves the runtime's own endpoints, on localhost by
// default (docs/deploying.md): /healthz for the deploy script and whoever
// watches the runtime, /metrics for Prometheus, and /status, the owner's
// page's data. None of them holds what anyone wrote, a token or a key:
// /status is ids, states and numbers, every string redacted. /status names
// agents, courses and members, so it answers only this machine, even when
// HTTP_ADDR listens wider (for a Prometheus elsewhere, say).
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/worker"
)

// storeTimeout bounds each of the store's reads a request makes.
const storeTimeout = 3 * time.Second

// Server serves /healthz, /metrics and /status.
type Server struct {
	addr string
	sup  *worker.Supervisor
	st   store.Store
	log  *slog.Logger
	mux  *http.ServeMux
	// Now is the clock /status reckons "today" by.
	Now func() time.Time

	mu  sync.Mutex
	srv *http.Server
	ln  net.Listener
}

// New makes a server of sup, its metrics' registry reg and its store st,
// to listen on addr (127.0.0.1:9090 by default, HTTP_ADDR).
func New(addr string, sup *worker.Supervisor, reg *prometheus.Registry, st store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{addr: addr, sup: sup, st: st, log: log, mux: http.NewServeMux(), Now: time.Now}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	s.mux.HandleFunc("GET /status", localOnly(s.status))
	return s
}

// Handler serves the endpoints.
func (s *Server) Handler() http.Handler { return s.mux }

// Listen binds the server's address; Addr says what it bound.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.srv = &http.Server{Handler: s.mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	s.mu.Unlock()
	return nil
}

// Addr is the address the server listens on, once Listen has bound it.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return s.addr
	}
	return s.ln.Addr().String()
}

// Serve serves until ctx is done, then shuts down, giving requests in
// progress a few seconds. It listens first if Listen was not called.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	bound := s.ln != nil
	s.mu.Unlock()
	if !bound {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	srv, ln := s.srv, s.ln
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(shut)
	if errors.Is(err, context.DeadlineExceeded) {
		// What is left is a request that would not end, or a connection
		// that never sent one (Shutdown waits five seconds for those): the
		// process is stopping, and they go.
		err = srv.Close()
	}
	if serveErr := <-done; !errors.Is(serveErr, http.ErrServerClosed) && err == nil {
		err = serveErr
	}
	return err
}

// shutdownGrace is how long requests in progress are given when the server
// stops.
const shutdownGrace = 2 * time.Second

// health is /healthz's answer. The deploy script waits for the new
// version and commit here.
type health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Reason  string `json:"reason,omitempty"`
}

// healthz answers 200 "ok" while the supervisor runs and its store can be
// reached, and 503 "unavailable" otherwise.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	h := health{Status: "ok", Version: version.Version, Commit: version.Commit}
	code := http.StatusOK
	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	switch _, err := s.st.AgentStates(ctx); {
	case err != nil:
		h.Status, h.Reason, code = "unavailable", "the store cannot be reached", http.StatusServiceUnavailable
		s.log.Warn("healthz: the store cannot be reached", "err", err)
	case !s.sup.Running():
		h.Status, h.Reason, code = "unavailable", "the worker is not running", http.StatusServiceUnavailable
	}
	writeJSON(w, code, h)
}

// localOnly serves h to requests from a loopback address alone, and
// answers 403 to any other.
func localOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			writeJSON(w, http.StatusForbidden, map[string]string{"status": "forbidden", "reason": "/status answers this machine alone"})
			return
		}
		h(w, r)
	}
}

// Status is /status's answer.
type Status struct {
	Worker  string        `json:"worker"`
	Version string        `json:"version"`
	Commit  string        `json:"commit"`
	At      time.Time     `json:"at"`
	Agents  []AgentStatus `json:"agents"`
}

// AgentStatus is one agent, as the store records it and, for one this
// worker runs, as the worker has it.
type AgentStatus struct {
	AgentID string `json:"agent_id"`
	// State, Detail, Worker and UpdatedAt are the store's: what the
	// worker that runs the agent last recorded.
	State     string     `json:"state,omitempty"`
	Detail    string     `json:"detail,omitempty"`
	Worker    string     `json:"worker,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Configured is whether this worker's configuration has the agent;
	// RunHere, whether this worker runs it now.
	Configured bool `json:"configured"`
	RunHere    bool `json:"run_here"`
	Paused     bool `json:"paused,omitempty"`
	// CatalogueHash is the hash of the Core catalogue it runs with.
	CatalogueHash string     `json:"catalogue_hash,omitempty"`
	SlowUntil     *time.Time `json:"slow_until,omitempty"`
	Answering     int        `json:"answering"`
	// ProposalsWaiting are answers waiting for a person's approval.
	ProposalsWaiting int          `json:"proposals_waiting"`
	Today            Spend        `json:"today"`
	Seats            []SeatStatus `json:"seats,omitempty"`
}

// Spend is what an agent used since the start of the UTC day.
type Spend struct {
	Answers  int     `json:"answers"`
	CostPUSD int64   `json:"cost_pusd"`
	CostUSD  float64 `json:"cost_usd"`
}

// SeatStatus is one seat of an agent this worker runs.
type SeatStatus struct {
	worker.SeatStatus
	ProposalsWaiting int `json:"proposals_waiting"`
}

// status answers the agents' states, seats, proposals waiting, the answers
// and spend of the day, and the catalogue's hash.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*storeTimeout)
	defer cancel()
	now := s.Now()
	out := Status{Worker: redact.String(s.sup.WorkerID()), Version: version.Version, Commit: version.Commit, At: now.UTC()}
	byID := map[string]*AgentStatus{}
	get := func(id string) *AgentStatus {
		if a := byID[id]; a != nil {
			return a
		}
		a := &AgentStatus{AgentID: id}
		byID[id] = a
		return a
	}
	states, err := s.st.AgentStates(ctx)
	if err != nil {
		s.log.Warn("status: the agents' states could not be read", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, health{Status: "unavailable", Version: version.Version, Commit: version.Commit,
			Reason: "the store cannot be reached"})
		return
	}
	for _, st := range states {
		a := get(st.AgentID)
		a.State, a.Detail, a.Worker = st.State, st.Detail, st.Worker
		t := st.UpdatedAt
		a.UpdatedAt = &t
	}
	for _, ws := range s.sup.Status() {
		a := get(ws.AgentID)
		a.Configured, a.RunHere, a.Paused = true, ws.Running, ws.Paused
		a.CatalogueHash, a.SlowUntil, a.Answering = ws.CatalogueHash, ws.SlowUntil, ws.Answering
		for _, seat := range ws.Seats {
			a.Seats = append(a.Seats, SeatStatus{SeatStatus: seat})
		}
	}
	since := startOfDay(now)
	for id, a := range byID {
		if sp, err := s.st.Spend(ctx, store.SpendScope{AgentID: id}, since); err == nil {
			a.Today = Spend{Answers: sp.Answers, CostPUSD: sp.CostPUSD, CostUSD: pricing.USD(sp.CostPUSD)}
		}
		a.ProposalsWaiting = s.proposals(ctx, a)
		out.Agents = append(out.Agents, redactAgent(*a))
	}
	slices.SortFunc(out.Agents, func(x, y AgentStatus) int { return strings.Compare(x.AgentID, y.AgentID) })
	writeJSON(w, http.StatusOK, out)
}

// proposals counts the agent's answers waiting for approval: in each seat
// this worker runs, or, for another's agent, in each seat the store knows.
func (s *Server) proposals(ctx context.Context, a *AgentStatus) int {
	members := map[string]*SeatStatus{}
	for i := range a.Seats {
		members[a.Seats[i].MemberID] = &a.Seats[i]
	}
	if len(members) == 0 {
		seats, err := s.st.KnownSeats(ctx, a.AgentID)
		if err != nil {
			return 0
		}
		for _, seat := range seats {
			if seat.GoneAt == nil {
				members[seat.MemberID] = nil
			}
		}
	}
	total := 0
	for member, seat := range members {
		atts, err := s.st.Unsettled(ctx, a.AgentID, member)
		if err != nil {
			continue
		}
		n := 0
		for _, at := range atts {
			if at.State == store.AttemptProposed {
				n++
			}
		}
		if seat != nil {
			seat.ProposalsWaiting = n
		}
		total += n
	}
	return total
}

// redactAgent passes every string of a through redact.String.
func redactAgent(a AgentStatus) AgentStatus {
	r := redact.String
	a.AgentID, a.State, a.Detail, a.Worker, a.CatalogueHash = r(a.AgentID), r(a.State), r(a.Detail), r(a.Worker), r(a.CatalogueHash)
	for i := range a.Seats {
		s := &a.Seats[i]
		s.MemberID, s.CourseID, s.CourseCode, s.Level, s.HeldWhy = r(s.MemberID), r(s.CourseID), r(s.CourseCode), r(s.Level), r(s.HeldWhy)
		tools := make([]string, len(s.Tools))
		for j, t := range s.Tools {
			tools[j] = r(t)
		}
		s.Tools = tools
	}
	return a
}

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
