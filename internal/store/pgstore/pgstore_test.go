package pgstore

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/storetest"
)

var update = flag.Bool("update", false, "rewrite testdata/schema.golden from the migrated schema")

// tables are every table the migrations make, in the order TRUNCATE takes
// them.
var tables = []string{"lease", "attempt", "cursor", "note", "seat", "llm_call", "answer", "agent_state"}

// openShared opens the run's scratch database, emptied, for one test.
func openShared(t *testing.T) *Store {
	t.Helper()
	needDB(t)
	s, err := Open(t.Context(), sharedURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.pool.Exec(t.Context(), "TRUNCATE "+strings.Join(tables, ", ")+" RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContract(t *testing.T) {
	needDB(t)
	storetest.Run(t, func(t *testing.T) store.Store { return openShared(t) })
}

// Every migration must be reversible, and reversing must leave nothing
// behind that stops it applying again.
func TestMigrateUpDownUp(t *testing.T) {
	u := freshDatabase(t)
	ctx := t.Context()

	version := func(want uint) {
		t.Helper()
		current, latest, dirty, err := SchemaVersion(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if latest != 1 {
			t.Errorf("latest = %d, want 1", latest)
		}
		if current != want || dirty {
			t.Fatalf("version = %d (dirty %v), want %d", current, dirty, want)
		}
	}
	tableCount := func() int {
		t.Helper()
		conn, err := pgx.Connect(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(context.Background()) }()
		var n int
		err = conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	version(0)
	for i, step := range []struct {
		direction string
		version   uint
		tables    int
	}{
		{Up, 1, len(tables)},
		{Up, 1, len(tables)}, // already there
		{Down, 0, 0},
		{Down, 0, 0}, // already there
		{Up, 1, len(tables)},
	} {
		if err := Migrate(u, step.direction); err != nil {
			t.Fatalf("step %d, %s: %v", i+1, step.direction, err)
		}
		version(step.version)
		if n := tableCount(); n != step.tables {
			t.Fatalf("step %d, %s: %d tables, want %d", i+1, step.direction, n, step.tables)
		}
	}

	s, err := Open(ctx, u)
	if err != nil {
		t.Fatalf("Open after up, down, up: %v", err)
	}
	defer func() { _ = s.Close() }()
	if ok, err := s.AcquireLease(ctx, "agent:a1", "w1", time.Minute); err != nil || !ok {
		t.Fatalf("AcquireLease on the migrated schema = %v, %v", ok, err)
	}
}

func TestMigrateRefusesAnUnknownDirection(t *testing.T) {
	// Refused before any connection: the URL is never used.
	for _, d := range []string{"", "UP", "sideways", "down --all"} {
		if err := Migrate("postgres:///nowhere", d); err == nil {
			t.Errorf("Migrate(%q) was taken", d)
		}
	}
}

// Open never migrates, and does not start against a schema older than the
// migrations it carries, nor a dirty one; it says what to do. A schema that
// is ahead is a rolling deploy or a rollback, and is taken.
func TestOpenChecksTheSchema(t *testing.T) {
	ctx := t.Context()

	t.Run("older", func(t *testing.T) {
		u := freshDatabase(t)
		_, err := Open(ctx, u)
		if err == nil || !strings.Contains(err.Error(), "version 0") || !strings.Contains(err.Error(), "needs 1") ||
			!strings.Contains(err.Error(), "aishie-runtime migrate up") {
			t.Fatalf("Open on an empty database: err = %v, want one saying to run migrate up", err)
		}
		// Open looked, and made nothing.
		if current, _, _, err := SchemaVersion(ctx, u); err != nil || current != 0 {
			t.Fatalf("SchemaVersion after the refusal = %d, %v", current, err)
		}
	})

	t.Run("dirty", func(t *testing.T) {
		u := freshDatabase(t)
		if err := Migrate(u, Up); err != nil {
			t.Fatal(err)
		}
		execOn(t, u, `UPDATE schema_migrations SET dirty = true`)
		if _, err := Open(ctx, u); err == nil || !strings.Contains(err.Error(), "dirty at version 1") {
			t.Fatalf("Open on a dirty schema: err = %v, want one saying it is dirty", err)
		}
		if err := Migrate(u, Up); err == nil || !strings.Contains(err.Error(), "dirty at version 1") {
			t.Fatalf("Migrate up on a dirty schema: err = %v, want one saying it is dirty", err)
		}
		if _, _, dirty, err := SchemaVersion(ctx, u); err != nil || !dirty {
			t.Fatalf("SchemaVersion of a dirty schema: dirty = %v, %v", dirty, err)
		}
	})

	t.Run("ahead", func(t *testing.T) {
		u := freshDatabase(t)
		if err := Migrate(u, Up); err != nil {
			t.Fatal(err)
		}
		execOn(t, u, `UPDATE schema_migrations SET version = 2`)
		s, err := Open(ctx, u)
		if err != nil {
			t.Fatalf("Open on a schema ahead: %v", err)
		}
		_ = s.Close()
		// migrate up, as a deploy runs it first, leaves it as it is.
		if err := Migrate(u, Up); err != nil {
			t.Fatalf("Migrate up on a schema ahead: %v", err)
		}
		if current, latest, dirty, err := SchemaVersion(ctx, u); err != nil || current != 2 || latest != 1 || dirty {
			t.Fatalf("SchemaVersion = %d, %d, %v, %v; want 2, 1, clean", current, latest, dirty, err)
		}
	})
}

// execOn runs sql on the database at u.
func execOn(t *testing.T, u, sql string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), sql); err != nil {
		t.Fatal(err)
	}
}

// A database URL that does not parse is refused without being quoted: it
// may carry a password.
func TestABadURLIsNeverQuoted(t *testing.T) {
	const bad = "postgres://runtime:hunter2@[::1/aishie?sslmode=nonsense"
	_, openErr := Open(t.Context(), bad)
	_, _, _, versionErr := SchemaVersion(t.Context(), bad)
	migrateErr := Migrate(bad, Up)
	for name, err := range map[string]error{"Open": openErr, "SchemaVersion": versionErr, "Migrate": migrateErr} {
		if err == nil {
			t.Errorf("%s took a URL that does not parse", name)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "runtime:") {
			t.Errorf("%s: %q quotes the URL", name, err)
		}
	}
}

// The bytes written ahead come back byte for byte, whitespace, key order
// and all: a resend under the same key must send the same body, or Core
// calls it an idempotency_conflict.
func TestArgsAreKeptByteForByte(t *testing.T) {
	s := openShared(t)
	args := []byte("{\"idempotency_key\":\"answer:x1:q1:1\",  \"body\":\"caf\\u00e9 é\\n\",\"course_id\":\"c1\" }\x00\xff")
	if _, err := s.PutAttempt(t.Context(), store.Attempt{Key: "answer:x1:q1:1", AgentID: "a1", MemberID: "m1", Args: args}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Attempt(t.Context(), "a1", "answer:x1:q1:1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Args, args) {
		t.Fatalf("args = %q, want %q", got.Args, args)
	}
}

// Raw usage is kept as jsonb, queryable, beside the numbers (§3.5).
func TestRawUsageIsKeptAsJSON(t *testing.T) {
	s := openShared(t)
	ctx := t.Context()
	err := s.RecordLLMCall(ctx, store.LLMCall{ID: "call-1", AgentID: "a1",
		RawUsage: []byte(`{"prompt_tokens": 1200, "prompt_tokens_details": {"cached_tokens": 1000}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordLLMCall(ctx, store.LLMCall{ID: "call-2", AgentID: "a1"}); err != nil {
		t.Fatal(err)
	}
	var cached *int64
	var missing bool
	err = s.pool.QueryRow(ctx, `
		SELECT (SELECT (raw_usage #>> '{prompt_tokens_details,cached_tokens}')::bigint FROM llm_call WHERE id = 'call-1'),
		       (SELECT raw_usage IS NULL FROM llm_call WHERE id = 'call-2')`).Scan(&cached, &missing)
	if err != nil {
		t.Fatal(err)
	}
	if cached == nil || *cached != 1000 || !missing {
		t.Fatalf("cached_tokens = %v, no raw usage stored as NULL = %v; want 1000, true", cached, missing)
	}
}

// A lease's time is the database's, so that workers whose clocks disagree
// still agree on who holds it.
func TestLeasesRunOnTheDatabasesClock(t *testing.T) {
	s := openShared(t)
	ctx := t.Context()
	if ok, err := s.AcquireLease(ctx, "agent:a1", "w1", time.Hour); err != nil || !ok {
		t.Fatalf("AcquireLease = %v, %v", ok, err)
	}
	var left time.Duration
	if err := s.pool.QueryRow(ctx, `SELECT expires_at - now() FROM lease WHERE name = 'agent:a1'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left <= 59*time.Minute || left > time.Hour {
		t.Fatalf("the lease has %s left on the database's clock, want about an hour", left)
	}
	// Expired on the database's clock, whatever the process's says.
	if _, err := s.pool.Exec(ctx, `UPDATE lease SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AcquireLease(ctx, "agent:a1", "w2", time.Hour); err != nil || !ok {
		t.Fatalf("AcquireLease of a lease expired on the database's clock = %v, %v", ok, err)
	}
}

// The schema, as the migrations leave it, is reviewed as text: every
// column, constraint and index. A change to it shows here, and needs
// -update and a migration that makes it.
func TestSchemaGolden(t *testing.T) {
	s := openShared(t)
	got := describeSchema(t, s)
	path := filepath.Join("testdata", "schema.golden")
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run TestSchemaGolden -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the schema differs from %s (run with -update if the change is meant):\n%s", path, got)
	}
}

// describeSchema writes the public schema out, one line a column, a
// constraint or an index, in a fixed order.
func describeSchema(t *testing.T, s *Store) []byte {
	t.Helper()
	ctx := t.Context()
	var b bytes.Buffer
	for _, q := range []struct{ title, sql string }{
		{"columns", `
			SELECT table_name || '.' || column_name || ' ' || data_type ||
			       CASE WHEN is_nullable = 'NO' THEN ' not null' ELSE '' END ||
			       CASE WHEN is_identity = 'YES' THEN ' identity' ELSE '' END ||
			       COALESCE(' default ' || column_default, '')
			  FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
			 ORDER BY table_name COLLATE "C", ordinal_position`},
		{"constraints", `
			SELECT line FROM (
			       SELECT conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid) AS line
			         FROM pg_constraint
			        WHERE connamespace = 'public'::regnamespace AND conrelid::regclass::text <> 'schema_migrations') c
			 ORDER BY line COLLATE "C"`},
		{"indexes", `
			SELECT indexdef FROM pg_indexes
			 WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
			 ORDER BY tablename COLLATE "C", indexname COLLATE "C"`},
	} {
		fmt.Fprintf(&b, "-- %s\n", q.title)
		rows, err := s.pool.Query(ctx, q.sql)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			fmt.Fprintln(&b, l)
		}
	}
	return b.Bytes()
}

// A store that is closed says so, rather than hanging.
func TestClosedStoreFails(t *testing.T) {
	needDB(t)
	s, err := Open(t.Context(), sharedURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := s.Cursor(ctx, "a1", "m1", store.CursorEvents); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Cursor on a closed store: err = %v, want a prompt failure", err)
	}
}
