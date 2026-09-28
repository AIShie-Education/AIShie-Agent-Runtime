package fakecore

import (
	"encoding/json"
	"errors"
	"fmt"
)

// me.site_chat, the declaration Core takes from the brain running an agent,
// with the agent's token, that it takes conversations in the site: a
// self-gated write. Core keeps which credential declared, and the
// declaration holds while that credential is live, the agent active and
// its owner, if it has one, active; nobody may open a conversation with an
// agent, or ask in one, while it does not (agent_answers_elsewhere). The
// fake keeps each actor's last declaration, and holds it while the actor
// has a live token at all: it does not tell one token of an agent's from
// another. Core has had it since the runtime's pin moved to 169cf50;
// Options.WithoutSiteChat answers as a Core from before it, where nobody
// is refused for it.

// withoutSiteChat is the catalogue raw without me.site_chat.
func withoutSiteChat(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fakecore: the catalogue: %w", err)
	}
	tools, _ := doc["tools"].([]any)
	kept := tools[:0]
	for _, t := range tools {
		if tool, _ := t.(map[string]any); tool == nil || tool["name"] != "me.site_chat" {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(tools) {
		return nil, errors.New("fakecore: the catalogue has no me.site_chat to take out")
	}
	doc["tools"] = kept
	return json.Marshal(doc)
}

type siteChatIn struct {
	On bool `json:"on"`
}

func meSiteChat() *impl {
	return define(spec[siteChatIn]{
		gate:    gate{self: true},
		resolve: func(*Core, *course, siteChatIn) (target, error) { return target{typ: "actor"}, nil },
		execute: func(c *Core, ec *execCtx, in siteChatIn) (any, error) {
			c.siteChat[ec.actor.id] = in.On
			return siteChatIn{On: in.On}, nil
		},
	})
}

// SiteChat reports whether the actor's last me.site_chat declared that it
// takes conversations in the site.
func (c *Core) SiteChat(actorID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.siteChat[actorID]
}

// DeclareSiteChat declares, as the brain running the agent actorID does
// with its token (me.site_chat), that it takes conversations in the site:
// what an earlier run of the runtime left, for a test that asks the agent
// before the runtime under test starts.
func (c *Core) DeclareSiteChat(actorID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.actors[actorID]
	if a == nil || a.kind != "agent" {
		return fmt.Errorf("fakecore: DeclareSiteChat: no agent %s", actorID)
	}
	c.siteChat[actorID] = true
	return nil
}

// errAnswersElsewhere is Core's refusal of a question to an agent that
// takes no conversations in the site.
var errAnswersElsewhere = precondition("that agent takes no conversations in the site: it is operated from an external tool, and acts there").
	with("reason", "agent_answers_elsewhere")

// answersElsewhere is Core's rule of a new question, conversation_open's
// and conversation_ask's, never an answer's or a read's: an agent is asked
// only while its declaration holds (SiteChatOf).
func (c *Core) answersElsewhere(respondent *member) error {
	a := respondent.actor
	if a.kind != "agent" || c.opts.WithoutSiteChat || c.takesSiteChat(a) {
		return nil
	}
	return errAnswersElsewhere
}

// takesSiteChat reports whether the agent a takes conversations in the
// site now: it declared so, it has a live token, and it and its owner, if
// it has one, are active (Core's SiteChatOf).
func (c *Core) takesSiteChat(a *actor) bool {
	return c.siteChat[a.id] && a.active() && (a.owner == nil || a.owner.active()) && c.liveToken(a)
}

// liveToken reports whether the actor has a token that is not revoked.
func (c *Core) liveToken(a *actor) bool {
	for _, cr := range c.tokens {
		if cr.actor == a && !cr.revoked() {
			return true
		}
	}
	return false
}
