package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// The routes of a hosted agent's life (docs/design.md §11.4): what an
// agent of the caller's is in Core, hosting it here by its id, the owner's
// agents, a new token, pause and resume, and deleting one. Every agent is
// hosted one way in Core, for good (AIShie-Core #52): an agent hosted
// runtime is the site's runtime's to run, and only it; an mcp agent is
// never hosted here. Nobody gives the runtime a token: the worker that runs
// the agent is issued its one token by the agent's id, with the runtime's
// own credential, and the API revokes it as the hosting ends (paused, or
// deleted). Only an agent's owner reads or writes it; another's answers
// 404, as an id that is not there does.

// maxListed bounds GET /agents.
const maxListed = 50

// agentRequest is inspect's and POST /agents' body: the agent's id in
// Core.
type agentRequest struct {
	AgentID string `json:"agent_id"`
}

// Inspection is POST /agents/inspect's answer: the agent as Core hosts it,
// whether the runtime may host it (and why not), and whether it is hosted
// here.
type Inspection struct {
	CoreActorID  string `json:"core_actor_id"`
	DisplayName  string `json:"display_name"`
	OwnerActorID string `json:"owner_actor_id"`
	// Hosting is how Core hosts it, for good: runtime or mcp.
	Hosting string `json:"hosting"`
	// Hostable is whether POST /agents would host it now; Reason, when
	// not, says why: mcp_agent, agent_suspended, owner_suspended,
	// operator_agent.
	Hostable bool    `json:"hostable"`
	Reason   *string `json:"reason"`
	// LiveSeats is how many of its seats are live in Core now.
	LiveSeats int `json:"live_seats"`
	// SiteChat is whether people in the site may ask it now: Core holds a
	// live token of the runtime's for it.
	SiteChat bool        `json:"site_chat"`
	Hosted   *HostedHere `json:"hosted"`
}

// HostedHere is an agent's row here: its id when it is the caller's (an
// earlier owner's is not theirs to name).
type HostedHere struct {
	ID    *string `json:"id"`
	ByYou bool    `json:"by_you"`
}

// Paused is POST …/pause's answer: the agent, and what became of the
// token the runtime held for it.
type Paused struct {
	HostedAgent
	Revocation Revocation `json:"revocation"`
}

// Deleted is DELETE's answer: the agent gone, and what became of its
// token.
type Deleted struct {
	Deleted    DeletedAgent `json:"deleted"`
	Revocation Revocation   `json:"revocation"`
}

// DeletedAgent names an agent deleted.
type DeletedAgent struct {
	ID          string `json:"id"`
	CoreActorID string `json:"core_actor_id"`
}

// errAgentNotFound is an agent that is not there, or not the caller's.
var errAgentNotFound = Error{Code: CodeNotFound, Reason: ReasonAgentNotFound, Message: "no such hosted agent of yours"}

// ownRow is the caller's hosted agent id, or nil, having answered: 404
// for an id of the wrong shape, one not there, or another's.
func (s *Server) ownRow(ctx context.Context, w http.ResponseWriter, id string, c *Caller) *store.HostedAgent {
	if !store.IsHostedAgentID(id) {
		WriteError(w, errAgentNotFound)
		return nil
	}
	row, err := s.o.Store.HostedAgent(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, errAgentNotFound)
		return nil
	case err != nil:
		s.storeUnavailable(w, "a hosted agent", err)
		return nil
	case !sameActor(row.OwnerActorID, c.ActorID):
		WriteError(w, errAgentNotFound)
		return nil
	}
	noteAgent(w, row.ID)
	return row
}

// writeAgent answers the agent as its owner reads it, with its ETag.
func (s *Server) writeAgent(ctx context.Context, w http.ResponseWriter, status int, row *store.HostedAgent) {
	v, err := s.view(ctx, row)
	if err != nil {
		s.storeUnavailable(w, "a hosted agent's view", err)
		return
	}
	w.Header().Set("ETag", etag(row.Version))
	writeJSON(w, status, v)
}

