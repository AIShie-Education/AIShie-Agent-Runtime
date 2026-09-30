// Package registry runs the agents people host on the runtime beside those
// an operator writes in YAML (docs/design.md §11.2). Each hosted agent, a
// row of the store with its courses, becomes the same agent document a YAML
// file holds, and goes through the same path: merged over the built-in and
// the runtime's defaults, decoded, and validated, but each on its own, so
// that one that does not pass is shown in state error and keeps none of the
// others from running. The configuration the worker runs is YAML ∪ registry
// (Build), rebuilt whenever the registry changes (Watcher).
//
// A hosted agent's settings come from people, through the API, so the
// registry holds them to more than configuration does:
//
//   - They never set what the registry sets from the agent's row: its id,
//     name, tenant, pause, and how it reaches Core (CORE_BASE_URL, with its
//     sealed token).
//   - They never refer to a file or a secret (no *_ref anywhere): a hosted
//     agent reads nothing but its own sealed secrets.
//   - Every model section on the owner's key is given the owner's sealed
//     key, or no key at all, never one it would inherit from the runtime's
//     defaults.
//   - A model section on the school's key is an offer of the school's plan
//     (runtime.school, and the offers the site's administrators made),
//     named by its id and nothing else: the registry writes the offer's
//     settings, its key's reference among them, in the document it builds,
//     which is never stored. The key stays a file on the runtime's host,
//     or a sealed secret of the school's for an offer the site made; the
//     plan's quotas hold the agent (the worker's quota check). Only the
//     agent's own model may be on the plan, not a course's. On an offer
//     the school has withdrawn, the owner's model behind it answers alone;
//     with none, the agent is not run (ErrOfferWithdrawn).
//   - Every model it calls has a key of its own: a model without one would
//     be called with the runtime's own credentials (Bedrock's, from the
//     host) or at a server that takes none.
//   - A model on the owner's key has a base_url that is empty (the
//     adapter's own) or an official provider's, over https, and sends no
//     extra headers (D9): no request goes to an address the owner chose.
//     An offer's endpoint is the operator's, but for one the site made,
//     which is held to the same as an owner's model.
//
// And gives them one default YAML does not: a hosted agent's owner is
// known, Core naming them, so its model is offered the writes its seats'
// perms allow, in the conversations its owner opens, unless its owner
// turns tools.writes off (docs/design.md §4). A YAML agent's owner is
// whoever its operator says, and it has writes only when its
// configuration turns them on.
//
// The agent document's shape is the key pool of the product owner's D8:
// on the plan, model is the school's offer and model.fallback the owner's
// own-key model, whose key is hosted_agent.key_secret_id, which the worker
// answers with when the provider of the offer cannot be reached, or the
// plan's quotas for the owner, the asker or the school are spent.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Options are the runtime's settings for its hosted agents.
type Options struct {
	// CoreBaseURL is CORE_BASE_URL: the Core every hosted agent connects
	// to. Empty, no hosted agent runs, and each says why.
	CoreBaseURL string
	// Allowlist is CORE_BASE_URL_ALLOWLIST, which CoreBaseURL must be
	// within.
	Allowlist []string
}

// Reader is what the registry reads: the store's hosted agents, and the
// site's settings and offers.
type Reader interface {
	RegistryRev(ctx context.Context) (int64, error)
	HostedAgents(ctx context.Context) ([]store.HostedAgent, error)
	ListHostedCourses(ctx context.Context) ([]store.HostedCourse, error)
	SiteReader
}

// SiteReader is what ReadSite reads: the store's site settings and the
// offers of the school's plan the site made.
type SiteReader interface {
	SiteSettings(ctx context.Context) ([]store.SiteSetting, error)
	SchoolOffers(ctx context.Context) ([]store.SchoolOffer, error)
}

// ReadSite reads what the site's administrators set through the API
// (docs/design.md §11.5): its offers of the school's plan that are turned
// on, each on its sealed key, and its settings. A setting whose value does
// not decode as the API writes it (one written by hand) counts as not set.
func ReadSite(ctx context.Context, r SiteReader) (config.Site, error) {
	var site config.Site
	settings, err := r.SiteSettings(ctx)
	if err != nil {
		return site, err
	}
	offers, err := r.SchoolOffers(ctx)
	if err != nil {
		return site, err
	}
	for _, st := range settings {
		switch st.Name {
		case store.SettingOCR:
			var o config.SiteOCR
			if DecodeSetting(st.Value, &o) {
				site.OCR = o
			}
		case store.SettingSchoolQuotas:
			var q config.SiteQuotas
			if DecodeSetting(st.Value, &q) {
				site.Quotas = &q
			}
		}
	}
	for _, o := range offers {
		if o.Enabled {
			site.Offers = append(site.Offers, SiteOffer(o))
		}
	}
	return site, nil
}

