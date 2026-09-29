package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/worker"
)

// binaryTimeout bounds building the binary, and each of its runs.
const binaryTimeout = 5 * time.Minute

// theBinary is the binary against the same Core: built as make build
// builds it, its catalogue command finds Core's catalogue to be the
// snapshot the runtime was built against, and check --live connects the
// tutor, words its seat as the handout does, and tries its model's key.
func theBinary(t *testing.T, w *world) {
	root := moduleRoot(t)
	bin := buildBinary(t, root)

	snapshot := filepath.Join(root, "internal", "core", "testdata", "catalogue.json")
	out := w.runBinary(t, bin, nil, "catalogue", "--core", w.api.base, "--check", snapshot)
	if !strings.HasPrefix(out, worker.SnapshotCatalogueHash+" ") || !strings.Contains(out, "the same as "+snapshot) {
		t.Errorf("catalogue --check printed:\n%s\nwant the snapshot's hash %s, and that it is the same", redact.String(out), worker.SnapshotCatalogueHash)
	}

	m := newModel(t, fakellm.DefaultResponder)
	cfg := w.writeConfig(t, m, agentConf{id: "tutor", seat: w.tutor})
	out = w.runBinary(t, bin, []string{"CONFIG=" + cfg}, "check", "--live")
	for _, want := range []string{
		`agent tutor: connected as "CS101 Tutor"`,
		"Tutor of CS101 (A): answers every student, reads the material",
		"tools: assignment_get, assignment_list, course_get, document_get, document_list",
		"the key works",
		"every agent connects",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check --live does not say %q; it printed:\n%s", want, redact.String(out))
		}
	}
	if len(m.Requests()) != 1 {
		t.Errorf("check --live called the model %d times; want once, to try its key", len(m.Requests()))
	}
}

// moduleRoot is the repository's root: the tests run in e2e/.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module's root is not %s: %v", root, err)
	}
	return root
}

// buildBinary builds cmd/aishie-runtime as make build does (with the Go
// tool the tests run under, CGO off, paths trimmed, so that the build cache
// make build filled serves it), into a directory of t's own.
func buildBinary(t *testing.T, root string) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go command to build the binary with: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "aishie-runtime")
	ctx, cancel := context.WithTimeout(t.Context(), binaryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goTool, "build", "-trimpath", "-o", bin, "./cmd/aishie-runtime")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/aishie-runtime: %v\n%s", err, out)
	}
	return bin
}

// runBinary runs the binary with args, in an environment of its own: the
// world's agents' tokens and model key, which its configuration refers
// to, and extra. It fails t unless the binary exits 0, and returns what it
// printed, which is kept with the world's logs.
func (w *world) runBinary(t *testing.T, bin string, extra []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), binaryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		w.own.tokenVar + "=" + w.own.token,
		w.tutor.tokenVar + "=" + w.tutor.token,
		w.modelKeyVar + "=" + w.modelKey,
	}, extra...)
	out, err := cmd.CombinedOutput()
	text := string(out)
	w.addLog("the binary's "+args[0], func() string { return text })
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		t.Fatalf("aishie-runtime %s exited %d; it printed:\n%s", args[0], exit.ExitCode(), redact.String(text))
	case err != nil:
		t.Fatalf("aishie-runtime %s: %v", args[0], err)
	}
	return text
}
