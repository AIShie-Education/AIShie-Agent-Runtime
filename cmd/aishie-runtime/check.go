package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	runtimeRunner "github.com/AIShie-Education/AIShie-Agent-Runtime/internal/runner"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/worker"
)

// liveTimeout bounds what check --live does for one agent.
const liveTimeout = 60 * time.Second

// cmdCheck is `aishie-runtime check [--live]`: the configuration loaded and
// validated as run loads it, and each agent and course shown. With --live,
// each agent is read as Core hosts it, with the runtime's own credential
// (CORE_SERVICE_CREDENTIAL), and connected with the token the runtime
// holds for it, if any (check is issued none): its seats are shown as the
// handout words them, with the tools each offers its model, and its
// model's key is tried with one call of one output token.
func cmdCheck(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	live := fs.Bool("live", false, "read each agent in Core, connect it, and try its model's key")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return usageError(stderr, "check takes only --live")
	}
	env, err := config.FromEnv(getenv)
	if err != nil {
		return failure(stderr, "the environment:\n%s", problemsText(err))
	}
	l, err := load(env)
	if err != nil {
		return failure(stderr, "the configuration does not pass:\n%s", problemsText(err))
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(stdout, format+"\n", a...) }
	cfg, pg := l.cfg, (*pgstore.Store)(nil)
	if env.DatabaseURL != "" {
		// The registry is read as run reads it; one that cannot be read
		// (a schema older than this binary's, before a deploy's migrate
		// up) is said, and fails nothing: the YAML is what check checks.
		if pg, err = pgstore.Open(ctx, env.DatabaseURL); err != nil {
			pg = nil
			p("registry: not read: %s", redact.String(err.Error()))
		} else {
			defer func() { _ = pg.Close() }()
			h := &hosting{env: env, pg: pg, log: slog.New(slog.DiscardHandler), yaml: l.cfg, prices: l.prices}
			h.mu.Lock()
			built, _, err := h.build(ctx)
			h.mu.Unlock()
			if err != nil {
				p("registry: not read: %s", redact.String(err.Error()))
			} else {
				cfg = built
			}
		}
	}
	for _, d := range cfg.Deprecated() {
		p("runtime: deprecated: %s: %s", d.Path, d.Msg)
	}
	hosted := 0
	for _, a := range cfg.Agents {
		describe(p, a)
		if a.Hosted != nil {
			hosted++
		}
	}
	for _, r := range cfg.Rejected {
		p("hosted agent %s: NOT RUN: %s", r.AgentID, redact.String(r.Detail()))
	}
	if env.KMSKeyID != "" {
		kek, err := vault.OpenKEK(env.KMSKeyID)
		if err != nil {
			return failure(stderr, "the sealed secrets' keyring: %v", err)
		}
		p("sealed secrets: new ones are sealed by %s", kek.ID())
	}
	p("runner: %s", env.RunnerAddress())
	site := cfg.Runtime.Site
	switch {
	case l.pricesPath != "":
		p("prices: %s (version %s)", l.pricesPath, l.prices.Version)
	case len(site.Prices) == 0:
		p("prices: none; the costs of model calls will be unknown")
	}
	if len(site.Prices) > 0 {
		p("prices: %d rows of the site's (version %s), before the file's", len(site.Prices), pricing.SiteVersion(site.PricesChanged))
	}
	describeSchool(p, cfg.Runtime.School)
	if pg != nil {
		p("the configuration passes: %d agents, %d of them hosted; %d hosted agents not run", len(cfg.Agents), hosted, len(cfg.Rejected))
	} else {
		p("the configuration passes: %d agents", len(cfg.Agents))
	}
	if len(cfg.Agents) == 0 {
		p("%s", noAgentsNote)
		return exitOK
	}
	if !*live {
		return exitOK
	}
	clients, err := newLiveClients(env)
	if err != nil {
		return failure(stderr, "%v", err)
	}
	var sealed store.Secrets
	if env.KMSKeyID != "" && env.DatabaseURL != "" {
		if pg == nil {
			return failure(stderr, "the store, which holds the sealed secrets, cannot be read")
		}
		sealed = pg
	}
	res, _, err := resolver(env, sealed)
	if err != nil {
		return failure(stderr, "the sealed secrets' keyring: %v", err)
	}
	failed := 0
	cats := map[string]*core.Catalogue{}
	ls := liveService{credential: serviceCredential(env, res)}
	if pg != nil {
		ls.st = pg
	}
	for _, a := range cfg.Agents {
		if !checkLive(ctx, p, a, res, clients, cats, ls) {
			failed++
		}
	}
	if failed > 0 {
		return failure(stderr, "%d of %d agents failed the live check", failed, len(cfg.Agents))
	}
	p("every agent connects")
	return exitOK
}

