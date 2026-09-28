// Package probe is what the runtime asks of Core and of a model provider on
// someone's behalf, outside an agent's run: what an agent's token is and
// whose (Inspect), revoking a token of an agent's with a token of its own
// (RevokeToken), and trying a model's key with one call (TryModel). The
// API does it for the people who host agents; check --live tries each
// agent's model with TryModel for the operator.
//
// A token is used for as long as the call that brought it, and never
// written anywhere: not in an error, a log line, or anything returned.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version"
)

// tokenRe is the shape of Core's API tokens: ais_, a public prefix of 12
// characters of base32 in lower case, _, and the secret (Core's
// auth.NewToken writes 43 characters of it).
var tokenRe = regexp.MustCompile(`^ais_([a-z2-7]{12})_[A-Za-z0-9_-]{32,128}$`)

// IsToken reports whether token has the shape of Core's API tokens, which
// is all that is checked of it before Core is asked.
func IsToken(token string) bool { return tokenRe.MatchString(token) }

// Prefix is a token's public prefix, as Core lists it (credential_list's
// token_prefix): the 12 characters after ais_; "" for a string not of a
// token's shape.
func Prefix(token string) string {
	if m := tokenRe.FindStringSubmatch(token); m != nil {
		return m[1]
	}
	return ""
}

// HintPrefix is the public prefix a token's hint (vault.Hint: ais_ and the
// prefix, then …) shows; "" for a hint that shows none.
func HintPrefix(hint string) string {
	rest, ok := strings.CutPrefix(hint, "ais_")
	if !ok || len(rest) < 12 {
		return ""
	}
	p := rest[:12]
	for _, c := range p {
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return ""
		}
	}
	return p
}

// NewClient is a client of Core at baseURL on token, over MCP, as the
// worker connects an agent but with no retry and no rate limit of its own:
// what it asks, it asks once, within its caller's deadline. Core's other
// way of saying 401 over MCP, an envelope of status error and code
// unauthenticated, is read as one. hc carries the calls; it follows no
// redirect.
func NewClient(baseURL, token string, hc *http.Client) *core.Client {
	return core.NewClient(authChecked{core.NewMCPCaller(core.MCPOptions{BaseURL: baseURL, Token: token, HTTPClient: hc,
		ClientVersion: version.Version})})
}

type authChecked struct{ next core.Caller }

func (c authChecked) Call(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
	env, err := c.next.Call(ctx, tool, args)
	if err == nil && env != nil && env.Status == core.StatusError && env.Code() == core.CodeUnauthenticated {
		return nil, core.ErrUnauthenticated
	}
	return env, err
}

// Why Inspect refuses a token: the API contract's reasons (§2.3).
const (
	// ReasonTokenRefused is Core's 401: the token is revoked, expired or
	// unknown.
	ReasonTokenRefused = "token_refused"
	// ReasonCoreUnavailable is a Core that could not be reached, answered
	// 5xx or 429, or did not answer in time.
	ReasonCoreUnavailable = "core_unavailable"
	// ReasonTokenNotAgent is a token of a person's, not an agent's.
	ReasonTokenNotAgent = "token_not_agent"
	// ReasonAgentSuspended is an agent suspended in Core: every call it
	// makes is denied, me_get's among them.
	ReasonAgentSuspended = "agent_suspended"
	// ReasonTokenOtherAgent is a token of another agent than the one meant.
	ReasonTokenOtherAgent = "token_other_agent"
	// ReasonCoreTooOld is a Core whose me_get does not say who owns an
	// agent (before Core's C1).
	ReasonCoreTooOld = "core_too_old"
	// ReasonAgentUnowned is an agent nobody owns in Core.
	ReasonAgentUnowned = "agent_unowned"
	// ReasonNotOwner is an agent someone else owns.
	ReasonNotOwner = "not_owner"
)

