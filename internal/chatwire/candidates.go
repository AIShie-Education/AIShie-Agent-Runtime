package chatwire

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// ChatCandidates enumerates configured possibilities, never authorization:
// CoreAccess must still call check_ask for each one. HostedAgents and
// KnownSeats supply display/seat metadata only. A store or effective-config
// failure is returned rather than disguised as an empty agent list.
func (w *Wiring) ChatCandidates(ctx context.Context, courseID string) ([]chat.Agent, error) {
	if courseID == "" {
		return nil, errors.New("chatwire: course id required")
	}
	cfg := w.opts.Config()
	if cfg == nil {
		return nil, errors.New("chatwire: current configuration is nil")
	}
	hosted, err := w.opts.Store.HostedAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("chatwire: hosted agents: %w", err)
	}
	rows := make(map[string]store.HostedAgent, len(hosted))
	for _, row := range hosted {
		rows[row.ID] = row
	}
	byActor := make(map[string]chat.Agent)
	for _, configured := range cfg.Agents {
		if configured == nil || configured.Core.AgentID == "" {
			continue
		}
		eff, err := configured.ForCourse(courseID)
		if err != nil {
			return nil, fmt.Errorf("chatwire: agent %s course configuration: %w", configured.ID, err)
		}
		row, isHosted := rows[configured.ID]
		if configured.Paused || isHosted && row.Paused || eff == nil || !eff.Enabled {
			continue
		}
		actor := strings.ToLower(configured.Core.AgentID)
		if isHosted && !strings.EqualFold(row.CoreActorID, actor) {
			return nil, fmt.Errorf("chatwire: hosted agent %s names another Core actor", configured.ID)
		}
		if _, duplicate := byActor[actor]; duplicate {
			return nil, fmt.Errorf("chatwire: more than one configured agent names Core actor %s", actor)
		}
		candidate := chat.Agent{AgentID: actor, DisplayName: eff.DisplayName}
		if isHosted && row.DisplayName != "" {
			candidate.DisplayName = row.DisplayName
		}
		seats, err := w.opts.Store.KnownSeats(ctx, configured.ID)
		if err != nil {
			return nil, fmt.Errorf("chatwire: known seats for agent %s: %w", configured.ID, err)
		}
		var selected *store.SeatRef
		for i := range seats {
			seat := &seats[i]
			if seat.GoneAt != nil || !strings.EqualFold(seat.CourseID, courseID) || seat.Status != "active" || seat.CourseStatus == "archived" {
				continue
			}
			if selected == nil || seat.SeenAt.After(selected.SeenAt) {
				selected = seat
			}
		}
		if selected != nil {
			candidate.AnswersCourse = selected.AnswersCourse
			candidate.AnswerLevel = selected.Perms["conversation_answer"]
		}
		byActor[actor] = candidate
	}
	out := make([]chat.Agent, 0, len(byActor))
	for _, candidate := range byActor {
		out = append(out, candidate)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}
