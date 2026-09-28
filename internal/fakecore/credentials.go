package fakecore

import (
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The caller's own credentials, as Core's credential.list and
// credential.revoke answer them (Core's internal/tools/me.go): both are
// self-gated, so any active actor may call them, an agent among them, and
// a suspended one is denied (actor_not_active). The runtime calls them with
// an agent's own token to revoke a token of the agent's (D7): the one it
// is deleting, or the one a new token replaces.

// credentialView is one credential as credential.list shows it: never the
// secret. The fake's are all API tokens.
type credentialView struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	TokenPrefix string     `json:"token_prefix,omitempty"`
	Label       *string    `json:"label,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	IssuedByID  *string    `json:"issued_by_actor_id,omitempty"`
	IssuedBy    *string    `json:"issued_by_name,omitempty"`
}

// credentialsOf lists act's tokens as Core orders them: newest first, then
// by id. Called with the lock held.
func (c *Core) credentialsOf(act *actor) []*credential {
	var out []*credential
	for _, cr := range c.tokens {
		if act != nil && cr.actor == act {
			out = append(out, cr)
		}
	}
	slices.SortFunc(out, func(x, y *credential) int {
		if n := y.createdAt.Compare(x.createdAt); n != 0 {
			return n
		}
		return strings.Compare(x.id, y.id)
	})
	return out
}

func credentialList() *impl {
	return define(spec[emptyIn]{
		gate:    gate{self: true},
		resolve: func(*Core, *course, emptyIn) (target, error) { return target{typ: "credential"}, nil },
		query: func(c *Core, rc *readCtx, _ emptyIn) (any, error) {
			out := struct {
				Credentials []credentialView `json:"credentials"`
			}{Credentials: []credentialView{}}
			for _, cr := range c.credentialsOf(rc.actor) {
				v := credentialView{ID: cr.id, Kind: "api_token", TokenPrefix: cr.prefix, CreatedAt: cr.createdAt}
				if cr.label != "" {
					v.Label = ptr(cr.label)
				}
				if cr.lastUsed != nil {
					v.LastUsedAt = ptr(*cr.lastUsed)
				}
				if cr.revokedAt != nil {
					v.RevokedAt = ptr(*cr.revokedAt)
				}
				if cr.issuer != nil {
					v.IssuedByID, v.IssuedBy = ptr(cr.issuer.id), ptr(cr.issuer.name)
				}
				out.Credentials = append(out.Credentials, v)
			}
			return out, nil
		},
	})
}

type revokeIn struct {
	CredentialID uuid.UUID `json:"credential_id"`
}

func credentialRevoke() *impl {
	return define(spec[revokeIn]{
		gate: gate{self: true},
		resolve: func(_ *Core, _ *course, in revokeIn) (target, error) {
			id := in.CredentialID.String()
			return target{typ: "credential", id: &id}, nil
		},
		execute: func(c *Core, ec *execCtx, in revokeIn) (any, error) {
			id := in.CredentialID.String()
			for _, cr := range c.tokens {
				if cr.id == id && cr.actor == ec.actor && !cr.revoked() {
					at := ec.now
					cr.revokedAt = &at
					return struct {
						OK bool `json:"ok"`
					}{true}, nil
				}
			}
			// Someone else's, unknown, or already revoked: all the same
			// to the caller.
			return nil, missing("no such live credential on this account")
		},
	})
}
