// Package anthropic is the adapter for Anthropic's Messages API (Core's
// docs/agent-runtime.md §3.1–§3.10, the Anthropic column of each table). It
// serves Anthropic itself and the servers that speak its format:
// OpenRouter's /messages, DeepSeek's /anthropic, and Ollama's and LM
// Studio's Messages endpoints.
//
// The adapter owns the translation both ways and nothing else: one POST of
// JSON through httpx, its answer read whole (Call) or as the Messages
// API's server-sent events (Stream, stream.go), which show the asker the
// answer as it is written.
package anthropic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/httpx"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

const (
	// DefaultBaseURL is Anthropic's API.
	DefaultBaseURL = "https://api.anthropic.com"
	// APIVersion is the anthropic-version every request carries (§3.9).
	APIVersion = "2023-06-01"
	// DefaultMaxTokens is max_tokens when neither the call nor the agent
	// sets an output cap. The API requires the field.
	DefaultMaxTokens = 4096
)

// Adapter is llm.Adapter for the Messages API. It is safe for concurrent
// use: every field is set once, by New.
type Adapter struct {
	model    string
	base     string
	endpoint string
	provider string
	maker    string
	caps     llm.Capabilities
	dialect  toolschema.Dialect
	// headers hold the key, and key is kept to scrub it from errors;
	// nothing prints either.
	headers map[string]string
	key     string
	client  *http.Client
	params  llm.Params
	effort  string
	family  family
	// noStream is set once the server refused to stream (Stream): its
	// calls are made whole from then on.
	noStream atomic.Bool
}

var (
	_ llm.Adapter  = (*Adapter)(nil)
	_ llm.Streamer = (*Adapter)(nil)
)

// New builds the adapter from one agent's model configuration. An empty
// BaseURL is Anthropic's own API.
func New(cfg llm.Config) (*Adapter, error) {
	if cfg.Adapter != "" && cfg.Adapter != llm.AdapterAnthropic {
		return nil, fmt.Errorf("anthropic: the configuration is for the %q adapter", cfg.Adapter)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("anthropic: no model is configured")
	}
	if cfg.Reasoning.Effort != "" {
		if _, ok := thinkingBudgets[cfg.Reasoning.Effort]; !ok {
			return nil, fmt.Errorf("anthropic: reasoning effort %q is not minimal, low, medium or high", cfg.Reasoning.Effort)
		}
	}
	for name, v := range map[string]*float64{"temperature": cfg.Params.Temperature, "top_p": cfg.Params.TopP} {
		if v != nil && math.IsNaN(*v) {
			return nil, fmt.Errorf("anthropic: %s is not a number", name)
		}
	}
	if cfg.Params.MaxOutputTokens < 0 {
		return nil, fmt.Errorf("anthropic: max_output_tokens %d is negative", cfg.Params.MaxOutputTokens)
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultBaseURL
	}
	endpoint, err := messagesURL(base)
	if err != nil {
		return nil, err
	}
	provider := cfg.Provider
	if provider == "" {
		provider = llm.DetectProvider(llm.AdapterAnthropic, cfg.BaseURL)
	}
	caps, dialect := llm.Defaults(llm.AdapterAnthropic, provider)
	caps = cfg.Capabilities.Apply(caps)
	if cfg.Dialect != "" {
		if !cfg.Dialect.Valid() {
			return nil, fmt.Errorf("anthropic: schema dialect %q is not one the runtime knows", cfg.Dialect)
		}
		dialect = cfg.Dialect
	}
	return &Adapter{
		model:    cfg.Model,
		base:     base,
		endpoint: endpoint,
		provider: provider,
		maker:    llm.MakerOf(llm.AdapterAnthropic, base, cfg.Model),
		caps:     caps,
		dialect:  dialect,
		headers:  requestHeaders(cfg, provider),
		key:      cfg.APIKey,
		client:   cfg.HTTPClient,
		params:   cfg.Params,
		effort:   cfg.Reasoning.Effort,
		family:   familyOf(cfg.Model),
	}, nil
}

// Name is the adapter's name in configuration.
func (a *Adapter) Name() string { return llm.AdapterAnthropic }

