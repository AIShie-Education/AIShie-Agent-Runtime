package gemini

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

// liveAdapter is the adapter against the real Gemini API, for make live:
// LIVE=1 and GEMINI_API_KEY set, GEMINI_MODEL naming the model
// (gemini-2.5-flash by default). Without them the test is skipped.
func liveAdapter(t *testing.T, effort string) *Adapter {
	t.Helper()
	key := os.Getenv("GEMINI_API_KEY")
	if os.Getenv("LIVE") != "1" || key == "" {
		t.Skip("LIVE=1 and GEMINI_API_KEY run this against the real Gemini API")
	}
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}
	a, err := New(llm.Config{Adapter: llm.AdapterGemini, Model: model, APIKey: key, Reasoning: llm.Reasoning{Effort: effort}})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A loop against the real API: a call, its result, the answer, and a forced
// last answer with the tools still declared. With thinking on, the second
// call only succeeds if the signatures went back where Gemini wants them.
func TestLiveLoop(t *testing.T) {
	a := liveAdapter(t, "low")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req := &llm.Request{
		System: "You are a course tutor. Look facts up with the tools; never guess them.",
		Messages: []llm.Message{
			llm.UserText("When is the assignment titled HW3 due? Look it up."),
		},
		Tools: []llm.Tool{
			{Name: "assignment_get", Description: "Gets one of the course's assignments by its title.",
				Schema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","description":"the assignment's title"}},"required":["title"],"additionalProperties":false}`)},
			{Name: "course_get", Description: "Gets the course.", Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		},
		ToolMode: llm.ToolAuto,
		Limits:   llm.Limits{MaxOutputTokens: 2048},
	}
	first, err := a.Call(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	calls := first.ToolCalls()
	if first.Stop != llm.StopToolCalls || len(calls) == 0 {
		t.Fatalf("stop %s (%s), %d calls; want a call", first.Stop, first.RawStop, len(calls))
	}
	results := llm.Message{Role: llm.RoleTool}
	for _, c := range calls {
		results.Parts = append(results.Parts, llm.Part{Type: llm.PartToolResult, CallID: c.ID, Name: c.Name,
			Content: `{"status":"executed","result":{"assignment":{"title":"HW3","due_at":"2026-10-02T17:00:00Z"}}}`})
	}
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Parts: first.Parts}, results)

	second, err := a.Call(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Stop != llm.StopEnd && second.Stop != llm.StopToolCalls {
		t.Fatalf("stop %s (%s)", second.Stop, second.RawStop)
	}
	if second.Usage.Input == 0 || len(second.Usage.Raw) == 0 {
		t.Fatalf("usage %+v", second.Usage)
	}

	req.ToolMode = llm.ToolNone
	last, err := a.Call(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if last.Stop != llm.StopEnd || len(last.ToolCalls()) > 0 || !strings.Contains(last.Text(), "2") {
		t.Fatalf("forced answer: stop %s (%s), text %q", last.Stop, last.RawStop, last.Text())
	}
}

// Every tool of Core's catalogue declared, in requests of at most
// livetest.MaxTools, at 16 output tokens, so that the provider checks
// every schema (§8.2). These are Core's own schemas, unsanitised:
// parametersJsonSchema must take them as they are, and whatever the
// sanitiser makes of them is simpler still.
func TestLiveEveryToolDeclared(t *testing.T) {
	a := liveAdapter(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for batch := range slices.Chunk(livetest.CatalogueTools(t, false), livetest.MaxTools) {
		req := &llm.Request{Messages: []llm.Message{llm.UserText("Say hello.")}, Tools: batch, ToolMode: llm.ToolAuto,
			Limits: llm.Limits{MaxOutputTokens: 16}}
		if _, err := a.Call(ctx, req); err != nil {
			t.Fatalf("%d tools declared, from %s: %v", len(batch), batch[0].Name, err)
		}
	}
}
