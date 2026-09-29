package worker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/safety"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
)

// What wrote an attempt's body (store.Attempt.Kind).
const (
	kindModel   = "model"
	kindQuota   = "quota"
	kindBudget  = "budget"
	kindRefusal = "refusal"
	kindClose   = "close"
)

// The writes the worker makes itself.
const (
	toolAnswer = "conversation_answer"
	toolClose  = "conversation_close"
)

const (
	// leaseSlack is how much longer than the wall clock a conversation's
	// lease lasts (§7.4): room for the post after a loop that used it all.
	leaseSlack = 30 * time.Second
	// passSlack is how much of leaseSlack a pass may use, leaving the rest
	// for its bookkeeping before the lease lapses.
	passSlack = 25 * time.Second
	// maxMoves is how often one claim follows the opener to a newer
	// message (design §5.3 step 10).
	maxMoves = 3
	// maxProviderFailures is how many answers to one message may find
	// every provider down before on_budget_text is posted instead.
	maxProviderFailures = 5
	// memoryNotes is how many notes of a conversation the prompt is given.
	memoryNotes = 20
	// recentMessages is how many of a conversation's newest messages are
	// read to see whether it still waits for an answer (stillWaiting): the
	// opener's latest message is among them, as only its answer follows it.
	recentMessages = 10
)

// claim is one inbox row being answered: from the conversation's lease to
// the ledger (design §5.3). A claim may answer more than one message, as
// the opener writes again, and more than one attempt at one.
type claim struct {
	s    *Seat
	a    *Agent
	eff  *config.Effective
	row  core.Conversation
	conv string
	// opener is the asker's member_id, what their quota is keyed on.
	opener    string
	claimedAt time.Time
	// asked is when the question was written, for the notice latency;
	// zero when not known.
	asked time.Time
	// ownKey is set for a pass answered with the fallback on the owner's
	// own key, the school's quota being spent (onOwnKey).
	ownKey bool
	// d is the conversation's drafter, taken when the claim first asks a
	// model and handed back when it ends; nil against a Core that takes no
	// drafts.
	d *drafter
}

// then is what a pass leaves to the claim.
type then int

const (
	thenStop  then = iota
	thenMoved      // answer passResult.moveTo, the newer message
	thenAgain      // another attempt at the same message
)

// passResult is one pass at answering one message: what goes in the
// ledger, and what the claim does next.
type passResult struct {
	msg     string
	no      int
	key     string
	kind    string
	outcome string // "" records nothing: nothing was tried
	posted  bool   // Core took it: executed or proposed
	postAt  time.Time
	// postedID is the message an executed answer made.
	postedID string
	stats    loopStats
	hash     string
	next     then
	moveTo   string
	shorter  bool
	// withdrawn: the question was withdrawn (questionWithdrawn), and
	// nothing more is tried at it.
	withdrawn bool
}

// answer answers one inbox row, holding its slot of the scheduler until it
// is done.
func (s *Seat) answer(ctx context.Context, row core.Conversation) {
	defer s.a.sched.done(s.course, row.ID)
	c := &claim{s: s, a: s.a, eff: s.config(), row: row, conv: row.ID, opener: row.Opener.MemberID, claimedAt: s.a.now()}
	if row.LastMessageAt != nil {
		if t, err := time.Parse(time.RFC3339Nano, *row.LastMessageAt); err == nil {
			c.asked = t
		}
	}
	if !c.lease(ctx) {
		return
	}
	defer c.release()
	defer func() { c.d.close() }()
	c.run(core.WithPriority(ctx, core.PriorityAnswer))
}

func (c *claim) leaseName() string { return "conv:" + c.a.id + ":" + c.conv }

// lease takes (or renews) the conversation's lease for the wall clock and
// leaseSlack, and reports whether this worker has it.
func (c *claim) lease(ctx context.Context) bool {
	ttl := c.eff.Budgets.PerAnswer.WallClock() + leaseSlack
	ok, err := c.a.store().AcquireLease(ctx, c.leaseName(), c.a.s.o.WorkerID, ttl)
	if err != nil {
		if ctx.Err() == nil {
			c.s.log.Warn("conversation lease not taken", "conversation", c.conv, "err", err)
		}
		return false
	}
	return ok
}