// DecodeSetting reads a setting's value into v, strictly, as ReadSite
// does: every member known, of its type, and nothing after.
func DecodeSetting(raw json.RawMessage, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

// SiteOffer is the site's offer o as the school's plan holds it: a model
// section on its sealed key, marked as the site's.
func SiteOffer(o store.SchoolOffer) config.SchoolOffer {
	return config.SchoolOffer{ID: o.ID, Label: o.Label, Adapter: o.Adapter, Model: o.Model, Provider: o.Provider, BaseURL: o.BaseURL,
		Region: o.Region, KeyRef: secrets.SchemeSealed + o.KeySecretID, Params: config.ModelParams{MaxOutputTokens: o.MaxOutputTokens},
		Reasoning: config.Reasoning{Effort: o.ReasoningEffort}, Site: true}
}

// WithSite is yaml with the site's settings in force (config.Runtime.WithSite):
// a copy, which shares yaml's agents.
func WithSite(yaml *config.Config, site config.Site) *config.Config {
	eff := *yaml
	eff.Runtime = yaml.Runtime.WithSite(site)
	return &eff
}

// ErrOfferWithdrawn is a hosted agent on an offer of the school's plan
// that the school no longer offers (removed, turned off, or held back),
// with no model of its owner's behind it: it is not run, saying so
// (store.ReasonOfferWithdrawn), until the school offers it again or its
// owner chooses another. With the owner's model behind it, that model
// answers alone, on the owner's key.
var ErrOfferWithdrawn = errors.New("the school no longer offers it, and no model of the owner's stands behind it")

// SourceName is what a hosted agent's problems name in place of a file.
func SourceName(agentID string) string { return "registry:" + agentID }

// Build is the configuration the runtime runs: yaml's runtime settings,
// with the site's in force (ReadSite, WithSite), and yaml's agents, then
// every hosted agent that passes. One that does not, or whose id is a YAML
// agent's (which wins), is in the result's Rejected, with why. The
// revision returned is the registry's as it stood before its agents and
// the site's settings were read, so that a change made meanwhile is seen
// as one. yaml is not changed.
func Build(ctx context.Context, yaml *config.Config, r Reader, o Options) (*config.Config, int64, error) {
	rev, err := r.RegistryRev(ctx)
	if err != nil {
		return nil, 0, err
	}
	hosted, err := r.HostedAgents(ctx)
	if err != nil {
		return nil, 0, err
	}
	all, err := r.ListHostedCourses(ctx)
	if err != nil {
		return nil, 0, err
	}
	site, err := ReadSite(ctx, r)
	if err != nil {
		return nil, 0, err
	}
	yaml = WithSite(yaml, site)
	courses := map[string][]store.HostedCourse{}
	for _, c := range all {
		courses[c.AgentID] = append(courses[c.AgentID], c)
	}
	out := &config.Config{Runtime: yaml.Runtime, Dir: yaml.Dir, Agents: slices.Clone(yaml.Agents)}
	yamlIDs := map[string]bool{}
	for _, a := range yaml.Agents {
		yamlIDs[a.ID] = true
	}
	versions := map[string]int{}
	for _, a := range hosted {
		versions[a.ID] = a.Version
	}
	reject := func(id, reason string, err error) {
		out.Rejected = append(out.Rejected, config.Rejection{AgentID: id, Source: SourceName(id), Err: err, Reason: reason,
			Version: versions[id]})
	}
	coreErr := checkCoreBaseURL(o)
	var sources []config.Source
	byID := map[string]store.HostedAgent{}
	for _, a := range hosted {
		switch {
		case yamlIDs[a.ID]:
			reject(a.ID, store.ReasonOperatorAgent, errors.New("a YAML agent has this id, and the operator's configuration wins"))
			continue
		case coreErr != nil:
			reject(a.ID, store.ReasonRuntimeMisconfigured, coreErr)
			continue
		}
		src, err := Document(a, courses[a.ID], o.CoreBaseURL, defaultKeySource(yaml), yaml.Runtime.School)
		switch {
		case errors.Is(err, ErrOfferWithdrawn):
			reject(a.ID, store.ReasonOfferWithdrawn, err)
			continue
		case err != nil:
			reject(a.ID, store.ReasonSettingsRejected, err)
			continue
		}
		sources = append(sources, src)
		byID[a.ID] = a
	}
	agents, rejected := config.LoadDocuments(yaml, o.Allowlist, sources...)
	for _, r := range rejected {
		r.Reason, r.Version = store.ReasonSettingsRejected, versions[r.AgentID]
		out.Rejected = append(out.Rejected, r)
	}
	for _, a := range agents {
		key := ""
		if k := byID[a.ID].KeySecretID; k != "" {
			key = secrets.SchemeSealed + k
		}
		if err := checkModels(a, key, yaml.Runtime.School); err != nil {
			reject(a.ID, store.ReasonSettingsRejected, err)
			continue
		}
		row := byID[a.ID]
		a.Hosted = &config.Hosted{CoreActorID: row.CoreActorID, OwnerActorID: row.OwnerActorID, OwnerVerified: row.OwnerVerified,
			Version: row.Version}
		out.Agents = append(out.Agents, a)
	}
	sort.SliceStable(out.Rejected, func(i, j int) bool { return out.Rejected[i].AgentID < out.Rejected[j].AgentID })
	return out, rev, nil
}

// Check holds one hosted agent, as its row would be written with its
// courses, to what Build holds it to: an id no YAML agent of yaml's has, a
// CORE_BASE_URL, its document, loaded and validated over yaml's runtime
// settings (with the site's in force, WithSite), and its models. It reads
// and writes nothing: the API tries a change with it before it writes one.
// The error is why it would not run, as a Rejection's Detail words it.
func Check(_ context.Context, yaml *config.Config, row store.HostedAgent, courses []store.HostedCourse, o Options) error {
	for _, a := range yaml.Agents {
		if a.ID == row.ID {
			return errors.New("a YAML agent has this id, and the operator's configuration wins")
		}
	}
	if err := checkCoreBaseURL(o); err != nil {
		return err
	}
	src, err := Document(row, courses, o.CoreBaseURL, defaultKeySource(yaml), yaml.Runtime.School)
	if err != nil {
		return err
	}
	agents, rejected := config.LoadDocuments(yaml, o.Allowlist, src)
	if len(rejected) > 0 {
		return rejected[0].Err
	}
	key := ""
	if row.KeySecretID != "" {
		key = secrets.SchemeSealed + row.KeySecretID
	}
	return checkModels(agents[0], key, yaml.Runtime.School)
}

// checkCoreBaseURL refuses a CORE_BASE_URL no hosted agent can use: none.
// One outside CORE_BASE_URL_ALLOWLIST is refused by FromEnv, and by each
// agent's validation.
func checkCoreBaseURL(o Options) error {
	if o.CoreBaseURL == "" {
		return errors.New("CORE_BASE_URL is not set: the runtime has no Core for its hosted agents to connect to")
	}
	return nil
}

// defaultKeySource is the key source an agent has when its settings name
// none: the runtime's defaults', else the built-in one.
func defaultKeySource(yaml *config.Config) string {
	if m, ok := yaml.Runtime.Defaults["model"].(map[string]any); ok {
		if ks, ok := m["key_source"].(string); ok && ks != "" {
			return ks
		}
	}
	return config.KeyOwn
}

// setByRegistry are the agent's settings that its row gives.
var setByRegistry = []string{"id", "display_name", "tenant_id", "paused", "core"}

// Document is a hosted agent's document, as a YAML file would hold it but in
// JSON: its settings and its courses', with what the registry sets itself
// (the package's comment): its id, name, tenant and pause from its row;
// Core at coreBaseURL, with its token as sealed://<token_secret_id>; the
// owner's key, sealed://<key_secret_id>, on each model section whose key
// source, as written or as it inherits it from defaultKeySource (and then
// written out), is own; and on the agent's model on the school's key, the
// settings of school's offer it names. Settings that set any of those
// themselves, refer to a file or a secret, or put a course's model on the
// school's key, are refused. On an offer school no longer has, the owner's
// model behind it is the agent's model; with none, the agent is refused
// with ErrOfferWithdrawn.
func Document(a store.HostedAgent, courses []store.HostedCourse, coreBaseURL, defaultKeySource string, school config.School) (config.Source, error) {
	src := config.Source{Name: SourceName(a.ID)}
	settings, err := object(a.Settings, "its settings")
	if err != nil {
		return src, err
	}
	var problems []string
	for _, k := range setByRegistry {
		if _, ok := settings[k]; ok {
			problems = append(problems, fmt.Sprintf("agent.%s: set by the registry, not by an agent's settings", k))
		}
	}
	problems = append(problems, refs("agent", settings)...)
	key := ""
	if a.KeySecretID != "" {
		key = secrets.SchemeSealed + a.KeySecretID
	}
	model, isMap := settings["model"].(map[string]any)
	switch {
	case !isMap && settings["model"] != nil:
		problems = append(problems, "agent.model: must be a mapping")
		model = map[string]any{}
	case model == nil:
		model = map[string]any{}
		settings["model"] = model
	}
	ks := keyed(model, defaultKeySource, key, "agent.model", &problems)
	if id, _ := model["offer"].(string); ks == config.KeySchool && id != "" && !offered(school, id) {
		fb, ok := model["fallback"].(map[string]any)
		if !ok {
			return src, fmt.Errorf("agent.model.offer %s: %w", strconv.Quote(clip(id, 64)), ErrOfferWithdrawn)
		}
		// The school withdrew the offer: the owner's model behind it
		// answers alone, on the owner's key, as it does when the plan's
		// quotas are spent.
		model = fb
		settings["model"] = model
		ks = keyed(model, config.KeyOwn, key, "agent.model", &problems)
	}
	if ks == config.KeySchool {
		onOffer(model, school, "agent.model", &problems)
	}
	if fb, ok := model["fallback"].(map[string]any); ok {
		keyed(fb, fallbackKeySource(ks), key, "agent.model.fallback", &problems)
		if ks, _ := fb["key_source"].(string); ks == config.KeySchool {
			problems = append(problems, "agent.model.fallback: a fallback is on the owner's key; the school's plan is the model")
		}
	} else {
		// Not one the runtime's defaults would give it, with the
		// operator's key.
		model["fallback"] = nil
	}
	writesOn(settings)
	settings["id"], settings["display_name"], settings["tenant_id"], settings["paused"] = a.ID, a.DisplayName, a.TenantID, a.Paused
	settings["core"] = map[string]any{"base_url": coreBaseURL, "token_ref": secrets.SchemeSealed + a.TokenSecretID}

	doc := map[string]any{"agent": settings}
	if len(courses) > 0 {
		cs := map[string]any{}
		for _, c := range courses {
			path := "courses." + c.CourseID
			cset, err := object(c.Settings, path)
			if err != nil {
				return src, err
			}
			problems = append(problems, refs(path, cset)...)
			if m, ok := cset["model"].(map[string]any); ok {
				cks := keyed(m, ks, key, path+".model", &problems)
				if cks == config.KeySchool {
					problems = append(problems, path+".model: a course's model is not on the school's plan; the plan is chosen for the whole agent")
				}
				if fb, ok := m["fallback"].(map[string]any); ok {
					keyed(fb, fallbackKeySource(cks), key, path+".model.fallback", &problems)
				}
			}
			cs[c.CourseID] = cset
		}
		doc["courses"] = cs
	}
	if len(problems) > 0 {
		return src, errors.New(strings.Join(problems, "; "))
	}
	if src.Data, err = json.Marshal(doc); err != nil {
		return src, fmt.Errorf("its document: %w", err)
	}
	return src, nil
}

// writesOn writes out tools.writes as true in a hosted agent's settings
// that do not set it (the package's comment): its default, over the
// runtime's defaults, as its key source is written out. Tools that are not
// a mapping are left for LoadDocuments to refuse.
func writesOn(settings map[string]any) {
	switch tools, isMap := settings["tools"].(map[string]any); {
	case settings["tools"] == nil:
		settings["tools"] = map[string]any{"writes": true}
	case isMap && tools["writes"] == nil:
		tools["writes"] = true
	}
}

// WritesOf is whether a hosted agent's settings, as its row holds them,
// offer its model its writes: tools.writes as set, and true when it is
// not (writesOn).
func WritesOf(settings json.RawMessage) bool {
	var s struct {
		Tools struct {
			Writes *bool `json:"writes"`
		} `json:"tools"`
	}
	if json.Unmarshal(settings, &s) != nil || s.Tools.Writes == nil {
		return true
	}
	return *s.Tools.Writes
}

// object reads settings that must be a JSON object; none is {}.
func object(raw json.RawMessage, what string) (map[string]any, error) {
	m := map[string]any{}
	if len(raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("%s: not a JSON object", what)
	}
	return m, nil
}

// refs names every key of v, at any depth, that is a reference (*_ref):
// a hosted agent's settings refer to no file and no secret.
func refs(path string, v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + "." + k
			if strings.HasSuffix(k, "_ref") {
				out = append(out, p+": a hosted agent refers to no file or secret; its prompts are *_text, its secrets the registry's")
				continue
			}
			out = append(out, refs(p, t[k])...)
		}
	case []any:
		for i, x := range t {
			out = append(out, refs(fmt.Sprintf("%s[%d]", path, i), x)...)
		}
	}
	return out
}

