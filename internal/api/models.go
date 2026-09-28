package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/probe"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/registry"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/vault"
)

// The models an owner may choose, and their own key (the API contract,
// §5.3, §5.4; R6): the offers, and a key tried with one token. The owner
// never gives a URL: a model's endpoint is made from the provider's offer.

// Models is GET /models' answer: the providers an owner may put their own
// key to, and the school's offers (D8), which v1 does not make.
type Models struct {
	OwnKey    OwnKeyOffers    `json:"own_key"`
	SchoolKey SchoolKeyOffers `json:"school_key"`
}

// OwnKeyOffers are the providers of the owner's own key.
type OwnKeyOffers struct {
	Offered   bool            `json:"offered"`
	Providers []ProviderOffer `json:"providers"`
}

// SchoolKeyOffers are the school's key's offers: none in v1.
type SchoolKeyOffers struct {
	Offered bool       `json:"offered"`
	Offers  []struct{} `json:"offers"`
}

// ProviderOffer is one provider: its APIs, the first the default; how its
// endpoint is chosen; its keys' prefix, a hint for the form; and the
// models the price table prices by name today.
type ProviderOffer struct {
	Provider        string           `json:"provider"`
	Label           string           `json:"label"`
	Adapters        []string         `json:"adapters"`
	Endpoint        EndpointOffer    `json:"endpoint"`
	KeyPrefix       *string          `json:"key_prefix"`
	SuggestedModels []SuggestedModel `json:"suggested_models"`
}

// EndpointOffer is how a provider's endpoint is chosen: fixed (its URL,
// to read), one of choices, an Azure resource of a pattern, or an AWS
// region of a pattern, some suggested.
type EndpointOffer struct {
	Kind      string        `json:"kind"`
	BaseURL   string        `json:"base_url,omitempty"`
	Choices   []ChoiceOffer `json:"choices,omitempty"`
	Pattern   string        `json:"pattern,omitempty"`
	Example   string        `json:"example,omitempty"`
	Suggested []string      `json:"suggested,omitempty"`
}

// ChoiceOffer is one of a provider's endpoints.
type ChoiceOffer struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	BaseURL string `json:"base_url"`
}

// SuggestedModel is a model a form suggests, and whether it is priced.
type SuggestedModel struct {
	Model  string `json:"model"`
	Priced bool   `json:"priced"`
}

