package toolschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
)

// decode parses exactly one JSON value, keeping numbers as json.Number so
// that they come out again exactly as they went in.
func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

// encode writes v as compact JSON. Maps come out with their keys sorted, so
// the same schema always makes the same bytes; <, > and & are left as they
// are, because a model reads them and < costs tokens and clarity.
func encode(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// deepCopy copies a decoded JSON value, so that a transform never changes
// the schema it was given (the same schema is transformed once per dialect).
func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = deepCopy(e)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, e := range x {
			s[i] = deepCopy(e)
		}
		return s
	}
	return v
}

// sameJSON reports whether two decoded values are the same JSON.
func sameJSON(a, b any) bool {
	ea, errA := encode(a)
	eb, errB := encode(b)
	return errA == nil && errB == nil && bytes.Equal(ea, eb)
}

// types reads a schema's "type": one name or a list of them. ok is false
// when there is no "type" or it is not of that shape.
func types(s map[string]any) (ts []string, ok bool) {
	switch t := s["type"].(type) {
	case string:
		return []string{t}, true
	case []any:
		for _, e := range t {
			name, isString := e.(string)
			if !isString {
				return nil, false
			}
			if !slices.Contains(ts, name) {
				ts = append(ts, name)
			}
		}
		return ts, true
	}
	return nil, false
}

// setTypes writes "type" as one name when there is one, else as a list.
func setTypes(s map[string]any, ts []string) {
	switch len(ts) {
	case 0:
		delete(s, "type")
	case 1:
		s["type"] = ts[0]
	default:
		list := make([]any, len(ts))
		for i, t := range ts {
			list[i] = t
		}
		s["type"] = list
	}
}

// stringList reads a list of strings, such as "required"; anything else in
// it is left out.
func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func anyList(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// number reads a JSON number exactly.
func number(v any) (*big.Rat, bool) {
	var s string
	switch n := v.(type) {
	case json.Number:
		s = n.String()
	case float64:
		return new(big.Rat).SetFloat64(n), true
	default:
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

// sortedKeys is m's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// objectOf reads v as a schema object; a boolean schema is not one.
func objectOf(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func schemaError(format string, args ...any) error {
	return fmt.Errorf("toolschema: "+format, args...)
}