// fallbackKeySource is the key source a fallback inherits from the model
// it stands behind: its own, but the owner's behind the school's plan.
func fallbackKeySource(ks string) string {
	if ks == config.KeySchool {
		return config.KeyOwn
	}
	return ks
}

// offered reports whether school has the offer id.
func offered(school config.School, id string) bool {
	_, ok := school.OfferOf(id)
	return ok
}

// offerKeys are what a model section on the school's plan holds as its
// row keeps it: the key source, the offer's id, and the owner's fallback.
var offerKeys = []string{"key_source", "offer", "fallback"}

// onOffer writes the settings of the offer of school a model section on
// the school's key names (config.SchoolOffer.Section) into it: one that
// names none, or sets anything the offer does, is a problem. One that
// names an offer school does not have is Document's to take.
func onOffer(section map[string]any, school config.School, path string, problems *[]string) {
	var extra []string
	for k := range section {
		if !slices.Contains(offerKeys, k) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		*problems = append(*problems, path+"."+k+": set by the school's offer, not by an agent's settings")
	}
	id, _ := section["offer"].(string)
	o, ok := school.OfferOf(id)
	if id == "" || !ok {
		*problems = append(*problems, path+": on the school's key, and names no offer of the school's plan")
		return
	}
	for k, v := range o.Section() {
		section[k] = v
	}
}

