// Package fakecore is a fake AIshie Core for the runtime's tests (Core's
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
// The transcription service's four tools, and a document's text versions
// (text.go), are carried out as Core #43 has them; so are the files a
// message carries (attachments.go): uploaded with conversation_upload_url
// and a PUT to the fake itself, named in conversation_open, _ask and
// _answer, listed by conversation_messages and served through
// conversation_attachment's download URL; and the site's agent runtime's
// service (hosting.go), which hosts the runtime agents by their ids and is
// issued each one's one token, as AIShie-Core #52 has it, and makes the
// PDF renditions of the Office files of documents and messages
// (renditions.go), as AIShie-Core's migration 0026 has it.
//
// The tools the runtime calls (me_*, conversation_*, event_list,
// action_list_mine, and agent_runtime_* with the service's credential) are
// carried out with Core's semantics: authorization, each seat's ceilings
// (the most it may hold of each permission, as Core's domain.Ceiling works
// them out, and as its views show them), idempotency, proposals and their
// decisions (an agent's owner's among them, where they could have done it
// themselves), the inbox, events and who sees them, the reads that wait
// for news (wait_s, wait.go) within Core's bounds on calls waiting, and no
// question to an agent people in the site may not ask: an mcp agent, or a
// runtime agent whose runtime token does not live; so
// are the writes people make that the test controls go through
// (conversation_open, conversation_ask, action_decide, action_review, an
// escalation for someone other than whoever had a hand in it to close),
// and document_create, member_add and grade_submit (grades.go, a draft
// grade, whose proposal a newer draft entered while it waits refuses on
// approval), and assignment_delete_preview and assignment_delete
// (deletion.go, an assignment deleted for good with its work, which an
// agent may delete only while nobody has started on it), writes a model
// makes through its seat's perms, and the roster
// a model reads (member_list, member_get,
// member_lookup_actor). The model's other read tools (where students stand
// on an assignment, submission_roster, and a document's versions among
// them) read canned material per course, and the files AddFile adds, under
// Core's gates and scope rules. Any other tool is refused as never attempted: status
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
	// WithoutHosting answers as a Core from before an agent's hosting
	// (AIShie-Core #52, hosting.go): its catalogue has no agent_runtime
	// service, so no runtime there is issued an agent's token by its id.
	// Its agents are hosted as ever here, and asked in the site as a
	// runtime agent is. It makes no renditions either.
	WithoutHosting bool
	// WithoutRenditions answers as a Core from before PDF renditions
	// (AIShie-Core's migration 0026, renditions.go), as the runtime was
	// pinned to before them (b6e7d95): its catalogue has neither the agent
	// runtime's renditions nor the tools that send one back, no file is
	// queued for one, and no file's view shows one.
	WithoutRenditions bool
	// WithoutWait answers as a Core from before its reads waited for news,
	// as the runtime was pinned to before 2c1fe1b (wait.go): its catalogue
	// offers no wait_s and no seen_state, and a call that gives either is
	// refused as the schema refuses any argument it does not name.
	WithoutWait bool
	// WithoutDraft answers as a Core from before conversation.draft, as
	// the runtime was pinned to before drafts (draft.go): its catalogue has
	// no such tool, the conversation's views show no draft, and
	// conversation_messages takes no seen_draft_version.
	WithoutDraft bool
	// WithoutFiles answers as a Core from before a version held several
	// files (AIShie-Core #49), as the runtime was pinned to before it
	// (documents.go): its catalogue has no document.file, and neither
	// document.text nor the transcription service's calls take file_id,
	// which their schemas refuse; document.get and document.versions list
	// no files, only the version's own fields of its one file; a claim,
	// a text read and a text event name no file.
	WithoutFiles bool
	// WithoutSources answers as a Core from before an answer said what it
	// relied on (AIShie-Core #71), as the runtime was pinned to before
	// 81ad1fe (sources.go): conversation.answer takes no sources, which its
	// schema refuses, and conversation.messages shows none.
	WithoutSources bool
	// WithoutRevises refuses revises as a Core from before a proposal was
	// sent back for changes (AIShie-Core #68; 2ba8ac7, the runtime's pin
	// before 81ad1fe) does: over MCP, no write's schema names it, in
	// tools/list or as a call is read, so that a call that gives it is
	// refused as one giving any argument its schema does not name; over
	// REST, the Revises header is not read. All else is the pinned Core's,
	// sending a proposal back included (RequestChanges), so that a test
	// sends one back and has the runtime name it to a Core that refuses
	// revises, as a rollback of Core since has it.
	WithoutRevises bool
	// WithdrawnWaits answers as a Core from before a question its opener
	// withdrew waited for no answer, as b0eb848 and older do
	// (conversation.go): with the opener's latest message retracted, the
	// views still say awaiting_answer, or reply_pending_approval with the
	// answer that waits; a draft of the answer is still written, and kept;
	// and an answer to that message is posted, proposed and approved as to
	// any other. The inbox leaves such a conversation out either way.
	WithdrawnWaits bool
	// LongPollWaiters bounds the calls that wait for news at once, every
	// actor's together (Core's LONG_POLL_WAITERS); LongPollWaitersPerActor,
	// one actor's (LONG_POLL_WAITERS_PER_ACTOR). 0 is Core's default, 1000
	// and 16; below 0 lets none wait. A call past either answers at once
	// with what it read, as Core's does.
	LongPollWaiters, LongPollWaitersPerActor int
}

