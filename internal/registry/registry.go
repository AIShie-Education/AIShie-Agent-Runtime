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
//     defaults. The school's key is not offered to hosted agents yet: that
//     waits on the school's model offers and the owners' quotas.
//   - Every model it calls has a key of its own: a model without one would
//     be called with the runtime's own credentials (Bedrock's, from the
//     host) or at a server that takes none.
//   - A model's base_url is empty (the adapter's own) or an official
//     provider's, over https, and it sends no extra headers (D9): no
//     request goes to an address the owner chose.
//
// And gives them one default YAML does not: a hosted agent's owner is
// known, Core naming them, so its model is offered the writes its seats'
// perms allow, in the conversations its owner opens, unless its owner
// turns tools.writes off (docs/design.md §4). A YAML agent's owner is
// whoever its operator says, and it has writes only when its
// configuration turns them on.
//
// The agent document's shape holds the key pool of the product owner's D8
// as it stands: model is the school's offer and model.fallback the owner's
// own-key model, whose key is hosted_agent.key_secret_id.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
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

// Reader is what the registry reads: the store's hosted agents.
type Reader interface {
	RegistryRev(ctx context.Context) (int64, error)
	HostedAgents(ctx context.Context) ([]store.HostedAgent, error)
	ListHostedCourses(ctx context.Context) ([]store.HostedCourse, error)
}

// SourceName is what a hosted agent's problems name in place of a file.
func SourceName(agentID string) string { return "registry:" + agentID }

// Build is the configuration the runtime runs: yaml's runtime settings and
// agents, then every hosted agent that passes. One that does not, or whose
// id is a YAML agent's (which wins), is in the result's Rejected, with why.
// The revision returned is the registry's as it stood before its agents
// were read, so that a change made meanwhile is seen as one. yaml is not
// changed.
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
		src, err := Document(a, courses[a.ID], o.CoreBaseURL, defaultKeySource(yaml))
		if err != nil {
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
		if err := checkModels(a, key); err != nil {
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
// settings, and its models. It reads and writes nothing: the API tries a
// change with it before it writes one. The error is why it would not run,
// as a Rejection's Detail words it.
func Check(_ context.Context, yaml *config.Config, row store.HostedAgent, courses []store.HostedCourse, o Options) error {
	for _, a := range yaml.Agents {
		if a.ID == row.ID {
			return errors.New("a YAML agent has this id, and the operator's configuration wins")
		}
	}
	if err := checkCoreBaseURL(o); err != nil {
		return err
	}
	src, err := Document(row, courses, o.CoreBaseURL, defaultKeySource(yaml))
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
	return checkModels(agents[0], key)
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
// Core at coreBaseURL, with its token as sealed://<token_secret_id>; and
// the owner's key, sealed://<key_secret_id>, on each model section whose
// key source, as written or as it inherits it from defaultKeySource (and
// then written out), is own. Settings that set any of those themselves,
// refer to a file or a secret, or put a model on the school's key, are
// refused.
func Document(a store.HostedAgent, courses []store.HostedCourse, coreBaseURL, defaultKeySource string) (config.Source, error) {
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
	if fb, ok := model["fallback"].(map[string]any); ok {
		keyed(fb, ks, key, "agent.model.fallback", &problems)
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
				if fb, ok := m["fallback"].(map[string]any); ok {
					keyed(fb, cks, key, path+".model.fallback", &problems)
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

// keyed gives a model section the owner's key, when its key source, as
// written or else inherited, is own; one on the owner's key when no key is
// stored, or on the school's, is a problem. A section that does not name its
// key source is given the one it inherits, written out: merged over the
// runtime's defaults, it would take theirs first (a fallback, that of the
// defaults' fallback), and be paid for otherwise than it is keyed. It
// returns the section's key source.
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
	case config.KeySchool:
		*problems = append(*problems, path+": the school's key is not offered to hosted agents yet; use the owner's own key")
	}
	return ks
}

// checkModels holds every model an agent calls, in every course it has
// settings for, as merged and decoded, to the rules of hosted agents: the
// owner's key source and key, key, and no other, an official endpoint over
// https, and no extra headers.
func checkModels(a *config.Agent, key string) error {
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
		msg := checkModel(v.m)
		switch {
		case msg != "":
		case v.m.KeySource != config.KeyOwn:
			// Document writes out every key source it gives; this holds
			// the model to it as merged and decoded, whatever the
			// runtime's defaults would give.
			msg = "is on the school's key, which is not offered to hosted agents yet"
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
