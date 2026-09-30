package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// GET, PUT and DELETE /admin/agent-budgets: the daily budgets a hosted
// agent has by default, per agent and per asker (budgets.per_agent_day and
// budgets.per_asker_day), in answers and in dollars, on whichever key it
// answers. runtime.defaults' are the operator's; the site's stand in place
// of them for the hosted agents, which are built on the site's settings
// each time the registry is read. runtime.yaml's agents keep the budgets
// runtime.yaml gives them. The budgets of one answer (turns, tokens, time)
// are not money, and stay runtime.yaml's.

// AgentBudgets is the routes' answer: the hosted agents' daily budgets in
// force, runtime.yaml's (defaults), and whether the site sets them, when
// and by whom.
type AgentBudgets struct {
	PerAgentDay DailyQuota     `json:"per_agent_day"`
	PerAskerDay DailyQuota     `json:"per_asker_day"`
	Defaults    BudgetDefaults `json:"defaults"`
	Set         bool           `json:"set"`
	UpdatedAt   *time.Time     `json:"updated_at"`
	UpdatedBy   *string        `json:"updated_by"`
}

// BudgetDefaults are runtime.defaults' daily budgets.
type BudgetDefaults struct {
	PerAgentDay DailyQuota `json:"per_agent_day"`
	PerAskerDay DailyQuota `json:"per_asker_day"`
}

// agentBudgets are the budgets as the routes answer them.
func (s *Server) agentBudgets(ctx context.Context) (*AgentBudgets, error) {
	eff, err := s.effective(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.siteSetting(ctx, store.SettingAgentBudgets)
	if err != nil {
		return nil, err
	}
	agent, asker := eff.Runtime.DefaultBudgets()
	yagent, yasker := s.yaml().Runtime.DefaultBudgets()
	out := &AgentBudgets{PerAgentDay: dailyQuota(agent), PerAskerDay: dailyQuota(asker), Set: eff.Runtime.Site.Budgets != nil,
		Defaults: BudgetDefaults{PerAgentDay: dailyQuota(yagent), PerAskerDay: dailyQuota(yasker)}}
	if row != nil && out.Set {
		at := row.UpdatedAt.UTC()
		out.UpdatedAt, out.UpdatedBy = &at, strPtr(row.UpdatedBy)
	}
	return out, nil
}

// writeBudgets answers the budgets, or that the store cannot be read.
func (s *Server) writeBudgets(ctx context.Context, w http.ResponseWriter) {
	b, err := s.agentBudgets(ctx)
	if err != nil {
		s.storeUnavailable(w, "the agents' budgets", err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) getAgentBudgets(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*storeTimeout)
	defer cancel()
	s.writeBudgets(ctx, w)
}

// budgetsRequest is PUT /admin/agent-budgets' body: both budgets, each
// with answers and usd, null for none.
type budgetsRequest struct {
	PerAgentDay json.RawMessage `json:"per_agent_day"`
	PerAskerDay json.RawMessage `json:"per_asker_day"`
}

// putAgentBudgets is PUT /admin/agent-budgets: the site's budgets, in
// place of runtime.defaults'. A budget in dollars no price would hold for
// a hosted agent's model is refused.
func (s *Server) putAgentBudgets(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("site_setting", store.SettingAgentBudgets)
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	var req budgetsRequest
	if !readBody(w, r, &req) {
		return
	}
	agent, _, e := readQuota(req.PerAgentDay, "/per_agent_day")
	var asker config.SiteQuota
	if e == nil {
		asker, _, e = readQuota(req.PerAskerDay, "/per_asker_day")
	}
	if e != nil {
		WriteError(w, *e)
		return
	}
	b := config.SiteBudgets{PerAgentDay: agent, PerAskerDay: asker}
	au.detail["per_agent_day"] = map[string]any{"answers": agent.Answers, "usd": usdText(agent.USD)}
	au.detail["per_asker_day"] = map[string]any{"answers": asker.Answers, "usd": usdText(asker.USD)}
	value, err := json.Marshal(b)
	if err != nil {
		WriteError(w, Error{Code: CodeInternal, Reason: ReasonInternal, Message: "the budgets could not be written"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	if agent.USD != nil || asker.USD != nil {
		cur, err := registry.ReadSite(ctx, s.o.Store)
		if err != nil {
			s.storeUnavailable(w, "the site's settings", err)
			return
		}
		next := cur
		next.Budgets = &b
		field := "/per_agent_day/usd"
		if agent.USD == nil {
			field = "/per_asker_day/usd"
		}
		if !s.checkUnpriced(ctx, w, cur, next, field) {
			return
		}
	}
	if err := s.o.Store.PutSiteSetting(ctx, store.SiteSetting{Name: store.SettingAgentBudgets, Value: value, UpdatedBy: c.ActorID,
		UpdatedAt: s.o.Now().UTC()}); err != nil {
		s.storeUnavailable(w, "the agents' budgets written", err)
		return
	}
	s.writeBudgets(ctx, w)
}

// resetAgentBudgets is DELETE /admin/agent-budgets: runtime.defaults'
// budgets again for the hosted agents. With none of the site's set,
// nothing is written or audited; where runtime.yaml's are in dollars no
// price would hold, it is refused.
func (s *Server) resetAgentBudgets(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	au.target("site_setting", store.SettingAgentBudgets)
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	set, err := s.siteSetting(ctx, store.SettingAgentBudgets)
	if err != nil {
		s.storeUnavailable(w, "the site's settings", err)
		return
	}
	if set == nil {
		au.skip = true
		s.writeBudgets(ctx, w)
		return
	}
	if agent, asker := s.yaml().Runtime.DefaultBudgets(); agent.USD != nil || asker.USD != nil {
		cur, err := registry.ReadSite(ctx, s.o.Store)
		if err != nil {
			s.storeUnavailable(w, "the site's settings", err)
			return
		}
		next := cur
		next.Budgets = nil
		if !s.checkUnpriced(ctx, w, cur, next, "") {
			return
		}
	}
	if err := s.o.Store.DeleteSiteSetting(ctx, store.SettingAgentBudgets); err != nil {
		s.storeUnavailable(w, "the agents' budgets unset", err)
		return
	}
	s.writeBudgets(ctx, w)
}
