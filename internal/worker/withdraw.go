package worker

import (
	"context"
	"errors"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

// An answer whose question is withdrawn stops (docs/design.md §5.3, A
// question withdrawn). The opener withdraws what they asked ("stop" in the
// chat) by retracting it, and the answer being written to it stops at once:
// its model call is cancelled, so that a provider streaming it stops too,
// and its tool calls with it; nothing more of its draft is written, not
// even its end, as Core deletes the draft with the retraction; nothing is
// posted, and nothing more is tried at it. Its ledger row says dropped.
//
// The retraction is read from the seat's events
// (conversation.message_retracted), which the seat long-polls while it
// writes an answer, where Core lets it (eventsWait). A draft Core refuses
// because the conversation waits for no answer, while its attempt is still
// being written, may be the same news come another way, when the event is
// late: the conversation is read, once per answer, and the answer stopped
// if its question is withdrawn. A retraction of any other message, an older
// one of the opener's or an answer of the agent's, stops nothing.
//
// Once the answer is being sent, a withdrawal leaves it be: Core orders the
// two. Sent before the retraction, it is posted; after, a Core since
// AIShie-Core #42 refuses it as moved_on naming no message, and the pinned
// Core, b0eb848, posts it.

// errWithdrawn is the cause an answer being written is stopped with: its
// question was withdrawn.
var errWithdrawn = errors.New("the question was withdrawn")

// retractedFor is how long a retraction read is remembered (withdraw), for
// an answer to that message that begins after it was read: one whose claim
// read the conversation just before the retraction.
const retractedFor = time.Minute

// underWay is an answer being written, from its model loop's start to its
// end: the question it answers, and how to stop it.
type underWay struct {
	course, conv, msg string
	ctx               context.Context
	stop              context.CancelCauseFunc
	d                 *drafter
	// withdrawn: it was stopped, its question withdrawn, as seen says
	// (the event, or a draft refused); checked: a draft refused had the
	// conversation read.
	withdrawn, checked bool
	seen               string
}

// writing marks the answer to msg in conv as being written, until written,
// and returns the context it is to be written under: ctx's, cancelled with
// errWithdrawn if its question is withdrawn meanwhile, or was, as far as
// the events read lately say. d is its drafter.
func (a *Agent) writing(ctx context.Context, course, conv, msg string, d *drafter) (*underWay, context.Context) {
	wctx, stop := context.WithCancelCause(ctx)
	u := &underWay{course: course, conv: conv, msg: msg, ctx: wctx, stop: stop, d: d}
	a.wayMu.Lock()
	defer a.wayMu.Unlock()
	if a.underWay == nil {
		a.underWay = map[string]*underWay{}
	}
	a.underWay[conv] = u
	a.forgetRetracted(a.now())
	if _, ok := a.retracted[msg]; ok {
		a.stopLocked(u, "event")
	}
	return u, wctx
}

// written ends u: nothing stops it from now on, and what it wrote is sent,
// unless its question was withdrawn before, which it reports, and how that
// was seen.
func (a *Agent) written(u *underWay) (withdrawn bool, seen string) {
	a.wayMu.Lock()
	if a.underWay[u.conv] == u {
		delete(a.underWay, u.conv)
	}
	withdrawn, seen = u.withdrawn, u.seen
	a.wayMu.Unlock()
	u.stop(nil)
	return withdrawn, seen
}

// withdraw stops the answer being written to msg in conv, if one is, and
// remembers msg a while, for one that begins after: msg was retracted.
func (a *Agent) withdraw(conv, msg string) {
	now := a.now()
	a.wayMu.Lock()
	defer a.wayMu.Unlock()
	a.forgetRetracted(now)
	if a.retracted == nil {
		a.retracted = map[string]time.Time{}
	}
	a.retracted[msg] = now
	if u := a.underWay[conv]; u != nil && u.msg == msg {
		a.stopLocked(u, "event")
	}
}

// forgetRetracted forgets the retractions read more than retractedFor
// before now. a.wayMu is held.
func (a *Agent) forgetRetracted(now time.Time) {
	for id, at := range a.retracted {
		if now.Sub(at) > retractedFor {
			delete(a.retracted, id)
		}
	}
}

// stopWithdrawn stops u, its question withdrawn, unless it has ended.
func (a *Agent) stopWithdrawn(u *underWay, seen string) {
	a.wayMu.Lock()
	defer a.wayMu.Unlock()
	if a.underWay[u.conv] == u {
		a.stopLocked(u, seen)
	}
}

// stopLocked stops u, its question withdrawn, as seen says, once: its
// context is cancelled, and its draft writes nothing more. a.wayMu is held.
func (a *Agent) stopLocked(u *underWay, seen string) {
	if u.withdrawn {
		return
	}
	u.withdrawn, u.seen = true, seen
	u.d.withdraw()
	u.stop(errWithdrawn)
}

// draftRefused is a draft of conv Core refused as the conversation waiting
// for no answer, while its attempt was still being written: the answer
// being written, if any, has its conversation read, once, and is stopped
// if its question was withdrawn. The read is its claim's, and ends with it.
func (a *Agent) draftRefused(conv string) {
	a.wayMu.Lock()
	u := a.underWay[conv]
	check := u != nil && !u.withdrawn && !u.checked
	if check {
		u.checked = true
		// u's claim holds a place in answers until it ends u, which it
		// has not: the count is not zero here.
		a.answers.Add(1)
	}
	a.wayMu.Unlock()
	if !check {
		return
	}
	go func() {
		defer a.answers.Done()
		read, err := a.client.Messages(core.WithPriority(u.ctx, core.PriorityAnswer), u.course, u.conv, core.MessagesQuery{Limit: recentMessages})
		if err == nil && questionWithdrawn(read) {
			a.stopWithdrawn(u, "draft")
		}
	}()
}