// describe shows an agent's configuration, and each course's own.
func describe(p func(string, ...any), a *config.Agent) {
	state := ""
	if a.Hosted != nil {
		state = " (hosted)"
	}
	if a.Paused {
		state += " (paused)"
	}
	p("agent %s%s: %q", a.ID, state, redact.String(a.DisplayName))
	p("  core: %s over %s", a.Core.BaseURL, a.Core.Transport)
	p("  model: %s", modelLine(a.Model))
	p("  tools: %s", toolsLine(a.Tools))
	p("  answers: at most %d attempts, then %s; out of quota, %s; %d at once, %d a course",
		a.Answer.MaxAttempts, a.Answer.OnAttemptsExhausted, a.Answer.OnQuotaExhausted, a.Answer.MaxConcurrent, a.Answer.MaxConcurrentPerCourse)
	p("  quotas: per agent %s; per asker %s", quotaLine(a.Budgets.PerAgentDay), quotaLine(a.Budgets.PerAskerDay))
	for _, d := range a.Deprecated() {
		p("  deprecated: %s: %s", d.Path, d.Msg)
	}
	ids := make([]string, 0, len(a.Courses))
	for id := range a.Courses {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		e, err := a.ForCourse(id)
		if err != nil {
			p("  course %s: %v", id, err)
			continue
		}
		if !e.Enabled {
			p("  course %s: disabled", id)
			continue
		}
		var parts []string
		if !modelSame(e.Model, a.Model) {
			parts = append(parts, "model "+modelLine(e.Model))
		}
		if e.PromptAppendRef != "" {
			parts = append(parts, "prompt appended from "+e.PromptAppendRef)
		}
		if e.PromptAppendText != "" {
			parts = append(parts, fmt.Sprintf("prompt appended, %d characters", len([]rune(e.PromptAppendText))))
		}
		if e.Budgets.PerAskerDay != a.Budgets.PerAskerDay || e.Budgets.PerAgentDay != a.Budgets.PerAgentDay {
			parts = append(parts, "quotas per agent "+quotaLine(e.Budgets.PerAgentDay)+", per asker "+quotaLine(e.Budgets.PerAskerDay))
		}
		if e.Polling != a.Polling {
			parts = append(parts, fmt.Sprintf("polling %gs idle, %gs hot, %gs at most", e.Polling.InboxIdleS, e.Polling.InboxHotS, e.Polling.InboxMaxS))
		}
		if len(parts) == 0 {
			parts = append(parts, "as the agent")
		}
		p("  course %s: %s", id, strings.Join(parts, "; "))
	}
}

// describeSchool shows the school's plan: its offers, never their keys'
// references, and its quotas.
func describeSchool(p func(string, ...any), sc config.School) {
	if !sc.Offered() {
		p("school plan: no offers; hosted agents are on their owners' own keys")
		if sc.PerDay.Answers != nil || sc.PerDay.USD != nil {
			p("school plan: the school's key is held to %s across the school", quotaLine(sc.PerDay))
		}
		return
	}
	for _, o := range sc.Offers {
		m := o.AsModel()
		site := ""
		if o.Site {
			site = ", made in the site"
		}
		p("school plan: offer %s, %q: %s %s (%s)%s", o.ID, redact.String(o.Label), m.Adapter, m.Model, m.EffectiveProvider(), site)
		if o.OpenRouter.Canonical() != nil {
			p("school plan: offer %s's upstream routing, sent to OpenRouter with every call: %s", o.ID, o.OpenRouter.JSON())
		}
	}
	p("school plan: per owner %s; per asker %s; across the school %s (UTC days)", quotaLine(sc.OwnerQuota()), quotaLine(sc.AskerQuota()), quotaLine(sc.PerDay))
}

func modelSame(x, y config.Model) bool {
	return modelLine(x) == modelLine(y)
}

