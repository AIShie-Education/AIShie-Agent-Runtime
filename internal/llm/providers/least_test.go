package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// Base URLs of the providers, as an agent's configuration writes them.
// No call leaves the test: the transport keeps each request's body.
const (
	openAI     = ""
	azure      = "https://school.openai.azure.com/openai/v1"
	openRouter = "https://openrouter.ai/api/v1"
	geminiAPI  = "https://generativelanguage.googleapis.com/v1beta/openai/"
	deepSeek   = "https://api.deepseek.com"
	deepSeekA  = "https://api.deepseek.com/anthropic"
	moonshot   = "https://api.moonshot.ai/v1"
	glm        = "https://api.z.ai/api/paas/v4"
	qwen       = "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"
	ollama     = "http://localhost:11434/v1"
	localResp  = "http://127.0.0.1:8000/v1"
)

// TestLeastReasoningNeverThinksMore holds, adapter by adapter, provider by
// provider and model by model, what a call asking for the least reasoning
// (ForceAnswer's, a continuation's) asks of the model's thinking, beside
// what an ordinary call asks, with and without an effort configured. It is
// the lowest setting the provider documents for the model's family: off
// where it can be switched off, the lowest effort where it cannot, and
// nothing where the model thinks least unasked (GPT-5.1's none, Claude
// 4.5, Gemini 2.5 Flash-Lite). A model whose family is not documented is
// sent no more than it was configured with, and nothing when it was
// configured with nothing.
//
// What is sent is written as the request's fields: effort= (Chat
// Completions' reasoning_effort), reasoning= (reasoning.effort, on
// OpenRouter and the Responses API), thinking= and output= (Anthropic's
// thinking and output_config.effort, on Bedrock inside
// additionalModelRequestFields), budget= and level= (Gemini's
// thinkingConfig); "" for nothing.
func TestLeastReasoningNeverThinksMore(t *testing.T) {
	for _, c := range []struct {
		adapter, base, model, effort string
		ordinary, least              string
	}{
		// OpenAI's own: the lowest effort each model page lists, and
		// nothing where that is the model's default.
		{"openai_chat", openAI, "gpt-5.1", "", "", ""},
		{"openai_chat", openAI, "gpt-5.1", "high", "effort=high", ""},
		{"openai_chat", openAI, "gpt-5.1-2025-11-13", "", "", ""},
		{"openai_chat", openAI, "gpt-5.2", "", "", ""},
		{"openai_chat", openAI, "gpt-5.4-mini", "medium", "effort=medium", ""},
		{"openai_chat", openAI, "gpt-5.5", "", "", "effort=none"},
		{"openai_chat", openAI, "gpt-5.5", "medium", "effort=medium", "effort=none"},
		{"openai_chat", openAI, "gpt-5.6-sol", "", "", "effort=none"},
		{"openai_chat", openAI, "gpt-6-luna", "", "", "effort=none"},
		{"openai_chat", openAI, "gpt-6-astra", "", "", "effort=low"},
		{"openai_chat", openAI, "gpt-6.1-sol", "", "", "effort=low"},
		{"openai_chat", openAI, "gpt-5", "", "", "effort=minimal"},
		{"openai_chat", openAI, "gpt-5-mini", "high", "effort=high", "effort=minimal"},
		{"openai_chat", openAI, "gpt-5-nano-2025-08-07", "", "", "effort=minimal"},
		{"openai_chat", openAI, "gpt-5-pro", "high", "effort=high", ""},
		{"openai_chat", openAI, "gpt-5.2-pro", "", "", "effort=medium"},
		{"openai_chat", openAI, "gpt-5.5-pro", "high", "effort=high", "effort=medium"},
		{"openai_chat", openAI, "gpt-5.4-pro", "", "", ""},
		{"openai_chat", openAI, "gpt-5.3-codex", "", "", "effort=low"},
		{"openai_chat", openAI, "o4-mini", "", "", "effort=low"},
		{"openai_chat", openAI, "o3", "high", "effort=high", "effort=low"},
		{"openai_chat", openAI, "ft:o4-mini-2025-04-16:school::a1b2", "", "", "effort=low"},
		{"openai_chat", openAI, "gpt-5-chat-latest", "", "", ""},
		{"openai_chat", openAI, "gpt-4.1", "high", "", ""},
		// Not documented: nothing unasked, no more than configured.
		{"openai_chat", openAI, "gpt-5.1-codex", "", "", ""},
		{"openai_chat", openAI, "gpt-5.1-codex", "high", "effort=high", "effort=low"},
		{"openai_chat", openAI, "gpt-5.1-codex", "minimal", "effort=minimal", "effort=minimal"},
		// Azure, by the deployment's name where it names its model.
		{"openai_chat", azure, "tutor-prod", "", "", ""},
		{"openai_chat", azure, "tutor-prod", "medium", "effort=medium", "effort=low"},
		{"openai_chat", azure, "gpt-5.1", "", "", ""},
		{"openai_chat", azure, "gpt-5.5", "", "", "effort=none"},
		{"openai_chat", azure, "o4-mini", "", "", "effort=low"},
		{"openai_chat", azure, "gpt-5-chat-latest", "medium", "effort=medium", ""},
		// OpenRouter, by each maker's documentation.
		{"openai_chat", openRouter, "openai/gpt-5.1", "", "", ""},
		{"openai_chat", openRouter, "openai/gpt-5.5", "", "", "reasoning=none"},
		{"openai_chat", openRouter, "openai/gpt-5-mini", "minimal", "reasoning=minimal", "reasoning=minimal"},
		{"openai_chat", openRouter, "openai/o4-mini", "", "", "reasoning=low"},
		{"openai_chat", openRouter, "anthropic/claude-opus-5", "", "", "reasoning=low"},
		{"openai_chat", openRouter, "anthropic/claude-sonnet-5.5", "high", "reasoning=high", "reasoning=low"},
		{"openai_chat", openRouter, "anthropic/claude-sonnet-4.5", "", "", ""},
		{"openai_chat", openRouter, "anthropic/claude-sonnet-4.5", "medium", "reasoning=medium", ""},
		{"openai_chat", openRouter, "google/gemini-2.5-flash", "", "", "reasoning=none"},
		{"openai_chat", openRouter, "google/gemini-2.5-pro", "", "", "reasoning=minimal"},
		{"openai_chat", openRouter, "google/gemini-2.5-flash-lite", "", "", ""},
		{"openai_chat", openRouter, "google/gemini-3-flash-preview", "", "", "reasoning=minimal"},
		{"openai_chat", openRouter, "google/gemini-3.1-pro-preview", "", "", "reasoning=low"},
		{"openai_chat", openRouter, "google/gemini-3.8-flash", "high", "reasoning=high", "reasoning=low"},
		{"openai_chat", openRouter, "google/gemini-3.1-flash-lite", "", "", ""},
		{"openai_chat", openRouter, "deepseek/deepseek-v4-flash", "", "", "reasoning=none"},
		{"openai_chat", openRouter, "deepseek/deepseek-v4-flash", "high", "reasoning=high", "reasoning=none"},
		{"openai_chat", openRouter, "qwen/qwen3-max", "", "", "reasoning=none"},
		{"openai_chat", openRouter, "z-ai/glm-4.6", "", "", ""},
		{"openai_chat", openRouter, "z-ai/glm-4.6", "high", "reasoning=high", "reasoning=low"},
		{"openai_chat", openRouter, "moonshotai/kimi-k3", "", "", ""},
		// Gemini's OpenAI-compatible endpoint, as Google maps an effort
		// there.
		{"openai_chat", geminiAPI, "gemini-2.5-flash", "", "", "effort=none"},
		{"openai_chat", geminiAPI, "gemini-2.5-pro", "high", "", "effort=minimal"},
		{"openai_chat", geminiAPI, "gemini-2.5-flash-lite", "", "", ""},
		{"openai_chat", geminiAPI, "gemini-3-flash-preview", "", "", "effort=minimal"},
		{"openai_chat", geminiAPI, "gemini-3.1-pro-preview", "", "", "effort=low"},
		{"openai_chat", geminiAPI, "gemini-3.5-flash", "", "", "effort=minimal"},
		{"openai_chat", geminiAPI, "gemini-3.8-flash", "", "", "effort=low"},
		{"openai_chat", geminiAPI, "gemini-3.1-flash-lite", "", "", ""},
		{"openai_chat", geminiAPI, "gemini-4-flash", "", "", ""},
		// DeepSeek's switch; nothing to Kimi, GLM, Qwen or a local server.
		{"openai_chat", deepSeek, "deepseek-flash", "", "", "thinking=disabled"},
		{"openai_chat", deepSeek, "deepseek-v4-pro", "high", "", "thinking=disabled"},
		{"openai_chat", moonshot, "kimi-k2-thinking", "high", "", ""},
		{"openai_chat", glm, "glm-4.6", "", "", ""},
		{"openai_chat", qwen, "qwen3-max", "", "", ""},
		{"openai_chat", ollama, "qwen3:8b", "high", "", ""},

		// The Responses API.
		{"openai_responses", openAI, "gpt-5.1", "", "", ""},
		{"openai_responses", openAI, "gpt-5.1", "high", "reasoning=high", ""},
		{"openai_responses", openAI, "gpt-5.5", "", "", "reasoning=none"},
		{"openai_responses", openAI, "gpt-5", "", "", "reasoning=minimal"},
		{"openai_responses", openAI, "gpt-5", "high", "reasoning=high", "reasoning=minimal"},
		{"openai_responses", openAI, "gpt-6-astra", "", "", "reasoning=low"},
		{"openai_responses", openAI, "o4-mini", "", "", "reasoning=low"},
		{"openai_responses", openAI, "gpt-5-pro", "", "", ""},
		{"openai_responses", openAI, "gpt-4.1", "", "", ""},
		{"openai_responses", azure, "tutor-prod", "medium", "reasoning=medium", "reasoning=low"},
		{"openai_responses", azure, "gpt-5.1", "", "", ""},
		{"openai_responses", deepSeek, "deepseek-flash", "", "", "reasoning=none"},
		{"openai_responses", localResp, "tutor-local", "high", "reasoning=high", "reasoning=low"},
		{"openai_responses", localResp, "tutor-local", "", "", ""},

		// Anthropic's Messages API: low, adaptively, for a Claude that
		// thinks unasked; nothing for one that does not.
		{"anthropic", "", "claude-opus-5", "", "", "thinking=adaptive output=low"},
		{"anthropic", "", "claude-opus-5-5", "high", "thinking=adaptive output=high", "thinking=adaptive output=low"},
		{"anthropic", "", "claude-sonnet-5-5", "", "", "thinking=adaptive output=low"},
		{"anthropic", "", "claude-fable-5-1", "", "", "thinking=adaptive output=low"},
		{"anthropic", "", "claude-sonnet-4-5", "", "", ""},
		{"anthropic", "", "claude-sonnet-4-5", "medium", "thinking=enabled budget=4096", ""},
		{"anthropic", "", "claude-opus-4-8", "high", "thinking=adaptive output=high", ""},
		{"anthropic", "", "claude-3-7-sonnet-20250219", "medium", "thinking=enabled budget=4096", "thinking=enabled budget=1024"},
		{"anthropic", "", "claude-haiku-6", "", "", ""},
		{"anthropic", deepSeekA, "deepseek-flash", "", "", "thinking=disabled"},

		// Gemini's own API: thinking off where it goes off, else the least
		// budget or level the model takes.
		{"gemini", "", "gemini-2.5-flash", "", "", "budget=0"},
		{"gemini", "", "gemini-2.5-flash", "low", "budget=1024", "budget=0"},
		{"gemini", "", "gemini-2.5-pro", "", "", "budget=128"},
		{"gemini", "", "gemini-2.5-pro", "high", "budget=24576", "budget=128"},
		{"gemini", "", "gemini-2.5-flash-lite", "", "", ""},
		{"gemini", "", "gemini-2.5-flash-lite", "low", "budget=1024", ""},
		{"gemini", "", "gemini-3-pro-preview", "", "", "level=LOW"},
		{"gemini", "", "gemini-3-flash-preview", "", "", "level=MINIMAL"},
		{"gemini", "", "gemini-3.5-flash", "", "", "level=MINIMAL"},
		{"gemini", "", "gemini-3.6-flash", "medium", "level=MEDIUM", "level=MINIMAL"},
		{"gemini", "", "gemini-3.8-flash", "", "", "level=LOW"},
		{"gemini", "", "gemini-3.1-flash-lite", "", "", ""},
		{"gemini", "", "gemini-3.1-flash-lite", "high", "level=HIGH", ""},
		{"gemini", "", "gemini-4-flash", "", "", ""},
		{"gemini", "", "gemini-4-flash", "high", "level=HIGH", "level=LOW"},
		{"gemini", "", "gemini-flash-latest", "", "", ""},

		// Bedrock: Claude as the anthropic adapter asks it.
		{"bedrock_converse", "", "us.anthropic.claude-opus-5-20260901-v1:0", "", "", "thinking=adaptive output=low"},
		{"bedrock_converse", "", "global.anthropic.claude-sonnet-5-5-v1", "high", "thinking=adaptive output=high", "thinking=adaptive output=low"},
		{"bedrock_converse", "", "anthropic.claude-sonnet-4-5-20250929-v1:0", "medium", "thinking=enabled budget=8192", ""},
		{"bedrock_converse", "", "anthropic.claude-opus-4-7", "high", "thinking=adaptive output=high", ""},
		{"bedrock_converse", "", "us.anthropic.claude-3-7-sonnet-20250219-v1:0", "medium", "thinking=enabled budget=8192", "thinking=enabled budget=2048"},
		{"bedrock_converse", "", "amazon.nova-pro-v1:0", "high", "", ""},
	} {
		t.Run(fmt.Sprintf("%s %s %s at %q", c.adapter, c.base, c.model, c.effort), func(t *testing.T) {
			ordinary, least := reasoningSent(t, c.adapter, c.base, c.model, c.effort)
			if ordinary != c.ordinary {
				t.Errorf("an ordinary call sent %q, want %q", ordinary, c.ordinary)
			}
			if least != c.least {
				t.Errorf("a call asking for the least reasoning sent %q, want %q", least, c.least)
			}
		})
	}
}

