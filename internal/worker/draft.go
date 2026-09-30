package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
)

// An answer's draft (docs/design.md §5.3, Drafts). While the model works on
// an answer, whoever reads the conversation sees what it does (its steps:
// thinking, reading a document, …) and, where its adapter streams, the
// round's text as it is written, after the answer so far when the round
// continues one the output cap cut short; the posted answer then takes the
// draft's place, as Core clears it when it posts or proposes the answer.
// The runtime writes it with conversation_draft, where the live catalogue
// has it (core.Catalogue.Drafts); against a Core without it, nothing.
//
// A draft is best effort: nothing the loop does waits for it, and nothing
// that befalls it touches the answer. The loop only changes the draft's
// state, under a lock, and wakes the conversation's drafter, whose own
// goroutine sends the latest state at most every Timing.DraftEvery, one
// write at a time: a state that changes again before it is sent is sent
// once, as it stands then. A write Core refuses as too soon is dropped, as
// is one it refuses because the conversation no longer waits for the
// answer (the answer just went in), and the attempt writes no more; one so
// refused while the attempt is still being written, its answer not yet
// being posted, has the conversation read, for its question may have been
// withdrawn (withdraw.go). One that failed on the way is sent once more,
// with the state as it stands then, and then given up; any other refusal
// stops the attempt's drafts. An attempt that ends without its answer
// posted or proposed (the providers failed, the claim's time ran out, Core
// took another path) is ended with done, which deletes its draft; one
// whose question was withdrawn is not, as Core deleted its draft with the
// question.
//
// A draft carries the model's own text, which Core shows the asker only
// where the answer would be posted without anyone's confirmation, and
// otherwise only to whoever could approve it; the posted answer, made safe
// (safety.Body), replaces it. Steps carry a kind, and for a document or an
// assignment every member of the course can read, its title: never a tool
// result's content, the system prompt, a key, or anything of the memory.
//
// Draft writes are kept off the agent's rate limiter: Core does not count
// a write it carried out against the actor's limit, only its own of 10 a
// second per conversation, which the drafter keeps well under; and with
// one write in flight per conversation, those Core counts until it gives
// them back are at most answer.max_concurrent, within the headroom the
// agent's bucket leaves below Core's limit (ratelimit.CoreShare). Taking
// the agent's tokens would slow its answers and polls for nothing.

const (
	// draftTimeout bounds one draft write.
	draftTimeout = 5 * time.Second
	// maxDraftSteps is the most steps a draft carries, as Core takes them:
	// the latest ones.
	maxDraftSteps = 20
	// maxDraftChars bounds a draft's text, in characters, as Core does.
	maxDraftChars = 20000
	// maxTargetChars bounds a step's target, in characters, as Core does.
	maxTargetChars = 120
)

// What came of a draft write, as draft_writes_total counts it.
const (
	draftSent    = "sent"
	draftDropped = "dropped"
	draftFailed  = "failed"
)

// drafter writes the drafts of one conversation's answers, attempt after
// attempt, for the claims that answer it: the loop changes its state, and
// its goroutine sends it (run). There is one per conversation the agent
// answers, while anything of it is left to send (Agent.drafter); a nil
// drafter, against a Core that takes no drafts, does nothing.
type drafter struct {
	a            *Agent
	course, conv string
	every        time.Duration
	wake         chan struct{}

	mu sync.Mutex
	// cur is the attempt being written; nil between attempts.
	cur *attemptDraft
	// final is the write that ends an attempt (done), sent before
	// anything else.
	final *core.DraftArgs
	// closed: no claim holds the drafter, which ends once it has sent
	// what it has; exited: it has ended, and a claim must start another.
	closed, exited bool
	// Touched by run's goroutine alone.
	last     time.Time
	retrying bool
}

// attemptDraft is one attempt's draft.
type attemptDraft struct {
	id      string
	version int64
	steps   []*core.DraftStep
	// calls are the steps of the round's tool calls, by call id.
	calls map[string]*core.DraftStep
	// text is the round's text as the model writes it, after kept: the
	// answer so far, when the round continues one the output cap cut
	// short (continue.go).
	kept string
	text strings.Builder
	// changed: since it was last sent; sent: a write of it was sent;
	// textSent: one carried text, so that text cleared is sent cleared.
	changed, sent, textSent bool
	// held: nothing more of it is sent, its answer being posted;
	// refused: Core refused it for good, and needs no done.
	held, refused bool
}

