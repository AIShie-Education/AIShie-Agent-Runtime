package toolschema

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
)

// ArgumentError is arguments that do not fit a tool's schema, said so that
// the model can correct its call. Any other error from Reverse or Validate
// is the schema's fault, not the model's.
type ArgumentError struct {
	Msg string
}

func (e *ArgumentError) Error() string { return e.Msg }

// maxMessage bounds what the model is told of one failure: the library's
// messages quote the value and the schema, which may be long.
const maxMessage = 1000

// Validate checks arguments against Core's own input schema, with the
// library Core validates with (github.com/google/jsonschema-go), read the
// way Core reads them (numbers as float64), so that what passes here passes
// Core's schema check. It also checks format uuid, which the library leaves
// alone, with the parser Core decodes UUIDs with: Core would refuse such a
// value anyway, and the model is better told here, by name, than by a call.
// A failure is an *ArgumentError whose message names where the arguments
// went wrong.
func Validate(coreSchema, args json.RawMessage) (err error) {
	// The library asserts its own invariants with panics; one must not take
	// a worker, and every agent in it, down with it.
	defer func() {
		if r := recover(); r != nil {
			err = schemaError("validating the arguments: %v", r)
		}
	}()
	c, err := compile(coreSchema)
	if err != nil {
		return err
	}
	var instance any
	if err := json.Unmarshal(args, &instance); err != nil {
		return &ArgumentError{Msg: "the arguments are not valid JSON"}
	}
	if _, ok := instance.(map[string]any); !ok {
		return &ArgumentError{Msg: "the arguments are not a JSON object"}
	}
	if err := c.resolved.Validate(instance); err != nil {
		return &ArgumentError{Msg: clip("the arguments do not match the tool's schema: " + readable(err))}
	}
	if msg := checkFormats(instance, c.prepared, ""); msg != "" {
		return &ArgumentError{Msg: clip("the arguments do not match the tool's schema: " + msg)}
	}
	return nil
}

// compiled is a schema ready to validate against.
type compiled struct {
	resolved *jsonschema.Resolved
	// prepared is the schema inlined, for the format check; nil when it
	// cannot be (a recursive schema), and the check is then skipped.
	prepared any
}

// maxCompiled bounds the compiled schemas kept. Core's catalogue has about a
// hundred; past the bound (several catalogues over a long run) all are
// dropped and compiled again as they are used.
const maxCompiled = 1024

var compiledCache = struct {
	sync.Mutex
	m map[[32]byte]*compiled
}{m: map[[32]byte]*compiled{}}

func compile(schema json.RawMessage) (*compiled, error) {
	key := sha256.Sum256(schema)
	compiledCache.Lock()
	c, ok := compiledCache.m[key]
	compiledCache.Unlock()
	if ok {
		return c, nil
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil, schemaError("the schema does not decode: %v", err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		return nil, schemaError("the schema does not resolve: %v", err)
	}
	c = &compiled{resolved: rs}
	if p, err := prepare(schema); err == nil {
		c.prepared = p
	}
	compiledCache.Lock()
	if len(compiledCache.m) >= maxCompiled {
		clear(compiledCache.m)
	}
	compiledCache.m[key] = c
	compiledCache.Unlock()
	return c, nil
}

// readable turns the library's error, "validating root: validating
// /properties/limit: type: …", into "limit: type: …": where in the
// arguments, then what is wrong there.
func readable(err error) string {
	path := ""
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			break
		}
		prefix, cut := strings.CutSuffix(err.Error(), ": "+inner.Error())
		if p, ok := strings.CutPrefix(prefix, "validating "); cut && ok {
			path = p
		}
		err = inner
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if where := instancePath(path); where != "" {
		return where + ": " + msg
	}
	return msg
}

// instancePath turns a schema's JSON pointer (/properties/a/items/properties/b)
// into where that is in the arguments (a[].b).
func instancePath(schemaPath string) string {
	if schemaPath == "" || schemaPath == "root" {
		return ""
	}
	segs := strings.Split(strings.TrimPrefix(schemaPath, "/"), "/")
	var b strings.Builder
	for i := 0; i < len(segs); i++ {
		switch segs[i] {
		case "properties", "patternProperties":
			if i+1 < len(segs) {
				if b.Len() > 0 {
					b.WriteByte('.')
				}
				b.WriteString(unescapePointer(segs[i+1]))
				i++
			}
		case "items", "additionalItems":
			b.WriteString("[]")
		case "prefixItems":
			if i+1 < len(segs) {
				b.WriteString("[" + segs[i+1] + "]")
				i++
			}
		case "additionalProperties":
			b.WriteString(".*")
		case "anyOf", "oneOf", "allOf":
			i++ // and its index
		}
	}
	return b.String()
}

func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

// checkFormats finds a value under format uuid that Core could not decode
// as a UUID. Alternatives are not looked into: which applies is not known.
func checkFormats(v, s any, path string) string {
	m, ok := objectOf(s)
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case string:
		if m["format"] == "uuid" {
			if _, err := uuid.Parse(x); err != nil {
				where := path
				if where == "" {
					where = "the arguments"
				}
				return fmt.Sprintf("%s: %s is not a UUID", where, quote(x))
			}
		}
	case map[string]any:
		props, _ := objectOf(m["properties"])
		for _, k := range sortedKeys(x) {
			sub, declared := props[k]
			if !declared {
				sub = m["additionalProperties"]
			}
			if msg := checkFormats(x[k], sub, join(path, k)); msg != "" {
				return msg
			}
		}
	case []any:
		for i, e := range x {
			if msg := checkFormats(e, m["items"], path+"["+strconv.Itoa(i)+"]"); msg != "" {
				return msg
			}
		}
	}
	return ""
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

// quote quotes a value the model wrote, cut short.
func quote(s string) string {
	const max = 80
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return strconv.Quote(s)
}

// clip cuts a message to maxMessage bytes, on a rune boundary.
func clip(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	cut := maxMessage - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
