package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
)

// Limits of Core's the configuration must keep to (§2.3).
const (
	// CoreMaxBodyChars is the most characters a message may have.
	CoreMaxBodyChars = 20000
)

// MaxWritesPerAnswer bounds budgets.per_answer.max_writes: an answer that
// changes more than this in the course is not an answer.
const MaxWritesPerAnswer = 100

// Limits of the prompts written in the configuration itself, rather than
// in files: a hosted agent's, kept in the runtime's database.
const (
	// MaxSystemText is the most characters prompt.system_text may have.
	MaxSystemText = 20000
	// MaxPromptAppendText is the most characters a course's
	// prompt_append_text may have.
	MaxPromptAppendText = 4000
	// MaxTruncatedText is the most characters prompt.on_truncated_text
	// may have: a line after the answer, which takes its place in it.
	MaxTruncatedText = 500
)

var (
	idRe       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	toolNameRe = regexp.MustCompile(`^[a-z_]{1,64}$`)
	// A deny entry ending in '*' covers every tool whose name begins with
	// what precedes it (design §4).
	denyRe = regexp.MustCompile(`^[a-z_]{1,64}$|^[a-z_]{0,63}\*$`)
	uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// A BCP 47 tag, near enough: a language of 2 to 8 letters, then
	// subtags of letters and digits.
	langTagRe    = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$`)
	headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	regionRe     = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	// A Core token, invitation or service credential, as Core makes them
	// (ais_, aisinv_ or aissvc_, a 12-character public prefix, then the
	// secret), anywhere, even inside what looks like a reference.
	coreTokenRe = regexp.MustCompile(`ais(?:inv|svc)?_[a-z2-7]{12}_[A-Za-z0-9_-]{16,}`)
)

// HoldsCoreToken reports whether s holds a Core token, invitation or
// service credential, as Core makes them, anywhere in it: what is never
// written in configuration, nor sent to a model's provider as its key.
func HoldsCoreToken(s string) bool { return coreTokenRe.MatchString(s) }

var (
	adapters = []string{llm.AdapterOpenAIChat, llm.AdapterOpenAIResponses, llm.AdapterAnthropic, llm.AdapterGemini, llm.AdapterBedrockConverse}
	// providers are the names llm.DetectProvider gives, which an agent may
	// set outright.
	providers = []string{
		llm.ProviderOpenAI, llm.ProviderAzure, llm.ProviderAnthropic, llm.ProviderGemini, llm.ProviderBedrock,
		llm.ProviderDeepSeek, llm.ProviderQwen, llm.ProviderMoonshot, llm.ProviderGLM, llm.ProviderOpenRouter,
		llm.ProviderOllama, llm.ProviderLMStudio, llm.ProviderVLLM, llm.ProviderOpenAICompat,
	}
	efforts = []string{"", "minimal", "low", "medium", "high"}
	// Headers that carry credentials, which come from key_ref only.
	credentialHeaders = []string{"authorization", "proxy-authorization", "x-api-key", "api-key", "x-goog-api-key", "cookie"}
)

// Validate checks the whole configuration and reports every problem at
// once, each naming its file, agent and field. allowlist is
// CORE_BASE_URL_ALLOWLIST: origins (https://lms.example.edu) or host
// patterns (*.example.edu) that core.base_url must be within; empty allows
// any. Load calls it with none.
func (c *Config) Validate(allowlist []string) error {
	var errs []error
	origins, bad := parseAllowlist(allowlist)
	for _, msg := range bad {
		errs = append(errs, &Problem{Path: "CORE_BASE_URL_ALLOWLIST", Msg: msg})
	}
	errs = append(errs, c.validateRuntime()...)
	ids := map[string]string{}
	for _, a := range c.Agents {
		is := &issues{prefix: "agent."}
		if !idRe.MatchString(a.ID) {
			is.add("id", "required: letters, digits, '_' and '-', at most 64")
		}
		validateAgent(a, &c.Runtime, origins, is)
		errs = append(errs, problems(is.list, a.File, a.ID)...)
		if prev, dup := ids[a.ID]; dup && a.ID != "" {
			errs = append(errs, &Problem{File: a.File, Agent: a.ID, Path: "agent.id", Msg: "another agent has this id, in " + prev})
		} else {
			ids[a.ID] = a.File
		}
		errs = append(errs, c.validateCourses(a, is.list)...)
	}
	return errors.Join(errs...)
}

// validateCourses checks each course's settings of a, reporting what is
// wrong with a course and not already with the agent.
func (c *Config) validateCourses(a *Agent, agentIssues []issue) []error {
	known := map[string]bool{}
	for _, is := range agentIssues {
		known[strings.TrimPrefix(is.path, "agent.")+"\x00"+is.msg] = true
	}
	keys := make([]string, 0, len(a.Courses))
	for k := range a.Courses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var errs []error
	seen := map[string]string{}
	for _, k := range keys {
		p := join("courses", k)
		if !uuidRe.MatchString(k) {
			errs = append(errs, &Problem{File: a.File, Agent: a.ID, Path: p, Msg: "a course is named by its id, a UUID"})
			continue
		}
		if prev, dup := seen[strings.ToLower(k)]; dup {
			errs = append(errs, &Problem{File: a.File, Agent: a.ID, Path: p, Msg: "the same course as courses." + prev})
			continue
		}
		seen[strings.ToLower(k)] = k
		e, list, err := a.forCourse(k)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		is := &issues{prefix: p + ".", list: list}
		validateRuntimeRules(&e.Agent, &c.Runtime, is)
		for _, i := range is.list {
			if !known[strings.TrimPrefix(i.path, p+".")+"\x00"+i.msg] {
				errs = append(errs, &Problem{File: a.File, Agent: a.ID, Path: i.path, Msg: i.msg})
			}
		}
	}
	return errs
}

// validateAgent checks one agent's settings (or one course's) but its id,
// adding what is wrong to is. With rt, it also checks them against the
// runtime's tenants and model lists; with origins, core.base_url against
// them.
func validateAgent(a *Agent, rt *Runtime, origins []origin, is *issues) {
	if strings.TrimSpace(a.DisplayName) == "" {
		is.add("display_name", "required")
	}
	if a.TenantID != "" && !idRe.MatchString(a.TenantID) {
		is.add("tenant_id", "letters, digits, '_' and '-', at most 64")
	}
	checkCore(is, a.Core, origins)
	checkModel(is, "model", &a.Model)
	if fb := a.Model.Fallback; fb != nil {
		checkModel(is, "model.fallback", fb)
		if fb.Fallback != nil {
			is.add("model.fallback.fallback", "a fallback has no fallback of its own")
		}
		if fb.Offer != "" {
			is.add("model.fallback.offer", "a fallback is not an offer of the school's plan: the plan's offer is the model, the owner's own key its fallback")
		}
	}
	checkSchoolKey(is, a)
	checkPrompt(is, a)
	checkTools(is, a.Tools)
	checkAnswer(is, a.Answer)
	checkBudgets(is, a.Budgets)
	checkPolling(is, a.Polling)
	if a.Memory.RetentionDaysAfterRemoval < 0 {
		is.add("memory.retention_days_after_removal", "must be zero or more")
	}
	if rt != nil {
		validateRuntimeRules(a, rt, is)
	}
}

func checkCore(is *issues, c Core, origins []origin) {
	if c.BaseURL == "" {
		is.add("core.base_url", "required: Core's address, such as https://lms.example.edu")
	} else if u, msg := parseCoreURL(c.BaseURL); msg != "" {
		is.add("core.base_url", "%s", msg)
	} else if len(origins) > 0 && !allowed(u, origins) {
		is.add("core.base_url", "%s is not within CORE_BASE_URL_ALLOWLIST", u.Host)
	}
	if c.Transport != TransportMCP && c.Transport != TransportREST {
		is.add("core.transport", "%q is not mcp or rest", redact.String(c.Transport))
	}
	if !slices.Contains(mcpRevisions, c.MCPProtocol) {
		is.add("core.mcp_protocol", "%q is not a revision Core takes (%s)", redact.String(c.MCPProtocol), strings.Join(mcpRevisions, ", "))
	}
	checkRef(is, "core.token_ref", c.TokenRef, "token", true)
}

// parseCoreURL checks Core's base URL: absolute, https (http only for this
// machine), with no user information, query or fragment.
func parseCoreURL(s string) (*url.URL, string) {
	u, err := url.Parse(s)
	switch {
	case err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "":
		return nil, "must be an absolute URL such as https://lms.example.edu"
	case u.User != nil:
		return nil, "must hold no user name or password: the agent's token is its only credential"
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, "must have no query or fragment"
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackName(u.Hostname()):
	default:
		return nil, "must be https (http only for localhost, 127.0.0.1 or ::1)"
	}
	return u, ""
}

func isLoopbackName(host string) bool {
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
}

// checkModel checks a model's settings, the model's own or its fallback's.
func checkModel(is *issues, path string, m *Model) {
	if !slices.Contains(adapters, m.Adapter) {
		is.add(path+".adapter", "%q is not one of %s", redact.String(m.Adapter), strings.Join(adapters, ", "))
	}
	if strings.TrimSpace(m.Model) == "" {
		is.add(path+".model", "required: the provider's model id (Azure: the deployment name)")
	}
	if m.Provider != "" && !slices.Contains(providers, m.Provider) {
		is.add(path+".provider", "%q is not one of %s", redact.String(m.Provider), strings.Join(providers, ", "))
	}
	if m.BaseURL != "" {
		if msg := checkEndpoint(m.BaseURL); msg != "" {
			is.add(path+".base_url", "%s", msg)
		}
	}
	if m.Region != "" && !regionRe.MatchString(m.Region) {
		is.add(path+".region", "is not an AWS region such as us-east-1")
	}
	checkRef(is, path+".key_ref", m.KeyRef, "key", !keyless(m))
	if m.KeySource != KeySchool && m.KeySource != KeyOwn {
		is.add(path+".key_source", "%q is not school or own", redact.String(m.KeySource))
	}
	if m.Offer != "" && m.KeySource != KeySchool {
		is.add(path+".offer", "an offer of the school's plan is on the school's key (key_source: school)")
	}
	if m.Params.MaxOutputTokens <= 0 {
		is.add(path+".params.max_output_tokens", "must be one or more")
	}
	if t := m.Params.Temperature; t != nil && (!finite(*t) || *t < 0 || *t > 2) {
		is.add(path+".params.temperature", "must be from 0 to 2")
	}
	if p := m.Params.TopP; p != nil && (!finite(*p) || *p <= 0 || *p > 1) {
		is.add(path+".params.top_p", "must be more than 0, and at most 1")
	}
	if !slices.Contains(efforts, m.Reasoning.Effort) {
		is.add(path+".reasoning.effort", "%q is not minimal, low, medium or high", redact.String(m.Reasoning.Effort))
	}
	if d := m.Capabilities.SchemaDialect; d != "" && !toolschema.Dialect(d).Valid() {
		is.add(path+".capabilities.schema_dialect", "%q is not a schema dialect", redact.String(d))
	}
	names := make([]string, 0, len(m.Headers))
	for k := range m.Headers {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		hp := path + ".headers." + redact.String(k)
		switch v := m.Headers[k]; {
		case !headerNameRe.MatchString(k):
			is.add(hp, "is not a header name")
		case slices.Contains(credentialHeaders, strings.ToLower(k)):
			is.add(hp, "credentials are never written in configuration: the key comes from key_ref")
		case redact.String(v) != v:
			is.add(hp, "holds what looks like a credential; credentials are never written in configuration")
		case strings.ContainsAny(v, "\r\n"):
			is.add(hp, "a header value is one line")
		}
	}
}

// checkEndpoint checks a model API's base URL.
func checkEndpoint(s string) string {
	u, err := url.Parse(s)
	switch {
	case err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return "must be an absolute http or https URL"
	case u.User != nil:
		return "must hold no user name or password: the key comes from key_ref"
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "must have no query or fragment"
	}
	return ""
}

// keyless reports whether a model needs no key: Bedrock, signed with the
// AWS credential chain, and servers on the operator's own machines.
func keyless(m *Model) bool {
	if m.Adapter == llm.AdapterBedrockConverse {
		return true
	}
	switch m.EffectiveProvider() {
	case llm.ProviderOllama, llm.ProviderLMStudio, llm.ProviderVLLM:
		return true
	}
	u, err := url.Parse(m.BaseURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	if isLoopbackName(u.Hostname()) {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

// checkRef checks a reference to a secret. A token or key written where
// its reference belongs is refused without being repeated.
func checkRef(is *issues, path, v, what string, required bool) {
	switch {
	case v == "":
		if required {
			is.add(path, "required: give the %s's reference (secret://…, env://NAME or file://…)", what)
		}
	case coreTokenRe.MatchString(v), secrets.Check(v) != nil && redact.String(v) != v:
		is.add(path, "%ss are never written in configuration: keep the %s in the secret store and give its reference (secret://…, env://NAME or file://…)", what, what)
	default:
		if err := secrets.Check(v); errors.Is(err, secrets.ErrNotReference) {
			is.add(path, "is not a reference: give secret://…, env://NAME or file://…")
		} else if err != nil {
			is.add(path, "%v", err)
		}
	}
}

// checkSchoolKey holds a model on the school's key to §5.2: a tenant, and
// daily quotas per agent and per asker, both. A model on an offer of the
// school's plan (runtime.school) is held by the plan's quotas, per owner
// (its tenant) and per asker, and needs its tenant alone.
func checkSchoolKey(is *issues, a *Agent) {
	plan := a.Model.KeySource == KeySchool && a.Model.Offer != ""
	other := a.Model.KeySource == KeySchool && a.Model.Offer == "" || a.Model.Fallback != nil && a.Model.Fallback.KeySource == KeySchool
	if !plan && !other {
		return
	}
	if a.TenantID == "" {
		is.add("tenant_id", "required on the school's key: its quotas are the tenant's")
	}
	if !other {
		return
	}
	if !a.Budgets.PerAgentDay.set() {
		is.add("budgets.per_agent_day", "required on the school's key: answers, usd or both")
	}
	if !a.Budgets.PerAskerDay.set() {
		is.add("budgets.per_asker_day", "required on the school's key: answers, usd or both")
	}
}

// validateRuntimeRules checks an agent against the runtime's settings: a
// school key's tenant, and the school's model lists.
func validateRuntimeRules(a *Agent, rt *Runtime, is *issues) {
	models := []struct {
		path string
		m    *Model
	}{{"model", &a.Model}}
	if a.Model.Fallback != nil {
		models = append(models, struct {
			path string
			m    *Model
		}{"model.fallback", a.Model.Fallback})
	}
	tenantChecked := false
	for _, x := range models {
		if !slices.Contains(adapters, x.m.Adapter) {
			continue // reported already; its provider means nothing
		}
		triple := x.m.Adapter + ":" + x.m.EffectiveProvider() + ":" + x.m.Model
		if p, ok := matchModel(rt.DeniedModels, triple); ok {
			is.add(x.path, "%s is denied by runtime.denied_models (%s)", triple, p)
		}
		if x.m.KeySource != KeySchool {
			continue
		}
		if len(rt.AllowedModels) > 0 {
			if _, ok := matchModel(rt.AllowedModels, triple); !ok {
				is.add(x.path, "%s is not in runtime.allowed_models, the models the school's key may use", triple)
			}
		}
		if x.m.Offer != "" {
			// The plan's quotas hold it, not its tenant's.
			checkOfferSection(is, x.path, x.m, rt.School)
			continue
		}
		if !tenantChecked && a.TenantID != "" {
			tenantChecked = true
			if t, ok := rt.Tenants[a.TenantID]; !ok {
				is.add("tenant_id", "%q is not in runtime.tenants", a.TenantID)
			} else if !t.PerDay.set() {
				is.add("tenant_id", "runtime.tenants.%s has no per_day quota, which the school's key needs", a.TenantID)
			}
		}
	}
}

// checkOfferSection holds a model section that names an offer of the
// school's plan to it: the offer is there, and the section calls what the
// offer does, with its key, as the registry writes it.
func checkOfferSection(is *issues, path string, m *Model, school School) {
	o, ok := school.OfferOf(m.Offer)
	if !ok {
		is.add(path+".offer", "%q is not an offer of runtime.school: the school no longer offers it", redact.String(m.Offer))
		return
	}
	om := o.AsModel()
	if m.Adapter != om.Adapter || m.Model != om.Model || m.EffectiveProvider() != om.EffectiveProvider() || m.BaseURL != om.BaseURL ||
		m.Region != om.Region || m.KeyRef != om.KeyRef {
		is.add(path, "is not runtime.school's offer %s: a model on an offer calls what the offer does, with its key", o.ID)
	}
}

// DeniedModel reports whether rt's runtime.denied_models denies model of
// provider behind adapter, even on an owner's own key, and the pattern
// that does.
func DeniedModel(rt Runtime, adapter, provider, model string) (pattern string, denied bool) {
	return matchModel(rt.DeniedModels, adapter+":"+provider+":"+model)
}

// matchModel finds the first of patterns that "adapter:provider:model"
// matches, part by part.
func matchModel(patterns []string, triple string) (string, bool) {
	t := strings.SplitN(triple, ":", 3)
	for _, p := range patterns {
		parts := strings.SplitN(p, ":", 3)
		if len(parts) == 3 && pricing.Match(parts[0], t[0]) && pricing.Match(parts[1], t[1]) && pricing.Match(parts[2], t[2]) {
			return p, true
		}
	}
	return "", false
}

func checkPrompt(is *issues, a *Agent) {
	p := a.Prompt
	switch lang := p.AnswerLanguage; {
	case lang == LanguageOpener:
	case strings.HasPrefix(lang, LanguageFixed) && langTagRe.MatchString(strings.TrimPrefix(lang, LanguageFixed)):
	default:
		is.add("prompt.answer_language", "%q is not opener or fixed:<a BCP 47 language tag, such as en or zh-Hant>", redact.String(lang))
	}
	maxBody := a.Answer.MaxBodyChars
	if maxBody < 1 || maxBody > CoreMaxBodyChars {
		maxBody = CoreMaxBodyChars
	}
	for _, t := range []struct{ path, text string }{
		{"prompt.on_refusal_text", p.OnRefusalText}, {"prompt.on_budget_text", p.OnBudgetText}, {"prompt.on_quota_text", p.OnQuotaText},
	} {
		switch n := utf8.RuneCountInString(t.text); {
		case strings.TrimSpace(t.text) == "":
			is.add(t.path, "required: it is posted as an answer")
		case n > maxBody:
			is.add(t.path, "is %d characters; an answer has at most %d (answer.max_body_chars)", n, maxBody)
		}
	}
	switch n := utf8.RuneCountInString(strings.TrimSpace(p.OnTruncatedText)); {
	case n == 0:
		is.add("prompt.on_truncated_text", "required: it is posted after an answer cut short, so that none ends mid-sentence unexplained")
	case n > MaxTruncatedText:
		is.add("prompt.on_truncated_text", "is %d characters; at most %d: it is a line after the answer, within answer.max_body_chars", n, MaxTruncatedText)
	case n > maxBody:
		is.add("prompt.on_truncated_text", "is %d characters; an answer has at most %d (answer.max_body_chars)", n, maxBody)
	}
	checkText(is, "prompt.system", p.SystemRef, p.SystemText, MaxSystemText)
	if p.SystemRef != "" {
		checkFileRef(is, "prompt.system_ref", p.SystemRef, a.Dir)
	}
}

// checkText checks a prompt that may be a file (<name>_ref) or its text
// (<name>_text), but not both: the text, when it is given, holds more than
// whitespace and at most max characters.
func checkText(is *issues, name, ref, text string, maxChars int) {
	switch n := utf8.RuneCountInString(text); {
	case text == "":
	case ref != "":
		is.add(name+"_text", "give %s_ref or %s_text, not both", name[strings.LastIndexByte(name, '.')+1:], name[strings.LastIndexByte(name, '.')+1:])
	case strings.TrimSpace(text) == "":
		is.add(name+"_text", "holds no text")
	case n > maxChars:
		is.add(name+"_text", "is %d characters; at most %d", n, maxChars)
	}
}

func checkTools(is *issues, t Tools) {
	if t.Mode != ToolsDerived && t.Mode != ToolsNone {
		is.add("tools.mode", "%q is not derived or none", redact.String(t.Mode))
	}
	for i, n := range t.Allow {
		if !toolNameRe.MatchString(n) {
			is.add(fmt.Sprintf("tools.allow[%d]", i), "%q is not a tool name: lower-case letters and '_', as Core names them", redact.String(n))
		}
	}
	for i, n := range t.Deny {
		if !denyRe.MatchString(n) {
			is.add(fmt.Sprintf("tools.deny[%d]", i), "%q is not a tool name (lower-case letters and '_', as Core names them) or one's beginning followed by '*'", redact.String(n))
		}
	}
	if t.MaxParallelTools < 1 {
		is.add("tools.max_parallel_tools", "must be one or more")
	}
}

func checkAnswer(is *issues, a Answer) {
	if a.MaxAttempts < 1 || a.MaxAttempts > 10 {
		is.add("answer.max_attempts", "must be from 1 to 10")
	}
	// close is decoded as skip (decodeAgent), and taken as skip from an
	// Agent built in code.
	if a.OnAttemptsExhausted != OnExhaustedSkip && a.OnAttemptsExhausted != OnExhaustedClose {
		is.add("answer.on_attempts_exhausted", "%q is not skip, which holds a message whose attempts are spent back until the next day",
			redact.String(a.OnAttemptsExhausted))
	}
	if a.OnQuotaExhausted != OnQuotaCanned && a.OnQuotaExhausted != OnQuotaSilent {
		is.add("answer.on_quota_exhausted", "%q is not canned or silent", redact.String(a.OnQuotaExhausted))
	}
	if a.MaxBodyChars < 100 || a.MaxBodyChars > CoreMaxBodyChars {
		is.add("answer.max_body_chars", "must be from 100 to %d, Core's limit", CoreMaxBodyChars)
	}
	if a.HistoryMessages < 1 || a.HistoryMessages > 200 {
		is.add("answer.history_messages", "must be from 1 to 200, as many as Core gives at once")
	}
	if a.MaxConcurrent < 1 {
		is.add("answer.max_concurrent", "must be one or more")
	}
	if a.MaxConcurrentPerCourse < 1 {
		is.add("answer.max_concurrent_per_course", "must be one or more")
	}
}

func checkBudgets(is *issues, b Budgets) {
	pa := b.PerAnswer
	for _, n := range []struct {
		path string
		v    int64
	}{
		{"budgets.per_answer.turns", int64(pa.Turns)}, {"budgets.per_answer.tool_calls", int64(pa.ToolCalls)},
		{"budgets.per_answer.input_tokens", pa.InputTokens}, {"budgets.per_answer.output_tokens", pa.OutputTokens},
	} {
		if n.v < 1 {
			is.add(n.path, "must be one or more")
		}
	}
	if pa.MaxWrites < 0 || pa.MaxWrites > MaxWritesPerAnswer {
		is.add("budgets.per_answer.max_writes", "must be from 0 to %d", MaxWritesPerAnswer)
	}
	if !finite(pa.WallClockS) || pa.WallClockS <= 0 {
		is.add("budgets.per_answer.wall_clock_s", "must be more than 0")
	}
	checkQuota(is, "budgets.per_agent_day", b.PerAgentDay)
	checkQuota(is, "budgets.per_asker_day", b.PerAskerDay)
}

func checkQuota(is *issues, path string, q Quota) {
	if q.USD != nil && (!finite(*q.USD) || *q.USD <= 0) {
		is.add(path+".usd", "must be more than 0")
	}
	if q.Answers != nil && *q.Answers < 1 {
		is.add(path+".answers", "must be one or more")
	}
}

// set reports whether the quota limits anything.
func (q Quota) set() bool { return q.USD != nil || q.Answers != nil }

func checkPolling(is *issues, p Polling) {
	for _, f := range []struct {
		path string
		v    float64
	}{
		{"polling.inbox_hot_s", p.InboxHotS}, {"polling.inbox_idle_s", p.InboxIdleS}, {"polling.inbox_max_s", p.InboxMaxS},
		{"polling.events_s", p.EventsS}, {"polling.memberships_s", p.MembershipsS},
	} {
		if !finite(f.v) || f.v <= 0 {
			is.add(f.path, "must be more than 0")
		}
	}
	if p.InboxHotS > p.InboxIdleS || p.InboxIdleS > p.InboxMaxS {
		is.add("polling", "must have inbox_hot_s ≤ inbox_idle_s ≤ inbox_max_s")
	}
	if !finite(p.HotWindowS) || p.HotWindowS < 0 {
		is.add("polling.hot_window_s", "must be zero or more")
	}
	if !finite(p.Jitter) || p.Jitter < 0 || p.Jitter > 0.9 {
		is.add("polling.jitter", "must be from 0 to 0.9")
	}
	if !finite(p.MaxRateShare) || p.MaxRateShare <= 0 || p.MaxRateShare > 1 {
		is.add("polling.max_rate_share", "must be more than 0, and at most 1")
	}
	if p.AssumedCoreRatePerMin < 1 {
		is.add("polling.assumed_core_rate_per_min", "must be one or more")
	}
	if p.AssumedCoreBurst < 1 {
		is.add("polling.assumed_core_burst", "must be one or more")
	}
	if !finite(p.LongPollWaitS) || p.LongPollWaitS < 0 || p.LongPollWaitS > MaxLongPollWaitS {
		is.add("polling.long_poll_wait_s", "must be from 0 to 25")
	}
	if p.LongPollMax < 0 {
		is.add("polling.long_poll_max", "must be zero or more")
	}
}

// MaxLongPollWaitS is the longest Core lets a call wait for news (wait_s).
const MaxLongPollWaitS = 25

// validateRuntime checks the runtime's own settings.
func (c *Config) validateRuntime() []error {
	rt := &c.Runtime
	var errs []error
	add := func(path, format string, args ...any) {
		errs = append(errs, &Problem{File: rt.File, Path: path, Msg: fmt.Sprintf(format, args...)})
	}
	names := make([]string, 0, len(rt.Tenants))
	for k := range rt.Tenants {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		p := "runtime.tenants." + k
		if !idRe.MatchString(k) {
			add(p, "a tenant id is letters, digits, '_' and '-', at most 64")
		}
		is := &issues{}
		checkQuota(is, p+".per_day", rt.Tenants[k].PerDay)
		for _, i := range is.list {
			add(i.path, "%s", i.msg)
		}
	}
	if rt.PricesRef != "" {
		if _, err := pricing.Load(c.PricesPath()); err != nil {
			add("runtime.prices_ref", "%v", err)
		}
	}
	is := &issues{}
	checkSchool(is, rt)
	for _, i := range is.list {
		add(i.path, "%s", i.msg)
	}
	for _, l := range []struct {
		path     string
		patterns []string
	}{{"runtime.allowed_models", rt.AllowedModels}, {"runtime.denied_models", rt.DeniedModels}} {
		for i, p := range l.patterns {
			if msg := checkModelPattern(p); msg != "" {
				add(fmt.Sprintf("%s[%d]", l.path, i), "%s", msg)
			}
		}
	}
	return errs
}

// SchoolKeyPrefix begins the reference of every key of the school's
// offers: the operator's secret store, never a sealed secret of an
// owner's.
const SchoolKeyPrefix = "secret://school/keys/"

// MaxOfferLabel bounds an offer's label, in characters.
const MaxOfferLabel = 80

// MaxSchoolQuotaText bounds runtime.school.on_quota_text, in characters.
const MaxSchoolQuotaText = 1000

// checkSchool checks the school's plan: each offer a model section on the
// school's key, whose key is under SchoolKeyPrefix, of a model the
// runtime's lists allow, with an id and label of its own; and its quotas.
func checkSchool(is *issues, rt *Runtime) {
	sc := rt.School
	ids := map[string]int{}
	for i, o := range sc.Offers {
		p := fmt.Sprintf("runtime.school.offers[%d]", i)
		switch prev, dup := ids[o.ID]; {
		case !idRe.MatchString(o.ID):
			is.add(p+".id", "required: letters, digits, '_' and '-', at most 64")
		case dup:
			is.add(p+".id", "offers[%d] has this id too", prev)
		default:
			ids[o.ID] = i
		}
		switch n := utf8.RuneCountInString(o.Label); {
		case strings.TrimSpace(o.Label) == "":
			is.add(p+".label", "required: what people are shown")
		case n > MaxOfferLabel || strings.ContainsAny(o.Label, "\r\n"):
			is.add(p+".label", "one line of at most %d characters", MaxOfferLabel)
		}
		m := o.AsModel()
		if m.Params.MaxOutputTokens == 0 {
			m.Params.MaxOutputTokens = 1 // unset: the agent's defaults give it
		}
		checkModel(is, p, &m)
		if o.KeyRef != "" && !strings.HasPrefix(o.KeyRef, SchoolKeyPrefix) && secrets.Check(o.KeyRef) == nil {
			is.add(p+".key_ref", "the school's keys are kept under %s<name>", SchoolKeyPrefix)
		}
		if !slices.Contains(adapters, o.Adapter) {
			continue
		}
		triple := o.Adapter + ":" + m.EffectiveProvider() + ":" + o.Model
		if pat, ok := matchModel(rt.DeniedModels, triple); ok {
			is.add(p, "%s is denied by runtime.denied_models (%s)", triple, pat)
		}
		if len(rt.AllowedModels) > 0 {
			if _, ok := matchModel(rt.AllowedModels, triple); !ok {
				is.add(p, "%s is not in runtime.allowed_models, the models the school's key may use", triple)
			}
		}
	}
	checkQuota(is, "runtime.school.per_owner_day", sc.PerOwnerDay)
	checkQuota(is, "runtime.school.per_asker_day", sc.PerAskerDay)
	checkQuota(is, "runtime.school.per_day", sc.PerDay)
	if t := sc.OnQuotaText; t != "" {
		switch n := utf8.RuneCountInString(t); {
		case strings.TrimSpace(t) == "":
			is.add("runtime.school.on_quota_text", "holds no text; leave it out for the built-in notice")
		case n > MaxSchoolQuotaText:
			is.add("runtime.school.on_quota_text", "is %d characters; at most %d", n, MaxSchoolQuotaText)
		}
	}
}

// checkModelPattern checks an "adapter:provider:model" pattern, where * is
// any text and the model may hold ':'.
func checkModelPattern(p string) string {
	parts := strings.SplitN(p, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return fmt.Sprintf("%q is not adapter:provider:model, such as anthropic:anthropic:claude-* or *:ollama:*", p)
	}
	if !strings.Contains(parts[0], "*") && !slices.Contains(adapters, parts[0]) {
		return fmt.Sprintf("%q is not one of %s, or a pattern", parts[0], strings.Join(adapters, ", "))
	}
	if !strings.Contains(parts[1], "*") && !slices.Contains(providers, parts[1]) {
		return fmt.Sprintf("%q is not one of %s, or a pattern", parts[1], strings.Join(providers, ", "))
	}
	return ""
}

// origin is one entry of CORE_BASE_URL_ALLOWLIST: an origin, or a host
// pattern.
type origin struct {
	scheme string // "" for a host pattern: any scheme Core's URL may have
	host   string // lower case; for a wildcard, the suffix after "*."
	port   string // "" for any port
	wild   bool
}

// parseAllowlist reads CORE_BASE_URL_ALLOWLIST's entries: origins
// (https://lms.example.edu, with a port if not the default), host
// patterns (*.example.edu, matching every name under example.edu but not
// example.edu itself), or plain hosts (lms.example.edu).
func parseAllowlist(entries []string) ([]origin, []string) {
	var out []origin
	var bad []string
	for i, e := range entries {
		o, ok := parseOrigin(strings.TrimSpace(e))
		if !ok {
			bad = append(bad, fmt.Sprintf("entry %d, %q, is not an origin such as https://lms.example.edu or a host pattern such as *.example.edu", i+1, redact.String(e)))
			continue
		}
		out = append(out, o)
	}
	return out, bad
}

var hostRe = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$|^\[[0-9A-Fa-f:.]+\]$`)