func (c *claim) release() {
	ctx, cancel := bookkeeping()
	defer cancel()
	if err := c.a.store().ReleaseLease(ctx, c.leaseName(), c.a.s.o.WorkerID); err != nil {
		c.s.log.Warn("conversation lease not released", "conversation", c.conv, "err", err)
	}
}

// bookkeeping is a context for the store's writes after a call's own
// context may have ended.
func bookkeeping() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), storeTimeout)
}

// run answers the row's message, following the opener to a newer one at
// most maxMoves times, and trying one message at most twice.
func (c *claim) run(ctx context.Context) {
	msg := *c.row.LatestOpenerMessageID
	moves, again := 0, 0
	shorter := false
	for {
		if (moves > 0 || again > 0) && !c.lease(ctx) {
			return
		}
		r := c.pass(ctx, msg, shorter)
		c.record(r)
		switch {
		case r.next == thenMoved && moves < maxMoves && r.moveTo != "":
			moves++
			msg, shorter = r.moveTo, false
		case r.next == thenAgain && again < 1:
			again++
			shorter = r.shorter
		default:
			return
		}
	}
}

// pass answers message msgID once (design §5.3 steps 2 to 10).
func (c *claim) pass(ctx context.Context, msgID string, shorter bool) passResult {
	wall := c.eff.Budgets.PerAnswer.WallClock()
	ctx, cancel := context.WithTimeout(ctx, wall+passSlack)
	defer cancel()
	for switched := 0; ; switched++ {
		r := passResult{msg: msgID}
		// 2. An attempt at msg still sending is sent again first.
		atts, err := c.a.store().AttemptsFor(ctx, c.a.id, c.conv, msgID)
		if err != nil {
			return c.failedHere(r, "the attempts could not be read", err)
		}
		for _, at := range atts {
			if at.State == store.AttemptSending {
				return c.resent(ctx, at, r)
			}
		}
		// 3. The attempt number.
		n, busy := nextAttempt(atts)
		if busy {
			// Posted, or waiting for a person: nothing to do but see
			// whether a decision was made.
			poke(c.s.wakeEvents)
			return r
		}
		if n > c.eff.Answer.MaxAttempts {
			return c.exhausted(ctx, r)
		}
		r.no, r.key = n, core.AnswerKey(c.conv, msgID, n)
		// 4. Quotas. One of the school's spent, the owner's own key
		// answers, when the agent has one behind the school's.
		q, err := c.quota(ctx)
		if err != nil {
			return c.failedHere(r, "the quotas could not be checked", err)
		}
		c.ownKey = false
		if q != nil {
			if !c.onOwnKey(q) {
				return c.outOfQuota(ctx, r, q)
			}
			c.ownKey = true
			c.a.s.o.Metrics.BudgetExhausted.WithLabelValues(q.name).Inc()
			c.s.log.Info("the school's quota is spent: the owner's own key answers", "conversation", c.conv, "opener", c.opener, "quota", q.name)
		}
		// 5. The conversation.
		read, err := c.a.client.Messages(ctx, c.s.course, c.conv, core.MessagesQuery{Limit: c.eff.Answer.HistoryMessages})
		if err != nil {
			return c.readFailed(ctx, r, err)
		}
		if questionWithdrawn(read) {
			return c.withdrawn(r, "it is not answered")
		}
		latest := deref(read.Conversation.LatestOpenerMessageID)
		if latest != "" && latest != msgID && switched < maxMoves {
			msgID = latest
			continue
		}
		if read.Conversation.State != core.StateAwaitingAnswer || latest != msgID {
			return r
		}
		return c.generate(ctx, r, read, shorter)
	}
}

