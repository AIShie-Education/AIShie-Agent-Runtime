package chatwire

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/chat"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/runner"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

const maxPromptBytes = 256 << 10

// Prepare resolves the conversation's exact configured agent and Core actor,
// validates the token and current seat, applies the course override and quotas,
// and returns at most primary plus fallback requests. No Core token is issued
// here: token issuance/leases belong to process hosting, not a generation.
func (w *Wiring) Prepare(ctx context.Context, in chat.GenerationInput) ([]chat.PreparedRun, error) {
	if err := validateGeneration(in); err != nil {
		return nil, err
	}
	cfg := w.opts.Config()
	if cfg == nil {
		return nil, errAgent
	}
	agent, err := configuredAgent(cfg, in.Conversation)
	if err != nil {
		return nil, err
	}
	eff, err := agent.ForCourse(in.Conversation.CourseID)
	if err != nil || eff == nil || !eff.Enabled || eff.Paused {
		return nil, errAgent
	}
	grant := in.Authorization.Grant
	token, err := w.openCoreToken(ctx, agent, eff)
	if err != nil {
		return nil, err
	}
	identity, err := w.core.Inspect(ctx, eff.Core, token)
	if err != nil {
		return nil, errCore
	}
	if err := verifyActor(agent, eff, in.Conversation.AgentID, identity); err != nil {
		return nil, err
	}
	membership, err := w.currentMembership(ctx, eff, in.Conversation.CourseID, identity, grant)
	if err != nil {
		return nil, err
	}
	policy := policyFor(eff, membership, grant, agent.Hosted)
	messages, hasFiles, err := w.runnerHistory(ctx, in, eff)
	if err != nil {
		return nil, err
	}
	location, err := w.quotaLocation(identity)
	if err != nil {
		return nil, err
	}
	fallbackOnly, err := w.checkQuotas(ctx, cfg.Runtime, eff, in, grant, location)
	if err != nil {
		return nil, err
	}
	system, err := w.systemPrompt(eff, identity, membership, grant, policy, hasFiles)
	if err != nil {
		return nil, err
	}
	base := runner.RunRequest{
		CoreMCPURL: strings.TrimRight(eff.Core.BaseURL, "/") + "/mcp",
		CoreToken:  token, CourseID: in.Conversation.CourseID, ConversationID: in.Conversation.ID,
		MessageID: in.Generation.UserMessageID, Attempt: in.Generation.Attempt,
		AllowWrites:  policy.writes,
		AllowedTools: append([]string{}, policy.allowed...), DeniedTools: append([]string{}, policy.denied...),
		Seat: policy.seat, SystemPrompt: system, Messages: messages,
		Limits: runLimits(eff, w.opts.MaxResultBytes),
	}
	if fallbackOnly {
		if eff.Model.Fallback == nil || effectiveKeySource(*eff.Model.Fallback) != config.KeyOwn {
			return nil, errQuota
		}
		run, err := w.prepareModel(ctx, agent, eff, base, *eff.Model.Fallback)
		if err != nil {
			return nil, err
		}
		return []chat.PreparedRun{run}, nil
	}
	primary, err := w.prepareModel(ctx, agent, eff, base, eff.Model)
	if err != nil {
		return nil, err
	}
	runs := []chat.PreparedRun{primary}
	if eff.Model.Fallback != nil {
		fallback, err := w.prepareModel(ctx, agent, eff, base, *eff.Model.Fallback)
		if err != nil {
			return nil, err
		}
		runs = append(runs, fallback)
	}
	return runs, nil
}

func validateGeneration(in chat.GenerationInput) error {
	c, g, a := in.Conversation, in.Generation, in.Authorization
	if c.ID == "" || c.CourseID == "" || c.AgentID == "" || c.OpenerActorID == "" || g.UserMessageID == "" ||
		g.Attempt < 1 || g.ConversationID != "" && g.ConversationID != c.ID || a == nil || !a.GrantPresent {
		return errConversation
	}
	if a.Agent.AgentID != "" && !strings.EqualFold(a.Agent.AgentID, c.AgentID) {
		return errGrant
	}
	grant := a.Grant
	if grant.SelfMemberID == "" || grant.OpenerMemberID == "" || !grant.AnswersCourseKnown ||
		!validLevel(grant.ActionDecideLevel) || !validLevel(grant.ConversationAskLevel) || !validLevel(grant.ConversationAnswerLevel) {
		return errGrant
	}
	return nil
}

func validLevel(level string) bool {
	switch level {
	case core.LevelDenied, core.LevelConfirmRequired, core.LevelPendingReview, core.LevelAutonomous:
		return true
	default:
		return false
	}
}

