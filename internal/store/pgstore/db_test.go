package pgstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The tests run against TEST_DATABASE_URL, a maintenance database on a
// server where the role may CREATE DATABASE (the Makefile's default is the
// local server's postgres database, over its socket). Each run makes a
// scratch database, aishie_store_t_<random>, migrated up, for the shared
// suite, and more for the tests of the migrations themselves; all are
// dropped at the end. `make clean-testdb` drops any a killed run left.
//
// Where Postgres cannot be reached the tests are skipped, unless CI is set,
// where they fail: CI must not go green on a suite it never ran.

const defaultTestURL = "postgres:///postgres"

var (
	// adminURL is TEST_DATABASE_URL, or the default.
	adminURL string
	// sharedURL is the run's scratch database, migrated up; empty when it
	// could not be made.
	sharedURL string
	// dbErr says why there is no database.
	dbErr error
)

// errUnreachable is the server not answering at all, which outside CI
// skips the tests rather than failing them.
var errUnreachable = errors.New("PostgreSQL at TEST_DATABASE_URL cannot be reached")

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	adminURL = os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		adminURL = defaultTestURL
	}
	name, u, err := createDatabase()
	if err != nil {
		dbErr = err
		return m.Run()
	}
	defer dropDatabase(name)
	if err := Migrate(u, Up); err != nil {
		dbErr = fmt.Errorf("migrate the scratch database up: %w", err)
		return m.Run()
	}
	sharedURL = u
	return m.Run()
}

// inCI reports whether the tests run in CI, where a database is required.
func inCI() bool {
	ci, err := strconv.ParseBool(os.Getenv("CI"))
	return err == nil && ci
}

// needDB skips t when Postgres cannot be reached, outside CI, and fails it
// when there is no database for any other reason.
func needDB(t *testing.T) {
	t.Helper()
	switch {
	case dbErr == nil:
	case errors.Is(dbErr, errUnreachable) && !inCI():
		t.Skipf("skipping the store's database tests: %v", dbErr)
	default:
		t.Fatalf("no test database: %v", dbErr)
	}
}

// freshDatabase is a new, empty scratch database's URL, dropped when t
// ends.
func freshDatabase(t *testing.T) string {
	t.Helper()
	needDB(t)
	name, u, err := createDatabase()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dropDatabase(name) })
	return u
}

// createDatabase makes an empty scratch database and returns its name and
// URL.
func createDatabase() (name, dbURL string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", errUnreachable, err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	name = "aishie_store_t_" + randomSuffix()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", "", fmt.Errorf("create a scratch database: %w", err)
	}
	dbURL, err = withDatabase(adminURL, name)
	if err != nil {
		dropDatabase(name)
		return "", "", err
	}
	return name, dbURL, nil
}

func dropDatabase(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return
	}
	defer func() { _ = admin.Close(context.Background()) }()
	// WITH (FORCE) closes the connections a failed test left open.
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

// withDatabase is rawURL naming database name instead.
func withDatabase(rawURL, name string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("TEST_DATABASE_URL must be a postgres:// URL")
	}
	u.Path = "/" + name
	return u.String(), nil
}

func randomSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