// nextAttempt is the number of the next attempt at a message, one more than
// those so far, all settled without posting; busy when one posted or waits
// for a person.
func nextAttempt(atts []store.Attempt) (n int, busy bool) {
	n = 1
	for _, at := range atts {
		if at.State.Posted() || at.State == store.AttemptProposed {
			return 0, true
		}
		n = max(n, at.No+1)
	}
	return n, false
}

// generate is steps 6 to 10: the prompt, the loop, the body made safe,
// and the post.
func (c *claim) generate(ctx context.Context, r passResult, read *core.Messages, shorter bool) passResult {
	access := c.access(read)
	m, _ := c.models()
	set, err := c.s.toolsFor(m.ad.Dialect(), access)
	if err != nil {
		return c.failedHere(r, "the toolset could not be built", err)
	}
	sys, hash, err := c.system(ctx, read, shorter, set)
	if err != nil {
		return c.failedHere(r, "the memory could not be read", err)
	}
	r.hash = hash
	hist, err := prompt.History(read.Messages, c.s.id, r.msg, read.More)
	if err != nil {
		return c.failedHere(r, "the question is not in the conversation read", err)
	}
	if c.d == nil {
		c.d = c.a.drafter(c.s.course, c.conv)
	}
	c.d.begin()
	l, err := newLoop(c, r.msg, r.no, access, c.guard(read), sys, hist)
	if err != nil {
		c.d.end(false)
		return c.failedHere(r, "the toolset could not be built", err)
	}
	end := l.run(ctx)
	r.stats = l.stats
	if end.fatal != nil || end.failed {
		// Given up: the attempt's draft goes.
		c.d.end(false)
	}
	switch {
	case end.fatal != nil:
		if isUnauthenticated(end.fatal) {
			c.a.stop(core.ErrUnauthenticated)
		} else {
			// The claim's time ran out, or the agent is stopping: the
			// next claim, if any, is not at once.
			c.s.holdBack(c.conv, c.a.now().Add(c.a.s.o.Timing.RetryLater), "the answer ran out of time")
		}
		r.outcome = store.OutcomeError
		return r
	case end.failed:
		return c.providersDown(ctx, r)
	}
	c.s.providerRecovered(r.msg)
	r = c.post(ctx, r, end.body, end.kind)
	// Posted or proposed, the answer took the draft's place; otherwise the
	// attempt is over, and its draft goes.
	c.d.end(r.posted)
	return r
}

// models are the model this pass answers with, and the one it falls back
// to when that one's provider cannot be reached: the seat's, or, on the
// owner's own key (ownKey), its fallback alone.
func (c *claim) models() (m, fallback *model) {
	if c.ownKey {
		return c.s.fallback, nil
	}
	return c.s.primary, c.s.fallback
}

// openerOf is the conversation's opener: Core's, as the conversation was
// read.
func (c *claim) openerOf(read *core.Messages) string {
	if opener := read.Conversation.Opener.MemberID; opener != "" {
		return opener
	}
	return c.opener
}

// access is what this conversation's model is offered (accessFor): the
// seat's writes only when its owner opened it.
func (c *claim) access(read *core.Messages) toolset.Access {
	return accessFor(c.a.cfg, c.eff.Tools, c.s.membership(), c.openerOf(read))
}

// guard is the seats this conversation's model never changes
// (toolset.SeatGuard): the agent's own, its principal's and the opener's.
func (c *claim) guard(read *core.Messages) toolset.SeatGuard {
	return toolset.SeatGuard{Self: c.s.id, Principal: deref(c.s.membership().PrincipalMemberID), Opener: c.openerOf(read)}
}

