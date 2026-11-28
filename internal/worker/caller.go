package worker

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/metrics"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ratelimit"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/version"
)

// DefaultCaller is the connection to Core the worker makes for an agent
// when Options.NewCaller is nil: MCP at the pinned revision, or REST, as
// core.transport says; every call waiting for the agent's rate limit (a
// bucket at ratelimit.CoreShare of assumed_core_rate_per_min and
// assumed_core_burst), then counted (m.CoreCaller); retried as
// core.Retrying does, with retry's options and onRateLimited told of every
// 429. client carries the calls.
func DefaultCaller(a *config.Agent, token string, cat *core.Catalogue, client *http.Client, m *metrics.Metrics,
	retry core.RetryOptions, onRateLimited func()) (core.Caller, error) {
	var transport core.Caller
	switch a.Core.Transport {
	case config.TransportREST:
		if cat == nil {
			return nil, errors.New("worker: the REST transport needs Core's catalogue")
		}
		transport = core.NewRESTCaller(core.RESTOptions{BaseURL: a.Core.BaseURL, Token: token, HTTPClient: client, Catalogue: cat})
	case config.TransportMCP, "":
		transport = core.NewMCPCaller(core.MCPOptions{BaseURL: a.Core.BaseURL, Token: token, Protocol: a.Core.MCPProtocol,
			HTTPClient: client, ClientVersion: version.Version})
	default:
		return nil, fmt.Errorf("worker: unknown core.transport %q", a.Core.Transport)
	}
	p := a.Polling
	bucket := ratelimit.New(ratelimit.CoreShare*float64(p.AssumedCoreRatePerMin), int(ratelimit.CoreShare*float64(p.AssumedCoreBurst)))
	counted := transport
	if m != nil {
		counted = m.CoreCaller(a.ID, transport)
	}
	if onRateLimited != nil {
		retry.OnRateLimited = func(_ time.Duration) { onRateLimited() }
	}
	return core.NewRetrying(core.Limited(counted, bucket), retry), nil
}
