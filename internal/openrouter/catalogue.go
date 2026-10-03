package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// The list of the upstream providers serving one model, as the
// administrators' page reads it to set an offer's routing: OpenRouter's
// public, keyless lists, read without the school's key (a read on a page
// must neither spend nor show it), and so without the latency and
// throughput OpenRouter gives only to callers with one.

// DefaultBaseURL is OpenRouter's API.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// The catalogue's bounds.
const (
	// FreshFor is how long a list read is answered without reading it
	// again; StaleFor how long it is still answered, as stale, while
	// OpenRouter cannot be reached.
	FreshFor = 10 * time.Minute
	StaleFor = time.Hour
	// MaxModels bounds the models whose lists are kept, the oldest read
	// dropped first.
	MaxModels = 256
	// CallTimeout bounds each call to OpenRouter, and MaxBody what is read
	// of its answer.
	CallTimeout = 10 * time.Second
	MaxBody     = 8 << 20
)

// ErrModelNotFound is a model OpenRouter does not have (its 404).
var ErrModelNotFound = errors.New("openrouter: no such model")

// UnavailableError is OpenRouter not reached, or not answering as it
// does: a network error, a timeout, a redirect, an address refused, an
// answer not 200 (but a 404), a body past MaxBody, or JSON not of its
// shape. HTTPStatus is OpenRouter's status, 0 when none came.
type UnavailableError struct {
	HTTPStatus int
	Err        error
}

func (e *UnavailableError) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("openrouter: unavailable (HTTP %d): %v", e.HTTPStatus, e.Err)
	}
	return fmt.Sprintf("openrouter: unavailable: %v", e.Err)
}

func (e *UnavailableError) Unwrap() error { return e.Err }

