// Package jsonstrict holds JSON to what Core's canon.Check holds it to
// before anything decodes it (Core's internal/canon): no object names a key
// twice, which a map or a struct would keep the last of without a word, and
// no number is out of canon's bounds. The fake Core refuses what Core
// refuses with it, and the runtime's API reads request bodies through it.
package jsonstrict

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Check refuses what Core's canon.Check refuses before anything parses
// the arguments into a map: an object that names a key twice, which a map
// would keep the last of without a word, and a number out of canon's
// bounds. Malformed JSON is left for the parse that follows to report.
func Check(raw []byte) error {
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
			if _, err := PlainNumber(string(n)); err != nil {
				return err
			}
		}
		if o := top(); o != nil && o.keys != nil {
			o.wantKey = true
		}
	}
}

// Numbers are bounded as Core's canon bounds them: an exponent within 400
// either way, and at most 400 digits as written and as written out. Past
// them a literal is refused before anything expands it (1e2000000000 is
// twelve bytes), so a call that Core refuses is refused here too.
const (
	maxExponent = 400
	maxDigits   = 400
)

// PlainNumber writes a JSON number literal as a plain decimal, working on
// its digits so that nothing is rounded: 1, 1.0, 1e0 and 10e-1 are all "1".
func PlainNumber(lit string) (string, error) {
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
