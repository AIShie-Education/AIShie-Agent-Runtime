package main

import (
	"cmp"
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
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/netguard"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/pricing"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/probe"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolschema"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/worker"
)

// liveTimeout bounds what check --live does for one agent.
const liveTimeout = 60 * time.Second

// cmdCheck is `aishie-runtime check [--live]`: the configuration loaded and
// validated as run loads it, and each agent and course shown. With --live,
// each agent is connected to Core as run connects it: its seats are shown as
// the handout words them, with the tools each offers its model, and its
// model's key is tried with one call of one output token.
func cmdCheck(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	live := fs.Bool("live", false, "connect each agent to Core and try its model's key")
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
	if o := env.OCR.WithDefaults(); o.Mode == ocr.ModeOff {
		p("ocr: off (OCR=off)")
	} else if e, err := ocr.NewEngine(ctx, o); err != nil {
		if o.Mode == ocr.ModeOn {
			return failure(stderr, "OCR=on, and OCR cannot run here: %v", err)
		}
		p("ocr: off: %v", err)
	} else {
		p("ocr: %s, %d at once, %d pages a file at most", e.Describe(), o.Concurrency, o.MaxPages)
		if so := cfg.Runtime.Site.OCR; so.Enabled != nil && !*so.Enabled {
			p("ocr: turned off in the site's settings")
		} else if len(so.Languages) > 0 {
			p("ocr: in %s, as the site's settings say", strings.Join(so.Languages, "+"))
		}
	}
	if o := env.Office.WithDefaults(); o.Mode == office.ModeOff {
		p("office: off (OFFICE_PDF=off)")
	} else if c, err := office.NewConverter(ctx, o); err != nil {
		if o.Mode == office.ModeOn {
			return failure(stderr, "OFFICE_PDF=on, and LibreOffice cannot run here: %v", err)
		}
		p("office: off: %v", err)
	} else {
		p("office: %s, %d at once, %d pages a file at most, %s a file", c.Describe(), o.Concurrency, o.MaxPages, o.Timeout)
	}
	if _, err := office.NewPager(env.Office); err != nil {
		p("pdf parts: off, PDFs are given whole: %v", err)
	} else {
		p("pdf parts: %d pages a file part, at most", cmp.Or(env.PDFPartPages, office.DefaultPartPages))
	}
	var kept store.Store
	if pg != nil {
		kept = pg
	}
	egress, err := egressClient(env)
	if err != nil {
		return failure(stderr, "the egress client: %v", err)
	}
	tr, err := checkTranscriber(ctx, env, cfg, kept, egress)
	if err != nil {
		return failure(stderr, "TRANSCRIBE=on, and the transcriber cannot run here: %v", err)
	}
	p("transcriber: %s", tr)
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
	for _, a := range cfg.Agents {
		if !checkLive(ctx, p, a, res, clients, cats) {
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
	}
	p("school plan: per owner %s; per asker %s; across the school %s (UTC days)", quotaLine(sc.OwnerQuota()), quotaLine(sc.AskerQuota()), quotaLine(sc.PerDay))
}

func modelSame(x, y config.Model) bool {
	return modelLine(x) == modelLine(y)
}

func modelLine(m config.Model) string {
	s := fmt.Sprintf("%s %s (%s) on the %s key, %d output tokens a call", m.Adapter, m.Model, m.EffectiveProvider(), m.KeySource, m.Params.MaxOutputTokens)
	if m.Fallback != nil {
		s += fmt.Sprintf("; fallback %s %s (%s)", m.Fallback.Adapter, m.Fallback.Model, m.Fallback.EffectiveProvider())
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

// liveClients are what check --live calls out with: the egress client,
// and the hosted-model client made from it (netguard.Client), which calls
// a hosted agent's model at public addresses alone and follows no
// redirect, as run calls it: an owner chose the model, and its key is
// theirs.
type liveClients struct {
	egress, hosted *http.Client
}

// newLiveClients makes check --live's clients from the environment, as
// run makes them.
func newLiveClients(env config.Env) (liveClients, error) {
	egress, err := egressClient(env)
	if err != nil {
		return liveClients{}, err
	}
	hosted, err := netguard.Client(egress)
	if err != nil {
		return liveClients{}, fmt.Errorf("the hosted agents' model client: %w", err)
	}
	return liveClients{egress: egress, hosted: hosted}, nil
}

// model is the client model m of agent a is called over, as run calls
// it: a hosted agent's over the hosted-model client, but for an offer of
// runtime.yaml's plan, whose endpoint is the operator's
// (config.Agent.OverHostedClient).
func (c liveClients) model(a *config.Agent, m config.Model) *http.Client {
	if a.OverHostedClient(m) {
		return c.hosted
	}
	return c.egress
}

// checkLive connects one agent as run would, shows its seats, and tries
// its model's key, over the client run would call it with. It reports
// whether all went well.
func checkLive(ctx context.Context, p func(string, ...any), a *config.Agent, res secrets.Resolver, clients liveClients, cats map[string]*core.Catalogue) bool {
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
	token, err := res.Resolve(ctx, a.Core.TokenRef, a.Dir)
	if err != nil {
		return fail("the Core token", err)
	}
	coreHTTP := *clients.egress
	coreHTTP.Timeout = core.DefaultTimeout
	cat := cats[a.Core.BaseURL]
	if cat == nil {
		if cat, err = core.FetchCatalogue(ctx, &coreHTTP, a.Core.BaseURL); err != nil {
			return fail("Core's catalogue", err)
		}
		if err := toolset.CheckCatalogue(cat); err != nil {
			return fail("Core's catalogue", err)
		}
		cats[a.Core.BaseURL] = cat
		p("core %s: catalogue %s (%d tools)", a.Core.BaseURL, cat.Hash(), cat.Len())
	}
	caller, err := worker.DefaultCaller(a, token, cat, &coreHTTP, nil, core.RetryOptions{}, nil)
	if err != nil {
		return fail("connecting", err)
	}
	c := core.NewClient(caller)
	me, err := c.Me(ctx)
	if errors.Is(err, core.ErrUnauthenticated) {
		return fail("me_get", errors.New("the token was refused (401): issue a new one for the agent in Core"))
	}
	if err != nil {
		return fail("me_get", err)
	}
	p("agent %s: connected as %q (%s, %s)", a.ID, redact.String(me.DisplayName), me.ID, me.Kind)
	if _, msg := worker.HostedActorProblem(a, me); msg != "" {
		return fail("me_get", errors.New(msg))
	}
	// A hosted agent's owner is checked as run checks it at the agent's
	// start, and a failure named by the state run would give it.
	if prob := worker.HostedOwnerProblem(a, me, cat); prob != nil {
		return fail(prob.State, prob)
	}
	if a.Hosted != nil {
		p("  owner: Core names the person who connected it")
	}
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
		set, err := toolset.Build(cat, m.Perms, e.Tools, toolset.ReadOnly, dialectOf(e.Model), nil)
		if err != nil {
			p("    tools: %v", err)
			ok = false
			continue
		}
		if names := set.Names(); len(names) > 0 {
			p("    tools: %s", strings.Join(names, ", "))
		} else {
			p("    tools: none; it answers from the conversation alone")
		}
		// The writes a conversation its owner opens is offered besides
		// (design §4): none with tools.writes off.
		owner, err := toolset.Build(cat, m.Perms, e.Tools, toolset.ReadWrite, dialectOf(e.Model), nil)
		if err != nil {
			p("    writes: %v", err)
			ok = false
			continue
		}
		switch writes := owner.Writes(); {
		case len(writes) > 0:
			p("    writes, in its owner's conversations: %s", strings.Join(writes, ", "))
		case e.Tools.Writes:
			p("    writes: none its seat allows")
		}
	}
	if !tryModel(ctx, p, a, a.Model, res, clients.model(a, a.Model)) {
		ok = false
	}
	if fb := a.Model.Fallback; fb != nil && !tryModel(ctx, p, a, *fb, res, clients.model(a, *fb)) {
		ok = false
	}
	return ok
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

// dialectOf is the schema dialect a model's tools are declared in: the
// configuration's, else its adapter's default for its provider.
func dialectOf(m config.Model) toolschema.Dialect {
	if d := toolschema.Dialect(m.Capabilities.SchemaDialect); d != "" {
		return d
	}
	_, d := llm.Defaults(m.Adapter, m.EffectiveProvider())
	return d
}

// tryModel tries a model's key with one call of one output token
// (probe.TryModel, as the API's keys/test does): the key works if the
// provider answers anything but a refusal of it.
func tryModel(ctx context.Context, p func(string, ...any), a *config.Agent, m config.Model, res secrets.Resolver, client *http.Client) bool {
	name := fmt.Sprintf("%s %s (%s)", m.Adapter, m.Model, m.EffectiveProvider())
	var key string
	if m.KeyRef != "" {
		var err error
		if key, err = res.Resolve(ctx, m.KeyRef, a.Dir); err != nil {
			p("  model %s: FAILED: its key: %s", name, redact.String(err.Error()))
			return false
		}
	}
	tr, err := probe.TryModel(ctx, m, key, client, nil)
	switch {
	case err != nil:
		p("  model %s: FAILED: %s", name, redact.String(err.Error()))
		return false
	case tr.Result == probe.ResultOK:
		p("  model %s: the key works", name)
	case tr.Err != nil:
		p("  model %s: FAILED: %s", name, redact.String(tr.Err.Error()))
		return false
	case tr.Result == probe.ResultKeyRefused, tr.Result == probe.ResultUnreachable:
		p("  model %s: FAILED: %s", name, tr.Kind)
		return false
	default:
		p("  model %s: the key was taken, though the call came back %s (HTTP %d)", name, tr.Kind, tr.HTTPStatus)
	}
	return true
}