func parseOrigin(e string) (origin, bool) {
	var o origin
	rest := e
	if scheme, r, ok := strings.Cut(e, "://"); ok {
		scheme = strings.ToLower(scheme)
		if scheme != "https" && scheme != "http" {
			return origin{}, false
		}
		o.scheme, rest = scheme, strings.TrimSuffix(r, "/")
	}
	host, port := rest, ""
	if i := strings.LastIndexByte(rest, ':'); i >= 0 && !strings.HasSuffix(rest, "]") {
		host, port = rest[:i], rest[i+1:]
		if port == "" || strings.Trim(port, "0123456789") != "" {
			return origin{}, false
		}
	}
	if !hostRe.MatchString(host) {
		return origin{}, false
	}
	o.host = strings.ToLower(strings.Trim(host, "[]"))
	if strings.HasPrefix(o.host, "*.") {
		o.wild, o.host = true, o.host[2:]
	}
	if o.scheme != "" && port == "" && !o.wild {
		port = defaultPort(o.scheme)
	}
	o.port = port
	return o, true
}

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

// allowed reports whether Core's URL is within one of the origins.
func allowed(u *url.URL, origins []origin) bool {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	for _, o := range origins {
		if o.scheme != "" && o.scheme != u.Scheme {
			continue
		}
		if o.port != "" && o.port != port {
			continue
		}
		if o.wild && strings.HasSuffix(host, "."+o.host) || !o.wild && host == o.host {
			return true
		}
	}
	return false
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