// system is the system prompt for this answer, whose model is offered set,
// and its hash.
func (c *claim) system(ctx context.Context, read *core.Messages, shorter bool, set *toolset.Set) (string, string, error) {
	var notes []store.Note
	if c.eff.Memory.Enabled {
		var err error
		if notes, err = c.a.store().Notes(ctx, c.a.id, c.s.id, c.conv, memoryNotes); err != nil {
			return "", "", err
		}
	}
	m := c.s.membership()
	text, hash := prompt.System(prompt.Input{
		Base: c.s.basePrompt(m), Append: c.s.appended,
		Seat: prompt.Seat{
			AgentName: c.a.name(), Course: prompt.CourseName(m), AnswersCourse: m.AnswersCourse,
			AskerName: read.Conversation.Opener.DisplayName, AnswerLevel: read.Conversation.Respondent.AnswerLevel,
			Tools: set.Reads(), Writes: set.Writes(),
		},
		AnswerLanguage: c.eff.Prompt.AnswerLanguage, Notes: notes, Now: c.a.now(),
	})
	if shorter {
		text += fmt.Sprintf("\n- Your last answer here could not be posted as it was: it was too long, or held links that had to be removed. "+
			"Write a shorter one, well under %d characters, with no links.", c.eff.Answer.MaxBodyChars)
	}
	return text, hash, nil
}

// post makes body safe (step 8) and posts it written ahead (step 9) under
// r.key, then acts on what came back (step 10).
func (c *claim) post(ctx context.Context, r passResult, body, kind string) passResult {
	// The answer takes its draft's place: nothing more of the draft is
	// sent, lest a write come after it.
	c.d.hold()
	safe, rep := safety.Body(body, c.eff.Answer.MaxBodyChars)
	if rep.Empty {
		safe, _ = safety.Body(c.eff.Prompt.OnBudgetText, c.eff.Answer.MaxBodyChars)
		kind = kindBudget
	}
	if rep.LinksRemoved+rep.ImagesRemoved > 0 || rep.Truncated {
		c.s.log.Info("the answer was made safe to post", "conversation", c.conv, "message", r.msg,
			"links_removed", rep.LinksRemoved, "images_removed", rep.ImagesRemoved, "truncated", rep.Truncated)
	}
	r.kind = kind
	args, err := json.Marshal(core.AnswerArgs{CourseID: c.s.course, ConversationID: c.conv, InReplyToMessageID: r.msg, Body: safe, IdempotencyKey: r.key})
	if err != nil {
		return c.failedHere(r, "the answer could not be written", err)
	}
	at := store.Attempt{Key: r.key, AgentID: c.a.id, MemberID: c.s.id, CourseID: c.s.course, ConversationID: c.conv,
		MessageID: r.msg, No: r.no, Tool: toolAnswer, Args: args, Kind: kind, State: store.AttemptSending}
	prev, err := c.a.store().PutAttempt(ctx, at)
	switch {
	case errors.Is(err, store.ErrExists):
		// Another worker, or an earlier run, wrote this key first: its
		// bytes are what goes under it.
		at = *prev
		r.kind = prev.Kind
	case err != nil:
		return c.failedHere(r, "the answer could not be written ahead", err)
	}
	return c.send(ctx, r, at, rep)
}

// resent sends a sending attempt's stored bytes again (step 2), and acts
// on what came back.
func (c *claim) resent(ctx context.Context, at store.Attempt, r passResult) passResult {
	r.no, r.key, r.kind = at.No, at.Key, at.Kind
	c.s.log.Info("an attempt written ahead is sent again", "conversation", c.conv, "key", at.Key)
	return c.send(ctx, r, at, safety.Report{})
}

// send sends an attempt's bytes, settles the attempt with what came back,
// and acts on it.
func (c *claim) send(ctx context.Context, r passResult, at store.Attempt, rep safety.Report) passResult {
	over := c.s.sendBegins()
	env, err := c.a.client.Send(ctx, at.Tool, at.Args)
	d := Classify(env, err)
	r.postAt, r.postedID = c.a.now(), messageID(env)
	settle(c.a, c.eff, at, env, d)
	over()
	return c.act(ctx, r, d, rep)
}