func configuredAgent(cfg *config.Config, conversation store.ChatConversation) (*config.Agent, error) {
	var found *config.Agent
	for _, agent := range cfg.Agents {
		if agent == nil {
			continue
		}
		if conversation.RuntimeAgentID != "" {
			if agent.ID != conversation.RuntimeAgentID {
				continue
			}
		} else if !strings.EqualFold(agent.Core.AgentID, conversation.AgentID) {
			continue
		}
		if found != nil {
			return nil, errAgent
		}
		found = agent
	}
	if found == nil || found.Core.AgentID == "" || !strings.EqualFold(found.Core.AgentID, conversation.AgentID) ||
		found.Core.BaseURL == "" {
		return nil, errAgent
	}
	if found.Hosted != nil && !strings.EqualFold(found.Hosted.CoreActorID, found.Core.AgentID) {
		return nil, errAgent
	}
	return found, nil
}

func (w *Wiring) openCoreToken(ctx context.Context, agent *config.Agent, eff *config.Effective) (string, error) {
	var ref string
	if hosted := agent.Hosted; hosted != nil {
		if hosted.TokenSecretID == "" || !hosted.TokenIssued {
			return "", errSecret
		}
		ref = secrets.SchemeSealed + hosted.TokenSecretID
	} else {
		if w.opts.OperatorToken != nil {
			token, found, err := w.opts.OperatorToken(ctx, agent)
			if err != nil || found && token == "" {
				return "", errSecret
			}
			if found {
				return token, nil
			}
		}
		held, err := w.opts.Store.AgentToken(ctx, eff.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "", errSecret
			}
			return "", errStore
		}
		if held == nil || held.SecretID == "" || !strings.EqualFold(held.CoreActorID, eff.Core.AgentID) {
			return "", errTokenActor
		}
		ref = secrets.SchemeSealed + held.SecretID
	}
	token, err := w.opts.Secrets.Resolve(ctx, ref, "")
	if err != nil || token == "" {
		return "", errSecret
	}
	return token, nil
}

func verifyActor(agent *config.Agent, eff *config.Effective, conversationActor string, identity *CoreIdentity) error {
	if identity == nil {
		return errCore
	}
	actor := identity.Actor
	if !strings.EqualFold(actor.ID, eff.Core.AgentID) || !strings.EqualFold(actor.ID, conversationActor) {
		return errTokenActor
	}
	if actor.Kind != "" && actor.Kind != core.KindAgent || actor.Status != "" && actor.Status != core.StatusActive ||
		actor.Hosting != "" && actor.Hosting != core.HostingRuntime {
		return errTokenActor
	}
	if hosted := agent.Hosted; hosted != nil && hosted.OwnerActorID != "" &&
		!strings.EqualFold(actor.OwnerActorID, hosted.OwnerActorID) {
		return errTokenActor
	}
	return nil
}

func (w *Wiring) currentMembership(ctx context.Context, eff *config.Effective, courseID string, identity *CoreIdentity, grant chat.Grant) (core.Membership, error) {
	var membership core.Membership
	found := false
	if identity.HasMemberships {
		for _, m := range identity.Memberships {
			if !strings.EqualFold(m.MemberID, grant.SelfMemberID) || !strings.EqualFold(m.CourseID, courseID) {
				continue
			}
			if found {
				return core.Membership{}, errGrant
			}
			membership, found = m, true
		}
	} else {
		seats, err := w.opts.Store.KnownSeats(ctx, eff.ID)
		if err != nil {
			return core.Membership{}, errStore
		}
		for _, seat := range seats {
			if seat.GoneAt != nil || !strings.EqualFold(seat.MemberID, grant.SelfMemberID) {
				continue
			}
			if !strings.EqualFold(seat.CourseID, courseID) {
				continue
			}
			if found {
				return core.Membership{}, errGrant
			}
			membership = core.Membership{MemberID: seat.MemberID, CourseID: seat.CourseID, Code: seat.CourseCode,
				Title: seat.CourseTitle, Section: seat.Section, Status: seat.Status, CourseStatus: seat.CourseStatus,
				AnswersCourse: seat.AnswersCourse, Perms: seat.Perms}
			if seat.PrincipalMemberID != "" {
				principal := seat.PrincipalMemberID
				membership.PrincipalMemberID = &principal
			}
			found = true
		}
	}
	if !found {
		return core.Membership{}, errGrant
	}
	if membership.Status != core.StatusActive || membership.CourseStatus == "archived" || !membership.Answers() {
		return core.Membership{}, errGrant
	}
	principal := ""
	if membership.PrincipalMemberID != nil {
		principal = *membership.PrincipalMemberID
	}
	if !strings.EqualFold(principal, grant.PrincipalMemberID) || membership.AnswersCourse != grant.AnswersCourse ||
		membership.Level("action_decide") != grant.ActionDecideLevel ||
		membership.Level("conversation_answer") != grant.ConversationAnswerLevel {
		return core.Membership{}, errGrant
	}
	return membership, nil
}

