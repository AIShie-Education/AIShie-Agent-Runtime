package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// GET /admin/tenants, and GET, PUT and DELETE /admin/tenants/{tenant_id}:
// the tenants' daily quotas on the school's key, which hold every agent
// of the tenant's on it across the UTC day, in answers and in dollars.
// runtime.yaml's (runtime.tenants, source config) are the operator's; the
// site's (source site) stand in place of them, a tenant's whole quota at
// once, until DELETE takes runtime.yaml's again. A hosted agents' owner is
// the tenant ten_<their actor id>, whose quota holds them beside the
// plan's per owner.

// TenantSourceNone is a tenant with no quota of its own: one of an agent,
// that neither runtime.yaml nor the site gives one.
const TenantSourceNone = "none"

// TenantList is GET /admin/tenants' answer: a page of the tenants the
// runtime knows (runtime.yaml's, the site's, and those of its agents), by
// id, and the cursor of the next page, null after the last.
type TenantList struct {
	Tenants []TenantQuota `json:"tenants"`
	Next    *string       `json:"next"`
}

// TenantQuota is a tenant's daily quota: the tenant, and, when it is a
// hosted agents' owner, their actor id and name as the runtime last saw
// it; where the quota in force comes from (site, config or none), the
// quota in force (per_day), and runtime.yaml's (config_per_day, null when
// runtime.tenants has none); how many of the runtime's agents are the
// tenant's, runtime.yaml's and hosted; and when and by whom the site's
// was set.
type TenantQuota struct {
	TenantID     string      `json:"tenant_id"`
	OwnerActorID *string     `json:"owner_actor_id"`
	DisplayName  *string     `json:"display_name"`
	Source       string      `json:"source"`
	PerDay       DailyQuota  `json:"per_day"`
	ConfigPerDay *DailyQuota `json:"config_per_day"`
	Agents       int         `json:"agents"`
	UpdatedAt    *time.Time  `json:"updated_at"`
	UpdatedBy    *string     `json:"updated_by"`
}

// Bounds of GET /admin/tenants' page.
const (
	defaultTenantPage = 100
	maxTenantPage     = 500
)

// tenantInfo is what the runtime knows of a tenant: the site's quota,
// runtime.yaml's, and how many agents are its.
type tenantInfo struct {
	site   *store.TenantQuota
	config *config.Tenant
	agents int
}

// tenants are the tenants the runtime knows, by id.
func (s *Server) tenants(ctx context.Context) (map[string]*tenantInfo, error) {
	quotas, err := s.o.Store.TenantQuotas(ctx)
	if err != nil {
		return nil, err
	}
	hosted, err := s.o.Store.HostedAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]*tenantInfo{}
	of := func(id string) *tenantInfo {
		if out[id] == nil {
			out[id] = &tenantInfo{}
		}
		return out[id]
	}
	yaml := s.yaml()
	for id, t := range yaml.Runtime.Tenants {
		of(id).config = &t
	}
	for i := range quotas {
		of(quotas[i].TenantID).site = &quotas[i]
	}
	for _, a := range yaml.Agents {
		if a.TenantID != "" {
			of(a.TenantID).agents++
		}
	}
	for _, a := range hosted {
		if a.TenantID != "" {
			of(a.TenantID).agents++
		}
	}
	return out, nil
}

// tenantView is the tenant id as the administrators read it.
func (s *Server) tenantView(ctx context.Context, id string, info *tenantInfo) (TenantQuota, error) {
	v := TenantQuota{TenantID: id, Source: TenantSourceNone}
	if info == nil {
		info = &tenantInfo{}
	}
	var err error
	if v.OwnerActorID, v.DisplayName, err = s.ownerOf(ctx, id); err != nil {
		return v, err
	}
	v.Agents = info.agents
	if info.config != nil {
		q := dailyQuota(info.config.PerDay)
		v.ConfigPerDay, v.PerDay, v.Source = &q, q, SourceConfig
	}
	if q := info.site; q != nil {
		v.Source, v.PerDay = SourceSite, DailyQuota{Answers: clonePtr(q.Answers)}
		if q.USDPUSD != nil {
			usd := costUSD(*q.USDPUSD)
			v.PerDay.USD = &usd
		}
		at := q.UpdatedAt.UTC()
		v.UpdatedAt, v.UpdatedBy = &at, strPtr(q.UpdatedBy)
	}
	return v, nil
}

