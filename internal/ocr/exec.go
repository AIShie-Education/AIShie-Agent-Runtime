package ocr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// A program OCR runs is held, whatever file it is given:
//
//   - in time by its context, whose end kills its whole process group
//     (SIGKILL), with a CPU-time limit besides;
//   - in memory, CPU time, the size of any file it writes, the files it
//     opens and core dumps by prlimit (util-linux), which sets the limits on
//     itself and then execs the program, so that they hold from the
//     program's first instruction. prlimit is Essential in Debian, and so in
//     the runtime's image; where it is missing, OCR is off (NewEngine),
//     rather than run without a memory limit;
//   - with nothing of the runtime's environment (no credential, no proxy):
//     PATH, and HOME and TMPDIR in its private directory; one thread
//     (OMP_THREAD_LIMIT=1); and a lower priority than the runtime's, so that
//     answering is never starved of CPU;
//   - with its output read to a bound, and its errors only counted, never
//     logged.

// maxProgramOutput bounds what one run of a program writes to its
// standard output that is kept; maxProgramErrors its standard error.
const (
	maxProgramOutput = 1 << 20
	maxProgramErrors = 4 << 10
)

// errProgramTimeout is a program killed at its timeout.
var errProgramTimeout = errors.New("ocr: the program took longer than it may")

// programError is a program that ended badly: its exit status, and the
// start of what it wrote on its standard error, for tests; never logged.
type programError struct {
	name   string
	status int
	stderr string
}

func (e *programError) Error() string {
	return fmt.Sprintf("ocr: %s ended with status %d", e.name, e.status)
}

// limits are what prlimit holds a program to.
type limits struct {
	memory  int64 // bytes of address space
	cpu     time.Duration
	fileMax int64 // bytes of any file it writes
}

// run runs name with args in dir, within timeout and lim, writing its
// standard output to out (at most maxProgramOutput of it).
func (e *Engine) run(ctx context.Context, timeout time.Duration, lim limits, dir string, out io.Writer, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append([]string{
		e.prlimit,
		"--as=" + strconv.FormatInt(lim.memory, 10),
		"--cpu=" + strconv.FormatInt(int64(lim.cpu/time.Second)+1, 10),
		"--fsize=" + strconv.FormatInt(lim.fileMax, 10),
		"--nofile=256",
		"--core=0",
		"--",
		name,
	}, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the programs NewEngine found, with arguments of the engine's own: no file's content is an argument.
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "OMP_THREAD_LIMIT=1", "LC_ALL=C"}
	var stderr bytes.Buffer
	cmd.Stdout = &capped{w: out, left: maxProgramOutput}
	cmd.Stderr = &capped{w: &stderr, left: maxProgramErrors}
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ocr: %s could not start: %w", name, err)
	}
	lowerPriority(cmd.Process.Pid)
	err := cmd.Wait()
	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && err != nil:
		return errProgramTimeout
	case ctx.Err() != nil && err != nil:
		return ctx.Err()
	case err != nil:
		status := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			status = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGXCPU {
				return errProgramTimeout
			}
		}
		return &programError{name: name, status: status, stderr: stderr.String()}
	}
	return nil
}

// capped writes to w until left bytes are written, and drops the rest.
type capped struct {
	w    io.Writer
	left int
}

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	if c.left <= 0 {
		return n, nil
	}
	if len(p) > c.left {
		p = p[:c.left]
	}
	c.left -= len(p)
	if _, err := c.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}