// Provider is who serves the endpoint.
func (a *Adapter) Provider() string { return a.provider }

// Model is the configured model id.
func (a *Adapter) Model() string { return a.model }

// Maker is the adapter, the base URL (Anthropic's when none was
// configured) and the model: thinking blocks go back only to it.
func (a *Adapter) Maker() string { return a.maker }

// Dialect is the schema dialect tools are given in.
func (a *Adapter) Dialect() toolschema.Dialect { return a.dialect }

// Capabilities are the provider's defaults with the agent's overrides.
func (a *Adapter) Capabilities() llm.Capabilities { return a.caps }

// FileLimits are the Messages API's on a PDF (its documentation of PDF
// support): 100 pages, and what a request's files may take here, less its
// base64 encoding.
func (a *Adapter) FileLimits() llm.FileLimits {
	return llm.FileLimits{PDFBytes: maxFileBytes / 4 * 3, PDFPages: 100}
}

// Endpoint is the URL requests are posted to.
func (a *Adapter) Endpoint() string { return a.endpoint }

// Call makes one model call. Every error is an *llm.Error.
//
// A request whose replayed thinking blocks the API refuses (staleThinking)
// is sent once more without them, as Anthropic advises: the model answers
// without that reasoning rather than not at all. With a thinking budget,
// the rest of that turn then goes without thinking (turnThinks).
func (a *Adapter) Call(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	return a.call(ctx, req, nil)
}

// call makes one model call, streamed when onText is set.
func (a *Adapter) call(ctx context.Context, req *llm.Request, onText llm.TextFunc) (*llm.Response, error) {
	body, err := a.encodeRequest(req, onText != nil)
	if err != nil {
		return nil, err
	}
	out, err := a.exchange(ctx, body, onText)
	var le *llm.Error
	if errors.As(err, &le) && staleThinking(le) {
		// Taking parts out cannot make a request that encoded fail to; a
		// body that replayed no thinking block is not sent again.
		if stripped, serr := a.encodeRequest(withoutReasoning(req), onText != nil); serr == nil && !bytes.Equal(stripped, body) {
			out, err = a.exchange(ctx, stripped, onText)
		}
	}
	return out, err
}

// exchange sends one body and reads the answer: whole, or, when onText is
// set, as a stream. Every error is an *llm.Error with the key scrubbed from
// it.
func (a *Adapter) exchange(ctx context.Context, body []byte, onText llm.TextFunc) (*llm.Response, error) {
	var out *llm.Response
	if onText == nil {
		resp, err := a.post(ctx, body)
		if err != nil {
			return nil, err
		}
		out, err = a.decodeResponse(resp)
		if err != nil {
			return nil, scrubbed(err, a.key)
		}
		return out, nil
	}
	s, err := httpx.PostStream(ctx, a.client, a.endpoint, a.headers, body)
	if err != nil {
		return nil, a.refused(err)
	}
	defer func() { _ = s.Close() }()
	out, err = a.readStream(s, onText)
	if err != nil {
		return nil, scrubbed(err, a.key)
	}
	return out, nil
}

// scrubbed is err with the key scrubbed from it, when it is an *llm.Error.
func scrubbed(err error, key string) error {
	var le *llm.Error
	if errors.As(err, &le) {
		return scrub(le, key)
	}
	return err
}

// post sends one body, and returns a refusal as an *llm.Error in
// Anthropic's terms with the key scrubbed from it.
func (a *Adapter) post(ctx context.Context, body []byte) (*httpx.Response, error) {
	resp, err := httpx.PostJSON(ctx, a.client, a.endpoint, a.headers, body)
	if err == nil {
		return resp, nil
	}
	return nil, a.refused(err)
}

// refused is a refusal of the call as an *llm.Error in Anthropic's terms,
// with the key scrubbed from it.
func (a *Adapter) refused(err error) *llm.Error {
	var le *llm.Error
	if !errors.As(err, &le) {
		le = &llm.Error{Kind: llm.ErrNetwork, Message: llm.Clip(err.Error())}
	}
	return scrub(refine(le), a.key)
}

