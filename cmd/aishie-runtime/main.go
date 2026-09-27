// Command aishie-runtime hosts AI agents for AIShiteru Core: it connects in
// to Core as each agent, finds the questions put to it, and answers them
// with the model its owner chose (Core's docs/agent-runtime.md; this
// repository's docs/design.md §1).
//
//	aishie-runtime run        the worker: pollers and answer loops, and /healthz, /metrics, /status
//	aishie-runtime check      validate the configuration; with --live, connect each agent and show its seats
//	aishie-runtime migrate    the store's schema (PostgreSQL)
//	aishie-runtime catalogue  fetch Core's GET /v1/tools, print its hash, compare it with a snapshot
//	aishie-runtime version
//
// It exits 0 when all went well, 1 on a failure, and 2 when it was used
// wrongly.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/version"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

const usage = `aishie-runtime — hosts AI agents for AIShiteru Core

Usage:
  aishie-runtime run                          run the worker: every agent configured, and /healthz, /metrics, /status
  aishie-runtime check [--live]               load and validate the configuration and show each agent; with --live,
                                              connect each agent to Core, show its seats and tools, and try its model's key
  aishie-runtime migrate up                   bring the store's schema (DATABASE_URL) up to this version
  aishie-runtime migrate down --yes           take it all down: every piece of the runtime's state, the ledger included
  aishie-runtime migrate version              show the schema's version
  aishie-runtime catalogue --core URL [--check FILE] [--write FILE]
                                              print the hash of Core's tool catalogue; with --check, fail if it is not
                                              FILE's (a catalogue, or its hash); with --write, save it to FILE
  aishie-runtime version                      print build information
  aishie-runtime help                         this text

run takes SIGHUP to read its configuration again, and SIGINT or SIGTERM to
stop, giving answers in progress SHUTDOWN_GRACE.

Exit status: 0 ok, 1 failure, 2 usage.

Environment:
`

func main() {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr, sigs))
}

// run is the command args names, with the environment of getenv, writing
// to stdout and stderr, and taking signals from sigs; its exit code.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, sigs <-chan os.Signal) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage+config.EnvHelp())
		return exitUsage
	}
	switch args[0] {
	case "run":
		return cmdRun(ctx, args[1:], getenv, stderr, sigs)
	case "check":
		return cmdCheck(ctx, args[1:], getenv, stdout, stderr)
	case "migrate":
		return cmdMigrate(ctx, args[1:], getenv, stdout, stderr)
	case "catalogue":
		return cmdCatalogue(ctx, args[1:], stdout, stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, "aishie-runtime "+version.String())
		return exitOK
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage+config.EnvHelp())
		return exitOK
	}
	_, _ = fmt.Fprintf(stderr, "aishie-runtime: unknown command %q\n\n%s", args[0], usage+config.EnvHelp())
	return exitUsage
}

// usageError reports a command used wrongly.
func usageError(stderr io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stderr, "aishie-runtime: "+format+"\n\nSee aishie-runtime help.\n", args...)
	return exitUsage
}

// failure reports a failure.
func failure(stderr io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stderr, "aishie-runtime: "+format+"\n", args...)
	return exitFailure
}
