package fakecore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// checkJSON refuses what Core's canon.Check refuses before anything parses
// the arguments into a map: an object that names a key twice, which a map
// would keep the last of without a word. Malformed JSON is left for the
// parse that follows to report.
func checkJSON(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type open struct {
		keys    map[string]bool
		wantKey bool
	}
	var stack []*open
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		top := func() *open {
			if len(stack) == 0 {
				return nil
			}
			return stack[len(stack)-1]
		}
		if o := top(); o != nil && o.keys != nil && o.wantKey {
			if tok == json.Delim('}') {
				stack = stack[:len(stack)-1]
				if p := top(); p != nil && p.keys != nil {
					p.wantKey = true
				}
				continue
			}
			k, _ := tok.(string)
			if o.keys[k] {
				return fmt.Errorf("the key %q is given twice", clip(k))
			}
			o.keys[k] = true
			o.wantKey = false
			continue
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &open{keys: map[string]bool{}, wantKey: true})
			continue
		case json.Delim('['):
			stack = append(stack, &open{})
			continue
		case json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		if o := top(); o != nil && o.keys != nil {
			o.wantKey = true
		}
	}
}

// canonicalize writes arguments as Core's ais-canon-1 does: keys sorted,
// no whitespace, numbers as plain decimals, HTML left unescaped. Two calls
// with the same canonical bytes are the same call.
func canonicalize(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("the arguments are not JSON: %w", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, errors.New("the arguments must be a JSON object")
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		n, err := plainNumber(string(x))
		if err != nil {
			return err
		}
		buf.WriteString(n)
	case string:
		return writeString(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonicalize: unexpected %T", v)
	}
	return nil
}

func writeString(buf *bytes.Buffer, s string) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return nil
}

// maxPlaces bounds how far a decimal is expanded, as Core bounds numbers.
const maxPlaces = 400

// plainNumber writes a JSON number with no exponent and no leading or
// trailing zeros: 1, 1.0, 1e0 and 10e-1 are all "1".
func plainNumber(lit string) (string, error) {
	r, ok := new(big.Rat).SetString(lit)
	if !ok {
		return "", fmt.Errorf("%q is not a number", clip(lit))
	}
	if r.IsInt() {
		return r.Num().String(), nil
	}
	ten := big.NewInt(10)
	scale := big.NewInt(1)
	for places := 1; places <= maxPlaces; places++ {
		scale.Mul(scale, ten)
		if new(big.Int).Mod(scale, r.Denom()).Sign() == 0 {
			s := r.FloatString(places)
			return strings.TrimRight(strings.TrimRight(s, "0"), "."), nil
		}
	}
	return "", fmt.Errorf("%q has too many digits", clip(lit))
}

// payloadHash is hex(SHA-256(tool name, "\n", canonical arguments)).
func payloadHash(tool string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte("\n"))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}
