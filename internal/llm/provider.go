package llm

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// Config is what an adapter is built from: one agent's model section, with
// its key resolved.
type Config struct {
	// Adapter is openai_chat, openai_responses, anthropic, gemini or
	// bedrock_converse.
	Adapter string
	// Model is the provider's model id; for Azure, the deployment name.
	Model string
	// BaseURL is the API's base, e.g. https://api.deepseek.com; empty for the
	// adapter's own default.
	BaseURL string
	// APIKey is the resolved key. It is sent only as the provider asks
	// (Authorization, x-api-key, api-key, x-goog-api-key) and never logged.
	// Empty for servers that take none (Ollama, LM Studio) and for Bedrock
	// under SigV4.
	APIKey string
	// Provider overrides DetectProvider.
	Provider string
	// Region is Bedrock's AWS region.
	Region string
	Params Params
	// Reasoning asks the model to think, as each adapter maps it.
	Reasoning Reasoning
	// Capabilities override the adapter's defaults where set.
	Capabilities CapabilityOverrides
	// Dialect overrides the default schema dialect where set.
	Dialect toolschema.Dialect
	// Headers are extra request headers, never secrets.
	Headers map[string]string
	// HTTPClient carries the egress proxy and TLS settings. Nil means
	// http.DefaultClient. Timeouts come from the call's context.
	HTTPClient *http.Client
}

// Params are sampling settings; zero values are left out of requests.
type Params struct {
	MaxOutputTokens int
	Temperature     *float64
	TopP            *float64
}

// Reasoning asks for thinking. Effort is "", minimal, low, medium or high;
// "" sends nothing.
type Reasoning struct {
	Effort string
}

// CapabilityOverrides are an agent's corrections to an adapter's defaults.
type CapabilityOverrides struct {
	ParallelToolCalls *bool
	StrictTools       *bool
	ToolChoiceNone    *bool
	FileInput         *bool
}

// Apply returns c with the overrides set.
func (o CapabilityOverrides) Apply(c Capabilities) Capabilities {
	if o.ParallelToolCalls != nil {
		c.ParallelToolCalls = *o.ParallelToolCalls
	}
	if o.StrictTools != nil {
		c.StrictTools = *o.StrictTools
	}
	if o.ToolChoiceNone != nil {
		c.ToolChoiceNone = *o.ToolChoiceNone
	}
	if o.FileInput != nil {
		c.FileInput = *o.FileInput
	}
	return c
}

// Adapter names.
const (
	AdapterOpenAIChat      = "openai_chat"
	AdapterOpenAIResponses = "openai_responses"
	AdapterAnthropic       = "anthropic"
	AdapterGemini          = "gemini"
	AdapterBedrockConverse = "bedrock_converse"
)

// Providers, as DetectProvider names them.
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
	ProviderOpenAICompat = "openai_compatible" // anything else behind openai_chat
)

// DetectProvider names who serves baseURL for adapter, from the host
// (Core's docs/agent-runtime.md §3.9). An empty baseURL is the adapter's own
// default provider. A host it does not know behind openai_chat is
// openai_compatible, which gets the most careful defaults.
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
		return ProviderAnthropic // an Anthropic-format endpoint elsewhere
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

// Defaults are an adapter's capabilities and schema dialect against a
// provider, before an agent's overrides: §3.3 for tool choice and parallel
// calls, §3.8 for the dialect. Where the handout marks a provider's
// behaviour unverified, the default is the one that cannot break a call:
// no tool_choice (ForceAnswer leaves the tools out), no files, and the full
// common schema transform.
func Defaults(adapter, provider string) (Capabilities, toolschema.Dialect) {
	switch adapter {
	case AdapterAnthropic:
		return Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: provider == ProviderAnthropic}, toolschema.Anthropic
	case AdapterGemini:
		return Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true}, toolschema.GeminiJSONSchema
	case AdapterOpenAIResponses:
		return Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true}, toolschema.OpenAI
	case AdapterBedrockConverse:
		return Capabilities{ParallelToolCalls: true, ToolsWithHistory: true}, toolschema.Bedrock
	}
	// openai_chat, by who serves it.
	switch provider {
	case ProviderOpenAI, ProviderAzure:
		return Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, FileInput: true}, toolschema.OpenAI
	case ProviderDeepSeek:
		return Capabilities{ParallelToolCalls: true}, toolschema.OpenAI
	case ProviderQwen:
		// Parallel calls are off unless asked for; tool_choice none is taken.
		return Capabilities{ToolChoiceNone: true}, toolschema.FullCommon
	case ProviderMoonshot:
		return Capabilities{ParallelToolCalls: true, ToolChoiceNone: true, StrictTools: true}, toolschema.Kimi
	case ProviderGemini:
		return Capabilities{ParallelToolCalls: true, ToolChoiceNone: true}, toolschema.FullCommon
	case ProviderGLM, ProviderOpenRouter, ProviderOllama, ProviderLMStudio, ProviderVLLM:
		return Capabilities{ParallelToolCalls: provider == ProviderGLM || provider == ProviderOpenRouter}, toolschema.FullCommon
	}
	return Capabilities{}, toolschema.FullCommon
}

// MakerOf is the Maker string for an adapter, endpoint and model.
func MakerOf(adapter, baseURL, model string) string {
	return adapter + "|" + strings.TrimRight(baseURL, "/") + "|" + model
}
