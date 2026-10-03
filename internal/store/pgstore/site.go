package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// SiteSettings lists every setting set, by name.
func (s *Store) SiteSettings(ctx context.Context) ([]store.SiteSetting, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, value::text, updated_by, updated_at FROM site_setting ORDER BY name COLLATE "C"`)
	if err != nil {
		return nil, fmt.Errorf("store: the site's settings: %w", err)
	}
	defer rows.Close()
	var out []store.SiteSetting
	for rows.Next() {
		var st store.SiteSetting
		var value string
		if err := rows.Scan(&st.Name, &value, &st.UpdatedBy, &st.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: the site's settings: %w", err)
		}
		st.Value = compact(value)
		utc(&st.UpdatedAt)
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: the site's settings: %w", err)
	}
	return out, nil
}

// PutSiteSetting sets st, in place of what was set.
func (s *Store) PutSiteSetting(ctx context.Context, st store.SiteSetting) error {
	st, err := store.CheckSiteSetting(st)
	if err != nil {
		return err
	}
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO site_setting (name, value, updated_by, updated_at)
			VALUES ($1, $2::jsonb, $3, COALESCE($4::timestamptz, now()))
			ON CONFLICT (name) DO UPDATE
			   SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
			st.Name, string(st.Value), st.UpdatedBy, orNow(st.UpdatedAt))
		return err
	})
	if err != nil {
		return fmt.Errorf("store: put site setting %s: %w", st.Name, err)
	}
	return nil
}

// DeleteSiteSetting unsets the setting. A delete of a setting not set is
// rolled back, as DeleteHostedCourse's is: the trigger moves the
// registry's revision on for every statement, even one that deletes
// nothing.
func (s *Store) DeleteSiteSetting(ctx context.Context, name string) error {
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM site_setting WHERE name = $1`, name)
		if err == nil && tag.RowsAffected() == 0 {
			return errNothingDeleted
		}
		return err
	})
	if errors.Is(err, errNothingDeleted) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: delete site setting %s: %w", name, err)
	}
	return nil
}

// offerColumns are what scanOffer reads, in its order.
const offerColumns = `id, label, adapter, provider, model, base_url, region, max_output_tokens, reasoning_effort, enabled,
	key_secret_id, key_hint, key_tested, version, created_by, created_at, updated_by, updated_at, openrouter`

func scanOffer(row pgx.Row) (*store.SchoolOffer, error) {
	var o store.SchoolOffer
	var routing []byte
	if err := row.Scan(&o.ID, &o.Label, &o.Adapter, &o.Provider, &o.Model, &o.BaseURL, &o.Region, &o.MaxOutputTokens,
		&o.ReasoningEffort, &o.Enabled, &o.KeySecretID, &o.KeyHint, &o.KeyTested, &o.Version, &o.CreatedBy, &o.CreatedAt,
		&o.UpdatedBy, &o.UpdatedAt, &routing); err != nil {
		return nil, err
	}
	if routing != nil {
		var r openrouter.Routing
		if err := json.Unmarshal(routing, &r); err != nil {
			return nil, fmt.Errorf("school offer %s: its upstream routing does not read: %w", o.ID, err)
		}
		o.OpenRouter = r.Canonical()
	}
	utc(&o.CreatedAt)
	utc(&o.UpdatedAt)
	return &o, nil
}

// routingColumn is an offer's upstream routing as its column keeps it:
// the canonical JSON, NULL for none.
func routingColumn(r *openrouter.Routing) []byte {
	if r.Canonical() == nil {
		return nil
	}
	return r.JSON()
}

// SchoolOffers lists the offers, by id.
func (s *Store) SchoolOffers(ctx context.Context) ([]store.SchoolOffer, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+offerColumns+` FROM school_offer ORDER BY id COLLATE "C"`)
	if err != nil {
		return nil, fmt.Errorf("store: the school's offers: %w", err)
	}
	defer rows.Close()
	var out []store.SchoolOffer
	for rows.Next() {
		o, err := scanOffer(rows)
		if err != nil {
			return nil, fmt.Errorf("store: the school's offers: %w", err)
		}
		out = append(out, *o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: the school's offers: %w", err)
	}
	return out, nil
}