// keep is a transport that keeps each request's body and refuses the call.
type keep struct{ bodies [][]byte }

func (k *keep) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		var err error
		if b, err = io.ReadAll(r.Body); err != nil {
			return nil, err
		}
	}
	k.bodies = append(k.bodies, b)
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"kept by the test"}}`)),
		Request:    r,
	}, nil
}

// reasoningSent makes an ordinary call (tools offered) and one asking for
// the least reasoning (tools declined, as ForceAnswer's), and reads what
// each asked of the model's thinking.
func reasoningSent(t *testing.T, adapter, base, model, effort string) (ordinary, least string) {
	t.Helper()
	k := &keep{}
	a, err := New(llm.Config{
		Adapter: adapter, Model: model, BaseURL: base, APIKey: "sk-test-0123456789", Region: "us-east-1",
		Params:     llm.Params{MaxOutputTokens: 32000},
		Reasoning:  llm.Reasoning{Effort: effort},
		HTTPClient: &http.Client{Transport: k},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools := []llm.Tool{{Name: "grade_list", Description: "Grades of an assignment.",
		Schema: json.RawMessage(`{"type":"object","properties":{"assignment_id":{"type":"string"}},"required":["assignment_id"]}`)}}
	sent := func(mode llm.ToolMode, leastReasoning bool) string {
		k.bodies = nil
		if _, err := a.Call(context.Background(), &llm.Request{System: "You are a course's tutor.",
			Messages: []llm.Message{llm.UserText("Why did I lose marks on HW3?")}, Tools: tools, ToolMode: mode,
			LeastReasoning: leastReasoning}); err == nil {
			t.Fatal("the call was not refused")
		}
		if len(k.bodies) == 0 {
			t.Fatal("no request was made")
		}
		return thinkingAsked(t, k.bodies[0])
	}
	return sent(llm.ToolAuto, false), sent(llm.ToolNone, true)
}

// thinkingAsked reads a request body's fields that ask of a model's
// thinking, in every adapter's shape.
func thinkingAsked(t *testing.T, body []byte) string {
	t.Helper()
	type anthropicShape struct {
		Thinking *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		OutputConfig *struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	var w struct {
		anthropicShape
		ReasoningEffort string `json:"reasoning_effort"`
		Reasoning       *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		GenerationConfig *struct {
			ThinkingConfig *struct {
				ThinkingBudget *int   `json:"thinkingBudget"`
				ThinkingLevel  string `json:"thinkingLevel"`
			} `json:"thinkingConfig"`
		} `json:"generationConfig"`
		AdditionalModelRequestFields *anthropicShape `json:"additionalModelRequestFields"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	var out []string
	add := func(f string, v any) { out = append(out, fmt.Sprintf("%s=%v", f, v)) }
	if w.ReasoningEffort != "" {
		add("effort", w.ReasoningEffort)
	}
	if w.Reasoning != nil {
		add("reasoning", w.Reasoning.Effort)
	}
	anth := w.anthropicShape
	if w.AdditionalModelRequestFields != nil {
		anth = *w.AdditionalModelRequestFields
	}
	if anth.Thinking != nil {
		add("thinking", anth.Thinking.Type)
		if anth.Thinking.BudgetTokens > 0 {
			add("budget", anth.Thinking.BudgetTokens)
		}
	}
	if anth.OutputConfig != nil {
		add("output", anth.OutputConfig.Effort)
	}
	if g := w.GenerationConfig; g != nil && g.ThinkingConfig != nil {
		if b := g.ThinkingConfig.ThinkingBudget; b != nil {
			add("budget", *b)
		}
		if l := g.ThinkingConfig.ThinkingLevel; l != "" {
			add("level", l)
		}
	}
	return strings.Join(out, " ")
}
