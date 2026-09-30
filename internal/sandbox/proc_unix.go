//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the program in a process group of its own, which
// its context's end kills whole.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// The group's id is the program's own: Setpgid made it the leader.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// lowerPriority gives the program a niceness of 10, so that the runtime's
// answers come first on a busy CPU. Failing to is not an error: the
// program still runs within its limits.
func lowerPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 10)
}