// drafter is the drafter of conv in course, running; nil when Core takes
// no drafts. A drafter still sending what an earlier claim left is taken
// on, so that a conversation never has two writes in flight.
func (a *Agent) drafter(course, conv string) *drafter {
	if !a.drafts {
		return nil
	}
	a.draftMu.Lock()
	defer a.draftMu.Unlock()
	if d := a.drafters[conv]; d != nil && d.reopen() {
		return d
	}
	d := &drafter{a: a, course: course, conv: conv, every: a.s.o.Timing.DraftEvery, wake: make(chan struct{}, 1)}
	if a.drafters == nil {
		a.drafters = map[string]*drafter{}
	}
	a.drafters[conv] = d
	// Its claim holds a place in answers until it hands the drafter back,
	// so the count is not zero here: the agent waits for the last writes
	// of a drafter as for an answer.
	a.answers.Add(1)
	go func() {
		defer a.answers.Done()
		d.run(a.answerCtx)
		a.draftMu.Lock()
		if a.drafters[conv] == d {
			delete(a.drafters, conv)
		}
		a.draftMu.Unlock()
	}()
	return d
}

// reopen takes on d for a new claim, unless it has ended.
func (d *drafter) reopen() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.exited {
		return false
	}
	d.closed = false
	return true
}

// close hands d back: it ends once it has sent what it has.
func (d *drafter) close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.poke()
}

// poke wakes d's goroutine, without waiting.
func (d *drafter) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// change applies f to the attempt being written, if any and not held, and
// wakes d to send it.
func (d *drafter) change(f func(c *attemptDraft) bool) {
	if d == nil {
		return
	}
	d.mu.Lock()
	c := d.cur
	changed := c != nil && !c.held && f(c)
	if changed {
		c.changed = true
	}
	d.mu.Unlock()
	if changed {
		d.poke()
	}
}

// begin starts an attempt: a draft of its own, under an id of its own.
func (d *drafter) begin() {
	if d == nil {
		return
	}
	d.end(false) // an attempt left open is over
	d.mu.Lock()
	d.cur = &attemptDraft{id: uuid.NewString(), calls: map[string]*core.DraftStep{}}
	d.mu.Unlock()
}

// round is a model call starting: the steps before it done, a thinking
// step, and no text yet.
func (d *drafter) round() {
	d.change(func(c *attemptDraft) bool {
		c.finish()
		c.add(&core.DraftStep{Kind: core.StepThinking, State: core.StepRunning})
		c.kept = ""
		c.text.Reset()
		return true
	})
}

// continues is a model call starting that continues the answer, whose
// text so far is sofar: the draft shows it, and what the call writes
// after it; its steps are left as they are, the answer still being
// written.
func (d *drafter) continues(sofar string) {
	d.change(func(c *attemptDraft) bool {
		if c.kept == sofar && c.text.Len() == 0 {
			return false
		}
		c.kept = sofar
		c.text.Reset()
		return true
	})
}

// again is the model call being made again (after a retryable failure, or
// by the fallback): the text it had written so far is no answer, though
// the answer it continues, if any, still is.
func (d *drafter) again() {
	d.change(func(c *attemptDraft) bool {
		if c.text.Len() == 0 {
			return false
		}
		c.text.Reset()
		return true
	})
}

// text adds a piece of the round's text as the model writes it: the first
// piece ends the thinking step and starts a writing one. It is the
// adapter's llm.TextFunc, and returns at once.
func (d *drafter) text(delta string) {
	if delta == "" {
		return
	}
	d.change(func(c *attemptDraft) bool {
		if n := len(c.steps); n == 0 || c.steps[n-1].Kind != core.StepWriting || c.steps[n-1].State != core.StepRunning {
			c.finish()
			c.add(&core.DraftStep{Kind: core.StepWriting, State: core.StepRunning})
		}
		c.text.WriteString(delta)
		return true
	})
}

// calls are the round's tool calls starting: the steps before them done,
// and a step running for each.
func (d *drafter) calls(calls []llm.Part) {
	d.change(func(c *attemptDraft) bool {
		c.finish()
		clear(c.calls)
		for _, call := range calls {
			if call.Type != llm.PartToolCall {
				continue
			}
			s := &core.DraftStep{Kind: stepKind(call.Name), State: core.StepRunning}
			c.add(s)
			c.calls[call.ID] = s
		}
		return true
	})
}

// seen is a call Core answered (toolset.Runner.Seen): its step done, with
// what it was about where Core's answer says (draftTarget).
func (d *drafter) seen(call llm.Part, env *core.Envelope) {
	target := draftTarget(call.Name, env)
	d.change(func(c *attemptDraft) bool {
		s := c.calls[call.ID]
		if s == nil {
			return false
		}
		s.State, s.Target = core.StepDone, target
		return true
	})
}

