package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// GET /admin/costs: what the ledger recorded, in dollars, for the
// runtime's administrators alone (D4): the model calls of a span of UTC
// days, summed as a whole and by one of day, tenant (its owner's name
// where it is a person), agent, model (with its key source), or key
// source (the school's key or the owners' own), on one key source or both,
// a page of groups at a time. Each sum is lines by kind of cost: the
// answers' model calls, and the transcriber's, a line of their own kind
// (transcription), so that a front end shows the lines it knows and a
// total of them all. The transcriber's calls are of no agent and no
// tenant: by agent, they are the group transcription, and by tenant the
// group site, each with a null id; by key source, the school's. Counts,
// tokens and costs, never what anyone wrote. A cost is what the price table in force priced the call
// at when it was made, and stays so whatever the table becomes; a call no
// price held counts as unpriced, at nothing.

// The kinds of cost, each a line: the answers' model calls, and the
// transcriber's.
const (
	CostKindModelCalls    = "model_calls"
	CostKindTranscription = "transcription"
)

// Bounds of GET /admin/costs: the span, in days, the page.
const (
	defaultCostDays = 30
	maxCostDays     = 366
	defaultCostPage = 100
	maxCostPage     = 500
	maxCostCursor   = 300
)

// CostReport is GET /admin/costs' answer: the span, its first and last
// UTC days (YYYY-MM-DD, both counted); how it is grouped, and the key
// source it is held to (null for both); the total of the whole span; a
// page of groups, by key; and the cursor of the next page (after), null
// after the last. A day, a tenant or an agent with nothing recorded has no
// group.
type CostReport struct {
	Since     string      `json:"since"`
	Until     string      `json:"until"`
	Group     string      `json:"group"`
	KeySource *string     `json:"key_source"`
	Total     CostSum     `json:"total"`
	Rows      []CostGroup `json:"rows"`
	Next      *string     `json:"next"`
}

// CostSum is what a group, or the whole, cost: in dollars with six
// places, every line's, and its lines, one per kind of cost.
type CostSum struct {
	CostUSD string     `json:"cost_usd"`
	Lines   []CostLine `json:"lines"`
}

// CostLine is one kind of cost: model_calls, the calls, those no price
// held, their tokens (uncached input, cache reads, cache writes and
// output) and their cost. A kind of its own may have other measures than
// tokens, which are then null.
type CostLine struct {
	Kind          string      `json:"kind"`
	Calls         int         `json:"calls"`
	UnpricedCalls int         `json:"unpriced_calls"`
	Tokens        *CostTokens `json:"tokens"`
	CostUSD       string      `json:"cost_usd"`
}

// CostTokens are a line's tokens: input counts them all, the cache's too.
type CostTokens struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
}

// CostGroup is one group: its key (the cursor a page ends at), and what
// it is by, null where it is by something else: the day; the tenant, and,
// where it is a person's, their actor id and name as the runtime last saw
// it (by tenant, and of an agent by agent); the agent and its name where
// it is still configured; the key source (by model and by key source);
// the provider and model (by model), with the ids of the plan's offers
// now of that provider and model, on the school's key.
type CostGroup struct {
	Key          string   `json:"key"`
	Day          *string  `json:"day"`
	TenantID     *string  `json:"tenant_id"`
	OwnerActorID *string  `json:"owner_actor_id"`
	DisplayName  *string  `json:"display_name"`
	AgentID      *string  `json:"agent_id"`
	AgentName    *string  `json:"agent_name"`
	KeySource    *string  `json:"key_source"`
	Provider     *string  `json:"provider"`
	Model        *string  `json:"model"`
	Offers       []string `json:"offers"`
	CostSum
}

// costSum is a row of the ledger's sums as lines: the model calls' (but
// in a group of the transcriber's calls alone), and the transcriber's
// where it has any.
func costSum(r store.CostRow) CostSum {
	t := r.Transcription
	sum := CostSum{CostUSD: costUSD(r.CostPUSD + t.CostPUSD), Lines: []CostLine{}}
	if r.ModelCalls > 0 || t.Calls == 0 {
		sum.Lines = append(sum.Lines, CostLine{Kind: CostKindModelCalls, Calls: r.ModelCalls, UnpricedCalls: r.Unpriced, CostUSD: costUSD(r.CostPUSD),
			Tokens: &CostTokens{Input: r.InputTokens, CacheRead: r.CacheReadTokens, CacheWrite: r.CacheWriteTokens, Output: r.OutputTokens}})
	}
	if t.Calls > 0 {
		sum.Lines = append(sum.Lines, CostLine{Kind: CostKindTranscription, Calls: t.Calls, UnpricedCalls: t.Unpriced, CostUSD: costUSD(t.CostPUSD),
			Tokens: &CostTokens{Input: t.InputTokens, CacheRead: t.CacheReadTokens, CacheWrite: t.CacheWriteTokens, Output: t.OutputTokens}})
	}
	return sum
}

