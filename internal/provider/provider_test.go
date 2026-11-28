package provider

import "testing"

func TestDetectProvider(t *testing.T) {
	for _, tc := range []struct {
		adapter, baseURL, want string
	}{
		{AdapterOpenAIChat, "", ProviderOpenAI},
		{AdapterOpenAIResponses, "", ProviderOpenAI},
		{AdapterAnthropic, "", ProviderAnthropic},
		{AdapterGemini, "", ProviderGemini},
		{AdapterBedrockConverse, "", ProviderBedrock},
		{AdapterOpenAIChat, "https://api.deepseek.com", ProviderDeepSeek},
		{AdapterOpenAIChat, "https://openrouter.ai/api/v1", ProviderOpenRouter},
		{AdapterOpenAIChat, "https://llm.internal.example/v1", ProviderOpenAICompat},
		{AdapterOpenAIChat, "http://localhost:11434/v1", ProviderOllama},
		{AdapterOpenAIChat, "http://localhost:1234/v1", ProviderLMStudio},
	} {
		if got := DetectProvider(tc.adapter, tc.baseURL); got != tc.want {
			t.Errorf("DetectProvider(%q, %q) = %q, want %q", tc.adapter, tc.baseURL, got, tc.want)
		}
	}
}

func TestUsageAdd(t *testing.T) {
	got := (Usage{Input: 10, Output: 4, CacheRead: 2, Estimated: true}).Add(Usage{Input: 5, Output: 3, CacheWrite: 1})
	if got.Input != 15 || got.Output != 7 || got.CacheRead != 2 || got.CacheWrite != 1 || !got.Estimated {
		t.Errorf("Usage.Add = %+v", got)
	}
}
