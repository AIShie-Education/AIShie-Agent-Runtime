package toolschema

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// allDialects is every dialect, in a fixed order.
var allDialects = Dialects

// catTool is one tool of Core's catalogue snapshot, as GET /v1/tools lists it.
type catTool struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func (c catTool) mcpName() string { return strings.ReplaceAll(c.Name, ".", "_") }

func loadCatalogue(t testing.TB) []catTool {
	t.Helper()
	raw, err := os.ReadFile("../core/testdata/catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Tools []catTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatal(err)
	}
	if len(cat.Tools) != 131 {
		t.Fatalf("the snapshot has %d tools, want 131", len(cat.Tools))
	}
	return cat.Tools
}

func catalogueTool(t testing.TB, mcpName string) catTool {
	t.Helper()
	for _, c := range loadCatalogue(t) {
		if c.mcpName() == mcpName {
			return c
		}
	}
	t.Fatalf("no tool %s in the snapshot", mcpName)
	return catTool{}
}

// walk calls fn on every schema in v: properties, items, map values and
// alternatives, with where it is.
func walk(v any, path string, fn func(path string, m map[string]any)) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	fn(path, m)
	if props, ok := m["properties"].(map[string]any); ok {
		for _, k := range sortedKeys(props) {
			walk(props[k], path+"."+k, fn)
		}
	}
	walk(m["items"], path+"[]", fn)
	walk(m["additionalProperties"], path+".*", fn)
	for _, k := range []string{"anyOf", "oneOf", "allOf"} {
		if list, ok := m[k].([]any); ok {
			for _, e := range list {
				walk(e, path+"|"+k, fn)
			}
		}
	}
}

func mustDecode(t testing.TB, raw []byte) any {
	t.Helper()
	v, err := decode(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return v
}

func isObjectSchema(m map[string]any) bool {
	ts, _ := types(m)
	_, hasProps := m["properties"]
	return slices.Contains(ts, "object") || hasProps
}

const testUUID = "0192f3c1-7d2e-7b4a-9c3d-2e1f0a9b8c7d"

// minimalArgs builds arguments Core's schema takes: every required property
// with a plausible value, and nothing else. The bound properties are left
// for Reverse to put back.
func minimalArgs(t testing.TB, schema json.RawMessage, bound []string) map[string]any {
	t.Helper()
	root := mustDecode(t, schema).(map[string]any)
	args := genObject(root)
	for _, b := range bound {
		delete(args, b)
	}
	return args
}

func genObject(m map[string]any) map[string]any {
	out := map[string]any{}
	props, _ := m["properties"].(map[string]any)
	for _, r := range stringList(m["required"]) {
		out[r] = genValue(props[r])
	}
	return out
}

// genValue is a plausible value for a schema: a UUID where it says uuid,
// "87.5" for a decimal, the first enum value, a value within bounds.
func genValue(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return "x"
	}
	if enum, ok := m["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	ts, _ := types(m)
	ts = slices.DeleteFunc(slices.Clone(ts), func(s string) bool { return s == "null" })
	switch {
	case slices.Contains(ts, "number") && slices.Contains(ts, "string"):
		return "87.5"
	case len(ts) == 0:
		return "x"
	}
	switch ts[0] {
	case "string":
		if m["format"] == "uuid" {
			return testUUID
		}
		if n, ok := number(m["minLength"]); ok && n.Num().Int64() > 1 {
			return strings.Repeat("k", int(n.Num().Int64()))
		}
		return "x"
	case "integer":
		return json.Number("1")
	case "number":
		return json.Number("1.5")
	case "boolean":
		return true
	case "array":
		if n, ok := number(m["minItems"]); ok && n.Sign() > 0 {
			return []any{genValue(m["items"])}
		}
		return []any{}
	case "object":
		return genObject(m)
	}
	return "x"
}

// strictArgs is arguments as a strict model writes them: every property
// it was offered, those it leaves out as null, and an array of objects with
// one item written the same way, so that nulls inside items are cleaned too.
func strictArgs(t testing.TB, schema json.RawMessage, bound []string) map[string]any {
	t.Helper()
	args := genStrict(mustDecode(t, schema).(map[string]any))
	for _, b := range bound {
		delete(args, b)
	}
	return args
}

func genStrict(m map[string]any) map[string]any {
	out := map[string]any{}
	props, _ := m["properties"].(map[string]any)
	required := stringList(m["required"])
	for name, p := range props {
		pm, _ := p.(map[string]any)
		items, _ := pm["items"].(map[string]any)
		switch {
		case items != nil && isObjectSchema(items):
			out[name] = []any{genStrict(items)}
		case slices.Contains(required, name) && isObjectSchema(pm):
			out[name] = genStrict(pm)
		case slices.Contains(required, name):
			out[name] = genValue(p)
		default:
			out[name] = nil
		}
	}
	return out
}

func mustJSON(t testing.TB, v any) json.RawMessage {
	t.Helper()
	b, err := encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
