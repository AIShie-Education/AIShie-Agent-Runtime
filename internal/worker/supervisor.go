// Package worker retains the small configuration and actor-lifecycle surface
// shared by the management API. Conversational polling and model loops have
// moved to internal/chat and the TypeScript runner; no Core inbox is polled.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Options configure the management supervisor.
type Options struct {
	Config   *config.Config
	Env      config.Env
	Store    store.Store
	Log      *slog.Logger
	Prices   *pricing.Table
	WorkerID string
}

// Supervisor tracks configuration and actor ownership only. It deliberately
// starts no conversational workers.
type Supervisor struct {
	o       Options
	running atomic.Bool
	kick    chan struct{}

	mu      sync.Mutex
	cfg     *config.Config
	pending *config.Config
	prices  *pricing.Table
	actors  map[string]actorRef
}

type actorRef struct {
	agentID string
	hosted  bool
}

// NewSupervisor validates and creates a supervisor.
func NewSupervisor(o Options) (*Supervisor, error) {
	if o.Config == nil {
		return nil, errors.New("worker: no configuration")
	}
	if o.Store == nil {
		return nil, errors.New("worker: no store")
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.WorkerID == "" {
		o.WorkerID = o.Env.WorkerID
	}
	if o.WorkerID == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "worker"
		}
		o.WorkerID = host + "-" + strconv.Itoa(os.Getpid())
	}
	s := &Supervisor{o: o, kick: make(chan struct{}, 1), pending: o.Config, actors: map[string]actorRef{}}
	s.cfg, s.prices = o.Config, o.Prices
	s.reindex(o.Config)
	return s, nil
}

// Run holds the management surface open until ctx ends. It performs no Core
// conversation_inbox polling and starts no legacy answer loop.
func (s *Supervisor) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("worker: the supervisor is running already")
	}
	defer s.running.Store(false)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.kick:
			s.mu.Lock()
			s.cfg = s.pending
			s.reindex(s.cfg)
			s.mu.Unlock()
		}
	}
}

// Reload replaces the configuration in force.
func (s *Supervisor) Reload(cfg *config.Config) { s.put(cfg) }

// Update replaces the configuration after a registry change.
func (s *Supervisor) Update(cfg *config.Config) { s.put(cfg) }

func (s *Supervisor) put(cfg *config.Config) {
	if cfg == nil {
		return
	}
	s.mu.Lock()
	s.pending = cfg
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// SetPrices replaces the price table shared with management readers.
func (s *Supervisor) SetPrices(t *pricing.Table) {
	s.mu.Lock()
	s.prices = t
	s.mu.Unlock()
}

// Config returns the configuration in force for the chat composition root.
// It never retains a model key or Core token; those are opened per generation.
func (s *Supervisor) Config() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Running reports whether Run is active.
func (s *Supervisor) Running() bool { return s.running.Load() }

// WorkerID names this process.
func (s *Supervisor) WorkerID() string { return s.o.WorkerID }

// Location is UTC until the chat composition root supplies the school zone.
func (s *Supervisor) Location() *time.Location { return time.UTC }

// ActorAgent resolves the configured agent running as actorID at baseURL.
func (s *Supervisor) ActorAgent(baseURL, actorID string) (agentID string, hosted, ok bool) {
	key := actorKey(baseURL, actorID)
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.actors[key]
	return ref.agentID, ref.hosted, ok
}

func (s *Supervisor) reindex(cfg *config.Config) {
	actors := map[string]actorRef{}
	if cfg != nil {
		for _, a := range cfg.Agents {
			actors[actorKey(a.Core.BaseURL, a.Core.AgentID)] = actorRef{agentID: a.ID, hosted: a.Hosted != nil}
		}
	}
	s.actors = actors
}

func actorKey(baseURL, actorID string) string {
	return strings.TrimRight(baseURL, "/") + "\x00" + strings.ToLower(actorID)
}

// Status is the management view of the configuration in force.
func (s *Supervisor) Status() []AgentStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		return nil
	}
	out := make([]AgentStatus, 0, len(s.cfg.Agents))
	for _, a := range s.cfg.Agents {
		out = append(out, AgentStatus{AgentID: a.ID, Paused: a.Paused, Hosted: a.Hosted != nil})
	}
	slices.SortFunc(out, func(a, b AgentStatus) int { return strings.Compare(a.AgentID, b.AgentID) })
	return out
}