// Error is a token Inspect refused, and why: Reason, one of the reasons
// above. Err, when set, is what Core or the network said, for the logs;
// it holds no token.
type Error struct {
	Reason string
	Err    error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return "probe: " + e.Reason + ": " + e.Err.Error()
	}
	return "probe: " + e.Reason
}

func (e *Error) Unwrap() error { return e.Err }

// Want is what the token must be, beside an agent's own, active and owned:
// the agent the caller means (ActorID, when set) and its owner (Owner).
type Want struct {
	ActorID string
	Owner   string
}

// Inspection is what Core says of a token that passed: its actor, and its
// seats.
type Inspection struct {
	Me          *core.Actor
	Memberships []core.Membership
}

// Inspect asks Core what c's token is (me_get, then me_memberships) and
// refuses it, in this order (the API contract, §5.5): Core refused it
// (token_refused); Core could not be reached (core_unavailable); it is a
// person's (token_not_agent); its agent is suspended (agent_suspended,
// which is also what me_get's denial of a suspended actor says); it is
// another agent's than want.ActorID (token_other_agent); Core names no
// owner, which a Core whose catalogue cat does not describe owners cannot
// (core_too_old), and a newer one means nobody owns it (agent_unowned);
// or Core names another owner than want.Owner (not_owner). Actor ids are
// compared in any case. me_memberships is read only for a token that
// passes.
func Inspect(ctx context.Context, c *core.Client, cat *core.Catalogue, want Want) (*Inspection, error) {
	me, err := c.Me(ctx)
	if err != nil {
		return nil, classify(err)
	}
	switch {
	case me.Kind != core.KindAgent:
		return nil, &Error{Reason: ReasonTokenNotAgent}
	case me.Status != core.StatusActive:
		return nil, &Error{Reason: ReasonAgentSuspended}
	case want.ActorID != "" && !strings.EqualFold(want.ActorID, me.ID):
		return nil, &Error{Reason: ReasonTokenOtherAgent}
	case me.OwnerActorID == "" && !cat.MeGetNamesOwners():
		return nil, &Error{Reason: ReasonCoreTooOld}
	case me.OwnerActorID == "":
		return nil, &Error{Reason: ReasonAgentUnowned}
	case !strings.EqualFold(me.OwnerActorID, want.Owner):
		return nil, &Error{Reason: ReasonNotOwner}
	}
	me.ID, me.OwnerActorID = strings.ToLower(me.ID), strings.ToLower(me.OwnerActorID)
	ms, err := c.Memberships(ctx)
	if err != nil {
		return nil, classify(err)
	}
	return &Inspection{Me: me, Memberships: ms}, nil
}

// reasonActorNotActive is the reason Core's authorization gives for a
// call of an actor that is not active.
const reasonActorNotActive = "actor_not_active"

// classify is a call to Core that failed, as a reason: 401, a suspended
// actor, or Core not answering as it should.
func classify(err error) *Error {
	var ee *core.EnvelopeError
	switch {
	case errors.Is(err, core.ErrUnauthenticated):
		return &Error{Reason: ReasonTokenRefused}
	case errors.As(err, &ee) && ee.Envelope.Status == core.StatusDenied && ee.Envelope.Reason() == reasonActorNotActive:
		return &Error{Reason: ReasonAgentSuspended}
	}
	return &Error{Reason: ReasonCoreUnavailable, Err: err}
}

// What became of a token's revocation (the API contract's Revocation).
const (
	// Revoked: Core revoked it now.
	Revoked = "revoked"
	// AlreadyInvalid: it was dead already: revoked, expired, or unknown.
	AlreadyInvalid = "already_invalid"
	// Failed: it may still work; Problem says why.
	Failed = "failed"
	// NotAttempted: nothing was asked of Core.
	NotAttempted = "not_attempted"
)

// Why a revocation failed.
const (
	// ProblemAgentSuspended: Core denies a suspended agent's calls, these
	// among them; its owner revokes the token instead.
	ProblemAgentSuspended = "agent_suspended"
	// ProblemCoreUnavailable: Core could not be reached, or did not answer
	// in time.
	ProblemCoreUnavailable = "core_unavailable"
	// ProblemCoreRefused: Core answered otherwise than the runtime expects.
	ProblemCoreRefused = "core_refused"
)

