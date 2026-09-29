package bedrock

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// TestLive calls Bedrock itself when LIVE=1 and BEDROCK_MODEL names a model,
// under AWS's default credentials, or AWS_BEARER_TOKEN_BEDROCK as an API
// key, in AWS_REGION: a text turn, a tool round trip, and ForceAnswer over
// the tool history, so that what the golden files hold is what Bedrock
// takes.
func TestLive(t *testing.T) {
	model := os.Getenv("BEDROCK_MODEL")
	if os.Getenv("LIVE") != "1" || model == "" {
		t.Skip("set LIVE=1 and BEDROCK_MODEL to call Bedrock")
	}
	a, err := New(llm.Config{Model: model, Region: os.Getenv("AWS_REGION"), APIKey: os.Getenv("AWS_BEARER_TOKEN_BEDROCK")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	resp, err := a.Call(ctx, &llm.Request{System: "Answer in one word.", Messages: []llm.Message{llm.UserText("Say ok.")},
		ToolMode: llm.ToolAuto, Limits: llm.Limits{MaxOutputTokens: 16}})
	if err != nil {
		t.Fatalf("text: %v", err)
	}
	if (resp.Stop != llm.StopEnd && resp.Stop != llm.StopMaxTokens) || resp.Usage.Input == 0 || len(resp.Usage.Raw) == 0 {
		t.Errorf("text: stop %s (%s), usage %+v", resp.Stop, resp.RawStop, resp.Usage)
	}

	tool := llm.Tool{Name: "course_get", Description: "Get the course: its title and code.",
		Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}
	msgs := []llm.Message{llm.UserText("What is this course's title? Call course_get to find out; do not guess.")}
	req := &llm.Request{System: "You answer questions about a course.", Messages: msgs, Tools: []llm.Tool{tool},
		ToolMode: llm.ToolAuto, Limits: llm.Limits{MaxOutputTokens: 512}}
	resp, err = a.Call(ctx, req)
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	calls := resp.ToolCalls()
	if len(calls) == 0 {
		t.Skipf("the model answered without calling the tool (stop %s): %q", resp.RawStop, resp.Text())
	}
	results := llm.Message{Role: llm.RoleTool}
	for _, c := range calls {
		results.Parts = append(results.Parts, llm.Part{Type: llm.PartToolResult, CallID: c.ID, Name: c.Name,
			Content: `{"status":"executed","result":{"title":"Introduction to Compilers","code":"CS101"}}`})
	}
	req.Messages = append(msgs, llm.Message{Role: llm.RoleAssistant, Parts: resp.Parts}, results)
	for _, mode := range []llm.ToolMode{llm.ToolAuto, llm.ToolNone} {
		req.ToolMode = mode
		resp, err = a.Call(ctx, req)
		if err != nil {
			t.Fatalf("after the results, tool mode %s: %v", mode, err)
		}
		if resp.Stop != llm.StopEnd || resp.Text() == "" {
			t.Errorf("after the results, tool mode %s: stop %s (%s), text %q", mode, resp.Stop, resp.RawStop, resp.Text())
		}
	}
}
