package config

import (
	"fmt"
	"math"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/redact"
)

// walker reads a YAML document against the Go type it configures, strictly:
// every key must be a field, every value of the field's kind. It reports
// each problem with its file, line and path, and returns the document as
// generic YAML (maps, lists and scalars) whose scalars already have their
// fields' types, ready to be merged and decoded.
type walker struct {
	file  string
	agent string
	errs  *[]error
	// budget bounds the values walked, so that aliases nested in aliases
	// cannot make a small file enormous.
	budget int
}

// maxValues bounds the values in one file, aliases expanded.
const maxValues = 100000

// shape is what a mapping may hold beyond its type's fields, and what it
// may not.
type shape struct {
	extra  map[string]reflect.Type
	forbid map[string]string // key → why not
	// special walks a key its own way.
	special map[string]func(n *yaml.Node, path string) any
}

var (
	agentType   = reflect.TypeFor[Agent]()
	runtimeType = reflect.TypeFor[Runtime]()
	boolType    = reflect.TypeFor[bool]()
	stringType  = reflect.TypeFor[string]()
)

// courseShape is what courses.<course_id> may hold: the agent's settings,
// but not those that are the agent's alone, and the course's own.
var courseShape = shape{
	extra: map[string]reflect.Type{"enabled": boolType, "prompt_append_ref": stringType, "prompt_append_text": stringType},
	forbid: map[string]string{
		"id":        "an agent's id is the same in every course",
		"tenant_id": "an agent's tenant is the same in every course",
		"core":      "an agent reaches Core one way, with one token, in every course",
		"paused":    "pausing stops the whole agent; set enabled: false to leave one course",
	},
}

// defaultsShape is what runtime.defaults may hold: any agent setting but
// an id.
var defaultsShape = shape{forbid: map[string]string{"id": "every agent has its own id"}}

func (w *walker) problem(n *yaml.Node, path, format string, args ...any) {
	line := 0
	if n != nil {
		line = n.Line
	}
	*w.errs = append(*w.errs, &Problem{File: w.file, Line: line, Agent: w.agent, Path: path, Msg: fmt.Sprintf(format, args...)})
}

// value walks n as a value of type t.
func (w *walker) value(n *yaml.Node, t reflect.Type, path string) any {
	n = resolve(n)
	if !w.spend(n, path) || isNull(n) {
		return nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		return w.value(n, t.Elem(), path)
	case reflect.Struct:
		return w.mapping(n, t, path, shape{})
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			w.problem(n, path, "must be a mapping")
			return nil
		}
		out := map[string]any{}
		for _, kv := range w.pairs(n, path) {
			out[kv.key] = w.value(kv.value, t.Elem(), join(path, kv.key))
		}
		return out
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			w.problem(n, path, "must be a list")
			return nil
		}
		out := make([]any, 0, len(n.Content))
		for i, c := range n.Content {
			out = append(out, w.value(c, t.Elem(), fmt.Sprintf("%s[%d]", path, i)))
		}
		return out
	case reflect.Interface:
		var v any
		if err := n.Decode(&v); err != nil {
			w.problem(n, path, "cannot be read")
		}
		return v
	}
	if n.Kind != yaml.ScalarNode {
		w.problem(n, path, "must be %s", kindName(t))
		return nil
	}
	v := reflect.New(t)
	if err := n.Decode(v.Interface()); err != nil || !integral(n, t) {
		w.problem(n, path, "must be %s", kindName(t))
		return nil
	}
	return v.Elem().Interface()
}

// spend counts one value against the budget, and reports whether any is
// left.
func (w *walker) spend(n *yaml.Node, path string) bool {
	if w.budget--; w.budget < 0 {
		if w.budget == -1 {
			w.problem(n, path, "the file holds more than %d values, aliases expanded", maxValues)
		}
		return false
	}
	return true
}

