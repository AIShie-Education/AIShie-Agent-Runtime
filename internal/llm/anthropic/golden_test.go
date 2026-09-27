package anthropic

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden from what the adapter makes now")

// goldenPath is testdata/golden/name.
func goldenPath(name string) string { return filepath.Join("testdata", "golden", name) }

// checkGolden compares got, as canonical JSON, with the golden file name,
// or writes it there under -update. Canonical JSON has its keys sorted and
// numbers as written, so field order and spacing never fail a test.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	have, err := canonical(got)
	if err != nil {
		t.Fatalf("%s: what the adapter made is not JSON: %v\n%s", name, err, got)
	}
	path := goldenPath(name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, have, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run go test -update to write it)", name, err)
	}
	want, err := canonical(raw)
	if err != nil {
		t.Fatalf("%s: the golden file is not JSON: %v", name, err)
	}
	if !bytes.Equal(have, want) {
		t.Errorf("%s differs from the golden file:\n%s", name, lineDiff(string(want), string(have)))
	}
}

// readGolden is a golden input file.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(goldenPath(name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// lineDiff shows the lines of want and have from the first that differs.
func lineDiff(want, have string) string {
	w, h := strings.Split(want, "\n"), strings.Split(have, "\n")
	i := 0
	for i < len(w) && i < len(h) && w[i] == h[i] {
		i++
	}
	from := max(i-3, 0)
	clip := func(lines []string) string {
		return strings.Join(lines[min(from, len(lines)):min(i+8, len(lines))], "\n")
	}
	return "first difference at line " + strconv.Itoa(i+1) + "\n--- want\n" + clip(w) + "\n+++ have\n" + clip(h)
}
