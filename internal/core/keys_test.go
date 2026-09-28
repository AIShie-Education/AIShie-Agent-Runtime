package core

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestToolKey(t *testing.T) {
	const x, m = "0192f3c1-7d2e-7b4a-9c3d-2e1f0a9b8c7d", "0192f3c1-aaaa-7b4a-9c3d-2e1f0a9b8c7d"
	if got, want := ToolKey(x, m, 2, 3), "tool:"+x+":"+m+":2:3"; got != want {
		t.Errorf("ToolKey = %q, want %q", got, want)
	}
	// The same attempt keys its writes the same; another attempt, or
	// another write, differently.
	if a, b := ToolKey(x, m, 1, 1), ToolKey(strings.Clone(x), m, 1, 1); a != b {
		t.Error("the same write of the same attempt has two keys")
	}
	seen := map[string]bool{}
	for _, k := range []string{ToolKey(x, m, 1, 1), ToolKey(x, m, 2, 1), ToolKey(x, m, 1, 2), ToolKey(x, m, 11, 1), ToolKey(x, m, 1, 11)} {
		if seen[k] {
			t.Errorf("%q is two writes' key", k)
		}
		seen[k] = true
	}
	// Ids too long for Core's 200 characters are hashed, and still apart.
	long := strings.Repeat("é", 120)
	a, b := ToolKey(long, m, 1, 1), ToolKey(long, m, 1, 2)
	for _, k := range []string{a, b} {
		if n := utf8.RuneCountInString(k); n > MaxKeyChars || !strings.HasPrefix(k, "tool:") {
			t.Errorf("%q: %d characters", k, n)
		}
	}
	if a == b || a != ToolKey(long, m, 1, 1) {
		t.Error("hashed keys are not one per write")
	}
}
