package toolschema

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// What the sanitiser adds to descriptions. A description is only ever added
// to, never replaced: Core's words stay first.
const (
	// NoteOptional marks a property Core takes as null or not at all
	// (["null", X] in Core's schema, §3.8 step 2), and under the strict
	// dialects every property the model may leave out.
	NoteOptional = " (optional; omit or null)"
	// NoteDecimal marks a decimal Core takes as a number or a string
	// (["number","string"]): the model is given a string, which is exact.
	NoteDecimal = ` (a decimal number as a string, e.g. "87.5")`
	// NoteUUID is format uuid, said in words where a dialect refuses format.
	NoteUUID = " (UUID)"

	noteOrNull   = " (or null)"
	noteAny      = " (any JSON value, given here as a string)"
	noteForms    = " (one of several forms)"
	noteEmptyMap = " (give {} here)"
)

// Dialects is every dialect, for checks that must hold under all of them.
var Dialects = []Dialect{OpenAI, OpenAIStrict, Anthropic, GeminiJSONSchema, GeminiOpenAPI, Bedrock, FullCommon, Kimi}

// rules are what a dialect takes.
type rules struct {
	// unions: type lists and anyOf are kept. Without it they are merged
	// into one schema (mergeAlternatives, collapse).
	unions bool
	// oneOf is kept as oneOf; where unions are kept without it, it becomes
	// anyOf, which says the same to a model.
	oneOf bool
	// format is kept; without it, it is moved into the description.
	format bool
	// strict: every property required, the ones the model may leave out
	// nullable, additionalProperties false on every object.
	strict bool
	// openAPI: OpenAPI 3.0's nullable, and no additionalProperties.
	openAPI bool
	// maps: an additionalProperties schema (a map, as Core's perms are) is
	// kept. Without it, a map is left out where Core does not require it,
	// and given as an empty object where it does.
	maps bool
	// typed: every schema has a type; an untyped one is given as a string,
	// which Core, taking anything there, takes too.
	typed bool
	// keywords, when set, are the only keywords kept.
	keywords map[string]bool
}

func keywordSet(ks ...string) map[string]bool {
	m := make(map[string]bool, len(ks))
	for _, k := range ks {
		m[k] = true
	}
	return m
}

