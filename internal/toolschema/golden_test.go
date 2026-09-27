package toolschema

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

// goldenTools stand for the shapes in Core's catalogue: a plain read with a
// nullable UUID (document_get), paging and nullable filters (grade_list),
// decimals and arrays of objects (grade_submit), a perms map and arrays of
// UUIDs (member_add), 32-bit bounds on nullable integers
// (component_update), and no arguments at all (me_get).
var goldenTools = []string{"document_get", "grade_list", "grade_submit", "member_add", "component_update", "me_get"}

// TestGolden holds each dialect's schema for the golden tools to what was
// reviewed, byte for byte: go test ./internal/toolschema -update rewrites
// them, and the diff is then read like code.
func TestGolden(t *testing.T) {
	for _, d := range allDialects {
		for _, name := range goldenTools {
			t.Run(string(d)+"/"+name, func(t *testing.T) {
				got, err := Sanitise(catalogueTool(t, name).InputSchema, d, testBound)
				if err != nil {
					t.Fatal(err)
				}
				var pretty bytes.Buffer
				if err := json.Indent(&pretty, got, "", "  "); err != nil {
					t.Fatal(err)
				}
				pretty.WriteByte('\n')
				path := filepath.Join("testdata", "golden", string(d), name+".json")
				if *update {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("%v (run with -update to write it)", err)
				}
				if !bytes.Equal(pretty.Bytes(), want) {
					t.Errorf("%s differs from what was reviewed:\ngot:\n%s", path, pretty.Bytes())
				}
			})
		}
	}
}
