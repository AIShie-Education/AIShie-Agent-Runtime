package pgstore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Usage is agentID's use in [since, until), a row per UTC day and course:
// the answers from the answer table, the calls, tokens and cost from
// llm_call, as Spend counts them.
func (s *Store) Usage(ctx context.Context, agentID string, since, until time.Time) ([]store.UsageRow, error) {
	if err := store.CheckSpan(agentID, since, until); err != nil {
		return nil, err
	}
	type key struct {
		day    time.Time
		course string
	}
	byKey := map[key]*store.UsageRow{}
	row := func(day time.Time, course string) *store.UsageRow {
		k := key{day.UTC(), course}
		if byKey[k] == nil {
			byKey[k] = &store.UsageRow{Day: k.day, CourseID: course, Outcomes: map[string]int{}}
		}
		return byKey[k]
	}
	answers, err := s.pool.Query(ctx, `
		SELECT date_trunc('day', at AT TIME ZONE 'UTC'), course_id, outcome, count(*), count(*) FILTER (WHERE billable),
		       sum(writes)::bigint, sum(writes_executed)::bigint, sum(writes_proposed)::bigint,
		       sum(writes_denied)::bigint, sum(writes_failed)::bigint
		  FROM answer
		 WHERE agent_id = $1 AND at >= $2 AND at < $3
		 GROUP BY 1, 2, 3`, agentID, since, until)
	if err != nil {
		return nil, fmt.Errorf("store: usage of %s: %w", agentID, err)
	}
	defer answers.Close()
	for answers.Next() {
		var day time.Time
		var course, outcome string
		var n, billable int
		var w store.WriteCounts
		if err := answers.Scan(&day, &course, &outcome, &n, &billable, &w.Sent, &w.Executed, &w.Proposed, &w.Denied, &w.Failed); err != nil {
			return nil, fmt.Errorf("store: usage of %s: %w", agentID, err)
		}
		r := row(day, course)
		r.Outcomes[outcome] += n
		r.Answers += billable
		r.Writes.Add(w)
	}
	if err := answers.Err(); err != nil {
		return nil, fmt.Errorf("store: usage of %s: %w", agentID, err)
	}
	calls, err := s.pool.Query(ctx, `
		SELECT date_trunc('day', at AT TIME ZONE 'UTC'), course_id, count(*),
		       sum(input_tokens)::bigint, sum(cache_read_tokens)::bigint, sum(cache_write_tokens)::bigint,
		       sum(output_tokens)::bigint, sum(reasoning_tokens)::bigint, sum(cost_pusd)::bigint
		  FROM llm_call
		 WHERE agent_id = $1 AND at >= $2 AND at < $3
		 GROUP BY 1, 2`, agentID, since, until)
	if err != nil {
		return nil, fmt.Errorf("store: usage of %s: %w", agentID, err)
	}
	defer calls.Close()
	for calls.Next() {
		var day time.Time
		var course string
		var c store.UsageRow
		if err := calls.Scan(&day, &course, &c.ModelCalls, &c.InputTokens, &c.CacheReadTokens, &c.CacheWriteTokens,
			&c.OutputTokens, &c.ReasoningTokens, &c.CostPUSD); err != nil {
			return nil, fmt.Errorf("store: usage of %s: %w", agentID, err)
		}
		r := row(day, course)
		r.ModelCalls, r.InputTokens, r.CacheReadTokens, r.CacheWriteTokens = c.ModelCalls, c.InputTokens, c.CacheReadTokens, c.CacheWriteTokens
		r.OutputTokens, r.ReasoningTokens, r.CostPUSD = c.OutputTokens, c.ReasoningTokens, c.CostPUSD
	}
	if err := calls.Err(); err != nil {
		return nil, fmt.Errorf("store: usage of %s: %w", agentID, err)
	}
	out := make([]store.UsageRow, 0, len(byKey))
	for _, r := range byKey {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b store.UsageRow) int {
		if c := a.Day.Compare(b.Day); c != 0 {
			return c
		}
		return strings.Compare(a.CourseID, b.CourseID)
	})
	return out, nil
}

