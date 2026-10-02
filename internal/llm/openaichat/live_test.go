package openaichat

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

// TestLive calls the real providers whose keys are set, with LIVE=1 (make
// live, and the nightly workflow): a cheap call, a tool call's round trip,
// an answer streamed, and, for OpenAI, every tool of the pinned catalogue
// declared at 16 output tokens, so that the provider itself checks the
// schemas.
func TestLive(t *testing.T) {
	if os.Getenv("LIVE") != "1" {
		t.Skip("set LIVE=1 to call the real providers")
	}
	for _, p := range []struct {
		name, keyEnv, modelEnv, model, base string
		everyTool                           bool
	}{
		{"openai", "OPENAI_API_KEY", "OPENAI_MODEL", "gpt-4.1-mini", "", true},
		{"deepseek", "DEEPSEEK_API_KEY", "DEEPSEEK_MODEL", "deepseek-chat", deepseekBase, false},
		{"gemini_compatible", "GEMINI_API_KEY", "GEMINI_MODEL", "gemini-2.5-flash", geminiBase, false},
	} {
		t.Run(p.name, func(t *testing.T) {
			key := os.Getenv(p.keyEnv)
			if key == "" {
				t.Skipf("%s is not set", p.keyEnv)
			}
			model := os.Getenv(p.modelEnv)
			if model == "" {
				model = p.model
			}
			a, err := New(llm.Config{Model: model, BaseURL: p.base, APIKey: key})
			if err != nil {
				t.Fatal(err)
			}
			liveRoundTrip(t, a)
			liveStream(t, a)
			if p.everyTool {
				liveEveryTool(t, a)
			}
		})
	}
}

func liveCall(t *testing.T, a *Adapter, req *llm.Request) *llm.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := a.Call(ctx, req)
	if err != nil {
		t.Fatalf("%s: %v", a.Model(), err)
	}
	if resp.Usage.Estimated || resp.Usage.Input == 0 {
		t.Errorf("usage was not reported: %+v", resp.Usage)
	}
	t.Logf("stop %s (%s), usage %d in, %d out", resp.Stop, resp.RawStop, resp.Usage.Input, resp.Usage.Output)
	return resp
}

func liveRoundTrip(t *testing.T, a *Adapter) {
	tool := llm.Tool{Name: "grade_get", Description: "The caller's grade on one assignment.",
		Schema: json.RawMessage(`{"type":"object","properties":{"assignment":{"type":"string","description":"the assignment's name"}},"required":["assignment"]}`)}
	req := &llm.Request{
		System:   "You answer students' questions about their grades. Look grades up with the tool; never guess.",
		Messages: []llm.Message{llm.UserText("What is my grade on HW3?")},
		Tools:    []llm.Tool{tool}, ToolMode: llm.ToolAuto, Limits: llm.Limits{MaxOutputTokens: 1000},
	}
	resp := liveCall(t, a, req)
	calls := resp.ToolCalls()
	if resp.Stop != llm.StopToolCalls || len(calls) == 0 {
		t.Fatalf("no tool call: stop %s, text %q", resp.Stop, resp.Text())
	}
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Parts: resp.Parts})
	var results []llm.Part
	for _, c := range calls {
		results = append(results, llm.Part{Type: llm.PartToolResult, CallID: c.ID, Name: c.Name,
			Content: `{"status":"executed","result":{"assignment":"HW3","score":"8.5","out_of":"10"}}`})
	}
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleTool, Parts: results})
	req.ToolMode = llm.ToolNone // the last turn of a spent budget: text only
	final := liveCall(t, a, req)
	if final.Stop != llm.StopEnd || !strings.Contains(final.Text(), "8.5") {
		t.Errorf("final answer: stop %s, text %q", final.Stop, final.Text())
	}
}

// liveStream streams an answer long enough to come in pieces: the pieces
// are its text, and the provider reports its usage in the last chunk.
func liveStream(t *testing.T, a *Adapter) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var told []string
	resp, err := a.Stream(ctx, &llm.Request{
		Messages: []llm.Message{llm.UserText("Count from 1 to 30, with commas.")}, Limits: llm.Limits{MaxOutputTokens: 300},
	}, func(d string) { told = append(told, d) })
	if err != nil {
		t.Fatalf("%s, streamed: %v", a.Model(), err)
	}
	if strings.Join(told, "") != resp.Text() || len(told) < 2 {
		t.Errorf("streamed in %d pieces %q; the text is %q", len(told), told, resp.Text())
	}
	if resp.Usage.Estimated || resp.Usage.Input == 0 {
		t.Errorf("a streamed call's usage was not reported: %+v", resp.Usage)
	}
}

// liveEveryTool declares every tool of the pinned catalogue, in requests
// of at most livetest.MaxTools: Core's schemas as they are, since OpenAI's
// non-strict mode takes JSON Schema whole, so this checks the
// declarations' shape, not the sanitiser.
func liveEveryTool(t *testing.T, a *Adapter) {
	for batch := range slices.Chunk(livetest.CatalogueTools(t, false), livetest.MaxTools) {
		liveCall(t, a, &llm.Request{
			Messages: []llm.Message{llm.UserText("Say OK.")},
			Tools:    batch, ToolMode: llm.ToolAuto, Limits: llm.Limits{MaxOutputTokens: 16},
		})
	}
}
