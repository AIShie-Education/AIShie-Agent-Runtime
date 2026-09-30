package pgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/storetest"
)

var update = flag.Bool("update", false, "rewrite testdata/schema.golden from the migrated schema")

// tables are every table the migrations make, in the order TRUNCATE takes
// them.
var tables = []string{"lease", "attempt", "cursor", "note", "seat", "llm_call", "answer", "agent_state", "secret",
	"person", "hosted_agent", "hosted_course", "audit", "ocr_text", "site_setting", "school_offer", "site_price", "site_tenant_quota",
	"transcription_credential", "transcription_job"}

// newest is the newest migration the binary carries.
func newest(t *testing.T) uint {
	t.Helper()
	v, err := latestEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// openShared opens the run's scratch database, emptied, for one test.
func openShared(t *testing.T) *Store {
	t.Helper()
	needDB(t)
	return openEmptied(t, sharedURL)
}

// openEmptied opens the database at u, closed when t ends, and empties it.
func openEmptied(t *testing.T, u string) *Store {
	t.Helper()
	s := openOn(t, u)
	if _, err := s.pool.Exec(t.Context(), "TRUNCATE "+strings.Join(tables, ", ")+" RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	return s
}

// openOn opens the database at u as it is, closed when t ends.
func openOn(t *testing.T, u string) *Store {
	t.Helper()
	s, err := Open(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
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
	top := newest(t)

	version := func(want uint) {
		t.Helper()
		current, latest, dirty, err := SchemaVersion(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if latest != top {
			t.Errorf("latest = %d, want %d", latest, top)
		}
		if current != want || dirty {
			t.Fatalf("version = %d (dirty %v), want %d", current, dirty, want)
		}
	}
	// registry_rev and site_price_rev are tables too, which TRUNCATE
	// leaves alone.
	all := len(tables) + 2
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
		{Up, top, all},
		{Up, top, all}, // already there
		{Down, 0, 0},
		{Down, 0, 0}, // already there
		{Up, top, all},
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
	top := newest(t)

	t.Run("older", func(t *testing.T) {
		u := freshDatabase(t)
		_, err := Open(ctx, u)
		if err == nil || !strings.Contains(err.Error(), "version 0") || !strings.Contains(err.Error(), fmt.Sprintf("needs %d", top)) ||
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
		dirtyAt := fmt.Sprintf("dirty at version %d", top)
		if _, err := Open(ctx, u); err == nil || !strings.Contains(err.Error(), dirtyAt) {
			t.Fatalf("Open on a dirty schema: err = %v, want one saying it is dirty", err)
		}
		if err := Migrate(u, Up); err == nil || !strings.Contains(err.Error(), dirtyAt) {
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
		execOn(t, u, fmt.Sprintf(`UPDATE schema_migrations SET version = %d`, top+1))
		s, err := Open(ctx, u)
		if err != nil {
			t.Fatalf("Open on a schema ahead: %v", err)
		}
		_ = s.Close()
		// migrate up, as a deploy runs it first, leaves it as it is.
		if err := Migrate(u, Up); err != nil {
			t.Fatalf("Migrate up on a schema ahead: %v", err)
		}
		if current, latest, dirty, err := SchemaVersion(ctx, u); err != nil || current != top+1 || latest != top || dirty {
			t.Fatalf("SchemaVersion = %d, %d, %v, %v; want %d, %d, clean", current, latest, dirty, err, top+1, top)
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

// The ledger is only ever summed through the interface, so the shared suite
// would not see a field written to another's column. Every field here has a
// value of its own, and comes back from its own column.
func TestLedgerRowsKeepEveryField(t *testing.T) {
	s := openShared(t)
	ctx := t.Context()
	when := time.Date(2026, time.September, 27, 9, 30, 0, 123456000, time.UTC)

	call := store.LLMCall{ID: "call-1", At: when, TenantID: "ten-1", AgentID: "agt-1", CourseID: "crs-1",
		MemberID: "mem-1", ConversationID: "cnv-1", MessageID: "msg-1", OpenerMemberID: "opn-1",
		Adapter: "anthropic", Provider: "anthropic-direct", Model: "claude-test", Stop: "tool_calls", RawStop: "tool_use",
		Input: 101, CacheRead: 102, CacheWrite: 103, Output: 104, Reasoning: 105, Estimated: true,
		RawUsage: json.RawMessage(`{"input_tokens":101,"output_tokens":104}`), PriceVersion: "2026-09-01",
		CostPUSD: 106, KeySource: "school", LatencyMS: 107}
	if err := s.RecordLLMCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	var got store.LLMCall
	var raw string
	err := s.pool.QueryRow(ctx, `
		SELECT id, at, tenant_id, agent_id, course_id, member_id, conversation_id, message_id, opener_member_id,
		       adapter, provider, model, stop, raw_stop, input_tokens, cache_read_tokens, cache_write_tokens,
		       output_tokens, reasoning_tokens, estimated, raw_usage::text, price_version, cost_pusd, key_source, latency_ms, kind
		  FROM llm_call`).Scan(&got.ID, &got.At, &got.TenantID, &got.AgentID, &got.CourseID, &got.MemberID,
		&got.ConversationID, &got.MessageID, &got.OpenerMemberID, &got.Adapter, &got.Provider, &got.Model, &got.Stop,
		&got.RawStop, &got.Input, &got.CacheRead, &got.CacheWrite, &got.Output, &got.Reasoning, &got.Estimated, &raw,
		&got.PriceVersion, &got.CostPUSD, &got.KeySource, &got.LatencyMS, &got.Kind)
	if err != nil {
		t.Fatal(err)
	}
	var wantUsage, gotUsage any
	if err := json.Unmarshal(call.RawUsage, &wantUsage); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &gotUsage); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotUsage, wantUsage) {
		t.Errorf("raw_usage = %s, want %s", raw, call.RawUsage)
	}
	got.At, got.RawUsage = got.At.UTC(), call.RawUsage
	if got.Kind != store.CallAnswer {
		t.Errorf("llm_call kind = %q, want %q", got.Kind, store.CallAnswer)
	}
	got.Kind = ""
	if !reflect.DeepEqual(got, call) {
		t.Errorf("llm_call row:\n got %+v\nwant %+v", got, call)
	}

	answer := store.AnswerRecord{ID: "ans-1", At: when, TenantID: "ten-1", AgentID: "agt-1", CourseID: "crs-1",
		MemberID: "mem-1", ConversationID: "cnv-1", MessageID: "msg-1", OpenerMemberID: "opn-1",
		Key: "answer:cnv-1:msg-1:2", Outcome: store.OutcomeProposed, Billable: true, Turns: 3, ToolCalls: 4,
		InputTokens: 201, OutputTokens: 202, CostPUSD: 203, KeySource: "own", PromptHash: "sha256:feed", LatencyMS: 204}
	if err := s.RecordAnswer(ctx, answer); err != nil {
		t.Fatal(err)
	}
	var gotAnswer store.AnswerRecord
	err = s.pool.QueryRow(ctx, `
		SELECT id, at, tenant_id, agent_id, course_id, member_id, conversation_id, message_id, opener_member_id,
		       key, outcome, billable, turns, tool_calls, input_tokens, output_tokens, cost_pusd, key_source,
		       prompt_hash, latency_ms
		  FROM answer`).Scan(&gotAnswer.ID, &gotAnswer.At, &gotAnswer.TenantID, &gotAnswer.AgentID, &gotAnswer.CourseID,
		&gotAnswer.MemberID, &gotAnswer.ConversationID, &gotAnswer.MessageID, &gotAnswer.OpenerMemberID, &gotAnswer.Key,
		&gotAnswer.Outcome, &gotAnswer.Billable, &gotAnswer.Turns, &gotAnswer.ToolCalls, &gotAnswer.InputTokens,
		&gotAnswer.OutputTokens, &gotAnswer.CostPUSD, &gotAnswer.KeySource, &gotAnswer.PromptHash, &gotAnswer.LatencyMS)
	if err != nil {
		t.Fatal(err)
	}
	gotAnswer.At = gotAnswer.At.UTC()
	if gotAnswer != answer {
		t.Errorf("answer row:\n got %+v\nwant %+v", gotAnswer, answer)
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
// column, constraint, index and trigger. A change to it shows here, and needs
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
			        WHERE connamespace = 'public'::regnamespace AND conrelid::regclass::text <> 'schema_migrations'
			          -- PostgreSQL 18 names each NOT NULL as a constraint of its own
			          -- (contype n), where earlier versions keep none; the columns
			          -- above already say which are not null.
			          AND contype <> 'n') c
			 ORDER BY line COLLATE "C"`},
		{"indexes", `
			SELECT indexdef FROM pg_indexes
			 WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
			 ORDER BY tablename COLLATE "C", indexname COLLATE "C"`},
		{"triggers", `
			SELECT pg_get_triggerdef(t.oid) FROM pg_trigger t
			 WHERE NOT t.tgisinternal AND t.tgrelid::regclass::text <> 'schema_migrations'
			 ORDER BY t.tgrelid::regclass::text COLLATE "C", t.tgname COLLATE "C"`},
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

// A migration keeps the release before it working (CONTRIBUTING.md,
// Migrations): the seats a release before 0004 wrote come through it with
// empty snapshots, that release's own write of a seat still works on the
// new schema, and this one's reads what it wrote.
func TestSeatSnapshotMigratesTheSeatsBefore(t *testing.T) {
	u := freshDatabase(t)
	m, err := newMigrator(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(3); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// The release before 0004's SeatSeen.
	before := `INSERT INTO seat (agent_id, member_id, course_id, seen_at, gone_at)
		VALUES ($1, $2, $3, now(), NULL)
		ON CONFLICT (agent_id, member_id) DO UPDATE
		   SET course_id = EXCLUDED.course_id, seen_at = EXCLUDED.seen_at, gone_at = NULL`
	conn, err := pgx.Connect(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), before, "a1", "m1", "c1"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(u, Up); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), before, "a1", "m2", "c2"); err != nil {
		t.Fatalf("the release before's write on the new schema: %v", err)
	}
	s := openOn(t, u)
	seats, err := s.KnownSeats(t.Context(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 2 || seats[0].CourseCode != "" || seats[0].Status != "" || seats[0].Perms == nil || len(seats[0].Perms) != 0 {
		t.Fatalf("the seats from before: %+v", seats)
	}
	if err := s.SeatSeen(t.Context(), store.SeatRef{AgentID: "a1", MemberID: "m1", CourseID: "c1", CourseCode: "CS101",
		Status: "active", Perms: map[string]string{"conversation_answer": "autonomous"}}); err != nil {
		t.Fatal(err)
	}
	seats, err = s.KnownSeats(t.Context(), "a1")
	if err != nil || seats[0].CourseCode != "CS101" || seats[0].Perms["conversation_answer"] != "autonomous" {
		t.Fatalf("a seat written on the new schema: %+v, %v", seats, err)
	}
}

// The ledger's rows from before 0011 are answers' calls, and the release
// before's write of one still works on the new schema: both are summed as
// the answers', and the transcriber's apart.
func TestLedgerMigratesTheCallsBefore(t *testing.T) {
	u := freshDatabase(t)
	m, err := newMigrator(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(10); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// The release before 0011's RecordLLMCall, but its raw usage.
	before := `INSERT INTO llm_call (agent_id, id, at, tenant_id, course_id, member_id, conversation_id, message_id,
		                      opener_member_id, adapter, provider, model, stop, raw_stop,
		                      input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
		                      estimated, raw_usage, price_version, cost_pusd, key_source, latency_ms)
		VALUES ($1, $2, now(), 'ten_1', 'c1', 'm1', 'x1', 'q1', 'm2', 'openai_chat', 'openai', 'gpt-test', 'end', 'stop',
		        100, 0, 0, 10, 0, false, NULL, 'v1', 7, 'school', 5)`
	conn, err := pgx.Connect(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), before, "agt_1", "call-1"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(u, Up); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), before, "agt_1", "call-2"); err != nil {
		t.Fatalf("the release before's write on the new schema: %v", err)
	}
	s := openOn(t, u)
	if err := s.RecordLLMCall(t.Context(), store.LLMCall{ID: "tx-1", Kind: store.CallTranscription, Adapter: "gemini",
		Provider: "gemini", Model: "gemini-flash-lite", KeySource: "school", CostPUSD: 3, PriceVersion: "v1"}); err != nil {
		t.Fatal(err)
	}
	day := store.UTCDay(time.Now())
	rows, err := s.CostReport(t.Context(), store.CostQuery{Group: store.CostByTotal, Since: day.AddDate(0, 0, -1), Until: day.AddDate(0, 0, 2),
		Limit: 1})
	if err != nil || len(rows) != 1 || rows[0].ModelCalls != 2 || rows[0].CostPUSD != 14 || rows[0].Transcription.Calls != 1 ||
		rows[0].Transcription.CostPUSD != 3 {
		t.Fatalf("the report after the migration: %+v, %v", rows, err)
	}
}
