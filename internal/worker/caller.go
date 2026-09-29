package worker

import (
	"context"
	"encoding/json"
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

// SnapshotCatalogueHash is the hash of the Core catalogue this runtime was
// built and tested against (internal/core/testdata/catalogue.json). An
// agent whose Core serves another is still run, if the tools it relies on
// are there (toolset.CheckCatalogue), and a warning says so.
const SnapshotCatalogueHash = "4a086f27445252b33a70b2d2d9f2193c41f49619ad30cd768ce05c998349dc55"

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

// authChecked reads Core's other way of saying 401 as one: over MCP, a
// call made with the token of an actor that no longer exists comes back as
// an envelope of status error and code unauthenticated, where REST answers
// HTTP 401. Either stops the agent.
type authChecked struct {
	next core.Caller
}

func (c authChecked) Call(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
	env, err := c.next.Call(ctx, tool, args)
	if err == nil && env != nil && env.Status == core.StatusError && env.Code() == core.CodeUnauthenticated {
		return nil, core.ErrUnauthenticated
	}
	return env, err
}

// isUnauthenticated reports whether err is Core refusing the agent's
// token, however it came.
func isUnauthenticated(err error) bool {
	if errors.Is(err, core.ErrUnauthenticated) {
		return true
	}
	var ee *core.EnvelopeError
	return errors.As(err, &ee) && ee.Envelope.Code() == core.CodeUnauthenticated
}
