package fakecore

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// conversation.draft, an answer's draft while it is written, with Core's
// semantics (Core's internal/tools/draft.go, docs/schema.md §2.8): an
// ephemeral write, authorized as a write is and carried out at once at any
// level above denied, recording nothing (no action, no idempotency key,
// never a proposal, no event), and, carried out, not counted against the
// caller's rate limit; its own is 10 writes a second per conversation. Only
// the respondent writes it, while the conversation waits for its answer
// and its opener may still address the respondent. A write replaces the
// draft kept if it is newer (PutDraft): none kept, or one gone stale; one
// of another attempt, unless it ends an attempt that is not the one kept;
// of the same attempt not ended, a higher version, or for its end, one not
// lower. Text and steps left out keep the attempt's. Posting or proposing
// the answer, the opener withdrawing the question (retracting their latest
// message), closing the conversation, and done clear it; one not written
// for 120 seconds is none.
//
// Options.WithoutDraft answers as a Core from before it, as 2c1fe1b, the
// runtime's pin before drafts, was.

const (
	draftTTL             = 120 * time.Second
	draftWritesPerSecond = 10
	maxDraftAttemptChars = 64
	maxDraftSteps        = 20
	maxStepTargetChars   = 120
)

var (
	draftStepKinds = []string{"thinking", "reading_document", "listing_documents", "reading_assignment", "reading_submission",
		"searching_memory", "writing", "tool"}
	draftStepStates = []string{"running", "done"}

	errNotTheRespondent = forbid("only the member a conversation is addressed to writes its answer's draft").
				with("reason", "not_the_respondent")
	errNotAwaiting = conflicts("the conversation waits for no answer now: it is answered, its question withdrawn, an "+
		"answer waits for approval, or it is closed; a draft is written only while it waits").with("reason", "conversation_not_awaiting")
)

// kindEphemeral is the catalogue's kind of a tool that changes state that
// is no action.
const kindEphemeral = "ephemeral"

// DraftStep is one step of a draft.
type DraftStep struct {
	Kind   string  `json:"kind"`
	Target *string `json:"target,omitempty"`
	State  string  `json:"state"`
}

type draftIn struct {
	inCourse
	ConversationID uuid.UUID   `json:"conversation_id"`
	Attempt        string      `json:"attempt"`
	Version        int64       `json:"version"`
	Text           *string     `json:"text,omitempty"`
	Steps          []DraftStep `json:"steps,omitempty"`
	Done           bool        `json:"done,omitempty"`
}

type draftOut struct {
	Stored  bool  `json:"stored"`
	Version int64 `json:"version"`
}

// draft is a conversation's draft row: done keeps an ended attempt's row,
// empty, so that a write of it that comes late is passed over.
type draft struct {
	attempt   string
	version   int64
	text      *string
	steps     []DraftStep
	done      bool
	updatedAt time.Time
}

// DraftWrite is one conversation_draft call the fake carried out, stored
// or not, for tests to look at.
type DraftWrite struct {
	ConversationID string
	Attempt        string
	Version        int64
	Text           *string
	Steps          []DraftStep
	Done           bool
	Stored         bool
	At             time.Time
}

// Draft is a conversation's draft as it stands.
type Draft struct {
	Attempt   string
	Version   int64
	Text      *string
	Steps     []DraftStep
	UpdatedAt time.Time
}

func conversationDraft() *impl {
	return define(spec[draftIn]{
		gate: gateAnswers,
		resolve: func(c *Core, co *course, in draftIn) (target, error) {
			return conversationTarget(c, co, in.ConversationID)
		},
		execute: func(c *Core, ec *execCtx, in draftIn) (any, error) {
			steps, err := checkDraft(in)
			if err != nil {
				return nil, err
			}
			cv, err := c.findConversation(ec.course, in.ConversationID)
			if err != nil {
				return nil, err
			}
			if cv.respondent != ec.member {
				return nil, errNotTheRespondent
			}
			if wait := c.draftTooSoon(cv, ec.now); wait > 0 {
				secs := max(int(math.Ceil(wait.Seconds())), 1)
				return nil, newErr(codeRateLimited, "a conversation's draft is written at most %d times a second; try again in %d seconds",
					draftWritesPerSecond, secs).with("reason", "draft_rate").with("retry_after_seconds", secs)
			}
			if c.view(cv).State != stateAwaitingAnswer {
				return nil, errNotAwaiting
			}
			if why := refusal(cv.opener, ec.member, ec.now); why != "" {
				return nil, notAddressable("you may no longer answer in this conversation", why)
			}
			w := DraftWrite{ConversationID: cv.id, Attempt: in.Attempt, Version: in.Version, Text: in.Text, Steps: steps,
				Done: in.Done, At: ec.now}
			out := c.putDraft(cv, in, steps, ec.now)
			w.Stored = out.Stored
			c.draftWrites = append(c.draftWrites, w)
			if out.Stored {
				c.draftNews(cv)
			}
			return out, nil
		},
	})
}