// Core's defaults.
const (
	defaultBurst       = 100
	defaultProposalTTL = 14 * 24 * time.Hour
	// Core's wake.DefaultMaxWaiters and wake.DefaultMaxPerActor.
	defaultLongPollWaiters         = 1000
	defaultLongPollWaitersPerActor = 16
)

// Core is a fake AIshie Core. Its methods are safe for concurrent use.
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
	blobs            map[string]*versionFile
	// uploads are the files uploaded for messages, by upload token, and
	// putURLs the same by the secret of their upload URL; attachments the
	// files messages carry, by id, and downloads the download URLs handed
	// out for them, by their secret (attachments.go).
	uploads     map[string]*upload
	putURLs     map[string]*upload
	attachments map[string]*attachment
	downloads   map[string]download
	calls       []Call
	nextKey     int
	// draftWrites are the conversation_draft calls carried out, in order.
	draftWrites []DraftWrite
	// presetIDs are the built-in presets' ids, by name.
	presetIDs map[string]string
	// services are the site services' actors by scope, each made with its
	// first credential (the transcription service's, text.go, and the
	// agent runtime's, hosting.go); serviceCreds their credentials, by id;
	// textNews is closed, and replaced, when a text version is queued.
	services     map[string]*actor
	serviceCreds map[string]*credential
	textNews     chan struct{}
	// rends are the files' PDF renditions, by id; rendUploads the upload
	// URLs handed out for their PDFs, by upload token, and rendPuts the
	// same by the secret of the URL; rendPDFs the done ones by the secret
	// of the URL that shows the PDF (renditions.go). A rendition queued
	// closes textNews too, which every claim that waits waits on.
	rends       map[string]*rendition
	rendUploads map[string]*renditionUpload
	rendPuts    map[string]*renditionUpload
	rendPDFs    map[string]*rendition

	// waiters are the calls waiting for news now (wait.go), which the
	// events flushed wake; shutdown is closed by Shutdown.
	waiters  map[*waiter]struct{}
	shutdown chan struct{}
	shut     bool

	hooks  sync.RWMutex
	inject func(InjectedCall) *Injection
	onCall func(tool string, args json.RawMessage)
}

