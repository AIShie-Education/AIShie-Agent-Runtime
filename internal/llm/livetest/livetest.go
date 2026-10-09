// Package livetest holds what the adapters' live tests (make live) share:
// Core's tools as the pinned catalogue has them, which one live run
// declares to each real provider, so that the provider itself checks every
// schema (Core's docs/agent-runtime.md §8.2 item 4).
package livetest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// MaxTools is the most tools one live request declares. OpenAI's APIs take
// at most 128 functions in a request, and the pinned catalogue has more
// (185 at AIShie-Core b5d6b43, its site services' own among them), so
// every tool is declared across requests of at most this many, to every
// provider alike (slices.Chunk). No seat is ever offered nearly as many:
// the runtime offers a model the tools its gates allow (internal/toolset).
const MaxTools = 128

// bound are the arguments the runtime sets and a model never sees
// (toolset.Bound, which this package does not import).
var bound = []string{"course_id", "idempotency_key", "revises"}

// CatalogueTools are the tools of the pinned catalogue
// (internal/core/testdata/catalogue.json) under their MCP names, with
// Core's schemas as they are; with unbound, without the arguments the
// runtime binds (course_id, idempotency_key, revises), as the runtime
// declares them. It is called from an adapter's tests, which run in the adapter's
// directory (internal/llm/<adapter>), so the catalogue is found from there,
// as the other tests find it, and under -trimpath too.
func CatalogueTools(t testing.TB, unbound bool) []llm.Tool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "core", "testdata", "catalogue.json"))
	if err != nil {
		t.Fatalf("livetest: the catalogue, from an adapter's directory: %v", err)
	}
	var cat struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatal(err)
	}
	tools := make([]llm.Tool, 0, len(cat.Tools))
	for _, c := range cat.Tools {
		schema := c.InputSchema
		if unbound {
			schema = unbind(t, schema)
		}
		tools = append(tools, llm.Tool{Name: strings.ReplaceAll(c.Name, ".", "_"), Description: c.Description, Schema: schema})
	}
	return tools
}

// unbind is schema without the bound arguments, among its properties or
// the required.
func unbind(t testing.TB, schema json.RawMessage) json.RawMessage {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatal(err)
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for _, b := range bound {
			delete(props, b)
		}
	}
	if req, ok := s["required"].([]any); ok {
		s["required"] = slices.DeleteFunc(req, func(v any) bool { name, _ := v.(string); return slices.Contains(bound, name) })
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