// settle records what became of an attempt, when anything is known to
// have: a decision with no state leaves it sending, to be sent again. An
// answer that comes back as a proposal rejected or cancelled (a replay: a
// person decided while the attempt was left sending) leaves a note in the
// conversation's memory, the rejection's reason for the next attempt's
// prompt, as the events poller notes a decision it reads (§2.4).
func settle(a *Agent, eff *config.Effective, at store.Attempt, env *core.Envelope, d Decision) {
	if d.State == "" {
		return
	}
	o := store.Outcome{State: d.State, ErrorCode: d.Code, Reason: d.Reason}
	if env != nil {
		o.ActionID, o.PostedMessageID = env.ActionID, messageID(env)
	}
	if d.State == store.AttemptRejected && env != nil {
		// A rejection's reason is the decision's, in the result.
		o.Reason = core.Action{Result: env.Result}.DecisionReason()
	}
	ctx, cancel := bookkeeping()
	defer cancel()
	if err := a.store().FinishAttempt(ctx, a.id, at.Key, o); err != nil {
		a.log.Error("attempt not settled", "key", at.Key, "state", d.State, "err", err)
		return
	}
	if at.Tool != toolAnswer {
		return
	}
	n := store.Note{AgentID: a.id, MemberID: at.MemberID, ConversationID: at.ConversationID, MessageID: at.MessageID, Text: o.Reason}
	switch d.State {
	case store.AttemptRejected:
		n.Kind = store.NoteRejected
	case store.AttemptCancelled:
		n.Kind = store.NoteCancelled
	default:
		return
	}
	addNote(a, eff, n)
}

// messageID is the message an executed answer made.
func messageID(env *core.Envelope) string {
	if env == nil || env.Status != core.StatusExecuted {
		return ""
	}
	var res struct {
		MessageID string `json:"message_id"`
	}
	if env.Decode(&res) != nil {
		return ""
	}
	return res.MessageID
}

// act does what Classify's decision says (step 10).
func (c *claim) act(ctx context.Context, r passResult, d Decision, rep safety.Report) passResult {
	r.outcome = d.Outcome
	switch d.Next {
	case NextDone:
		r.posted = true
		r.outcome = postedOutcome(r.kind)
		c.s.markHot()
		c.noteAnswered(r.msg, r.postedID)
	case NextProposed:
		r.posted = true
		c.s.followUp()
		c.s.log.Info("the answer waits for a person's approval", "conversation", c.conv, "key", r.key)
	case NextHoldSeat:
		c.s.holdSeat("Core denied its answer: its level was lowered, or the seat or its principal was paused")
	case NextMovedOn:
		r.next, r.moveTo = thenMoved, d.LatestMessageID
	case NextLeave, NextDrop:
	case NextDropReseat:
		c.a.requestReseat()
	case NextFix:
		if rep.Truncated || rep.LinksRemoved+rep.ImagesRemoved > 0 {
			r.next, r.shorter = thenAgain, true
		}
	case NextAttempt:
		r = c.stillWaiting(ctx, r)
	case NextRetryLater:
		c.s.holdBack(c.conv, c.a.now().Add(c.a.s.o.Timing.RetryLater), "Core could not be reached")
	case NextStopAgent:
		c.a.stop(core.ErrUnauthenticated)
	}
	if d.Code != "" {
		c.s.log.Info("Core did not post the answer", "conversation", c.conv, "key", r.key, "next", d.Next.String(),
			"code", d.Code, "reason", d.Reason)
	}
	return r
}

// postedOutcome is the ledger's outcome for a posted body of kind.
func postedOutcome(kind string) string {
	switch kind {
	case kindQuota:
		return store.OutcomeQuota
	case kindBudget:
		return store.OutcomeBudget
	case kindRefusal:
		return store.OutcomeRefusal
	}
	return store.OutcomePosted
}

