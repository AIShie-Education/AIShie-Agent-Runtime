package toolschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

var testBound = []string{"course_id", "idempotency_key", "revises"}

// TestCatalogueEveryDialect puts every tool of Core's catalogue through every
// dialect and checks what each dialect promises (§3.8, §8.2 item 4).
func TestCatalogueEveryDialect(t *testing.T) {
	for _, tool := range loadCatalogue(t) {
		core := mustDecode(t, tool.InputSchema).(map[string]any)
		for _, d := range allDialects {
			t.Run(tool.mcpName()+"/"+string(d), func(t *testing.T) {
				out, err := Sanitise(tool.InputSchema, d, testBound)
				if err != nil {
					t.Fatal(err)
				}
				again, err := Sanitise(tool.InputSchema, d, testBound)
				if err != nil || !bytes.Equal(out, again) {
					t.Fatalf("not deterministic:\n%s\n%s", out, again)
				}
				checkCompiles(t, out)
				s := mustDecode(t, out).(map[string]any)
				checkRoot(t, s)
				checkDialect(t, d, s)
				checkDescriptions(t, core, s)
			})
		}
	}
}

func checkCompiles(t *testing.T, out json.RawMessage) {
	t.Helper()
	var s jsonschema.Schema
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("does not parse as JSON Schema: %v\n%s", err, out)
	}
	if _, err := s.Resolve(nil); err != nil {
		t.Fatalf("does not compile: %v\n%s", err, out)
	}
}

func checkRoot(t *testing.T, s map[string]any) {
	t.Helper()
	if s["type"] != "object" {
		t.Errorf("root type %v, want object", s["type"])
	}
	props, ok := s["properties"].(map[string]any)
	if !ok {
		t.Fatalf("root has no properties object")
	}
	for _, b := range testBound {
		if _, has := props[b]; has {
			t.Errorf("bound property %s is offered", b)
		}
		if slices.Contains(stringList(s["required"]), b) {
			t.Errorf("bound property %s is required", b)
		}
	}
}

func checkDialect(t *testing.T, d Dialect, root map[string]any) {
	t.Helper()
	r, err := rulesFor(d)
	if err != nil {
		t.Fatal(err)
	}
	walk(root, "$", func(path string, m map[string]any) {
		for k := range m {
			if strings.HasPrefix(k, "$") || k == "definitions" || k == "allOf" {
				t.Errorf("%s: %s left in", path, k)
			}
			if r.keywords != nil && !r.keywords[k] && (k != "nullable" || !r.openAPI) {
				t.Errorf("%s: keyword %s is not one %s takes", path, k, d)
			}
		}
		ts, hasType := types(m)
		if !r.unions {
			if _, has := m["anyOf"]; has {
				t.Errorf("%s: anyOf under %s", path, d)
			}
			if _, has := m["oneOf"]; has {
				t.Errorf("%s: oneOf under %s", path, d)
			}
			nonNull := slices.DeleteFunc(slices.Clone(ts), func(s string) bool { return s == "null" })
			if len(nonNull) > 1 || len(ts) > 1 && !r.strict {
				t.Errorf("%s: union type %v under %s", path, ts, d)
			}
		}
		if r.typed && !hasType {
			if _, has := m["anyOf"]; !has {
				t.Errorf("%s: no type under %s", path, d)
			}
		}
		if _, has := m["format"]; has && !r.format {
			t.Errorf("%s: format under %s", path, d)
		}
		if r.openAPI {
			if _, has := m["additionalProperties"]; has {
				t.Errorf("%s: additionalProperties under %s", path, d)
			}
		}
		if r.strict {
			for _, k := range []string{"not", "if", "then", "else"} {
				if _, has := m[k]; has {
					t.Errorf("%s: %s under %s", path, k, d)
				}
			}
			if isObjectSchema(m) {
				if m["additionalProperties"] != false {
					t.Errorf("%s: additionalProperties %v, want false", path, m["additionalProperties"])
				}
				props, _ := m["properties"].(map[string]any)
				if got, want := stringList(m["required"]), sortedKeys(props); !slices.Equal(got, want) {
					t.Errorf("%s: required %v, want every property %v", path, got, want)
				}
			}
		}
		if d == Kimi {
			for _, k := range []string{"minLength", "maxLength", "minItems", "maxItems", "minimum", "maximum"} {
				if _, has := m[k]; has {
					t.Errorf("%s: %s under kimi", path, k)
				}
			}
		}
	})
}

// checkDescriptions checks that Core's descriptions are kept, only ever
// added to, for every property the model is offered.
func checkDescriptions(t *testing.T, core, out map[string]any) {
	t.Helper()
	cp, _ := core["properties"].(map[string]any)
	op, _ := out["properties"].(map[string]any)
	for name, p := range op {
		want := str(cp[name].(map[string]any)["description"])
		got := str(p.(map[string]any)["description"])
		if !strings.HasPrefix(got, want) {
			t.Errorf("%s: description %q does not begin with Core's %q", name, got, want)
		}
		if nested, ok := cp[name].(map[string]any); ok && isObjectSchema(nested) {
			checkDescriptions(t, nested, p.(map[string]any))
		}
	}
}

