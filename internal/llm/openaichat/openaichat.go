// Package openaichat is the openai_chat adapter: OpenAI's Chat Completions
// API and every server that speaks it (Core's docs/agent-runtime.md §3.10).
// That is OpenAI and Azure, DeepSeek, Qwen, Kimi, GLM, OpenRouter, Gemini's
// OpenAI-compatible endpoint, Ollama, vLLM, LM Studio and a LiteLLM proxy.
//
// They share one wire format and differ in its corners: the system prompt's
// role, which field caps the output, how reasoning goes back (§3.6), whether
// tool_choice may be sent (§3.3), how usage is reported (§3.5). The provider
// decides each of them, named by the configuration or detected from the
// base URL (llm.DetectProvider), and where the handout could not confirm a
// provider's behaviour the choice is the one that cannot break a call.
package openaichat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/httpx"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// DefaultBaseURL is OpenAI's, used when the configuration names no base.
const DefaultBaseURL = "https://api.openai.com/v1"

// Adapter calls one Chat Completions endpoint with one model. It keeps no
// state between calls, so one Adapter serves any number of concurrent
// answers.
type Adapter struct {
	provider string
	model    string
	endpoint string
	maker    string
	headers  map[string]string
	caps     llm.Capabilities
	dialect  toolschema.Dialect
	params   llm.Params
	effort   string
	client   *http.Client
	// key is kept only to be removed from any error text a provider echoes
	// it in; it is sent in headers alone.
	key string
}

var _ llm.Adapter = (*Adapter)(nil)

// New builds the adapter for cfg. The provider is cfg.Provider, or the one
// the base URL names; its defaults (llm.Defaults) give the capabilities and
// the schema dialect, which the configuration then overrides.
func New(cfg llm.Config) (*Adapter, error) {
	if cfg.Adapter != "" && cfg.Adapter != llm.AdapterOpenAIChat {
		return nil, fmt.Errorf("openaichat: the configuration is for the %q adapter", cfg.Adapter)
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, errors.New("openaichat: no model is configured")
	}
	configured := strings.TrimSpace(cfg.BaseURL)
	base := configured
	if base == "" {
		base = DefaultBaseURL
	}
	endpoint, err := endpointURL(base)
	if err != nil {
		return nil, err
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = llm.DetectProvider(llm.AdapterOpenAIChat, configured)
	}
	caps, dialect := llm.Defaults(llm.AdapterOpenAIChat, provider)
	caps = cfg.Capabilities.Apply(caps)
	switch {
	case cfg.Dialect != "":
		if !cfg.Dialect.Valid() {
			return nil, fmt.Errorf("openaichat: %q is not a schema dialect", cfg.Dialect)
		}
		dialect = cfg.Dialect
	case caps.StrictTools && dialect == toolschema.OpenAI && isOpenAI(provider):
		// capabilities.strict_tools is how §3.8 turns strict mode on; OpenAI
		// and Azure take strict schemas in exactly the openai_strict shape.
		// DeepSeek's strict mode needs its /beta base and a narrower schema,
		// so it is chosen there by naming the dialect.
		dialect = toolschema.OpenAIStrict
	}
	return &Adapter{
		provider: provider,
		model:    model,
		endpoint: endpoint,
		maker:    llm.MakerOf(llm.AdapterOpenAIChat, base, model),
		headers:  authHeaders(provider, cfg.APIKey, cfg.Headers),
		caps:     caps,
		dialect:  dialect,
		params:   cfg.Params,
		effort:   strings.TrimSpace(cfg.Reasoning.Effort),
		client:   withStatusFix(cfg.HTTPClient),
		key:      cfg.APIKey,
	}, nil
}

// endpointURL is base with /chat/completions after its path. A query string
// stays where it is, for proxies that route on one. The URL is never put in
// an error: it is not secret, but an operator may have put something there
// that is.
func endpointURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("openaichat: the base URL is not an absolute http or https URL")
	}
	if u.User != nil {
		return "", errors.New("openaichat: the base URL holds credentials; the key belongs in key_ref")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	u.RawPath = ""
	u.Fragment = ""
	return u.String(), nil
}

// authHeaders are the configured extra headers, then the key: Azure takes
// it as api-key, everyone else as a bearer token. The key is set last so
// that no configured header can replace it, and names are canonical so that
// "authorization" and "Authorization" cannot both be sent.
func authHeaders(provider, key string, extra map[string]string) map[string]string {
	h := make(map[string]string, len(extra)+1)
	for k, v := range extra {
		h[http.CanonicalHeaderKey(k)] = v
	}
	switch {
	case key == "":
	case provider == llm.ProviderAzure:
		h["Api-Key"] = key
	default:
		h["Authorization"] = "Bearer " + key
	}
	return h
}

// Name is openai_chat.
func (a *Adapter) Name() string { return llm.AdapterOpenAIChat }

// Provider is who serves the endpoint.
func (a *Adapter) Provider() string { return a.provider }

// Model is the configured model id; for Azure, the deployment name.
func (a *Adapter) Model() string { return a.model }

// Maker is openai_chat, the base URL and the model: reasoning goes back
// only to the endpoint and model that wrote it.
func (a *Adapter) Maker() string { return a.maker }

// Dialect is the schema dialect the tools must be given in.
func (a *Adapter) Dialect() toolschema.Dialect { return a.dialect }

// Capabilities are the provider's defaults with the configuration's
// overrides.
func (a *Adapter) Capabilities() llm.Capabilities { return a.caps }

// Call sends req and reads the answer. A refusal of the call (an HTTP
// error, a network failure, an error body in a 200) is an *llm.Error; any
// completed call is a Response, whatever it stopped for.
func (a *Adapter) Call(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	if req == nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: "openaichat: no request"}
	}
	body, err := marshal(a.request(req))
	if err != nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: llm.Clip("openaichat: the request does not encode: " + err.Error())}
	}
	resp, err := httpx.PostJSON(ctx, a.client, a.endpoint, a.headers, body)
	if err != nil {
		return nil, a.scrub(err)
	}
	out, err := a.response(resp, req)
	if err != nil {
		return nil, a.scrub(err)
	}
	return out, nil
}

// scrub removes the key from an error's text. Errors are built from
// response bodies only, but a server that echoes the key it was given would
// otherwise carry it into logs.
func (a *Adapter) scrub(err error) error {
	var e *llm.Error
	if len(a.key) < 8 || !errors.As(err, &e) {
		return err
	}
	e.Message = strings.ReplaceAll(e.Message, a.key, "[redacted]")
	e.Code = strings.ReplaceAll(e.Code, a.key, "[redacted]")
	return err
}

// isOpenAI reports whether the provider is OpenAI's own API or Azure's,
// which share parameter names that other servers do not take.
func isOpenAI(provider string) bool {
	return provider == llm.ProviderOpenAI || provider == llm.ProviderAzure
}

// marshal is json.Marshal without HTML escaping, so that text such as
// "a < b" goes out as written and the goldens stay readable.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
