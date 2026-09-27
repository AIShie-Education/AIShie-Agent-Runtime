package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AcquireLease takes name for holder until ttl from now on the database's
// clock, when it is free, expired, or holder's already. One statement does
// it: of workers racing for one name, the first insert wins, and the others
// find its row and, it being neither theirs nor expired, update nothing.
func (s *Store) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if err := required("name", name, "holder", holder); err != nil {
		return false, err
	}
	if ttl <= 0 {
		return false, fmt.Errorf("store: lease %s for %s: ttl must be positive", name, ttl)
	}
	var got string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO lease (name, holder, expires_at)
		VALUES ($1, $2, now() + $3::interval)
		ON CONFLICT (name) DO UPDATE
		   SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
		 WHERE lease.holder = EXCLUDED.holder OR lease.expires_at < now()
		RETURNING holder`, name, holder, ttl).Scan(&got)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: acquire lease %s: %w", name, err)
	}
	return got == holder, nil
}

// ReleaseLease frees name if holder has it.
func (s *Store) ReleaseLease(ctx context.Context, name, holder string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM lease WHERE name = $1 AND holder = $2`, name, holder); err != nil {
		return fmt.Errorf("store: release lease %s: %w", name, err)
	}
	return nil
}