var (
	// simpleKeywords are the full common transform's: plain JSON Schema that
	// every chat template renders, and no more.
	simpleKeywords = keywordSet("type", "description", "properties", "required", "additionalProperties",
		"items", "enum", "pattern", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
		"minLength", "maxLength", "minItems", "maxItems")
	// kimiKeywords are the simple ones less every bound (§3.8: Kimi's strict
	// mode, as DeepSeek's, refuses them).
	kimiKeywords = keywordSet("type", "description", "properties", "required", "additionalProperties",
		"items", "enum", "pattern")
	// strictKeywords are those OpenAI's strict mode documents as supported.
	// minLength and maxLength are not among them, so they are dropped: the
	// arguments are still checked against Core's schema before any call.
	strictKeywords = keywordSet("type", "description", "properties", "required", "additionalProperties",
		"items", "enum", "const", "anyOf", "pattern", "format", "minimum", "maximum", "exclusiveMinimum",
		"exclusiveMaximum", "multipleOf", "minItems", "maxItems")
	// openAPIKeywords are the part of OpenAPI 3.0's Schema that Gemini's
	// parameters take (nullable is added after this filter).
	openAPIKeywords = keywordSet("type", "description", "enum", "properties", "required", "items",
		"pattern", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems")
)

func rulesFor(d Dialect) (rules, error) {
	switch d {
	case OpenAI, Anthropic, GeminiJSONSchema:
		return rules{unions: true, oneOf: true, format: true, maps: true}, nil
	case OpenAIStrict:
		return rules{unions: true, format: true, strict: true, typed: true, keywords: strictKeywords}, nil
	case GeminiOpenAPI:
		return rules{openAPI: true, typed: true, keywords: openAPIKeywords}, nil
	case FullCommon, Bedrock:
		// Bedrock is unverified per model family (§3.8), so it takes the
		// full common transform, format included: moving format into the
		// description cannot break a call, and keeping it might.
		return rules{maps: true, typed: true, keywords: simpleKeywords}, nil
	case Kimi:
		return rules{strict: true, typed: true, keywords: kimiKeywords}, nil
	}
	return rules{}, schemaError("unknown dialect %q", string(d))
}

// Sanitise is the schema a model is given for one tool (Core's
// docs/agent-runtime.md §3.8): Core's input schema with the bound properties
// taken out (the runtime sets them: course_id, idempotency_key), then the
// lowest common denominator every dialect gets, then what d takes.
//
// The lowest common denominator: ["null", X] becomes X, with NoteOptional
// (and a property Core requires but takes as null is no longer required:
// Reverse puts the null back); ["number","string"] becomes a string, keeping
// the pattern, with NoteDecimal; where d refuses format, it goes into the
// description (NoteUUID). References are inlined (a recursive schema is
// refused) and allOf merged, for every dialect.
//
// Then, by dialect:
//   - OpenAI, Anthropic, GeminiJSONSchema: nothing more.
//   - GeminiOpenAPI: nullable: true where Core takes null, no
//     additionalProperties anywhere, only the OpenAPI 3.0 keywords above.
//   - FullCommon, Bedrock: no unions anywhere, no format, only the simple
//     keywords.
//   - OpenAIStrict: every property required, those the model may leave out
//     nullable ([X, "null"]), additionalProperties false on every object, no
//     allOf, not, if, then or else, oneOf as anyOf.
//   - Kimi: the full common transform and the strict one, less every bound
//     (minLength, maxLength, minItems, maxItems, minimum, maximum).
//
// Where unions are refused, anyOf and oneOf are merged into one schema:
// objects into one object with every branch's properties, requiring only
// what every branch requires; one scalar type into that type; mixed types
// into the one Core is surest to take (a string, when it takes one), the
// others named in the description. A map (additionalProperties with a
// schema) that GeminiOpenAPI or the strict dialects cannot say is left out
// where Core does not require it. The output's object keys are sorted, so
// the same input always gives the same bytes. The root is always an object
// with properties, even none; an adapter whose API refuses an empty
// parameters object leaves it out.
func Sanitise(coreSchema json.RawMessage, d Dialect, bound []string) (json.RawMessage, error) {
	r, err := rulesFor(d)
	if err != nil {
		return nil, err
	}
	v, err := prepare(coreSchema)
	if err != nil {
		return nil, err
	}
	root, err := rootObject(v)
	if err != nil {
		return nil, err
	}
	bind(root, bound)
	t := &transformer{r: r}
	out, err := t.schema(root, true)
	if err != nil {
		return nil, err
	}
	s := out.s
	s["type"] = "object"
	if _, ok := s["properties"]; !ok {
		s["properties"] = map[string]any{}
	}
	delete(s, "nullable")
	return encode(s)
}

// rootObject is the schema of a tool's arguments as one object schema:
// alternatives at the root are merged, since every API wants one object.
func rootObject(v any) (map[string]any, error) {
	switch v {
	case true:
		return map[string]any{"type": "object"}, nil
	case false:
		return nil, schemaError("the tool takes no arguments at all (false)")
	}
	m, ok := objectOf(v)
	if !ok {
		return nil, schemaError("the input schema is not a schema object")
	}
	for _, k := range []string{"anyOf", "oneOf"} {
		alts, has := m[k].([]any)
		if !has {
			continue
		}
		delete(m, k)
		var rest []any
		for _, a := range alts {
			if !isNullSchema(a) {
				rest = append(rest, a)
			}
		}
		merged, _, err := mergeAlternatives(rest)
		if err != nil {
			return nil, err
		}
		if merged == nil {
			return nil, schemaError("the input schema takes nothing")
		}
		v, err := merge(m, merged)
		if err != nil {
			return nil, err
		}
		if m, ok = objectOf(v); !ok {
			return nil, schemaError("the input schema takes nothing")
		}
	}
	if ts, has := types(m); has && !slices.Contains(ts, "object") {
		return nil, schemaError("the input schema is not an object (type %v)", ts)
	}
	m["type"] = "object"
	return m, nil
}

// bind takes the bound properties out of the root: the runtime sets them.
func bind(root map[string]any, bound []string) {
	if props, ok := objectOf(root["properties"]); ok {
		for _, b := range bound {
			delete(props, b)
		}
	}
	if req, ok := root["required"]; ok {
		root["required"] = anyList(slices.DeleteFunc(stringList(req), func(s string) bool {
			return slices.Contains(bound, s)
		}))
	}
}

type transformer struct {
	r rules
}

// result is one schema transformed.
type result struct {
	// s is nil for the false schema: nothing may be given there.
	s map[string]any
	// nullable: Core's schema took null here. The caller, which knows
	// whether this is a property and whether Core requires it, says so.
	nullable bool
	// unexpressible: an object this dialect cannot say (a map, or an object
	// open to any keys, under the strict dialects or OpenAPI). Left out
	// where Core does not require it.
	unexpressible bool
}

func (t *transformer) schema(v any, root bool) (result, error) {
	switch v {
	case false:
		return result{}, nil
	case nil, true:
		v = map[string]any{}
	}
	in, ok := objectOf(v)
	if !ok {
		return result{}, schemaError("a schema is neither an object nor a boolean")
	}
	m := deepCopy(in).(map[string]any)
	s := make(map[string]any)
	var res result
	var notes []string

	// Alternatives: a null branch is nullability, one branch is more of
	// the same schema, and several are kept or merged as the dialect says.
	var kept []any
	keptAs := "anyOf"
	for _, k := range []string{"anyOf", "oneOf"} {
		alts, has := m[k].([]any)
		if !has {
			continue
		}
		delete(m, k)
		var rest []any
		for _, a := range alts {
			if isNullSchema(a) {
				res.nullable = true
				continue
			}
			rest = append(rest, a)
		}
		switch {
		case len(rest) == 0:
		case len(rest) == 1:
			merged, err := merge(m, rest[0])
			if err != nil {
				return result{}, err
			}
			if m, ok = objectOf(merged); !ok {
				return result{}, nil
			}
		case t.r.unions && kept == nil:
			kept = rest
			if k == "oneOf" && t.r.oneOf {
				keptAs = "oneOf"
			}
		default:
			alt, note, err := mergeAlternatives(rest)
			if err != nil {
				return result{}, err
			}
			if alt == nil {
				return result{}, nil
			}
			merged, err := merge(m, alt)
			if err != nil {
				return result{}, err
			}
			if m, ok = objectOf(merged); !ok {
				return result{}, nil
			}
			notes = append(notes, note)
		}
	}

	// Type, and the lowest common denominator.
	ts, _ := types(m)
	if slices.Contains(ts, "null") {
		res.nullable = true
		ts = slices.DeleteFunc(ts, func(s string) bool { return s == "null" })
	}
	switch {
	case len(ts) == 2 && slices.Contains(ts, "number") && slices.Contains(ts, "string"):
		ts = []string{"string"}
		notes = append(notes, NoteDecimal)
	case len(ts) > 1 && !t.r.unions:
		chosen, others := collapse(ts)
		ts = []string{chosen}
		notes = append(notes, alsoAccepts(others))
	}
	if enum, has := m["enum"].([]any); has && slices.Contains(enum, nil) {
		res.nullable = true
		if rest := slices.DeleteFunc(slices.Clone(enum), func(e any) bool { return e == nil }); len(rest) > 0 {
			m["enum"] = rest
		} else {
			delete(m, "enum")
		}
	}
	if len(ts) == 0 && kept == nil && t.r.typed {
		inferred, untyped := inferType(m)
		ts = []string{inferred}
		if untyped {
			notes = append(notes, noteAny)
		}
	}
	if len(ts) == 1 {
		pruneForType(m, ts[0])
	}
	setTypes(s, ts)

	// Format, enum, const.
	if f, has := m["format"].(string); has {
		if t.r.format {
			s["format"] = f
		} else {
			notes = append([]string{formatNote(f)}, notes...)
		}
	}
	if enum, has := m["enum"]; has {
		s["enum"] = enum
	}
	if c, has := m["const"]; has {
		if t.keep("const") {
			s["const"] = c
		} else if _, hasEnum := s["enum"]; !hasEnum {
			s["enum"] = []any{c}
		}
	}
	isString := len(ts) == 1 && ts[0] == "string"
	if enum, has := s["enum"].([]any); has && t.r.openAPI && !isString {
		// OpenAPI's enum is for strings; other values are said in words.
		delete(s, "enum")
		notes = append(notes, oneOfValues(enum))
	}

	// What it holds.
	kind := ""
	if len(ts) == 1 {
		kind = ts[0]
	}
	if kind == "object" || kind == "" && hasAny(m, "properties", "additionalProperties", "required") {
		unexpressible, err := t.object(m, s, root)
		if err != nil {
			return result{}, err
		}
		res.unexpressible = unexpressible
		if unexpressible && !root {
			notes = append(notes, noteEmptyMap)
		}
	}
	if kind == "array" || kind == "" && hasAny(m, "items") {
		if err := t.items(m, s); err != nil {
			return result{}, err
		}
	}
	if kept != nil {
		branches := make([]any, 0, len(kept))
		for _, b := range kept {
			br, err := t.schema(b, false)
			if err != nil {
				return result{}, err
			}
			if br.s == nil {
				continue
			}
			if br.nullable {
				res.nullable = true
			}
			branches = append(branches, br.s)
		}
		if len(branches) == 0 {
			return result{}, nil
		}
		s[keptAs] = branches
	}

	// Everything else, as the dialect takes it.
	for _, k := range sortedKeys(m) {
		if _, done := s[k]; done || handled[k] {
			continue
		}
		s[k] = m[k]
	}
	desc, _ := m["description"].(string)
	for _, n := range notes {
		desc = addNote(desc, n)
	}
	if desc != "" {
		s["description"] = desc
	}
	for k := range s {
		if !t.keep(k) {
			delete(s, k)
		}
	}
	res.s = s
	return res, nil
}

// handled are the keywords schema writes itself.
var handled = keywordSet("type", "anyOf", "oneOf", "format", "enum", "const", "description",
	"properties", "required", "additionalProperties", "items", "allOf")

func (t *transformer) keep(k string) bool {
	if strings.HasPrefix(k, "$") || k == "definitions" {
		return false
	}
	if t.r.keywords != nil {
		return t.r.keywords[k]
	}
	return k != "allOf"
}

// object writes an object's properties, required and additionalProperties
// into s. It reports whether the object is one the dialect cannot say.
func (t *transformer) object(m, s map[string]any, root bool) (unexpressible bool, err error) {
	props, _ := objectOf(m["properties"])
	required := stringList(m["required"])
	outProps := make(map[string]any, len(props))
	optional := make(map[string]bool, len(props))
	for _, name := range sortedKeys(props) {
		pr, err := t.schema(props[name], false)
		if err != nil {
			return false, fmt.Errorf("%s: %w", name, err)
		}
		if pr.s == nil {
			continue // false: nothing may be given
		}
		modelOptional := !slices.Contains(required, name) || pr.nullable
		if pr.unexpressible && modelOptional {
			continue
		}
		ps := pr.s
		if pr.nullable {
			ps["description"] = addNote(str(ps["description"]), NoteOptional)
			if t.r.openAPI {
				ps["nullable"] = true
			}
		}
		if t.r.strict && modelOptional {
			if !pr.nullable {
				ps["description"] = addNote(str(ps["description"]), NoteOptional)
			}
			makeNullable(ps)
		}
		optional[name] = modelOptional
		outProps[name] = ps
	}
	if _, has := m["properties"]; has || t.r.strict || root {
		s["properties"] = outProps
	}
	if t.r.strict {
		s["required"] = anyList(sortedKeys(outProps))
	} else {
		var req []string
		for _, name := range required {
			// A name required but never declared is kept where the dialect
			// takes plain JSON Schema; the picky ones refuse it.
			_, declared := outProps[name]
			undeclared := !declared && props[name] == nil && t.r.keywords == nil
			if declared && !optional[name] || undeclared {
				req = append(req, name)
			}
		}
		if len(req) > 0 {
			s["required"] = anyList(req)
		}
	}

	ap, hasAP := m["additionalProperties"]
	apSchema, isMap := objectOf(ap)
	open := !hasAP || ap == true || isMap
	switch {
	case t.r.strict:
		s["additionalProperties"] = false
		return open && len(outProps) == 0, nil
	case t.r.openAPI:
		// Gemini refuses an object with no properties; a map is one.
		return open && len(outProps) == 0, nil
	case isMap && t.r.maps:
		vr, err := t.schema(apSchema, false)
		if err != nil {
			return false, err
		}
		if vr.s == nil {
			s["additionalProperties"] = false
			return false, nil
		}
		if vr.nullable {
			t.nullableElsewhere(vr.s)
		}
		s["additionalProperties"] = vr.s
	case isMap:
		return len(outProps) == 0, nil
	case hasAP:
		s["additionalProperties"] = ap
	}
	return false, nil
}

func (t *transformer) items(m, s map[string]any) error {
	it, has := m["items"]
	if tuple, isTuple := it.([]any); isTuple {
		// A tuple (draft 7's items list) is given as one schema its items
		// all fit, which is as much as most APIs take.
		merged, _, err := mergeAlternatives(tuple)
		if err != nil {
			return err
		}
		it = false
		if merged != nil {
			it = merged
		}
	}
	if !has {
		if !t.r.typed {
			return nil
		}
		it = map[string]any{}
	}
	ir, err := t.schema(it, false)
	if err != nil {
		return fmt.Errorf("items: %w", err)
	}
	if ir.s == nil {
		ir.s = map[string]any{"type": "string"}
		s["maxItems"] = 0
	}
	if ir.nullable {
		t.nullableElsewhere(ir.s)
	}
	s["items"] = ir.s
	return nil
}

// nullableElsewhere marks a schema that takes null and is not a property
// (an array's items, a map's values), which no caller can leave out.
func (t *transformer) nullableElsewhere(s map[string]any) {
	switch {
	case t.r.strict:
		makeNullable(s)
	case t.r.openAPI:
		s["nullable"] = true
	default:
		s["description"] = addNote(str(s["description"]), noteOrNull)
	}
}

// makeNullable adds null to what s takes, the strict dialects' way.
func makeNullable(s map[string]any) {
	if ts, ok := types(s); ok && !slices.Contains(ts, "null") {
		setTypes(s, append(ts, "null"))
	} else if alts, ok := s["anyOf"].([]any); ok {
		s["anyOf"] = append(alts, map[string]any{"type": "null"})
	}
	if enum, ok := s["enum"].([]any); ok && !slices.Contains(enum, nil) {
		s["enum"] = append(enum, nil)
	}
}

// mergeAlternatives is one schema for anyOf/oneOf branches, where a dialect
// takes no unions: objects become one object with every branch's
// properties, requiring what every branch requires; arrays one array of
// items that are any of theirs; one scalar type that type; mixed types the
// one collapse picks, the others named in the note. A branch that takes
// anything makes the whole take anything. The arguments are still checked
// against Core's own schema, so a merge that is looser than Core only lets
// the model make a mistake it is then told about. It returns nil when every
// branch is false: nothing is valid.
func mergeAlternatives(branches []any) (map[string]any, string, error) {
	var maps []map[string]any
	sawFalse := false
	for _, b := range branches {
		switch b {
		case false:
			sawFalse = true
			continue
		case true, nil:
			return map[string]any{}, "", nil
		}
		m, ok := objectOf(b)
		if !ok {
			return nil, "", schemaError("an alternative is not a schema")
		}
		maps = append(maps, m)
	}
	switch {
	case len(maps) == 0 && sawFalse:
		return nil, "", nil
	case len(maps) == 0:
		return map[string]any{}, "", nil
	case len(maps) == 1:
		return deepCopy(maps[0]).(map[string]any), "", nil
	}
	var kinds []string
	byKind := map[string][]map[string]any{}
	for _, m := range maps {
		ks := branchKinds(m)
		if len(ks) == 0 {
			return map[string]any{}, "", nil
		}
		for _, k := range ks {
			if !slices.Contains(kinds, k) {
				kinds = append(kinds, k)
			}
			byKind[k] = append(byKind[k], m)
		}
	}
	if len(kinds) == 1 {
		note := ""
		if kinds[0] == "object" || kinds[0] == "array" {
			// A scalar's forms merge into one; an object's or an array's
			// are several shapes, and the model is told so.
			note = noteForms
		}
		return mergeSameKind(kinds[0], maps), note, nil
	}
	chosen, others := collapse(kinds)
	picked := byKind[chosen]
	if chosen == "number" {
		picked = append(picked, byKind["integer"]...)
	}
	merged, _, err := mergeAlternatives(anySchemas(picked))
	if err != nil {
		return nil, "", err
	}
	if len(kinds) > 0 {
		merged["type"] = chosen
	}
	return merged, alsoAccepts(others), nil
}

// mergeSameKind merges branches that all take one type.
func mergeSameKind(kind string, maps []map[string]any) map[string]any {
	out := map[string]any{"type": kind}
	switch kind {
	case "object":
		props := map[string][]any{}
		var required []string
		closed := true
		for i, m := range maps {
			bp, _ := objectOf(m["properties"])
			for name, ps := range bp {
				props[name] = append(props[name], ps)
			}
			r := stringList(m["required"])
			if i == 0 {
				required = r
			} else {
				required = slices.DeleteFunc(required, func(s string) bool { return !slices.Contains(r, s) })
			}
			if m["additionalProperties"] != false {
				closed = false
			}
		}
		outProps := make(map[string]any, len(props))
		for name, list := range props {
			outProps[name] = oneSchema(list)
		}
		out["properties"] = outProps
		if len(required) > 0 {
			out["required"] = anyList(required)
		}
		if closed {
			out["additionalProperties"] = false
		}
	case "array":
		var items []any
		for _, m := range maps {
			if it, has := m["items"]; has {
				items = append(items, it)
			} else {
				items = append(items, true)
			}
		}
		out["items"] = oneSchema(items)
	default:
		// Keywords every branch says alike are kept; enums are united.
		first := maps[0]
		for _, k := range sortedKeys(first) {
			if k == "type" || k == "enum" || k == "description" {
				continue
			}
			if !slices.ContainsFunc(maps[1:], func(m map[string]any) bool { return !sameJSON(m[k], first[k]) }) {
				out[k] = deepCopy(first[k])
			}
		}
		var enum []any
		for _, m := range maps {
			e, has := m["enum"].([]any)
			if !has {
				enum = nil
				break
			}
			for _, v := range e {
				if !slices.ContainsFunc(enum, func(w any) bool { return sameJSON(v, w) }) {
					enum = append(enum, v)
				}
			}
		}
		if enum != nil {
			out["enum"] = enum
		}
	}
	return out
}

// oneSchema is the one schema of a list, or their anyOf when they differ;
// the transform that reads it merges that again where it must.
func oneSchema(list []any) any {
	if !slices.ContainsFunc(list[1:], func(s any) bool { return !sameJSON(s, list[0]) }) {
		return deepCopy(list[0])
	}
	return map[string]any{"anyOf": deepCopy(list)}
}

func anySchemas(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

// branchKinds is the types an alternative takes, without null, as far as
// its keywords say; none means it takes anything.
func branchKinds(m map[string]any) []string {
	if ts, ok := types(m); ok {
		return slices.DeleteFunc(slices.Clone(ts), func(s string) bool { return s == "null" })
	}
	if k, untyped := inferType(m); !untyped {
		return []string{k}
	}
	return nil
}

// collapse picks one type of several, for a dialect that takes one: the
// one a model can always write and Core then takes, a string before a
// number before an integer (a number covers it) before a boolean, an
// object or an array.
func collapse(ts []string) (chosen string, others []string) {
	if slices.Contains(ts, "number") {
		ts = slices.DeleteFunc(slices.Clone(ts), func(s string) bool { return s == "integer" })
	}
	order := []string{"string", "number", "integer", "boolean", "object", "array"}
	best := len(order)
	for _, t := range ts {
		if i := slices.Index(order, t); i >= 0 && i < best {
			best = i
		}
	}
	if best == len(order) {
		return ts[0], ts[1:]
	}
	chosen = order[best]
	for _, t := range ts {
		if t != chosen {
			others = append(others, t)
		}
	}
	return chosen, others
}

// inferType is the type an untyped schema's keywords imply; untyped is true
// when they imply none (it takes anything), and the type is then a string.
func inferType(m map[string]any) (kind string, untyped bool) {
	switch {
	case hasAny(m, "properties", "additionalProperties", "required", "patternProperties"):
		return "object", false
	case hasAny(m, "items", "prefixItems", "minItems", "maxItems"):
		return "array", false
	case hasAny(m, "pattern", "minLength", "maxLength", "format"):
		return "string", false
	case hasAny(m, "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"):
		return "number", false
	}
	var values []any
	if e, ok := m["enum"].([]any); ok {
		values = e
	} else if c, ok := m["const"]; ok {
		values = []any{c}
	}
	kind = ""
	for _, v := range values {
		k := valueType(v)
		if k == "null" {
			continue
		}
		if kind != "" && kind != k {
			return "string", true
		}
		kind = k
	}
	if kind == "" {
		return "string", true
	}
	return kind, false
}

func valueType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case []any:
		return "array"
	}
	return "object"
}

