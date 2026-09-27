// Package providers builds a model adapter from an agent's configuration:
// the one place that knows every adapter, so that nothing else imports
// them.
package providers

import (
	"fmt"
	"net/http"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/anthropic"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/bedrock"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/gemini"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/openaichat"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/openairesponses"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// Adapters are the adapters this runtime has, by their names in
// configuration. gemini_interactions is not among them yet (§3.10, M3).
var Adapters = []string{
	llm.AdapterOpenAIChat, llm.AdapterOpenAIResponses, llm.AdapterAnthropic,
	llm.AdapterGemini, llm.AdapterBedrockConverse,
}

// New builds the adapter cfg names.
func New(cfg llm.Config) (llm.Adapter, error) {
	switch cfg.Adapter {
	case llm.AdapterOpenAIChat:
		return openaichat.New(cfg)
	case llm.AdapterOpenAIResponses:
		return openairesponses.New(cfg)
	case llm.AdapterAnthropic:
		return anthropic.New(cfg)
	case llm.AdapterGemini:
		return gemini.New(cfg)
	case llm.AdapterBedrockConverse:
		return bedrock.New(cfg)
	}
	return nil, fmt.Errorf("providers: no adapter %q; the adapters are %v", cfg.Adapter, Adapters)
}

// Config is the llm.Config for a model section of an agent's
// configuration, with its key resolved and the HTTP client every call goes
// through (the egress proxy's).
func Config(m config.Model, key string, client *http.Client) llm.Config {
	c := llm.Config{
		Adapter:  m.Adapter,
		Model:    m.Model,
		BaseURL:  m.BaseURL,
		APIKey:   key,
		Provider: m.Provider,
		Region:   m.Region,
		Params: llm.Params{
			MaxOutputTokens: m.Params.MaxOutputTokens,
			Temperature:     m.Params.Temperature,
			TopP:            m.Params.TopP,
		},
		Reasoning: llm.Reasoning{Effort: m.Reasoning.Effort},
		Capabilities: llm.CapabilityOverrides{
			ParallelToolCalls: m.Capabilities.ParallelToolCalls,
			StrictTools:       m.Capabilities.StrictTools,
			ToolChoiceNone:    m.Capabilities.ToolChoiceNone,
			FileInput:         m.Capabilities.FileInput,
		},
		Dialect:    toolschema.Dialect(m.Capabilities.SchemaDialect),
		HTTPClient: client,
	}
	if len(m.Headers) > 0 {
		c.Headers = make(map[string]string, len(m.Headers))
		for k, v := range m.Headers {
			c.Headers[k] = v
		}
	}
	return c
}
