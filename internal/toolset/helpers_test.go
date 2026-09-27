package toolset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
)

const (
	courseID = "0192f3c1-7d2e-7b4a-9c3d-2e1f0a9b8c7d"
	docID    = "0192f3c1-aaaa-7b4a-9c3d-2e1f0a9b8c7d"
)

// snapshot is Core's catalogue snapshot as the toolset reads it: what
// FromCore makes of core.Catalogue, here without it.
func snapshot(t testing.TB) *Catalogue {
	t.Helper()
	raw, err := os.ReadFile("../core/testdata/catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Kind        string          `json:"kind"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	out := &Catalogue{Hash: hex.EncodeToString(sum[:]), Tools: map[string]CatalogueTool{}}
	for _, tool := range cat.Tools {
		name := strings.ReplaceAll(tool.Name, ".", "_")
		out.Tools[name] = CatalogueTool{Name: name, Description: tool.Description, Kind: tool.Kind, InputSchema: tool.InputSchema}
	}
	return out
}

// Seats' perms, as me_memberships gives them.
var (
	// tutorPerms is a course tutor's: the material, nobody's work (§2.7).
	tutorPerms = map[string]string{
		"conversation_answer": "autonomous", "document_read": "autonomous",
		"submission_read": "denied", "grade_read": "denied",
	}
	// delegatePerms is a student's own agent's: its principal's work too
	// (§2.6).
	delegatePerms = map[string]string{
		"conversation_answer": "autonomous", "document_read": "autonomous",
		"submission_read": "autonomous", "grade_read": "autonomous",
	}
)

// fakeCore is a core.Caller that plays what a test scripts, and records
// every call.
type fakeCore struct {
	mu      sync.Mutex
	calls   []fakeCall
	respond func(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error)
}

type fakeCall struct {
	tool     string
	args     map[string]any
	priority core.Priority
}

func (f *fakeCore) Call(ctx context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
	var m map[string]any
	_ = json.Unmarshal(args, &m)
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{tool: tool, args: m, priority: core.PriorityOf(ctx)})
	f.mu.Unlock()
	if f.respond == nil {
		return executed(`{}`), nil
	}
	return f.respond(ctx, tool, args)
}

func (f *fakeCore) recorded() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func executed(result string) *core.Envelope {
	return &core.Envelope{Status: core.StatusExecuted, Result: json.RawMessage(result)}
}

func call(id, name, args string) llm.Part {
	return llm.Part{Type: llm.PartToolCall, ID: id, Name: name, Args: json.RawMessage(args)}
}

// contentOf decodes a result's content, which must be one JSON object.
func contentOf(t testing.TB, p llm.Part) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(p.Content), &m); err != nil {
		t.Fatalf("content is not JSON: %v\n%s", err, p.Content)
	}
	return m
}

func errorOf(t testing.TB, p llm.Part) (code, message string) {
	t.Helper()
	e, _ := contentOf(t, p)["error"].(map[string]any)
	code, _ = e["code"].(string)
	message, _ = e["message"].(string)
	return code, message
}
