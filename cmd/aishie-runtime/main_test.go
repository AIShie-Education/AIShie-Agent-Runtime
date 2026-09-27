package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var out, errs bytes.Buffer
	if code := run([]string{"version"}, &out, &errs); code != 0 || !strings.HasPrefix(out.String(), "aishie-runtime ") {
		t.Errorf("version: %d %q", code, out.String())
	}
	if code := run(nil, &out, &errs); code != 2 {
		t.Errorf("no command: %d", code)
	}
	if code := run([]string{"nonsense"}, &out, &errs); code != 2 || !strings.Contains(errs.String(), "unknown command") {
		t.Errorf("unknown command: %d %q", code, errs.String())
	}
}