// limited serves h within the caller's allowance of l.
func (s *Server) limited(l *limiter, h func(http.ResponseWriter, *http.Request, *Caller)) func(http.ResponseWriter, *http.Request, *Caller) {
	return func(w http.ResponseWriter, r *http.Request, c *Caller) {
		if ok, wait := l.take(c.ActorID, s.o.Now()); !ok {
			writeRateLimited(w, wait)
			return
		}
		h(w, r, c)
	}
}

// limitedKeyTest serves h within the caller's allowance of keys/test: its
// bucket, and KeyTestsPerDay in a UTC day.
func (s *Server) limitedKeyTest(h func(http.ResponseWriter, *http.Request, *Caller)) func(http.ResponseWriter, *http.Request, *Caller) {
	return func(w http.ResponseWriter, r *http.Request, c *Caller) {
		now := s.o.Now()
		if ok, wait := s.keyTest.take(c.ActorID, now); !ok {
			writeRateLimited(w, wait)
			return
		}
		if ok, wait := s.keyDay.take(c.ActorID, now); !ok {
			writeRateLimited(w, wait)
			return
		}
		h(w, r, c)
	}
}

// readAgent reads inspect's and POST /agents' body, and the agent's id in
// Core it names, having answered a refusal.
func readAgent(w http.ResponseWriter, r *http.Request, au *auditing) (string, bool) {
	var req agentRequest
	if !readBody(w, r, &req) {
		return "", false
	}
	id, bad := agentIDOf(req.AgentID)
	if bad != nil {
		WriteError(w, *bad)
		return "", false
	}
	au.target("core_actor", id)
	au.detail["core_actor_id"] = id
	return id, true
}

// operatorRuns reports whether the operator's configuration runs the agent
// coreActorID (the worker's view): a hosted agent may not be it.
func (s *Server) operatorRuns(coreActorID string) bool {
	if s.o.Actors == nil {
		return false
	}
	_, hosted, ok := s.o.Actors.ActorAgent(s.o.CoreBaseURL, coreActorID)
	return ok && !hosted
}

// inspect is POST /agents/inspect: what an agent of the caller's is, as
// Core hosts it, before it is hosted here. Nothing is written.
func (s *Server) inspect(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	id, ok := readAgent(w, r, au)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), askTimeout)
	defer cancel()
	view, e := s.ownedAgent(ctx, c, id)
	if e != nil {
		WriteError(w, *e)
		return
	}
	out := Inspection{CoreActorID: view.AgentID, DisplayName: view.DisplayName, OwnerActorID: view.OwnerActorID, Hosting: view.Hosting,
		Hostable: view.Hostable, LiveSeats: view.LiveSeats, SiteChat: view.SiteChat}
	reason := hostReason(view.Reason)
	if view.Hostable && s.operatorRuns(view.AgentID) {
		out.Hostable, reason = false, ReasonOperatorAgent
	}
	if !out.Hostable && reason != "" {
		out.Reason = &reason
	}
	row, err := s.o.Store.HostedAgentByActor(ctx, view.AgentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		s.storeUnavailable(w, "a hosted agent by its actor", err)
		return
	default:
		here := &HostedHere{ByYou: sameActor(row.OwnerActorID, c.ActorID)}
		if here.ByYou {
			rowID := row.ID
			here.ID = &rowID
		}
		out.Hosted = here
	}
	writeJSON(w, http.StatusOK, out)
}

// hostAttempts bounds how often host looks again for the agent's row when
// a concurrent request made one first.
const hostAttempts = 3

