package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// priceColumns are what scanPrice reads, in its order.
const priceColumns = `id, provider, model, from_day, input_pusd, cache_read_pusd, cache_write_pusd, output_pusd, version,
	created_by, created_at, updated_by, updated_at`

func scanPrice(row pgx.Row) (*store.SitePrice, error) {
	var p store.SitePrice
	if err := row.Scan(&p.ID, &p.Provider, &p.Model, &p.From, &p.InputPUSD, &p.CacheReadPUSD, &p.CacheWritePUSD, &p.OutputPUSD,
		&p.Version, &p.CreatedBy, &p.CreatedAt, &p.UpdatedBy, &p.UpdatedAt); err != nil {
		return nil, err
	}
	y, m, d := p.From.Date()
	p.From = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	utc(&p.CreatedAt)
	utc(&p.UpdatedAt)
	return &p, nil
}

// SitePrices lists the site's rows, by id, and when they last changed,
// read in one snapshot.
func (s *Store) SitePrices(ctx context.Context) ([]store.SitePrice, time.Time, error) {
	var out []store.SitePrice
	var changed time.Time
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+priceColumns+` FROM site_price ORDER BY id COLLATE "C"`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPrice(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT changed_at FROM site_price_rev`).Scan(&changed)
	})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: the site's prices: %w", err)
	}
	if changed.Equal(time.Unix(0, 0)) {
		changed = time.Time{}
	}
	return out, changed.UTC(), nil
}

// priceErr is a write's error in the store's words: an id, or a
// provider, model and from, taken is store.ErrExists.
func priceErr(p store.SitePrice, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("site price %s: %w", p.ID, store.ErrExists)
	}
	return err
}

// CreateSitePrice stores p at version 1.
func (s *Store) CreateSitePrice(ctx context.Context, p store.SitePrice) (*store.SitePrice, error) {
	p, err := store.CheckSitePrice(p)
	if err != nil {
		return nil, err
	}
	var out *store.SitePrice
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanPrice(tx.QueryRow(ctx, `
			INSERT INTO site_price (id, provider, model, from_day, input_pusd, cache_read_pusd, cache_write_pusd, output_pusd, version,
			                        created_by, created_at, updated_by, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, $9, COALESCE($10::timestamptz, now()),
			        CASE WHEN $11::text = '' THEN $9 ELSE $11::text END, COALESCE($10::timestamptz, now()))
			RETURNING `+priceColumns,
			p.ID, p.Provider, p.Model, p.From, p.InputPUSD, p.CacheReadPUSD, p.CacheWritePUSD, p.OutputPUSD, p.CreatedBy,
			orNow(p.CreatedAt), p.UpdatedBy))
		return priceErr(p, err)
	})
	if err != nil {
		return nil, fmt.Errorf("store: create site price %s: %w", p.ID, err)
	}
	return out, nil
}

// UpdateSitePrice writes p over the row of its id, at p.Version when it
// is not 0.
func (s *Store) UpdateSitePrice(ctx context.Context, p store.SitePrice) (*store.SitePrice, error) {
	p, err := store.CheckSitePrice(p)
	if err != nil {
		return nil, err
	}
	var out *store.SitePrice
	err = s.readCommitted(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanPrice(tx.QueryRow(ctx, `
			UPDATE site_price
			   SET provider = $2, model = $3, from_day = $4, input_pusd = $5, cache_read_pusd = $6, cache_write_pusd = $7,
			       output_pusd = $8, version = version + 1, updated_by = $9, updated_at = now()
			 WHERE id = $1 AND ($10::int = 0 OR version = $10::int)
			RETURNING `+priceColumns,
			p.ID, p.Provider, p.Model, p.From, p.InputPUSD, p.CacheReadPUSD, p.CacheWritePUSD, p.OutputPUSD, p.UpdatedBy, p.Version))
		if errors.Is(err, pgx.ErrNoRows) {
			return priceMissingOrMoved(ctx, tx, p.ID)
		}
		return priceErr(p, err)
	})
	if err != nil {
		return nil, fmt.Errorf("store: update site price %s: %w", p.ID, err)
	}
	return out, nil
}

// priceMissingOrMoved is why a write of the row id held to a version found
// nothing: ErrNotFound when it is gone, ErrConflict when it has moved on.
func priceMissingOrMoved(ctx context.Context, tx pgx.Tx, id string) error {
	var there bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM site_price WHERE id = $1)`, id).Scan(&there); err != nil {
		return err
	}
	if there {
		return fmt.Errorf("site price %s: %w", id, store.ErrConflict)
	}
	return fmt.Errorf("site price %s: %w", id, store.ErrNotFound)
}

