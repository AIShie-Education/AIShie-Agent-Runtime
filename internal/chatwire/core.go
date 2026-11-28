package chatwire

import (
	"context"
	"fmt"
	"net/http"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// MCPInspector validates the exact token that will be put in the Runner
// request, over the same MCP transport the Runner uses. It does not use Core's
// conversation APIs: me_get authenticates the actor and me_memberships supplies
// current course/seat permissions.
type MCPInspector struct {
	HTTPClient *http.Client
}

func (i MCPInspector) Inspect(ctx context.Context, cfg config.Core, token string) (*CoreIdentity, error) {
	caller := core.NewMCPCaller(core.MCPOptions{
		BaseURL: cfg.BaseURL, Token: token, Protocol: cfg.MCPProtocol, HTTPClient: i.HTTPClient,
	})
	client := core.NewClient(caller)
	actor, err := client.Me(core.WithPriority(ctx, core.PriorityAnswer))
	if err != nil {
		return nil, fmt.Errorf("chatwire: me_get: %w", err)
	}
	memberships, err := client.Memberships(core.WithPriority(ctx, core.PriorityAnswer))
	if err != nil {
		return nil, fmt.Errorf("chatwire: me_memberships: %w", err)
	}
	return &CoreIdentity{Actor: *actor, Memberships: memberships, HasMemberships: true}, nil
}
