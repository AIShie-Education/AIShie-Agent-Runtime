// Package metrics is what the runtime counts (Core's docs/agent-runtime.md
// §8.1): polls, answers and their latency, model calls, tokens and cost,
// Core calls, budgets spent, lease takeovers, and each agent's presence gap.
// Labels hold ids, names and codes, never text.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

// Metrics are the runtime's collectors, registered on one registry.
type Metrics struct {
	InboxPolls      *prometheus.CounterVec
	AnswerLatency   *prometheus.HistogramVec
	Answers         *prometheus.CounterVec
	LLMCalls        *prometheus.CounterVec
	LLMTokens       *prometheus.CounterVec
	LLMCost         *prometheus.CounterVec
	CoreCalls       *prometheus.CounterVec
	BudgetExhausted *prometheus.CounterVec
	LeaseTakeovers  prometheus.Counter
	AgentStates     *prometheus.GaugeVec

	presence *presence
}

// New makes the collectors and registers them on reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		InboxPolls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inbox_polls_total", Help: "conversation_inbox calls, by agent and by whether they found work.",
		}, []string{"agent", "result"}),
		AnswerLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "answer_latency_seconds",
			Help: "Answer latency by stage: notice (question written to claimed), answer (claimed to conversation_answer returned).",
			// Targets are 3 s and 13 s to notice, 10 s and 30 s to answer.
			Buckets: []float64{0.5, 1, 2, 3, 5, 8, 10, 13, 20, 30, 45, 60, 90, 120, 300},
		}, []string{"stage"}),
		Answers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "answers_total", Help: "Claimed questions, by how each ended.",
		}, []string{"outcome"}),
		LLMCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_calls_total", Help: "Model calls, by adapter, model and internal stop reason (error for a call that did not complete).",
		}, []string{"adapter", "model", "stop"}),
		LLMTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_tokens_total", Help: "Tokens, by adapter, model and kind (input, cache_read, cache_write, output, reasoning).",
		}, []string{"adapter", "model", "kind"}),
		LLMCost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_cost_usd_total", Help: "Cost of model calls in US dollars, by whose key paid.",
		}, []string{"key_source"}),
		CoreCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "core_calls_total", Help: "Calls to Core, by tool, envelope status (or transport failure) and error code.",
		}, []string{"tool", "status", "error_code"}),
		BudgetExhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "budget_exhausted_total", Help: "Budgets and quotas reached, by which.",
		}, []string{"budget"}),
		LeaseTakeovers: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lease_takeovers_total", Help: "Agents this worker took over from another worker whose lease had lapsed.",
		}),
		AgentStates: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "agents", Help: "Agents this worker runs, by state.",
		}, []string{"state"}),
		presence: &presence{last: map[string]time.Time{}},
	}
	reg.MustRegister(m.InboxPolls, m.AnswerLatency, m.Answers, m.LLMCalls, m.LLMTokens, m.LLMCost,
		m.CoreCalls, m.BudgetExhausted, m.LeaseTakeovers, m.AgentStates, m.presence)
	return m
}

// ObserveLLM counts one model call.
func (m *Metrics) ObserveLLM(adapter, model string, resp *llm.Response, err error, costUSD float64, keySource string) {
	if err != nil || resp == nil {
		m.LLMCalls.WithLabelValues(adapter, model, "error").Inc()
		return
	}
	m.LLMCalls.WithLabelValues(adapter, model, string(resp.Stop)).Inc()
	for kind, n := range map[string]int64{
		"input": resp.Usage.Input, "cache_read": resp.Usage.CacheRead, "cache_write": resp.Usage.CacheWrite,
		"output": resp.Usage.Output, "reasoning": resp.Usage.Reasoning,
	} {
		if n > 0 {
			m.LLMTokens.WithLabelValues(adapter, model, kind).Add(float64(n))
		}
	}
	if costUSD > 0 {
		m.LLMCost.WithLabelValues(keySource).Add(costUSD)
	}
}

// Seen records that agentID called Core now, for presence_gap_seconds.
func (m *Metrics) Seen(agentID string) { m.presence.seen(agentID, time.Now()) }

// Forget stops reporting agentID's presence gap: it was stopped or paused
// on purpose, which is not a gap.
func (m *Metrics) Forget(agentID string) { m.presence.forget(agentID) }

// CoreCaller wraps next so that each call is counted and marks agentID as
// present.
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

// presence reports, per agent, the seconds since it last reached Core.
// Core's presence shows an agent online if seen in the last two minutes,
// and records a token's use at most once a minute; above 60 s is worth an
// alert (§2.5, §8.1).
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
