// Package openairesponses is the openai_responses adapter: OpenAI's
// Responses API, and Azure OpenAI's v1 Responses API, in the runtime's one
// format (Core's docs/agent-runtime.md §3, the Responses column).
//
// Every request says store: false. What reaches the model is a school's
// data: students' questions, their work, a course's documents. The API keeps
// a response for 30 days or more unless told not to, and the runtime needs
// nothing kept: it sends the whole history on every turn and keeps its own
// record (§5.2, §6.3). Being stateless is also why reasoning must come back
// as encrypted_content: an item the API did not store can be replayed only
// with its content, never by its id.
package openairesponses

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/httpx"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// DefaultBaseURL is OpenAI's API, used when the configuration names no base.
const DefaultBaseURL = "https://api.openai.com/v1"

// azureV1Path is where Azure OpenAI's v1 API lives on a resource's host.
const azureV1Path = "/openai/v1"

// Adapter is an openai_responses adapter for one endpoint and model. It
// holds no state between calls and is safe for concurrent use.
type Adapter struct {
	model    string
	provider string
	endpoint string
	maker    string
	// headers are the extra headers and the key's header, built once.
	headers map[string]string
	// key is kept only to take it out of errors (withoutKey).
	key     string
	client  *http.Client
	params  llm.Params
	effort  string
	caps    llm.Capabilities
	dialect toolschema.Dialect
}

var _ llm.Adapter = (*Adapter)(nil)

// New builds an adapter from one agent's model configuration. The provider
// is cfg.Provider, or detected from the base URL; Azure's key goes in the
// api-key header, every other provider's in Authorization as a bearer.
func New(cfg llm.Config) (*Adapter, error) {
	if cfg.Adapter != "" && cfg.Adapter != llm.AdapterOpenAIResponses {
		return nil, fmt.Errorf("openairesponses: the configuration is for adapter %q", cfg.Adapter)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("openairesponses: no model is configured")
	}
	provider := cfg.Provider
	if provider == "" {
		provider = llm.DetectProvider(llm.AdapterOpenAIResponses, cfg.BaseURL)
	}
	base, err := baseURL(cfg.BaseURL, provider)
	if err != nil {
		return nil, err
	}

	caps, dialect := llm.Defaults(llm.AdapterOpenAIResponses, provider)
	caps = cfg.Capabilities.Apply(caps)
	switch {
	case cfg.Dialect != "":
		if !cfg.Dialect.Valid() {
			return nil, fmt.Errorf("openairesponses: unknown schema dialect %q", cfg.Dialect)
		}
		dialect = cfg.Dialect
	case caps.StrictTools:
		dialect = toolschema.OpenAIStrict
	}
	// Tools are declared strict exactly when their schemas were made for
	// it; the capability says what is sent, whatever was asked.
	caps.StrictTools = dialect == toolschema.OpenAIStrict

	// A key read from a file often ends in a newline, which no header may
	// carry: every call would fail as a network error, and be retried.
	key := strings.TrimSpace(cfg.APIKey)
	if !validHeaderValue(key) {
		return nil, errors.New("openairesponses: the API key holds characters no HTTP header can carry")
	}
	headers, err := buildHeaders(cfg.Headers, key, provider)
	if err != nil {
		return nil, err
	}
	return &Adapter{
		model:    cfg.Model,
		provider: provider,
		endpoint: httpx.Join(base, "responses"),
		maker:    llm.MakerOf(llm.AdapterOpenAIResponses, base, cfg.Model),
		headers:  headers,
		key:      key,
		client:   cfg.HTTPClient,
		params:   cfg.Params,
		effort:   strings.TrimSpace(cfg.Reasoning.Effort),
		caps:     caps,
		dialect:  dialect,
	}, nil
}