// clip is s cut to at most n bytes, for a problem that quotes it.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// keyed gives a model section the owner's key, when its key source, as
// written or else inherited, is own; one on the owner's key when no key is
// stored is a problem. A section that does not name its key source is given
// the one it inherits, written out: merged over the runtime's defaults, it
// would take theirs first (a fallback, that of the defaults' fallback), and
// be paid for otherwise than it is keyed. A section on the school's key is
// left to onOffer. It returns the section's key source.
func keyed(section map[string]any, inherited, key, path string, problems *[]string) string {
	ks, _ := section["key_source"].(string)
	if ks == "" {
		ks = inherited
		if _, written := section["key_source"]; !written {
			section["key_source"] = ks
		}
	}
	switch ks {
	case config.KeyOwn:
		section["key_ref"] = key
		if key == "" {
			*problems = append(*problems, path+": on the owner's key, and no key of the owner's is stored")
		}
	}
	return ks
}

// checkModels holds every model an agent calls, in every course it has
// settings for, as merged and decoded, to the rules of hosted agents: the
// owner's key source and key, key, and no other, an official endpoint over
// https, and no extra headers; or, for the agent's own model alone, an
// offer of school's, with the offer's key.
func checkModels(a *config.Agent, key string, school config.School) error {
	type section struct {
		path string
		m    config.Model
	}
	views := []section{{"agent.model", a.Model}}
	if a.Model.Fallback != nil {
		views = append(views, section{"agent.model.fallback", *a.Model.Fallback})
	}
	ids := make([]string, 0, len(a.Courses))
	for id := range a.Courses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e, err := a.ForCourse(id)
		if err != nil {
			return err
		}
		views = append(views, section{"courses." + id + ".model", e.Model})
		if e.Model.Fallback != nil {
			views = append(views, section{"courses." + id + ".model.fallback", *e.Model.Fallback})
		}
	}
	var problems []string
	for _, v := range views {
		var msg string
		if v.m.KeySource == config.KeySchool {
			// Document writes out every key source it gives, and the
			// offer's settings; this holds the model to them as merged
			// and decoded, whatever the runtime's defaults would give. An
			// offer the site made is held to what an owner's model is:
			// its endpoint is a provider's own.
			o, ok := school.OfferOf(v.m.Offer)
			switch {
			case v.m.Offer == "" || v.m.Offer != a.Model.Offer || !ok:
				msg = "is on the school's key, and not on the agent's offer of the school's plan"
			case v.m.KeyRef != o.KeyRef:
				msg = "has a key that is not its offer's"
			case o.Site:
				msg = checkModel(v.m)
			}
			if msg != "" {
				problems = append(problems, v.path+": "+msg)
			}
			continue
		}
		msg = checkModel(v.m)
		switch {
		case msg != "":
		case v.m.KeySource != config.KeyOwn:
			msg = "is on a key that is neither the owner's nor the school's"
		case v.m.KeyRef != key:
			msg = "has a key that is not the owner's"
		}
		if msg != "" {
			problems = append(problems, v.path+": "+msg)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(slices.Compact(problems), "; "))
	}
	return nil
}

