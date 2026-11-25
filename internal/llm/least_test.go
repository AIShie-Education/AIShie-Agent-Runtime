package llm

import "testing"

// TestOpenAILeastEffort holds the lowest effort each of OpenAI's model
// families takes, by its id as OpenAI, Azure (a deployment named for its
// model), OpenRouter (after openai/) or a fine-tune writes it, and "" where
// the model reasons least unasked or does not reason; a model not listed is
// not known.
func TestOpenAILeastEffort(t *testing.T) {
	for _, c := range []struct {
		model, effort string
		known         bool
	}{
		{"gpt-5.1", "", true},
		{"GPT-5.1-2025-11-13", "", true},
		{"gpt-5.2", "", true},
		{"gpt-5.4-nano", "", true},
		{"gpt-5.4-pro", "", true},
		{"gpt-5-pro", "", true},
		{"gpt-5.5", "none", true},
		{"gpt-5.5:batch", "none", true},
		{"gpt-5.6-terra", "none", true},
		{"gpt-6-sol", "none", true},
		{"gpt-6-astra", "low", true},
		{"gpt-6.1-sol", "low", true},
		{"gpt-5", "minimal", true},
		{"gpt-5-mini-2025-08-07", "minimal", true},
		{"gpt-5.2-pro", "medium", true},
		{"gpt-5.5-pro", "medium", true},
		{"gpt-5.3-codex", "low", true},
		{"o3", "low", true},
		{"o3-mini-2025-01-31", "low", true},
		{"ft:o4-mini-2025-04-16:school::a1b2", "low", true},
		{"gpt-oss-120b", "low", true},
		{"gpt-5-chat-latest", "", true},
		{"gpt-5.3-chat-latest", "", true},
		{"chat-latest", "", true},
		{"gpt-5-codex", "", false},
		{"gpt-5.1-codex-max", "", false},
		{"gpt-5.6-cyber", "", false},
		{"gpt-7", "", false},
		{"gpt-4.1", "", false},
		{"tutor-prod", "", false},
	} {
		if effort, known := OpenAILeastEffort(c.model); effort != c.effort || known != c.known {
			t.Errorf("OpenAILeastEffort(%q) = %q, %v; want %q, %v", c.model, effort, known, c.effort, c.known)
		}
	}
}

// TestOpenAIChatToolEffort holds OpenAI's Chat Completions tool-call
// constraint to model ids, including snapshots and fine-tunes. An unknown
// newer model is not guessed at; a model that cannot take none is not
// supported there at all.
func TestOpenAIChatToolEffort(t *testing.T) {
	for _, c := range []struct {
		model, effort          string
		constrained, supported bool
	}{
		{"gpt-5.3", "", false, true},
		{"gpt-5.4", "none", true, true},
		{"GPT-5.5-2026-10-01", "none", true, true},
		{"ft:gpt-6-luna:school::a1b2", "none", true, true},
		{"gpt-6-astra", "", true, false},
		{"gpt-6.1-sol:batch", "", true, false},
		{"gpt-5.4-pro", "", true, false},
		{"gpt-7", "", false, true},
	} {
		effort, constrained, supported := OpenAIChatToolEffort(c.model)
		if effort != c.effort || constrained != c.constrained || supported != c.supported {
			t.Errorf("OpenAIChatToolEffort(%q) = %q, %v, %v; want %q, %v, %v", c.model, effort, constrained, supported, c.effort, c.constrained, c.supported)
		}
	}
}

// TestLeastEffort: a configured effort on a model whose family is not
// known goes no higher than low, and nothing stays nothing.
func TestLeastEffort(t *testing.T) {
	for effort, want := range map[string]string{"": "", "minimal": "minimal", "low": "low", "medium": "low", "high": "low"} {
		if got := LeastEffort(effort); got != want {
			t.Errorf("LeastEffort(%q) = %q, want %q", effort, got, want)
		}
	}
}
