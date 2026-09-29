package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/probe"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// The API asks Core about a token, with the token, at CORE_BASE_URL and
// nowhere else: what it is (inspect, connect, a new token), and, with an
// agent's own token, to revoke a token of the agent's (D7).

// Deadlines of what a request asks of Core.
const (
	// inspectTimeout bounds inspecting a token: the catalogue, me_get,
	// me_memberships and credential_list.
	inspectTimeout = 20 * time.Second
	// revokeTimeout bounds revoking a token in Core.
	revokeTimeout = 15 * time.Second
	// catalogueTTL is how long Core's catalogue is kept.
	catalogueTTL = 10 * time.Minute
)

// catalogueCache is Core's catalogue, fetched once every catalogueTTL:
// what says whether Core's me_get names owners.
type catalogueCache struct {
	mu  sync.Mutex
	cat *core.Catalogue
	at  time.Time
}

// catalogue is Core's catalogue, fetched when it is not kept or is older
// than catalogueTTL, one fetch at a time.
func (s *Server) catalogue(ctx context.Context) (*core.Catalogue, error) {
	s.cats.mu.Lock()
	defer s.cats.mu.Unlock()
	now := s.o.Now()
	if s.cats.cat != nil && now.Sub(s.cats.at) < catalogueTTL {
		return s.cats.cat, nil
	}
	cat, err := core.FetchCatalogue(ctx, s.coreHTTP, s.o.CoreBaseURL)
	if err != nil {
		return nil, err
	}
	s.cats.cat, s.cats.at = cat, now
	return cat, nil
}

// inspected is a token Core took: the token, and what Core said of it.
type inspected struct {
	token  string
	prefix string
	hint   string
	client *core.Client
	me     *core.Actor
	seats  []core.Membership
}

// errTokenMalformed is a token not of Core's shape, which Core is not
// asked about.
var errTokenMalformed = Error{Code: CodeInvalidArgument, Reason: ReasonTokenMalformed,
	Message: "the token is not an agent token of AIshie's (ais_ and a prefix of 12 characters)"}

// tokenErrors are Inspect's refusals, as the API answers them.
var tokenErrors = map[string]Error{
	probe.ReasonTokenRefused:    {Code: CodeFailedPrecondition, Message: "Core refused the token: it was revoked or has expired"},
	probe.ReasonCoreUnavailable: {Code: CodeUnavailable, Message: "Core could not be reached to check the token"},
	probe.ReasonTokenNotAgent:   {Code: CodeFailedPrecondition, Message: "the token belongs to a person, not an agent"},
	probe.ReasonAgentSuspended:  {Code: CodeFailedPrecondition, Message: "the agent is suspended in Core"},
	probe.ReasonTokenOtherAgent: {Code: CodeFailedPrecondition, Message: "the token belongs to another agent"},
	probe.ReasonCoreTooOld:      {Code: CodeFailedPrecondition, Message: "this Core does not say who owns an agent"},
	probe.ReasonAgentUnowned:    {Code: CodeForbidden, Message: "nobody owns the agent in Core"},
	probe.ReasonNotOwner:        {Code: CodeForbidden, Message: "the agent belongs to someone else in Core"},
}

// inspectToken asks Core what token is and holds it to want (probe.Inspect):
// a token not of Core's shape is not sent to Core at all. The error is
// the refusal to answer.
func (s *Server) inspectToken(ctx context.Context, token string, want probe.Want) (*inspected, *Error) {
	if !probe.IsToken(token) {
		e := errTokenMalformed
		return nil, &e
	}
	cat, err := s.catalogue(ctx)
	if err != nil {
		s.o.Log.Warn("Core's catalogue could not be read", "err", err)
		e := tokenErrors[probe.ReasonCoreUnavailable]
		e.Reason = probe.ReasonCoreUnavailable
		return nil, &e
	}
	c := probe.NewClient(s.o.CoreBaseURL, token, s.coreHTTP)
	ins, err := probe.Inspect(ctx, c, cat, want)
	if err != nil {
		return nil, s.tokenRefusal(err)
	}
	return &inspected{token: token, prefix: probe.Prefix(token), hint: vault.Hint(store.SecretCoreToken, token), client: c,
		me: ins.Me, seats: ins.Memberships}, nil
}

// recheck asks Core again, with ins's token, whether it is still what
// inspectToken found it to be, held to want (probe.Check: me_get alone),
// for a request about to act on it; the error is the refusal to answer.
func (s *Server) recheck(ctx context.Context, ins *inspected, want probe.Want) *Error {
	cat, err := s.catalogue(ctx)
	if err != nil {
		s.o.Log.Warn("Core's catalogue could not be read", "err", err)
		e := tokenErrors[probe.ReasonCoreUnavailable]
		e.Reason = probe.ReasonCoreUnavailable
		return &e
	}
	if _, err := probe.Check(ctx, ins.client, cat, want); err != nil {
		return s.tokenRefusal(err)
	}
	return nil
}

