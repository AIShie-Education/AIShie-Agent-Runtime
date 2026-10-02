package openairesponses

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/livetest"
)

// TestLive runs the adapter against the real APIs whose keys are set, when
// LIVE=1 (make live): OpenAI with OPENAI_API_KEY (the model is
// OPENAI_RESPONSES_MODEL, gpt-5-mini by default), and Azure with
// AZURE_OPENAI_BASE_URL, AZURE_OPENAI_API_KEY and AZURE_OPENAI_DEPLOYMENT.
// It spends a few thousand tokens of a cheap model.
func TestLive(t *testing.T) {
	if os.Getenv("LIVE") != "1" {
		t.Skip("LIVE=1 runs the adapters against the real providers")
	}
	var cfgs []llm.Config
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		model := os.Getenv("OPENAI_RESPONSES_MODEL")
		if model == "" {
			model = "gpt-5-mini"
		}
		cfgs = append(cfgs, llm.Config{Model: model, APIKey: key, Reasoning: llm.Reasoning{Effort: "low"}})
	}
	if base, key, dep := os.Getenv("AZURE_OPENAI_BASE_URL"), os.Getenv("AZURE_OPENAI_API_KEY"), os.Getenv("AZURE_OPENAI_DEPLOYMENT"); base != "" && key != "" && dep != "" {
		cfgs = append(cfgs, llm.Config{Model: dep, BaseURL: base, APIKey: key, Provider: llm.ProviderAzure})
	}
	if len(cfgs) == 0 {
		t.Skip("no OPENAI_API_KEY, nor AZURE_OPENAI_BASE_URL, AZURE_OPENAI_API_KEY and AZURE_OPENAI_DEPLOYMENT")
	}
	for _, cfg := range cfgs {
		a, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(a.Provider()+"/"+a.Model(), func(t *testing.T) {
			t.Run("text", func(t *testing.T) { liveText(t, a) })
			t.Run("tool round trip", func(t *testing.T) { liveToolRoundTrip(t, a) })
			t.Run("every tool declared", func(t *testing.T) { liveEveryTool(t, a) })
		})
	}
}

func liveCall(t *testing.T, a *Adapter, req *llm.Request) *llm.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := a.Call(ctx, req)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	t.Logf("stop %s (%s), usage %+v", resp.Stop, resp.RawStop, resp.Usage)
	if resp.Usage.Input == 0 || len(resp.Usage.Raw) == 0 {
		t.Errorf("no usage: %+v", resp.Usage)
	}
	return resp
}

func liveText(t *testing.T, a *Adapter) {
	resp := liveCall(t, a, &llm.Request{
		System:   "Answer in one short sentence.",
		Messages: []llm.Message{llm.UserText("What is 2 + 2?")},
		Limits:   llm.Limits{MaxOutputTokens: 1000},
	})
	if resp.Stop != llm.StopEnd || !strings.Contains(resp.Text(), "4") {
		t.Errorf("stop %s, text %q; want end with 4", resp.Stop, resp.Text())
	}
}

// liveToolRoundTrip calls a tool, answers it, and sends the model's own
// turn back with its reasoning: the API must take it.
func liveToolRoundTrip(t *testing.T, a *Adapter) {
	tools := []llm.Tool{{
		Name:        "grade_get",
		Description: "Reads the student's grade for an assignment.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"assignment":{"type":"string"}},"required":["assignment"],"additionalProperties":false}`),
	}}
	req := &llm.Request{
		System:   "You are a tutor. Use the tools to look things up; answer in one sentence.",
		Messages: []llm.Message{llm.UserText("What was my grade on HW3?")},
		Tools:    tools,
		ToolMode: llm.ToolAuto,
		Limits:   llm.Limits{MaxOutputTokens: 2000},
	}
	first := liveCall(t, a, req)
	calls := first.ToolCalls()
	if first.Stop != llm.StopToolCalls || len(calls) == 0 {
		t.Fatalf("stop %s with %d calls; want a tool call", first.Stop, len(calls))
	}
	var results []llm.Part
	for _, c := range calls {
		results = append(results, llm.Part{Type: llm.PartToolResult, CallID: c.ID, Name: c.Name, Content: `{"status":"executed","result":{"points":"8.5","max_points":"10"}}`})
	}
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Parts: first.Parts}, llm.Message{Role: llm.RoleTool, Parts: results})
	second := liveCall(t, a, req)
	if second.Stop != llm.StopEnd || !strings.Contains(second.Text(), "8.5") {
		t.Errorf("stop %s, text %q; want end with 8.5", second.Stop, second.Text())
	}
	req.ToolMode = llm.ToolNone
	forced := liveCall(t, a, req)
	if len(forced.ToolCalls()) != 0 {
		t.Errorf("ForceAnswer called %d tools", len(forced.ToolCalls()))
	}
}

// liveEveryTool declares every tool of the pinned catalogue, bound as the
// runtime binds them, in requests of at most livetest.MaxTools, so that
// the provider checks every schema.
func liveEveryTool(t *testing.T, a *Adapter) {
	for batch := range slices.Chunk(livetest.CatalogueTools(t, true), livetest.MaxTools) {
		liveCall(t, a, &llm.Request{
			System:   "Say OK.",
			Messages: []llm.Message{llm.UserText("OK?")},
			Tools:    batch,
			ToolMode: llm.ToolAuto,
			Limits:   llm.Limits{MaxOutputTokens: 16},
		})
	}
}
