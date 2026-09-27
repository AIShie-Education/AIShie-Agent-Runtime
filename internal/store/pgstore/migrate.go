package pgstore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// migrations are the schema's, embedded so that the binary carries them.
// Each file holds its own BEGIN and COMMIT and is sent as one batch, so that
// it applies whole or not at all; golang-migrate's dirty flag covers the gap
// between a file committing and its version being recorded.
//
//go:embed migrations/*.sql
var migrations embed.FS

const migrationsDir = "migrations"

// Directions Migrate takes.
const (
	Up   = "up"
	Down = "down"
)

// errBadURL stands for every failure to parse the database URL. The parser's
// own message quotes the URL, password and all where it cannot find it, and
// no secret goes into an error.
var errBadURL = errors.New("pgstore: the database URL does not parse")

// Migrate moves the schema up to the newest migration embedded, or down,
// reverting every one: down destroys all the runtime's state, the ledger
// included. Already where it is asked to be is not an error.
//
// Up leaves a schema that is ahead of the binary as it is: a newer release
// migrated it, and this one is a rollback, which runs against it (Open takes
// a schema that is ahead). golang-migrate would otherwise fail looking for
// the file of a version it does not know.
func Migrate(databaseURL string, direction string) error {
	if direction != Up && direction != Down {
		return fmt.Errorf("pgstore: migrate %q: want %s or %s", direction, Up, Down)
	}
	m, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()

	if direction == Down {
		return migrateErr(m.Down())
	}
	current, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("pgstore: read the schema version: %w", err)
	}
	latest, err := latestEmbedded()
	if err != nil {
		return err
	}
	if !dirty && current > latest {
		return nil
	}
	return migrateErr(m.Up())
}

// newMigrator connects golang-migrate to the database on a connection of
// its own. The caller closes it.
func newMigrator(databaseURL string) (*migrate.Migrate, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, errBadURL
	}
	src, err := iofs.New(migrations, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("pgstore: embedded migrations: %w", err)
	}
	db := stdlib.OpenDB(*cfg)
	drv, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		_ = db.Close()
		_ = src.Close()
		return nil, fmt.Errorf("pgstore: connect to migrate: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", drv)
	if err != nil {
		_ = drv.Close()
		_ = src.Close()
		return nil, fmt.Errorf("pgstore: migrate: %w", err)
	}
	return m, nil
}

// migrateErr is err as the operator needs it: nothing to do is no error, and
// a dirty schema says what to do about it.
func migrateErr(err error) error {
	var dirty migrate.ErrDirty
	switch {
	case err == nil, errors.Is(err, migrate.ErrNoChange):
		return nil
	case errors.As(err, &dirty):
		return fmt.Errorf("pgstore: %w", dirtyError(uint(max(dirty.Version, 0))))
	default:
		return fmt.Errorf("pgstore: migrate: %w", err)
	}
}

// dirtyError says that a migration failed half-way, and what to do.
func dirtyError(version uint) error {
	return fmt.Errorf("the schema is dirty at version %d: a migration failed half-way; "+
		"fix the database by hand, then set schema_migrations to the last migration fully applied, with dirty false", version)
}

// SchemaVersion reads the version golang-migrate recorded, without taking
// its lock or creating its table, and the newest version embedded. A
// database no migration has touched is at 0.
func SchemaVersion(ctx context.Context, databaseURL string) (current, latest uint, dirty bool, err error) {
	latest, err = latestEmbedded()
	if err != nil {
		return 0, 0, false, err
	}
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return 0, 0, false, errBadURL
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return 0, 0, false, fmt.Errorf("pgstore: connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	current, dirty, err = readVersion(ctx, conn)
	return current, latest, dirty, err
}

// rowQuerier is what readVersion needs of a connection or a pool.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readVersion is the version golang-migrate recorded; 0 when it has
// recorded none, or has never run.
func readVersion(ctx context.Context, q rowQuerier) (version uint, dirty bool, err error) {
	var v int64
	err = q.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&v, &dirty)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case errors.As(err, &pgErr) && pgErr.Code == "42P01": // undefined_table
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("pgstore: read the schema version: %w", err)
	case v < 0: // golang-migrate's "none", should a Force have written it
		return 0, dirty, nil
	}
	return uint(v), dirty, nil
}

// latestEmbedded is the newest migration version the binary carries.
func latestEmbedded() (uint, error) {
	src, err := iofs.New(migrations, migrationsDir)
	if err != nil {
		return 0, fmt.Errorf("pgstore: embedded migrations: %w", err)
	}
	defer func() { _ = src.Close() }()
	v, err := src.First()
	if err != nil {
		return 0, fmt.Errorf("pgstore: embedded migrations: %w", err)
	}
	for {
		next, err := src.Next(v)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return v, nil
		}
		if err != nil {
			return 0, fmt.Errorf("pgstore: embedded migrations: %w", err)
		}
		v = next
	}
}
