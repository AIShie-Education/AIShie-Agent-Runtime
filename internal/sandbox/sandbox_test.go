package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sh runs a shell snippet under the real prlimit, or the test is skipped
// where there is none.
func sh(t *testing.T, timeout time.Duration, snippet string, out *bytes.Buffer) error {
	t.Helper()
	prlimit, err := exec.LookPath("prlimit")
	if err != nil {
		t.Skip("prlimit (util-linux) is not installed")
	}
	c := Cmd{Prlimit: prlimit, Path: "/bin/sh", Args: []string{"-c", snippet}, Dir: t.TempDir(), Env: []string{"EXTRA=1"},
		Timeout: timeout, Limits: Limits{Memory: 256 << 20, CPU: timeout, FileSize: 1 << 20}}
	if out != nil {
		c.Stdout = out
	}
	return Run(t.Context(), c)
}

// TestRun: a program runs with the environment it is given and no other,
// under its limits, and its output is kept to MaxOutput; one that fails is
// a ProgramError with its status and the start of its errors; one past its
// time is ErrTimeout, killed with its children.
func TestRun(t *testing.T) {
	t.Setenv("AISHIE_TEST_SECRET", "ais_Secret0123456789")
	var out bytes.Buffer
	// Its niceness is read first: it is the program's from its first
	// instruction, not set once it has started.
	if err := sh(t, 5*time.Second, `echo "nice=$(cut -d' ' -f19 /proc/self/stat)"; env; grep -E '^Max (address space|open files|core file size)' /proc/self/limits`, &out); err != nil {
		t.Fatal(err)
	}
	lot := strings.Join(strings.Fields(out.String()), " ")
	// nice adds to the niceness of the process that runs it.
	own, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.Fields(string(own))[18])
	if want := fmt.Sprintf("nice=%d", min(n+niceness, 19)); !strings.HasPrefix(lot, want+" ") {
		t.Errorf("the program's priority from its first instruction is not %s: %s", want, lot)
	}
	if strings.Contains(lot, "ais_Secret") {
		t.Errorf("the runtime's environment reached the program: %s", lot)
	}
	for _, want := range []string{"EXTRA=1", "LC_ALL=C", "HOME=", "TMPDIR=", "Max address space 268435456", "Max open files 256", "Max core file size 0"} {
		if !strings.Contains(lot, want) {
			t.Errorf("the program's lot lacks %q: %s", want, lot)
		}
	}

	out.Reset()
	if err := sh(t, 5*time.Second, `head -c 3000000 /dev/zero`, &out); err != nil || out.Len() != MaxOutput {
		t.Errorf("an output of 3 MB: %d kept, %v", out.Len(), err)
	}

	err = sh(t, 5*time.Second, `echo 'no such page' >&2; exit 99`, nil)
	var pe *ProgramError
	if !errors.As(err, &pe) || pe.Status != 99 || !strings.Contains(pe.Stderr, "no such page") || strings.Contains(err.Error(), "no such page") {
		t.Errorf("a failure: %v", err)
	}

	start := time.Now()
	if err := sh(t, 200*time.Millisecond, `sleep 30 & wait`, nil); !errors.Is(err, ErrTimeout) {
		t.Errorf("past its time: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %s: its time did not end it", took)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	prlimit, _ := exec.LookPath("prlimit")
	if err := Run(ctx, Cmd{Prlimit: prlimit, Path: "/bin/true", Dir: t.TempDir(), Timeout: time.Second, Limits: Limits{Memory: 64 << 20}}); err == nil {
		t.Error("a cancelled run ran")
	}
}

func TestFind(t *testing.T) {
	found, missing := Find(map[string]string{"sh": "", "nothing-here": "", "true": "/bin/true"})
	if found["sh"] == "" || found["true"] != "/bin/true" || len(missing) != 1 || missing[0] != "nothing-here" {
		t.Errorf("found %v, missing %v", found, missing)
	}
}
