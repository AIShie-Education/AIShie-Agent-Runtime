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

// TestEveryAdapterKnowsItsFileLimits: every adapter says how large a PDF,
// and of how many pages, its provider takes as a file, as each documents
// it: past them, the runtime gives the model the PDF's text instead.
func TestEveryAdapterKnowsItsFileLimits(t *testing.T) {
	want := map[string]llm.FileLimits{
		llm.AdapterOpenAIChat:      {PDFBytes: 32 << 20, PDFPages: 100},
		llm.AdapterOpenAIResponses: {PDFBytes: 32 << 20, PDFPages: 100},
		llm.AdapterAnthropic:       {PDFBytes: 18 << 20, PDFPages: 100},
		llm.AdapterGemini:          {PDFBytes: 48 << 20, PDFPages: 1000},
		llm.AdapterBedrockConverse: {PDFBytes: 4_500_000, PDFPages: 100},
	}
	for _, name := range Adapters {
		a, err := New(llm.Config{Adapter: name, Model: "a-model", APIKey: "sk-test-0123456789", Region: "us-east-1"})
		if err != nil {
			t.Fatal(err)
		}
		fl, ok := a.(llm.FileLimiter)
		if !ok {
			t.Errorf("%s does not say its file limits", name)
			continue
		}
		if got := fl.FileLimits(); got != want[name] {
			t.Errorf("%s: %+v, want %+v", name, got, want[name])
		}
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