// stillWaiting says what follows an attempt at r.msg that posted nothing:
// another attempt, if the conversation still waits for an answer to it; the
// newer message, if the opener wrote again; else nothing. Nothing, too,
// when the opener withdrew what they asked last, which the pinned Core's
// state does not say: it reads the conversation's newest messages, which
// show the question retracted, as Core's moved_on naming no message means.
func (c *claim) stillWaiting(ctx context.Context, r passResult) passResult {
	read, err := c.a.client.Messages(ctx, c.s.course, c.conv, core.MessagesQuery{Limit: recentMessages})
	if err != nil {
		if isUnauthenticated(err) {
			c.a.stop(core.ErrUnauthenticated)
		}
		return r
	}
	latest := deref(read.Conversation.LatestOpenerMessageID)
	switch {
	case questionWithdrawn(read):
		r.withdrawn = true
		c.s.log.Info("the question was withdrawn: it is not answered again", "conversation", c.conv, "message", latest)
	case read.Conversation.State != core.StateAwaitingAnswer:
	case latest != "" && latest != r.msg:
		r.next, r.moveTo = thenMoved, latest
	default:
		r.next = thenAgain
	}
	return r
}

// questionWithdrawn reports whether the conversation read waits for no
// answer because its opener withdrew what they asked last: their latest
// message is retracted ("stop" in the chat). The inbox leaves such a
// conversation out. A Core since AIShie-Core #42 says it is answered, and
// refuses an answer to it; the pinned Core, b0eb848, says it still waits
// for one, and would post it.
func questionWithdrawn(read *core.Messages) bool {
	latest := deref(read.Conversation.LatestOpenerMessageID)
	if latest == "" {
		return false
	}
	for _, m := range read.Messages {
		if m.ID == latest {
			return m.Retracted != nil
		}
	}
	return false
}

// withdrawn ends a pass at a question its opener withdrew: nothing is
// posted, and nothing more is tried at it.
func (c *claim) withdrawn(r passResult, what string) passResult {
	c.s.log.Info("the question was withdrawn: "+what, "conversation", c.conv, "message", r.msg)
	r.outcome, r.next, r.withdrawn = store.OutcomeDropped, thenStop, true
	return r
}

// noteAnswered remembers, in the conversation's memory, that an answer was
// posted; posted is its message, when known.
func (c *claim) noteAnswered(question, posted string) {
	if posted == "" {
		posted = c.postedFor(question)
	}
	addNote(c.a, c.eff, store.Note{AgentID: c.a.id, MemberID: c.s.id, ConversationID: c.conv, Kind: store.NoteAnswered,
		Text: answeredNote(posted), MessageID: posted})
}

// postedFor is the message an attempt at question posted, from the store.
func (c *claim) postedFor(question string) string {
	ctx, cancel := bookkeeping()
	defer cancel()
	atts, err := c.a.store().AttemptsFor(ctx, c.a.id, c.conv, question)
	if err != nil {
		return ""
	}
	for _, at := range atts {
		if at.State.Posted() {
			return at.PostedMessageID
		}
	}
	return ""
}

func answeredNote(posted string) string {
	if posted == "" {
		return "You answered this question."
	}
	return "You answered this question (message " + posted + ")."
}

// addNote adds a note to a conversation's memory, when the agent keeps
// memory.
func addNote(a *Agent, eff *config.Effective, n store.Note) {
	if eff != nil && !eff.Memory.Enabled {
		return
	}
	ctx, cancel := bookkeeping()
	defer cancel()
	if err := a.store().AddNote(ctx, n); err != nil {
		a.log.Error("note not kept", "conversation", n.ConversationID, "kind", n.Kind, "err", err)
	}
}

// readFailed ends a pass whose read of the conversation came back other
// than executed.
func (c *claim) readFailed(ctx context.Context, r passResult, err error) passResult {
	var ee *core.EnvelopeError
	switch {
	case isUnauthenticated(err):
		c.a.stop(core.ErrUnauthenticated)
		r.outcome = store.OutcomeError
	case errors.As(err, &ee) && ee.Envelope.Status == core.StatusDenied:
		c.s.holdSeat("Core denied reading a conversation (" + ee.Envelope.Reason() + ")")
		r.outcome = store.OutcomeDenied
	case errors.As(err, &ee) && ee.Envelope.Code() == core.CodeNotFound:
		r.outcome = store.OutcomeDropped
		c.a.requestReseat()
	case errors.As(err, &ee) && ee.Envelope.Code() == core.CodeForbidden:
		r.outcome = store.OutcomeDropped
		c.a.requestReseat()
	default:
		if ctx.Err() == nil {
			c.s.holdBack(c.conv, c.a.now().Add(c.a.s.o.Timing.RetryLater), "the conversation could not be read")
		}
		r.outcome = store.OutcomeError
	}
	c.s.log.Warn("the conversation could not be read", "conversation", c.conv, "err", err)
	return r
}

