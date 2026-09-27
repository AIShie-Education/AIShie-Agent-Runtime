package toolschema

import (
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// A schema is inlined and merged into one self-contained tree before
// anything reads it. These bound what that may cost: a few references to one
// another can otherwise expand exponentially.
const (
	maxNodes = 20000
	maxDepth = 64
)

// Keywords whose values are schemas, by shape. Everything else is copied as
// it is.
var (
	schemaKeywords = map[string]bool{
		"additionalProperties": true, "not": true, "if": true, "then": true, "else": true,
		"contains": true, "propertyNames": true, "unevaluatedProperties": true,
		"unevaluatedItems": true, "additionalItems": true, "contentSchema": true,
	}
	schemaMapKeywords  = map[string]bool{"properties": true, "patternProperties": true, "dependentSchemas": true}
	schemaListKeywords = map[string]bool{"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true}
)

// prepare decodes a Core input schema and makes it self-contained: every
// $ref replaced by what it points at, and every allOf merged into the schema
// that holds it. Sanitise, Reverse and Validate's format check all read this
// shape. A recursive schema is refused: no model API is given one, and the
// sanitised schema must be a finite tree.
func prepare(raw json.RawMessage) (any, error) {
	v, err := decode(raw)
	if err != nil {
		return nil, schemaError("the schema is not JSON: %v", err)
	}
	in := &inliner{root: v}
	v, err = in.inline(v, 0)
	if err != nil {
		return nil, err
	}
	return mergeAllOf(v)
}

type inliner struct {
	root  any
	stack []string // the references being expanded, to find recursion
	nodes int
}

func (in *inliner) inline(v any, depth int) (any, error) {
	m, ok := objectOf(v)
	if !ok {
		return v, nil
	}
	in.nodes++
	if in.nodes > maxNodes {
		return nil, schemaError("the schema has more than %d parts once its references are inlined", maxNodes)
	}
	if depth > maxDepth {
		return nil, schemaError("the schema nests deeper than %d", maxDepth)
	}
	out := make(map[string]any, len(m))
	for _, k := range sortedKeys(m) {
		e := m[k]
		var err error
		switch {
		case k == "$ref", k == "$defs", k == "definitions":
			// $ref is followed below; definitions are inlined where used.
			continue
		case schemaKeywords[k]:
			out[k], err = in.inline(e, depth+1)
		case schemaMapKeywords[k]:
			out[k], err = in.inlineMap(e, depth)
		case schemaListKeywords[k]:
			out[k], err = in.inlineList(e, depth)
		case k == "items":
			if _, isList := e.([]any); isList {
				out[k], err = in.inlineList(e, depth)
			} else {
				out[k], err = in.inline(e, depth+1)
			}
		default:
			out[k] = deepCopy(e)
		}
		if err != nil {
			return nil, err
		}
	}
	raw, has := m["$ref"]
	if !has {
		return out, nil
	}
	ref, isString := raw.(string)
	if !isString {
		return nil, schemaError("$ref is not a string")
	}
	target, err := in.follow(ref, depth)
	if err != nil {
		return nil, err
	}
	// Keywords beside a $ref apply as well as it (2020-12), as an allOf.
	for _, k := range []string{"$comment", "$id", "$schema", "$anchor"} {
		delete(out, k)
	}
	if len(out) == 0 {
		return target, nil
	}
	return merge(target, out)
}

func (in *inliner) inlineMap(v any, depth int) (any, error) {
	m, ok := objectOf(v)
	if !ok {
		return deepCopy(v), nil
	}
	out := make(map[string]any, len(m))
	for _, k := range sortedKeys(m) {
		s, err := in.inline(m[k], depth+1)
		if err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, nil
}

func (in *inliner) inlineList(v any, depth int) (any, error) {
	list, ok := v.([]any)
	if !ok {
		return deepCopy(v), nil
	}
	out := make([]any, len(list))
	for i, e := range list {
		s, err := in.inline(e, depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

// follow inlines what a local reference points at. Only references into the
// schema itself are followed: Core's schemas are self-contained, and fetching
// anything from elsewhere is not the sanitiser's to do.
func (in *inliner) follow(ref string, depth int) (any, error) {
	fragment, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return nil, schemaError("the reference %q is not within the schema", ref)
	}
	if slices.Contains(in.stack, ref) {
		return nil, schemaError("the schema is recursive through %q, which no model API is given", ref)
	}
	pointer, err := url.PathUnescape(fragment)
	if err != nil {
		return nil, schemaError("the reference %q does not decode: %v", ref, err)
	}
	target, err := lookup(in.root, pointer)
	if err != nil {
		return nil, schemaError("the reference %q: %v", ref, err)
	}
	in.stack = append(in.stack, ref)
	defer func() { in.stack = in.stack[:len(in.stack)-1] }()
	return in.inline(target, depth+1)
}

// lookup follows a JSON pointer (RFC 6901) from root.
func lookup(root any, pointer string) (any, error) {
	if pointer == "" {
		return root, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, schemaError("%q is not a JSON pointer", pointer)
	}
	v := root
	for _, tok := range strings.Split(pointer[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			next, ok := x[tok]
			if !ok {
				return nil, schemaError("nothing at %q", pointer)
			}
			v = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(x) {
				return nil, schemaError("nothing at %q", pointer)
			}
			v = x[i]
		default:
			return nil, schemaError("nothing at %q", pointer)
		}
	}
	return v, nil
}

// mergeAllOf replaces every allOf, from the leaves up, by the merge of its
// branches into the schema that holds it.
func mergeAllOf(v any) (any, error) {
	m, ok := objectOf(v)
	if !ok {
		return v, nil
	}
	for _, k := range sortedKeys(m) {
		e := m[k]
		var err error
		switch {
		case schemaKeywords[k]:
			m[k], err = mergeAllOf(e)
		case schemaMapKeywords[k]:
			if sub, isMap := objectOf(e); isMap {
				for _, name := range sortedKeys(sub) {
					if sub[name], err = mergeAllOf(sub[name]); err != nil {
						break
					}
				}
			}
		case schemaListKeywords[k], k == "items":
			if list, isList := e.([]any); isList {
				for i := range list {
					if list[i], err = mergeAllOf(list[i]); err != nil {
						break
					}
				}
			} else if k == "items" {
				m[k], err = mergeAllOf(e)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	branches, has := m["allOf"].([]any)
	if !has {
		return m, nil
	}
	delete(m, "allOf")
	var out any = m
	for _, b := range branches {
		var err error
		if out, err = merge(out, b); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// merge is one schema that holds what both a and b say, as far as a model
// needs to know it: properties and required united, types intersected,
// bounds the stricter, descriptions joined. Where two keywords cannot be
// said as one (two patterns), a's is kept: the arguments are still checked
// against Core's own schema, so what is lost here is a hint, never a check.
func merge(a, b any) (any, error) {
	switch {
	case a == false || b == false:
		return false, nil
	case a == nil || a == true:
		return deepCopy(b), nil
	case b == nil || b == true:
		return deepCopy(a), nil
	}
	am, aok := objectOf(a)
	bm, bok := objectOf(b)
	if !aok || !bok {
		return nil, schemaError("allOf holds something that is not a schema")
	}
	out := deepCopy(am).(map[string]any)
	for _, k := range sortedKeys(bm) {
		bv := deepCopy(bm[k])
		av, has := out[k]
		if !has {
			out[k] = bv
			continue
		}
		var err error
		switch k {
		case "type":
			err = mergeTypes(out, am, bm)
		case "properties":
			out[k], err = mergeProperties(av, bv)
		case "required":
			out[k] = anyList(unite(stringList(av), stringList(bv)))
		case "description":
			out[k] = joinDescriptions(av, bv)
		case "enum":
			out[k], err = intersectEnums(av, bv)
		case "minimum", "exclusiveMinimum", "minLength", "minItems", "minProperties":
			out[k] = bound(av, bv, 1)
		case "maximum", "exclusiveMaximum", "maxLength", "maxItems", "maxProperties":
			out[k] = bound(av, bv, -1)
		case "additionalProperties", "items", "propertyNames", "unevaluatedProperties":
			out[k], err = merge(av, bv)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func mergeTypes(out, a, b map[string]any) error {
	at, aok := types(a)
	bt, bok := types(b)
	if !aok || !bok {
		return schemaError("a type that is neither a name nor a list of names")
	}
	var both []string
	for _, t := range at {
		switch {
		case slices.Contains(bt, t):
			both = append(both, t)
		case t == "number" && slices.Contains(bt, "integer"), t == "integer" && slices.Contains(bt, "number"):
			both = append(both, "integer")
		}
	}
	both = unite(both, nil)
	if len(both) == 0 {
		return schemaError("allOf with no type in common (%v and %v)", at, bt)
	}
	setTypes(out, both)
	return nil
}

func mergeProperties(a, b any) (any, error) {
	am, aok := objectOf(a)
	bm, bok := objectOf(b)
	if !aok || !bok {
		return nil, schemaError("properties is not an object")
	}
	for _, name := range sortedKeys(bm) {
		if prev, has := am[name]; has {
			s, err := merge(prev, bm[name])
			if err != nil {
				return nil, err
			}
			am[name] = s
		} else {
			am[name] = bm[name]
		}
	}
	return am, nil
}

func intersectEnums(a, b any) (any, error) {
	al, _ := a.([]any)
	bl, _ := b.([]any)
	var out []any
	for _, x := range al {
		if slices.ContainsFunc(bl, func(y any) bool { return sameJSON(x, y) }) {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		return nil, schemaError("allOf with no enum value in common")
	}
	return out, nil
}

// bound picks the stricter of two numeric bounds: the larger when sign is
// 1 (a minimum), the smaller when -1 (a maximum).
func bound(a, b any, sign int) any {
	ar, aok := number(a)
	br, bok := number(b)
	switch {
	case !aok:
		return b
	case !bok:
		return a
	case ar.Cmp(br)*sign >= 0:
		return a
	}
	return b
}

// unite is a then those of b not in a, each once.
func unite(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(slices.Clone(a), b...) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func joinDescriptions(a, b any) any {
	as, _ := a.(string)
	bs, _ := b.(string)
	switch {
	case bs == "" || as == bs:
		return as
	case as == "":
		return bs
	}
	return as + "; " + bs
}