// host is POST /agents: the caller's agent, which Core hosts runtime,
// hosted here by its id, in a new row without a model (needs_model); the
// worker is issued its token once it has one. Core says whether the agent
// is the caller's (404 when not) and may be hosted (mcp_agent,
// agent_suspended, owner_suspended); one the operator's configuration runs
// is operator_agent. Asked again, it replays the row as it is (200,
// Idempotency-Replayed); an earlier owner's row of an agent Core says is
// the caller's is taken over: deleted, purged, and its token revoked.
func (s *Server) host(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	id, ok := readAgent(w, r, au)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), askTimeout+revokeTimeout)
	defer cancel()
	view, e := s.ownedAgent(ctx, c, id)
	if e != nil {
		WriteError(w, *e)
		return
	}
	if !view.Hostable {
		WriteError(w, hostRefusal(view))
		return
	}
	if s.operatorRuns(view.AgentID) {
		WriteError(w, Error{Code: CodeConflict, Reason: ReasonOperatorAgent,
			Message: "the runtime's operator runs this agent already, and the operator's configuration wins"})
		return
	}
	if !s.keepsTokens(w) {
		return
	}
	for range hostAttempts {
		row, err := s.o.Store.HostedAgentByActor(ctx, view.AgentID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			s.storeUnavailable(w, "a hosted agent by its actor", err)
			return
		case sameActor(row.OwnerActorID, c.ActorID):
			// Hosted already: what the first request did.
			au.target("hosted_agent", row.ID)
			au.detail["agent_id"], au.detail["replayed"] = row.ID, true
			noteAgent(w, row.ID)
			w.Header().Set("Idempotency-Replayed", "true")
			s.writeAgent(ctx, w, http.StatusOK, row)
			return
		default:
			if !s.takeOver(ctx, w, r, row) {
				return
			}
		}
		row, err = s.o.Store.CreateHostedAgent(ctx, store.HostedAgent{
			ID: "agt_" + uuid.NewString(), CoreActorID: view.AgentID, OwnerActorID: c.ActorID, OwnerVerified: true,
			TenantID: tenantOf(c.ActorID), DisplayName: view.DisplayName, Settings: []byte(`{}`),
		})
		switch {
		case errors.Is(err, store.ErrExists):
			continue
		case err != nil:
			s.storeUnavailable(w, "a hosted agent created", err)
			return
		}
		au.target("hosted_agent", row.ID)
		au.detail["agent_id"] = row.ID
		noteAgent(w, row.ID)
		w.Header().Set("Location", Prefix+"agents/"+row.ID)
		s.writeAgent(ctx, w, http.StatusCreated, row)
		return
	}
	s.storeUnavailable(w, "a hosted agent created", errors.New("another request hosting the agent kept winning"))
}

// takeOver deletes and purges an earlier owner's row of an agent Core
// says is the caller's (an agent given to them before an agent's owner
// was fixed in Core), revokes the token the runtime held for it, and
// audits it; it reports whether hosting may go on, having answered when
// not.
func (s *Server) takeOver(ctx context.Context, w http.ResponseWriter, r *http.Request, row *store.HostedAgent) bool {
	if err := s.o.Store.DeleteHostedAgent(ctx, row.ID, store.DeleteIf{}); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.storeUnavailable(w, "an earlier owner's hosted agent deleted", err)
		return false
	}
	s.purge(ctx, row.ID)
	rev := s.revokeHosting(ctx, row.ID, row.CoreActorID)
	detail := map[string]any{"agent_id": row.ID, "core_actor_id": row.CoreActorID, "revocation": rev.Outcome}
	if rev.Problem != nil {
		detail["revocation_problem"] = *rev.Problem
	}
	s.Audit(ctx, r, store.AuditEvent{Action: "agent.takeover", TargetType: "hosted_agent", TargetID: row.ID, Outcome: "ok",
		Detail: mustJSON(detail)})
	s.o.Log.Info("a hosted agent was taken over by its owner in Core; the earlier owner's row was deleted", "agent", row.ID)
	return true
}

// AgentList is GET /agents' answer.
type AgentList struct {
	Agents []HostedAgent `json:"agents"`
}