// implemented is the fake's implementation of each tool it carries out, by
// registry name.
func implemented() map[string]*impl {
	impls := map[string]*impl{
		"me.get":                  meGet(),
		"me.memberships":          meMemberships(),
		"conversation.inbox":      conversationInbox(),
		"conversation.messages":   conversationMessages(),
		"conversation.get":        conversationGet(),
		"conversation.answer":     conversationAnswer(),
		"conversation.close":      conversationClose(),
		"conversation.retract":    conversationRetract(),
		"conversation.open":       conversationOpen(),
		"conversation.ask":        conversationAsk(),
		"action.decide":           actionDecide(),
		"action.review":           actionReview(),
		"action.list_mine":        actionListMine(),
		"event.list":              eventList(),
		"course.get":              courseGet(),
		"document.list":           documentList(),
		"document.get":            documentGet(),
		"document.create":         documentCreate(),
		"assignment.list":         assignmentList(),
		"assignment.get":          assignmentGet(),
		"submission.list":         submissionList(),
		"submission.get":          submissionGet(),
		"grade.list":              gradeList(),
		"grade.get":               gradeGet(),
		"grade.submit":            gradeSubmit(),
		"component.tree":          componentTree(),
		"gradebook.get":           gradebookGet(),
		"member.list":             memberList(),
		"member.get":              memberGet(),
		"member.lookup_actor":     memberLookupActor(),
		"member.add":              memberAdd(),
		"submission.roster":       submissionRoster(),
		"document.versions":       documentVersions(),
		"conversation.draft":      conversationDraft(),
		"document.text":           documentText(),
		"document.file":           documentFile(),
		"conversation.upload_url": conversationUploadURL(),
		"conversation.attachment": conversationAttachment(),
	}
	// Deleting an assignment for good (deletion.go).
	impls["assignment.delete_preview"], impls[toolAssignmentDelete] = assignmentDeletePreview(), assignmentDelete()
	return impls
}

// theCatalogue is the embedded catalogue with the fake's implementations,
// loaded once: it is never changed after, and every fake shares it.
var theCatalogue = sync.OnceValues(func() (*catalogue, error) {
	return withImpls(catalogueJSON)
})

// catalogueOf is the catalogue as the older Core o names serves it: from
// before PDF renditions (Options.WithoutRenditions), before an agent's
// hosting (Options.WithoutHosting, which had none either), before wait_s
// (Options.WithoutWait), before conversation.draft (Options.WithoutDraft),
// before several files to a version (Options.WithoutFiles), before an
// answer's sources (Options.WithoutSources), or any of them.
func catalogueOf(o Options) (*catalogue, error) {
	raw := catalogueJSON
	for _, older := range []struct {
		is   bool
		edit func([]byte) ([]byte, error)
	}{{o.WithoutRenditions || o.WithoutHosting, withoutRenditions}, {o.WithoutHosting, withoutHosting}, {o.WithoutWait, withoutWait},
		{o.WithoutDraft, withoutDraft}, {o.WithoutFiles, withoutFiles}, {o.WithoutSources, withoutSources}} {
		if !older.is {
			continue
		}
		var err error
		if raw, err = older.edit(raw); err != nil {
			return nil, err
		}
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
	if o.WithoutHosting || o.WithoutRenditions || o.WithoutWait || o.WithoutDraft || o.WithoutFiles || o.WithoutSources {
		load = func() (*catalogue, error) { return catalogueOf(o) }
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
	if o.LongPollWaiters == 0 {
		o.LongPollWaiters = defaultLongPollWaiters
	}
	if o.LongPollWaitersPerActor == 0 {
		o.LongPollWaitersPerActor = defaultLongPollWaitersPerActor
	}
	c := &Core{
		opts: o, cat: cat,
		actors: map[string]*actor{}, tokens: map[string]*credential{}, courses: map[string]*course{},
		members: map[string]*member{}, conversations: map[string]*conversation{}, messages: map[string]*message{},
		actions: map[string]*action{}, keys: map[actorKey]*action{}, blobs: map[string]*versionFile{},
		uploads: map[string]*upload{}, putURLs: map[string]*upload{}, attachments: map[string]*attachment{}, downloads: map[string]download{},
		presetIDs: map[string]string{}, services: map[string]*actor{}, serviceCreds: map[string]*credential{},
		rends: map[string]*rendition{}, rendUploads: map[string]*renditionUpload{}, rendPuts: map[string]*renditionUpload{},
		rendPDFs: map[string]*rendition{}, waiters: map[*waiter]struct{}{}, shutdown: make(chan struct{}),
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
