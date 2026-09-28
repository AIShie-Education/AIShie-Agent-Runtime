package fakecore

import (
	"encoding/json"
	"fmt"
)

// me.site_chat, the declaration a newer Core takes from the brain running
// an agent, with the agent's token, that it takes conversations in the
// site. The Core the runtime is pinned to does not have it: the fake
// offers it only with Options.SiteChat, as a self-gated write, and keeps
// each actor's last declaration (SiteChat), not the token's life.

// siteChatTool is me.site_chat as the fake describes it in its catalogue.
var siteChatTool = map[string]any{
	"name": "me.site_chat", "kind": "write", "method": "POST", "path": "/v1/me/site-chat",
	"description": "Declare whether the brain running you, with this token, takes conversations in the site: while it does, " +
		"and this token is live, people may talk to you there.",
	"input_schema": map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"on"},
		"properties": map[string]any{"on": map[string]any{"type": "boolean", "description": "true to take them, false to stop"}},
	},
	"output_schema": map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"on"},
		"properties": map[string]any{"on": map[string]any{"type": "boolean"}},
	},
}

// withSiteChat is the catalogue raw with me.site_chat added.
func withSiteChat(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fakecore: the catalogue: %w", err)
	}
	tools, _ := doc["tools"].([]any)
	doc["tools"] = append(tools, siteChatTool)
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