// failedHere ends a pass that failed on the runtime's side, holding the
// conversation back a little.
func (c *claim) failedHere(r passResult, what string, err error) passResult {
	c.s.log.Error(what, "conversation", c.conv, "message", r.msg, "err", err)
	c.s.holdBack(c.conv, c.a.now().Add(c.a.s.o.Timing.RetryLater), what)
	r.outcome = store.OutcomeError
	return r
}

// providersDown ends a pass whose providers all failed: nothing is posted
// and the conversation is held back, a minute doubling to ten, until the
// fifth such failure on the message, when on_budget_text is posted
// (design §5.3 step 7).
func (c *claim) providersDown(ctx context.Context, r passResult) passResult {
	n, hold := c.s.providerFailed(r.msg)
	if n >= maxProviderFailures {
		c.s.providerRecovered(r.msg)
		c.s.log.Warn("the providers failed again: the budget text is posted", "conversation", c.conv, "failures", n)
		return c.post(ctx, r, c.eff.Prompt.OnBudgetText, kindBudget)
	}
	c.s.holdBack(c.conv, c.a.now().Add(hold), "the model's providers could not be reached")
	r.outcome = store.OutcomeError
	return r
}

// exhausted acts on a message whose attempts are spent (step 3): the
// conversation is closed, or skipped until tomorrow.
func (c *claim) exhausted(ctx context.Context, r passResult) passResult {
	if c.eff.Answer.OnAttemptsExhausted == config.OnExhaustedSkip {
		c.s.holdBack(c.conv, nextDay(c.a.now()), "its attempts are spent")
		r.outcome = store.OutcomeSkipped
		return r
	}
	r.key, r.kind = core.CloseKey(c.conv), kindClose
	args, err := json.Marshal(core.CloseArgs{CourseID: c.s.course, ConversationID: c.conv, Reason: c.eff.Prompt.CloseReasonText, IdempotencyKey: r.key})
	if err != nil {
		return c.failedHere(r, "the close could not be written", err)
	}
	at := store.Attempt{Key: r.key, AgentID: c.a.id, MemberID: c.s.id, CourseID: c.s.course, ConversationID: c.conv,
		Tool: toolClose, Args: args, Kind: kindClose, State: store.AttemptSending}
	if prev, err := c.a.store().PutAttempt(ctx, at); errors.Is(err, store.ErrExists) {
		at = *prev
	} else if err != nil {
		return c.failedHere(r, "the close could not be written ahead", err)
	}
	over := c.s.sendBegins()
	env, err := c.a.client.Send(ctx, at.Tool, at.Args)
	d := classifyClose(env, err)
	settle(c.a, c.eff, at, env, d)
	over()
	r.outcome = d.Outcome
	switch d.Next {
	case NextDone:
		c.s.log.Info("the conversation was closed: its attempts are spent", "conversation", c.conv)
	case NextHoldSeat:
		c.s.holdSeat("Core denied closing a conversation")
	case NextDropReseat:
		c.a.requestReseat()
	case NextRetryLater:
		c.s.holdBack(c.conv, c.a.now().Add(c.a.s.o.Timing.RetryLater), "Core could not be reached")
	case NextStopAgent:
		c.a.stop(core.ErrUnauthenticated)
	}
	return r
}