// DeleteSitePrice destroys the row, at version when it is not 0. A delete
// that finds nothing is rolled back, so that neither the prices' version
// nor the registry's revision moves on for it.
func (s *Store) DeleteSitePrice(ctx context.Context, id string, version int) error {
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM site_price WHERE id = $1 AND ($2::int = 0 OR version = $2::int)`, id, version)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return priceMissingOrMoved(ctx, tx, id)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: delete site price %s: %w", id, err)
	}
	return nil
}

// TenantQuotas lists the tenants' quotas the site sets, by tenant.
func (s *Store) TenantQuotas(ctx context.Context) ([]store.TenantQuota, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, answers, usd_pusd, updated_by, updated_at FROM site_tenant_quota ORDER BY tenant_id COLLATE "C"`)
	if err != nil {
		return nil, fmt.Errorf("store: the tenants' quotas: %w", err)
	}
	defer rows.Close()
	var out []store.TenantQuota
	for rows.Next() {
		var q store.TenantQuota
		if err := rows.Scan(&q.TenantID, &q.Answers, &q.USDPUSD, &q.UpdatedBy, &q.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: the tenants' quotas: %w", err)
		}
		utc(&q.UpdatedAt)
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: the tenants' quotas: %w", err)
	}
	return out, nil
}

// PutTenantQuota sets q, in place of what was set for its tenant.
func (s *Store) PutTenantQuota(ctx context.Context, q store.TenantQuota) error {
	if err := store.CheckTenantQuota(q); err != nil {
		return err
	}
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO site_tenant_quota (tenant_id, answers, usd_pusd, updated_by, updated_at)
			VALUES ($1, $2, $3, $4, COALESCE($5::timestamptz, now()))
			ON CONFLICT (tenant_id) DO UPDATE
			   SET answers = EXCLUDED.answers, usd_pusd = EXCLUDED.usd_pusd, updated_by = EXCLUDED.updated_by,
			       updated_at = EXCLUDED.updated_at`,
			q.TenantID, q.Answers, q.USDPUSD, q.UpdatedBy, orNow(q.UpdatedAt))
		return err
	})
	if err != nil {
		return fmt.Errorf("store: put the quota of tenant %s: %w", q.TenantID, err)
	}
	return nil
}

// DeleteTenantQuota unsets the tenant's quota; a delete of none is rolled
// back, as DeleteSiteSetting's is.
func (s *Store) DeleteTenantQuota(ctx context.Context, tenantID string) error {
	err := s.readCommitted(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM site_tenant_quota WHERE tenant_id = $1`, tenantID)
		if err == nil && tag.RowsAffected() == 0 {
			return errNothingDeleted
		}
		return err
	})
	if errors.Is(err, errNothingDeleted) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: delete the quota of tenant %s: %w", tenantID, err)
	}
	return nil
}

