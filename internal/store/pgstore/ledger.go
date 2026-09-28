package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// RecordLLMCall records c, once: a call already recorded under its agent
// and id is left as it is, so that a retried write never counts twice.
func (s *Store) RecordLLMCall(ctx context.Context, c store.LLMCall) error {
	if err := required("id", c.ID, "agent_id", c.AgentID); err != nil {
		return err
	}
	// Checked here as memstore checks it, rather than in the database's
	// words.
	var raw any
	if len(c.RawUsage) > 0 {
		if !json.Valid(c.RawUsage) {
			return fmt.Errorf("store: llm call %s: raw usage is not JSON", c.ID)
		}
		raw = string(c.RawUsage)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO llm_call (agent_id, id, at, tenant_id, course_id, member_id, conversation_id, message_id,
		                      opener_member_id, adapter, provider, model, stop, raw_stop,
		                      input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
		                      estimated, raw_usage, price_version, cost_pusd, key_source, latency_ms)
		VALUES ($1, $2, COALESCE($3::timestamptz, now()), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		        $15, $16, $17, $18, $19, $20, $21::jsonb, $22, $23, $24, $25)
		ON CONFLICT (agent_id, id) DO NOTHING`,
		c.AgentID, c.ID, orNow(c.At), c.TenantID, c.CourseID, c.MemberID, c.ConversationID, c.MessageID,
		c.OpenerMemberID, c.Adapter, c.Provider, c.Model, c.Stop, c.RawStop,
		c.Input, c.CacheRead, c.CacheWrite, c.Output, c.Reasoning,
		c.Estimated, raw, c.PriceVersion, c.CostPUSD, c.KeySource, c.LatencyMS)
	if err != nil {
		return fmt.Errorf("store: record llm call %s: %w", c.ID, err)
	}
	return nil
}

// RecordAnswer records a, once: an answer already recorded under its agent
// and id is left as it is.
func (s *Store) RecordAnswer(ctx context.Context, a store.AnswerRecord) error {
	if err := required("id", a.ID, "agent_id", a.AgentID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO answer (agent_id, id, at, tenant_id, course_id, member_id, conversation_id, message_id,
		                    opener_member_id, key, outcome, billable, turns, tool_calls,
		                    input_tokens, output_tokens, cost_pusd, key_source, prompt_hash, latency_ms)
		VALUES ($1, $2, COALESCE($3::timestamptz, now()), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		        $15, $16, $17, $18, $19, $20)
		ON CONFLICT (agent_id, id) DO NOTHING`,
		a.AgentID, a.ID, orNow(a.At), a.TenantID, a.CourseID, a.MemberID, a.ConversationID, a.MessageID,
		a.OpenerMemberID, a.Key, a.Outcome, a.Billable, a.Turns, a.ToolCalls,
		a.InputTokens, a.OutputTokens, a.CostPUSD, a.KeySource, a.PromptHash, a.LatencyMS)
	if err != nil {
		return fmt.Errorf("store: record answer %s: %w", a.ID, err)
	}
	return nil
}

// spendFilter is the WHERE clause of a scope since $1, and its arguments.
// Only the scope's set fields are named, so that the planner sees plain
// equalities and takes the index that fits (agent, tenant or asker, then
// at). The column names are constants; only values are parameters.
func spendFilter(sc store.SpendScope, since time.Time) (string, []any) {
	var b strings.Builder
	b.WriteString("at >= $1")
	args := []any{since}
	for _, f := range []struct{ column, value string }{
		{"agent_id", sc.AgentID},
		{"tenant_id", sc.TenantID},
		{"course_id", sc.CourseID},
		{"opener_member_id", sc.OpenerMemberID},
		{"key_source", sc.KeySource},
	} {
		if f.value == "" {
			continue
		}
		args = append(args, f.value)
		b.WriteString(" AND " + f.column + " = $" + strconv.Itoa(len(args)))
	}
	return b.String(), args
}

// Spend is what the scope has used since a time: its billable answers, and
// the cost of its model calls.
func (s *Store) Spend(ctx context.Context, sc store.SpendScope, since time.Time) (store.Spend, error) {
	if sc == (store.SpendScope{}) {
		return store.Spend{}, errNoScope
	}
	where, args := spendFilter(sc, since)
	var out store.Spend
	err := s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM answer WHERE billable AND `+where+`),
		        (SELECT COALESCE(sum(cost_pusd), 0)::bigint FROM llm_call WHERE `+where+`)`,
		args...).Scan(&out.Answers, &out.CostPUSD)
	if err != nil {
		return store.Spend{}, fmt.Errorf("store: spend: %w", err)
	}
	return out, nil
}

// RecentAnswerCosts are the costs of the agent's newest n billable answers,
// newest first; none when n is not positive.
func (s *Store) RecentAnswerCosts(ctx context.Context, agentID string, n int) ([]int64, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT cost_pusd FROM answer
		 WHERE agent_id = $1 AND billable
		 ORDER BY at DESC, seq DESC
		 LIMIT $2`, agentID, n)
	if err != nil {
		return nil, fmt.Errorf("store: recent answer costs: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var c int64
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("store: recent answer costs: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recent answer costs: %w", err)
	}
	return out, nil
}

// SetAgentState records the agent's state, replacing the one before
// unless that one names a later config version.
func (s *Store) SetAgentState(ctx context.Context, st store.AgentState) error {
	if err := required("agent_id", st.AgentID, "state", st.State); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO agent_state (agent_id, state, reason, detail, worker, config_version, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::timestamptz, now()))
		ON CONFLICT (agent_id) DO UPDATE
		   SET state = EXCLUDED.state, reason = EXCLUDED.reason, detail = EXCLUDED.detail, worker = EXCLUDED.worker,
		       config_version = EXCLUDED.config_version, updated_at = EXCLUDED.updated_at
		 WHERE agent_state.config_version <= EXCLUDED.config_version`,
		st.AgentID, st.State, st.Reason, st.Detail, st.Worker, st.ConfigVersion, orNow(st.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: set state of agent %s: %w", st.AgentID, err)
	}
	return nil
}

// stateColumns are what scanState reads, in its order.
const stateColumns = `agent_id, state, reason, detail, worker, config_version, updated_at`

func scanState(row pgx.Row) (*store.AgentState, error) {
	var st store.AgentState
	if err := row.Scan(&st.AgentID, &st.State, &st.Reason, &st.Detail, &st.Worker, &st.ConfigVersion, &st.UpdatedAt); err != nil {
		return nil, err
	}
	utc(&st.UpdatedAt)
	return &st, nil
}

// AgentState is one agent's state, or store.ErrNotFound.
func (s *Store) AgentState(ctx context.Context, agentID string) (*store.AgentState, error) {
	st, err := scanState(s.pool.QueryRow(ctx, `SELECT `+stateColumns+` FROM agent_state WHERE agent_id = $1`, agentID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("the state of agent %s: %w", agentID, store.ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: the state of agent %s: %w", agentID, err)
	}
	return st, nil
}

// AgentStates lists every agent's state, by agent id, sorted bytewise as
// memstore sorts it.
func (s *Store) AgentStates(ctx context.Context) ([]store.AgentState, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+stateColumns+` FROM agent_state ORDER BY agent_id COLLATE "C"`)
	if err != nil {
		return nil, fmt.Errorf("store: agent states: %w", err)
	}
	defer rows.Close()
	var out []store.AgentState
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, fmt.Errorf("store: agent states: %w", err)
		}
		out = append(out, *st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: agent states: %w", err)
	}
	return out, nil
}
