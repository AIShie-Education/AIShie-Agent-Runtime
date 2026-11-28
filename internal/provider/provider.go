// Package provider holds the small, wire-neutral model types shared by the
// Go control plane. Model calls themselves live in the TypeScript runner.
package provider

import (
	"encoding/json"
	"net"
	"net/url"
	"strings"
)

// Adapter names accepted in agent configuration.
const (
	AdapterOpenAIChat      = "openai_chat"
	AdapterOpenAIResponses = "openai_responses"
	AdapterAnthropic       = "anthropic"
	AdapterGemini          = "gemini"
	AdapterBedrockConverse = "bedrock_converse"
)

// Provider names, as DetectProvider reports them.
const (
	ProviderOpenAI       = "openai"
	ProviderAzure        = "azure"
	ProviderAnthropic    = "anthropic"
	ProviderGemini       = "gemini"
	ProviderBedrock      = "bedrock"
	ProviderDeepSeek     = "deepseek"
	ProviderQwen         = "qwen"
	ProviderMoonshot     = "moonshot"
	ProviderGLM          = "glm"
	ProviderOpenRouter   = "openrouter"
	ProviderOllama       = "ollama"
	ProviderLMStudio     = "lmstudio"
	ProviderVLLM         = "vllm"
	ProviderOpenAICompat = "openai_compatible"
)

// DetectProvider names who serves baseURL for adapter. An empty baseURL is
// the adapter's official endpoint; an unknown OpenAI Chat endpoint is the
// deliberately conservative openai_compatible provider.
func DetectProvider(adapter, baseURL string) string {
	switch adapter {
	case AdapterAnthropic:
		if baseURL == "" {
			return ProviderAnthropic
		}
	case AdapterGemini:
		return ProviderGemini
	case AdapterBedrockConverse:
		return ProviderBedrock
	case AdapterOpenAIResponses:
		if baseURL == "" {
			return ProviderOpenAI
		}
	case AdapterOpenAIChat:
		if baseURL == "" {
			return ProviderOpenAI
		}
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return ProviderOpenAICompat
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	has := func(suffix string) bool { return host == suffix || strings.HasSuffix(host, "."+suffix) }
	switch {
	case has("api.openai.com"):
		return ProviderOpenAI
	case has("openai.azure.com"), has("cognitiveservices.azure.com"), has("services.ai.azure.com"):
		return ProviderAzure
	case has("api.anthropic.com"):
		return ProviderAnthropic
	case has("generativelanguage.googleapis.com"):
		return ProviderGemini
	case has("amazonaws.com") && strings.Contains(host, "bedrock"):
		return ProviderBedrock
	case has("api.deepseek.com"):
		return ProviderDeepSeek
	case has("aliyuncs.com"):
		return ProviderQwen
	case has("api.moonshot.ai"), has("api.moonshot.cn"):
		return ProviderMoonshot
	case has("api.z.ai"), has("open.bigmodel.cn"):
		return ProviderGLM
	case has("openrouter.ai"):
		return ProviderOpenRouter
	case port == "11434":
		return ProviderOllama
	case port == "1234" && isLocal(host):
		return ProviderLMStudio
	}
	switch adapter {
	case AdapterAnthropic:
		return ProviderAnthropic
	case AdapterOpenAIResponses:
		return ProviderOpenAI
	}
	return ProviderOpenAICompat
}

func isLocal(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ErrorKind classifies a runner or provider call that did not complete.
type ErrorKind string

const (
	ErrRateLimited     ErrorKind = "rate_limited"
	ErrOverloaded      ErrorKind = "overloaded"
	ErrAuth            ErrorKind = "auth"
	ErrBadRequest      ErrorKind = "bad_request"
	ErrContextOverflow ErrorKind = "context_overflow"
	ErrContentFilter   ErrorKind = "content_filter"
	ErrServer          ErrorKind = "server"
	ErrNetwork         ErrorKind = "network"
	ErrTimeout         ErrorKind = "timeout"
)

// Usage is the aggregate token accounting reported by the runner. Input
// includes cached input. Raw and Estimated are optional locally supplied
// accounting metadata and are not required on the runner wire.
type Usage struct {
	Input      int64           `json:"input"`
	CacheRead  int64           `json:"cache_read"`
	CacheWrite int64           `json:"cache_write"`
	Output     int64           `json:"output"`
	Reasoning  int64           `json:"reasoning"`
	Raw        json.RawMessage `json:"raw,omitempty"`
	Estimated  bool            `json:"estimated,omitempty"`
}

// Add returns u plus v; Raw is v's.
func (u Usage) Add(v Usage) Usage {
	return Usage{
		Input: u.Input + v.Input, CacheRead: u.CacheRead + v.CacheRead, CacheWrite: u.CacheWrite + v.CacheWrite,
		Output: u.Output + v.Output, Reasoning: u.Reasoning + v.Reasoning,
		Raw: v.Raw, Estimated: u.Estimated || v.Estimated,
	}
}
