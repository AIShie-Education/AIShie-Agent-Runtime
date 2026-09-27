// Package gemini is the adapter for Gemini's generateContent API (Core's
// docs/agent-runtime.md §3, the "Gemini gC" column): one POST to
// {base}/v1beta/models/{model}:generateContent, the key in x-goog-api-key,
// and the translation of the internal format both ways.
//
// Three things set it apart from the other adapters. A thoughtSignature
// rides on the part it came with (a text or a functionCall part), not in a
// part of its own, so it is kept in that part's Opaque and sent back on the
// same part, and only to the adapter that made it (Gemini 3, which refuses
// a call without one, is given Google's stand-in on another model's calls).
// A functionCall's id is optional, so Opaque also records the id Gemini
// gave, and the call_{n} ids that Normalize makes are never sent. And a
// tool result is an object, not text: {"output": …}, or {"error": …} for an
// is_error result.
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/httpx"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// DefaultBaseURL is Gemini's own endpoint, used when the configuration
// names none.
const DefaultBaseURL = "https://generativelanguage.googleapis.com"

// apiVersion is the path segment every call goes under. Thought signatures,
// parametersJsonSchema and thinkingConfig are documented for v1beta.
const apiVersion = "v1beta"

// keyHeader carries the key. It never goes in the URL, where it would end
// up in proxies' logs and in the text of network errors.
const keyHeader = "x-goog-api-key"

// Adapter is one model behind generateContent. It holds no state between
// calls and is safe for concurrent use.
type Adapter struct {
	model    string
	endpoint string
	key      string
	provider string
	maker    string
	dialect  toolschema.Dialect
	caps     llm.Capabilities
	params   llm.Params
	effort   string
	headers  map[string]string
	client   *http.Client
	// checksSignatures: the model refuses a step without a thought
	// signature on its first call (checksSignatures).
	checksSignatures bool
}

var _ llm.Adapter = (*Adapter)(nil)

// New builds the adapter from one agent's model configuration. It refuses a
// configuration it could only send broken requests with: no model, a model
// id that is not a path segment, a base URL that is not http(s) or that
// carries a query or credentials, the default endpoint without a key, an
// effort it does not know, or a dialect that does not exist.
func New(cfg llm.Config) (*Adapter, error) {
	if cfg.Adapter != "" && cfg.Adapter != llm.AdapterGemini {
		return nil, fmt.Errorf("gemini: configured as adapter %q", cfg.Adapter)
	}
	model := strings.TrimSpace(cfg.Model)
	resource, err := resourceOf(model)
	if err != nil {
		return nil, err
	}
	base, err := baseOf(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey == "" && base == DefaultBaseURL {
		return nil, errors.New("gemini: no API key, and generativelanguage.googleapis.com needs one")
	}
	switch cfg.Reasoning.Effort {
	case "", "minimal", "low", "medium", "high":
	default:
		return nil, fmt.Errorf("gemini: reasoning effort %q is not minimal, low, medium or high", cfg.Reasoning.Effort)
	}
	provider := cfg.Provider
	if provider == "" {
		provider = llm.DetectProvider(llm.AdapterGemini, base)
	}
	caps, dialect := llm.Defaults(llm.AdapterGemini, provider)
	caps = cfg.Capabilities.Apply(caps)
	if cfg.Dialect != "" {
		if !cfg.Dialect.Valid() {
			return nil, fmt.Errorf("gemini: schema dialect %q does not exist", cfg.Dialect)
		}
		dialect = cfg.Dialect
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Adapter{
		model:    model,
		endpoint: base + "/" + apiVersion + "/" + resource + ":generateContent",
		key:      cfg.APIKey,
		provider: provider,
		maker:    llm.MakerOf(llm.AdapterGemini, base, model),
		dialect:  dialect,
		caps:     caps,
		params:   cfg.Params,
		effort:   cfg.Reasoning.Effort,
		headers:  headersOf(cfg.Headers, cfg.APIKey),
		client:   client,

		checksSignatures: checksSignatures(model),
	}, nil
}

// Name is gemini.
func (a *Adapter) Name() string { return llm.AdapterGemini }

// Provider is who serves the endpoint: gemini, unless configured otherwise.
func (a *Adapter) Provider() string { return a.provider }

// Model is the configured model id.
func (a *Adapter) Model() string { return a.model }

// Maker is gemini, the base URL and the model: a signature is replayed only
// to the model that made it.
func (a *Adapter) Maker() string { return a.maker }

// Dialect is gemini_json_schema unless configured otherwise.
func (a *Adapter) Dialect() toolschema.Dialect { return a.dialect }

// Capabilities are the gemini defaults with the agent's overrides.
func (a *Adapter) Capabilities() llm.Capabilities { return a.caps }

// Call makes one generateContent call. A refusal is an *llm.Error; a call
// that completed, for whatever reason it stopped, is a Response.
func (a *Adapter) Call(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	if req == nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: "gemini: no request"}
	}
	wire, err := a.buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: llm.Clip("gemini: encoding the request: " + err.Error())}
	}
	client, refusal := a.capturingClient()
	resp, err := httpx.PostJSON(ctx, client, a.endpoint, a.headers, body)
	if err != nil {
		var le *llm.Error
		if errors.As(err, &le) {
			a.refine(le, refusal.body)
		}
		return nil, err
	}
	out, err := decodeResponse(resp.Body, a.maker)
	if err != nil {
		return nil, err
	}
	out.Normalize()
	return out, nil
}

