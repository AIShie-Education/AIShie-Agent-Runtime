package e2e

import (
	"bytes"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// thinkingOff is how the openai_chat adapter tells DeepSeek, on the wire,
// not to think on a call (its Thinking Mode guide).
var thinkingOff = []byte(`"thinking":{"type":"disabled"}`)

// aThinkingModelIsForcedToAnswer is the answers of 2026-10-04 on
// test.aishie.app against the real Core: an agent on DeepSeek's
// deepseek-flash, which thinks by default and counts its thinking in its
// completion tokens. Its turn thinks until its cap and writes nothing; the
// output tokens left cannot hold the turn again with twice the cap. The
// last turn is forced, telling DeepSeek not to think, and its answer is
// posted. A model that writes nothing even so, forced and asked once more,
// gets the budget's notice, in the question's Simplified Chinese, saying
// nothing of sources: Core shows none under it.
func aThinkingModelIsForcedToAnswer(t *testing.T, w *world) {
	const answer = "这门课要注意三件事：按时交作业、读讲义、做练习。"
	m := newModel(t, func(req fakellm.ChatRequest) fakellm.ChatResponse {
		writes := !strings.Contains(req.Messages[1].Text(), "别的")
		if writes && bytes.Contains(req.Raw, thinkingOff) {
			return fakellm.Reply(answer)
		}
		// Thinks until its cap, writing nothing: DeepSeek's finish_reason
		// length, with reasoning_content and an empty content.
		return fakellm.ChatResponse{
			Choices: []fakellm.Choice{{Message: fakellm.ChatMessage{Role: "assistant", Content: "",
				ReasoningContent: "The student asks what to watch out for. Let me weigh each part of the course again"}, FinishReason: "length"}},
			Usage: &fakellm.Usage{PromptTokens: 900, CompletionTokens: req.MaxTokens, TotalTokens: 900 + req.MaxTokens,
				CompletionTokensDetails: &fakellm.CompletionTokensDetails{ReasoningTokens: req.MaxTokens}},
		}
	})
	over := map[string]any{
		"model": map[string]any{"provider": "deepseek", "model": "deepseek-flash", "params": map[string]any{"max_output_tokens": 500}},
		// 1,200 output tokens keep a reserve of 300 for a forced turn:
		// after 500 thinking, 400 is not twice the cap.
		"budgets": map[string]any{"per_answer": map[string]any{"output_tokens": 1200}},
	}
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{{id: "yuki-helper", seat: w.own, over: over}}})
	rt.waitPolling("yuki-helper")

	conv, _ := w.ask(t, w.yuki, w.own.member, "有什么问题吗?")
	if got := w.waitAnswer(t, w.yuki, conv, w.own.member); got.text() != answer {
		t.Fatalf("Yuki's answer %q, want %q", got.text(), answer)
	}
	reqs := m.Requests()
	if len(reqs) != 2 {
		t.Fatalf("the model was asked %d times; want the turn cut off, then the forced one", len(reqs))
	}
	if bytes.Contains(reqs[0].Raw, thinkingOff) || reqs[0].MaxTokens != 500 || len(reqs[0].Tools) == 0 {
		t.Errorf("the first turn: %s", reqs[0].Raw)
	}
	// DeepSeek is not known to take tool_choice none: the forced turn is
	// offered no tools, and told not to think, within what is left.
	if f := reqs[1]; !bytes.Contains(f.Raw, thinkingOff) || len(f.Tools) != 0 || f.MaxTokens != 500 {
		t.Errorf("the forced turn: %s", f.Raw)
	}
	eventually(t, answerWait, "the answer recorded posted", func() bool {
		return rt.metric("answers_total", map[string]string{"outcome": "posted"}) == 1
	})
	if n := rt.metric("budget_exhausted_total", map[string]string{"budget": "output_tokens"}); n != 1 {
		t.Errorf("budget_exhausted_total{output_tokens} = %v", n)
	}

	conv2, _ := w.ask(t, w.yuki, w.own.member, "还有别的问题吗?")
	got := w.waitAnswer(t, w.yuki, conv2, w.own.member)
	if got.text() != config.BudgetNotice.ZhHans || got.Sources != nil {
		t.Errorf("the notice %q, sources %v; want %q and none", got.text(), got.Sources, config.BudgetNotice.ZhHans)
	}
	// The turn cut off; the forced one, 500; asked once more within the
	// 200 left.
	if reqs := m.Requests()[2:]; len(reqs) != 3 || reqs[1].MaxTokens != 500 || reqs[2].MaxTokens != 200 ||
		!strings.Contains(reqs[2].Messages[len(reqs[2].Messages)-1].Text(), "[You cannot look anything more up") {
		t.Errorf("the model was asked %d times for the notice", len(reqs))
	}
	eventually(t, answerWait, "the notice recorded", func() bool {
		return rt.metric("answers_total", map[string]string{"outcome": "budget"}) == 1
	})
}