// list is GET /agents: the caller's hosted agents, oldest first, at most
// 50.
func (s *Server) list(w http.ResponseWriter, r *http.Request, c *Caller) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	rows, err := s.o.Store.HostedAgentsOwnedBy(ctx, c.ActorID)
	if err != nil {
		s.storeUnavailable(w, "a person's hosted agents", err)
		return
	}
	slices.SortFunc(rows, func(x, y store.HostedAgent) int {
		return cmp.Or(x.CreatedAt.Compare(y.CreatedAt), strings.Compare(x.ID, y.ID))
	})
	if len(rows) > maxListed {
		rows = rows[:maxListed]
	}
	out := AgentList{Agents: make([]HostedAgent, 0, len(rows))}
	for i := range rows {
		v, err := s.view(ctx, &rows[i])
		if err != nil {
			s.storeUnavailable(w, "a hosted agent's view", err)
			return
		}
		out.Agents = append(out.Agents, *v)
	}
	writeJSON(w, http.StatusOK, out)
}

// get is GET /agents/{id}: the caller's hosted agent, with its ETag.
func (s *Server) get(w http.ResponseWriter, r *http.Request, c *Caller) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	if row := s.ownRow(ctx, w, r.PathValue("id"), c); row != nil {
		s.writeAgent(ctx, w, http.StatusOK, row)
	}
}

// checkVersion refuses a write whose If-Match names another version than
// row's, having answered; with none named, any is taken.
func checkVersion(w http.ResponseWriter, row *store.HostedAgent, version int, named bool) bool {
	if named && version != row.Version {
		WriteError(w, versionMismatch(row.Version))
		return false
	}
	return true
}

// renewAttempts bounds POST …/token's read-modify-write when the row
// changes under it.
const renewAttempts = 3

// renewToken is POST /agents/{id}/token: the agent issued a new token,
// after Core refused the one the runtime held (revoked there by its owner
// or an administrator: status needs_token). Core must still say the agent
// is the caller's and may be hosted; the token the row holds is dropped,
// at the version If-Match names when it names one, and the worker running
// the agent is issued another, which revokes any left in Core. A row
// holding none changes nothing (200, Idempotency-Replayed).
func (s *Server) renewToken(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	version, named, bad := ifMatch(r)
	if bad != nil {
		WriteError(w, *bad)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), askTimeout+2*storeTimeout)
	defer cancel()
	row := s.ownRow(ctx, w, r.PathValue("id"), c)
	if row == nil || !checkVersion(w, row, version, named) {
		return
	}
	au.detail["agent_id"], au.detail["core_actor_id"] = row.ID, row.CoreActorID
	view, e := s.ownedAgent(ctx, c, row.CoreActorID)
	switch {
	case e != nil && e.Reason == ReasonAgentNotFound:
		WriteError(w, Error{Code: CodeFailedPrecondition, Reason: ReasonOwnerChanged,
			Message: "Core does not say the agent is yours: delete it here"})
		return
	case e != nil:
		WriteError(w, *e)
		return
	case !view.Hostable:
		WriteError(w, hostRefusal(view))
		return
	}
	for attempt := range renewAttempts {
		if row.TokenSecretID == "" {
			au.skip = true
			w.Header().Set("Idempotency-Replayed", "true")
			s.writeAgent(ctx, w, http.StatusOK, row)
			return
		}
		next := *row
		next.TokenSecretID, next.TokenHint, next.TokenIssued, next.TokenCredentialID = "", "", false, ""
		updated, err := s.o.Store.UpdateHostedAgent(ctx, next)
		switch {
		case err == nil:
			au.detail["version"] = updated.Version
			s.writeAgent(ctx, w, http.StatusOK, updated)
			return
		case errors.Is(err, store.ErrNotFound):
			WriteError(w, errAgentNotFound)
			return
		case !errors.Is(err, store.ErrConflict):
			s.storeUnavailable(w, "a hosted agent's token dropped", err)
			return
		case named || attempt == renewAttempts-1:
			s.writeMismatch(ctx, w, row.ID, row.Version)
			return
		}
		if row = s.ownRow(ctx, w, row.ID, c); row == nil {
			return
		}
	}
}