// TestCatalogueRoundTrip checks, for every tool and dialect, that arguments a
// model could write come back through Reverse as arguments Core's own schema
// takes (§8.2 item 4): the minimal ones, and a strict model's, with nulls for
// everything it leaves out.
func TestCatalogueRoundTrip(t *testing.T) {
	bound := map[string]any{"course_id": testUUID}
	for _, tool := range loadCatalogue(t) {
		t.Run(tool.mcpName(), func(t *testing.T) {
			for name, args := range map[string]map[string]any{
				"minimal": minimalArgs(t, tool.InputSchema, testBound),
				"strict":  strictArgs(t, tool.InputSchema, testBound),
			} {
				// What the model wrote for course_id is overwritten.
				args["course_id"] = "not-the-course"
				got, err := Reverse(tool.InputSchema, mustJSON(t, args), bound)
				if err != nil {
					t.Fatalf("%s: Reverse: %v", name, err)
				}
				if err := Validate(tool.InputSchema, got); err != nil {
					t.Fatalf("%s: Validate: %v\nargs %s", name, err, got)
				}
				back := mustDecode(t, got).(map[string]any)
				props, _ := mustDecode(t, tool.InputSchema).(map[string]any)["properties"].(map[string]any)
				_, takesCourse := props["course_id"]
				switch {
				case takesCourse && back["course_id"] != testUUID:
					t.Errorf("%s: course_id %v, want the bound one", name, back["course_id"])
				case !takesCourse && back["course_id"] != nil:
					t.Errorf("%s: course_id given to a tool that takes none", name)
				}
			}
		})
	}
}

// TestCatalogueStrictNullsCleaned checks that the nulls a strict model sends
// for optional properties Core takes no null for are dropped: without
// Reverse, Core's schema refuses them.
func TestCatalogueStrictNullsCleaned(t *testing.T) {
	cleaned := 0
	for _, tool := range loadCatalogue(t) {
		args := mustJSON(t, strictArgs(t, tool.InputSchema, testBound))
		rawErr := Validate(tool.InputSchema, withCourse(t, tool.InputSchema, args))
		got, err := Reverse(tool.InputSchema, args, map[string]any{"course_id": testUUID})
		if err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		if err := Validate(tool.InputSchema, got); err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		var ae *ArgumentError
		if errors.As(rawErr, &ae) {
			cleaned++
		}
	}
	// limit, include_archived, treat_ungraded_as_zero and the like: optional
	// and not nullable in dozens of Core's tools.
	t.Logf("%d of 185 tools' strict arguments needed cleaning", cleaned)
	if cleaned < 20 {
		t.Errorf("only %d tools needed cleaning; the test is not exercising Reverse", cleaned)
	}
}

// withCourse adds course_id, where the tool takes one, without Reverse: to
// see what Core would say to the model's arguments as they came.
func withCourse(t *testing.T, schema, args json.RawMessage) json.RawMessage {
	t.Helper()
	m := mustDecode(t, args).(map[string]any)
	if props, _ := mustDecode(t, schema).(map[string]any)["properties"].(map[string]any); props["course_id"] != nil {
		m["course_id"] = testUUID
	}
	return mustJSON(t, m)
}

// TestCatalogueModelRoundTrip is §8.2 item 4 as a model meets it: for every
// tool and dialect, arguments written from the schema the model is shown,
// as a model following it writes them (a UUID where it says UUID, a decimal
// string where it says so, null for what a strict model leaves out), come
// back through Reverse as arguments Core's own schema takes.
func TestCatalogueModelRoundTrip(t *testing.T) {
	bound := map[string]any{"course_id": testUUID}
	for _, tool := range loadCatalogue(t) {
		for _, d := range allDialects {
			shown, err := Sanitise(tool.InputSchema, d, testBound)
			if err != nil {
				t.Fatal(err)
			}
			shownSchema := mustDecode(t, shown).(map[string]any)
			for _, all := range []bool{false, true} {
				args := modelWrites(shownSchema, all)
				got, err := Reverse(tool.InputSchema, mustJSON(t, args), bound)
				if err != nil {
					t.Fatalf("%s/%s: Reverse: %v", tool.mcpName(), d, err)
				}
				if err := Validate(tool.InputSchema, got); err != nil {
					t.Errorf("%s/%s (every property %v): %v\nshown %s\nwrote %s\nsent  %s", tool.mcpName(), d, all, err, shown, mustJSON(t, args), got)
				}
			}
		}
	}
}

// modelWrites is what a model following a sanitised object schema writes:
// every required property, null where a strict schema takes null (what it
// leaves out), and values that do what the descriptions say. With all, it
// writes every property it was shown, with a value, never null: the model
// that fills in everything it may.
func modelWrites(m map[string]any, all bool) map[string]any {
	out := map[string]any{}
	props, _ := m["properties"].(map[string]any)
	names := stringList(m["required"])
	if all {
		names = sortedKeys(props)
	}
	for _, name := range names {
		out[name] = modelValue(props[name], all)
	}
	return out
}

func modelValue(v any, all bool) any {
	m, ok := v.(map[string]any)
	if !ok {
		return "x"
	}
	ts, _ := types(m)
	if slices.Contains(ts, "null") {
		if !all {
			return nil
		}
		ts = slices.DeleteFunc(slices.Clone(ts), func(s string) bool { return s == "null" })
	}
	if enum, ok := m["enum"].([]any); ok && slices.ContainsFunc(enum, func(e any) bool { return e != nil }) {
		return enum[slices.IndexFunc(enum, func(e any) bool { return e != nil })]
	}
	desc := str(m["description"])
	if len(ts) == 0 {
		return "x"
	}
	switch ts[0] {
	case "string":
		switch {
		case m["format"] == "uuid" || strings.Contains(desc, strings.TrimSpace(NoteUUID)):
			return testUUID
		case strings.Contains(desc, strings.TrimSpace(NoteDecimal)):
			return "87.5"
		}
		return "x"
	case "integer":
		return json.Number("1")
	case "number":
		return json.Number("1.5")
	case "boolean":
		return true
	case "array":
		if all && m["items"] != nil {
			return []any{modelValue(m["items"], all)}
		}
		return []any{}
	case "object":
		return modelWrites(m, all)
	}
	return "x"
}
