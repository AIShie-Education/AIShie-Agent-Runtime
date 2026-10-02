package fakecore

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Agents' hosting (AIShie-Core #52, its docs/schema.md "Agents'
// hosting"): every agent is run one way, chosen when it is registered and
// never changed. A runtime agent is run by the site's agent runtime and
// nothing else: its one token is issued to the runtime, through the
// agent_runtime service by the agent's id, and its owner holds none. An
// mcp agent is its owner's own tools', with tokens its owner issues, and
// nobody asks it in the site.
//
// People in the site ask an agent exactly while it is a runtime agent with
// a live runtime token, and it and its owner, if it has one, are active
// (Core's SiteChatOf): nothing is declared. A question to an mcp agent is
// refused mcp_agent, and to a runtime agent not hosted now
// agent_not_hosted. There is no me.site_chat any more (AIShie-Core #61):
// MCP lists no such tool, and a call of it is an unknown tool's.
//
// The agent runtime's four tools are REST's alone, and the agent_runtime
// service's (a site service of its own, beside the transcription
// service): agent and check_owner read an agent as the runtime hosts it,
// issue_token issues its one token, revoking the one before, and
// revoke_token revokes it. A replay of issue_token comes back without the
// token, which is shown once.

// An agent's hosting.
const (
	hostingRuntime = "runtime"
	hostingMCP     = "mcp"
)

// defaultRuntimeLabel labels a runtime token issued without a label.
const defaultRuntimeLabel = "agent runtime"

// maxRuntimeLabel bounds a runtime token's label, in bytes, as Core's does.
const maxRuntimeLabel = 200

