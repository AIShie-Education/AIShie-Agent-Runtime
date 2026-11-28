package runner

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveProviderProbes exercises every provider family through the real TS
// runner, never through a Go adapter. Existing secret/model environment names
// and no-key skip semantics are preserved for the nightly workflow.
func TestLiveProviderProbes(t *testing.T) {
	if os.Getenv("LIVE") != "1" {
		t.Skip("set LIVE=1 to probe real providers through the runner")
	}
	client := NewClient(os.Getenv("RUNNER_URL"), nil)
	cases := []struct {
		name, adapter, provider, model, base, keyEnv, modelEnv, region string
	}{
		{"openai_chat", "openai_chat", "openai", "gpt-4.1-mini", "", "OPENAI_API_KEY", "OPENAI_MODEL", ""},
		{"openai_responses", "openai_responses", "openai", "gpt-5-mini", "", "OPENAI_API_KEY", "OPENAI_RESPONSES_MODEL", ""},
		{"anthropic", "anthropic", "anthropic", "claude-haiku-4-5", "", "ANTHROPIC_API_KEY", "ANTHROPIC_MODEL", ""},
		{"gemini", "gemini", "gemini", "gemini-2.5-flash", "", "GEMINI_API_KEY", "GEMINI_MODEL", ""},
		{"deepseek", "openai_chat", "deepseek", "deepseek-chat", "https://api.deepseek.com", "DEEPSEEK_API_KEY", "DEEPSEEK_MODEL", ""},
		{"gemini_openai_compatible", "openai_chat", "gemini", "gemini-2.5-flash", "https://generativelanguage.googleapis.com/v1beta/openai/", "GEMINI_API_KEY", "GEMINI_MODEL", ""},
		{"openrouter", "openai_chat", "openrouter", "meta-llama/llama-3.3-70b-instruct", "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY", "OPENROUTER_MODEL", ""},
		{"bedrock", "bedrock_converse", "bedrock", "", "", "AWS_BEARER_TOKEN_BEDROCK", "BEDROCK_MODEL", os.Getenv("AWS_REGION")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := os.Getenv(tc.keyEnv)
			if key == "" && tc.name == "anthropic" {
				// Keep the original Anthropic model fallback environment too.
				key = os.Getenv("ANTHROPIC_API_KEY")
			}
			if key == "" && tc.name != "bedrock" {
				t.Skipf("%s is not set", tc.keyEnv)
			}
			model := os.Getenv(tc.modelEnv)
			if model == "" {
				model = tc.model
			}
			if tc.name == "anthropic" && os.Getenv("ANTHROPIC_MODEL") == "" && os.Getenv("ANTHROPIC_LIVE_MODEL") != "" {
				model = os.Getenv("ANTHROPIC_LIVE_MODEL")
			}
			if model == "" {
				t.Skipf("%s is not set", tc.modelEnv)
			}
			m := Model{Adapter: tc.adapter, Provider: tc.provider, ModelID: model, APIKey: key, Region: tc.region,
				Headers: map[string]string{}, MaxOutputTokens: 16, Hosted: false}
			if tc.base != "" {
				m.BaseURL = &tc.base
			}
			ctx, cancel := context.WithTimeout(context.Background(), ProbeTimeout+5*time.Second)
			defer cancel()
			got, err := client.Probe(ctx, m)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("result=%s http=%v provider_code=%v latency_ms=%d", got.Result, got.HTTPStatus, got.ProviderCode, got.LatencyMS)
			if got.Result != ResultOK && got.Result != ResultKeyAccepted {
				t.Fatalf("probe result %s (HTTP %v, code %v)", got.Result, got.HTTPStatus, got.ProviderCode)
			}
		})
	}

	t.Run("azure_openai", func(t *testing.T) {
		base, key, deployment := os.Getenv("AZURE_OPENAI_BASE_URL"), os.Getenv("AZURE_OPENAI_API_KEY"), os.Getenv("AZURE_OPENAI_DEPLOYMENT")
		if base == "" || key == "" || deployment == "" {
			t.Skip("AZURE_OPENAI_BASE_URL, AZURE_OPENAI_API_KEY and AZURE_OPENAI_DEPLOYMENT are not all set")
		}
		m := Model{Adapter: "openai_responses", Provider: "azure", ModelID: deployment, BaseURL: &base, APIKey: key,
			Headers: map[string]string{}, MaxOutputTokens: 16, Hosted: false}
		got, err := client.Probe(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if got.Result != ResultOK && got.Result != ResultKeyAccepted {
			t.Fatalf("probe result %s (HTTP %v, code %v)", got.Result, got.HTTPStatus, got.ProviderCode)
		}
	})
}