// segment is one segment of a model's resource name, as Google names models
// (gemini-2.5-flash, gemini-2.0-flash-001, tunedModels/my-model-abc).
var segment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// errNotModelID does not repeat the configured value, which could be
// anything pasted into the wrong field.
var errNotModelID = errors.New("gemini: the model is not a model id: letters, digits, '.', '_' and '-', perhaps after models/ or tunedModels/")

// resourceOf turns a model id into its resource name: models/{id}, or the
// id itself when it already names a collection (models/…, tunedModels/…).
// Only plain segments are taken, so that nothing in a model id can change
// the URL's path or add a query to it.
func resourceOf(model string) (string, error) {
	if model == "" {
		return "", errors.New("gemini: no model is configured")
	}
	parts := strings.Split(model, "/")
	switch {
	case len(parts) == 1:
		parts = []string{"models", parts[0]}
	case len(parts) == 2 && (parts[0] == "models" || parts[0] == "tunedModels"):
	default:
		return "", errNotModelID
	}
	for _, p := range parts {
		if !segment.MatchString(p) {
			return "", errNotModelID
		}
	}
	return parts[0] + "/" + parts[1], nil
}

// baseOf checks the configured base URL and returns it without a trailing
// slash, or DefaultBaseURL for none. A base that already ends in /v1beta is
// taken as the same base without it. A query string or user information is
// refused: the key goes in a header, and nothing secret belongs in a URL.
func baseOf(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultBaseURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("gemini: the base URL does not parse")
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("gemini: the base URL must be an absolute http or https URL")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil {
		return "", errors.New("gemini: the base URL must carry no query, fragment or credentials; the key goes in a header")
	}
	base := strings.TrimRight(u.Scheme+"://"+u.Host+u.EscapedPath(), "/")
	base = strings.TrimSuffix(base, "/"+apiVersion)
	return base, nil
}

// headersOf is the configured extra headers and the key. A configured
// header that would stand in for the key is dropped: the resolved key is
// the only one sent, and header maps are applied in no fixed order.
func headersOf(extra map[string]string, key string) map[string]string {
	h := make(map[string]string, len(extra)+1)
	for k, v := range extra {
		if strings.EqualFold(k, keyHeader) {
			continue
		}
		h[k] = v
	}
	if key != "" {
		h[keyHeader] = key
	}
	return h
}
