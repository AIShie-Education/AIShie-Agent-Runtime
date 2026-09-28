package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/probe"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/vault"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/worker"
)

// The routes of a hosted agent's life (the API contract, §5.5 to §5.13):
// what a token is, connecting an agent by it, the owner's agents, a new
// token, pause and resume, and deleting one, its token revoked in Core
// (D7). Only an agent's owner reads or writes it; another's answers 404,
// as an id that is not there does.

// maxListed bounds GET /agents.
const maxListed = 50

// tokenRequest is inspect's, POST /agents' and PUT /token's body.
type tokenRequest struct {
	Token string `json:"token"`
	// CoreActorID is the agent the caller means, when given.
	CoreActorID string `json:"core_actor_id,omitempty"`
}

// Inspection is POST /agents/inspect's answer: what the token is, the
// agent's seats as Core lists them now, whether it is hosted here, and the
// agent's other live tokens.
type Inspection struct {
	CoreActorID  string       `json:"core_actor_id"`
	DisplayName  string       `json:"display_name"`
	OwnerActorID string       `json:"owner_actor_id"`
	Token        TokenInfo    `json:"token"`
	Seats        []Seat       `json:"seats"`
	Hosted       *HostedHere  `json:"hosted"`
	OtherTokens  *OtherTokens `json:"other_tokens"`
}

// HostedHere is an agent's row here: its id when it is the caller's (an
// earlier owner's is not theirs to name), and whether it holds this very
// token.
type HostedHere struct {
	AgentID   *string `json:"agent_id"`
	ByYou     bool    `json:"by_you"`
	SameToken bool    `json:"same_token"`
}

// Connected is POST /agents' answer: the agent, and its other live tokens
// in Core, for the one-brain warning.
type Connected struct {
	HostedAgent
	OtherTokens *OtherTokens `json:"other_tokens"`
}

// RevokedToken is a token the runtime revoked, or tried to, in Core: what
// may be shown of it, what became of it, and why it failed.
type RevokedToken struct {
	TokenInfo
	Revocation string  `json:"revocation"`
	Problem    *string `json:"problem"`
}

func revokedToken(hint string, r probe.Revocation) RevokedToken {
	t := RevokedToken{TokenInfo: tokenInfo(hint), Revocation: r.Outcome}
	if r.Problem != "" {
		p := r.Problem
		t.Problem = &p
	}
	return t
}

// TokenReplaced is PUT /token's answer: the agent, and its previous token.
type TokenReplaced struct {
	Agent         HostedAgent  `json:"agent"`
	PreviousToken RevokedToken `json:"previous_token"`
}

// Deleted is DELETE's answer: the agent gone, and its token.
type Deleted struct {
	Deleted DeletedAgent `json:"deleted"`
	Token   RevokedToken `json:"token"`
}

// DeletedAgent names an agent deleted.
type DeletedAgent struct {
	ID          string `json:"id"`
	CoreActorID string `json:"core_actor_id"`
}

// tenantOf is the tenant of an owner's agents and secrets.
func tenantOf(owner string) string { return "ten_" + owner }

// noteAgent puts the agent's id on the request's log line.
func noteAgent(w http.ResponseWriter, id string) {
	if rec, ok := w.(*recorder); ok {
		rec.agent = id
	}
}

