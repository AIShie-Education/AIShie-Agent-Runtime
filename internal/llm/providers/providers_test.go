package providers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

func TestNewBuildsEveryAdapter(t *testing.T) {
	for _, name := range Adapters {
		cfg := llm.Config{Adapter: name, Model: "a-model", APIKey: "sk-test-0123456789", Region: "us-east-1"}
		a, err := New(cfg)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if a.Name() != name || a.Model() != "a-model" || !a.Dialect().Valid() || a.Maker() == "" {
			t.Errorf("%s describes itself as %s, %s, %s, %q", name, a.Name(), a.Model(), a.Dialect(), a.Maker())
		}
	}
	if _, err := New(llm.Config{Adapter: "gemini_interactions", Model: "m"}); err == nil || !strings.Contains(err.Error(), "openai_chat") {
		t.Errorf("an adapter the runtime does not have: %v", err)
	}
}

func TestConfigCarriesTheModelSection(t *testing.T) {
	yes, temp := true, 0.3
	m := config.Model{
		Adapter: "openai_chat", Model: "deepseek-chat", BaseURL: "https://api.deepseek.com", Provider: "deepseek",
		Params:       config.ModelParams{MaxOutputTokens: 1500, Temperature: &temp},
		Reasoning:    config.Reasoning{Effort: "low"},
		Capabilities: config.Capabilities{FileInput: &yes, SchemaDialect: "full_common"},
		Headers:      map[string]string{"X-Title": "runtime"},
	}
	client := &http.Client{}
	c := Config(m, "sk-key", client)
	if c.Adapter != "openai_chat" || c.Model != "deepseek-chat" || c.BaseURL != "https://api.deepseek.com" || c.APIKey != "sk-key" ||
		c.Provider != "deepseek" || c.Params.MaxOutputTokens != 1500 || *c.Params.Temperature != 0.3 || c.Reasoning.Effort != "low" ||
		c.Capabilities.FileInput == nil || !*c.Capabilities.FileInput || c.Dialect != "full_common" || c.HTTPClient != client {
		t.Errorf("Config = %+v", c)
	}
	m.Headers["X-Title"] = "changed"
	if c.Headers["X-Title"] != "runtime" {
		t.Error("the headers are shared with the configuration")
	}
	if _, err := New(c); err != nil {
		t.Errorf("New(Config(...)): %v", err)
	}
}