// baseURL checks the configured base and fills in the default. A base
// carries no query string or credentials: keys go in a header, and the base
// is part of every reasoning part's Maker, which is stored and logged.
func baseURL(raw, provider string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if provider == llm.ProviderAzure {
			return "", errors.New("openairesponses: Azure needs base_url, https://<resource>.openai.azure.com/openai/v1")
		}
		return DefaultBaseURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("openairesponses: base_url is not a URL")
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("openairesponses: base_url must be an http or https URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("openairesponses: base_url must carry no credentials, query string or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	// A bare Azure resource URL is a common slip; the v1 API is always at
	// the same path under it, and nothing else answers there.
	if provider == llm.ProviderAzure && u.Path == "" {
		u.Path = azureV1Path
	}
	return u.String(), nil
}

// buildHeaders is the extra headers with the key's header added. Extra
// headers never carry the credentials: the key goes only where the provider
// expects it, from the key reference. A header net/http would refuse is
// refused here, once, rather than on every call as a network error.
func buildHeaders(extra map[string]string, key, provider string) (map[string]string, error) {
	h := make(map[string]string, len(extra)+1)
	for k, v := range extra {
		if !validHeaderName(k) || !validHeaderValue(v) {
			return nil, fmt.Errorf("openairesponses: header %q is not a valid HTTP header", k)
		}
		switch textproto.CanonicalMIMEHeaderKey(k) {
		case "Authorization", "Api-Key":
			return nil, fmt.Errorf("openairesponses: header %q is set from the key and may not be configured", k)
		}
		h[k] = v
	}
	if key == "" {
		return h, nil
	}
	if provider == llm.ProviderAzure {
		h["api-key"] = key
	} else {
		h["Authorization"] = "Bearer " + key
	}
	return h, nil
}

// validHeaderName reports whether s is a header name (RFC 9110's token).
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, c) >= 0 {
			return false
		}
	}
	return true
}

// validHeaderValue reports whether s may be a header's value: no control
// characters but tab.
func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < ' ' && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// Name is openai_responses.
func (a *Adapter) Name() string { return llm.AdapterOpenAIResponses }

// Provider is who serves the endpoint: openai, azure, or whoever
// cfg.Provider named.
func (a *Adapter) Provider() string { return a.provider }

// Model is the configured model id; on Azure, the deployment name.
func (a *Adapter) Model() string { return a.model }

// Maker is the adapter, base URL and model. The parts that carry what only
// this model may be sent again (reasoning items, item ids, phases) are
// stamped with it, and are replayed only to an adapter with the same Maker.
func (a *Adapter) Maker() string { return a.maker }

// Dialect is the schema dialect tools must be given in: openai, or
// openai_strict when strict tools were asked for.
func (a *Adapter) Dialect() toolschema.Dialect { return a.dialect }

// Capabilities are the adapter's defaults with the agent's overrides.
func (a *Adapter) Capabilities() llm.Capabilities { return a.caps }

// FileLimits are OpenAI's on file inputs (its documentation of PDF input):
// 100 pages and 32 MB in one request.
func (a *Adapter) FileLimits() llm.FileLimits { return llm.OpenAIFileLimits }

// Call sends one request to {base}/responses and translates the answer. A
// refusal of the call (4xx, 5xx, a network error) is an *llm.Error; a
// response the API completed, cut short or failed is a Response whose Stop
// says which.
func (a *Adapter) Call(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	if req == nil {
		return nil, &llm.Error{Kind: llm.ErrBadRequest, Message: "no request"}
	}
	body, err := a.encode(req)
	if err != nil {
		return nil, err
	}
	resp, err := httpx.PostJSON(ctx, a.client, a.endpoint, a.headers, body)
	if err != nil {
		return nil, a.withoutKey(err)
	}
	out, err := a.decode(resp.Body)
	if err != nil {
		return nil, err
	}
	if out.RequestID == "" {
		out.RequestID = resp.Header.Get("X-Request-Id")
	}
	return out, nil
}

// redacted stands for the key in an error.
const redacted = "[redacted]"

// minKeyPrefix is the shortest start of the key that is taken out of an
// error cut in the middle of it.
const minKeyPrefix = 8

// withoutKey is err with the key taken out of its code and message. httpx
// builds them from the response body alone, but a provider, or a proxy on
// the way, that echoes the request's headers in its refusal would put the
// key there, and errors are logged and shown to the agent's owner.
func (a *Adapter) withoutKey(err error) error {
	var le *llm.Error
	if a.key == "" || !errors.As(err, &le) {
		return err
	}
	le.Code = redactKey(le.Code, a.key)
	le.Message = llm.Clip(redactKey(le.Message, a.key))
	return err
}

// redactKey is s with every copy of key replaced, and the start of key
// too where s was clipped in the middle of it.
func redactKey(s, key string) string {
	s = strings.ReplaceAll(s, key, redacted)
	body, clipped := strings.CutSuffix(s, "…")
	if !clipped {
		return s
	}
	for n := len(key) - 1; n >= minKeyPrefix; n-- {
		if strings.HasSuffix(body, key[:n]) {
			return body[:len(body)-n] + redacted + "…"
		}
	}
	return s
}