// storeUnavailable answers a store that did not answer.
func (s *Server) storeUnavailable(w http.ResponseWriter, what string, err error) {
	s.o.Log.Warn("the store cannot be reached", "what", what, "err", err)
	WriteError(w, Error{Code: CodeUnavailable, Reason: ReasonStoreUnavailable, Message: "the runtime's store cannot be reached"})
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

// liveSeats are an agent's seats as Core lists them now, as facts.
func (s *Server) liveSeats(ins *inspected) []Seat {
	now := s.o.Now()
	seats := make([]Seat, 0, len(ins.seats))
	for _, m := range ins.seats {
		seats = append(seats, seatOf(worker.SeatSnapshot("", m, now)))
	}
	sortSeats(seats)
	return seats
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

// readToken reads a token route's body and the agent it means, having
// answered a refusal.
func readToken(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) (tokenRequest, probe.Want, bool) {
	var req tokenRequest
	if !decodeBody(w, r, &req, false) {
		return req, probe.Want{}, false
	}
	au.detail["token_hint"] = vault.Hint(store.SecretCoreToken, req.Token)
	want, bad := wantOf(req.CoreActorID, c)
	if bad != nil {
		WriteError(w, *bad)
		return req, want, false
	}
	if want.ActorID != "" {
		au.target("core_actor", want.ActorID)
		au.detail["core_actor_id"] = want.ActorID
	}
	return req, want, true
}

// inspect is POST /agents/inspect: what a token is, before it is
// connected. Nothing is written; the token is dropped when the request
// ends.
func (s *Server) inspect(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	req, want, ok := readToken(w, r, c, au)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), inspectTimeout)
	defer cancel()
	ins, e := s.inspectToken(ctx, req.Token, want)
	if e != nil {
		WriteError(w, *e)
		return
	}
	au.target("core_actor", ins.me.ID)
	au.detail["core_actor_id"] = ins.me.ID
	out := Inspection{CoreActorID: ins.me.ID, DisplayName: ins.me.DisplayName, OwnerActorID: ins.me.OwnerActorID,
		Token: TokenInfo{Hint: ins.hint, Prefix: ins.prefix}, Seats: s.liveSeats(ins)}
	var except []string
	row, err := s.o.Store.HostedAgentByActor(ctx, ins.me.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		s.storeUnavailable(w, "a hosted agent by its actor", err)
		return
	default:
		here := &HostedHere{ByYou: sameActor(row.OwnerActorID, c.ActorID), SameToken: probe.HintPrefix(row.TokenHint) == ins.prefix}
		if here.ByYou {
			id := row.ID
			here.AgentID = &id
		}
		out.Hosted = here
		except = append(except, probe.HintPrefix(row.TokenHint))
	}
	out.OtherTokens = s.otherTokens(ctx, ins, except...)
	writeJSON(w, http.StatusOK, out)
}

// connectAttempts bounds how often connect looks again for the agent's row
// when a concurrent connect made one first.
const connectAttempts = 3

// connect is POST /agents: the agent the token is hosted here, the token
// sealed in its row, its seats recorded. A retry with the same token
// replays the agent as it is (200, Idempotency-Replayed); another token of
// an agent the caller hosts already is already_hosted; an earlier owner's
// row is taken over, deleted and purged first.
func (s *Server) connect(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	req, want, ok := readToken(w, r, c, au)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), inspectTimeout)
	defer cancel()
	ins, e := s.inspectToken(ctx, req.Token, want)
	if e != nil {
		WriteError(w, *e)
		return
	}
	au.target("core_actor", ins.me.ID)
	au.detail["core_actor_id"] = ins.me.ID
	for range connectAttempts {
		row, err := s.o.Store.HostedAgentByActor(ctx, ins.me.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			s.storeUnavailable(w, "a hosted agent by its actor", err)
			return
		case sameActor(row.OwnerActorID, c.ActorID) && probe.HintPrefix(row.TokenHint) == ins.prefix:
			// The same token, connected again: what the first did.
			au.target("hosted_agent", row.ID)
			au.detail["agent_id"], au.detail["replayed"] = row.ID, true
			noteAgent(w, row.ID)
			s.writeConnected(ctx, w, http.StatusOK, row, ins, true)
			return
		case sameActor(row.OwnerActorID, c.ActorID):
			au.detail["agent_id"] = row.ID
			WriteError(w, Error{Code: CodeConflict, Reason: ReasonAlreadyHosted,
				Message: "the agent is hosted here already, with another token: replace its token with PUT …/token",
				Details: map[string]any{"agent_id": row.ID}})
			return
		default:
			// Core gave the agent to the caller since an earlier owner
			// connected it, and revoked every token it had then.
			if !s.takeOver(ctx, w, r, row) {
				return
			}
		}
		if s.o.Actors != nil {
			if _, hosted, ok := s.o.Actors.ActorAgent(s.o.CoreBaseURL, ins.me.ID); ok && !hosted {
				WriteError(w, Error{Code: CodeConflict, Reason: ReasonOperatorAgent,
					Message: "the runtime's operator runs this agent already, and the operator's configuration wins"})
				return
			}
		}
		created, done := s.create(ctx, w, c, ins)
		if done {
			if created != nil {
				au.target("hosted_agent", created.ID)
				au.detail["agent_id"], au.detail["seats"] = created.ID, len(ins.seats)
			}
			return
		}
	}
	s.storeUnavailable(w, "a hosted agent created", errors.New("another connection of the agent kept winning"))
}

