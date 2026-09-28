package worker

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

// Tests that need two workers sharing leases run on PostgreSQL, at
// TEST_DATABASE_URL (the local server's postgres database by default): a
// scratch database, aishie_worker_t_<random>, migrated up and dropped at
// the end. Where Postgres cannot be reached they are skipped, unless CI is
// set, where they fail.

// errUnreachable is the server not answering at all.
var errUnreachable = errors.New("PostgreSQL at TEST_DATABASE_URL cannot be reached")

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
	name, u, err := createDatabase()
	if err != nil {
		ci, _ := strconv.ParseBool(os.Getenv("CI"))
		if errors.Is(err, errUnreachable) && !ci {
			t.Skipf("skipping a test on PostgreSQL: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { dropDatabase(name) })
	if err := pgstore.Migrate(u, pgstore.Up); err != nil {
		t.Fatal(err)
	}
	return u
}

// pgStore is a pgstore on a fresh database, closed when t ends.
func pgStore(t *testing.T, dbURL string) *pgstore.Store {
	t.Helper()
	st, err := pgstore.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func createDatabase() (name, dbURL string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL())
	if err != nil {
		return "", "", errors.Join(errUnreachable, err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	var b [6]byte
	_, _ = rand.Read(b[:])
	name = "aishie_worker_t_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", "", err
	}
	u, err := url.Parse(adminURL())
	if err != nil {
		dropDatabase(name)
		return "", "", err
	}
	u.Path = "/" + name
	return name, u.String(), nil
}

func dropDatabase(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL())
	if err != nil {
		return
	}
	defer func() { _ = admin.Close(context.Background()) }()
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}
