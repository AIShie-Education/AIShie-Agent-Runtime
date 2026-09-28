package api

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/registry"
)

// modelSection is a model section of a hosted agent's settings, as the API
// writes one (and reads one written by hand, whatever else it holds).
type modelSection struct {
	Adapter   string          `json:"adapter,omitempty"`
	Model     string          `json:"model,omitempty"`
	Provider  string          `json:"provider,omitempty"`
	BaseURL   string          `json:"base_url,omitempty"`
	Region    string          `json:"region,omitempty"`
	KeySource string          `json:"key_source,omitempty"`
	Params    *modelParams    `json:"params,omitempty"`
	Reasoning *modelReasoning `json:"reasoning,omitempty"`
	Fallback  *modelSection   `json:"fallback,omitempty"`
}

type modelParams struct {
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

type modelReasoning struct {
	Effort string `json:"effort,omitempty"`
}

// provider is who serves the section's model: the provider it names, or
// the one its adapter and base_url say.
func (m *modelSection) provider() string {
	if m.Provider != "" {
		return m.Provider
	}
	return llm.DetectProvider(m.Adapter, m.BaseURL)
}

// modelSlots are the models a hosted agent's settings hold, by key (D8,
// the API contract's §7.4): the owner's own key's, and the school's. The
// school's, when there is one, is settings.model on key_source school, and
// the owner's its fallback; otherwise settings.model is the owner's. A
// section without an adapter is none. This, and putOwnModel, are the one
// place the slots meet the row.
func modelSlots(settings json.RawMessage) (own, school *modelSection) {
	var s struct {
		Model *modelSection `json:"model"`
	}
	if json.Unmarshal(settings, &s) != nil || s.Model == nil || s.Model.Adapter == "" {
		return nil, nil
	}
	m := s.Model
	if m.KeySource == config.KeySchool {
		if m.Fallback != nil && m.Fallback.Adapter != "" {
			own = m.Fallback
		}
		return own, m
	}
	return m, nil
}

// putOwnModel is settings with the owner's own-key model own in its slot:
// settings.model on key_source own, or no settings.model when own is nil.
// The rest of the settings are kept as they are.
func putOwnModel(settings json.RawMessage, own *modelSection) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(settings) > 0 {
		if err := json.Unmarshal(settings, &m); err != nil || m == nil {
			return nil, errors.New("api: the agent's settings are not a JSON object")
		}
	}
	if own == nil {
		delete(m, "model")
	} else {
		cp := *own
		cp.KeySource, cp.Fallback = config.KeyOwn, nil
		raw, err := json.Marshal(cp)
		if err != nil {
			return nil, err
		}
		m["model"] = raw
	}
	return json.Marshal(m)
}

// OwnModel is the owner's own-key model as its owner chose it (§4): the
// provider, the API it is spoken to by, the model, the endpoint chosen
// (a choice's id, an Azure resource or an AWS region, as the provider's
// offer has it), and whether the price table prices it today.
type OwnModel struct {
	Provider        string  `json:"provider"`
	Adapter         string  `json:"adapter"`
	Model           string  `json:"model"`
	Endpoint        *string `json:"endpoint"`
	Resource        *string `json:"resource"`
	Region          *string `json:"region"`
	MaxOutputTokens *int    `json:"max_output_tokens"`
	ReasoningEffort *string `json:"reasoning_effort"`
	PriceKnown      bool    `json:"price_known"`
}

// ownModelView is the model section own as its owner reads it; nil for
// none.
func (s *Server) ownModelView(own *modelSection, now time.Time) *OwnModel {
	if own == nil {
		return nil
	}
	v := &OwnModel{Provider: own.provider(), Adapter: own.Adapter, Model: own.Model}
	str := func(x string) *string {
		if x == "" {
			return nil
		}
		return &x
	}
	if offer, ok := registry.OfferOf(v.Provider); ok {
		if endpoint, resource, ok := offer.ChoiceOf(own.BaseURL); ok {
			v.Endpoint, v.Resource = str(endpoint), str(resource)
		}
	}
	v.Region = str(own.Region)
	if own.Params != nil && own.Params.MaxOutputTokens > 0 {
		n := own.Params.MaxOutputTokens
		v.MaxOutputTokens = &n
	}
	if own.Reasoning != nil {
		v.ReasoningEffort = str(own.Reasoning.Effort)
	}
	_, v.PriceKnown = s.prices().Lookup(v.Provider, v.Model, now)
	return v
}