// tokenRefusal is probe's refusal of a token (Inspect, Check) as the API
// answers it.
func (s *Server) tokenRefusal(err error) *Error {
	var pe *probe.Error
	reason := probe.ReasonCoreUnavailable
	if errors.As(err, &pe) {
		reason = pe.Reason
	}
	if reason == probe.ReasonCoreUnavailable {
		s.o.Log.Warn("Core could not be reached to inspect a token", "err", err)
	}
	e := tokenErrors[reason]
	e.Reason = reason
	return &e
}

// wantOf is the agent a request means: its core_actor_id when given (it
// must be a UUID), and the caller as the agent's owner.
func wantOf(coreActorID string, c *Caller) (probe.Want, *Error) {
	w := probe.Want{Owner: c.ActorID}
	if coreActorID != "" {
		id, err := uuid.Parse(coreActorID)
		if err != nil {
			return w, &Error{Code: CodeInvalidArgument, Reason: ReasonInvalidField, Message: "core_actor_id is not a UUID",
				Details: map[string]any{"field": "/core_actor_id"}}
		}
		w.ActorID = id.String()
	}
	return w, nil
}

// recentUse is how recently another token of an agent's must have been
// used for its agent to be taken to run elsewhere now: Core notes a
// token's use at most once a minute, and an agent a runtime runs calls
// Core far more often than this.
const recentUse = 15 * time.Minute

// maxOtherTokens bounds the tokens OtherTokens lists.
const maxOtherTokens = 20

// OtherTokens are an agent's other live API tokens, as Core lists them to
// the token being connected (the one-brain rule: an agent has one brain at
// a time). InUse says one of them was used within WindowSeconds: the agent
// likely runs elsewhere now, and the front end warns its owner. Tokens
// holds at most 20, the most recently used first; the token being
// connected, and the one this runtime holds for the agent, are not among
// them.
type OtherTokens struct {
	InUse         bool         `json:"in_use"`
	WindowSeconds int          `json:"window_seconds"`
	Tokens        []OtherToken `json:"tokens"`
}

// OtherToken is one of an agent's other live tokens: its public prefix
// and label, when it was made and last used (null for never), when it
// expires (null for never), and whether its last use is recent.
type OtherToken struct {
	Prefix     string     `json:"prefix"`
	Label      *string    `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	Recent     bool       `json:"recent"`
}

// otherTokens lists the agent's other live tokens with ins's token
// (credential_list), leaving out the prefixes of except; nil when Core
// would not list them, which fails nothing.
func (s *Server) otherTokens(ctx context.Context, ins *inspected, except ...string) *OtherTokens {
	creds, err := ins.client.Credentials(ctx)
	if err != nil {
		s.o.Log.Warn("an agent's tokens could not be listed", "agent_actor", ins.me.ID, "err", err)
		return nil
	}
	now := s.o.Now()
	out := &OtherTokens{WindowSeconds: int(recentUse / time.Second), Tokens: []OtherToken{}}
	for _, cr := range creds {
		if cr.Kind != core.CredentialAPIToken || !cr.Live(now) || cr.TokenPrefix == ins.prefix || slices.Contains(except, cr.TokenPrefix) {
			continue
		}
		t := OtherToken{Prefix: cr.TokenPrefix, CreatedAt: cr.CreatedAt.UTC(), LastUsedAt: utcPtr(cr.LastUsedAt), ExpiresAt: utcPtr(cr.ExpiresAt)}
		if cr.Label != "" {
			l := cr.Label
			t.Label = &l
		}
		t.Recent = cr.LastUsedAt != nil && now.Sub(*cr.LastUsedAt) < recentUse
		out.InUse = out.InUse || t.Recent
		out.Tokens = append(out.Tokens, t)
	}
	slices.SortStableFunc(out.Tokens, func(x, y OtherToken) int {
		switch {
		case x.LastUsedAt == nil && y.LastUsedAt == nil:
			return y.CreatedAt.Compare(x.CreatedAt)
		case x.LastUsedAt == nil:
			return 1
		case y.LastUsedAt == nil:
			return -1
		}
		return y.LastUsedAt.Compare(*x.LastUsedAt)
	})
	if len(out.Tokens) > maxOtherTokens {
		out.Tokens = out.Tokens[:maxOtherTokens]
	}
	return out
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// coreClient is the client calls to Core go through, bounded by
// core.DefaultTimeout when it has none of its own.
func coreClient(c *http.Client) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	if c.Timeout > 0 {
		return c
	}
	cp := *c
	cp.Timeout = core.DefaultTimeout
	return &cp
}

// sameActor reports whether two actor ids are one, in any case.
func sameActor(x, y string) bool { return strings.EqualFold(x, y) }
