package memstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// SitePrices lists the site's rows, by id, and when they last changed.
func (s *Store) SitePrices(_ context.Context) ([]store.SitePrice, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.SitePrice, 0, len(s.prices))
	for _, p := range s.prices {
		out = append(out, p)
	}
	slices.SortFunc(out, func(x, y store.SitePrice) int { return strings.Compare(x.ID, y.ID) })
	return out, s.pricesAt, nil
}

// priceKeyTaken reports whether a row other than id has p's provider,
// model and from. Called with the lock held.
func (s *Store) priceKeyTaken(p store.SitePrice) bool {
	for _, o := range s.prices {
		if o.ID != p.ID && o.Provider == p.Provider && o.Model == p.Model && o.From.Equal(p.From) {
			return true
		}
	}
	return false
}

// pricesChanged moves on when the site's prices last changed, and the
// registry's revision. Called with the lock held.
func (s *Store) pricesChanged() {
	s.pricesAt = store.NextPricesChange(s.pricesAt, s.now())
	s.rev++
}

// CreateSitePrice stores p at version 1.
func (s *Store) CreateSitePrice(_ context.Context, p store.SitePrice) (*store.SitePrice, error) {
	p, err := store.CheckSitePrice(p)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.prices[p.ID]; ok || s.priceKeyTaken(p) {
		return nil, fmt.Errorf("site price %s: %w", p.ID, store.ErrExists)
	}
	p.Version, p.CreatedAt = 1, s.orNow(p.CreatedAt)
	p.UpdatedAt = p.CreatedAt
	if p.UpdatedBy == "" {
		p.UpdatedBy = p.CreatedBy
	}
	s.prices[p.ID] = p
	s.pricesChanged()
	return &p, nil
}

// UpdateSitePrice writes p over the row of its id, at the version it
// names when it names one.
func (s *Store) UpdateSitePrice(_ context.Context, p store.SitePrice) (*store.SitePrice, error) {
	p, err := store.CheckSitePrice(p)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.prices[p.ID]
	switch {
	case !ok:
		return nil, fmt.Errorf("site price %s: %w", p.ID, store.ErrNotFound)
	case p.Version != 0 && old.Version != p.Version:
		return nil, fmt.Errorf("site price %s at version %d: %w", p.ID, p.Version, store.ErrConflict)
	case s.priceKeyTaken(p):
		return nil, fmt.Errorf("site price %s: another row has its provider, model and from: %w", p.ID, store.ErrExists)
	}
	p.Version, p.CreatedBy, p.CreatedAt, p.UpdatedAt = old.Version+1, old.CreatedBy, old.CreatedAt, s.clock()
	s.prices[p.ID] = p
	s.pricesChanged()
	return &p, nil
}

// DeleteSitePrice destroys the row, at version when it is not 0.
func (s *Store) DeleteSitePrice(_ context.Context, id string, version int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prices[id]
	switch {
	case !ok:
		return fmt.Errorf("site price %s: %w", id, store.ErrNotFound)
	case version != 0 && p.Version != version:
		return fmt.Errorf("site price %s at version %d: %w", id, version, store.ErrConflict)
	}
	delete(s.prices, id)
	s.pricesChanged()
	return nil
}

// TenantQuotas lists the tenants' quotas the site sets, by tenant.
func (s *Store) TenantQuotas(_ context.Context) ([]store.TenantQuota, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.TenantQuota, 0, len(s.tenants))
	for _, q := range s.tenants {
		out = append(out, copyQuota(q))
	}
	slices.SortFunc(out, func(x, y store.TenantQuota) int { return strings.Compare(x.TenantID, y.TenantID) })
	return out, nil
}

// copyQuota is q with numbers of its own.
func copyQuota(q store.TenantQuota) store.TenantQuota {
	if q.Answers != nil {
		n := *q.Answers
		q.Answers = &n
	}
	if q.USDPUSD != nil {
		n := *q.USDPUSD
		q.USDPUSD = &n
	}
	return q
}

// PutTenantQuota sets q, in place of what was set for its tenant.
func (s *Store) PutTenantQuota(_ context.Context, q store.TenantQuota) error {
	if err := store.CheckTenantQuota(q); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	q = copyQuota(q)
	q.UpdatedAt = s.orNow(q.UpdatedAt)
	s.tenants[q.TenantID] = q
	s.rev++
	return nil
}

// DeleteTenantQuota unsets the tenant's quota; one not set is nothing, and
// moves the revision on no more than pgstore's does.
func (s *Store) DeleteTenantQuota(_ context.Context, tenantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tenants[tenantID]; !ok {
		return nil
	}
	delete(s.tenants, tenantID)
	s.rev++
	return nil
}

// CostReport sums the model calls as q says, a row per group, by key.
func (s *Store) CostReport(_ context.Context, q store.CostQuery) ([]store.CostRow, error) {
	if err := store.CheckCostQuery(q); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	groups := map[string]*store.CostRow{}
	for _, c := range s.calls {
		if c.At.Before(q.Since) || !c.At.Before(q.Until) || q.KeySource != "" && c.KeySource != q.KeySource {
			continue
		}
		var r store.CostRow
		switch q.Group {
		case store.CostByDay:
			r.Day = store.UTCDay(c.At)
			r.Key = r.Day.Format(time.DateOnly)
		case store.CostByTenant:
			r.Key, r.TenantID = c.TenantID, c.TenantID
		case store.CostByAgent:
			r.Key, r.AgentID, r.TenantID = c.AgentID, c.AgentID, c.TenantID
		case store.CostByModel:
			r.Key, r.KeySource, r.Provider, r.Model = c.KeySource+"/"+c.Provider+"/"+c.Model, c.KeySource, c.Provider, c.Model
		case store.CostByKeySource:
			r.Key, r.KeySource = c.KeySource, c.KeySource
		}
		g := groups[r.Key]
		if g == nil {
			g = &r
			groups[r.Key] = g
		}
		if c.TenantID > g.TenantID && q.Group == store.CostByAgent {
			g.TenantID = c.TenantID
		}
		g.ModelCalls++
		if c.PriceVersion == "" {
			g.Unpriced++
		}
		g.InputTokens += c.Input
		g.CacheReadTokens += c.CacheRead
		g.CacheWriteTokens += c.CacheWrite
		g.OutputTokens += c.Output
		g.CostPUSD += c.CostPUSD
	}
	out := make([]store.CostRow, 0, len(groups))
	for k, g := range groups {
		if q.After == "" || k > q.After {
			out = append(out, *g)
		}
	}
	slices.SortFunc(out, func(x, y store.CostRow) int { return strings.Compare(x.Key, y.Key) })
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}
