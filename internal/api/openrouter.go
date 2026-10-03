package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
)

// GET /admin/openrouter/endpoints?model=author/slug: the upstream
// providers serving one of OpenRouter's models, as OpenRouter lists them,
// for an administrator setting an offer's upstream routing; and the price
// the price table in force counts the model at. OpenRouter is read
// without a key (package openrouter), over the hosted-model client, and
// its lists are kept ten minutes, and answered as stale for an hour while
// it cannot be reached. A read: not audited.

// OpenRouterEndpoints is the route's answer: the model as OpenRouter names
// it, when its list was read and whether that is stale, the price table's
// price of the model as the query gave it (null where no row prices it
// today), and its endpoints in OpenRouter's order.
type OpenRouterEndpoints struct {
	Model     string               `json:"model"`
	Name      string               `json:"name"`
	FetchedAt time.Time            `json:"fetched_at"`
	Stale     bool                 `json:"stale"`
	Price     *OpenRouterPrice     `json:"price"`
	Endpoints []OpenRouterEndpoint `json:"endpoints"`
}

// OpenRouterPrice is the price table's price of the model: its version,
// and its rates as GET /admin/prices writes them.
type OpenRouterPrice struct {
	Version    string     `json:"version"`
	USDPerMTok PriceRates `json:"usd_per_mtok"`
}

// OpenRouterEndpoint is one upstream endpoint: its slug, what order, only
// and ignore name; its provider (null when it cannot be told) and the
// provider's name; its quantization; its prices as listed, before
// discount, in dollars per million tokens, a request and an image, null
// where none is given; the first long-context price's bound; its limits;
// whether it calls tools, takes tool_choice and reasons; whether it is a
// zero-data-retention endpoint (null when that list could not be read);
// its status, uptime, latency and throughput (null without a key); and
// where its provider is based, its datacenters and its https links.
type OpenRouterEndpoint struct {
	Slug              string                  `json:"slug"`
	Provider          *string                 `json:"provider"`
	ProviderName      string                  `json:"provider_name"`
	Quantization      string                  `json:"quantization"`
	USDPerMTok        OpenRouterRates         `json:"usd_per_mtok"`
	USDPerRequest     *string                 `json:"usd_per_request"`
	USDPerImage       *string                 `json:"usd_per_image"`
	Discount          float64                 `json:"discount"`
	HigherAboveTokens *int64                  `json:"higher_above_tokens"`
	ContextLength     int64                   `json:"context_length"`
	MaxOutputTokens   *int64                  `json:"max_output_tokens"`
	MaxPromptTokens   *int64                  `json:"max_prompt_tokens"`
	Tools             bool                    `json:"tools"`
	ToolChoice        bool                    `json:"tool_choice"`
	Reasoning         bool                    `json:"reasoning"`
	ZDR               *bool                   `json:"zdr"`
	Status            int                     `json:"status"`
	Uptime30m         *float64                `json:"uptime_30m"`
	Uptime1d          *float64                `json:"uptime_1d"`
	Latency           *openrouter.Percentiles `json:"latency"`
	Throughput        *openrouter.Percentiles `json:"throughput"`
	Headquarters      *string                 `json:"headquarters"`
	Datacenters       []string                `json:"datacenters"`
	PrivacyPolicyURL  *string                 `json:"privacy_policy_url"`
	TermsOfServiceURL *string                 `json:"terms_of_service_url"`
	StatusPageURL     *string                 `json:"status_page_url"`
}

// OpenRouterRates are an endpoint's prices in dollars per million
// tokens, as listed: input and output always there, each null where it is
// not given or does not read.
type OpenRouterRates struct {
	Input      *string `json:"input"`
	Output     *string `json:"output"`
	CacheRead  *string `json:"cache_read"`
	CacheWrite *string `json:"cache_write"`
}

// openRouterTimeout bounds the route's reads of OpenRouter, all three.
const openRouterTimeout = 15 * time.Second

// errOpenRouterModel refuses a query's model that is not OpenRouter's id.
var errOpenRouterModel = fieldError(CodeInvalidArgument, ReasonInvalidField, "model", "model is OpenRouter's model ID, author/slug")

// openRouterModel reports whether model is an OpenRouter model's id: a
// model's id of the API's, author/slug, a variant (…:nitro) allowed.
func openRouterModel(model string) bool {
	author, slug, ok := strings.Cut(model, "/")
	return ok && modelRe.MatchString(model) && author != "" && slug != "" && !strings.Contains(slug, "/")
}