// takeOver deletes and purges an earlier owner's row of an agent the
// caller now owns in Core, and audits it; it reports whether connecting
// may go on, having answered when not.
func (s *Server) takeOver(ctx context.Context, w http.ResponseWriter, r *http.Request, row *store.HostedAgent) bool {
	if err := s.o.Store.DeleteHostedAgent(ctx, row.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.storeUnavailable(w, "an earlier owner's hosted agent deleted", err)
		return false
	}
	s.purge(ctx, row.ID)
	s.Audit(ctx, r, store.AuditEvent{Action: "agent.takeover", TargetType: "hosted_agent", TargetID: row.ID, Outcome: "ok",
		Detail: mustJSON(map[string]any{"agent_id": row.ID, "core_actor_id": row.CoreActorID, "token_hint": row.TokenHint})})
	s.o.Log.Info("a hosted agent was taken over by its new owner in Core; the earlier owner's row was deleted", "agent", row.ID)
	return true
}

// create stores the agent ins names, its token sealed, and records its
// seats; done is false when a concurrent connect stored it first, to be
// looked at again. It answers otherwise.
func (s *Server) create(ctx context.Context, w http.ResponseWriter, c *Caller, ins *inspected) (created *store.HostedAgent, done bool) {
	if s.o.Vault == nil {
		s.o.Log.Error("the API cannot seal a token: it has no keyring (KMS_KEY_ID)")
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime cannot keep tokens now"})
		return nil, true
	}
	tenant := tenantOf(c.ActorID)
	sealed, err := s.o.Vault.Seal(ctx, store.Secret{ID: vault.NewSecretID(), TenantID: tenant, Kind: store.SecretCoreToken,
		CreatedBy: c.ActorID}, ins.token)
	if err != nil {
		s.o.Log.Error("a token could not be sealed", "err", err)
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime could not keep the token"})
		return nil, true
	}
	row, err := s.o.Store.CreateHostedAgent(ctx, store.HostedAgent{
		ID: "agt_" + uuid.NewString(), CoreActorID: ins.me.ID, OwnerActorID: c.ActorID, OwnerVerified: true, TenantID: tenant,
		DisplayName: ins.me.DisplayName, TokenSecretID: sealed.ID, TokenHint: sealed.Hint, Settings: []byte(`{}`),
	}, sealed)
	switch {
	case errors.Is(err, store.ErrExists):
		return nil, false
	case err != nil:
		s.storeUnavailable(w, "a hosted agent created", err)
		return nil, true
	}
	noteAgent(w, row.ID)
	now := s.o.Now()
	for _, m := range ins.seats {
		if err := s.o.Store.SeatSeen(ctx, worker.SeatSnapshot(row.ID, m, now)); err != nil {
			s.o.Log.Warn("a newly connected agent's seat was not recorded; the worker records it", "agent", row.ID, "err", err)
		}
	}
	w.Header().Set("Location", Prefix+"agents/"+row.ID)
	s.writeConnected(ctx, w, http.StatusCreated, row, ins, false)
	return row, true
}

