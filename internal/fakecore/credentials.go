package fakecore

import (
	"slices"
	"strings"
)

// An actor's tokens, as the test controls list them (Credentials). The
// runtime holds no token but a runtime agent's, which the agent_runtime
// service issues and revokes (hosting.go): it calls neither credential.list
// nor credential.revoke, and the fake carries out neither.

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
