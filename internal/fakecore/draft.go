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
// the answer, closing the conversation, and done clear it; one not written
// for 120 seconds is none.
//
// Options.Drafts serves the tool as the contract the runtime was built to
// describes it (draftToolJSON): the pinned Core's catalogue has no such
// tool yet.

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
	errNotAwaiting = conflicts("the conversation waits for no answer now: it is answered, an answer waits for approval, "+
		"or it is closed; a draft is written only while it waits").with("reason", "conversation_not_awaiting")
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

// clearDraft is what posting or proposing the answer, or closing the
// conversation, does to its draft.
func (cv *conversation) clearDraft() { cv.draft = nil }

// draftNews wakes those reading cv who watch its draft. Nothing waits on
// a draft in the fake yet: the reads that show it come with Core's
// catalogue.
func (c *Core) draftNews(*conversation) {}

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

// draftToolJSON is conversation.draft as the contract between Core, the
// runtime and the site describes it, in the shape of Core's catalogue: what
// Options.Drafts serves until the runtime is pinned to a Core that has it.
const draftToolJSON = `{
 "description": "For an agent runtime, never for a model: say what you are doing towards an answer in a conversation addressed to you, and the answer's text so far, for whoever reads the conversation to watch it come.",
 "input_schema": {
  "additionalProperties": false,
  "properties": {
   "attempt": {"type": "string"},
   "conversation_id": {"format": "uuid", "type": "string"},
   "course_id": {"description": "the course this call is about", "format": "uuid", "type": "string"},
   "done": {"type": "boolean"},
   "steps": {
    "items": {
     "additionalProperties": false,
     "properties": {"kind": {"type": "string"}, "state": {"type": "string"}, "target": {"type": "string"}},
     "required": ["kind", "state"],
     "type": "object"
    },
    "type": ["null", "array"]
   },
   "text": {"type": ["null", "string"]},
   "version": {"type": "integer"}
  },
  "required": ["course_id", "conversation_id", "attempt", "version"],
  "type": "object"
 },
 "kind": "ephemeral",
 "method": "POST",
 "name": "conversation.draft",
 "output_schema": {
  "additionalProperties": false,
  "properties": {"stored": {"type": "boolean"}, "version": {"type": "integer"}},
  "required": ["stored", "version"],
  "type": "object"
 },
 "path": "/v1/courses/{course_id}/conversations/{conversation_id}/draft"
}`

// withDraft is the catalogue raw with conversation.draft (draftToolJSON).
func withDraft(raw []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fakecore: the catalogue: %w", err)
	}
	tools, _ := doc["tools"].([]any)
	for _, t := range tools {
		if tool, _ := t.(map[string]any); tool != nil && tool["name"] == "conversation.draft" {
			return nil, errors.New("fakecore: the catalogue has conversation.draft already")
		}
	}
	var tool map[string]any
	if err := json.Unmarshal([]byte(draftToolJSON), &tool); err != nil {
		return nil, err
	}
	doc["tools"] = append(tools, tool)
	return json.Marshal(doc)
}
