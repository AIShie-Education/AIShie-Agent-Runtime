// Package pgstore keeps the runtime's state in PostgreSQL, for a cluster of
// workers (docs/design.md §8): its leases, on the database's clock, are what
// let several workers share agents. The database is the runtime's own,
// never Core's.
//
// The schema is golang-migrate's, embedded (Migrate). Open never migrates:
// a migration is somebody's decision, made with `aishie-runtime migrate`.
// But it does not start against a schema older than the one it was built
// for, either.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// Store is store.Store on a pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*Store)(nil)

// Open connects to databaseURL and checks the schema: dirty, or older than
// the newest migration embedded, it refuses to start and says what to run.
// A schema that is ahead is taken: that is what a rolling deploy, or a
// rollback, looks like (migrations are additive).
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errBadURL
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgstore: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: connect: %w", err)
	}
	if err := checkSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// checkSchema refuses a schema this binary cannot work with.
func checkSchema(ctx context.Context, pool *pgxpool.Pool) error {
	latest, err := latestEmbedded()
	if err != nil {
		return err
	}
	have, dirty, err := readVersion(ctx, pool)
	switch {
	case err != nil:
		return err
	case dirty:
		return fmt.Errorf("pgstore: %w", dirtyError(have))
	case have < latest:
		return fmt.Errorf("pgstore: the schema is at version %d and this runtime needs %d; run `aishie-runtime migrate up` first", have, latest)
	}
	return nil
}

// Close releases the pool's connections.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// required refuses a write missing the ids it is keyed on, before the
// database is asked. pairs holds a field's name, then its value.
func required(pairs ...string) error {
	var missing []string
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			missing = append(missing, pairs[i])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("store: %s required", strings.Join(missing, ", "))
	}
	return nil
}

// orNow is t for a timestamptz parameter written COALESCE($n::timestamptz,
// now()): nil, so the database's now, when t is zero.
func orNow(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// utc is t in UTC: pgx gives timestamptz in the process's zone, and memstore
// gives UTC.
func utc(t *time.Time) {
	*t = t.UTC()
}

// errNoScope is Spend asked for everything, which no quota is.
var errNoScope = errors.New("store: spend needs at least one scope field")