// writeConnected answers POST /agents: the agent and its other tokens.
func (s *Server) writeConnected(ctx context.Context, w http.ResponseWriter, status int, row *store.HostedAgent, ins *inspected, replayed bool) {
	v, err := s.view(ctx, row)
	if err != nil {
		s.storeUnavailable(w, "a hosted agent's view", err)
		return
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("ETag", etag(row.Version))
	writeJSON(w, status, Connected{HostedAgent: *v, OtherTokens: s.otherTokens(ctx, ins)})
}

// purge removes what the store holds of a deleted agent but its ledger;
// what it misses, or a worker writes after it, housekeeping purges.
func (s *Server) purge(ctx context.Context, id string) {
	if err := s.o.Store.PurgeAgent(ctx, id); err != nil {
		s.o.Log.Warn("a deleted agent was not purged; housekeeping purges it", "agent", id, "err", err)
	}
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

// replaceAttempts bounds PUT /token's read-modify-write when the row
// changes under it.
const replaceAttempts = 3

// replaceToken is PUT /agents/{id}/token: a new token of the agent's own,
// which Core must say is the agent's and the caller's, sealed in its row;
// the previous one's secret is destroyed with the write, and the new
// token revokes it in Core. The same token again changes nothing.
func (s *Server) replaceToken(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	version, named, bad := ifMatch(r)
	if bad != nil {
		WriteError(w, *bad)
		return
	}
	var req tokenRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	if req.CoreActorID != "" {
		WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonUnknownField, Message: "PUT …/token takes the token alone",
			Details: map[string]any{"field": "/core_actor_id"}})
		return
	}
	au.detail["new_hint"] = vault.Hint(store.SecretCoreToken, req.Token)
	ctx, cancel := context.WithTimeout(r.Context(), inspectTimeout+revokeTimeout)
	defer cancel()
	row := s.ownRow(ctx, w, r.PathValue("id"), c)
	if row == nil || !checkVersion(w, row, version, named) {
		return
	}
	au.detail["agent_id"], au.detail["old_hint"] = row.ID, row.TokenHint
	ictx, icancel := context.WithTimeout(ctx, inspectTimeout)
	defer icancel()
	ins, e := s.inspectToken(ictx, req.Token, probe.Want{ActorID: row.CoreActorID, Owner: c.ActorID})
	if e != nil {
		WriteError(w, *e)
		return
	}
	oldHint := row.TokenHint
	if probe.HintPrefix(oldHint) == ins.prefix {
		au.detail["revocation"] = probe.NotAttempted
		v, err := s.view(ctx, row)
		if err != nil {
			s.storeUnavailable(w, "a hosted agent's view", err)
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("ETag", etag(row.Version))
		writeJSON(w, http.StatusOK, TokenReplaced{Agent: *v, PreviousToken: revokedToken(oldHint, probe.Revocation{Outcome: probe.NotAttempted})})
		return
	}
	if s.o.Vault == nil {
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime cannot keep tokens now"})
		return
	}
	sealed, err := s.o.Vault.Seal(ctx, store.Secret{ID: vault.NewSecretID(), TenantID: row.TenantID, Kind: store.SecretCoreToken,
		CreatedBy: c.ActorID}, ins.token)
	if err != nil {
		s.o.Log.Error("a token could not be sealed", "agent", row.ID, "err", err)
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the runtime could not keep the token"})
		return
	}
	var updated *store.HostedAgent
	for attempt := range replaceAttempts {
		next := *row
		next.TokenSecretID, next.TokenHint, next.DisplayName, next.OwnerVerified = sealed.ID, sealed.Hint, ins.me.DisplayName, true
		updated, err = s.o.Store.UpdateHostedAgent(ctx, next, sealed)
		if !errors.Is(err, store.ErrConflict) || named || attempt == replaceAttempts-1 {
			break
		}
		if row = s.ownRow(ctx, w, row.ID, c); row == nil {
			return
		}
		oldHint = row.TokenHint
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, errAgentNotFound)
		return
	case errors.Is(err, store.ErrConflict):
		current := row.Version
		if again, rerr := s.o.Store.HostedAgent(ctx, row.ID); rerr == nil {
			current = again.Version
		}
		WriteError(w, versionMismatch(current))
		return
	case err != nil:
		s.storeUnavailable(w, "a hosted agent's token replaced", err)
		return
	}
	// The new token, the agent's own, revokes the one it replaces: the
	// old secret is never opened.
	rctx, rcancel := context.WithTimeout(ctx, revokeTimeout)
	defer rcancel()
	rev := probe.RevokeToken(rctx, ins.client, probe.HintPrefix(oldHint), s.o.Now())
	au.detail["old_hint"], au.detail["revocation"] = oldHint, rev.Outcome
	if rev.Problem != "" {
		au.detail["revocation_problem"] = rev.Problem
		s.o.Log.Warn("the token a new one replaced was not revoked in Core", "agent", updated.ID, "revocation", rev.String())
	}
	v, err := s.view(ctx, updated)
	if err != nil {
		s.storeUnavailable(w, "a hosted agent's view", err)
		return
	}
	w.Header().Set("ETag", etag(updated.Version))
	writeJSON(w, http.StatusOK, TokenReplaced{Agent: *v, PreviousToken: revokedToken(oldHint, rev)})
}

