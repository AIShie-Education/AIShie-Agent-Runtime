package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/provider"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// AgentPrivacy is the small part of an agent's answer-model configuration
// that a person who may ask it needs for the privacy notice. AgentID is the
// agent's actor id in Core, not its owner-only hosted row id. Primary and
// Fallback are null when the agent has no model that can answer in the course.
// No model name, key, key hint, endpoint, credential, owner or other setting
// is part of this contract.
type AgentPrivacy struct {
	AgentID  string                 `json:"agent_id"`
	Primary  *PrimaryModelProvider  `json:"primary"`
	Fallback *FallbackModelProvider `json:"fallback"`
}

// PrimaryModelProvider is who serves the model that answers first. Plan is
// school or own, as config.KeySchool and config.KeyOwn. ProviderLabel is the
// provider catalogue's display label, or null for a provider it does not
// offer (in particular a school's own OpenAI-compatible server).
// SchoolOfferLabel is the real label of the school offer in force, or null
// when the primary model is not one or its label is unavailable.
type PrimaryModelProvider struct {
	Plan             string  `json:"plan"`
	Provider         string  `json:"provider"`
	ProviderLabel    *string `json:"provider_label"`
	SchoolOfferLabel *string `json:"school_offer_label"`
}

// FallbackModelProvider is who serves the fallback when it may answer. Its
// provider is disclosed because a question can go there when the primary is
// unavailable or, behind a school offer, its quota is spent.
type FallbackModelProvider struct {
	Provider      string  `json:"provider"`
	ProviderLabel *string `json:"provider_label"`
}

// privacyProviders is the provider disclosure of the configuration that
// would answer in one course. eff must be the course's effective
// configuration, after a withdrawn school offer has been replaced by its
// fallback, rather than the agent's owner-only stored settings.
func privacyProviders(eff *config.Effective, school config.School) (*PrimaryModelProvider, *FallbackModelProvider) {
	if eff == nil || !eff.Enabled || eff.Paused || eff.Model.Adapter == "" || eff.Model.Model == "" {
		return nil, nil
	}
	m := eff.Model
	provider := m.EffectiveProvider()
	if provider == "" {
		return nil, nil
	}
	plan := m.KeySource
	if plan == "" {
		plan = config.KeyOwn
	}
	primary := &PrimaryModelProvider{Plan: plan, Provider: provider, ProviderLabel: privacyProviderLabel(m)}
	if plan == config.KeySchool && m.Offer != "" {
		if offer, ok := school.OfferOf(m.Offer); ok {
			primary.SchoolOfferLabel = strPtr(offer.Label)
		}
	}
	var fallback *FallbackModelProvider
	if m.Fallback != nil && m.Fallback.Adapter != "" && m.Fallback.Model != "" {
		if p := m.Fallback.EffectiveProvider(); p != "" {
			fallback = &FallbackModelProvider{Provider: p, ProviderLabel: privacyProviderLabel(*m.Fallback)}
		}
	}
	return primary, fallback
}

// privacyProviderLabel never lets a provider override lend an official
// company's label to a custom endpoint. The endpoint must both detect as the
// same provider and be one of its official hosts; an empty URL is the
// adapter's own default. The provider id itself remains EffectiveProvider,
// the same model fact the owner view and worker use.
func privacyProviderLabel(m config.Model) *string {
	providerID := m.EffectiveProvider()
	if providerID == "" || provider.DetectProvider(m.Adapter, m.BaseURL) != providerID {
		return nil
	}
	if m.BaseURL != "" {
		u, err := url.Parse(m.BaseURL)
		if err != nil || u.Scheme != "https" || !registry.Official(m.Adapter, u) {
			return nil
		}
	}
	return catalogueProviderLabel(providerID)
}

// catalogueProviderLabel is the label GET /models already gives for a
// provider of an owner's own key. A self-hosted or otherwise custom endpoint
// has no trustworthy provider-company label, so its label is null.
func catalogueProviderLabel(provider string) *string {
	offer, ok := registry.OfferOf(provider)
	if !ok {
		return nil
	}
	return strPtr(offer.Label)
}

// errPrivacyNotFound is the same refusal for an agent or course that is not
// there and for one the caller may not ask: the disclosure must not become a
// way to enumerate other people's agents or course memberships.
var errPrivacyNotFound = Error{Code: CodeNotFound, Reason: ReasonAgentNotFound,
	Message: "no such agent that you may ask in this course"}

