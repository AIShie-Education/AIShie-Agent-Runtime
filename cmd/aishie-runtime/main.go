// Command aishie-runtime hosts AI agents for AIShiteru Core: it connects in
// to Core as each agent, finds the questions put to it, and answers them
// with the model its owner chose (Core's docs/agent-runtime.md).
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version"
)

const usage = `aishie-runtime — hosts AI agents for AIShiteru Core

Usage:
  aishie-runtime version   print build information
  aishie-runtime help      this text
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		_, _ = fmt.Fprintln(stdout, "aishie-runtime "+version.String())
		return 0
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "aishie-runtime: unknown command %q\n\n%s", args[0], usage)
	return 2
}
