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
	"transcription_credential", "transcription_job", "agent_token", "search_passage", "search_file"}

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

// An attempt sent back for changes is one the release before reads as
// posting nothing; 0015's down makes it one rejected, with what was asked
// as its reason, and the old constraint holds again. Its note in memory,
// of a kind the release before does not read, becomes a rejection's, with
// what was asked as its text, through which that release reads a
// rejection's reason. A note whose request was not read stays as it is,
// passed over by that release, which would read it, made a rejection's,
// as one given no reason; the conversation's other notes are left as they
// are, a rejection whose reason was not read among them.
func TestChangesRequestedMigratesBack(t *testing.T) {
	u := freshDatabase(t)
	ctx := t.Context()
	if err := Migrate(u, Up); err != nil {
		t.Fatal(err)
	}
	s := openOn(t, u)
	a := store.Attempt{Key: "answer:x1:q1:1", AgentID: "a1", MemberID: "m1", CourseID: "c1", ConversationID: "x1", MessageID: "q1",
		No: 1, Tool: "conversation_answer", Args: []byte(`{"body":"b"}`), Kind: "model"}
	if _, err := s.PutAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttempt(ctx, "a1", a.Key, store.Outcome{State: store.AttemptChangesRequested, ActionID: "act-1", Reason: "Cite it."}); err != nil {
		t.Fatal(err)
	}
	// The second attempt, sent back in turn, its note not read.
	a2 := a
	a2.Key, a2.No = "answer:x1:q1:2", 2
	if _, err := s.PutAttempt(ctx, a2); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAttempt(ctx, "a1", a2.Key, store.Outcome{State: store.AttemptChangesRequested, ActionID: "act-2"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []store.Note{
		{Kind: store.NoteRejected, Text: "Too terse.", MessageID: "q1"},
		{Kind: store.NoteChangesRequested, Text: "Cite it.", MessageID: "q1"},
		{Kind: store.NoteChangesRequested, Text: "", MessageID: "q1"},
		{Kind: store.NoteRejectedUnread, Text: "", MessageID: "q1"},
		{Kind: store.NoteAnswered, Text: "You answered this question (message p2).", MessageID: "p2"},
	} {
		n.AgentID, n.MemberID, n.ConversationID = "a1", "m1", "x1"
		if err := s.AddNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	m, err := newMigrator(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(14); err != nil {
		t.Fatalf("0015 down: %v", err)
	}
	if _, err := m.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var state, action, reason string
	if err := conn.QueryRow(ctx, `SELECT state, action_id, reason FROM attempt WHERE key = $1`, a.Key).Scan(&state, &action, &reason); err != nil ||
		state != "rejected" || action != "act-1" || reason != "Cite it." {
		t.Fatalf("after the down: %s %s %q, %v; want rejected act-1 with its reason", state, action, reason, err)
	}
	// The notes as the release before reads them, oldest first.
	rows, err := conn.Query(ctx, `SELECT kind, text FROM note WHERE agent_id = 'a1' AND member_id = 'm1' AND conversation_id = 'x1' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	for rows.Next() {
		var kind, text string
		if err := rows.Scan(&kind, &text); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, kind+": "+text)
	}
	if want := []string{"rejected: Too terse.", "rejected: Cite it.", "changes_requested: ", "rejected_unread: ",
		"answered: You answered this question (message p2)."}; rows.Err() != nil ||
		strings.Join(notes, " | ") != strings.Join(want, " | ") {
		t.Errorf("the notes after the down: %q, %v; want %q", notes, rows.Err(), want)
	}
	if _, err := conn.Exec(ctx, `UPDATE attempt SET state = 'changes_requested' WHERE key = $1`, a.Key); err == nil {
		t.Error("the release before's schema took an attempt in changes_requested")
	}
	if err := Migrate(u, Up); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// The hosted agents from before 0013 hold the tokens their owners pasted:
// read on the new schema as tokens not issued, which the worker replaces.
// The release before reads every row that holds a token on it, an issued
// token's among them; and 0013's down deletes the rows that hold none,
// which only this release writes, and forgets the operator's tokens.
func TestHostingByIDMigratesTheRowsBefore(t *testing.T) {
	u := freshDatabase(t)
	ctx := t.Context()
	m, err := newMigrator(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(12); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	secret := func(id string) {
		t.Helper()
		if _, err := conn.Exec(ctx, `INSERT INTO secret (id, tenant_id, kind, kek_id, wrapped_dek, nonce, ciphertext, hint, created_by, created_at)
			VALUES ($1, 'ten_o1', 'core_token', 'local:v1', '\x01', '\x02', '\x03', 'ais_k7v2m4qhx3ab…', 'o1', now())`, id); err != nil {
			t.Fatal(err)
		}
	}
	// The release before's CreateHostedAgent.
	insert := `INSERT INTO hosted_agent (id, core_actor_id, owner_actor_id, owner_verified, tenant_id, display_name,
		token_secret_id, token_hint, key_secret_id, key_hint, key_provider, paused, settings, version, created_at, updated_at)
		VALUES ($1, $2, 'o1', true, 'ten_o1', 'Helper', $3, 'ais_k7v2m4qhx3ab…', NULL, '', '', false, '{}', 1, now(), now())`
	secret("sec_pasted")
	if _, err := conn.Exec(ctx, insert, "agt_pasted", "actor-1", "sec_pasted"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(u, Up); err != nil {
		t.Fatal(err)
	}
	s := openOn(t, u)
	pasted, err := s.HostedAgent(ctx, "agt_pasted")
	if err != nil || pasted.TokenSecretID != "sec_pasted" || pasted.TokenIssued || pasted.TokenCredentialID != "" {
		t.Fatalf("a row from before: %+v, %v", pasted, err)
	}
	// The release before's write, and its read, on the new schema.
	secret("sec_old")
	if _, err := conn.Exec(ctx, insert, "agt_old", "actor-2", "sec_old"); err != nil {
		t.Fatalf("the release before's write on the new schema: %v", err)
	}
	issued := *pasted
	issued.TokenSecretID, issued.TokenIssued, issued.TokenCredentialID = "sec_issued", true, "cred-1"
	if _, err := s.UpdateHostedAgent(ctx, issued, store.Secret{ID: "sec_issued", TenantID: "ten_o1", Kind: store.SecretCoreToken,
		KEKID: "local:v1", WrappedDEK: []byte{1}, Nonce: []byte{2}, Ciphertext: []byte{3}, Hint: "ais_k7v2m4qhx3ab…"}); err != nil {
		t.Fatal(err)
	}
	before := `SELECT id, token_secret_id FROM hosted_agent ORDER BY id`
	read := func(want ...string) {
		t.Helper()
		rows, err := conn.Query(ctx, before)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var id, token string
			if err := rows.Scan(&id, &token); err != nil {
				t.Fatalf("the release before's read: %v", err)
			}
			got = append(got, id+":"+token)
		}
		if rows.Err() != nil || strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("the release before reads %q, %v; want %q", got, rows.Err(), want)
		}
	}
	read("agt_old:sec_old", "agt_pasted:sec_issued")

	// Rows of this release's alone: hosted by an id before a model, and an
	// operator's agent's token.
	if _, err := s.CreateHostedAgent(ctx, store.HostedAgent{ID: "agt_new", CoreActorID: "actor-3", OwnerActorID: "o1", OwnerVerified: true,
		TenantID: "ten_o1", DisplayName: "New"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAgentToken(ctx, store.AgentToken{AgentID: "tutor", CoreActorID: "actor-4", SecretID: "sec_yaml", CredentialID: "cred-2"},
		store.Secret{ID: "sec_yaml", TenantID: "operator", Kind: store.SecretCoreToken, KEKID: "local:v1", WrappedDEK: []byte{1},
			Nonce: []byte{2}, Ciphertext: []byte{3}}, ""); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	m, err = newMigrator(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(12); err != nil {
		t.Fatalf("0013 down: %v", err)
	}
	if _, err := m.Close(); err != nil {
		t.Fatal(err)
	}
	read("agt_old:sec_old", "agt_pasted:sec_issued")
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM secret WHERE id = 'sec_yaml'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the operator's token's secret after the down: %d, %v", n, err)
	}
	if err := Migrate(u, Up); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