// callsDone is the round's calls all answered, or refused before Core.
func (d *drafter) callsDone() {
	d.change(func(c *attemptDraft) bool {
		return c.finish()
	})
}

// hold stops the attempt's writes: its answer is being posted, which
// takes the draft's place.
func (d *drafter) hold() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.cur != nil {
		d.cur.held = true
	}
	d.mu.Unlock()
}

// withdraw stops the attempt's writes, its end included: its question was
// withdrawn, which deletes its draft in Core (a Core before AIShie-Core
// #42 keeps it until it goes stale).
func (d *drafter) withdraw() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.cur; c != nil {
		c.held, c.refused = true, true
	}
}

// end ends the attempt. Its answer posted or proposed, Core has cleared
// its draft; otherwise the draft is ended with done, when any of it was
// sent and Core has not refused it for good.
func (d *drafter) end(posted bool) {
	if d == nil {
		return
	}
	d.mu.Lock()
	c := d.cur
	d.cur = nil
	queued := c != nil && !posted && c.sent && !c.refused
	if queued {
		c.version++
		d.final = &core.DraftArgs{CourseID: d.course, ConversationID: d.conv, Attempt: c.id, Version: c.version, Done: true}
	}
	d.mu.Unlock()
	if queued {
		d.poke()
	}
}

// finish marks every running step done, and reports whether any was.
func (c *attemptDraft) finish() bool {
	was := false
	for _, s := range c.steps {
		if s.State == core.StepRunning {
			s.State, was = core.StepDone, true
		}
	}
	return was
}

// add appends a step, keeping the latest maxDraftSteps.
func (c *attemptDraft) add(s *core.DraftStep) {
	c.steps = append(c.steps, s)
	if n := len(c.steps); n > maxDraftSteps {
		c.steps = append([]*core.DraftStep(nil), c.steps[n-maxDraftSteps:]...)
	}
}

// run sends d's writes until it is closed with nothing left to send, or
// ctx ends.
func (d *drafter) run(ctx context.Context) {
	defer d.exit()
	for {
		select {
		case <-d.wake:
		case <-ctx.Done():
			return
		}
		for {
			if wait := time.Until(d.last.Add(d.every)); wait > 0 && !sleep(ctx, wait) {
				return
			}
			args, ok := d.next()
			if !ok {
				break
			}
			d.write(ctx, args)
		}
		if d.idle() {
			return
		}
	}
}

// exit marks d ended, for Agent.drafter to start another.
func (d *drafter) exit() {
	d.mu.Lock()
	d.exited = true
	d.mu.Unlock()
}

// idle reports whether d is closed with nothing left to send.
func (d *drafter) idle() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.cur
	return d.closed && d.final == nil && (c == nil || c.held || !c.changed)
}

// next is the write to send now: an attempt's end first, else the attempt's
// state as it stands, if it changed since it was last sent.
func (d *drafter) next() (core.DraftArgs, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f := d.final; f != nil {
		d.final = nil
		return *f, true
	}
	c := d.cur
	if c == nil || c.held || !c.changed {
		return core.DraftArgs{}, false
	}
	c.version++
	c.changed, c.sent = false, true
	args := core.DraftArgs{CourseID: d.course, ConversationID: d.conv, Attempt: c.id, Version: c.version,
		Steps: make([]core.DraftStep, 0, len(c.steps))}
	for _, s := range c.steps {
		args.Steps = append(args.Steps, *s)
	}
	if text := c.kept + c.text.String(); text != "" || c.textSent {
		text = cutChars(text, maxDraftChars)
		args.Text, c.textSent = &text, true
	}
	return args, true
}

// write sends one write, and acts on what came of it.
func (d *drafter) write(ctx context.Context, args core.DraftArgs) {
	d.last = time.Now()
	wctx, cancel := context.WithTimeout(ctx, draftTimeout)
	env, err := d.a.client.Draft(wctx, args)
	cancel()
	if ctx.Err() != nil {
		return // the agent is stopping: nothing to count
	}
	outcome, code := draftOutcome(env, err)
	switch outcome {
	case draftSent, draftDropped:
		d.retrying = false
		if code == reasonNotAwaiting && d.refuse(args.Attempt) {
			// Refused while its attempt was being written, not for its
			// answer going in: its question may have been withdrawn.
			d.a.draftRefused(d.conv)
		}
	case "retry":
		if !d.retrying {
			d.retrying = true
			d.retry(args)
			return
		}
		d.retrying, outcome = false, draftFailed
	default:
		d.retrying = false
		d.refuse(args.Attempt)
		d.a.log.Info("a draft of an answer was refused: its attempt writes no more", "conversation", d.conv, "code", code)
	}
	d.a.countDraft(outcome)
}