// costGroups are CostReport's groups: each one's key, as SQL, and the
// columns it reads beside the sums. The transcriber's calls are no
// tenant's or agent's, and go under keys of their own
// (store.CostKeySite, store.CostKeyTranscription), naming neither.
var costGroups = map[string]struct{ key, cols string }{
	store.CostByTotal: {`''`, `NULL::timestamptz, '', '', '', '', ''`},
	store.CostByDay:   {`to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD')`, `min(date_trunc('day', at AT TIME ZONE 'UTC')) AT TIME ZONE 'UTC', '', '', '', '', ''`},
	store.CostByTenant: {`CASE WHEN kind = 'transcription' THEN '` + store.CostKeySite + `' ELSE tenant_id END`,
		`NULL::timestamptz, COALESCE(min(tenant_id) FILTER (WHERE kind <> 'transcription'), ''), '', '', '', ''`},
	store.CostByAgent: {`CASE WHEN kind = 'transcription' THEN '` + store.CostKeyTranscription + `' ELSE agent_id END`,
		`NULL::timestamptz, COALESCE(max(tenant_id) FILTER (WHERE kind <> 'transcription'), ''),
		 COALESCE(min(agent_id) FILTER (WHERE kind <> 'transcription'), ''), '', '', ''`},
	store.CostByModel:     {`key_source || '/' || provider || '/' || model`, `NULL::timestamptz, '', '', min(key_source), min(provider), min(model)`},
	store.CostByKeySource: {`key_source`, `NULL::timestamptz, '', '', min(key_source), '', ''`},
}

// CostReport sums the model calls as q says, a row per group, by key: the
// answers' and the transcriber's apart.
func (s *Store) CostReport(ctx context.Context, q store.CostQuery) ([]store.CostRow, error) {
	if err := store.CheckCostQuery(q); err != nil {
		return nil, err
	}
	g := costGroups[q.Group]
	// The sums of each kind of call: an answer's (a row of a release
	// before the kind is one too), and the transcriber's.
	sums := func(kind string) string {
		f := ` FILTER (WHERE kind ` + kind + `)`
		return `(count(*)` + f + `)::int, (count(*) FILTER (WHERE kind ` + kind + ` AND price_version = ''))::int,
		        COALESCE(sum(input_tokens)` + f + `, 0)::bigint, COALESCE(sum(cache_read_tokens)` + f + `, 0)::bigint,
		        COALESCE(sum(cache_write_tokens)` + f + `, 0)::bigint, COALESCE(sum(output_tokens)` + f + `, 0)::bigint,
		        COALESCE(sum(cost_pusd)` + f + `, 0)::bigint`
	}
	rows, err := s.pool.Query(ctx, `
		SELECT k, day, tenant_id, agent_id, key_source, provider, model, calls, unpriced, input, cache_read, cache_write, output, cost,
		       t_calls, t_unpriced, t_input, t_cache_read, t_cache_write, t_output, t_cost
		  FROM (SELECT `+g.key+` AS k, `+g.cols+`, `+sums(`<> 'transcription'`)+`, `+sums(`= 'transcription'`)+`
		          FROM llm_call
		         WHERE at >= $1 AND at < $2 AND ($3::text = '' OR key_source = $3::text)
		         GROUP BY 1) AS g (k, day, tenant_id, agent_id, key_source, provider, model, calls, unpriced, input, cache_read,
		                          cache_write, output, cost, t_calls, t_unpriced, t_input, t_cache_read, t_cache_write, t_output,
		                          t_cost)
		 WHERE $4::text = '' OR k COLLATE "C" > $4::text
		 ORDER BY k COLLATE "C"
		 LIMIT $5`, q.Since, q.Until, q.KeySource, q.After, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("store: the cost report: %w", err)
	}
	defer rows.Close()
	var out []store.CostRow
	for rows.Next() {
		var r store.CostRow
		var day *time.Time
		tr := &r.Transcription
		if err := rows.Scan(&r.Key, &day, &r.TenantID, &r.AgentID, &r.KeySource, &r.Provider, &r.Model, &r.ModelCalls, &r.Unpriced,
			&r.InputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.OutputTokens, &r.CostPUSD, &tr.Calls, &tr.Unpriced,
			&tr.InputTokens, &tr.CacheReadTokens, &tr.CacheWriteTokens, &tr.OutputTokens, &tr.CostPUSD); err != nil {
			return nil, fmt.Errorf("store: the cost report: %w", err)
		}
		if day != nil {
			r.Day = day.UTC()
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: the cost report: %w", err)
	}
	return out, nil
}
