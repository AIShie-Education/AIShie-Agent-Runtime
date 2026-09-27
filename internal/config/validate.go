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

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// Limits of Core's the configuration must keep to (§2.3).
const (
	// CoreMaxBodyChars is the most characters a message may have.
	CoreMaxBodyChars = 20000
	// CoreMaxCloseReason is the most characters a closing reason may have.
	CoreMaxCloseReason = 500
)

var (
	idRe       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	toolNameRe = regexp.MustCompile(`^[a-z_]{1,64}$`)
	uuidRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	// A BCP 47 tag, near enough: a language of 2 to 8 letters, then
	// subtags of letters and digits.
	langTagRe    = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$`)
	headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	regionRe     = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	// A Core token or invitation, as Core makes them (ais_ or aisinv_, a
	// 12-character public prefix, then the secret), anywhere, even inside
	// what looks like a reference.
	coreTokenRe = regexp.MustCompile(`ais(?:inv)?_[a-z2-7]{12}_[A-Za-z0-9_-]{16,}`)
)

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
		p := "courses." + k
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
// daily quotas per agent and per asker, both.
func checkSchoolKey(is *issues, a *Agent) {
	school := a.Model.KeySource == KeySchool || a.Model.Fallback != nil && a.Model.Fallback.KeySource == KeySchool
	if !school {
		return
	}
	if a.TenantID == "" {
		is.add("tenant_id", "required on the school's key: its quotas are the tenant's")
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
	switch n := utf8.RuneCountInString(strings.TrimSpace(p.CloseReasonText)); {
	case n == 0:
		is.add("prompt.close_reason_text", "required: it is the reason given when a conversation is closed")
	case n > CoreMaxCloseReason:
		is.add("prompt.close_reason_text", "is %d characters; Core takes at most %d", n, CoreMaxCloseReason)
	}
	if p.SystemRef != "" {
		checkFileRef(is, "prompt.system_ref", p.SystemRef, a.Dir)
	}
}

func checkTools(is *issues, t Tools) {
	if t.Mode != ToolsDerived && t.Mode != ToolsNone {
		is.add("tools.mode", "%q is not derived or none", redact.String(t.Mode))
	}
	for _, l := range []struct {
		path  string
		names []string
	}{{"tools.allow", t.Allow}, {"tools.deny", t.Deny}} {
		for i, n := range l.names {
			if !toolNameRe.MatchString(n) {
				is.add(fmt.Sprintf("%s[%d]", l.path, i), "%q is not a tool name: lower-case letters and '_', as Core names them", redact.String(n))
			}
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
	if a.OnAttemptsExhausted != OnExhaustedClose && a.OnAttemptsExhausted != OnExhaustedSkip {
		is.add("answer.on_attempts_exhausted", "%q is not close or skip", redact.String(a.OnAttemptsExhausted))
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
}

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
