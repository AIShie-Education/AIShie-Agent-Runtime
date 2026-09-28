package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/pgstore"
)

// The watcher's tests run on PostgreSQL, at TEST_DATABASE_URL (the local
// server's postgres database by default): a scratch database,
// aishie_registry_t_<random>, migrated up and dropped at the end. Where
// Postgres cannot be reached they are skipped, unless CI is set, where they
// fail.

func adminURL() string {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres:///postgres"
}

// pgDatabase is a fresh, migrated scratch database's URL, dropped when t
// ends.
func pgDatabase(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL())
	if err != nil {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); !ci {
			t.Skipf("skipping a test on PostgreSQL: it cannot be reached: %v", err)
		}
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "aishie_registry_t_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c, err := pgx.Connect(ctx, adminURL()); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
			_ = c.Close(ctx)
		}
	})
	u, err := url.Parse(adminURL())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	if err := pgstore.Migrate(u.String(), pgstore.Up); err != nil {
		t.Fatal(err)
	}
	return u.String()
}

// pgStore opens the database at u, closed when t ends.
func pgStore(t *testing.T, u string) *pgstore.Store {
	t.Helper()
	st, err := pgstore.Open(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// execOn runs sql on the database at u, on a connection of its own.
func execOn(t *testing.T, u, sql string, args ...any) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

var errTimeout = errors.New("timed out")
