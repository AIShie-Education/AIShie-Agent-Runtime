package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// secretColumns are what scanSecret reads, in its order.
const secretColumns = `id, tenant_id, kind, kek_id, wrapped_dek, nonce, ciphertext, hint, created_by, created_at` // #nosec G101 -- column names, not a credential.

func scanSecret(row pgx.Row) (*store.Secret, error) {
	var s store.Secret
	if err := row.Scan(&s.ID, &s.TenantID, &s.Kind, &s.KEKID, &s.WrappedDEK, &s.Nonce, &s.Ciphertext,
		&s.Hint, &s.CreatedBy, &s.CreatedAt); err != nil {
		return nil, err
	}
	utc(&s.CreatedAt)
	return &s, nil
}

// execer is what insertSecret needs of a pool or a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// insertSecret inserts sec on q, which may be a transaction's.
func insertSecret(ctx context.Context, q execer, sec store.Secret) error {
	if err := store.CheckSecret(sec); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `
		INSERT INTO secret (id, tenant_id, kind, kek_id, wrapped_dek, nonce, ciphertext, hint, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE($10::timestamptz, now()))`,
		sec.ID, sec.TenantID, sec.Kind, sec.KEKID, sec.WrappedDEK, sec.Nonce, sec.Ciphertext, sec.Hint, sec.CreatedBy, orNow(sec.CreatedAt))
	if isUniqueViolation(err) {
		return fmt.Errorf("secret %s: %w", sec.ID, store.ErrExists)
	}
	if err != nil {
		return fmt.Errorf("store: put secret %s: %w", sec.ID, err)
	}
	return nil
}

// PutSecret stores sec; an id already taken is store.ErrExists.
func (s *Store) PutSecret(ctx context.Context, sec store.Secret) error {
	return insertSecret(ctx, s.pool, sec)
}

// Secret is the secret id, or store.ErrNotFound.
func (s *Store) Secret(ctx context.Context, id string) (*store.Secret, error) {
	sec, err := scanSecret(s.pool.QueryRow(ctx, `SELECT `+secretColumns+` FROM secret WHERE id = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("secret %s: %w", id, store.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: secret %s: %w", id, err)
	}
	return sec, nil
}

// ListSecrets lists up to limit secrets whose ids sort after afterID,
// bytewise, as memstore sorts them.
func (s *Store) ListSecrets(ctx context.Context, afterID string, limit int) ([]store.Secret, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+secretColumns+` FROM secret
		 WHERE id COLLATE "C" > $1
		 ORDER BY id COLLATE "C"
		 LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list secrets: %w", err)
	}
	defer rows.Close()
	var out []store.Secret
	for rows.Next() {
		sec, err := scanSecret(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list secrets: %w", err)
		}
		out = append(out, *sec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list secrets: %w", err)
	}
	return out, nil
}

// RewrapSecret replaces the secret's wrapped data key, if fromKEKID still
// wraps it. One statement at READ COMMITTED does it: of two rewraps racing,
// the second waits for the first, finds the key changed, and is told so.
func (s *Store) RewrapSecret(ctx context.Context, id, fromKEKID, kekID string, wrapped []byte) error {
	if kekID == "" || len(wrapped) == 0 {
		return fmt.Errorf("store: secret %s: kek_id and the wrapped key required", id)
	}
	var rewrapped, found bool
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			WITH upd AS (UPDATE secret SET kek_id = $3, wrapped_dek = $4 WHERE id = $1 AND kek_id = $2 RETURNING 1)
			SELECT EXISTS (SELECT 1 FROM upd), EXISTS (SELECT 1 FROM secret WHERE id = $1)`,
			id, fromKEKID, kekID, wrapped).Scan(&rewrapped, &found)
	})
	switch {
	case err != nil:
		return fmt.Errorf("store: rewrap secret %s: %w", id, err)
	case rewrapped:
		return nil
	case !found:
		return fmt.Errorf("secret %s: %w", id, store.ErrNotFound)
	}
	return fmt.Errorf("secret %s: %w", id, store.ErrConflict)
}

// DeleteSecret destroys the secret; one that is not there is nothing. A
// secret a hosted agent refers to is refused by its foreign key, and
// reported as store.ErrInUse.
func (s *Store) DeleteSecret(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM secret WHERE id = $1`, id)
	if isForeignKeyViolation(err) {
		return fmt.Errorf("secret %s: %w", id, store.ErrInUse)
	}
	if err != nil {
		return fmt.Errorf("store: delete secret %s: %w", id, err)
	}
	return nil
}

// isUniqueViolation reports whether err is Postgres refusing a duplicate
// key.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isForeignKeyViolation reports whether err is Postgres refusing to delete
// a row another refers to, or to refer to one that is not there.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
