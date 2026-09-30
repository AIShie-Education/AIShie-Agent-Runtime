package ocr

import (
	"context"
	"io"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/sandbox"
)

// A program OCR runs is held as package sandbox holds it, whatever file it
// is given: its time, memory, CPU, files written and open, and core dumps
// bound; nothing of the runtime's environment but a PATH, and one thread
// (OMP_THREAD_LIMIT=1); a lower priority; its output read to a bound.

// errProgramTimeout is a program killed at its timeout.
var errProgramTimeout = sandbox.ErrTimeout

// programError is a program that ended badly.
type programError = sandbox.ProgramError

// limits are what prlimit holds a program to.
type limits = sandbox.Limits

// run runs name with args in dir, within timeout and lim, writing its
// standard output to out (at most sandbox.MaxOutput of it).
func (e *Engine) run(ctx context.Context, timeout time.Duration, lim limits, dir string, out io.Writer, name string, args ...string) error {
	return sandbox.Run(ctx, sandbox.Cmd{Prlimit: e.prlimit, Path: name, Args: args, Dir: dir, Env: []string{"OMP_THREAD_LIMIT=1"},
		Timeout: timeout, Limits: lim, Stdout: out})
}