// retry sends a write that failed on the way once more: an attempt's end
// as it was, an attempt's state as it stands by then.
func (d *drafter) retry(args core.DraftArgs) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch c := d.cur; {
	case args.Done:
		if d.final == nil {
			d.final = &args
		}
	case c != nil && c.id == args.Attempt:
		c.changed = true
	}
}

// refuse stops an attempt Core refused for good: nothing more of it is
// sent, not even its end. It reports whether the attempt was still being
// written, its answer not being posted (hold).
func (d *drafter) refuse(attempt string) (writing bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.cur; c != nil && c.id == attempt {
		writing = !c.held
		c.held, c.refused = true, true
	}
	return writing
}

// reasonNotAwaiting is Core's refusal of a draft for a conversation that
// no longer waits for an answer.
const reasonNotAwaiting = "conversation_not_awaiting"

// draftOutcome is what came of a draft write: sent, dropped (too soon, or
// the conversation no longer waits for its answer), retry (it failed on the
// way, and may go through if sent again), or failed; and Core's code, or
// its reason where it gave one.
func draftOutcome(env *core.Envelope, err error) (string, string) {
	var rl *core.RateLimitedError
	var te *core.TransientError
	switch {
	case errors.As(err, &rl):
		return draftDropped, core.CodeRateLimited
	case errors.As(err, &te), errors.Is(err, context.DeadlineExceeded):
		return "retry", "unreachable"
	case err != nil, env == nil:
		return draftFailed, "error"
	case env.Status == core.StatusExecuted:
		return draftSent, ""
	}
	code := env.Code()
	switch {
	case code == core.CodeRateLimited:
		return draftDropped, code
	case env.Reason() == reasonNotAwaiting:
		return draftDropped, reasonNotAwaiting
	case env.Status == core.StatusError && code == core.CodeInternal:
		return "retry", code
	case env.Reason() != "":
		return draftFailed, env.Reason()
	}
	return draftFailed, code
}

// countDraft counts a draft write's outcome, in the metrics and for the
// agent's status.
func (a *Agent) countDraft(outcome string) {
	a.s.o.Metrics.DraftWrites.WithLabelValues(a.id, outcome).Inc()
	switch outcome {
	case draftSent:
		a.draftCounts.sent.Add(1)
	case draftDropped:
		a.draftCounts.dropped.Add(1)
	case draftFailed:
		a.draftCounts.failed.Add(1)
	}
}

// stepKind is the kind of the step a call of tool is.
func stepKind(tool string) string {
	switch tool {
	case "document_get", toolset.AttachmentTool:
		return core.StepReadingDocument
	case "document_list":
		return core.StepListingDocuments
	case "assignment_get":
		return core.StepReadingAssignment
	case "submission_get":
		return core.StepReadingSubmission
	case "memory_search":
		return core.StepSearchingMemory
	}
	return core.StepTool
}

// draftTarget is what a call of tool was about, as Core's answer to it
// says: the title of a document or an assignment that every member of the
// course may read, as plain text on one line: published course material,
// or a published assignment. Anything else, a rubric's title or a
// submission's among them, would tell the asker of what they may not see;
// and nothing else of a result goes into a draft.
func draftTarget(tool string, env *core.Envelope) string {
	if env == nil || env.Status != core.StatusExecuted {
		return ""
	}
	var r struct {
		Title              string  `json:"title"`
		Kind               string  `json:"kind"`
		Status             string  `json:"status"`
		PublishedVersionID *string `json:"published_version_id"`
		PublishedAt        *string `json:"published_at"`
	}
	if env.Decode(&r) != nil {
		return ""
	}
	switch tool {
	case "document_get":
		if r.Kind != "material" || r.PublishedVersionID == nil || r.Status == "archived" {
			return ""
		}
	case "assignment_get":
		if r.PublishedAt == nil {
			return ""
		}
	default:
		return ""
	}
	return plainLine(r.Title, maxTargetChars)
}

// plainLine is s on one line, without control characters or runs of
// spaces, cut to at most n characters.
func plainLine(s string, n int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return cutChars(s, n-1) + "…"
}

// cutChars is s cut to its first n characters.
func cutChars(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}