func modelLine(m config.Model) string {
	s := fmt.Sprintf("%s %s (%s) on the %s key, %d output tokens a call", m.Adapter, m.Model, m.EffectiveProvider(), m.KeySource, m.Params.MaxOutputTokens)
	if m.OpenRouter.Canonical() != nil {
		s += fmt.Sprintf(", upstream routing %s", m.OpenRouter.JSON())
	}
	if m.Fallback != nil {
		s += fmt.Sprintf("; fallback %s %s (%s)", m.Fallback.Adapter, m.Fallback.Model, m.Fallback.EffectiveProvider())
		if m.Fallback.OpenRouter.Canonical() != nil {
			s += fmt.Sprintf(", upstream routing %s", m.Fallback.OpenRouter.JSON())
		}
	}
	return s
}

func toolsLine(t config.Tools) string {
	if t.Mode == config.ToolsNone {
		return "none"
	}
	allow := "the default list"
	if len(t.Allow) > 0 {
		allow = strings.Join(t.Allow, ", ")
	}
	s := "derived from each seat's perms, of " + allow
	if len(t.Deny) > 0 {
		s += ", less " + strings.Join(t.Deny, ", ")
	}
	return s
}

func quotaLine(q config.Quota) string {
	var parts []string
	if q.Answers != nil {
		parts = append(parts, fmt.Sprintf("%d answers", *q.Answers))
	}
	if q.USD != nil {
		parts = append(parts, fmt.Sprintf("$%.2f", *q.USD))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " and ") + " a day"
}

// liveClients are what check --live calls out with: the egress client for
// Core and the loopback client whose runner probes every model key.
type liveClients struct {
	egress *http.Client
	runner *runtimeRunner.Client
}

// newLiveClients makes check --live's clients from the environment.
func newLiveClients(env config.Env) (liveClients, error) {
	egress, err := egressClient(env)
	if err != nil {
		return liveClients{}, err
	}
	return liveClients{egress: egress, runner: runtimeRunner.NewClient(env.RunnerAddress(), nil)}, nil
}