// selfHosted are the providers DetectProvider names for servers of the
// operator's or anyone's own, rather than a provider's official endpoint.
var selfHosted = []string{llm.ProviderOpenAICompat, llm.ProviderOllama, llm.ProviderLMStudio, llm.ProviderVLLM}

// checkModel says what is wrong with one model a hosted agent calls, or "".
func checkModel(m config.Model) string {
	if m.KeyRef == "" {
		return "has no key: store the owner's key for it; a hosted agent's model is never called with the runtime's own credentials, nor at a server that takes none"
	}
	if len(m.Headers) > 0 {
		return "sends extra headers, which a hosted agent may not"
	}
	if m.BaseURL == "" {
		return ""
	}
	u, err := url.Parse(m.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "base_url must be https"
	}
	if !official(m.Adapter, u) {
		return "base_url " + u.Host + " is not an official provider's endpoint for " + m.Adapter + "; a hosted agent calls only those (or leaves base_url out)"
	}
	return ""
}

// official reports whether u is a provider's own endpoint for adapter
// (Core's docs/agent-runtime.md §3.9): behind openai_chat, any provider
// DetectProvider knows by its host; behind the others, their provider's
// hosts alone, since DetectProvider names their provider for any host.
// Where DetectProvider knows a provider by a cloud's domain, under which
// anyone can have a name of their own (Alibaba Cloud's, for Qwen: a bucket,
// a function), only the provider's own hosts under it are official.
func official(adapter string, u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	switch adapter {
	case llm.AdapterOpenAIChat:
		p := llm.DetectProvider(adapter, u.String())
		if p == llm.ProviderQwen {
			return dashScope(host)
		}
		return !slices.Contains(selfHosted, p)
	case llm.AdapterOpenAIResponses:
		return isHost(host, "api.openai.com") || llm.DetectProvider(llm.AdapterOpenAIChat, u.String()) == llm.ProviderAzure
	case llm.AdapterAnthropic:
		return isHost(host, "api.anthropic.com")
	case llm.AdapterGemini:
		return isHost(host, "generativelanguage.googleapis.com")
	case llm.AdapterBedrockConverse:
		return bedrockRuntimeRe.MatchString(host)
	}
	return false
}

// bedrockRuntimeRe is Bedrock's runtime endpoint in a region (§3.9), FIPS
// or not, in China's partition too; not any name of AWS's that holds
// "bedrock", which a bucket or a load balancer of anyone's may.
var bedrockRuntimeRe = regexp.MustCompile(`^bedrock-runtime(-fips)?\.[a-z0-9-]+\.amazonaws\.com(\.cn)?$`)

// dashScope reports whether host is Alibaba Cloud Model Studio's (§3.9):
// dashscope.aliyuncs.com and its regional hosts (dashscope-intl,
// dashscope-us), or a workspace's, <workspace>.<region>.maas.aliyuncs.com.
func dashScope(host string) bool {
	name, ok := strings.CutSuffix(host, ".aliyuncs.com")
	if !ok {
		return false
	}
	if name == "dashscope" || strings.HasPrefix(name, "dashscope-") && !strings.Contains(name, ".") {
		return true
	}
	parts := strings.Split(name, ".")
	return len(parts) == 3 && parts[2] == "maas" && parts[0] != "" && parts[1] != ""
}

// isHost reports whether host is domain or a name under it.
func isHost(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}