// withoutHosting is the catalogue raw without the agent_runtime service's
// tools, as a Core from before an agent's hosting serves it.
func withoutHosting(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fakecore: the catalogue: %w", err)
	}
	tools, _ := doc["tools"].([]any)
	kept := tools[:0]
	for _, t := range tools {
		if tool, _ := t.(map[string]any); tool == nil || !strings.HasPrefix(fmt.Sprint(tool["name"]), scopeAgentRuntime+".") {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(tools) {
		return nil, errors.New("fakecore: the catalogue has no agent_runtime service to take out")
	}
	doc["tools"] = kept
	return json.Marshal(doc)
}

// runtimeTokenOf is the agent a's live runtime token, nil for none: one at
// most (Core's credential_one_runtime_token). Called with the lock held.
func (c *Core) runtimeTokenOf(a *actor) *credential {
	for _, cr := range c.tokens {
		if cr.actor == a && cr.issuedTo == scopeAgentRuntime && !cr.revoked() {
			return cr
		}
	}
	return nil
}

// askable reports whether people in the site may ask the agent a now
// (Core's SiteChatOf). Called with the lock held.
func (c *Core) askable(a *actor) bool {
	return a.kind == "agent" && a.hosting == hostingRuntime && a.active() && (a.owner == nil || a.owner.active()) &&
		c.runtimeTokenOf(a) != nil
}

// Core's refusals of a question to an agent that is not asked in the site.
var (
	errMCPAgent = precondition("that agent is never asked in the site: it is used from its owner's own tools, over MCP").
			with("reason", "mcp_agent")
	errAgentNotHosted = precondition("that agent is not running in the site just now: the site's agent runtime does not host it at "+
		"the moment; ask again later, or ask its owner").with("reason", "agent_not_hosted")
)

// notAskable is Core's rule of a new question, conversation_open's and
// conversation_ask's, after the addressing rule, never an answer's or a
// read's: an mcp agent is never asked in the site, and a runtime agent only
// while it is hosted (askable).
func (c *Core) notAskable(respondent *member) error {
	a := respondent.actor
	switch {
	case a.kind != "agent":
		return nil
	case a.hosting == hostingMCP:
		return errMCPAgent
	case !c.askable(a):
		return errAgentNotHosted
	}
	return nil
}

// The agent runtime's refusals of an agent it may not host.
var (
	errNoSuchAgent           = missing("no such agent")
	errRuntimeNotRuntimeHost = precondition("that agent is an mcp agent: its owner's own tools reach it over MCP, with tokens of the "+
		"owner's, and the site's agent runtime does not host it").with("reason", "not_runtime_hosted")
	errRuntimeAgentSuspended = precondition("that agent is suspended: it is hosted again once it is reactivated").with("reason", "agent_suspended")
	errRuntimeOwnerSuspended = precondition("that agent's owner is suspended: it is hosted again once they are reactivated").
					with("reason", "owner_suspended")
)

// hostRefusal is why the runtime may not host a now, nil when it may.
func hostRefusal(a *actor) *apiError {
	switch {
	case a.hosting != hostingRuntime:
		return errRuntimeNotRuntimeHost
	case !a.active():
		return errRuntimeAgentSuspended
	case a.owner != nil && !a.owner.active():
		return errRuntimeOwnerSuspended
	}
	return nil
}

// runtimeTokenView is the runtime's live token of an agent as the runtime
// may see it: never the secret.
type runtimeTokenView struct {
	CredentialID string     `json:"credential_id"`
	TokenPrefix  *string    `json:"token_prefix,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

// runtimeAgentView is an agent as agent_runtime.agent and check_owner show
// it.
type runtimeAgentView struct {
	AgentID      string            `json:"agent_id"`
	DisplayName  string            `json:"display_name"`
	Hosting      string            `json:"hosting"`
	Status       string            `json:"status"`
	OwnerActorID *string           `json:"owner_actor_id,omitempty"`
	OwnerName    *string           `json:"owner_name,omitempty"`
	OwnerStatus  *string           `json:"owner_status,omitempty"`
	LiveSeats    int               `json:"live_seats"`
	Hostable     bool              `json:"hostable"`
	Reason       *string           `json:"reason,omitempty"`
	RuntimeToken *runtimeTokenView `json:"runtime_token,omitempty"`
	SiteChat     bool              `json:"site_chat"`
}

// runtimeView is a as the runtime sees it at now. Called with the lock
// held.
func (c *Core) runtimeView(a *actor, now time.Time) runtimeAgentView {
	v := runtimeAgentView{AgentID: a.id, DisplayName: a.name, Hosting: a.hosting, Status: a.status, Hostable: true, SiteChat: c.askable(a)}
	if o := a.owner; o != nil {
		v.OwnerActorID, v.OwnerName, v.OwnerStatus = ptr(o.id), ptr(o.name), ptr(o.status)
	}
	for _, m := range c.memberList {
		if m.actor == a && m.status == statusActive && (m.expiresAt == nil || m.expiresAt.After(now)) {
			v.LiveSeats++
		}
	}
	if why := hostRefusal(a); why != nil {
		reason, _ := why.Details["reason"].(string)
		v.Hostable, v.Reason = false, &reason
	}
	if cr := c.runtimeTokenOf(a); cr != nil {
		v.RuntimeToken = &runtimeTokenView{CredentialID: cr.id, TokenPrefix: ptr(cr.prefix), CreatedAt: cr.createdAt}
		if cr.lastUsed != nil {
			v.RuntimeToken.LastUsedAt = ptr(*cr.lastUsed)
		}
	}
	return v
}

// agentByID is the agent of the id, in any case; nil for anyone else and
// nobody. Called with the lock held.
func (c *Core) agentByID(id string) *actor {
	a := c.actors[strings.ToLower(id)]
	if a == nil || a.kind != "agent" {
		return nil
	}
	return a
}

// issuedRuntimeToken is agent_runtime.issue_token's result; Token is
// shown once, and a replay comes back without it.
type issuedRuntimeToken struct {
	AgentID      string   `json:"agent_id"`
	CredentialID string   `json:"credential_id"`
	Token        string   `json:"token,omitempty"`
	TokenPrefix  string   `json:"token_prefix"`
	Replaced     []string `json:"replaced,omitempty"`
}

// issueRuntime issues the agent a its one runtime token from the service
// svc, labelled label, revoking the one before, and returns it and the
// credentials it revoked. Called with the lock held.
func (c *Core) issueRuntime(a, svc *actor, label string, now time.Time) (*credential, []string) {
	replaced := c.revokeRuntime(a, now)
	cr := c.issue(a, svc, label)
	cr.issuedTo = scopeAgentRuntime
	return cr, replaced
}

// revokeRuntime revokes a's runtime tokens, and returns their ids: none,
// an empty list. Called with the lock held.
func (c *Core) revokeRuntime(a *actor, now time.Time) []string {
	revoked := []string{}
	for _, cr := range c.tokens {
		if cr.actor == a && cr.issuedTo == scopeAgentRuntime && !cr.revoked() {
			at := now
			cr.revokedAt = &at
			revoked = append(revoked, cr.id)
		}
	}
	return revoked
}

// invokeRuntime carries out one of the agent runtime's four tools, called
// by the service caller. A write is an action of the service's, recorded
// and replayed under its key; one about no agent is never attempted.
// Called with the lock held.
func (c *Core) invokeRuntime(caller *actor, t *toolDef, raw []byte, key string, now time.Time) outcome {
	var in struct {
		ActorID string  `json:"actor_id"`
		AgentID string  `json:"agent_id"`
		Label   *string `json:"label"`
	}
	_ = json.Unmarshal(raw, &in)
	switch t.Name {
	case "agent_runtime.agent":
		a := c.agentByID(in.AgentID)
		if a == nil {
			return errorOutcome(errNoSuchAgent)
		}
		return executed(c.runtimeView(a, now))
	case "agent_runtime.check_owner":
		a := c.agentByID(in.AgentID)
		if a == nil || a.owner == nil || !strings.EqualFold(a.owner.id, in.ActorID) {
			return executed(map[string]any{"owns": false})
		}
		return executed(map[string]any{"owns": true, "agent": c.runtimeView(a, now)})
	}
	if err := checkKey(t, key); err != nil {
		return c.failure(err)
	}
	canonical, err := canonicalize(raw)
	if err != nil {
		return errorOutcome(invalid("%v", err))
	}
	hash := payloadHash(t.Name, canonical)
	if existing := c.keys[actorKey{caller.id, key}]; existing != nil {
		out, err := replay(existing, hash, "")
		if err != nil {
			return c.failure(err)
		}
		return out
	}
	a := c.agentByID(in.AgentID)
	if a == nil {
		return errorOutcome(errNoSuchAgent)
	}
	act := &action{id: newID(), actor: caller, actionType: t.Name, targetType: "actor", targetID: &a.id, payload: canonical, hash: hash,
		key: key, authz: autonomous, status: actExecuted, reviewState: reviewNone, createdAt: now}
	c.recordAction(act)
	failed := func(e *apiError) outcome {
		act.status, act.result = actFailed, errorResult(e)
		return outcome{Status: actFailed, ActionID: act.id, ReviewState: reviewNone, Error: e}
	}
	var res, shown json.RawMessage
	switch t.Name {
	case "agent_runtime.issue_token":
		label := defaultRuntimeLabel
		if in.Label != nil {
			label = strings.TrimSpace(*in.Label)
			if label == "" || len(label) > maxRuntimeLabel {
				return failed(invalid("label is 1 to %d characters", maxRuntimeLabel).with("field", "label"))
			}
		}
		if why := hostRefusal(a); why != nil {
			return failed(why)
		}
		cr, replaced := c.issueRuntime(a, caller, label, now)
		out := issuedRuntimeToken{AgentID: a.id, CredentialID: cr.id, TokenPrefix: cr.prefix, Replaced: replaced}
		res = mustJSON(out)
		out.Token = cr.token
		shown = mustJSON(out)
	case "agent_runtime.revoke_token":
		res = mustJSON(map[string]any{"agent_id": a.id, "revoked": c.revokeRuntime(a, now)})
		shown = res
	default:
		return errorOutcome(missing("no tool %s here", t.Name))
	}
	act.executedAt, act.result = &now, res
	return outcome{Status: actExecuted, ActionID: act.id, ReviewState: reviewNone, Result: shown}
}

// SiteChat reports whether people in the site may ask the agent actorID
// now: a runtime agent with a live runtime token, it and its owner active.
func (c *Core) SiteChat(actorID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.actors[actorID]
	return a != nil && c.askable(a)
}

// Hosting is the agent actorID's hosting, runtime or mcp; "" for anyone
// else.
func (c *Core) Hosting(actorID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[actorID]; a != nil {
		return a.hosting
	}
	return ""
}

// IssueRuntimeToken issues the runtime agent agentID its one token, as the
// site's agent runtime is issued it (agent_runtime.issue_token): the one
// before is revoked. Refused for an mcp agent, a suspended one, and one
// whose owner is suspended.
func (c *Core) IssueRuntimeToken(agentID string) (Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.actors[agentID]
	if a == nil || a.kind != "agent" {
		return Token{}, fmt.Errorf("fakecore: IssueRuntimeToken: no agent %s", agentID)
	}
	if why := hostRefusal(a); why != nil {
		return Token{}, refused("agent_runtime_issue_token", outcome{Status: actFailed, Error: why})
	}
	cr, _ := c.issueRuntime(a, c.serviceOf(scopeAgentRuntime), defaultRuntimeLabel, c.now())
	return Token{Token: cr.token, CredentialID: cr.id, Prefix: cr.prefix}, nil
}

// RevokeRuntimeToken revokes the agent agentID's runtime token, as the
// site's agent runtime does when its hosting ends, or its owner revoking
// it in Core (agent.revoke_credential): its next call is a 401, and
// people in the site no longer ask it. It returns the credentials
// revoked.
func (c *Core) RevokeRuntimeToken(agentID string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.actors[agentID]
	if a == nil || a.kind != "agent" {
		return nil, fmt.Errorf("fakecore: RevokeRuntimeToken: no agent %s", agentID)
	}
	return c.revokeRuntime(a, c.now()), nil
}

// RuntimeToken is the agent agentID's live runtime token, "" for none: the
// token the runtime under test was issued, for a test that calls Core as
// the agent.
func (c *Core) RuntimeToken(agentID string) Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.actors[agentID]; a != nil {
		if cr := c.runtimeTokenOf(a); cr != nil {
			return Token{Token: cr.token, CredentialID: cr.id, Prefix: cr.prefix}
		}
	}
	return Token{}
}

// RuntimeIssues counts the runtime tokens the agent agentID was ever
// issued, revoked ones included.
func (c *Core) RuntimeIssues(agentID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, cr := range c.tokens {
		if cr.actor != nil && cr.actor.id == agentID && cr.issuedTo == scopeAgentRuntime {
			n++
		}
	}
	return n
}
