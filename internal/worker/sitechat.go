package worker

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// The site's chat (Core's me.site_chat): Core takes conversations in the
// site for an agent only after the brain running it declares so, with the
// agent's own token, and only while that token is live. The runtime
// declares it once each time it starts running an agent, and never takes
// it back: a restart must not flap it, and the revocation of the token (a
// new one given, the agent deleted) takes it back in Core.

// siteChat is where an agent instance stands with its declaration.
type siteChat int

const (
	// siteChatPending: not declared yet; tried after me_get for an agent
	// with an owner, else once a seat of its answers, and again at each
	// read of its seats until it is declared or refused.
	siteChatPending siteChat = iota
	// siteChatDeclared: Core took it.
	siteChatDeclared
	// siteChatNone: nothing to declare: Core does not know the tool, or
	// refused the declaration, which a new start tries again.
	siteChatNone
)

// siteChatTimeout bounds one declaration, its retries included.
const siteChatTimeout = 10 * time.Second

// siteChatKey is a declaration's idempotency key: one of its own each
// time, since a replay would do nothing after the token that declared it
// before was revoked.
func siteChatKey() string { return "site-chat:" + uuid.NewString() }

// wantsSiteChat reports whether the agent takes conversations in the site
// by its owner: a hosted agent always has one; a YAML agent when Core names
// one.
func (a *Agent) wantsSiteChat() bool {
	return a.cfg.Hosted != nil || (a.me != nil && a.me.OwnerActorID != "")
}

// declareSiteChat sends me.site_chat {on: true} with the agent's token, once
// for this start: a Core whose catalogue does not offer the tool, or that
// refuses it as a tool it does not know, has nothing to declare, which is
// logged once per catalogue; a call Core did not answer is tried again at
// the next read of the agent's seats; a refusal is logged, and left until
// the agent starts again. Its error is a 401: the agent must stop.
func (a *Agent) declareSiteChat(ctx context.Context) error {
	if a.siteChat != siteChatPending {
		return nil
	}
	t, ok := a.cat.Tool(core.ToolSiteChat)
	if !ok {
		a.siteChat = siteChatNone
		a.s.noteNoSiteChat(a.cat.Hash(), a.cfg.Core.BaseURL)
		return nil
	}
	key := ""
	if t.Kind == core.KindWrite {
		key = siteChatKey()
	}
	// Bounded: a Core that keeps failing it must not hold the agent's
	// start, nor its reads of its seats.
	callCtx, cancel := context.WithTimeout(core.WithPriority(ctx, core.PriorityBackground), siteChatTimeout)
	defer cancel()
	env, err := a.client.SiteChat(callCtx, true, key)
	var pe *core.ProtocolError
	switch {
	case isUnauthenticated(err):
		return core.ErrUnauthenticated
	case errors.As(err, &pe):
		// A Core that serves the tool in its catalogue but not over its
		// transport: nothing to declare there.
		a.siteChat = siteChatNone
		a.log.Info("Core did not take me.site_chat as a tool it knows: there is no site chat to declare", "err", err)
	case err != nil:
		if ctx.Err() == nil {
			a.log.Warn("me.site_chat could not be sent; it is tried again at the next read of the seats", "err", err)
		}
	case env.Status == core.StatusExecuted:
		a.siteChat = siteChatDeclared
		a.log.Info("the agent is declared to take conversations in the site (me.site_chat)", "replayed", env.Replayed)
	case env.Code() == core.CodeNotFound:
		a.siteChat = siteChatNone
		a.log.Info("Core does not know me.site_chat: there is no site chat to declare")
	default:
		a.siteChat = siteChatNone
		a.log.Warn("Core refused me.site_chat: the agent takes no conversation in the site until it starts again",
			"status", env.Status, "code", env.Code(), "reason", env.Reason())
	}
	return nil
}

// noteNoSiteChat logs, once per catalogue, that a Core does not offer
// me.site_chat.
func (s *Supervisor) noteNoSiteChat(hash, baseURL string) {
	if _, seen := s.siteChatAbsent.LoadOrStore(hash, true); !seen {
		s.log.Info("Core's catalogue does not offer me.site_chat: its agents have no site chat to declare", "core", baseURL, "catalogue", hash)
	}
}
