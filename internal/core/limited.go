package core

import (
	"context"
	"encoding/json"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/ratelimit"
)

// Limited is next with every call waiting first for a token of b, at the
// call's priority (PriorityOf): answers, then polling, then events and
// seats (§7.3). The bucket is the agent's own, set below Core's limit
// (ratelimit.CoreShare), so that the runtime waits where Core would refuse.
// A call whose context ends while it waits is not made, and returns the
// context's error. A nil b limits nothing.
//
// Under Retrying, each attempt waits for its own token: Core counts every
// request. A call that waits for news (WithWait) takes one token, as Core
// counts it one call however long it waits, and holds none while it waits:
// the token is spent before the call is made. A best-effort call
// (WithBestEffort), which Core does not count against the actor's limit,
// takes no token and waits for none: its caller keeps it to Core's own
// limit on it.
func Limited(next Caller, b *ratelimit.Bucket) Caller {
	if b == nil {
		return next
	}
	return &limited{next: next, b: b}
}

type limited struct {
	next Caller
	b    *ratelimit.Bucket
}

func (l *limited) Call(ctx context.Context, tool string, args json.RawMessage) (*Envelope, error) {
	if !BestEffort(ctx) {
		if err := l.b.Wait(ctx, int(PriorityOf(ctx))); err != nil {
			return nil, err
		}
	}
	return l.next.Call(ctx, tool, args)
}