// CatalogueOptions configure a Catalogue.
type CatalogueOptions struct {
	// BaseURL is OpenRouter's API; "" is DefaultBaseURL. Tests set another.
	BaseURL string
	// Client carries the calls: the hosted-model client, which connects
	// to public addresses alone and follows no redirect. Nil is
	// http.DefaultClient.
	Client *http.Client
	// UserAgent is sent with every call.
	UserAgent string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Catalogue reads OpenRouter's lists of a model's upstream endpoints, of
// its providers, and of its zero-data-retention endpoints, and keeps
// them: each list for FreshFor, and for StaleFor to answer while
// OpenRouter cannot be reached. A failure is never kept, nor a model
// OpenRouter does not have, and callers that want one list at once share
// one call. It is safe for concurrent use.
type Catalogue struct {
	o       CatalogueOptions
	flights group
	// callTimeout is CallTimeout, which tests shorten.
	callTimeout time.Duration

	mu        sync.Mutex
	models    map[string]*modelEntry
	providers *listEntry
	zdr       *listEntry
}

// NewCatalogue makes a catalogue.
func NewCatalogue(o CatalogueOptions) *Catalogue {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Catalogue{o: o, models: map[string]*modelEntry{}, callTimeout: CallTimeout}
}

// Listing is a model's upstream endpoints as the page reads them.
type Listing struct {
	// Model and Name are OpenRouter's id and name of the model: a
	// variant's (…:nitro) are its model's.
	Model, Name string
	// FetchedAt is when OpenRouter was read, to the second; Stale is set
	// when it could not be read now, and this is the last list read.
	FetchedAt time.Time
	Stale     bool
	// Endpoints are in OpenRouter's order.
	Endpoints []Endpoint
}

// Endpoint is one upstream endpoint of a model.
type Endpoint struct {
	// Slug is OpenRouter's tag: what order, only and ignore name.
	Slug string
	// Provider is the provider's slug as /providers lists it, nil when
	// it cannot be told.
	Provider     *string
	ProviderName string
	// Quantization is OpenRouter's, "unknown" where it gives none.
	Quantization string
	// Input, Output, CacheRead and CacheWrite are the listed prices
	// (before Discount) in pUSD a token; Request and Image in pUSD a
	// request and an image. Each is nil where OpenRouter gives none, or
	// one negative or that does not parse.
	Input, Output, CacheRead, CacheWrite *int64
	Request, Image                       *int64
	Discount                             float64
	// HigherAboveTokens is the first long-context price's
	// min_prompt_tokens.
	HigherAboveTokens *int64
	ContextLength     int64
	MaxOutputTokens   *int64
	MaxPromptTokens   *int64
	// Tools, ToolChoice and Reasoning are whether supported_parameters
	// holds them.
	Tools, ToolChoice, Reasoning bool
	// ZDR is whether /endpoints/zdr lists it; nil when that list could
	// not be read.
	ZDR *bool
	// Status is OpenRouter's: 0 normal, below it degraded.
	Status int
	// Uptime30m and Uptime1d are percentages, nil where too few calls
	// were made.
	Uptime30m, Uptime1d *float64
	// Latency and Throughput are OpenRouter's over the last 30 minutes:
	// nil without a key.
	Latency, Throughput *Percentiles
	// From /providers: where the provider is based and has datacenters
	// (ISO 3166 codes), and its links, https alone.
	Headquarters                                       *string
	Datacenters                                        []string
	PrivacyPolicyURL, TermsOfServiceURL, StatusPageURL *string

	modelID string
}

// Percentiles are a measure's percentiles.
type Percentiles struct {
	P50 float64 `json:"p50"`
	P75 float64 `json:"p75"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
}

type modelEntry struct {
	model, name string
	endpoints   []Endpoint
	fetched     time.Time
}

// listEntry is /providers' or /endpoints/zdr's, as read.
type listEntry struct {
	providers []providerInfo
	zdr       map[string]bool
	fetched   time.Time
}

type providerInfo struct {
	slug, name             string
	headquarters           *string
	datacenters            []string
	privacy, terms, status *string
}

// Endpoints is model's list (author/slug, a variant allowed): read now,
// or as kept. It is ErrModelNotFound for a model OpenRouter does not have,
// and an *UnavailableError when OpenRouter cannot be read and no list of
// the model's read within StaleFor is kept. /providers and /endpoints/zdr
// not read leave what they give nil.
func (c *Catalogue) Endpoints(ctx context.Context, model string) (*Listing, error) {
	key := strings.ToLower(model)
	var (
		wg         sync.WaitGroup
		prov, zdrs *listEntry
	)
	wg.Go(func() { prov = c.list(ctx, "providers", &c.providers, "/providers", readProviders) })
	wg.Go(func() { zdrs = c.list(ctx, "zdr", &c.zdr, "/endpoints/zdr", readZDR) })
	e, stale, err := c.model(ctx, key, model)
	wg.Wait()
	if err != nil {
		return nil, err
	}
	out := &Listing{Model: e.model, Name: e.name, FetchedAt: e.fetched, Stale: stale, Endpoints: make([]Endpoint, 0, len(e.endpoints))}
	for _, ep := range e.endpoints {
		out.Endpoints = append(out.Endpoints, join(ep, prov, zdrs))
	}
	return out, nil
}

// model is the model's endpoints as kept, or read again when they are
// FreshFor old; stale when read again in vain, and kept StaleFor.
func (c *Catalogue) model(ctx context.Context, key, model string) (*modelEntry, bool, error) {
	now := c.o.Now()
	c.mu.Lock()
	kept := c.models[key]
	c.mu.Unlock()
	if kept != nil && now.Sub(kept.fetched) < FreshFor {
		return kept, false, nil
	}
	v, err := c.flights.do(ctx, "model\x00"+key, c.callTimeout, func(ctx context.Context) (any, error) {
		e, err := c.fetchModel(ctx, model)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.models[key]; !ok && len(c.models) >= MaxModels {
			oldest := ""
			for k, m := range c.models {
				if oldest == "" || m.fetched.Before(c.models[oldest].fetched) {
					oldest = k
				}
			}
			delete(c.models, oldest)
		}
		c.models[key] = e
		return e, nil
	})
	switch {
	case err == nil:
		return v.(*modelEntry), false, nil
	case errors.Is(err, ErrModelNotFound):
		c.mu.Lock()
		delete(c.models, key)
		c.mu.Unlock()
		return nil, false, err
	case kept != nil && now.Sub(kept.fetched) <= StaleFor:
		return kept, true, nil
	}
	return nil, false, err
}

// list is /providers' or /endpoints/zdr's list, as kept or read again,
// as model reads a model's; nil when it cannot be read and none read
// within StaleFor is kept.
func (c *Catalogue) list(ctx context.Context, name string, slot **listEntry, path string, read func([]byte) (*listEntry, error)) *listEntry {
	now := c.o.Now()
	c.mu.Lock()
	kept := *slot
	c.mu.Unlock()
	if kept != nil && now.Sub(kept.fetched) < FreshFor {
		return kept
	}
	v, err := c.flights.do(ctx, name, c.callTimeout, func(ctx context.Context) (any, error) {
		body, err := c.get(ctx, path)
		if err != nil {
			return nil, err
		}
		e, err := read(body)
		if err != nil {
			return nil, &UnavailableError{HTTPStatus: http.StatusOK, Err: err}
		}
		e.fetched = c.o.Now()
		c.mu.Lock()
		*slot = e
		c.mu.Unlock()
		return e, nil
	})
	switch {
	case err == nil:
		return v.(*listEntry)
	case kept != nil && now.Sub(kept.fetched) <= StaleFor:
		return kept
	}
	return nil
}

// get reads one of OpenRouter's lists: 200 and its body, at most MaxBody;
// ErrModelNotFound for a 404; an *UnavailableError otherwise.
func (c *Catalogue) get(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.o.BaseURL+path, nil)
	if err != nil {
		return nil, &UnavailableError{Err: err}
	}
	req.Header.Set("Accept", "application/json")
	if c.o.UserAgent != "" {
		req.Header.Set("User-Agent", c.o.UserAgent)
	}
	resp, err := c.o.Client.Do(req)
	if err != nil {
		return nil, &UnavailableError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, ErrModelNotFound
	case resp.StatusCode != http.StatusOK:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &UnavailableError{HTTPStatus: resp.StatusCode, Err: errors.New("not 200")}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	switch {
	case err != nil:
		return nil, &UnavailableError{HTTPStatus: resp.StatusCode, Err: err}
	case len(body) > MaxBody:
		return nil, &UnavailableError{HTTPStatus: resp.StatusCode, Err: fmt.Errorf("the answer is larger than %d bytes", MaxBody)}
	}
	return body, nil
}

// fetchModel reads the model's endpoints.
func (c *Catalogue) fetchModel(ctx context.Context, model string) (*modelEntry, error) {
	author, slug, ok := strings.Cut(model, "/")
	if !ok || author == "" || slug == "" {
		return nil, ErrModelNotFound
	}
	body, err := c.get(ctx, "/models/"+url.PathEscape(author)+"/"+url.PathEscape(slug)+"/endpoints")
	if err != nil {
		return nil, err
	}
	e, err := readModel(body)
	if err != nil {
		return nil, &UnavailableError{HTTPStatus: http.StatusOK, Err: err}
	}
	e.fetched = c.o.Now().UTC().Truncate(time.Second)
	return e, nil
}

// wireEndpoint is one endpoint as OpenRouter lists it, of what is read.
type wireEndpoint struct {
	ModelID             string          `json:"model_id"`
	Tag                 string          `json:"tag"`
	ProviderName        string          `json:"provider_name"`
	Quantization        *string         `json:"quantization"`
	ContextLength       *float64        `json:"context_length"`
	MaxCompletionTokens *float64        `json:"max_completion_tokens"`
	MaxPromptTokens     *float64        `json:"max_prompt_tokens"`
	SupportedParameters []string        `json:"supported_parameters"`
	Status              *float64        `json:"status"`
	Uptime30m           *float64        `json:"uptime_last_30m"`
	Uptime1d            *float64        `json:"uptime_last_1d"`
	Latency             json.RawMessage `json:"latency_last_30m"`
	Throughput          json.RawMessage `json:"throughput_last_30m"`
	Pricing             struct {
		Prompt          json.RawMessage `json:"prompt"`
		Completion      json.RawMessage `json:"completion"`
		InputCacheRead  json.RawMessage `json:"input_cache_read"`
		InputCacheWrite json.RawMessage `json:"input_cache_write"`
		Request         json.RawMessage `json:"request"`
		Image           json.RawMessage `json:"image"`
		Discount        *float64        `json:"discount"`
		Overrides       []struct {
			MinPromptTokens *float64 `json:"min_prompt_tokens"`
		} `json:"overrides"`
	} `json:"pricing"`
}

// readModel reads /models/{author}/{slug}/endpoints' answer.
func readModel(body []byte) (*modelEntry, error) {
	var w struct {
		Data *struct {
			ID        string         `json:"id"`
			Name      string         `json:"name"`
			Endpoints []wireEndpoint `json:"endpoints"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("not the shape of a model's endpoints: %w", err)
	}
	if w.Data == nil || w.Data.ID == "" {
		return nil, errors.New("not the shape of a model's endpoints: no data.id")
	}
	e := &modelEntry{model: w.Data.ID, name: w.Data.Name, endpoints: []Endpoint{}}
	for _, x := range w.Data.Endpoints {
		if x.Tag == "" {
			continue
		}
		ep := Endpoint{Slug: x.Tag, ProviderName: x.ProviderName, Quantization: "unknown", modelID: x.ModelID,
			Input: pusd(x.Pricing.Prompt), Output: pusd(x.Pricing.Completion), CacheRead: pusd(x.Pricing.InputCacheRead),
			CacheWrite: pusd(x.Pricing.InputCacheWrite), Request: pusd(x.Pricing.Request), Image: pusd(x.Pricing.Image),
			ContextLength: whole(x.ContextLength), MaxOutputTokens: wholePtr(x.MaxCompletionTokens), MaxPromptTokens: wholePtr(x.MaxPromptTokens),
			Tools: slices.Contains(x.SupportedParameters, "tools"), ToolChoice: slices.Contains(x.SupportedParameters, "tool_choice"),
			Reasoning: slices.Contains(x.SupportedParameters, "reasoning"), Status: int(whole(x.Status)),
			Uptime30m: percent(x.Uptime30m), Uptime1d: percent(x.Uptime1d), Latency: readPercentiles(x.Latency), Throughput: readPercentiles(x.Throughput)}
		if ep.modelID == "" {
			ep.modelID = w.Data.ID
		}
		if x.Quantization != nil && *x.Quantization != "" {
			ep.Quantization = *x.Quantization
		}
		if d := x.Pricing.Discount; d != nil && finite(*d) && *d >= 0 && *d <= 1 {
			ep.Discount = *d
		}
		for _, o := range x.Pricing.Overrides {
			if o.MinPromptTokens != nil {
				ep.HigherAboveTokens = wholePtr(o.MinPromptTokens)
				break
			}
		}
		e.endpoints = append(e.endpoints, ep)
	}
	return e, nil
}