// pause is POST /agents/{id}/pause (paused) and …/resume: the agent set
// to it. Already so, nothing is written, and nothing audited.
func (s *Server) pause(paused bool) func(http.ResponseWriter, *http.Request, *Caller, *auditing) {
	return func(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
		version, named, bad := ifMatch(r)
		if bad != nil {
			WriteError(w, *bad)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
		defer cancel()
		row := s.ownRow(ctx, w, r.PathValue("id"), c)
		if row == nil || !checkVersion(w, row, version, named) {
			return
		}
		au.detail["agent_id"] = row.ID
		if row.Paused == paused {
			au.skip = true
			s.writeAgent(ctx, w, http.StatusOK, row)
			return
		}
		updated, err := s.o.Store.SetHostedAgentPaused(ctx, row.ID, paused)
		switch {
		case errors.Is(err, store.ErrNotFound):
			WriteError(w, errAgentNotFound)
			return
		case err != nil:
			s.storeUnavailable(w, "a hosted agent paused", err)
			return
		}
		au.detail["version"] = updated.Version
		s.writeAgent(ctx, w, http.StatusOK, updated)
	}
}

// revokeParam reads DELETE's one query parameter, revoke_token, true by
// default, having answered a refusal: any other parameter is
// unknown_parameter, and a value but true or false invalid_field.
func revokeParam(w http.ResponseWriter, r *http.Request) (bool, bool) {
	q := r.URL.Query()
	for k := range q {
		if k != "revoke_token" {
			WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonUnknownParameter, Message: "DELETE takes revoke_token alone",
				Details: map[string]any{"field": k}})
			return false, false
		}
	}
	if strings.Contains(r.URL.RawQuery, ";") {
		WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonUnknownParameter, Message: "DELETE takes revoke_token alone",
			Details: map[string]any{"field": ""}})
		return false, false
	}
	vs, ok := q["revoke_token"]
	if !ok {
		return true, true
	}
	if len(vs) == 1 && (vs[0] == "true" || vs[0] == "false") {
		return vs[0] == "true", true
	}
	WriteError(w, Error{Code: CodeInvalidArgument, Reason: ReasonInvalidField, Message: "revoke_token is true or false",
		Details: map[string]any{"field": "revoke_token"}})
	return false, false
}

// remove is DELETE /agents/{id}: the agent's token revoked in Core with
// itself (D7, unless revoke_token=false), then the agent, its courses and
// its secrets destroyed in one transaction, which stops it on every
// worker, and what the store held of it purged, its ledger kept. The row
// is deleted whatever became of the revocation.
func (s *Server) remove(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	revoke, ok := revokeParam(w, r)
	if !ok {
		return
	}
	var none struct{}
	if !decodeBody(w, r, &none, true) {
		return
	}
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
	au.detail["agent_id"], au.detail["core_actor_id"], au.detail["token_hint"] = row.ID, row.CoreActorID, row.TokenHint
	rev := probe.Revocation{Outcome: probe.NotAttempted}
	if revoke {
		rev = s.revokeStored(ctx, row)
	}
	au.detail["revocation"] = rev.Outcome
	if rev.Problem != "" {
		au.detail["revocation_problem"] = rev.Problem
	}
	switch err := s.o.Store.DeleteHostedAgent(ctx, row.ID); {
	case errors.Is(err, store.ErrNotFound):
		WriteError(w, errAgentNotFound)
		return
	case err != nil:
		s.storeUnavailable(w, "a hosted agent deleted", err)
		return
	}
	s.purge(ctx, row.ID)
	writeJSON(w, http.StatusOK, Deleted{Deleted: DeletedAgent{ID: row.ID, CoreActorID: row.CoreActorID}, Token: revokedToken(row.TokenHint, rev)})
}

// revokeStored revokes the agent's stored token in Core with itself:
// opened from the vault here, the one stored secret the API opens, and
// dropped straight after.
func (s *Server) revokeStored(ctx context.Context, row *store.HostedAgent) probe.Revocation {
	if s.o.Vault == nil {
		s.o.Log.Error("the API cannot open a token to revoke it: it has no keyring (KMS_KEY_ID)", "agent", row.ID)
		return probe.Revocation{Outcome: probe.Failed, Problem: probe.ProblemCoreRefused}
	}
	token, err := vault.Opener{Vault: s.o.Vault, Store: s.o.Store}.OpenSecret(ctx, row.TokenSecretID)
	if err != nil {
		s.o.Log.Error("a hosted agent's token could not be opened to revoke it", "agent", row.ID, "err", err)
		return probe.Revocation{Outcome: probe.Failed, Problem: probe.ProblemCoreRefused}
	}
	rctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	rev := probe.RevokeToken(rctx, probe.NewClient(s.o.CoreBaseURL, token, s.coreHTTP), probe.HintPrefix(row.TokenHint), s.o.Now())
	if rev.Problem != "" {
		s.o.Log.Warn("a deleted agent's token was not revoked in Core; its owner revokes it", "agent", row.ID, "revocation", rev.String())
	}
	return rev
}