// integral reports whether a number for an integer field is whole: the
// YAML decoder would cut 1.5 to 1 without a word.
func integral(n *yaml.Node, t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
	default:
		return true
	}
	if n.ShortTag() != "!!float" {
		return true
	}
	var f float64
	return n.Decode(&f) == nil && f == math.Trunc(f)
}

// mapping walks n as a struct of type t, with sh's additions.
func (w *walker) mapping(n *yaml.Node, t reflect.Type, path string, sh shape) map[string]any {
	n = resolve(n)
	if n.Kind != yaml.MappingNode {
		w.problem(n, path, "must be a mapping")
		return nil
	}
	fields := fieldsOf(t)
	out := map[string]any{}
	for _, kv := range w.pairs(n, path) {
		p := join(path, kv.key)
		if why, ok := sh.forbid[kv.key]; ok {
			w.problem(kv.keyNode, p, "not allowed here: %s", why)
			continue
		}
		if f, ok := sh.special[kv.key]; ok {
			out[kv.key] = f(kv.value, p)
			continue
		}
		ft, ok := fields[kv.key]
		if !ok {
			ft, ok = sh.extra[kv.key]
		}
		if !ok {
			w.problem(kv.keyNode, p, "unknown field")
			continue
		}
		out[kv.key] = w.value(kv.value, ft, p)
	}
	return out
}

type pair struct {
	key            string
	keyNode, value *yaml.Node
}

// pairs is a mapping's keys and values, with YAML merge keys (<<: *alias)
// merged in under the keys written, and a key written twice reported.
func (w *walker) pairs(n *yaml.Node, path string) []pair {
	var (
		own    []pair
		merged []pair
		seen   = map[string]bool{}
	)
	for i := 0; i+1 < len(n.Content); i += 2 {
		if !w.spend(n, path) {
			return nil
		}
		k, v := resolve(n.Content[i]), n.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			w.problem(k, path, "a key must be a plain name")
			continue
		}
		if k.ShortTag() == "!!merge" {
			merged = append(merged, w.mergeSources(v, path)...)
			continue
		}
		if seen[k.Value] {
			w.problem(k, join(path, k.Value), "written twice")
			continue
		}
		seen[k.Value] = true
		own = append(own, pair{key: k.Value, keyNode: k, value: v})
	}
	for _, kv := range merged {
		if !seen[kv.key] {
			seen[kv.key] = true
			own = append(own, kv)
		}
	}
	return own
}

// mergeSources is the pairs a merge key brings in: one mapping, or a list
// of them, the first winning.
func (w *walker) mergeSources(v *yaml.Node, path string) []pair {
	v = resolve(v)
	var sources []*yaml.Node
	switch v.Kind {
	case yaml.MappingNode:
		sources = []*yaml.Node{v}
	case yaml.SequenceNode:
		sources = v.Content
	default:
		w.problem(v, path, "a merge (<<) takes a mapping or a list of them")
		return nil
	}
	var out []pair
	for _, s := range sources {
		if s = resolve(s); s.Kind != yaml.MappingNode {
			w.problem(s, path, "a merge (<<) takes a mapping or a list of them")
			continue
		}
		out = append(out, w.pairs(s, path)...)
	}
	return out
}

func resolve(n *yaml.Node) *yaml.Node {
	for i := 0; n != nil && n.Kind == yaml.AliasNode && n.Alias != nil && i < 100; i++ {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n == nil || n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

// join adds key to path. A key is whatever was written, so it is
// redacted: a problem never repeats a secret pasted as a key.
func join(path, key string) string {
	key = redact.String(key)
	if path == "" {
		return key
	}
	return path + "." + key
}

// fieldsOf maps a struct's YAML keys to their types.
func fieldsOf(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if !f.IsExported() || name == "-" || name == "" {
			continue
		}
		out[name] = f.Type
	}
	return out
}

func kindName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int64, reflect.Int32:
		return "a whole number"
	case reflect.Float64, reflect.Float32:
		return "a number"
	case reflect.String:
		return "text"
	case reflect.Slice:
		return "a list"
	}
	return "a mapping"
}
