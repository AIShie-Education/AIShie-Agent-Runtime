package jsonstrict

import (
	"strings"
	"testing"
)

// Check refuses a key given twice in one object, at any depth, and a
// number out of canon's bounds; it passes the same key in two objects, and
// leaves malformed JSON to the parse that follows.
func TestCheck(t *testing.T) {
	for _, lit := range []string{`{"a":1,"b":{"c":2,"c":3}}`, `{"a":1,"a":1}`, `[{"x":1,"x":2}]`,
		`{"n":1e401}`, `{"n":1e-401}`, `{"n":1` + strings.Repeat("0", 400) + `}`, `{"n":1e400}`} {
		if err := Check([]byte(lit)); err == nil {
			t.Errorf("%.40s passed", lit)
		}
	}
	for _, lit := range []string{`{"a":[{"b":1},{"b":2}],"c":{}}`, `{}`, `{"n":1e2}`, `{"a":`, `not json`, ``} {
		if err := Check([]byte(lit)); err != nil {
			t.Errorf("%s: %v", lit, err)
		}
	}
	if err := Check([]byte(`{"` + strings.Repeat("k", 100) + `":1,"` + strings.Repeat("k", 100) + `":2}`)); err == nil ||
		strings.Contains(err.Error(), strings.Repeat("k", 100)) {
		t.Errorf("a long key is repeated in full: %v", err)
	}
}

// PlainNumber writes a number as a plain decimal, rounding nothing.
func TestPlainNumber(t *testing.T) {
	for lit, want := range map[string]string{"1": "1", "1.0": "1", "1e0": "1", "10e-1": "1", "-0.0": "0", "12.340e-3": "0.01234", "1e2": "100"} {
		if got, err := PlainNumber(lit); err != nil || got != want {
			t.Errorf("%s: %s %v, want %s", lit, got, err, want)
		}
	}
}
