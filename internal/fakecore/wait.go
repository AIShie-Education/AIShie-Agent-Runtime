package fakecore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Waiting for news, as Core has done since 2c1fe1b (its package wake and
// pipeline.invokeRead): conversation.inbox, conversation.messages and
// event.list take wait_s, 0 to 25, and a call that finds nothing new waits,
// holding no lock, until news it would read is flushed to a course's feed or
// its time is up, reads again as it read the first time, and answers once it
// finds something, or at the end of its time with what it then finds. What
// wakes each is Core's filter: an inbox, its seat's conversations' messages
// and decided proposals; a conversation, any news of it; a feed, any news
// of its course. A change that is no news (a seat's perms, a token revoked)
// is seen when the call reads again at the end of its time. A call past the
// bounds on calls waiting (Options.LongPollWaiters and
// LongPollWaitersPerActor), or while the fake shuts down, answers at once
// with what it read; one whose client has gone answers what it last read.
// However long it waits, it is one call to the rate limit.
//
// Options.WithoutWait answers as a Core from before it, as 1bcb5ef, the
// runtime's pin before 2c1fe1b, was: its catalogue has no wait_s.

// canWait is embedded by the input of a read that can wait (Core's
// tool.CanWait).
type canWait struct {
	WaitS int `json:"wait_s,omitempty"`
}

func (w canWait) waitSeconds() int { return w.WaitS }

type waitable interface{ waitSeconds() int }

// waits is what a read that can wait waits for (Core's tool.Waiting): the
// news that wakes it (For), and whether what it read now, the tool's result
// as JSON, is nothing new to its caller, first being what it read first
// (Nothing): a call waits only while it is.
type waits struct {
	For     func(c *Core, caller *actor, in any) filter
	Nothing func(in any, first, now json.RawMessage) bool
}

// inboxNews is the news that can put a conversation in its respondent's
// inbox (Core's inboxNews): a message, and a proposed answer decided.
var inboxNews = []string{"conversation.message_posted", "action.approved", "action.rejected", "action.cancelled"}

// waitingTools are the reads that can wait, by registry name.
var waitingTools = map[string]waits{
	// The inbox waits while it is empty, for news of its seat's
	// conversations.
	"conversation.inbox": {
		For: func(c *Core, caller *actor, in any) filter {
			f := filter{course: in.(inboxIn).CourseID.String(), kinds: inboxNews}
			if m := c.seatOf(caller, c.courses[f.course]); m != nil {
				f.respondent = m.id
			}
			return f
		},
		Nothing: func(_ any, _, now json.RawMessage) bool { return emptyList(now, "conversations") },
	},
	// The feed waits while it has no event the caller may see, for any
	// news of its course.
	"event.list": {
		For:     func(_ *Core, _ *actor, in any) filter { return filter{course: in.(eventListIn).CourseID.String()} },
		Nothing: func(_ any, _, now json.RawMessage) bool { return emptyList(now, "events") },
	},
	// A conversation waits while no message comes after after_seq, it
	// stands as it did when the call first read it, and in the state the
	// caller last saw (seen_state), for any news of it.
	"conversation.messages": {
		For: func(_ *Core, _ *actor, in any) filter {
			mi := in.(messagesIn)
			return filter{course: mi.CourseID.String(), conversation: mi.ConversationID.String()}
		},
		Nothing: func(in any, first, now json.RawMessage) bool {
			if !emptyList(now, "messages") {
				return false
			}
			var a, b struct {
				Conversation standing `json:"conversation"`
			}
			if json.Unmarshal(first, &a) != nil || json.Unmarshal(now, &b) != nil {
				return false
			}
			if mi, ok := in.(messagesIn); ok && mi.SeenState != nil && *mi.SeenState != b.Conversation.State {
				return false
			}
			return reflect.DeepEqual(a.Conversation, b.Conversation)
		},
	},
}

// filter is the news a waiting call is woken by (Core's wake.Filter): of
// its course, and, where they are set, of one conversation, of
// conversations addressed to one seat, and of some kinds alone.
type filter struct {
	course, conversation, respondent string
	kinds                            []string
}

// note is one piece of news (Core's wake.Note): an event's course and
// type, and, for news of a conversation or of a proposal to write in one,
// the conversation and the seat it is addressed to.
type note struct {
	course, kind, conversation, respondent string
}

func (f filter) matches(n note) bool {
	switch {
	case n.course != f.course:
		return false
	case f.conversation != "" && n.conversation != f.conversation:
		return false
	case f.respondent != "" && n.respondent != f.respondent:
		return false
	case len(f.kinds) > 0 && !slices.Contains(f.kinds, n.kind):
		return false
	}
	return true
}

// waiter is one waiting call's place.
type waiter struct {
	actor string
	f     filter
	// woken holds one wake-up: any that come while one is held are the
	// same news to a call about to read again.
	woken chan struct{}
}

// standing is as much of a conversation's view as a reader waiting on it is
// told of when it changes, though no message is written (Core's
// sameStanding): its state, an answer waiting for approval, a retraction,
// and why it closed.
type standing struct {
	State                string  `json:"state"`
	Status               string  `json:"status"`
	PendingReplyActionID *string `json:"pending_reply_action_id"`
	LastRetractedAt      *string `json:"last_retracted_at"`
	ClosedReason         *string `json:"closed_reason"`
}

// emptyList reports whether result's list under key is empty.
func emptyList(result json.RawMessage, key string) bool {
	var r map[string]json.RawMessage
	if json.Unmarshal(result, &r) != nil {
		return false
	}
	list := bytes.TrimSpace(r[key])
	return len(list) == 0 || bytes.Equal(list, []byte("[]")) || bytes.Equal(list, []byte("null"))
}

// conversationStates are the states a conversation's view is in, which
// seen_state names one of (Core's conversationViewStates).
var conversationStates = []string{"awaiting_answer", "reply_pending_approval", "answered", "closed"}

// withoutWait is the catalogue raw as a Core from before wait_s served it:
// no wait_s, and no seen_state, in any tool's input.
func withoutWait(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("fakecore: the catalogue: %w", err)
	}
	tools, _ := doc["tools"].([]any)
	found := 0
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		in, _ := tool["input_schema"].(map[string]any)
		props, _ := in["properties"].(map[string]any)
		if _, ok := props["wait_s"]; ok {
			delete(props, "wait_s")
			delete(props, "seen_state")
			found++
		}
	}
	if found != len(waitingTools) {
		return nil, fmt.Errorf("fakecore: the catalogue has wait_s in %d tools, not %d", found, len(waitingTools))
	}
	return json.Marshal(doc)
}

