//go:build !unix

package sandbox

import "os/exec"

// setProcessGroup does nothing where there are no process groups; nothing
// runs there anyway, having no prlimit.
func setProcessGroup(*exec.Cmd) {}

func lowerPriority(int) {}