func (w *Wiring) quotaLocation(identity *CoreIdentity) (*time.Location, error) {
	if identity != nil && identity.Actor.TimeZone != "" {
		location, err := time.LoadLocation(identity.Actor.TimeZone)
		if err != nil {
			return nil, errAgent
		}
		return location, nil
	}
	return w.opts.Location, nil
}

func (w *Wiring) systemPrompt(eff *config.Effective, identity *CoreIdentity, membership core.Membership,
	grant chat.Grant, policy toolPolicy, hasFiles bool) (string, error) {
	base := ""
	switch {
	case eff.Prompt.SystemRef != "":
		text, err := readPrompt(eff.Path(eff.Prompt.SystemRef))
		if err != nil {
			return "", errAgent
		}
		base = text
	case eff.Prompt.SystemText != "":
		base = eff.Prompt.SystemText
	default:
		base = prompt.Builtin(grant.AnswersCourse)
	}
	appendText := eff.PromptAppendText
	if eff.PromptAppendRef != "" {
		text, err := readPrompt(eff.Path(eff.PromptAppendRef))
		if err != nil {
			return "", errAgent
		}
		appendText = text
	}
	agentName := eff.DisplayName
	if identity != nil && identity.Actor.DisplayName != "" {
		agentName = identity.Actor.DisplayName
	}
	search := ""
	if slices.Contains(policy.allowed, "course_materials_search") {
		search = "course_materials_search"
	}
	system, _ := prompt.System(prompt.Input{
		Base: base, Append: appendText,
		Seat: prompt.Seat{AgentName: agentName, Course: prompt.CourseName(membership), AnswersCourse: grant.AnswersCourse,
			AnswerLevel: grant.ConversationAnswerLevel, Tools: policy.reads, Writes: policy.changes,
			Files: hasFiles, SearchTool: search},
		AnswerLanguage: eff.Prompt.AnswerLanguage, Now: w.now(),
	})
	return system, nil
}

func readPrompt(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- validated operator configuration names this file.
	if err != nil {
		return "", err
	}
	if len(b) > maxPromptBytes {
		return "", fmt.Errorf("prompt exceeds %d bytes", maxPromptBytes)
	}
	return string(b), nil
}

func runLimits(eff *config.Effective, maxResultBytes int) runner.Limits {
	budget := eff.Budgets.PerAnswer
	turns, calls, writes := budget.Turns, budget.ToolCalls, budget.MaxWrites
	if turns <= 0 {
		turns = 8
	}
	if calls <= 0 {
		calls = 12
	}
	if writes <= 0 {
		writes = 10
	}
	wall := budget.WallClock()
	if wall <= 0 {
		wall = 180 * time.Second
	}
	return runner.Limits{MaxSteps: turns, MaxToolCalls: calls, MaxWrites: writes,
		TimeoutMS: max(wall.Milliseconds(), 1), MaxResultBytes: maxResultBytes}
}

func effectiveKeySource(model config.Model) string {
	if model.KeySource == "" {
		return config.KeyOwn
	}
	return model.KeySource
}

func (w *Wiring) prepareModel(ctx context.Context, agent *config.Agent, eff *config.Effective, base runner.RunRequest, model config.Model) (chat.PreparedRun, error) {
	key := ""
	if model.KeyRef != "" {
		resolved, err := w.opts.Secrets.Resolve(ctx, model.KeyRef, eff.Dir)
		if err != nil {
			return chat.PreparedRun{}, errSecret
		}
		key = resolved
	}
	if agent.Hosted != nil && key == "" {
		return chat.PreparedRun{}, errSecret
	}
	if model.Params.MaxOutputTokens <= 0 {
		model.Params.MaxOutputTokens = 4000
	}
	base.Model = runner.ModelOf(model, key, agent.OverHostedClient(model))
	return chat.PreparedRun{Request: base, TenantID: eff.TenantID, AgentID: eff.ID,
		KeySource: effectiveKeySource(model), Adapter: model.Adapter, Provider: model.EffectiveProvider(), Model: model.Model}, nil
}