// Revocation is what RevokeToken did.
type Revocation struct {
	// Outcome is Revoked, AlreadyInvalid, Failed or NotAttempted.
	Outcome string
	// Problem is why a revocation Failed, "" otherwise.
	Problem string
	// CredentialID is the credential revoked, when one was found.
	CredentialID string
}

// RevokeToken revokes, in Core, the live API token whose public prefix is
// prefix, with c: a client on a token of the same agent, the token itself
// or one that replaces it (the API contract, §7.3). credential_list finds
// it: the first credential of kind api_token with that prefix, neither
// revoked nor expired at now; with none, the token is dead already. Then
// credential_revoke revokes it, under aishie-revoke:<id>. Core refusing
// c's token (401) means the token was dead already, when it is the token
// itself; so does credential_revoke's failed not_found. A suspended agent
// is denied both calls; a Core that cannot be reached, or answers
// otherwise, leaves the token as it was.
func RevokeToken(ctx context.Context, c *core.Client, prefix string, now time.Time) Revocation {
	if prefix == "" {
		return Revocation{Outcome: Failed, Problem: ProblemCoreRefused}
	}
	creds, err := c.Credentials(ctx)
	if err != nil {
		return failedRevocation(err)
	}
	id := ""
	for _, cr := range creds {
		if cr.Kind == core.CredentialAPIToken && cr.TokenPrefix == prefix && cr.Live(now) {
			id = cr.ID
			break
		}
	}
	if id == "" {
		return Revocation{Outcome: AlreadyInvalid}
	}
	env, err := c.RevokeCredential(ctx, id)
	switch {
	case err != nil:
		r := failedRevocation(err)
		r.CredentialID = id
		return r
	case env.Status == core.StatusExecuted:
		return Revocation{Outcome: Revoked, CredentialID: id}
	case env.Status == core.StatusFailed && env.Code() == core.CodeNotFound:
		return Revocation{Outcome: AlreadyInvalid, CredentialID: id}
	case env.Status == core.StatusDenied && env.Reason() == reasonActorNotActive:
		return Revocation{Outcome: Failed, Problem: ProblemAgentSuspended, CredentialID: id}
	}
	return Revocation{Outcome: Failed, Problem: ProblemCoreRefused, CredentialID: id}
}

// failedRevocation is a call of RevokeToken's that Core did not carry out.
func failedRevocation(err error) Revocation {
	var ee *core.EnvelopeError
	var tr *core.TransientError
	var rl *core.RateLimitedError
	switch {
	case errors.Is(err, core.ErrUnauthenticated):
		return Revocation{Outcome: AlreadyInvalid}
	case errors.As(err, &ee) && ee.Envelope.Status == core.StatusDenied && ee.Envelope.Reason() == reasonActorNotActive:
		return Revocation{Outcome: Failed, Problem: ProblemAgentSuspended}
	case errors.As(err, &ee):
		return Revocation{Outcome: Failed, Problem: ProblemCoreRefused}
	case errors.As(err, &tr), errors.As(err, &rl), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return Revocation{Outcome: Failed, Problem: ProblemCoreUnavailable}
	}
	var pe *core.ProtocolError
	if errors.As(err, &pe) {
		return Revocation{Outcome: Failed, Problem: ProblemCoreRefused}
	}
	return Revocation{Outcome: Failed, Problem: ProblemCoreUnavailable}
}

// Unavailable reports whether err is Core not being reachable, as Inspect
// and RevokeToken tell it: for a caller that asks Core anything else.
func Unavailable(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Reason == ReasonCoreUnavailable
}

// String says what r is, for a log line: never a token.
func (r Revocation) String() string {
	if r.Problem != "" {
		return fmt.Sprintf("%s (%s)", r.Outcome, r.Problem)
	}
	return r.Outcome
}