// checkDraft holds a draft to what Core takes, and returns its steps as
// they are kept, trimmed; nil when the write keeps the attempt's.
func checkDraft(in draftIn) ([]DraftStep, error) {
	if n := utf8.RuneCountInString(in.Attempt); n < 1 || n > maxDraftAttemptChars || !plainLine(in.Attempt) {
		return nil, invalid("attempt is 1 to %d characters of plain text", maxDraftAttemptChars).with("field", "attempt")
	}
	if in.Version < 1 {
		return nil, invalid("version is 1 or more").with("field", "version")
	}
	if in.Text != nil {
		if n := utf8.RuneCountInString(*in.Text); n > maxMessageChars {
			return nil, invalid("the text is %d characters long; the most is %d", n, maxMessageChars).with("field", "text")
		}
	}
	if in.Steps == nil {
		return nil, nil
	}
	if len(in.Steps) > maxDraftSteps {
		return nil, invalid("at most %d steps", maxDraftSteps).with("field", "steps")
	}
	steps := make([]DraftStep, len(in.Steps))
	for i, s := range in.Steps {
		if !slices.Contains(draftStepKinds, s.Kind) {
			return nil, invalid("a step's kind is one of %s", strings.Join(draftStepKinds, ", ")).with("field", "steps")
		}
		if !slices.Contains(draftStepStates, s.State) {
			return nil, invalid("a step's state is running or done").with("field", "steps")
		}
		steps[i] = DraftStep{Kind: s.Kind, State: s.State}
		if s.Target != nil {
			t := strings.TrimSpace(*s.Target)
			if utf8.RuneCountInString(t) > maxStepTargetChars || !plainLine(t) {
				return nil, invalid("a step's target is plain text on one line, at most %d characters", maxStepTargetChars).
					with("field", "steps")
			}
			if t != "" {
				steps[i].Target = &t
			}
		}
	}
	return steps, nil
}

func plainLine(s string) bool { return !strings.ContainsFunc(s, unicode.IsControl) }

// draftTooSoon is how long until cv's draft may be written again, 0 when
// it may now: at most draftWritesPerSecond in any second.
func (c *Core) draftTooSoon(cv *conversation, now time.Time) time.Duration {
	recent := cv.draftTimes[:0]
	for _, t := range cv.draftTimes {
		if now.Sub(t) < time.Second {
			recent = append(recent, t)
		}
	}
	cv.draftTimes = recent
	if len(recent) >= draftWritesPerSecond {
		return time.Second - now.Sub(recent[0])
	}
	cv.draftTimes = append(cv.draftTimes, now)
	return 0
}

// putDraft writes in if it is newer than the draft kept (Core's PutDraft),
// and says what is kept now.
func (c *Core) putDraft(cv *conversation, in draftIn, steps []DraftStep, now time.Time) draftOut {
	d := cv.draft
	fresh := d != nil && !d.updatedAt.Before(now.Add(-draftTTL))
	newer := !fresh ||
		(d.attempt != in.Attempt && !in.Done) ||
		(d.attempt == in.Attempt && !d.done && (d.version < in.Version || (in.Done && d.version <= in.Version)))
	if !newer {
		// The version a reader finds: none for a draft gone stale, or an
		// attempt that is over.
		if !fresh || d.done {
			return draftOut{}
		}
		return draftOut{Version: d.version}
	}
	next := &draft{attempt: in.Attempt, version: in.Version, done: in.Done, updatedAt: now, text: in.Text, steps: steps}
	switch {
	case in.Done:
		next.text, next.steps = nil, nil
	case fresh && d.attempt == in.Attempt:
		if in.Text == nil {
			next.text = d.text
		}
		if steps == nil {
			next.steps = d.steps
		}
	}
	cv.draft = next
	if in.Done {
		return draftOut{Stored: true} // the attempt is over: no draft to see
	}
	return draftOut{Stored: true, Version: in.Version}
}

// draftOf is cv's draft, if it has one: written within draftTTL and not
// the end of its attempt, while cv waits for its answer.
func (c *Core) draftOf(cv *conversation) *draft {
	d := cv.draft
	if d == nil || d.done || d.updatedAt.Before(c.now().Add(-draftTTL)) || c.view(cv).State != stateAwaitingAnswer {
		return nil
	}
	return d
}

// clearDraft is what posting or proposing the answer, withdrawing the
// question, or closing the conversation, does to its draft.
func (cv *conversation) clearDraft() { cv.draft = nil }

// kindDraftNews is the news of a draft written (Core's wake.KindDraft),
// which wakes only a reader that watches the draft.
const kindDraftNews = "conversation.draft"

