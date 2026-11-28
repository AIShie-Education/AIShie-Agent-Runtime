package runner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunnerChildProcess is run as the child of TestManagerRestartsChild.
// It serves health on the requested loopback port and records each start.
func TestRunnerChildProcess(t *testing.T) {
	state := os.Getenv("AISHIE_RUNNER_TEST_STATE")
	if state == "" {
		t.Skip("not the runner child process")
	}
	port := os.Getenv("RUNNER_PORT")
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(2)
	}
	f, err := os.OpenFile(state, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = fmt.Fprintln(f, strconv.Itoa(os.Getpid()))
	_ = f.Close()
	if d, _ := time.ParseDuration(os.Getenv("AISHIE_RUNNER_TEST_EXIT_AFTER")); d > 0 {
		time.AfterFunc(d, func() { os.Exit(0) })
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintln(w, `{"status":"ok"}`)
	})
	if err := http.Serve(ln, mux); err != nil {
		os.Exit(2)
	}
}

func TestManagerRestartsChild(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle test")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	state := t.TempDir() + "/starts"
	t.Setenv("AISHIE_RUNNER_TEST_STATE", state)
	t.Setenv("AISHIE_RUNNER_TEST_EXIT_AFTER", "150ms")

	m, err := Start(context.Background(), ProcessOptions{Bin: os.Args[0], Entry: "-test.run=^TestRunnerChildProcess$",
		Port: mustPort(t, port), StartTimeout: 2 * time.Second, StopTimeout: time.Second,
		RestartMin: 20 * time.Millisecond, RestartMax: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for starts(t, state) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := starts(t, state); n < 2 {
		t.Fatalf("child starts = %d, want a restart", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func mustPort(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func starts(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test-owned state file.
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(b)))
}