// readProviders reads /providers' answer.
func readProviders(body []byte) (*listEntry, error) {
	var w struct {
		Data []struct {
			Name              string   `json:"name"`
			Slug              string   `json:"slug"`
			PrivacyPolicyURL  *string  `json:"privacy_policy_url"`
			TermsOfServiceURL *string  `json:"terms_of_service_url"`
			StatusPageURL     *string  `json:"status_page_url"`
			Headquarters      *string  `json:"headquarters"`
			Datacenters       []string `json:"datacenters"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &w); err != nil || w.Data == nil {
		return nil, errors.New("not the shape of OpenRouter's providers")
	}
	e := &listEntry{}
	for _, p := range w.Data {
		if p.Slug == "" {
			continue
		}
		info := providerInfo{slug: p.Slug, name: p.Name, datacenters: []string{}, privacy: https(p.PrivacyPolicyURL),
			terms: https(p.TermsOfServiceURL), status: https(p.StatusPageURL)}
		if p.Headquarters != nil && countryRe.MatchString(*p.Headquarters) {
			hq := *p.Headquarters
			info.headquarters = &hq
		}
		for _, d := range p.Datacenters {
			if countryRe.MatchString(d) {
				info.datacenters = append(info.datacenters, d)
			}
		}
		e.providers = append(e.providers, info)
	}
	return e, nil
}

// readZDR reads /endpoints/zdr's answer: the (model, tag) of each
// endpoint, alone.
func readZDR(body []byte) (*listEntry, error) {
	var w struct {
		Data []struct {
			ModelID string `json:"model_id"`
			Tag     string `json:"tag"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &w); err != nil || w.Data == nil {
		return nil, errors.New("not the shape of OpenRouter's ZDR endpoints")
	}
	e := &listEntry{zdr: make(map[string]bool, len(w.Data))}
	for _, x := range w.Data {
		e.zdr[x.ModelID+"\x00"+x.Tag] = true
	}
	return e, nil
}

// countryRe is an ISO 3166 code, as /providers gives them.
var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

// join is ep with what /providers (prov) and /endpoints/zdr (zdr) say
// of it, when they were read.
func join(ep Endpoint, prov, zdr *listEntry) Endpoint {
	ep.Datacenters = []string{}
	if zdr != nil {
		listed := zdr.zdr[ep.modelID+"\x00"+ep.Slug]
		ep.ZDR = &listed
	}
	if prov == nil {
		return ep
	}
	base, _, _ := strings.Cut(ep.Slug, "/")
	var found *providerInfo
	for i := range prov.providers {
		if prov.providers[i].slug == base {
			found = &prov.providers[i]
			break
		}
	}
	if found == nil {
		for i := range prov.providers {
			if ep.ProviderName != "" && strings.EqualFold(prov.providers[i].name, ep.ProviderName) {
				found = &prov.providers[i]
				break
			}
		}
	}
	if found == nil {
		return ep
	}
	slug := found.slug
	ep.Provider = &slug
	ep.Headquarters, ep.Datacenters = clonePtr(found.headquarters), slices.Clone(found.datacenters)
	ep.PrivacyPolicyURL, ep.TermsOfServiceURL, ep.StatusPageURL = clonePtr(found.privacy), clonePtr(found.terms), clonePtr(found.status)
	return ep
}

// https is a link kept only when it is an https URL with a host.
func https(s *string) *string {
	if s == nil {
		return nil
	}
	u, err := url.Parse(*s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil
	}
	v := *s
	return &v
}

// priceRe is a price as OpenRouter writes one: a decimal, with an
// exponent of at most two digits, so that none is enormous to read.
var priceRe = regexp.MustCompile(`^-?(?:[0-9]{1,32}(?:\.[0-9]{0,32})?|\.[0-9]{1,32})(?:[eE][+-]?[0-9]{1,2})?$`)

// pusdPerUSD is how many pico-dollars make a dollar.
var pusdPerUSD = big.NewRat(1_000_000_000_000, 1)

// pusd is a price in dollars a unit (a token, a request, an image), a
// string or a number, in pUSD a unit: exact where it has at most twelve
// places, and else rounded to the nearest. nil for none, one negative,
// and one that does not parse.
func pusd(raw json.RawMessage) *int64 {
	s, ok := decimalText(raw)
	if !ok || s == nil || !priceRe.MatchString(*s) {
		return nil
	}
	r, ok := new(big.Rat).SetString(*s)
	if !ok || r.Sign() < 0 {
		return nil
	}
	r.Mul(r, pusdPerUSD)
	// Round half up: (2·num + den) / (2·den).
	num := new(big.Int).Add(new(big.Int).Mul(r.Num(), big.NewInt(2)), r.Denom())
	n := num.Quo(num, new(big.Int).Mul(r.Denom(), big.NewInt(2)))
	if !n.IsInt64() {
		return nil
	}
	v := n.Int64()
	return &v
}

// whole is n as a whole number, 0 for none or one that is not finite.
func whole(n *float64) int64 {
	if n == nil || !finite(*n) || math.Abs(*n) > 1<<53 {
		return 0
	}
	return int64(math.Round(*n))
}

func wholePtr(n *float64) *int64 {
	if n == nil || !finite(*n) {
		return nil
	}
	v := whole(n)
	return &v
}

// percent is an uptime, nil for none or one not from 0 to 100.
func percent(n *float64) *float64 {
	if n == nil || !finite(*n) || *n < 0 || *n > 100 {
		return nil
	}
	v := *n
	return &v
}

// readPercentiles reads {p50, p75, p90, p99}, nil unless all four are numbers.
func readPercentiles(raw json.RawMessage) *Percentiles {
	var m map[string]*float64
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil
	}
	var p Percentiles
	for k, into := range map[string]*float64{"p50": &p.P50, "p75": &p.P75, "p90": &p.P90, "p99": &p.P99} {
		v := m[k]
		if v == nil || !finite(*v) {
			return nil
		}
		*into = *v
	}
	return &p
}

// group makes the calls of one key at once one call (singleflight). The
// call runs on a context of its own, not canceled with the caller that
// began it, within timeout; each caller waits on its own.
type group struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	done    chan struct{}
	val     any
	err     error
	waiters int
}

func (g *group) do(ctx context.Context, key string, timeout time.Duration, fn func(context.Context) (any, error)) (any, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*flight{}
	}
	f, ok := g.m[key]
	if !ok {
		f = &flight{done: make(chan struct{})}
		g.m[key] = f
		run, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		go func() {
			defer cancel()
			f.val, f.err = fn(run)
			g.mu.Lock()
			delete(g.m, key)
			g.mu.Unlock()
			close(f.done)
		}()
	}
	f.waiters++
	g.mu.Unlock()
	select {
	case <-f.done:
		return f.val, f.err
	case <-ctx.Done():
		return nil, &UnavailableError{Err: ctx.Err()}
	}
}

// waiting is how many callers wait on the call of key: for tests.
func (g *group) waiting(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.m[key]; ok {
		return f.waiters
	}
	return 0
}