// costQuery reads GET /admin/costs' parameters, answering a refusal:
// since and until, UTC days, until the last counted (today, and 30 days
// before it, unless given), at most 366 days; group, one of
// store.CostBy* (day unless given); key_source, school or own (both
// unless given); limit, 1 to 500 (100 unless given); after, the key a page
// ends at.
func (s *Server) costQuery(w http.ResponseWriter, r *http.Request) (store.CostQuery, bool) {
	var q store.CostQuery
	p, ok := queryOf(w, r, "since", "until", "group", "key_source", "limit", "after")
	if !ok {
		return q, false
	}
	refuse := func(field, msg string) (store.CostQuery, bool) {
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonInvalidField, field, msg))
		return q, false
	}
	day := func(field string, def time.Time) (time.Time, bool) {
		v, given := p[field]
		if !given {
			return def, true
		}
		t, err := time.Parse(time.DateOnly, v)
		return t, err == nil
	}
	until, ok := day("until", store.UTCDay(s.o.Now()))
	if !ok {
		return refuse("until", "until is the last day counted, YYYY-MM-DD, in UTC")
	}
	since, ok := day("since", until.AddDate(0, 0, 1-defaultCostDays))
	if !ok {
		return refuse("since", "since is the first day counted, YYYY-MM-DD, in UTC")
	}
	switch days := int(until.Sub(since).Hours()/24) + 1; {
	case days < 1:
		return refuse("since", "since is on or before until")
	case days > maxCostDays:
		return refuse("since", "a report spans at most 366 days")
	}
	q.Since, q.Until = since, until.AddDate(0, 0, 1)
	q.Group = store.CostByDay
	if g, given := p["group"]; given {
		if !slices.Contains([]string{store.CostByTotal, store.CostByDay, store.CostByTenant, store.CostByAgent, store.CostByModel,
			store.CostByKeySource}, g) {
			return refuse("group", "group is total, day, tenant, agent, model or key_source")
		}
		q.Group = g
	}
	if ks, given := p["key_source"]; given {
		if ks != config.KeySchool && ks != config.KeyOwn {
			return refuse("key_source", "key_source is school or own")
		}
		q.KeySource = ks
	}
	q.Limit = defaultCostPage
	if v, given := p["limit"]; given {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxCostPage {
			return refuse("limit", "limit is from 1 to 500")
		}
		q.Limit = n
	}
	if q.After = p["after"]; len(q.After) > maxCostCursor {
		return refuse("after", "after is the next of a page before")
	}
	return q, true
}

// costs is GET /admin/costs.
func (s *Server) costs(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	q, ok := s.costQuery(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*storeTimeout)
	defer cancel()
	whole := q
	whole.Group, whole.After, whole.Limit = store.CostByTotal, "", 1
	total, err := s.o.Store.CostReport(ctx, whole)
	if err != nil {
		s.storeUnavailable(w, "the cost report", err)
		return
	}
	page := q
	page.Limit++
	rows, err := s.o.Store.CostReport(ctx, page)
	if err != nil {
		s.storeUnavailable(w, "the cost report", err)
		return
	}
	out := CostReport{Since: utcDay(q.Since), Until: utcDay(q.Until.AddDate(0, 0, -1)), Group: q.Group, KeySource: strPtr(q.KeySource),
		Total: costSum(store.CostRow{}), Rows: []CostGroup{}}
	if len(total) > 0 {
		out.Total = costSum(total[0])
	}
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
		next := rows[q.Limit-1].Key
		out.Next = &next
	}
	names, err := s.costNames(ctx, q.Group)
	if err != nil {
		s.storeUnavailable(w, "the hosted agents", err)
		return
	}
	owners := map[string][2]*string{}
	for _, row := range rows {
		g := CostGroup{Key: row.Key, CostSum: costSum(row)}
		switch q.Group {
		case store.CostByDay:
			g.Day = strPtr(utcDay(row.Day))
		case store.CostByAgent:
			g.AgentID, g.AgentName = strPtr(row.AgentID), names.agents[row.AgentID]
			g.TenantID = strPtr(row.TenantID)
		case store.CostByTenant:
			g.TenantID = strPtr(row.TenantID)
		case store.CostByModel:
			g.KeySource, g.Provider, g.Model = strPtr(row.KeySource), strPtr(row.Provider), strPtr(row.Model)
			if row.KeySource == config.KeySchool {
				g.Offers = nonNil(names.offers[row.Provider+"\x00"+row.Model])
			}
		case store.CostByKeySource:
			g.KeySource = strPtr(row.KeySource)
		}
		if g.TenantID != nil {
			o, seen := owners[row.TenantID]
			if !seen {
				if o[0], o[1], err = s.ownerOf(ctx, row.TenantID); err != nil {
					s.storeUnavailable(w, "a person", err)
					return
				}
				owners[row.TenantID] = o
			}
			g.OwnerActorID, g.DisplayName = o[0], o[1]
		}
		out.Rows = append(out.Rows, g)
	}
	writeJSON(w, http.StatusOK, out)
}

// costNames are what a report by group names beside the ledger's ids: the
// agents' names, by agent, and the plan's offers, by provider and model.
type costNames struct {
	agents map[string]*string
	offers map[string][]string
}

// costNames reads the names a report by group needs: the agents' by
// agent (runtime.yaml's and the hosted), the offers' by model (the
// plan's, runtime.yaml's and the site's, on or off).
func (s *Server) costNames(ctx context.Context, group string) (costNames, error) {
	n := costNames{agents: map[string]*string{}, offers: map[string][]string{}}
	switch group {
	case store.CostByAgent:
		hosted, err := s.o.Store.HostedAgents(ctx)
		if err != nil {
			return n, err
		}
		for _, a := range hosted {
			n.agents[a.ID] = strPtr(a.DisplayName)
		}
		for _, a := range s.yaml().Agents {
			n.agents[a.ID] = strPtr(a.DisplayName)
		}
	case store.CostByModel:
		offers, err := s.o.Store.SchoolOffers(ctx)
		if err != nil {
			return n, err
		}
		add := func(provider, model, id string) {
			k := provider + "\x00" + model
			if !slices.Contains(n.offers[k], id) {
				n.offers[k] = append(n.offers[k], id)
			}
		}
		for _, o := range s.yaml().Runtime.School.Offers {
			m := o.AsModel()
			add(m.EffectiveProvider(), m.Model, o.ID)
		}
		for _, o := range offers {
			add(o.Provider, o.Model, o.ID)
		}
	}
	return n, nil
}
