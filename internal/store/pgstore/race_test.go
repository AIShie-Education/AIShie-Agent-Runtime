package pgstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/storetest"
)

// A database whose default isolation is stricter than READ COMMITTED, set by
// an administrator on the database or the role, changes nothing the
// contract says: writers racing for a lease or an attempt's key are told
// plainly who won, never given a serialization error.
func TestContractUnderASerializableDefault(t *testing.T) {
	u := serializableDatabase(t)
	storetest.Run(t, func(t *testing.T) store.Store { return openEmptied(t, u) })
}

// serializableDatabase is a fresh scratch database, migrated up, whose
// default isolation is SERIALIZABLE, dropped when t ends.
func serializableDatabase(t *testing.T) string {
	t.Helper()
	u := freshDatabase(t)
	if err := Migrate(u, Up); err != nil {
		t.Fatal(err)
	}
	execOn(t, u, `DO $$ BEGIN
		EXECUTE format('ALTER DATABASE %I SET default_transaction_isolation = %L', current_database(), 'serializable');
	END $$`)
	var iso string
	if err := openOn(t, u).pool.QueryRow(t.Context(), `SHOW default_transaction_isolation`).Scan(&iso); err != nil {
		t.Fatal(err)
	}
	if iso != "serializable" {
		t.Fatalf("default_transaction_isolation = %q, want serializable: the test would prove nothing", iso)
	}
	return u
}

// A writer that finds its row held by another's uncommitted write waits for
// it, and is then told plainly what that write left: the key is taken, with
// its bytes; the lease is held. The overlap is made certain rather than
// hoped for: the other write is held open until every racer is seen waiting
// on it, and only then committed.
func TestAWriterWaitingOnAnothersRowIsToldPlainly(t *testing.T) {
	for _, c := range []struct {
		name string
		url  func(*testing.T) string
	}{
		{"read committed", func(t *testing.T) string { needDB(t); return sharedURL }},
		{"serializable", serializableDatabase},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := c.url(t)
			// Two workers, with two racers each, on pools of their own.
			workers := []*Store{openEmptied(t, u), openOn(t, u)}
			const perWorker = 2

			t.Run("an attempt's key", func(t *testing.T) {
				first := []byte(`{"body":"first"}`)
				results := raceWhileHeld(t, u, workers, perWorker, func(tx pgx.Tx) error {
					_, err := tx.Exec(t.Context(), `
						INSERT INTO attempt (agent_id, key, member_id, course_id, conversation_id, message_id, attempt_no,
						                     tool, args, kind, state, created_at, updated_at)
						VALUES ('a1', 'answer:x1:q1:1', 'm1', 'c1', 'x1', 'q1', 1,
						        'conversation_answer', $1, 'model', 'sending', now(), now())`, first)
					return err
				}, func(s *Store, racer string) (any, error) {
					return s.PutAttempt(t.Context(), store.Attempt{Key: "answer:x1:q1:1", AgentID: "a1", MemberID: "m1",
						ConversationID: "x1", MessageID: "q1", No: 1, Args: fmt.Appendf(nil, `{"body":"from %s"}`, racer)})
				})
				for _, r := range results {
					row, _ := r.got.(*store.Attempt)
					if !errors.Is(r.err, store.ErrExists) || row == nil || !bytes.Equal(row.Args, first) {
						t.Errorf("%s: PutAttempt = %v, %v; want ErrExists with the bytes written first", r.racer, row, r.err)
					}
				}
			})

			// A delete named with the token it read, and a pause at the
			// version it read, that wait on a new token being written are
			// refused once it commits, and write nothing.
			for _, hc := range []struct {
				name string
				race func(s *Store) (any, error)
			}{
				{"a hosted agent deleted with the token it had", func(s *Store) (any, error) {
					return nil, s.DeleteHostedAgent(t.Context(), "agt_held", store.DeleteIf{TokenSecretID: "sec_old"})
				}},
				{"a hosted agent paused at the version it was at", func(s *Store) (any, error) {
					return s.SetHostedAgentPaused(t.Context(), "agt_held", true, 1)
				}},
			} {
				t.Run(hc.name, func(t *testing.T) {
					execOn(t, u, `DELETE FROM hosted_agent`)
					execOn(t, u, `DELETE FROM secret`)
					sec := func(id string) store.Secret {
						return store.Secret{ID: id, TenantID: "ten_o", Kind: store.SecretCoreToken, KEKID: "local:v1",
							WrappedDEK: []byte("w"), Nonce: []byte("n"), Ciphertext: []byte("c")}
					}
					_, err := workers[0].CreateHostedAgent(t.Context(), store.HostedAgent{ID: "agt_held", CoreActorID: "actor-held",
						OwnerActorID: "o", TenantID: "ten_o", TokenSecretID: "sec_old"}, sec("sec_old"))
					if err != nil {
						t.Fatal(err)
					}
					if err := workers[0].PutSecret(t.Context(), sec("sec_new")); err != nil {
						t.Fatal(err)
					}
					results := raceWhileHeld(t, u, workers, perWorker, func(tx pgx.Tx) error {
						_, err := tx.Exec(t.Context(), `UPDATE hosted_agent SET token_secret_id = 'sec_new', version = version + 1 WHERE id = 'agt_held'`)
						return err
					}, func(s *Store, _ string) (any, error) { return hc.race(s) })
					for _, r := range results {
						if !errors.Is(r.err, store.ErrConflict) {
							t.Errorf("%s: %v, want ErrConflict", r.racer, r.err)
						}
					}
					row, err := workers[0].HostedAgent(t.Context(), "agt_held")
					if err != nil || row.TokenSecretID != "sec_new" || row.Paused || row.Version != 2 {
						t.Errorf("the agent after: %+v %v", row, err)
					}
				})
			}

			for _, lc := range []struct{ name, setup, hold string }{
				{"a free lease", "", `INSERT INTO lease VALUES ('agent:held', 'w0', now() + interval '1 minute')`},
				{"a lapsed lease",
					`INSERT INTO lease VALUES ('agent:held', 'dead', now() - interval '1 minute')`,
					`UPDATE lease SET holder = 'w0', expires_at = now() + interval '1 minute' WHERE name = 'agent:held'`},
			} {
				t.Run(lc.name, func(t *testing.T) {
					execOn(t, u, `DELETE FROM lease`)
					if lc.setup != "" {
						execOn(t, u, lc.setup)
					}
					results := raceWhileHeld(t, u, workers, perWorker, func(tx pgx.Tx) error {
						_, err := tx.Exec(t.Context(), lc.hold)
						return err
					}, func(s *Store, racer string) (any, error) {
						return s.AcquireLease(t.Context(), "agent:held", racer, time.Minute)
					})
					for _, r := range results {
						if ok, _ := r.got.(bool); r.err != nil || ok {
							t.Errorf("%s: AcquireLease = %v, %v; want false, as w0 took it first", r.racer, r.got, r.err)
						}
					}
				})
			}
		})
	}
}

