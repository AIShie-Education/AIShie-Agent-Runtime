package metrics

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

type stubCaller struct {
	env *core.Envelope
	err error
}

func (s stubCaller) Call(context.Context, string, json.RawMessage) (*core.Envelope, error) {
	return s.env, s.err
}

func TestCoreCallerCountsAndMarksPresence(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)
	ok := m.CoreCaller("agt_1", stubCaller{env: &core.Envelope{Status: core.StatusFailed, Error: &core.Error{Code: "conflict"}}})
	if _, err := ok.Call(context.Background(), "conversation_answer", nil); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(m.CoreCalls.WithLabelValues("conversation_answer", "failed", "conflict")); got != 1 {
		t.Errorf("core_calls_total = %v", got)
	}
	bad := m.CoreCaller("agt_2", stubCaller{err: core.ErrUnauthenticated})
	_, _ = bad.Call(context.Background(), "me_get", nil)
	if got := testutil.ToFloat64(m.CoreCalls.WithLabelValues("me_get", "unauthenticated", "")); got != 1 {
		t.Errorf("core_calls_total unauthenticated = %v", got)
	}

	out, err := testutil.CollectAndFormat(m.presence, expfmt.TypeTextPlain, "presence_gap_seconds")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `agent="agt_1"`) || strings.Contains(string(out), `agent="agt_2"`) {
		t.Errorf("presence reports the wrong agents:\n%s", out)
	}
	m.Forget("agt_1")
	if n := testutil.CollectAndCount(m.presence, "presence_gap_seconds"); n != 0 {
		t.Errorf("a forgotten agent is still reported (%d)", n)
	}
}

func TestObserveLLM(t *testing.T) {
	m := New(prometheus.NewRegistry())
	m.ObserveLLM("openai_chat", "m", &llm.Response{Stop: llm.StopEnd, Usage: llm.Usage{Input: 10, Output: 3}}, nil, 0.25, "school")
	m.ObserveLLM("openai_chat", "m", nil, &llm.Error{Kind: llm.ErrServer}, 0, "school")
	if got := testutil.ToFloat64(m.LLMCalls.WithLabelValues("openai_chat", "m", "end")); got != 1 {
		t.Errorf("llm_calls_total end = %v", got)
	}
	if got := testutil.ToFloat64(m.LLMCalls.WithLabelValues("openai_chat", "m", "error")); got != 1 {
		t.Errorf("llm_calls_total error = %v", got)
	}
	if got := testutil.ToFloat64(m.LLMTokens.WithLabelValues("openai_chat", "m", "input")); got != 10 {
		t.Errorf("llm_tokens_total input = %v", got)
	}
	if got := testutil.ToFloat64(m.LLMCost.WithLabelValues("school")); got != 0.25 {
		t.Errorf("llm_cost_usd_total = %v", got)
	}
}