func (s *Server) openRouterEndpoints(w http.ResponseWriter, r *http.Request, c *Caller) {
	if !c.IsAdmin {
		WriteError(w, errNotAdmin)
		return
	}
	q, ok := queryOf(w, r, "model")
	if !ok {
		return
	}
	model := q["model"]
	switch {
	case model == "":
		WriteError(w, *fieldError(CodeInvalidArgument, ReasonMissingField, "model", "model is required: OpenRouter's model ID, author/slug"))
		return
	case !openRouterModel(model):
		WriteError(w, *errOpenRouterModel)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), openRouterTimeout)
	defer cancel()
	listing, err := s.openRouter.Endpoints(ctx, model)
	var unavailable *openrouter.UnavailableError
	switch {
	case errors.Is(err, openrouter.ErrModelNotFound):
		WriteError(w, Error{Code: CodeNotFound, Reason: ReasonOpenRouterModelNotFound, Message: "OpenRouter has no model of this id",
			Details: map[string]any{"field": "model"}})
		return
	case err != nil:
		status := any(nil)
		if errors.As(err, &unavailable) && unavailable.HTTPStatus != 0 {
			status = unavailable.HTTPStatus
		}
		s.o.Log.Warn("OpenRouter's list of a model's upstream providers could not be read", "http_status", status)
		WriteError(w, Error{Code: CodeUnavailable, Reason: ReasonOpenRouterUnavailable, Message: "OpenRouter could not be reached",
			Details: map[string]any{"http_status": status}})
		return
	}
	sctx, scancel := context.WithTimeout(r.Context(), storeTimeout)
	defer scancel()
	eff, err := s.effective(sctx)
	if err != nil {
		s.storeUnavailable(w, "the site's prices", err)
		return
	}
	out := OpenRouterEndpoints{Model: listing.Model, Name: listing.Name, FetchedAt: listing.FetchedAt.UTC(), Stale: listing.Stale,
		Endpoints: make([]OpenRouterEndpoint, 0, len(listing.Endpoints))}
	if p, ok := s.pricesOf(eff).Lookup(llm.ProviderOpenRouter, model, s.o.Now()); ok {
		out.Price = &OpenRouterPrice{Version: p.Version, USDPerMTok: ratesOf(p)}
	}
	for _, e := range listing.Endpoints {
		out.Endpoints = append(out.Endpoints, endpointView(e))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// endpointView is e as the route answers it.
func endpointView(e openrouter.Endpoint) OpenRouterEndpoint {
	perMTok := func(p *int64) *string {
		if p == nil {
			return nil
		}
		return strPtr(pricing.FormatUSDPerMTok(*p))
	}
	each := func(p *int64) *string {
		if p == nil {
			return nil
		}
		return strPtr(trimUSD(pricing.FormatUSD(*p, usdPlaces)))
	}
	datacenters := e.Datacenters
	if datacenters == nil {
		datacenters = []string{}
	}
	return OpenRouterEndpoint{Slug: e.Slug, Provider: e.Provider, ProviderName: e.ProviderName, Quantization: e.Quantization,
		USDPerMTok:    OpenRouterRates{Input: perMTok(e.Input), Output: perMTok(e.Output), CacheRead: perMTok(e.CacheRead), CacheWrite: perMTok(e.CacheWrite)},
		USDPerRequest: each(e.Request), USDPerImage: each(e.Image), Discount: e.Discount, HigherAboveTokens: e.HigherAboveTokens,
		ContextLength: e.ContextLength, MaxOutputTokens: e.MaxOutputTokens, MaxPromptTokens: e.MaxPromptTokens, Tools: e.Tools,
		ToolChoice: e.ToolChoice, Reasoning: e.Reasoning, ZDR: e.ZDR, Status: e.Status, Uptime30m: e.Uptime30m, Uptime1d: e.Uptime1d,
		Latency: e.Latency, Throughput: e.Throughput, Headquarters: e.Headquarters, Datacenters: datacenters,
		PrivacyPolicyURL: e.PrivacyPolicyURL, TermsOfServiceURL: e.TermsOfServiceURL, StatusPageURL: e.StatusPageURL}
}

// trimUSD is an amount of dollars written to fixed places with its
// trailing zeros removed, as FormatUSDPerMTok writes a price: "0.0005",
// "2".
func trimUSD(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}
