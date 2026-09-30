package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// GET /admin/school-plan/usage: today's use of the school's plan (D8), per
// owner, for the runtime's administrators alone (D4): what the store's
// reports sum of the school's key since the start of the UTC day, and the
// plan's quotas. Counts and costs, never what anyone wrote.

// SchoolPlanUsage is the route's answer: since when, the plan's quotas
// (per_day null when the school sets no ceiling), the total, and a row per
// tenant that used the school's key today, by tenant id.
type SchoolPlanUsage struct {
	Since  time.Time        `json:"since"`
	Limits SchoolPlanLimits `json:"limits"`
	Total  PlanUse          `json:"total"`
	Owners []OwnerPlanUse   `json:"owners"`
}

// SchoolPlanLimits are the plan's quotas a UTC day: in answers (per_day
// null for no ceiling), and in dollars, each a decimal with six places, or
// null for none.
type SchoolPlanLimits struct {
	PerOwnerDay    int     `json:"per_owner_day"`
	PerAskerDay    int     `json:"per_asker_day"`
	PerDay         *int    `json:"per_day"`
	PerOwnerDayUSD *string `json:"per_owner_day_usd"`
	PerAskerDayUSD *string `json:"per_asker_day_usd"`
	PerDayUSD      *string `json:"per_day_usd"`
}

// PlanUse is billable answers and the cost of model calls on the school's
// key, in dollars with six places.
type PlanUse struct {
	Answers    int    `json:"answers"`
	ModelCalls int    `json:"model_calls"`
	CostUSD    string `json:"cost_usd"`
}

// OwnerPlanUse is one tenant's use: a hosted agent's owner's (ten_ and
// their actor id), whose actor id, and name as the runtime last saw them,
// are given; or a tenant of the operator's YAML agents on the school's
// key, with neither.
type OwnerPlanUse struct {
	TenantID     string  `json:"tenant_id"`
	OwnerActorID *string `json:"owner_actor_id"`
	DisplayName  *string `json:"display_name"`
	PlanUse
}

// errNotAdmin is a route of the runtime's administrators asked by someone
// else.
var errNotAdmin = Error{Code: CodeForbidden, Reason: ReasonNotAdmin, Message: "only the runtime's administrators may read or change this"}

func (s *Server) schoolPlanUsage(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	since := store.UTCDay(s.o.Now())
	rows, err := s.o.Store.TenantUsage(ctx, config.KeySchool, since, since.AddDate(0, 0, 1))
	if err != nil {
		s.storeUnavailable(w, "the school's plan's use", err)
		return
	}
	sc, err := s.plan(ctx)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	out := SchoolPlanUsage{Since: since, Owners: []OwnerPlanUse{}, Limits: limitsOf(sc)}
	var total int64
	for _, u := range rows {
		o := OwnerPlanUse{TenantID: u.TenantID, PlanUse: PlanUse{Answers: u.Answers, ModelCalls: u.ModelCalls, CostUSD: costUSD(u.CostPUSD)}}
		if o.OwnerActorID, o.DisplayName, err = s.ownerOf(ctx, u.TenantID); err != nil {
			s.storeUnavailable(w, "a person", err)
			return
		}
		out.Owners = append(out.Owners, o)
		out.Total.Answers += u.Answers
		out.Total.ModelCalls += u.ModelCalls
		total += u.CostPUSD
	}
	out.Total.CostUSD = costUSD(total)
	writeJSON(w, http.StatusOK, out)
}

// ownerOf is the person a tenant is, when it is a hosted agents' owner's
// (ten_ and their actor id): their actor id, and their name as the
// runtime last saw them (nil when it has not); both nil for any other
// tenant.
func (s *Server) ownerOf(ctx context.Context, tenantID string) (actorID, name *string, err error) {
	actor, ok := strings.CutPrefix(tenantID, "ten_")
	if !ok || uuid.Validate(actor) != nil {
		return nil, nil, nil
	}
	switch p, err := s.o.Store.Person(ctx, actor); {
	case err == nil:
		n := p.DisplayName
		return &actor, &n, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, nil, err
	}
	return &actor, nil, nil
}