// racerResult is what one racer was told.
type racerResult struct {
	racer string
	got   any
	err   error
}

// raceWhileHeld runs hold in a transaction of its own, starts perWorker
// racers on each worker, waits until each is blocked on a lock, commits
// hold's transaction, and returns what every racer was told.
func raceWhileHeld(t *testing.T, u string, workers []*Store, perWorker int,
	hold func(pgx.Tx) error, race func(s *Store, racer string) (any, error),
) []racerResult {
	t.Helper()
	ctx := t.Context()
	// Each racer starts on a connection already open, so that what it waits
	// on is the held row, and not a connection's start.
	for _, s := range workers {
		warm(t, s, perWorker)
	}
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	// The racers are watched from a connection of its own: inside the held
	// transaction, pg_stat_activity would stay as it was first read, and
	// never show a racer that connected after.
	watcher, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watcher.Close(context.Background()) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := hold(tx); err != nil {
		t.Fatal(err)
	}

	var (
		wg      sync.WaitGroup
		results = make([]racerResult, len(workers)*perWorker)
	)
	for i := range results {
		s, racer := workers[i%len(workers)], fmt.Sprintf("racer-%d", i)
		wg.Go(func() {
			got, err := race(s, racer)
			results[i] = racerResult{racer, got, err}
		})
	}

	waited := waitForBlocked(ctx, watcher, len(results))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if waited != nil {
		t.Fatalf("%v; the racers were told %+v", waited, results)
	}
	return results
}