// staleThinking reports whether e is the API refusing a replayed thinking
// block: its signature does not verify, or, where preserved thinking is
// enforced (Claude Fable 5.1 and Opus 5.5, for accounts created from
// 2026-08-31), the history before it changed since the block was made
// ("The block is bound to a different conversation"). Anthropic's
// documented recovery is to send the history again without its thinking
// blocks.
func staleThinking(e *llm.Error) bool {
	if e.Status != http.StatusBadRequest || e.Kind != llm.ErrBadRequest {
		return false
	}
	lower := strings.ToLower(e.Message)
	return strings.Contains(lower, "signature") && strings.Contains(lower, "thinking")
}

// withoutReasoning is req with every reasoning part removed, so that no
// thinking block is replayed. req is not changed.
func withoutReasoning(req *llm.Request) *llm.Request {
	out := *req
	out.Messages = make([]llm.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		parts := make([]llm.Part, 0, len(m.Parts))
		for _, p := range m.Parts {
			if p.Type != llm.PartReasoning {
				parts = append(parts, p)
			}
		}
		out.Messages = append(out.Messages, llm.Message{Role: m.Role, Parts: parts})
	}
	return &out
}

// scrub removes the key from an error's text. Errors are built from
// response bodies only, but a server or proxy that echoes the key it was
// given would otherwise carry it into logs, whole or cut off by Clip.
func scrub(e *llm.Error, key string) *llm.Error {
	if len(key) < 8 {
		return e
	}
	e.Message = scrubText(e.Message, key)
	e.Code = scrubText(e.Code, key)
	return e
}

func scrubText(s, key string) string {
	s = strings.ReplaceAll(s, key, "[redacted]")
	// Clip may have cut the key off at the end, leaving its start.
	body := strings.TrimSuffix(s, "…")
	for n := min(len(key)-1, len(body)); n >= 8; n-- {
		if strings.HasSuffix(body, key[:n]) {
			return body[:len(body)-n] + "[redacted]" + s[len(body):]
		}
	}
	return s
}

// versionSegment is a path segment that names an API version (v1).
var versionSegment = regexp.MustCompile(`^v[0-9]+$`)

// messagesURL joins a base URL and the Messages endpoint. Anthropic's base
// is a root under which the API lives at /v1/messages, and so are the
// servers that copy it: DeepSeek's https://api.deepseek.com/anthropic,
// Ollama's and LM Studio's hosts. OpenRouter documents its base with the
// version in it (https://openrouter.ai/api/v1), so a base whose last
// segment is a version gets only /messages. A base that already ends in
// /messages is taken to be the endpoint itself.
func messagesURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("anthropic: base URL %q does not parse", base)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("anthropic: base URL %q is not an http or https URL", base)
	}
	if u.User != nil {
		// A key in the URL would reach logs; keys go in headers.
		return "", errors.New("anthropic: the base URL carries user information; put the key in key_ref")
	}
	path := strings.TrimRight(u.Path, "/")
	last := path[strings.LastIndex(path, "/")+1:]
	switch {
	case last == "messages":
	case versionSegment.MatchString(last):
		path += "/messages"
	default:
		path += "/v1/messages"
	}
	u.Path = path
	u.RawPath = ""
	return u.String(), nil
}

// requestHeaders are the headers every call carries: the agent's own, then
// the API version and the key, which the agent's cannot replace.
//
// The key goes in x-api-key, as Anthropic and the servers that copy its
// format take it. OpenRouter documents a bearer token for every endpoint
// (§3.9), so it gets the key that way too. Anthropic itself refuses a
// request that carries both, which is why nobody else does.
func requestHeaders(cfg llm.Config, provider string) map[string]string {
	h := make(map[string]string, len(cfg.Headers)+3)
	for k, v := range cfg.Headers {
		h[http.CanonicalHeaderKey(k)] = v
	}
	h["Anthropic-Version"] = APIVersion
	if cfg.APIKey != "" {
		h["X-Api-Key"] = cfg.APIKey
		if provider == llm.ProviderOpenRouter {
			h["Authorization"] = "Bearer " + cfg.APIKey
		}
	}
	return h
}
