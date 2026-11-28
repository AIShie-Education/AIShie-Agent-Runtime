// Package metrics contains the control plane's Prometheus collectors. Labels
// hold ids, names and codes, never message text, keys or tool arguments.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/provider"
)

// Metrics are the runtime's non-Media collectors, registered on one registry.
type Metrics struct {
	AnswerLatency   *prometheus.HistogramVec
	Answers         *prometheus.CounterVec
	LLMCalls        *prometheus.CounterVec
	LLMTokens       *prometheus.CounterVec
	LLMCost         *prometheus.CounterVec
	CoreCalls       *prometheus.CounterVec
	ToolWrites      *prometheus.CounterVec
	BudgetExhausted *prometheus.CounterVec
	LeaseTakeovers  prometheus.Counter
	AgentStates     *prometheus.GaugeVec
	TokensIssued    prometheus.Counter
	presence        *presence
}

// New makes and registers the collectors.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		AnswerLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "answer_latency_seconds", Help: "Answer latency by stage: notice and answer.",
			Buckets: []float64{0.5, 1, 2, 3, 5, 8, 10, 13, 20, 30, 45, 60, 90, 120, 300},
		}, []string{"stage"}),
		Answers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "answers_total", Help: "Claimed questions, by outcome.",
		}, []string{"outcome"}),
		LLMCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_calls_total", Help: "Model runs, by adapter, model and stop reason.",
		}, []string{"adapter", "model", "stop"}),
		LLMTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_tokens_total", Help: "Tokens, by adapter, model and kind.",
		}, []string{"adapter", "model", "kind"}),
		LLMCost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_cost_usd_total", Help: "Cost of model calls in US dollars, by key source.",
		}, []string{"key_source"}),
		CoreCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "core_calls_total", Help: "Calls to Core, by tool, status and error code.",
		}, []string{"tool", "status", "error_code"}),
		ToolWrites: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tool_writes_total", Help: "Model writes through Core, by tool and outcome.",
		}, []string{"tool", "outcome"}),
		BudgetExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "budget_exhausted_total", Help: "Budgets and quotas reached, by which.",
		}, []string{"budget"}),
		LeaseTakeovers: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lease_takeovers_total", Help: "Agents taken over from a worker whose lease lapsed.",
		}),
		TokensIssued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "agent_tokens_issued_total", Help: "Agent tokens Core issued this worker.",
		}),
		AgentStates: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "agents", Help: "Agents this worker runs, by state.",
		}, []string{"state"}),
		presence: &presence{last: map[string]time.Time{}},
	}
	reg.MustRegister(m.AnswerLatency, m.Answers, m.LLMCalls, m.LLMTokens, m.LLMCost, m.CoreCalls,
		m.ToolWrites, m.BudgetExhausted, m.LeaseTakeovers, m.TokensIssued, m.AgentStates, m.presence)
	return m
}

// ObserveLLM counts one completed runner request's aggregate usage.
func (m *Metrics) ObserveLLM(adapter, model, stop string, usage provider.Usage, err error, costUSD float64, keySource string) {
	if err != nil {
		m.LLMCalls.WithLabelValues(adapter, model, "error").Inc()
		return
	}
	m.LLMCalls.WithLabelValues(adapter, model, stop).Inc()
	for kind, n := range map[string]int64{
		"input": usage.Input, "cache_read": usage.CacheRead, "cache_write": usage.CacheWrite,
		"output": usage.Output, "reasoning": usage.Reasoning,
	} {
		if n > 0 {
			m.LLMTokens.WithLabelValues(adapter, model, kind).Add(float64(n))
		}
	}
	if costUSD > 0 {
		m.LLMCost.WithLabelValues(keySource).Add(costUSD)
	}
}

// Seen records that agentID called Core now.
func (m *Metrics) Seen(agentID string) { m.presence.seen(agentID, time.Now()) }

// Forget stops reporting agentID's presence gap.
func (m *Metrics) Forget(agentID string) { m.presence.forget(agentID) }

// CoreCaller wraps next so each call is counted and marks agentID present.
func (m *Metrics) CoreCaller(agentID string, next core.Caller) core.Caller {
	return &countingCaller{m: m, agent: agentID, next: next}
}

type countingCaller struct {
	m     *Metrics
	agent string
	next  core.Caller
}

func (c *countingCaller) Call(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
	env, err := c.next.Call(ctx, tool, args)
	status, code := "", ""
	var rl *core.RateLimitedError
	var te *core.TransientError
	switch {
	case err == nil:
		status, code = string(env.Status), env.Code()
		c.m.Seen(c.agent)
	case errors.Is(err, core.ErrUnauthenticated):
		status = "unauthenticated"
	case errors.As(err, &rl):
		status = "rate_limited"
		c.m.Seen(c.agent)
	case errors.As(err, &te):
		status = "unreachable"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status = "cancelled"
	default:
		status = "protocol_error"
	}
	c.m.CoreCalls.WithLabelValues(tool, status, code).Inc()
	return env, err
}

// presence reports seconds since each agent last reached Core.
type presence struct {
	mu   sync.Mutex
	last map[string]time.Time
}

var presenceDesc = prometheus.NewDesc("presence_gap_seconds",
	"Seconds since each running agent last reached Core; above 60 its presence in Core may lapse.", []string{"agent"}, nil)

func (p *presence) seen(agent string, at time.Time) {
	p.mu.Lock()
	p.last[agent] = at
	p.mu.Unlock()
}

func (p *presence) forget(agent string) {
	p.mu.Lock()
	delete(p.last, agent)
	p.mu.Unlock()
}

func (p *presence) Describe(ch chan<- *prometheus.Desc) { ch <- presenceDesc }

func (p *presence) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for agent, at := range p.last {
		ch <- prometheus.MustNewConstMetric(presenceDesc, prometheus.GaugeValue, now.Sub(at).Seconds(), agent)
	}
}