// SchoolOffer is the offer id, or store.ErrNotFound.
func (s *Store) SchoolOffer(ctx context.Context, id string) (*store.SchoolOffer, error) {
	o, err := scanOffer(s.pool.QueryRow(ctx, `SELECT `+offerColumns+` FROM school_offer WHERE id = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("school offer %s: %w", id, store.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: school offer %s: %w", id, err)
	}
	return o, nil
}

// insertOfferKey stores an offer's new key in the transaction tx.
func insertOfferKey(ctx context.Context, tx pgx.Tx, o store.SchoolOffer, key store.Secret) error {
	if err := store.CheckOfferKey(o, key); err != nil {
		return err
	}
	return insertSecret(ctx, tx, key)
}

// CreateSchoolOffer stores o at version 1 with its key, in one
// transaction.
func (s *Store) CreateSchoolOffer(ctx context.Context, o store.SchoolOffer, key store.Secret) (*store.SchoolOffer, error) {
	if err := store.CheckSchoolOffer(o); err != nil {
		return nil, err
	}
	var out *store.SchoolOffer
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		if err := insertOfferKey(ctx, tx, o, key); err != nil {
			return err
		}
		var err error
		out, err = scanOffer(tx.QueryRow(ctx, `
			INSERT INTO school_offer (id, label, adapter, provider, model, base_url, region, max_output_tokens, reasoning_effort,
			                          enabled, key_secret_id, key_hint, key_tested, version, created_by, created_at, updated_by,
			                          updated_at, openrouter)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 1, $14, COALESCE($15::timestamptz, now()),
			        CASE WHEN $16::text = '' THEN $14 ELSE $16::text END, COALESCE($15::timestamptz, now()), $17::jsonb)
			RETURNING `+offerColumns,
			o.ID, o.Label, o.Adapter, o.Provider, o.Model, o.BaseURL, o.Region, o.MaxOutputTokens, o.ReasoningEffort, o.Enabled,
			o.KeySecretID, o.KeyHint, o.KeyTested, o.CreatedBy, orNow(o.CreatedAt), o.UpdatedBy, routingColumn(o.OpenRouter)))
		if isUniqueViolation(err) {
			return fmt.Errorf("school offer %s: %w", o.ID, store.ErrExists)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: create school offer %s: %w", o.ID, err)
	}
	return out, nil
}

// UpdateSchoolOffer writes o over the offer of its id, at o.Version when
// it is not 0, in one transaction with its new key and the destruction of
// the one before.
func (s *Store) UpdateSchoolOffer(ctx context.Context, o store.SchoolOffer, key ...store.Secret) (*store.SchoolOffer, error) {
	if err := store.CheckSchoolOffer(o); err != nil {
		return nil, err
	}
	if len(key) > 1 {
		return nil, fmt.Errorf("store: school offer %s: one key at most", o.ID)
	}
	var out *store.SchoolOffer
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		old, err := scanOffer(tx.QueryRow(ctx, `SELECT `+offerColumns+` FROM school_offer WHERE id = $1 FOR UPDATE`, o.ID))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("school offer %s: %w", o.ID, store.ErrNotFound)
		case err != nil:
			return err
		case o.Version != 0 && old.Version != o.Version:
			return fmt.Errorf("school offer %s at version %d: %w", o.ID, o.Version, store.ErrConflict)
		case len(key) == 0 && o.KeySecretID != old.KeySecretID:
			return fmt.Errorf("store: school offer %s: a new key must be given with the write", o.ID)
		}
		if len(key) == 1 {
			if err := insertOfferKey(ctx, tx, o, key[0]); err != nil {
				return err
			}
		}
		out, err = scanOffer(tx.QueryRow(ctx, `
			UPDATE school_offer
			   SET label = $2, adapter = $3, provider = $4, model = $5, base_url = $6, region = $7, max_output_tokens = $8,
			       reasoning_effort = $9, enabled = $10, key_secret_id = $11, key_hint = $12, key_tested = $13,
			       version = version + 1, updated_by = $14, updated_at = now(), openrouter = $15::jsonb
			 WHERE id = $1
			RETURNING `+offerColumns,
			o.ID, o.Label, o.Adapter, o.Provider, o.Model, o.BaseURL, o.Region, o.MaxOutputTokens, o.ReasoningEffort, o.Enabled,
			o.KeySecretID, o.KeyHint, o.KeyTested, o.UpdatedBy, routingColumn(o.OpenRouter)))
		if err != nil {
			return err
		}
		if old.KeySecretID != o.KeySecretID {
			if _, err := tx.Exec(ctx, `DELETE FROM secret WHERE id = $1`, old.KeySecretID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: update school offer %s: %w", o.ID, err)
	}
	return out, nil
}

// DeleteSchoolOffer destroys the offer and its key in one transaction, at
// version when it is not 0.
func (s *Store) DeleteSchoolOffer(ctx context.Context, id string, version int) error {
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		var key string
		err := tx.QueryRow(ctx, `
			DELETE FROM school_offer WHERE id = $1 AND ($2::int = 0 OR version = $2::int) RETURNING key_secret_id`,
			id, version).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			var there bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM school_offer WHERE id = $1)`, id).Scan(&there); err != nil {
				return err
			}
			if there {
				return fmt.Errorf("school offer %s at version %d: %w", id, version, store.ErrConflict)
			}
			return fmt.Errorf("school offer %s: %w", id, store.ErrNotFound)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM secret WHERE id = $1`, key)
		return err
	})
	if err != nil {
		return fmt.Errorf("store: delete school offer %s: %w", id, err)
	}
	return nil
}
