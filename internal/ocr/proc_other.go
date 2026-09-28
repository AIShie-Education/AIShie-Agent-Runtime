//go:build !unix

package ocr

import "os/exec"

// setProcessGroup does nothing where there are no process groups; OCR is
// off there anyway, having no prlimit.
func setProcessGroup(*exec.Cmd) {}

func lowerPriority(int) {}
