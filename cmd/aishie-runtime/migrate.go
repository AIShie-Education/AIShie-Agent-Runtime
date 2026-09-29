package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/pgstore"
)

// cmdMigrate is `aishie-runtime migrate up|down --yes|version`: the
// store's schema at DATABASE_URL. run never migrates on its own.
func cmdMigrate(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "migrate takes up, down --yes, or version")
	}
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "take the schema down")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		return usageError(stderr, "migrate %s: unknown arguments", args[0])
	}
	switch args[0] {
	case "up", "version":
		if *yes {
			return usageError(stderr, "migrate %s takes no --yes", args[0])
		}
	case "down":
		if !*yes {
			return usageError(stderr, "migrate down deletes all of the runtime's state, the ledger included: give --yes to mean it")
		}
	default:
		return usageError(stderr, "migrate takes up, down --yes, or version, not %q", args[0])
	}
	dbURL := strings.TrimSpace(getenv("DATABASE_URL"))
	if dbURL == "" {
		return failure(stderr, "DATABASE_URL is not set: there is no schema to migrate in memory")
	}
	switch args[0] {
	case "up":
		if err := pgstore.Migrate(dbURL, pgstore.Up); err != nil {
			return failure(stderr, "migrate up: %v", err)
		}
	case "down":
		if err := pgstore.Migrate(dbURL, pgstore.Down); err != nil {
			return failure(stderr, "migrate down: %v", err)
		}
	}
	cur, latest, dirty, err := pgstore.SchemaVersion(ctx, dbURL)
	if err != nil {
		return failure(stderr, "the schema's version: %v", err)
	}
	state := ""
	switch {
	case dirty:
		state = " (dirty: a migration failed part-way)"
	case cur < latest:
		state = " (older than this binary's: run migrate up)"
	case cur > latest:
		state = " (newer than this binary's, which works with it)"
	}
	_, _ = fmt.Fprintf(stdout, "schema version %d%s; this binary's newest is %d\n", cur, state, latest)
	if dirty {
		return exitFailure
	}
	return exitOK
}