// draftNews wakes those reading cv who watch its draft
// (seen_draft_version). The lock is held.
func (c *Core) draftNews(cv *conversation) {
	c.wake(note{course: cv.course.id, kind: kindDraftNews, conversation: cv.id, respondent: cv.respondent.id})
}

// draftView is a draft as conversation.get and conversation.messages show
// it: its text to whom it would show the answer (seesDraftText), else
// text_hidden.
type draftView struct {
	Attempt    string      `json:"attempt"`
	Version    int64       `json:"version"`
	UpdatedAt  time.Time   `json:"updated_at"`
	Steps      []DraftStep `json:"steps"`
	Text       *string     `json:"text,omitempty"`
	TextHidden bool        `json:"text_hidden,omitempty"`
}

// draftFor is cv's draft as rc's caller reads it, as JSON: null for none,
// and nil, for the view to leave the field out, from a Core without drafts
// (Options.WithoutDraft).
func (c *Core) draftFor(rc *readCtx, cv *conversation) json.RawMessage {
	if c.opts.WithoutDraft {
		return nil
	}
	d := c.draftOf(cv)
	if d == nil {
		return json.RawMessage("null")
	}
	v := draftView{Attempt: d.attempt, Version: d.version, UpdatedAt: d.updatedAt, Steps: slices.Clone(d.steps)}
	if v.Steps == nil {
		v.Steps = []DraftStep{}
	}
	if c.seesDraftText(rc, cv) {
		v.Text = d.text
	} else {
		v.TextHidden = true
	}
	return mustJSON(v)
}

// seesDraftText says whether rc's caller sees the text of the answer being
// written in cv (Core's seesDraftText): everyone who may read it while the
// respondent's answers post as they are written (conversation_answer
// autonomous); otherwise the respondent, and whoever could decide the
// answer once proposed: anyone who decides actions here who is not of the
// respondent's party, and, of it, the respondent's owner where they decide
// actions without anyone's confirmation.
func (c *Core) seesDraftText(rc *readCtx, cv *conversation) bool {
	r := cv.respondent
	if r.effectivePerms(rc.now)[permConversationAnswer] == autonomous.String() || rc.member == r {
		return true
	}
	level := rc.member.perm(permActionDecide)
	if !level.allowed() {
		return false
	}
	if !sameParty(rc.actor, r.actor) {
		return true
	}
	return level == autonomous && r.actor.owner == rc.actor
}

// draftVersionOf is what seen_draft_version names of a result's draft: its
// version, 0 for none; and its attempt.
func draftVersionOf(result json.RawMessage) (int64, string) {
	var r struct {
		Draft *struct {
			Attempt string `json:"attempt"`
			Version int64  `json:"version"`
		} `json:"draft"`
	}
	if json.Unmarshal(result, &r) != nil || r.Draft == nil {
		return 0, ""
	}
	return r.Draft.Version, r.Draft.Attempt
}

// DraftWrites are the conversation_draft calls the fake carried out in
// conv, stored or not, in order.
func (c *Core) DraftWrites(conv string) []DraftWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []DraftWrite
	for _, w := range c.draftWrites {
		if w.ConversationID == conv {
			out = append(out, w)
		}
	}
	return out
}

// Draft is conv's draft as a reader would find it now, and whether there
// is one.
func (c *Core) Draft(conv string) (Draft, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cv := c.conversations[conv]
	if cv == nil {
		return Draft{}, false
	}
	d := c.draftOf(cv)
	if d == nil {
		return Draft{}, false
	}
	return Draft{Attempt: d.attempt, Version: d.version, Text: d.text, Steps: slices.Clone(d.steps), UpdatedAt: d.updatedAt}, true
}

// withoutDraft is the catalogue raw as a Core from before drafts served
// it: no conversation.draft, no draft in conversation.get's and
// conversation.messages' results, and no seen_draft_version.
func withoutDraft(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fakecore: the catalogue: %w", err)
	}
	tools, _ := doc["tools"].([]any)
	kept := tools[:0]
	found := 0
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		switch name, _ := tool["name"].(string); name {
		case "conversation.draft":
			found++
			continue
		case "conversation.get", "conversation.messages":
			out, _ := tool["output_schema"].(map[string]any)
			props, _ := out["properties"].(map[string]any)
			if _, ok := props["draft"]; ok {
				delete(props, "draft")
				found++
			}
			in, _ := tool["input_schema"].(map[string]any)
			inProps, _ := in["properties"].(map[string]any)
			delete(inProps, "seen_draft_version")
		}
		kept = append(kept, t)
	}
	if found != 3 {
		return nil, errors.New("fakecore: the catalogue has no conversation.draft, or no draft in the conversation's views, to take out")
	}
	doc["tools"] = kept
	return json.Marshal(doc)
}
