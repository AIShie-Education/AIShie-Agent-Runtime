// Package fakecore is a fake AIShiteru Core for the runtime's tests (Core's
// docs/agent-runtime.md §8.2): the same MCP surface and envelope as the real
// one, served in-process, scripted from Go, and held to fixtures recorded
// from a real Core (testdata/fixtures, conformance_test.go).
//
// Its handler serves:
//   - POST /mcp: an official MCP Go SDK server set up as Core sets up its
//     own: stateless, JSON responses, tools only, the bearer token checked on
//     every request, one message per request, and the per-actor rate limit;
//   - GET /v1/tools: the catalogue of the Core the runtime is pinned to;
//   - REST routes for every tool, answered as Core answers them;
//   - GET /healthz, and the files document_get's download_url points at.
//
// The tools the runtime calls (me_*, conversation_*, event_list,
// action_list_mine, and credential_list and credential_revoke, with which
// an agent's token revokes a token of its own; me_site_chat, a newer
// Core's, with Options.SiteChat) are carried out with Core's semantics:
// authorization,
// idempotency, proposals and their decisions, the inbox, events and who sees
// them; so are the writes people make that the test controls go through
// (conversation_open, conversation_ask, action_decide, action_review), and
// document_create, a write a model makes through its seat's perms. The
// model's read tools read canned material per course, under Core's gates
// and scope rules. Any other tool is refused as never attempted: status
// error, code not_found for a course that does not exist, forbidden
// (details.reason not_implemented) otherwise.
//
// State is in memory, behind one lock, as Core's transactions would have
// it. The test controls (AddCourse, Seat, Ask, Approve, …) act as the people
// in Core would, through the same pipeline and rules, and are safe to call
// while calls are being served.
package fakecore

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Options configure a fake Core. The zero value is Core's defaults with no
// rate limit, as scripts/ci-core.sh runs it.
type Options struct {
	// RatePerMinute is each actor's allowance of calls a minute, MCP and
	// REST together (Core's RATE_LIMIT_PER_MINUTE); 0 is no limit.
	RatePerMinute int
	// RateBurst is Core's RATE_LIMIT_BURST; 0 is Core's default, 100.
	RateBurst int
	// ProposalTTL is how long a proposal may wait for a decision (Core's
	// PROPOSAL_TTL); 0 is Core's default, 14 days. Expire ends one at once.
	ProposalTTL time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// BaseURL is where the fake is served, for the download URLs it hands
	// out; empty takes it from each request.
	BaseURL string
	// BeforeOwners answers as a Core from before me_get said who owns an
	// agent (Core's C1): me_get names no one's owner, and the catalogue,
	// GET /v1/tools and tools/list alike, describes no owner_actor_id.
	BeforeOwners bool
	// SiteChat answers as a Core newer than the pinned one, which offers
	// me.site_chat (sitechat.go): its catalogue has the tool, and the fake
	// carries it out.
	SiteChat bool
}

// Core's defaults.
const (
	defaultBurst       = 100
	defaultProposalTTL = 14 * 24 * time.Hour
)

// Core is a fake AIShiteru Core. Its methods are safe for concurrent use.
type Core struct {
	opts    Options
	cat     *catalogue
	limiter *limiter
	handler http.Handler

	mu               sync.Mutex
	system           *actor
	actors           map[string]*actor
	tokens           map[string]*credential
	courses          map[string]*course
	members          map[string]*member
	memberList       []*member
	conversations    map[string]*conversation
	conversationList []*conversation
	messages         map[string]*message
	actions          map[string]*action
	actionList       []*action
	keys             map[actorKey]*action
	seq              int64
	blobs            map[string]*document
	calls            []Call
	nextKey          int
	// siteChat is each actor's last me.site_chat (Options.SiteChat).
	siteChat map[string]bool

	hooks  sync.RWMutex
	inject func(InjectedCall) *Injection
	onCall func(tool string, args json.RawMessage)
}