// pause is POST /agents/{id}/pause (paused) and …/resume: the agent set
// to it, at the version If-Match names when it names one (a write since is
// 412), and at any otherwise. Paused, its row holds no token, and the
// token the runtime held for it is revoked in Core, after the write, so
// that no worker is issued one after it; pausing an agent paused already
// writes nothing, and audits nothing, but revokes again (one that failed
// is tried again so). Resumed, the worker that runs it is issued another.
func (s *Server) pause(paused bool) func(http.ResponseWriter, *http.Request, *Caller, *auditing) {
	return func(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
		version, named, bad := ifMatch(r)
		if bad != nil {
			WriteError(w, *bad)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout+revokeTimeout)
		defer cancel()
		row := s.ownRow(ctx, w, r.PathValue("id"), c)
		if row == nil || !checkVersion(w, row, version, named) {
			return
		}
		au.detail["agent_id"] = row.ID
		updated := row
		if row.Paused == paused {
			au.skip = true
		} else {
			var err error
			updated, err = s.o.Store.SetHostedAgentPaused(ctx, row.ID, paused, heldTo(version, named))
			switch {
			case errors.Is(err, store.ErrNotFound):
				WriteError(w, errAgentNotFound)
				return
			case errors.Is(err, store.ErrConflict):
				s.writeMismatch(ctx, w, row.ID, row.Version)
				return
			case err != nil:
				s.storeUnavailable(w, "a hosted agent paused", err)
				return
			}
			au.detail["version"] = updated.Version
		}
		if !paused {
			s.writeAgent(ctx, w, http.StatusOK, updated)
			return
		}
		rev := s.revokeHosting(ctx, updated.ID, updated.CoreActorID)
		au.detail["revocation"] = rev.Outcome
		if rev.Problem != nil {
			au.detail["revocation_problem"] = *rev.Problem
		}
		v, err := s.view(ctx, updated)
		if err != nil {
			s.storeUnavailable(w, "a hosted agent's view", err)
			return
		}
		w.Header().Set("ETag", etag(updated.Version))
		writeJSON(w, http.StatusOK, Paused{HostedAgent: *v, Revocation: rev})
	}
}

// remove is DELETE /agents/{id}: the agent, its courses and its secrets
// (its token, its key) destroyed in one transaction, at the version
// If-Match names when it names one (a write since is 412), which stops it
// on every worker; then the token the runtime held for it revoked in Core
// (a worker issued one meanwhile finds the row gone, and revokes it), and
// what the store held of it purged, its ledger kept.
func (s *Server) remove(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	version, named, bad := ifMatch(r)
	if bad != nil {
		WriteError(w, *bad)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), revokeTimeout+2*storeTimeout)
	defer cancel()
	row := s.ownRow(ctx, w, r.PathValue("id"), c)
	if row == nil || !checkVersion(w, row, version, named) {
		return
	}
	au.detail["agent_id"], au.detail["core_actor_id"] = row.ID, row.CoreActorID
	err := s.o.Store.DeleteHostedAgent(ctx, row.ID, store.DeleteIf{Version: heldTo(version, named)})
	switch {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, errAgentNotFound)
		return
	case errors.Is(err, store.ErrConflict):
		s.writeMismatch(ctx, w, row.ID, row.Version)
		return
	case err != nil:
		s.storeUnavailable(w, "a hosted agent deleted", err)
		return
	}
	s.purge(ctx, row.ID)
	rev := s.revokeHosting(ctx, row.ID, row.CoreActorID)
	au.detail["revocation"] = rev.Outcome
	if rev.Problem != nil {
		au.detail["revocation_problem"] = *rev.Problem
	}
	writeJSON(w, http.StatusOK, Deleted{Deleted: DeletedAgent{ID: row.ID, CoreActorID: row.CoreActorID}, Revocation: rev})
}
