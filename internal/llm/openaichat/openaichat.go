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
	"sync/atomic"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/httpx"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/openrouter"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

// DefaultBaseURL is OpenAI's, used when the configuration names no base.
const DefaultBaseURL = "https://api.openai.com/v1"

// Adapter calls one Chat Completions endpoint with one model. It keeps no
// state between calls but whether its provider refused to stream, so one
// Adapter serves any number of concurrent answers.
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
	// routing is OpenRouter's upstream routing, canonical: kept for
	// OpenRouter alone, nil for none.
	routing *openrouter.Routing
	client  *http.Client
	// key is kept only to be removed from any error text a provider echoes
	// it in; it is sent in headers alone.
	key string
	// noStream is set once the provider refused to stream (Stream): its
	// calls are made whole from then on.
	noStream atomic.Bool
}

var (
	_ llm.Adapter  = (*Adapter)(nil)
	_ llm.Streamer = (*Adapter)(nil)
)

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
	var routing *openrouter.Routing
	if provider == llm.ProviderOpenRouter {
		routing = cfg.OpenRouter.Canonical()
	}
	return &Adapter{
		provider: provider,
		model:    model,
		endpoint: endpoint,
		maker:    llm.MakerOf(llm.AdapterOpenAIChat, withoutQuery(base), model),
		headers:  authHeaders(provider, cfg.APIKey, cfg.Headers),
		caps:     caps,
		dialect:  dialect,
		params:   cfg.Params,
		effort:   strings.TrimSpace(cfg.Reasoning.Effort),
		routing:  routing,
		client:   cfg.HTTPClient,
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
	// A base given as the whole endpoint, as operators often paste it, is
	// taken as it is rather than made …/chat/completions/chat/completions.
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/chat/completions") {
		u.Path += "/chat/completions"
	}
	u.RawPath = ""
	u.Fragment = ""
	return u.String(), nil
}

// withoutQuery is base without its query string or fragment: the endpoint
// a Maker names. Makers are stamped on every reasoning part the runtime
// keeps, and a query string is where an operator might have put something
// secret. base has been parsed by endpointURL already.
func withoutQuery(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	return u.String()
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

// FileLimits are OpenAI's on file inputs (its documentation of PDF input):
// 100 pages and 32 MB in one request.
func (a *Adapter) FileLimits() llm.FileLimits { return llm.OpenAIFileLimits }

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
		return nil, a.refused(err)
	}
	out, err := a.response(resp, req)
	if err != nil {
		return nil, a.refused(err)
	}
	return out, nil
}

// refused is a refusal of the call as the runtime acts on it: its kind
// corrected for what the provider says in its own words (refine), and the
// key removed from its text.
func (a *Adapter) refused(err error) error {
	var e *llm.Error
	if !errors.As(err, &e) {
		return err
	}
	refine(a.provider, e)
	if len(a.key) >= minRedacted {
		e.Message = redactKey(e.Message, a.key)
		e.Code = redactKey(e.Code, a.key)
	}
	return err
}

// minRedacted is the shortest key, and the shortest piece of one, that is
// redacted: anything shorter would match ordinary text.
const minRedacted = 8

// redactKey removes key from s. Errors are built from response bodies only,
// but a server that echoes the key it was given would otherwise carry it
// into logs; and where s was clipped (llm.Clip) in the middle of the key,
// the part left before the cut is removed too.
func redactKey(s, key string) string {
	s = strings.ReplaceAll(s, key, "[redacted]")
	body := strings.TrimSuffix(s, "…")
	for i := 0; len(body)-i >= minRedacted; i++ {
		if strings.HasPrefix(key, body[i:]) {
			return body[:i] + "[redacted]" + s[len(body):]
		}
	}
	return s
}

// refine corrects the kind of a refusal whose provider says what it is in
// words of its own that httpx, which reads OpenAI's, takes for something
// else: a prompt its moderation flagged (a content filter, which the loop
// answers with the refusal text, not a failure), an account out of credit
// (which no retry mends), a prompt too long (which a shorter history
// mends). Each rule is its provider's documented code or message.
func refine(provider string, e *llm.Error) {
	code := strings.ToLower(e.Code)
	msg := strings.ToLower(e.Message)
	switch provider {
	case llm.ProviderOpenAI, llm.ProviderAzure:
		// A reasoning model refuses a prompt its safety checks flag.
		if code == "invalid_prompt" && (strings.Contains(msg, "flagged") || strings.Contains(msg, "usage policy")) {
			e.Kind = llm.ErrContentFilter
		}
	case llm.ProviderOpenRouter:
		// A model that requires moderation refuses flagged input with a
		// 403, which is no fault of the key.
		if e.Kind == llm.ErrAuth && (strings.Contains(msg, "moderation") || strings.Contains(msg, "flagged")) {
			e.Kind = llm.ErrContentFilter
		}
	case llm.ProviderDeepSeek:
		if strings.Contains(msg, "content exists risk") {
			e.Kind = llm.ErrContentFilter
		}
	case llm.ProviderQwen:
		switch code {
		case "data_inspection_failed", "datainspectionfailed":
			e.Kind = llm.ErrContentFilter
		case "arrearage":
			e.Kind = llm.ErrAuth
		}
	case llm.ProviderGLM:
		switch code {
		case "1301": // unsafe or sensitive content
			e.Kind = llm.ErrContentFilter
		case "1113": // no balance, sent as a 429
			e.Kind = llm.ErrAuth
		case "1261": // prompt too long
			e.Kind = llm.ErrContextOverflow
		}
	case llm.ProviderMoonshot:
		switch {
		case code == "exceeded_current_quota_error": // no balance, sent as a 429
			e.Kind = llm.ErrAuth
		case e.Kind == llm.ErrBadRequest && strings.Contains(msg, "exceeded model token limit"):
			e.Kind = llm.ErrContextOverflow
		}
	}
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