// privacy is GET /agents/{id}/privacy?course_id=…. id is the agent's actor id
// in Core, which the chat already has, never the owner-only hosted row id.
// Core's agent_runtime.check_ask is the authorization decision, under the
// same rules as a real question; only after it allows the caller does the
// runtime read and project the model configuration. Nothing is written.
func (s *Server) privacy(w http.ResponseWriter, r *http.Request, c *Caller) {
	q, ok := queryOf(w, r, "course_id")
	if !ok {
		return
	}
	agentID, bad := agentIDOf(r.PathValue("id"))
	if bad != nil {
		WriteError(w, *bad)
		return
	}
	courseID, bad := privacyCourseID(q["course_id"])
	if bad != nil {
		WriteError(w, *bad)
		return
	}
	permission := s.o.AskPermission
	if permission == nil && s.o.Runtime != nil {
		permission = s.o.Runtime
	}
	if permission == nil {
		s.o.Log.Error("the API cannot check whether a person may ask an agent: the runtime has no credential of its own (CORE_SERVICE_CREDENTIAL)")
		WriteError(w, errRuntimeMisconfigured)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), askTimeout+2*storeTimeout)
	defer cancel()
	decision, err := permission.CheckAsk(core.WithPriority(ctx, core.PriorityAnswer), c.ActorID, agentID, courseID)
	mayAsk := decision != nil && decision.MayAsk
	switch {
	case err != nil:
		e := s.serviceRefusal("agent_runtime.check_ask", err)
		if e.Reason == ReasonCoreTooOld {
			e.Message = "Core cannot check whether this person may ask the agent (agent_runtime.check_ask): it must be upgraded"
		}
		WriteError(w, e)
		return
	case !mayAsk:
		WriteError(w, errPrivacyNotFound)
		return
	}
	eff, school, err := s.privacyEffective(ctx, agentID, courseID)
	if err != nil {
		s.storeUnavailable(w, "an agent's answer-model privacy", err)
		return
	}
	out := AgentPrivacy{AgentID: agentID}
	out.Primary, out.Fallback = privacyProviders(eff, school)
	writeJSON(w, http.StatusOK, out)
}

// privacyCourseID reads the course's Core UUID, in lower case as Core writes
// it. Its field name follows the other query routes' errors.
func privacyCourseID(id string) (string, *Error) {
	if id == "" {
		return "", &Error{Code: CodeInvalidArgument, Reason: ReasonMissingField, Message: "course_id is the course's id in Core",
			Details: map[string]any{"field": "course_id"}}
	}
	u, err := uuid.Parse(id)
	if err != nil || len(id) != 36 {
		return "", &Error{Code: CodeInvalidArgument, Reason: ReasonInvalidField, Message: "course_id is not a UUID",
			Details: map[string]any{"field": "course_id"}}
	}
	return u.String(), nil
}

// privacyEffective is the actual model configuration for this agent in this
// course, not just the hosted row's owner-only base settings. An operator's
// YAML agent wins over a hosted row as registry.Build does. A missing or
// rejected configuration has no provider to disclose; a store failure is
// still an error. In particular registry.ForCourse applies a withdrawn
// school offer's fallback exactly as the worker does.
func (s *Server) privacyEffective(ctx context.Context, agentID, courseID string) (*config.Effective, config.School, error) {
	eff, err := s.effective(ctx)
	if err != nil {
		return nil, config.School{}, err
	}
	school := eff.Runtime.School
	for _, a := range eff.Agents {
		if sameActor(a.Core.AgentID, agentID) {
			course, err := a.ForCourse(courseID)
			if err != nil {
				return nil, school, nil
			}
			return course, school, nil
		}
	}
	row, err := s.o.Store.HostedAgentByActor(ctx, agentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, school, nil
	case err != nil:
		return nil, school, err
	}
	courses, err := s.o.Store.HostedCourses(ctx, row.ID)
	if err != nil {
		return nil, school, err
	}
	course, err := registry.ForCourse(ctx, eff, *row, courses, registry.Options{CoreBaseURL: s.o.CoreBaseURL, Allowlist: s.o.Allowlist}, courseID)
	if err != nil {
		return nil, school, nil
	}
	return course, school, nil
}