// models is GET /models.
func (s *Server) models(w http.ResponseWriter, _ *http.Request, _ *Caller) {
	now := s.o.Now()
	prices, rt := s.prices(), s.yaml().Runtime
	out := Models{OwnKey: OwnKeyOffers{Offered: true, Providers: []ProviderOffer{}}, SchoolKey: SchoolKeyOffers{Offers: []struct{}{}}}
	for _, o := range registry.Offers() {
		p := ProviderOffer{Provider: o.Provider, Label: o.Label, Adapters: o.Adapters, SuggestedModels: []SuggestedModel{},
			Endpoint: EndpointOffer{Kind: o.Endpoint.Kind, BaseURL: o.Endpoint.BaseURL, Pattern: o.Endpoint.Pattern,
				Example: o.Endpoint.Example, Suggested: o.Endpoint.Suggested}}
		for _, c := range o.Endpoint.Choices {
			p.Endpoint.Choices = append(p.Endpoint.Choices, ChoiceOffer(c))
		}
		if o.KeyPrefix != "" {
			kp := o.KeyPrefix
			p.KeyPrefix = &kp
		}
		for _, m := range prices.Models(o.Provider, now) {
			if _, denied := config.DeniedModel(rt, o.Adapters[0], o.Provider, m); denied {
				continue
			}
			_, priced := prices.Lookup(o.Provider, m, now)
			p.SuggestedModels = append(p.SuggestedModels, SuggestedModel{Model: m, Priced: priced})
		}
		out.OwnKey.Providers = append(out.OwnKey.Providers, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// OwnModelChoice is the owner's choice of a model on their own key (§4):
// a provider of GET /models, one of its adapters (its first by default),
// the model, and the endpoint, resource or region the provider's offer
// takes, with the output bound and the reasoning effort when set.
type OwnModelChoice struct {
	Provider        string  `json:"provider"`
	Adapter         string  `json:"adapter,omitempty"`
	Model           string  `json:"model"`
	Endpoint        string  `json:"endpoint,omitempty"`
	Resource        string  `json:"resource,omitempty"`
	Region          string  `json:"region,omitempty"`
	MaxOutputTokens *int    `json:"max_output_tokens,omitempty"`
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
}

// modelRe is what a model's id may be.
var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

// The bounds of max_output_tokens, and the efforts.
const (
	minOutputTokens = 256
	maxOutputTokens = 32000
)

var efforts = []string{"minimal", "low", "medium", "high"}

// fieldError is a refusal of a member, named by its JSON Pointer.
func fieldError(code, reason, field, msg string) *Error {
	return &Error{Code: code, Reason: reason, Message: msg, Details: map[string]any{"field": field}}
}

// shape holds a choice's members to their own rules (step 1 of §5.9's
// order), at the JSON Pointer at: what is required is there, and each is
// of its form.
func (c *OwnModelChoice) shape(at string) *Error {
	switch {
	case c.Provider == "":
		return fieldError(CodeInvalidArgument, ReasonMissingField, at+"/provider", "a provider is required")
	case c.Model == "":
		return fieldError(CodeInvalidArgument, ReasonMissingField, at+"/model", "a model is required")
	case !modelRe.MatchString(c.Model):
		return fieldError(CodeInvalidArgument, ReasonInvalidField, at+"/model", "a model's id is letters, digits and . _ : / @ + -, at most 128")
	case c.MaxOutputTokens != nil && (*c.MaxOutputTokens < minOutputTokens || *c.MaxOutputTokens > maxOutputTokens):
		return fieldError(CodeInvalidArgument, ReasonInvalidField, at+"/max_output_tokens", "max_output_tokens is from 256 to 32000")
	case c.ReasoningEffort != nil && !slices.Contains(efforts, *c.ReasoningEffort):
		return fieldError(CodeInvalidArgument, ReasonInvalidField, at+"/reasoning_effort", "reasoning_effort is minimal, low, medium or high")
	}
	return nil
}

// section makes the choice a model section on the owner's key, its
// endpoint from the provider's offer (step 3 of §5.9's order): a provider
// not offered, an adapter it does not have, an endpoint choice it does not
// have, and a resource or region not of the offer's pattern, or given
// where the offer takes none, are refused.
func (c *OwnModelChoice) section(at string) (*modelSection, *Error) {
	offer, ok := registry.OfferOf(c.Provider)
	if !ok {
		return nil, fieldError(CodeInvalidArgument, ReasonUnknownProvider, at+"/provider", "not a provider GET /models offers")
	}
	adapter := c.Adapter
	if adapter == "" {
		adapter = offer.Adapters[0]
	} else if !slices.Contains(offer.Adapters, adapter) {
		return nil, fieldError(CodeInvalidArgument, ReasonAdapterNotOffered, at+"/adapter", "not an API the provider is offered by")
	}
	kind := offer.Endpoint.Kind
	switch {
	case c.Endpoint != "" && kind != registry.EndpointChoice:
		return nil, fieldError(CodeInvalidArgument, ReasonInvalidField, at+"/endpoint", "this provider's endpoint is not chosen")
	case c.Resource != "" && kind != registry.EndpointAzureResource:
		return nil, fieldError(CodeInvalidArgument, ReasonInvalidField, at+"/resource", "this provider takes no resource")
	case c.Region != "" && kind != registry.EndpointBedrockRegion:
		return nil, fieldError(CodeInvalidArgument, ReasonInvalidField, at+"/region", "this provider takes no region")
	case kind == registry.EndpointAzureResource && c.Resource == "":
		return nil, fieldError(CodeInvalidArgument, ReasonMissingField, at+"/resource", "Azure OpenAI needs its resource's name")
	case kind == registry.EndpointBedrockRegion && c.Region == "":
		return nil, fieldError(CodeInvalidArgument, ReasonMissingField, at+"/region", "Bedrock needs its region")
	}
	base, region, err := offer.ModelBase(c.Endpoint, c.Resource, c.Region)
	switch {
	case errors.Is(err, registry.ErrUnknownEndpoint):
		return nil, fieldError(CodeInvalidArgument, ReasonUnknownEndpoint, at+"/endpoint", "not one of the provider's endpoints")
	case errors.Is(err, registry.ErrBadResource):
		return nil, fieldError(CodeInvalidArgument, ReasonInvalidField, at+"/resource", "an Azure resource's name is lower-case letters, digits and -")
	case errors.Is(err, registry.ErrBadRegion):
		return nil, fieldError(CodeInvalidArgument, ReasonInvalidField, at+"/region", "not an AWS region, such as us-east-1")
	case err != nil:
		return nil, fieldError(CodeInvalidArgument, ReasonInvalidField, at, "the endpoint cannot be made")
	}
	sec := &modelSection{Adapter: adapter, Model: c.Model, Provider: offer.Provider, BaseURL: base, Region: region, KeySource: config.KeyOwn}
	if c.MaxOutputTokens != nil {
		sec.Params = &modelParams{MaxOutputTokens: *c.MaxOutputTokens}
	}
	if c.ReasoningEffort != nil {
		sec.Reasoning = &modelReasoning{Effort: *c.ReasoningEffort}
	}
	return sec, nil
}

// denied refuses a model runtime.denied_models denies (step 4 of §5.9's
// order).
func (s *Server) denied(sec *modelSection, at string) *Error {
	if p, denied := config.DeniedModel(s.yaml().Runtime, sec.Adapter, sec.Provider, sec.Model); denied {
		return &Error{Code: CodeFailedPrecondition, Reason: ReasonModelDenied, Message: "the school does not allow this model (" + p + ")",
			Details: map[string]any{"field": at + "/model"}}
	}
	return nil
}

// keyMalformed refuses a key that cannot be a provider's: not 8 to 4,096
// printable characters of ASCII with no space, or a Core token, which is
// never sent to a provider.
func keyMalformed(key, field string) *Error {
	bad := len(key) < 8 || len(key) > 4096 || strings.HasPrefix(key, "ais_") || strings.HasPrefix(key, "aisinv_")
	for i := 0; i < len(key) && !bad; i++ {
		bad = key[i] < 0x21 || key[i] > 0x7e
	}
	if bad {
		return fieldError(CodeInvalidArgument, ReasonKeyMalformed, field, "that is not a provider's API key")
	}
	return nil
}

// keyTestRequest is POST /keys/test's body.
type keyTestRequest struct {
	OwnModelChoice
	Key string `json:"key"`
}

// KeyTest is POST /keys/test's answer: what the provider said, as one of
// the contract's results, its HTTP status and code when it gave them, and
// how long it took. Never what it wrote.
type KeyTest struct {
	Result       string  `json:"result"`
	HTTPStatus   *int    `json:"http_status"`
	ProviderCode *string `json:"provider_code"`
	LatencyMS    int64   `json:"latency_ms"`
}

// testKey is POST /keys/test: the owner's key tried with the model they
// chose, in one call of one output token over the hosted-model client.
// The key is never stored, logged, audited or given back: its hint alone
// is audited.
func (s *Server) testKey(w http.ResponseWriter, r *http.Request, c *Caller, au *auditing) {
	var req keyTestRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	if req.Key != "" {
		au.detail["key_hint"] = vault.Hint(store.SecretModelKey, req.Key)
	}
	if req.Provider != "" {
		au.target("provider", req.Provider)
		au.detail["provider"] = req.Provider
	}
	if e := req.shape(""); e != nil {
		WriteError(w, *e)
		return
	}
	if e := keyMalformed(req.Key, "/key"); e != nil {
		delete(au.detail, "key_hint")
		WriteError(w, *e)
		return
	}
	sec, e := req.section("")
	if e == nil {
		e = s.denied(sec, "")
	}
	if e != nil {
		WriteError(w, *e)
		return
	}
	au.detail["adapter"], au.detail["model"] = sec.Adapter, sec.Model
	ctx, cancel := context.WithTimeout(r.Context(), probe.TrialTimeout+5*time.Second)
	defer cancel()
	tr, err := probe.TryModel(ctx, sec.config(), req.Key, s.modelHTTP, s.o.NewAdapter)
	if err != nil {
		s.o.Log.Warn("keys/test: the model's adapter could not be built", "provider", sec.Provider, "adapter", sec.Adapter)
		tr = probe.Trial{Result: probe.ResultUnreachable}
	}
	out := KeyTest{Result: tr.Result, LatencyMS: tr.Latency.Milliseconds()}
	if tr.HTTPStatus != 0 {
		st := tr.HTTPStatus
		out.HTTPStatus = &st
		au.detail["http_status"] = st
	}
	if tr.ProviderCode != "" {
		pc := tr.ProviderCode
		out.ProviderCode = &pc
	}
	au.detail["result"] = tr.Result
	writeJSON(w, http.StatusOK, out)
}

// config is the section as the runtime's configuration holds a model, for
// a trial: the runtime's default output bound when none is chosen.
func (m *modelSection) config() config.Model {
	c := config.Model{Adapter: m.Adapter, Model: m.Model, BaseURL: m.BaseURL, Provider: m.Provider, Region: m.Region,
		KeySource: config.KeyOwn, Params: config.ModelParams{MaxOutputTokens: defaultOutputTokens}}
	if m.Params != nil && m.Params.MaxOutputTokens > 0 {
		c.Params.MaxOutputTokens = m.Params.MaxOutputTokens
	}
	if m.Reasoning != nil {
		c.Reasoning.Effort = m.Reasoning.Effort
	}
	return c
}

// defaultOutputTokens is the runtime's built-in output bound
// (config.Defaults).
const defaultOutputTokens = 2000
