package chat

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
)

// ConfigSource is api.Hosting's small configuration face.
type ConfigSource interface{ YAML() *config.Config }

// ConfigCandidates discovers real configured agents for a course. Core
// check_ask remains the authorization filter; configuration errors are never
// converted into an empty agent list.
type ConfigCandidates struct{ Source ConfigSource }

func (c ConfigCandidates) ChatCandidates(_ context.Context, courseID string) ([]Agent, error) {
	if c.Source == nil {
		return nil, errors.New("chat: no configuration source")
	}
	cfg := c.Source.YAML()
	if cfg == nil {
		return nil, errors.New("chat: runtime configuration is unavailable")
	}
	out := make([]Agent, 0, len(cfg.Agents))
	seen := map[string]bool{}
	for _, configured := range cfg.Agents {
		if configured == nil || configured.Paused {
			continue
		}
		id := strings.ToLower(configured.Core.AgentID)
		if id == "" || seen[id] {
			continue
		}
		effective, err := configured.ForCourse(courseID)
		if err != nil {
			return nil, err
		}
		if !effective.Enabled {
			continue
		}
		seen[id] = true
		out = append(out, Agent{AgentID: id, DisplayName: configured.DisplayName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}
