package api

import (
	"context"
	"net/http"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/version"
)

// Info is GET /info's answer: what a front end needs to know to find the
// runtime and call it, which anyone may ask. The front end takes the
// runtime to be there only when api and api_version are these.
type Info struct {
	API        string `json:"api"`
	APIVersion int    `json:"api_version"`
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	// Audience is what the front end asks Core's assertions for, exactly
	// as Core lists it.
	Audience string `json:"audience"`
	// Issuer is the Core whose assertions the API takes.
	Issuer   string   `json:"issuer"`
	Features Features `json:"features"`
}

// Features say what the API offers, as it is served: hosting an agent by
// its id, and choosing its model on the owner's own key, each only when
// the API was given a Core to ask, the runtime's credential to ask it
// with, and a vault to seal with, as run gives it; and the school's plan
// beside them.
type Features struct {
	HostByID bool `json:"host_by_id"`
	OwnKey   bool `json:"own_key"`
	// SchoolKey is the school's plan for hosted agents (D8): true when
	// the plan in force, runtime.yaml's and the site's, offers at least
	// one model.
	SchoolKey bool `json:"school_key"`
	// Transcription is the transcriber running, or standing by, as the
	// site's settings turn it on: what a front end shows the text
	// versions' queue for.
	Transcription bool `json:"transcription"`
}

// features are what this API can do: the school's plan by the plan in
// force, or runtime.yaml's alone while the store cannot be read.
func (s *Server) features(ctx context.Context) Features {
	hosts := s.o.Vault != nil && s.o.CoreBaseURL != "" && s.o.Runtime != nil
	sc, err := s.plan(ctx)
	if err != nil {
		s.o.Log.Warn("GET /info: the site's settings cannot be read; the school's plan is runtime.yaml's", "err", err)
		sc = s.yaml().Runtime.School
	}
	return Features{HostByID: hosts, OwnKey: hosts, SchoolKey: hosts && sc.Offered(), Transcription: s.transcriptionRunning(ctx)}
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	features := s.features(ctx)
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, Info{API: "aishie-runtime", APIVersion: 1, Version: version.Version, Commit: version.Commit,
		Audience: s.o.Verifier.Audience, Issuer: s.o.Verifier.Issuer, Features: features})
}

// Me is GET /me's answer: the person the assertion names, whether they are
// one of the runtime's administrators, and how many agents they host here.
type Me struct {
	ActorID      string `json:"actor_id"`
	DisplayName  string `json:"display_name"`
	IsAdmin      bool   `json:"is_admin"`
	HostedAgents int    `json:"hosted_agents"`
}

// storeTimeout bounds each of the store's reads and writes a request makes.
const storeTimeout = 5 * time.Second

func (s *Server) me(w http.ResponseWriter, r *http.Request, c *Caller) {
	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	s.recordPerson(ctx, c)
	agents, err := s.o.Store.HostedAgentsOwnedBy(ctx, c.ActorID)
	if err != nil {
		s.o.Log.Warn("GET /me: the store cannot be read", "err", err)
		WriteError(w, Error{Code: CodeUnavailable, Reason: ReasonStoreUnavailable, Message: "the runtime's store cannot be reached"})
		return
	}
	writeJSON(w, http.StatusOK, Me{ActorID: c.ActorID, DisplayName: c.Name, IsAdmin: c.IsAdmin, HostedAgents: len(agents)})
}

// personEvery is how often a person who uses the API is recorded
// (store.PutPerson) at most.
const personEvery = 5 * time.Minute

// recordPerson records c as a person who uses the API, for the
// administrators' view and the audit, at most every personEvery; a write
// that fails is logged, and tried again at their next request.
func (s *Server) recordPerson(ctx context.Context, c *Caller) {
	now := s.o.Now()
	s.seenMu.Lock()
	last, ok := s.seen[c.ActorID]
	if ok && now.Sub(last) < personEvery {
		s.seenMu.Unlock()
		return
	}
	if len(s.seen) >= maxBuckets {
		for id, t := range s.seen {
			if now.Sub(t) >= personEvery {
				delete(s.seen, id)
			}
		}
	}
	s.seen[c.ActorID] = now
	s.seenMu.Unlock()
	err := s.o.Store.PutPerson(ctx, store.Person{CoreActorID: c.ActorID, DisplayName: c.Name, PlatformRole: c.PlatformRole, LastSeenAt: now.UTC()})
	if err != nil {
		s.o.Log.Warn("a person who used the API was not recorded", "actor", c.ActorID, "err", err)
		s.seenMu.Lock()
		delete(s.seen, c.ActorID)
		s.seenMu.Unlock()
	}
}
