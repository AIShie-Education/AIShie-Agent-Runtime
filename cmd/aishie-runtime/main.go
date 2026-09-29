// Command aishie-runtime hosts AI agents for AIshie Core: it connects in
// to Core as each agent, finds the questions put to it, and answers them
// with the model its owner chose (Core's docs/agent-runtime.md; this
// repository's docs/design.md §1).
//
//	aishie-runtime run        the worker: pollers and answer loops, and /healthz, /metrics, /status
//	aishie-runtime check      validate the configuration; with --live, connect each agent and show its seats
//	aishie-runtime migrate    the store's schema (PostgreSQL)
//	aishie-runtime keys       the sealed secrets: check them, or rewrap them under the current key
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

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/version"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

const usage = `aishie-runtime — hosts AI agents for AIshie Core

Usage:
  aishie-runtime run                          run the worker: every agent configured, and /healthz, /metrics, /status
  aishie-runtime check [--live]               load and validate the configuration and show each agent; with --live,
                                              connect each agent to Core, show its seats and tools, and try its model's key
  aishie-runtime migrate up                   bring the store's schema (DATABASE_URL) up to this version
  aishie-runtime migrate down --yes           take it all down: every piece of the runtime's state, the ledger included
  aishie-runtime migrate version              show the schema's version
  aishie-runtime keys check                   open every secret sealed in the store with KMS_KEY_ID's keyring, and say
                                              which key wraps each; nothing of them is printed
  aishie-runtime keys rewrap                  wrap every secret's data key that an older key wraps under KMS_KEY_ID's
  aishie-runtime catalogue --core URL [--check FILE] [--write FILE]
                                              print the hash of Core's tool catalogue; with --check, fail if it is not
                                              FILE's (a catalogue, or its hash); with --write, save it to FILE
  aishie-runtime version                      print build information
  aishie-runtime help                         this text

run takes SIGHUP to read its configuration again, and SIGINT or SIGTERM to
stop, giving answers in progress SHUTDOWN_GRACE; a second SIGINT or SIGTERM
stops it at once.

Exit status: 0 ok, 1 failure, 2 usage.

Environment:
`

func main() {
	ctx, sigs := signals(os.Args[1:])
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, sigs))
}

// signals is how the command args names takes signals. run reads SIGHUP,
// SIGINT and SIGTERM itself, from the channel. Any other command runs in a
// context that ends at the first SIGINT or SIGTERM, so that an operator can
// stop a check or a fetch that hangs; a second one ends the process as it
// would without this.
func signals(args []string) (context.Context, <-chan os.Signal) {
	if len(args) > 0 && args[0] == "run" {
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
		return context.Background(), sigs
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, nil
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
	case "keys":
		return cmdKeys(ctx, args[1:], getenv, stdout, stderr)
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
