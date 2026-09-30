// Package sandbox runs the programs the runtime hands a course's files to
// (docs/design.md §4, Files): tesseract and poppler's for OCR, LibreOffice
// and poppler's for the PDFs of Office files and their pages. Every file is
// taken to be hostile, so a program is held, whatever file it is given:
//
//   - in time by its context and its own timeout, whose end kills its whole
//     process group (SIGKILL), with a CPU-time limit besides;
//   - in memory, CPU time, the size of any file it writes, the files it
//     opens and core dumps by prlimit (util-linux), which sets the limits on
//     itself and then execs the program, so that they hold from the
//     program's first instruction. prlimit is Essential in Debian, and so in
//     the runtime's image; where it is missing, what needs it is off, rather
//     than run without a memory limit;
//   - with nothing of the runtime's environment (no credential, no proxy):
//     PATH, LC_ALL=C, HOME and TMPDIR in its private directory, and what the
//     caller adds; and a lower priority than the runtime's, so that answering
//     is never starved of CPU;
//   - with its output read to a bound, and its errors only counted, never
//     logged.
//
// Go cannot set limits between fork and exec for a child alone, and a
// helper binary of the runtime's own would be one more thing in the image;
// prlimit does it. A network namespace would need privileges the container
// does not have, so a program is given only files the runtime wrote, by
// arguments the runtime fixed, never a URL.
package sandbox

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

// Limits are what prlimit holds a program to.
type Limits struct {
	// Memory is its address space, in bytes.
	Memory int64
	// CPU is its CPU time; a second more is given, as prlimit counts whole
	// seconds.
	CPU time.Duration
	// FileSize bounds any file it writes, in bytes.
	FileSize int64
	// Files is how many files it may have open; DefaultFiles when 0.
	Files int
}

// DefaultFiles is the files a program may have open unless Limits says.
const DefaultFiles = 256

// MaxOutput bounds what one run writes to its standard output that is kept;
// maxErrors its standard error.
const (
	MaxOutput = 1 << 20
	maxErrors = 4 << 10
)

// Cmd is one run of a program.
type Cmd struct {
	// Prlimit is prlimit's path.
	Prlimit string
	// Path is the program's, and Args its arguments: the caller's own, never
	// anything a file holds.
	Path string
	Args []string
	// Dir is the program's private directory: its working directory, and
	// its HOME and TMPDIR.
	Dir string
	// Env is added to its environment: PATH, LC_ALL=C, HOME and TMPDIR.
	Env []string
	// Timeout bounds the run, within its context.
	Timeout time.Duration
	Limits  Limits
	// Stdout is given what the program writes to its standard output, at
	// most MaxOutput of it; nil drops it.
	Stdout io.Writer
}

// ErrTimeout is a program killed at its timeout, or past its CPU time.
var ErrTimeout = errors.New("sandbox: the program took longer than it may")

// ProgramError is a program that ended badly: its exit status, and the start
// of what it wrote on its standard error, which callers may match but never
// log.
type ProgramError struct {
	Name   string
	Status int
	Stderr string
}

func (e *ProgramError) Error() string {
	return fmt.Sprintf("sandbox: %s ended with status %d", e.Name, e.Status)
}

// Run runs c under prlimit, within c.Timeout and ctx. Its error is
// ErrTimeout, ctx's own error when ctx ended first, a *ProgramError, or a
// program that could not start.
func Run(ctx context.Context, c Cmd) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	files := c.Limits.Files
	if files <= 0 {
		files = DefaultFiles
	}
	argv := append([]string{
		c.Prlimit,
		"--as=" + strconv.FormatInt(c.Limits.Memory, 10),
		"--cpu=" + strconv.FormatInt(int64(c.Limits.CPU/time.Second)+1, 10),
		"--fsize=" + strconv.FormatInt(c.Limits.FileSize, 10),
		"--nofile=" + strconv.Itoa(files),
		"--core=0",
		"--",
		c.Path,
	}, c.Args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the programs the caller found, with arguments of its own: no file's content is an argument.
	cmd.Dir = c.Dir
	cmd.Env = append([]string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + c.Dir, "TMPDIR=" + c.Dir, "LC_ALL=C"}, c.Env...)
	var stderr bytes.Buffer
	out := c.Stdout
	if out == nil {
		out = io.Discard
	}
	cmd.Stdout = &capped{w: out, left: MaxOutput}
	cmd.Stderr = &capped{w: &stderr, left: maxErrors}
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("sandbox: %s could not start: %w", c.Path, err)
	}
	lowerPriority(cmd.Process.Pid)
	err := cmd.Wait()
	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && err != nil:
		return ErrTimeout
	case ctx.Err() != nil && err != nil:
		return ctx.Err()
	case err != nil:
		status := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			status = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGXCPU {
				return ErrTimeout
			}
		}
		return &ProgramError{Name: c.Path, Status: status, Stderr: stderr.String()}
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

// Find finds each program: at the path given, or by its name on PATH. It
// returns the paths found, and the names of those that are not.
func Find(programs map[string]string) (found map[string]string, missing []string) {
	found = map[string]string{}
	for name, at := range programs {
		if at == "" {
			at = name
		}
		p, err := exec.LookPath(at)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		found[name] = p
	}
	return found, missing
}