// errNoWait is a waiting call refused a place: the fake is shutting down,
// or too many calls wait already.
var errNoWait = errors.New("no place to wait")

// subscribe gives a call of actor's a place to wait for news f matches.
// The lock is held.
func (c *Core) subscribe(actor string, f filter) (*waiter, error) {
	perActor := 0
	for w := range c.waiters {
		if w.actor == actor {
			perActor++
		}
	}
	if c.shut || c.opts.LongPollWaiters < 0 || c.opts.LongPollWaitersPerActor < 0 ||
		len(c.waiters) >= c.opts.LongPollWaiters || perActor >= c.opts.LongPollWaitersPerActor {
		return nil, errNoWait
	}
	w := &waiter{actor: actor, f: f, woken: make(chan struct{}, 1)}
	c.waiters[w] = struct{}{}
	return w, nil
}

// Waiting is how many calls wait for news now.
func (c *Core) Waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// WaitingOf is how many of actor's calls wait for news now.
func (c *Core) WaitingOf(actor string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for w := range c.waiters {
		if w.actor == actor {
			n++
		}
	}
	return n
}

// Shutdown ends every wait, now and to come, as Core's does when it stops:
// the calls waiting answer with what they read, and a call that asks to wait
// answers at once. httptest.Server.Close waits for the calls in progress;
// call Shutdown first.
func (c *Core) Shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.shut {
		c.shut = true
		close(c.shutdown)
	}
}

// newsFlushed wakes the calls waiting for news evs are, as Core notifies
// them once its transaction commits. The lock is held.
func (c *Core) newsFlushed(evs []*event) {
	for _, e := range evs {
		if e.course == nil {
			continue
		}
		n := note{course: e.course.id, kind: e.typ}
		switch {
		case e.subjectType == "conversation" && e.subjectID != nil:
			n.conversation = *e.subjectID
		case strings.HasPrefix(e.typ, "action.") && e.actionID != nil:
			if a := c.actions[*e.actionID]; a != nil && a.targetType == "conversation" && a.targetID != nil {
				n.conversation = *a.targetID
			}
		}
		if cv := c.conversations[n.conversation]; cv != nil {
			n.respondent = cv.respondent.id
		}
		for w := range c.waiters {
			if w.f.matches(n) {
				select {
				case w.woken <- struct{}{}:
				default:
				}
			}
		}
	}
}

// why a wait ended.
type why int

const (
	woken why = iota
	timedOut
	cancelled
	shutDown
)

// waitNews waits for news until then, and says why it stopped.
func waitNews(ctx context.Context, news, shutdown <-chan struct{}, until time.Time) why {
	t := time.NewTimer(time.Until(until))
	defer t.Stop()
	select {
	case <-news:
		return woken
	case <-ctx.Done():
		return cancelled
	case <-shutdown:
		return shutDown
	case <-t.C:
		return timedOut
	}
}

// waitForNews is what a call of a read that can wait does after its first
// read, first (Core's invokeRead): if it asked to wait (wait_s), read
// something, and found nothing new, it waits for news without the lock,
// and reads again each time it is woken, until it finds something or its
// time is up; then it reads once more and answers that. A call whose ctx
// ends answers what it last read; one that cannot wait, what it first
// read. The lock is held on entry and on return; ctx is the request's.
func (c *Core) waitForNews(ctx context.Context, caller *actor, t *toolDef, args []byte, base string, first outcome) outcome {
	wt, ok := waitingTools[t.Name]
	if !ok || t.impl == nil || first.Status != actExecuted || ctx == nil {
		return first
	}
	in, err := t.decodeArgs(args)
	if err != nil {
		return first
	}
	secs, ok := in.(waitable)
	if !ok || secs.waitSeconds() <= 0 || !wt.Nothing(in, first.Result, first.Result) {
		return first
	}
	w, err := c.subscribe(caller.id, wt.For(c, caller, in))
	if err != nil {
		return first
	}
	defer delete(c.waiters, w)
	until := time.Now().Add(time.Duration(secs.waitSeconds()) * time.Second)
	last := first
	for {
		shutdown := c.shutdown
		c.mu.Unlock()
		stop := waitNews(ctx, w.woken, shutdown, until)
		c.mu.Lock()
		if stop == cancelled {
			return last
		}
		out := c.invoke(caller, t, args, "", base)
		if out.Status != actExecuted || stop != woken || !wt.Nothing(in, first.Result, out.Result) || !time.Now().Before(until) {
			return out
		}
		last = out
	}
}