// checkLive reads one agent as Core hosts it, with the runtime's own
// credential, as run reads it at the agent's start, and is issued nothing:
// an mcp agent, one Core has not, one suspended (or whose owner is), and a
// hosted agent whose owner in Core is not the person who hosted it each
// fail. With the token the runtime holds for it (a hosted agent's row, or
// the store's for an operator's agent), it connects as run would and shows
// its seats; then it tries the model's key, over the client run would call
// it with. It reports whether all went well.
func checkLive(ctx context.Context, p func(string, ...any), a *config.Agent, res secrets.Resolver, clients liveClients, cats map[string]*core.Catalogue,
	l liveService) bool {
	if a.Paused {
		p("agent %s: paused, not connected", a.ID)
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()
	fail := func(what string, err error) bool {
		p("agent %s: FAILED: %s: %s", a.ID, what, redact.String(err.Error()))
		return false
	}
	coreHTTP := *clients.egress
	coreHTTP.Timeout = core.DefaultTimeout
	cat := cats[a.Core.BaseURL]
	if cat == nil {
		var err error
		if cat, err = core.FetchCatalogue(ctx, &coreHTTP, a.Core.BaseURL); err != nil {
			return fail("Core's catalogue", err)
		}
		cats[a.Core.BaseURL] = cat
		p("core %s: catalogue %s (%d tools)", a.Core.BaseURL, cat.Hash(), cat.Len())
	}
	if !core.HasRuntimeService(cat) {
		return fail("Core", errors.New("it has no agent_runtime service (it is older than AIShie-Core #52): it hosts no agent by its id"))
	}
	agentID := strings.ToLower(a.Core.AgentID)
	rs := core.NewRuntimeService(core.RuntimeCaller(core.RuntimeOptions{BaseURL: a.Core.BaseURL, Credential: l.credential,
		HTTPClient: &coreHTTP, Catalogue: cat, Once: true}))
	view, err := rs.Agent(ctx, agentID)
	var ce *core.CredentialError
	switch {
	case errors.As(err, &ce):
		return fail("the runtime's own credential (CORE_SERVICE_CREDENTIAL)", ce.Err)
	case core.CredentialRefused(err):
		return fail("the runtime's own credential (CORE_SERVICE_CREDENTIAL)",
			errors.New("refused by Core: it was revoked, has expired, or is not the agent_runtime service's (aishie-core service issue agent_runtime)"))
	case core.IsNotFound(err):
		return fail("agent_runtime.agent", errors.New("no agent of this id in Core (core.agent_id)"))
	case err != nil:
		return fail("agent_runtime.agent", err)
	}
	asked := "not asked in the site now"
	if view.SiteChat {
		asked = "asked in the site"
	}
	p("agent %s: in Core %q (%s), hosted %s, %s; %d live seats; %s", a.ID, redact.String(view.DisplayName), view.AgentID, view.Hosting,
		view.Status, view.LiveSeats, asked)
	switch h := a.Hosted; {
	case view.Hosting != core.HostingRuntime:
		return fail("hosting", errors.New("an mcp agent: its owner's own tools reach it over MCP, and the runtime cannot host it"))
	case h != nil && !strings.EqualFold(view.OwnerActorID, h.OwnerActorID):
		return fail(store.AgentOwnerChanged, errors.New("the agent's owner in Core is not the person who hosted it here"))
	case !view.Hostable:
		return fail("hosting", fmt.Errorf("the runtime may not host it now, Core says (%s)", view.Reason))
	case h != nil:
		p("  owner: Core names the person who hosted it")
	}
	token, err := l.held(ctx, a, res)
	switch {
	case err != nil:
		return fail("the token the runtime holds", err)
	case token == "":
		p("  token: none held here yet; the worker is issued one by the agent's id as it starts it")
		return tryModels(ctx, p, a, res, clients)
	}
	caller, err := worker.DefaultCaller(a, token, cat, &coreHTTP, nil, core.RetryOptions{}, nil)
	if err != nil {
		return fail("connecting", err)
	}
	c := core.NewClient(caller)
	me, err := c.Me(ctx)
	if errors.Is(err, core.ErrUnauthenticated) {
		return fail("me_get", errors.New("the token the runtime holds was refused (401): it was revoked in Core; the worker is issued "+
			"another once the agent's owner asks for one (a hosted agent), or at a reload (an operator's)"))
	}
	if err != nil {
		return fail("me_get", err)
	}
	if !strings.EqualFold(me.ID, agentID) {
		return fail("me_get", errors.New("the token the runtime holds is another agent's"))
	}
	p("  connected with the token the runtime holds")
	ms, err := c.Memberships(ctx)
	if err != nil {
		return fail("me_memberships", err)
	}
	ok := true
	if len(ms) == 0 {
		p("  no seats: the agent is seated nowhere yet")
	}
	for _, m := range ms {
		e, err := a.ForCourse(m.CourseID)
		if err != nil {
			p("  %s: the course's configuration: %v", seatWords(m), err)
			ok = false
			continue
		}
		line := "  " + seatWords(m)
		if why := notAnswering(m, e); why != "" {
			line += "; does not answer now: " + why
		}
		p("%s", line)
		readPolicy := worker.ForSeat(cat, e, m, false)
		if len(readPolicy.AllowedTools) > 0 {
			p("    tools: %s", strings.Join(readPolicy.AllowedTools, ", "))
		} else {
			p("    tools: none; it answers from the conversation alone")
		}
		writePolicy := worker.ForSeat(cat, e, m, true)
		var writes []string
		for _, name := range writePolicy.AllowedTools {
			if !slices.Contains(readPolicy.AllowedTools, name) {
				writes = append(writes, name)
			}
		}
		switch {
		case len(writes) > 0:
			p("    writes, in its owner's conversations: %s", strings.Join(writes, ", "))
		case e.Tools.Writes:
			p("    writes: none its seat allows")
		}
	}
	return tryModels(ctx, p, a, res, clients) && ok
}

// tryModels tries the agent's model key, and its fallback's, through the
// same runner probe as the management API.
func tryModels(ctx context.Context, p func(string, ...any), a *config.Agent, res secrets.Resolver, clients liveClients) bool {
	ok := tryModel(ctx, p, a, a.Model, res, clients.runner)
	if fb := a.Model.Fallback; fb != nil && !tryModel(ctx, p, a, *fb, res, clients.runner) {
		ok = false
	}
	return ok
}

// liveService is what check --live asks Core's agent_runtime service with:
// the runtime's own credential, and the token the runtime holds for an
// agent, if any.
type liveService struct {
	credential func(context.Context) (string, error)
	// st is the store, nil when it is not read: an operator's agent's
	// token is kept there, sealed, when the runtime has a keyring.
	st store.Store
}

// held is the token the runtime holds for the agent, "" for none: a
// hosted agent's issued token, sealed in its row; an operator's agent's
// sealed in the store, while it is for the agent core.agent_id names.
func (l liveService) held(ctx context.Context, a *config.Agent, res secrets.Resolver) (string, error) {
	if h := a.Hosted; h != nil {
		if h.TokenSecretID == "" || !h.TokenIssued {
			return "", nil
		}
		return res.Resolve(ctx, secrets.SchemeSealed+h.TokenSecretID, "")
	}
	if l.st == nil || res.Sealed == nil {
		return "", nil
	}
	t, err := l.st.AgentToken(ctx, a.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", nil
	case err != nil:
		return "", err
	case !strings.EqualFold(t.CoreActorID, a.Core.AgentID):
		return "", nil
	}
	return res.Resolve(ctx, secrets.SchemeSealed+t.SecretID, "")
}

// seatWords words a seat as the handout's example does (§5.1): "Delegate
// of … in CS101: reads your work, answers only you", "Tutor of CS101:
// answers every student, reads the material".
func seatWords(m core.Membership) string {
	course := m.Code
	if m.Section != "" {
		course += " (" + m.Section + ")"
	}
	reads := readsWords(m)
	switch {
	case m.AnswersCourse:
		return fmt.Sprintf("Tutor of %s: answers every student, %s", course, reads)
	case m.PrincipalMemberID != nil:
		return fmt.Sprintf("Delegate of member %s in %s: %s, answers only you", *m.PrincipalMemberID, course, reads)
	}
	return fmt.Sprintf("%s in %s: %s", m.Role, course, reads)
}

// readsWords says what the seat may read.
func readsWords(m core.Membership) string {
	allowed := func(perm string) bool { return m.Level(perm) != core.LevelDenied }
	work := allowed("submission_read") || allowed("grade_read")
	material := allowed("document_read")
	whose := "your work"
	if m.AnswersCourse {
		whose = "students' work"
	}
	switch {
	case work && material:
		return "reads " + whose + " and the material"
	case work:
		return "reads " + whose
	case material:
		return "reads the material"
	}
	return "reads nothing"
}

// notAnswering says why the runtime would not answer in a seat, or "".
func notAnswering(m core.Membership, e *config.Effective) string {
	switch {
	case m.Status != "active":
		return "the seat is " + m.Status
	case m.CourseStatus == "archived":
		return "the course is archived"
	case m.Level("conversation_answer") == core.LevelDenied:
		return "conversation_answer is denied"
	case !e.Enabled:
		return "the course is disabled in the configuration"
	}
	return ""
}

// tryModel tries a model's key through the runner's one-token probe. Its
// response contains a safe classification only, never provider error text.
func tryModel(ctx context.Context, p func(string, ...any), a *config.Agent, m config.Model, res secrets.Resolver, client *runtimeRunner.Client) bool {
	name := fmt.Sprintf("%s %s (%s)", m.Adapter, m.Model, m.EffectiveProvider())
	var key string
	if m.KeyRef != "" {
		var err error
		if key, err = res.Resolve(ctx, m.KeyRef, a.Dir); err != nil {
			p("  model %s: FAILED: its key: %s", name, redact.String(err.Error()))
			return false
		}
	}
	tr, err := client.Probe(ctx, runtimeRunner.ModelOf(m, key, a.OverHostedClient(m)))
	switch {
	case err != nil:
		p("  model %s: FAILED: %s", name, redact.String(err.Error()))
		return false
	case tr.Result == runtimeRunner.ResultOK:
		p("  model %s: the key works", name)
	case tr.Result == runtimeRunner.ResultKeyRefused, tr.Result == runtimeRunner.ResultUnreachable:
		p("  model %s: FAILED: %s", name, tr.Result)
		return false
	default:
		status := 0
		if tr.HTTPStatus != nil {
			status = *tr.HTTPStatus
		}
		p("  model %s: the key was taken, though the call came back %s (HTTP %d)", name, tr.Result, status)
	}
	return true
}
