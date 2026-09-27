package toolschema

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
)

// Reverse turns a model's arguments back into Core's (§3.8 step 4).
//
//   - They must be one JSON object; anything else is an *ArgumentError, for
//     the model to correct.
//   - A null where Core's schema takes no null is dropped, at any depth: the
//     strict dialects make every optional property nullable, and models
//     then send null for what they leave out. A null inside an array is
//     left for Validate to report, since dropping it would move the items
//     after it.
//   - A property Core requires but takes as null, which the model was told
//     it may leave out, is given as null when it is left out.
//   - The bound values are put back, over anything the model wrote for them
//     (course_id is always the conversation's course). One the schema has no
//     property for, and takes no other properties, is left out instead:
//     Core would refuse the call for it.
//
// Numbers pass through exactly as written (json.Number), and the output's
// keys are sorted. The result is not validated: Validate does that against
// Core's schema.
func Reverse(coreSchema, modelArgs json.RawMessage, bound map[string]any) (json.RawMessage, error) {
	schema, err := prepare(coreSchema)
	if err != nil {
		return nil, err
	}
	args, err := argsObject(modelArgs)
	if err != nil {
		return nil, err
	}
	clean(args, []any{schema})
	for _, k := range sortedKeys(bound) {
		if acceptsProperty(schema, k) {
			args[k] = bound[k]
		} else {
			delete(args, k)
		}
	}
	return encode(args)
}

// argsObject parses a model's arguments as one JSON object. No arguments at
// all are an empty object, as Core takes them.
func argsObject(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	v, err := decode(raw)
	if err != nil {
		return nil, &ArgumentError{Msg: "the arguments are not a JSON object: they do not parse as JSON"}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &ArgumentError{Msg: "the arguments are not a JSON object: they are " + article(valueType(v))}
	}
	return m, nil
}

func article(kind string) string {
	switch kind {
	case "array", "object":
		return "an " + kind
	case "null":
		return "null"
	}
	return "a " + kind
}

// clean drops the nulls that none of the schemas v may be read against
// take, and fills in the nulls of required nullable properties.
func clean(v any, schemas []any) {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			var cands []any
			anything := false
			for _, s := range schemas {
				c, a := propertySchemas(s, k)
				cands = append(cands, c...)
				anything = anything || a
			}
			if x[k] == nil {
				if !anything && !slices.ContainsFunc(cands, allowsNull) {
					delete(x, k)
				}
				continue
			}
			if !anything {
				clean(x[k], cands)
			}
		}
		// Which required nullable properties to fill is clear only when
		// there is one schema to read.
		if len(schemas) == 1 {
			fillNulls(x, schemas[0])
		}
	case []any:
		var items []any
		for _, s := range schemas {
			items = append(items, itemSchemas(s)...)
		}
		for _, e := range x {
			clean(e, items)
		}
	}
}

// propertySchemas is the schemas an object's property k is read against
// under s; anything is true when s puts no bound on it.
func propertySchemas(s any, k string) (cands []any, anything bool) {
	m, ok := objectOf(s)
	if !ok {
		return nil, s != false
	}
	alts := alternatives(m)
	for _, a := range alts {
		c, free := propertySchemas(a, k)
		cands = append(cands, c...)
		anything = anything || free
	}
	props, _ := objectOf(m["properties"])
	if p, has := props[k]; has {
		return append(cands, p), anything
	}
	matched := false
	if pp, has := objectOf(m["patternProperties"]); has {
		for _, pattern := range sortedKeys(pp) {
			re, err := regexp.Compile(pattern)
			if err != nil || re.MatchString(k) {
				cands = append(cands, pp[pattern])
				matched = true
			}
		}
	}
	if matched {
		return cands, anything
	}
	ap, hasAP := m["additionalProperties"]
	switch {
	case hasAP && ap == false:
	case hasAP && ap != true:
		cands = append(cands, ap)
	case len(alts) == 0 || props != nil || hasAP:
		// Only a schema that says nothing of its properties, beside its
		// alternatives, leaves them to its alternatives.
		anything = true
	}
	return cands, anything
}

// itemSchemas is the schemas an array's items are read against under s.
func itemSchemas(s any) []any {
	m, ok := objectOf(s)
	if !ok {
		return []any{s}
	}
	var out []any
	switch it := m["items"].(type) {
	case nil:
	case []any:
		out = append(out, it...)
	default:
		out = append(out, it)
	}
	if pi, ok := m["prefixItems"].([]any); ok {
		out = append(out, pi...)
	}
	for _, a := range alternatives(m) {
		out = append(out, itemSchemas(a)...)
	}
	if len(out) == 0 {
		return []any{true}
	}
	return out
}

func alternatives(m map[string]any) []any {
	var out []any
	for _, k := range []string{"anyOf", "oneOf"} {
		if list, ok := m[k].([]any); ok {
			out = append(out, list...)
		}
	}
	return out
}

// allowsNull reports whether null is valid under s.
func allowsNull(s any) bool {
	m, ok := objectOf(s)
	if !ok {
		return s != false
	}
	if ts, has := types(m); has && !slices.Contains(ts, "null") {
		return false
	}
	if enum, has := m["enum"].([]any); has && !slices.Contains(enum, nil) {
		return false
	}
	if c, has := m["const"]; has && c != nil {
		return false
	}
	if alts := alternatives(m); len(alts) > 0 {
		return slices.ContainsFunc(alts, allowsNull)
	}
	return true
}

// explicitNull reports whether s names null among what it takes, as
// Sanitise reads it: such a property was offered to the model as optional.
func explicitNull(s any) bool {
	m, ok := objectOf(s)
	if !ok {
		return false
	}
	if ts, has := types(m); has && slices.Contains(ts, "null") {
		return true
	}
	if enum, has := m["enum"].([]any); has && slices.Contains(enum, nil) {
		return true
	}
	return slices.ContainsFunc(alternatives(m), isNullSchema)
}

// fillNulls gives a required property Core takes as null the null the model
// was told it could leave out.
func fillNulls(x map[string]any, s any) {
	m, ok := objectOf(s)
	if !ok {
		return
	}
	props, _ := objectOf(m["properties"])
	for _, r := range stringList(m["required"]) {
		if _, has := x[r]; has {
			continue
		}
		if p, declared := props[r]; declared && explicitNull(p) && allowsNull(p) {
			x[r] = nil
		}
	}
}

// acceptsProperty reports whether an object under s may hold property k.
func acceptsProperty(s any, k string) bool {
	m, ok := objectOf(s)
	if !ok {
		return s != false
	}
	if props, ok := objectOf(m["properties"]); ok {
		if _, has := props[k]; has {
			return true
		}
	}
	return m["additionalProperties"] != false
}