// classifyClose reads what came back from conversation_close.
func classifyClose(env *core.Envelope, err error) Decision {
	if err != nil {
		return classifyError(err)
	}
	d := Decision{Code: env.Code(), Reason: env.Reason()}
	switch env.Status {
	case core.StatusExecuted:
		d.Next, d.State, d.Outcome = NextDone, store.AttemptExecuted, store.OutcomeClosed
	case core.StatusProposed:
		d.Next, d.State, d.Outcome = NextProposed, store.AttemptProposed, store.OutcomeProposed
	case core.StatusDenied:
		d.Next, d.State, d.Outcome = NextHoldSeat, store.AttemptDenied, store.OutcomeDenied
	case core.StatusFailed:
		// Closed already, by someone; or the conversation is not there.
		d.Next, d.State, d.Outcome = NextDrop, store.AttemptFailed, store.OutcomeDropped
		if d.Code == core.CodeForbidden {
			d.Next = NextDropReseat
		}
	case core.StatusRejected, core.StatusCancelled:
		d.Next, d.State, d.Outcome = NextDrop, store.AttemptState(env.Status), store.OutcomeDropped
	default:
		d.State = store.AttemptError
		switch d.Code {
		case core.CodeUnauthenticated:
			d.Next, d.State, d.Outcome = NextStopAgent, "", store.OutcomeError
		case core.CodeInternal, core.CodeRateLimited:
			d.Next, d.State, d.Outcome = NextRetryLater, "", store.OutcomeError
		default:
			d.Next, d.Outcome = NextDrop, store.OutcomeDropped
		}
	}
	return d
}

// record ends a pass (step 11): its ledger row, its metrics and one log
// line, with ids, counts, the outcome and timings, never text.
func (c *claim) record(r passResult) {
	if r.outcome == "" {
		return
	}
	now := c.a.now()
	rec := store.AnswerRecord{
		ID: uuid.NewString(), At: now, TenantID: c.a.cfg.TenantID, AgentID: c.a.id, CourseID: c.s.course, MemberID: c.s.id,
		ConversationID: c.conv, MessageID: r.msg, OpenerMemberID: c.opener, Key: r.key, Outcome: r.outcome,
		Billable: r.kind == kindModel && r.posted, Turns: r.stats.Turns, ToolCalls: r.stats.ToolCalls,
		Writes:      r.stats.Writes,
		InputTokens: r.stats.In, OutputTokens: r.stats.Out, CostPUSD: r.stats.Cost, KeySource: cmp.Or(r.stats.KeySource, c.eff.Model.KeySource),
		PromptHash: r.hash, LatencyMS: now.Sub(c.claimedAt).Milliseconds(),
	}
	ctx, cancel := bookkeeping()
	defer cancel()
	if err := c.a.store().RecordAnswer(ctx, rec); err != nil {
		c.s.log.Error("answer not recorded in the ledger", "conversation", c.conv, "err", err)
	}
	m := c.a.s.o.Metrics
	m.Answers.WithLabelValues(r.outcome).Inc()
	attrs := []any{"conversation", c.conv, "message", r.msg, "opener", c.opener, "key", r.key, "attempt", r.no,
		"outcome", r.outcome, "kind", r.kind, "turns", r.stats.Turns, "tool_calls", r.stats.ToolCalls,
		"writes", r.stats.Writes.Sent, "input_tokens", r.stats.In, "output_tokens", r.stats.Out, "cost_pusd", r.stats.Cost}
	if !c.asked.IsZero() {
		notice := c.claimedAt.Sub(c.asked)
		m.AnswerLatency.WithLabelValues("notice").Observe(max(notice, 0).Seconds())
		attrs = append(attrs, "notice_ms", notice.Milliseconds())
		c.asked = time.Time{}
	}
	if r.posted && !r.postAt.IsZero() {
		took := r.postAt.Sub(c.claimedAt)
		m.AnswerLatency.WithLabelValues("answer").Observe(max(took, 0).Seconds())
		attrs = append(attrs, "answer_ms", took.Milliseconds())
	}
	c.s.log.Info("answer", attrs...)
}

// deref is *p, or "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// startOfDay is the start of t's UTC day: where daily quotas start.
func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// nextDay is the start of the UTC day after t's.
func nextDay(t time.Time) time.Time { return startOfDay(t).AddDate(0, 0, 1) }
