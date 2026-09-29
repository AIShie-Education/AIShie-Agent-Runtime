package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

// TestLive calls Anthropic's API when LIVE=1 and ANTHROPIC_API_KEY are
// set (make live): a plain answer, one streamed, then one request declaring every tool
// of Core's catalogue at 16 output tokens, so that the API itself checks
// their schemas (§8.2). ANTHROPIC_LIVE_MODEL picks the model.
func TestLive(t *testing.T) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if os.Getenv("LIVE") != "1" || key == "" {
		t.Skip("LIVE=1 and ANTHROPIC_API_KEY run this against the real API")
	}
	model := os.Getenv("ANTHROPIC_LIVE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	a, err := New(llm.Config{Model: model, APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := a.Call(ctx, &llm.Request{
		System:   "Answer in one word.",
		Messages: []llm.Message{llm.UserText("What colour is a clear daytime sky?")},
		Limits:   llm.Limits{MaxOutputTokens: 16},
	})
	if err != nil {
		t.Fatalf("a plain call: %v", err)
	}
	if resp.Stop != llm.StopEnd && resp.Stop != llm.StopMaxTokens {
		t.Errorf("stop %q (%q), want end or max_tokens", resp.Stop, resp.RawStop)
	}
	if resp.Usage.Input == 0 || resp.Usage.Output == 0 || len(resp.Usage.Raw) == 0 || resp.RequestID == "" {
		t.Errorf("usage %+v, request id %q", resp.Usage, resp.RequestID)
	}

	var told strings.Builder
	resp, err = a.Stream(ctx, &llm.Request{
		Messages: []llm.Message{llm.UserText("Count from 1 to 30, with commas.")},
		Limits:   llm.Limits{MaxOutputTokens: 200},
	}, func(d string) { told.WriteString(d) })
	if err != nil {
		t.Fatalf("a streamed call: %v", err)
	}
	if told.String() != resp.Text() || resp.Usage.Input == 0 || resp.Usage.Output == 0 {
		t.Errorf("streamed: told %q, text %q, usage %+v", told.String(), resp.Text(), resp.Usage)
	}

	resp, err = a.Call(ctx, &llm.Request{
		System:   "You are a course tutor.",
		Messages: []llm.Message{llm.UserText("Which tools do you have? Answer in one sentence.")},
		Tools:    catalogueTools(t),
		ToolMode: llm.ToolAuto,
		Limits:   llm.Limits{MaxOutputTokens: 16},
	})
	if err != nil {
		t.Fatalf("a call declaring every tool of the catalogue: %v", err)
	}
	t.Logf("every tool declared: stop %s (%s), usage %+v", resp.Stop, resp.RawStop, resp.Usage)
}

// catalogueTools are Core's tools with the arguments the runtime binds
// (course_id, idempotency_key) removed, and their MCP names.
func catalogueTools(t *testing.T) []llm.Tool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "core", "testdata", "catalogue.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatal(err)
	}
	var tools []llm.Tool
	for _, c := range cat.Tools {
		s := c.InputSchema
		if props, ok := s["properties"].(map[string]any); ok {
			delete(props, "course_id")
			delete(props, "idempotency_key")
		}
		if req, ok := s["required"].([]any); ok {
			s["required"] = slices.DeleteFunc(req, func(v any) bool { return v == "course_id" || v == "idempotency_key" })
		}
		schema, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, llm.Tool{Name: strings.ReplaceAll(c.Name, ".", "_"), Description: c.Description, Schema: schema})
	}
	return tools
}

// Every tool of Core's catalogue can be declared: a name the API takes, and
// a schema that is an object.
func TestCatalogueCanBeDeclared(t *testing.T) {
	tools := catalogueTools(t)
	if len(tools) != 134 {
		t.Fatalf("%d tools in the catalogue, want 134", len(tools))
	}
	a := newAdapter(t, llm.Config{})
	w, err := a.buildRequest(&llm.Request{Messages: []llm.Message{question}, Tools: tools, ToolMode: llm.ToolAuto})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range w.Tools {
		if len(tool.Name) == 0 || len(tool.Name) > 128 || strings.IndexFunc(tool.Name, func(r rune) bool { return r > 127 || !idByte(byte(r)) }) >= 0 {
			t.Errorf("tool name %q is not one the API takes", tool.Name)
		}
	}
	if w.Tools[len(w.Tools)-1].CacheControl == nil || w.Tools[0].CacheControl != nil {
		t.Error("only the last tool is marked for caching")
	}
}
