package chat

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// ErrAccessDenied deliberately has no reason: Core's check_ask makes every
// refusal and unknown actor/agent/course indistinguishable.
var ErrAccessDenied = errors.New("chat: access denied")

// Agent is the canonical chat agent DTO.
type Agent struct {
	AgentID       string `json:"agent_id"`
	DisplayName   string `json:"display_name"`
	AnswersCourse bool   `json:"answers_course"`
	AnswerLevel   string `json:"answer_level"`
	IsMyDelegate  bool   `json:"is_my_delegate"`
}

// Grant is Core's optional additive authorization grant. It is used only to
// build a Runner request and is never persisted in plaintext chat rows.
type Grant struct {
	SelfMemberID            string
	PrincipalMemberID       string
	OpenerMemberID          string
	ActionDecideLevel       string
	ConversationAskLevel    string
	ConversationAnswerLevel string
	AnswersCourse           bool
	AnswersCourseKnown      bool
}

// Authorization is a positive check_ask answer and its safe display metadata.
type Authorization struct {
	Agent        Agent
	Grant        Grant
	GrantPresent bool
}

// Access is the authorization boundary for every create, ask and stream.
type Access interface {
	Check(ctx context.Context, actorID, courseID, agentID string) (*Authorization, error)
	List(ctx context.Context, actorID, courseID string) ([]Agent, error)
}

// CheckAskClient is implemented by *core.RuntimeService.
type CheckAskClient interface {
	CheckAsk(ctx context.Context, actorID, agentID, courseID string) (*core.RuntimeCheckAskResult, error)
}

// DecisionAccess is Core's fresh, service-only reviewer authorization check.
type DecisionAccess interface {
	CheckChatDecision(ctx context.Context, actorID, agentID, courseID, openerActorID, subject, operation string) (*core.RuntimeChatDecision, error)
}

func (a CoreAccess) CheckChatDecision(ctx context.Context, actorID, agentID, courseID, openerActorID, subject, operation string) (*core.RuntimeChatDecision, error) {
	client, ok := a.Core.(DecisionAccess)
	if !ok {
		return nil, errors.New("chat: Core decision authorization is not configured")
	}
	return client.CheckChatDecision(ctx, actorID, agentID, courseID, openerActorID, subject, operation)
}

// Candidates enumerates AR-configured agents that may be visible in a course.
// CoreAccess still calls check_ask for every candidate before returning it.
type Candidates interface {
	ChatCandidates(ctx context.Context, courseID string) ([]Agent, error)
}

// CandidatesFunc adapts configuration/catalogue discovery to Candidates.
type CandidatesFunc func(context.Context, string) ([]Agent, error)

func (f CandidatesFunc) ChatCandidates(ctx context.Context, courseID string) ([]Agent, error) {
	return f(ctx, courseID)
}

// CoreAccess applies Core's service-only check_ask. Candidate discovery is
// separate because Core intentionally has no "list every askable agent"
// service call and a denied check reveals nothing.
type CoreAccess struct {
	Core       CheckAskClient
	Candidates Candidates
}

func (a CoreAccess) Check(ctx context.Context, actorID, courseID, agentID string) (*Authorization, error) {
	if a.Core == nil {
		return nil, errors.New("chat: Core authorization is not configured")
	}
	r, err := a.Core.CheckAsk(ctx, actorID, agentID, courseID)
	if err != nil {
		return nil, err
	}
	if r == nil || !r.MayAsk {
		return nil, ErrAccessDenied
	}
	auth := &Authorization{Agent: Agent{AgentID: agentID}}
	if r.Grant != nil {
		auth.GrantPresent = true
		auth.Grant = Grant{SelfMemberID: r.Grant.RespondentMemberID, PrincipalMemberID: r.Grant.RespondentPrincipalMemberID,
			OpenerMemberID: r.Grant.OpenerMemberID, ActionDecideLevel: r.Grant.ActionDecideLevel,
			ConversationAskLevel: r.Grant.ConversationAskLevel, ConversationAnswerLevel: r.Grant.ConversationAnswerLevel}
		if r.Grant.AnswersCourse != nil {
			auth.Grant.AnswersCourse, auth.Grant.AnswersCourseKnown = *r.Grant.AnswersCourse, true
			auth.Agent.AnswersCourse = *r.Grant.AnswersCourse
		}
		auth.Agent.AnswerLevel = r.Grant.ConversationAnswerLevel
		auth.Agent.IsMyDelegate = r.Grant.RespondentPrincipalMemberID != "" && r.Grant.RespondentPrincipalMemberID == r.Grant.OpenerMemberID
	}
	return auth, nil
}

func (a CoreAccess) List(ctx context.Context, actorID, courseID string) ([]Agent, error) {
	if a.Candidates == nil {
		return []Agent{}, nil
	}
	candidates, err := a.Candidates.ChatCandidates(ctx, courseID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Agent, len(candidates))
	for _, candidate := range candidates {
		id := strings.ToLower(candidate.AgentID)
		if id == "" {
			continue
		}
		auth, err := a.Check(ctx, actorID, courseID, id)
		if errors.Is(err, ErrAccessDenied) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if auth.Agent.DisplayName == "" {
			auth.Agent.DisplayName = candidate.DisplayName
		}
		if auth.Agent.AnswerLevel == "" {
			auth.Agent.AnswerLevel = candidate.AnswerLevel
		}
		if !auth.Grant.AnswersCourseKnown {
			auth.Agent.AnswersCourse = candidate.AnswersCourse
		}
		byID[id] = auth.Agent
	}
	out := make([]Agent, 0, len(byID))
	for _, agent := range byID {
		out = append(out, agent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}