// AskerUsage is agentID's use in one course in [since, until), a row per
// asker, sorted bytewise by member id as memstore sorts them.
func (s *Store) AskerUsage(ctx context.Context, agentID, courseID string, since, until time.Time) ([]store.AskerUsage, error) {
	if err := store.CheckSpan(agentID, since, until); err != nil {
		return nil, err
	}
	byAsker := map[string]*store.AskerUsage{}
	row := func(opener string) *store.AskerUsage {
		if byAsker[opener] == nil {
			byAsker[opener] = &store.AskerUsage{OpenerMemberID: opener, Outcomes: map[string]int{}}
		}
		return byAsker[opener]
	}
	answers, err := s.pool.Query(ctx, `
		SELECT opener_member_id, outcome, count(*), count(*) FILTER (WHERE billable)
		  FROM answer
		 WHERE agent_id = $1 AND course_id = $2 AND at >= $3 AND at < $4
		 GROUP BY 1, 2`, agentID, courseID, since, until)
	if err != nil {
		return nil, fmt.Errorf("store: askers of %s in %s: %w", agentID, courseID, err)
	}
	defer answers.Close()
	for answers.Next() {
		var opener, outcome string
		var n, billable int
		if err := answers.Scan(&opener, &outcome, &n, &billable); err != nil {
			return nil, fmt.Errorf("store: askers of %s in %s: %w", agentID, courseID, err)
		}
		r := row(opener)
		r.Outcomes[outcome] += n
		r.Answers += billable
	}
	if err := answers.Err(); err != nil {
		return nil, fmt.Errorf("store: askers of %s in %s: %w", agentID, courseID, err)
	}
	calls, err := s.pool.Query(ctx, `
		SELECT opener_member_id, count(*), sum(input_tokens)::bigint, sum(output_tokens)::bigint, sum(cost_pusd)::bigint
		  FROM llm_call
		 WHERE agent_id = $1 AND course_id = $2 AND at >= $3 AND at < $4
		 GROUP BY 1`, agentID, courseID, since, until)
	if err != nil {
		return nil, fmt.Errorf("store: askers of %s in %s: %w", agentID, courseID, err)
	}
	defer calls.Close()
	for calls.Next() {
		var opener string
		var c store.AskerUsage
		if err := calls.Scan(&opener, &c.ModelCalls, &c.InputTokens, &c.OutputTokens, &c.CostPUSD); err != nil {
			return nil, fmt.Errorf("store: askers of %s in %s: %w", agentID, courseID, err)
		}
		r := row(opener)
		r.ModelCalls, r.InputTokens, r.OutputTokens, r.CostPUSD = c.ModelCalls, c.InputTokens, c.OutputTokens, c.CostPUSD
	}
	if err := calls.Err(); err != nil {
		return nil, fmt.Errorf("store: askers of %s in %s: %w", agentID, courseID, err)
	}
	out := make([]store.AskerUsage, 0, len(byAsker))
	for _, r := range byAsker {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b store.AskerUsage) int { return strings.Compare(a.OpenerMemberID, b.OpenerMemberID) })
	return out, nil
}

// TenantUsage is the use on keySource in [since, until), a row per tenant.
func (s *Store) TenantUsage(ctx context.Context, keySource string, since, until time.Time) ([]store.TenantUsage, error) {
	if err := store.CheckKeySpan(keySource, since, until); err != nil {
		return nil, err
	}
	byTenant := map[string]*store.TenantUsage{}
	row := func(tenant string) *store.TenantUsage {
		if byTenant[tenant] == nil {
			byTenant[tenant] = &store.TenantUsage{TenantID: tenant}
		}
		return byTenant[tenant]
	}
	answers, err := s.pool.Query(ctx, `
		SELECT tenant_id, count(*) FILTER (WHERE billable)
		  FROM answer
		 WHERE key_source = $1 AND at >= $2 AND at < $3
		 GROUP BY 1`, keySource, since, until)
	if err != nil {
		return nil, fmt.Errorf("store: use of the %s key: %w", keySource, err)
	}
	defer answers.Close()
	for answers.Next() {
		var tenant string
		var billable int
		if err := answers.Scan(&tenant, &billable); err != nil {
			return nil, fmt.Errorf("store: use of the %s key: %w", keySource, err)
		}
		row(tenant).Answers += billable
	}
	if err := answers.Err(); err != nil {
		return nil, fmt.Errorf("store: use of the %s key: %w", keySource, err)
	}
	calls, err := s.pool.Query(ctx, `
		SELECT tenant_id, count(*), sum(cost_pusd)::bigint
		  FROM llm_call
		 WHERE key_source = $1 AND at >= $2 AND at < $3 AND kind = 'model_calls'
		 GROUP BY 1`, keySource, since, until)
	if err != nil {
		return nil, fmt.Errorf("store: use of the %s key: %w", keySource, err)
	}
	defer calls.Close()
	for calls.Next() {
		var tenant string
		var n int
		var cost int64
		if err := calls.Scan(&tenant, &n, &cost); err != nil {
			return nil, fmt.Errorf("store: use of the %s key: %w", keySource, err)
		}
		r := row(tenant)
		r.ModelCalls += n
		r.CostPUSD += cost
	}
	if err := calls.Err(); err != nil {
		return nil, fmt.Errorf("store: use of the %s key: %w", keySource, err)
	}
	out := make([]store.TenantUsage, 0, len(byTenant))
	for _, r := range byTenant {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b store.TenantUsage) int { return strings.Compare(a.TenantID, b.TenantID) })
	return out, nil
}