// implemented is the fake's implementation of each tool it carries out, by
// registry name.
func implemented() map[string]*impl {
	return map[string]*impl{
		"me.get":                meGet(),
		"me.memberships":        meMemberships(),
		"conversation.inbox":    conversationInbox(),
		"conversation.messages": conversationMessages(),
		"conversation.get":      conversationGet(),
		"conversation.answer":   conversationAnswer(),
		"conversation.close":    conversationClose(),
		"conversation.retract":  conversationRetract(),
		"conversation.open":     conversationOpen(),
		"conversation.ask":      conversationAsk(),
		"action.decide":         actionDecide(),
		"action.review":         actionReview(),
		"action.list_mine":      actionListMine(),
		"event.list":            eventList(),
		"course.get":            courseGet(),
		"document.list":         documentList(),
		"document.get":          documentGet(),
		"document.create":       documentCreate(),
		"assignment.list":       assignmentList(),
		"assignment.get":        assignmentGet(),
		"submission.list":       submissionList(),
		"submission.get":        submissionGet(),
		"grade.list":            gradeList(),
		"grade.get":             gradeGet(),
		"component.tree":        componentTree(),
		"gradebook.get":         gradebookGet(),
		"credential.list":       credentialList(),
		"credential.revoke":     credentialRevoke(),
		"me.site_chat":          meSiteChat(),
	}
}

// theCatalogue is the embedded catalogue with the fake's implementations,
// loaded once: it is never changed after, and every fake shares it.
var theCatalogue = sync.OnceValues(func() (*catalogue, error) {
	return withImpls(catalogueJSON)
})

// catalogueBeforeOwners is theCatalogue as a Core from before C1 serves
// it (Options.BeforeOwners), loaded once.
var catalogueBeforeOwners = sync.OnceValues(func() (*catalogue, error) {
	raw, err := withoutOwners(catalogueJSON)
	if err != nil {
		return nil, err
	}
	return withImpls(raw)
})

// siteChatCatalogue is the catalogue with me.site_chat, as a Core newer
// than the pinned one serves it (Options.SiteChat), before C1 or not.
func siteChatCatalogue(beforeOwners bool) (*catalogue, error) {
	raw := catalogueJSON
	if beforeOwners {
		var err error
		if raw, err = withoutOwners(raw); err != nil {
			return nil, err
		}
	}
	raw, err := withSiteChat(raw)
	if err != nil {
		return nil, err
	}
	return withImpls(raw)
}

// withImpls loads the catalogue raw, with the fake's implementations.
func withImpls(raw []byte) (*catalogue, error) {
	cat, err := loadCatalogue(raw)
	if err != nil {
		return nil, err
	}
	impls := implemented()
	for _, t := range cat.tools {
		t.impl = impls[t.Name]
	}
	return cat, nil
}

// New makes a fake Core with nobody in it. It panics only if the embedded
// catalogue does not load, which TestCatalogueSnapshot rules out at build
// time, as the SDK panics on a tool it cannot register.
func New(o Options) *Core {
	load := theCatalogue
	if o.BeforeOwners {
		load = catalogueBeforeOwners
	}
	if o.SiteChat {
		load = func() (*catalogue, error) { return siteChatCatalogue(o.BeforeOwners) }
	}
	cat, err := load()
	if err != nil {
		panic(err)
	}
	if o.RateBurst <= 0 {
		o.RateBurst = defaultBurst
	}
	if o.ProposalTTL <= 0 {
		o.ProposalTTL = defaultProposalTTL
	}
	c := &Core{
		opts: o, cat: cat,
		actors: map[string]*actor{}, tokens: map[string]*credential{}, courses: map[string]*course{},
		members: map[string]*member{}, conversations: map[string]*conversation{}, messages: map[string]*message{},
		actions: map[string]*action{}, keys: map[actorKey]*action{}, blobs: map[string]*document{},
		siteChat: map[string]bool{},
	}
	c.system = &actor{id: newID(), kind: "system", name: "system", status: statusActive}
	c.limiter = newLimiter(o.RatePerMinute, o.RateBurst, c.now)
	c.handler = c.routes()
	return c
}

// Handler serves the fake's HTTP surface.
func (c *Core) Handler() http.Handler { return c.handler }

func (c *Core) now() time.Time {
	if c.opts.Now != nil {
		return c.opts.Now().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}

func (c *Core) proposalTTL() time.Duration { return c.opts.ProposalTTL }