// listTenants is GET /admin/tenants?after=&limit=: limit (1 to 500, 100
// unless given) tenants whose id sorts after after.
func (s *Server) listTenants(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	q, ok := queryOf(w, r, "after", "limit")
	if !ok {
		return
	}
	limit := defaultTenantPage
	if v, given := q["limit"]; given {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxTenantPage {
			WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "limit", "limit is from 1 to 500"))
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	all, err := s.tenants(ctx)
	if err != nil {
		s.storeUnavailable(w, "the tenants", err)
		return
	}
	ids := slices.Sorted(maps.Keys(all))
	ids = slices.DeleteFunc(ids, func(id string) bool { return id <= q["after"] })
	out := TenantList{Tenants: []TenantQuota{}}
	if len(ids) > limit {
		ids = ids[:limit]
		next := ids[limit-1]
		out.Next = &next
	}
	for _, id := range ids {
		v, err := s.tenantView(ctx, id, all[id])
		if err != nil {
			s.storeUnavailable(w, "a person", err)
			return
		}
		out.Tenants = append(out.Tenants, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// tenantID is the path's tenant, answering when it is not one: letters,
// digits, '_' and '-', at most 64.
func tenantID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("tenant_id")
	if store.CheckTenantQuota(store.TenantQuota{TenantID: id}) != nil {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, "tenant_id", "a tenant is letters, digits, '_' and '-', at most 64"))
		return "", false
	}
	return id, true
}

// writeTenant answers the tenant id as it now is.
func (s *Server) writeTenant(ctx context.Context, w http.ResponseWriter, id string) {
	all, err := s.tenants(ctx)
	if err != nil {
		s.storeUnavailable(w, "the tenants", err)
		return
	}
	v, err := s.tenantView(ctx, id, all[id])
	if err != nil {
		s.storeUnavailable(w, "a person", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// getTenant is GET /admin/tenants/{tenant_id}: the tenant as GET
// /admin/tenants lists it, any tenant of the right shape, with no quota
// (none) when the runtime knows nothing of it.
func (s *Server) getTenant(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	id, ok := tenantID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	s.writeTenant(ctx, w, id)
}

// tenantRequest is PUT /admin/tenants/{tenant_id}'s body: the tenant's
// quota a UTC day, answers and usd, each null for none, but not both.
type tenantRequest struct {
	PerDay json.RawMessage `json:"per_day"`
}

// putTenant is PUT /admin/tenants/{tenant_id}: the site's quota for the
// tenant, in place of runtime.yaml's. A quota in dollars no price would
// hold is refused.
func (s *Server) putTenant(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("tenant", r.PathValue("tenant_id"))
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	id, ok := tenantID(w, r)
	if !ok {
		return
	}
	var req tenantRequest
	if !readBody(w, r, &req) {
		return
	}
	q, pusd, e := readQuota(req.PerDay, "/per_day")
	if e == nil && q.Answers == nil && q.USD == nil {
		e = fieldError(CodeInvalidArgument, ReasonInvalidField, "/per_day",
			"a tenant's quota holds answers, dollars or both: DELETE takes runtime.yaml's again")
	}
	if e != nil {
		WriteError(w, *e)
		return
	}
	au.detail["answers"], au.detail["usd"] = q.Answers, usdText(q.USD)
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	if q.USD != nil {
		cur, err := registry.ReadSite(ctx, s.o.Store)
		if err != nil {
			s.storeUnavailable(w, "the site's settings", err)
			return
		}
		next := cur
		next.Tenants = maps.Clone(cur.Tenants)
		if next.Tenants == nil {
			next.Tenants = map[string]config.SiteQuota{}
		}
		next.Tenants[id] = q
		if !s.checkUnpriced(ctx, w, cur, next, "/per_day/usd") {
			return
		}
	}
	if err := s.o.Store.PutTenantQuota(ctx, store.TenantQuota{TenantID: id, Answers: q.Answers, USDPUSD: pusd, UpdatedBy: c.ActorID,
		UpdatedAt: s.o.Now().UTC()}); err != nil {
		s.storeUnavailable(w, "a tenant's quota written", err)
		return
	}
	s.writeTenant(ctx, w, id)
}

// resetTenant is DELETE /admin/tenants/{tenant_id}: runtime.yaml's quota
// for the tenant again, or none. With none of the site's set, nothing is
// written or audited. Where runtime.yaml's is in dollars no price would
// hold, it is refused.
func (s *Server) resetTenant(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("tenant", r.PathValue("tenant_id"))
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	id, ok := tenantID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	cur, err := registry.ReadSite(ctx, s.o.Store)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	if _, set := cur.Tenants[id]; !set {
		au.skip = true
		s.writeTenant(ctx, w, id)
		return
	}
	if t, ok := s.yaml().Runtime.Tenants[id]; ok && t.PerDay.USD != nil {
		next := cur
		next.Tenants = maps.Clone(cur.Tenants)
		delete(next.Tenants, id)
		if !s.checkUnpriced(ctx, w, cur, next, "") {
			return
		}
	}
	if err := s.o.Store.DeleteTenantQuota(ctx, id); err != nil {
		s.storeUnavailable(w, "a tenant's quota unset", err)
		return
	}
	s.writeTenant(ctx, w, id)
}
