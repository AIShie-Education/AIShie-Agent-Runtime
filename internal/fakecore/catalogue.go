package fakecore

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// catalogueJSON is GET /v1/tools of the Core the runtime is pinned to, the
// same bytes as internal/core/testdata/catalogue.json (a test holds the two
// together). The fake offers exactly these tools, with these schemas, so a
// client tested against it has seen every name and schema Core will show it.
//
//go:embed testdata/catalogue.json
var catalogueJSON []byte

// idempotencyKey is the argument every write takes over MCP; REST carries it
// in a header.
const idempotencyKey = "idempotency_key"

// maxKeyChars bounds an idempotency key, in characters, as Core's schema
// does.
const maxKeyChars = 200

// toolDef is one tool of the catalogue, as the fake serves it.
type toolDef struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Kind         string          `json:"kind"`
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`

	mcpName string
	write   bool
	// ephemeral: a change that is no action (conversation.draft): no
	// idempotency key, authorized as a write, carried out at once.
	ephemeral bool
	// restOnly: a tool Core serves at its REST route alone, and never
	// lists or takes over MCP: a site service's, the transcription
	// service's (document_text.*) or the site's agent runtime's
	// (agent_runtime.*), which no agent's token may call. service is the
	// scope of the service it is for.
	restOnly bool
	service  string
	// schema is the tool's own input schema, resolved for validation: what
	// a REST body, or MCP arguments without the key, are judged by.
	schema *jsonschema.Resolved
	// props are its top-level properties, which a REST query string is
	// coerced by.
	props map[string]*jsonschema.Schema
	// mcpInput and mcpOutput are the schemas tools/list shows: the input
	// with the idempotency key for a write, and the envelope around the
	// tool's own output.
	mcpInput  json.RawMessage
	mcpOutput json.RawMessage
	// impl is the fake's implementation, nil for a tool it refuses.
	impl *impl
}

// catalogue is every tool, by registry name and by MCP name.
type catalogue struct {
	// raw is what GET /v1/tools serves.
	raw    []byte
	tools  []*toolDef
	byName map[string]*toolDef
	byMCP  map[string]*toolDef
}

// mcpName is a registry name as MCP offers it: grade.submit is grade_submit.
func mcpName(registryName string) string { return strings.ReplaceAll(registryName, ".", "_") }

func loadCatalogue(raw []byte) (*catalogue, error) {
	var doc struct {
		Tools []*toolDef `json:"tools"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fakecore: the catalogue: %w", err)
	}
	c := &catalogue{raw: raw, byName: map[string]*toolDef{}, byMCP: map[string]*toolDef{}}
	for _, t := range doc.Tools {
		if err := t.prepare(); err != nil {
			return nil, fmt.Errorf("fakecore: the catalogue: %s: %w", t.Name, err)
		}
		if _, dup := c.byMCP[t.mcpName]; dup {
			return nil, fmt.Errorf("fakecore: the catalogue: %s is there twice", t.mcpName)
		}
		c.tools = append(c.tools, t)
		c.byName[t.Name] = t
		c.byMCP[t.mcpName] = t
	}
	return c, nil
}

// The site services, each one's tools named by its scope, which Core serves
// over REST alone and to that service alone: the transcription service's
// queue, file, renew and complete, and the agent runtime's agent,
// check_owner, issue_token and revoke_token.
const (
	scopeDocumentText = "document_text"
	scopeAgentRuntime = "agent_runtime"
)

func (t *toolDef) prepare() error {
	t.mcpName = mcpName(t.Name)
	for _, scope := range []string{scopeDocumentText, scopeAgentRuntime} {
		if strings.HasPrefix(t.Name, scope+".") {
			t.restOnly, t.service = true, scope
		}
	}
	switch t.Kind {
	case "read":
	case "write":
		t.write = true
	case kindEphemeral:
		t.ephemeral = true
	default:
		return fmt.Errorf("kind %q is neither read, write nor ephemeral", t.Kind)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(t.InputSchema, &s); err != nil {
		return fmt.Errorf("input schema: %w", err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return fmt.Errorf("input schema: %w", err)
	}
	t.schema, t.props = resolved, s.Properties
	if t.mcpInput, err = mcpInputSchema(t.InputSchema, t.write); err != nil {
		return err
	}
	t.mcpOutput, err = envelopeSchema(t.OutputSchema)
	return err
}

// mcpInputSchema is the tool's own schema; for a write, plus the idempotency
// key, worded as Core words it.
func mcpInputSchema(raw json.RawMessage, write bool) (json.RawMessage, error) {
	if !write {
		return raw, nil
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	props, _ := s["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props[idempotencyKey] = map[string]any{
		"type": "string", "minLength": 1, "maxLength": maxKeyChars,
		"description": "Any string unique to this request. Retrying after a timeout with the SAME key and arguments returns " +
			"the original outcome and does nothing twice. Use a new key only for a new request.",
	}
	s["properties"] = props
	required, _ := s["required"].([]any)
	s["required"] = append(required, idempotencyKey)
	return json.Marshal(s)
}

// envelopeSchema describes the envelope every call returns, with the tool's
// own output as its result, as Core describes it.
func envelopeSchema(result json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"type":     "object",
		"required": []string{"status"},
		"properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"executed", "proposed", "denied", "failed", "rejected", "cancelled", "error"},
				"description": "executed: done. proposed: queued for a person's confirmation, not done. denied, failed: not done. error: the call was never attempted."},
			"action_id":    map[string]any{"type": "string", "format": "uuid", "description": "the recorded action; absent for reads"},
			"review_state": map[string]any{"type": "string"},
			"replayed":     map[string]any{"type": "boolean", "description": "true when this is the stored outcome of an earlier call with the same idempotency_key"},
			"note":         map[string]any{"type": "string", "description": "what to do next, when that is not obvious"},
			"result":       result,
			"error": map[string]any{"type": "object", "properties": map[string]any{
				"code": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "details": map[string]any{"type": "object"}}},
		},
	})
}