// warm opens n connections in s's pool, and leaves them idle there.
func warm(t *testing.T, s *Store, n int) {
	t.Helper()
	var conns []*pgxpool.Conn
	defer func() {
		for _, c := range conns {
			c.Release()
		}
	}()
	for range n {
		c, err := s.pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
}

// waitForBlocked waits until n client sessions of the database, other than
// conn's, are waiting on a lock. Each poll is a statement, and so a
// transaction, of its own, and reads the sessions as they are then.
func waitForBlocked(ctx context.Context, conn *pgx.Conn, n int) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked int
		err := conn.QueryRow(ctx, `
			SELECT count(DISTINCT l.pid) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			 WHERE NOT l.granted AND a.datname = current_database()
			   AND a.backend_type = 'client backend' AND a.pid <> pg_backend_pid()`).Scan(&blocked)
		switch {
		case err != nil:
			return err
		case blocked >= n:
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf("only %d of %d racers were ever seen waiting on the held row", blocked, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// raceResult is what one worker was told in one round.
type raceResult struct {
	fresh, lapsed bool
	row           *store.Attempt
	putErr        error
	errs          []error
}

// Two workers, each with a pool of its own, race for the same leases and the
// same attempt keys, as two replicas of the runtime do (Core's
// docs/agent-runtime.md §7.4): of each pair exactly one wins, the other is
// told so without an error, and the loser of a key is handed the winner's
// bytes to send.
func TestTwoWorkersRace(t *testing.T) {
	// openShared empties the database, and skips the test where there is
	// none, before the second worker opens it.
	workers := []*Store{openShared(t), openOn(t, sharedURL)}
	ctx := t.Context()
	const rounds = 40

	// Leases a dead worker held, long lapsed on the database's clock when
	// the race starts.
	for i := range rounds {
		if ok, err := workers[0].AcquireLease(ctx, fmt.Sprintf("agent:lapsed-%d", i), "dead", time.Millisecond); err != nil || !ok {
			t.Fatalf("AcquireLease by the dead worker = %v, %v", ok, err)
		}
	}
	time.Sleep(20 * time.Millisecond)

	for i := range rounds {
		key := fmt.Sprintf("answer:x1:q%d:1", i)
		var (
			wg      sync.WaitGroup
			start   = make(chan struct{})
			results [2]raceResult
		)
		for w, s := range workers {
			holder := fmt.Sprintf("w%d", w+1)
			wg.Go(func() {
				r := &results[w]
				<-start
				var err error
				if r.fresh, err = s.AcquireLease(ctx, fmt.Sprintf("agent:fresh-%d", i), holder, time.Minute); err != nil {
					r.errs = append(r.errs, err)
				}
				if r.lapsed, err = s.AcquireLease(ctx, fmt.Sprintf("agent:lapsed-%d", i), holder, time.Minute); err != nil {
					r.errs = append(r.errs, err)
				}
				r.row, r.putErr = s.PutAttempt(ctx, store.Attempt{
					Key: key, AgentID: "a1", MemberID: "m1", ConversationID: "x1", MessageID: fmt.Sprintf("q%d", i), No: 1,
					Tool: "conversation_answer", Args: fmt.Appendf(nil, `{"body":"from %s"}`, holder), Kind: "model",
				})
			})
		}
		close(start)
		wg.Wait()

		for w, r := range results {
			for _, err := range r.errs {
				t.Errorf("round %d, w%d: %v", i, w+1, err)
			}
		}
		if results[0].fresh == results[1].fresh {
			t.Errorf("round %d: a free lease went to both or neither (%v, %v)", i, results[0].fresh, results[1].fresh)
		}
		if results[0].lapsed == results[1].lapsed {
			t.Errorf("round %d: a lapsed lease went to both or neither (%v, %v)", i, results[0].lapsed, results[1].lapsed)
		}

		winner, loser := results[0], results[1]
		if winner.putErr != nil {
			winner, loser = loser, winner
		}
		if winner.putErr != nil || !errors.Is(loser.putErr, store.ErrExists) {
			t.Fatalf("round %d: PutAttempt told the workers %v and %v; want one nil, one ErrExists", i, winner.putErr, loser.putErr)
		}
		if loser.row == nil || !bytes.Equal(loser.row.Args, winner.row.Args) {
			t.Fatalf("round %d: the loser was handed other bytes than the winner's", i)
		}
		stored, err := workers[0].Attempt(ctx, "a1", key)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stored.Args, winner.row.Args) {
			t.Fatalf("round %d: stored %q, want the winner's %q", i, stored.Args, winner.row.Args)
		}
	}
}

// A pool of one connection is enough: nothing the store does holds one
// connection while it waits for another.
func TestOneConnectionIsEnough(t *testing.T) {
	needDB(t)
	openShared(t) // empties the database
	u, err := url.Parse(sharedURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("pool_max_conns", "1")
	u.RawQuery = q.Encode()
	s := openOn(t, u.String())
	if n := s.pool.Config().MaxConns; n != 1 {
		t.Fatalf("the pool has %d connections, want 1: the test would prove nothing", n)
	}
	// A store that waited on itself would hang: it fails instead.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	a := store.Attempt{Key: "answer:x1:q1:1", AgentID: "a1", MemberID: "m1", Args: []byte(`{}`)}
	if _, err := s.PutAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAttempt(ctx, a); !errors.Is(err, store.ErrExists) {
		t.Fatalf("PutAttempt of a key taken on a one-connection pool: err = %v, want ErrExists", err)
	}
	if ok, err := s.AcquireLease(ctx, "agent:a1", "w1", time.Minute); err != nil || !ok {
		t.Fatalf("AcquireLease on a one-connection pool = %v, %v", ok, err)
	}
}
