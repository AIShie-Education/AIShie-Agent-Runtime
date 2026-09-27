package fakecore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// checkJSON refuses what Core's canon.Check refuses before anything parses
// the arguments into a map: an object that names a key twice, which a map
// would keep the last of without a word, and a number out of canon's
// bounds. Malformed JSON is left for the parse that follows to report.
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
				return fmt.Errorf("%q is given twice in one object", clipLiteral(k))
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
		if n, ok := tok.(json.Number); ok {
			if _, err := plainNumber(string(n)); err != nil {
				return err
			}
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

// Numbers are bounded as Core's canon bounds them: an exponent within 400
// either way, and at most 400 digits as written and as written out. Past
// them a literal is refused before anything expands it (1e2000000000 is
// twelve bytes), so a call that Core refuses is refused here too.
const (
	maxExponent = 400
	maxDigits   = 400
)

// plainNumber writes a JSON number literal as a plain decimal, working on
// its digits so that nothing is rounded: 1, 1.0, 1e0 and 10e-1 are all "1".
func plainNumber(lit string) (string, error) {
	s := lit
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	exp := 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e > maxExponent || e < -maxExponent {
			return "", fmt.Errorf("number %q is out of range", clipLiteral(lit))
		}
		exp, s = e, s[:i]
	}
	intPart, fracPart, _ := strings.Cut(s, ".")
	digits := intPart + fracPart
	if len(digits) > maxDigits {
		return "", fmt.Errorf("number %q has too many digits", clipLiteral(lit))
	}
	scale := len(fracPart) - exp // value = digits × 10^-scale
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil
	}
	for scale > 0 && strings.HasSuffix(digits, "0") {
		digits = digits[:len(digits)-1]
		scale--
	}
	var out string
	switch {
	case scale <= 0:
		out = digits + strings.Repeat("0", -scale)
	case len(digits) <= scale:
		out = "0." + strings.Repeat("0", scale-len(digits)) + digits
	default:
		out = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	if written := len(out) - strings.Count(out, "."); written > maxDigits {
		return "", fmt.Errorf("number %q has too many digits written out", clipLiteral(lit))
	}
	if neg {
		out = "-" + out
	}
	return out, nil
}

// clipLiteral shortens what an error repeats of the caller's input, as
// Core's canon does.
func clipLiteral(s string) string {
	const keep = 40
	if len(s) <= keep {
		return s
	}
	return fmt.Sprintf("%s… (%d bytes)", s[:keep], len(s))
}

// payloadHash is hex(SHA-256(tool name, "\n", canonical arguments)).
func payloadHash(tool string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(tool))
	h.Write([]byte("\n"))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}