// keywordType is the type each type-specific keyword applies to.
var keywordType = map[string]string{
	"minimum": "number", "maximum": "number", "exclusiveMinimum": "number", "exclusiveMaximum": "number",
	"multipleOf": "number",
	"minLength":  "string", "maxLength": "string", "pattern": "string",
	"items": "array", "prefixItems": "array", "minItems": "array", "maxItems": "array",
	"uniqueItems": "array", "contains": "array", "minContains": "array", "maxContains": "array",
	"properties": "object", "required": "object", "additionalProperties": "object",
	"patternProperties": "object", "minProperties": "object", "maxProperties": "object",
	"propertyNames": "object", "dependentRequired": "object", "dependentSchemas": "object",
}

// pruneForType drops keywords that apply only to other types than kind:
// they validate nothing there, and picky APIs refuse them.
func pruneForType(m map[string]any, kind string) {
	if kind == "integer" {
		kind = "number"
	}
	for k := range m {
		if t, ok := keywordType[k]; ok && t != kind {
			delete(m, k)
		}
	}
}

// isNullSchema reports whether a schema takes null and nothing else.
func isNullSchema(v any) bool {
	m, ok := objectOf(v)
	if !ok {
		return false
	}
	ts, ok := types(m)
	return ok && len(ts) == 1 && ts[0] == "null"
}

func hasAny(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

func formatNote(f string) string {
	switch f {
	case "uuid":
		return NoteUUID
	case "date-time":
		return " (a date and time, RFC 3339)"
	case "date":
		return " (a date, YYYY-MM-DD)"
	case "email":
		return " (an email address)"
	case "uri":
		return " (a URI)"
	}
	return " (format: " + f + ")"
}

func alsoAccepts(others []string) string {
	if len(others) == 0 {
		return ""
	}
	return " (also accepts: " + strings.Join(others, ", ") + ")"
}

func oneOfValues(enum []any) string {
	vals := make([]string, 0, len(enum))
	for _, e := range enum {
		b, err := encode(e)
		if err == nil {
			vals = append(vals, string(b))
		}
	}
	return " (one of: " + strings.Join(vals, ", ") + ")"
}

// addNote appends a note to a description; with no description, the note
// is the description.
func addNote(desc, note string) string {
	if note == "" {
		return desc
	}
	if desc == "" {
		return strings.TrimPrefix(note, " ")
	}
	return desc + note
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
