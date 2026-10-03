package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

func TestSchedulerBoundsAnswers(t *testing.T) {
	s := newScheduler(3)
	if !s.tryStart("c1", "x", 2) || !s.tryStart("c1", "y", 2) {
		t.Fatal("two answers in a course of two")
	}
	if s.tryStart("c1", "z", 2) {
		t.Error("a third in a course of two")
	}
	if s.tryStart("c2", "x", 2) {
		t.Error("one conversation twice at once")
	}
	if !s.tryStart("c2", "w", 2) {
		t.Error("the third answer of the agent")
	}
	if s.tryStart("c3", "v", 2) || s.busy() != 3 {
		t.Errorf("a fourth answer of an agent of three; busy %d", s.busy())
	}
	if s.reserve("w") {
		t.Error("a conversation being answered reserved")
	}
	s.done("c1", "x")
	if !s.reserve("x") || s.tryStart("c3", "x", 2) {
		t.Error("a reserved conversation answered")
	}
	s.release("x")
	if !s.tryStart("c3", "x", 2) {
		t.Error("a slot given back is not taken again")
	}
}

func TestNextAttempt(t *testing.T) {
	at := func(no int, st store.AttemptState) store.Attempt { return store.Attempt{No: no, State: st} }
	for _, c := range []struct {
		name string
		atts []store.Attempt
		n    int
		busy bool
	}{
		{"none", nil, 1, false},
		{"one failed", []store.Attempt{at(1, store.AttemptFailed)}, 2, false},
		{"rejected and cancelled", []store.Attempt{at(1, store.AttemptRejected), at(2, store.AttemptCancelled)}, 3, false},
		{"sent back for changes", []store.Attempt{at(1, store.AttemptChangesRequested)}, 2, false},
		{"a key taken by another body", []store.Attempt{at(1, store.AttemptError)}, 2, false},
		{"posted", []store.Attempt{at(1, store.AttemptFailed), at(2, store.AttemptExecuted)}, 0, true},
		{"waiting for a person", []store.Attempt{at(1, store.AttemptProposed)}, 0, true},
	} {
		n, busy := nextAttempt(c.atts)
		if n != c.n || busy != c.busy {
			t.Errorf("%s: %d %v, want %d %v", c.name, n, busy, c.n, c.busy)
		}
	}
}

// TestRevised: the next attempt at a message writes again the newest
// attempt a person sent back for changes, and names it in revises; a
// revision sent back in turn is the one revised next. One written since
// and decided by nobody (failed, expired, refused) leaves the request
// standing, made of an earlier answer. A rejection since overrules it:
// the next attempt revises nothing and names nothing. One Core refused for
// what it named (not_revisable, or revises itself, as a Core from before
// AIShie-Core #68 refuses it) has the attempts after it name nothing,
// though they still write the answer sent back again, until another is
// sent back; so does one sent back whose action is not known.
func TestRevised(t *testing.T) {
	sent := func(no int, action string) store.Attempt {
		return store.Attempt{No: no, State: store.AttemptChangesRequested, ActionID: action, Reason: fmt.Sprintf("change %d", no)}
	}
	other := func(no int, st store.AttemptState, reason string) store.Attempt {
		return store.Attempt{No: no, State: st, ActionID: fmt.Sprintf("act-%d", no), Reason: reason}
	}
	for _, c := range []struct {
		name  string
		atts  []store.Attempt
		of    int // the attempt written again, by its number; 0 for none
		since bool
		names string
	}{
		{"none", nil, 0, false, ""},
		{"rejected, not sent back", []store.Attempt{other(1, store.AttemptRejected, "Too terse.")}, 0, false, ""},
		{"sent back", []store.Attempt{sent(1, "act-1")}, 1, false, "act-1"},
		{"sent back after a rejection", []store.Attempt{other(1, store.AttemptRejected, "Too terse."), sent(2, "act-2")}, 2, false, "act-2"},
		{"a chain of two", []store.Attempt{sent(1, "act-1"), sent(2, "act-2")}, 2, false, "act-2"},
		{"a revision that failed, then", []store.Attempt{sent(1, "act-1"), other(2, store.AttemptFailed, "")}, 1, true, "act-1"},
		{"a revision expired, then", []store.Attempt{sent(1, "act-1"), other(2, store.AttemptCancelled, "proposal_expired")}, 1, true, "act-1"},
		{"a revision rejected", []store.Attempt{sent(1, "act-1"), other(2, store.AttemptRejected, "No.")}, 0, false, ""},
		{"rejected after one that failed", []store.Attempt{sent(1, "act-1"), other(2, store.AttemptFailed, ""),
			other(3, store.AttemptRejected, "No.")}, 0, false, ""},
		{"refused for what it named", []store.Attempt{sent(1, "act-1"), other(2, store.AttemptError, core.ReasonNotRevisable)}, 1, true, ""},
		{"refused for naming any", []store.Attempt{sent(1, "act-1"), other(2, store.AttemptError, reasonRevisesNotTaken)}, 1, true, ""},
		{"refused, then expired", []store.Attempt{sent(1, "act-1"), other(2, store.AttemptError, core.ReasonNotRevisable),
			other(3, store.AttemptCancelled, "proposal_expired")}, 1, true, ""},
		{"refused, then sent back again", []store.Attempt{sent(1, "act-1"), other(2, store.AttemptError, core.ReasonNotRevisable),
			sent(3, "act-3")}, 3, false, "act-3"},
		{"sent back with no action known", []store.Attempt{sent(1, "")}, 1, false, ""},
		{"sent back since, no action known", []store.Attempt{sent(1, "act-1"), sent(2, "")}, 2, false, ""},
	} {
		at, since, named := revised(c.atts)
		of, names := 0, ""
		if at != nil {
			of = at.No
			if at.Reason != fmt.Sprintf("change %d", at.No) {
				t.Errorf("%s: the attempt written again is %+v", c.name, at)
			}
			if named {
				names = at.ActionID
			}
		} else if since || named {
			t.Errorf("%s: none written again, since %v, named %v", c.name, since, named)
		}
		if of != c.of || since != c.since || names != c.names {
			t.Errorf("%s: writes attempt %d again, since %v, naming %q; want %d, %v, %q", c.name, of, since, names, c.of, c.since, c.names)
		}
	}
}

func TestClassifyClose(t *testing.T) {
	for _, c := range []struct {
		name    string
		env     *core.Envelope
		err     error
		next    Next
		state   store.AttemptState
		outcome string
	}{
		{"closed", &core.Envelope{Status: core.StatusExecuted}, nil, NextDone, store.AttemptExecuted, store.OutcomeClosed},
		{"closed already", &core.Envelope{Status: core.StatusFailed, Error: &core.Error{Code: core.CodeConflict}}, nil, NextDrop, store.AttemptFailed, store.OutcomeDropped},
		{"proposed", &core.Envelope{Status: core.StatusProposed, ActionID: "a"}, nil, NextProposed, store.AttemptProposed, store.OutcomeProposed},
		{"denied", &core.Envelope{Status: core.StatusDenied}, nil, NextHoldSeat, store.AttemptDenied, store.OutcomeDenied},
		{"not a participant", &core.Envelope{Status: core.StatusFailed, Error: &core.Error{Code: core.CodeForbidden}}, nil, NextDropReseat, store.AttemptFailed, store.OutcomeDropped},
		{"not found", &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeNotFound}}, nil, NextDrop, store.AttemptError, store.OutcomeDropped},
		{"internal", &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeInternal}}, nil, NextRetryLater, "", store.OutcomeError},
		{"unauthenticated", &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeUnauthenticated}}, nil, NextStopAgent, "", store.OutcomeError},
		{"401", nil, core.ErrUnauthenticated, NextStopAgent, "", store.OutcomeError},
		{"unreachable", nil, &core.TransientError{Err: errors.New("reset")}, NextRetryLater, "", store.OutcomeError},
	} {
		d := classifyClose(c.env, c.err)
		if d.Next != c.next || d.State != c.state || d.Outcome != c.outcome {
			t.Errorf("%s: %s %q %q, want %s %q %q", c.name, d.Next, d.State, d.Outcome, c.next, c.state, c.outcome)
		}
	}
}

func TestAuthCheckedReadsTheEnvelopeAsA401(t *testing.T) {
	next := callerFunc(func(_ context.Context, tool string, _ json.RawMessage) (*core.Envelope, error) {
		if tool == "gone" {
			return &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeUnauthenticated}}, nil
		}
		return &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeNotFound}}, nil
	})
	c := authChecked{next: next}
	if _, err := c.Call(context.Background(), "gone", nil); !errors.Is(err, core.ErrUnauthenticated) {
		t.Errorf("the unauthenticated envelope: %v", err)
	}
	if env, err := c.Call(context.Background(), "other", nil); err != nil || env.Code() != core.CodeNotFound {
		t.Errorf("another envelope: %v %v", env, err)
	}
	if !isUnauthenticated(&core.EnvelopeError{Tool: "me_get", Envelope: &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeUnauthenticated}}}) {
		t.Error("an unauthenticated read is not a 401")
	}
}

func TestDays(t *testing.T) {
	at := time.Date(2026, 9, 27, 23, 59, 0, 0, time.FixedZone("JST", 9*3600))
	if got := startOfDay(at); !got.Equal(time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("startOfDay = %s", got)
	}
	if got := nextDay(at); !got.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("nextDay = %s", got)
	}
}

// TestHalve takes the older half of the history out as whole turns, keeps
// the question, and begins with the mark of what is not shown.
func TestHalve(t *testing.T) {
	user, bot := llm.UserText, func(s string) llm.Message {
		return llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{llm.Text(s)}}
	}
	l := &loop{c: &claim{s: &Seat{log: discardLog()}}, history: []llm.Message{
		user(prompt.Omitted + "\n\nq1"), bot("a1"), user("q2"), bot("a2"), user("q3"), bot("a3"), user("q4"),
	}}
	if !l.halve() {
		t.Fatal("nothing taken out")
	}
	// Six turns before the question: the older three go.
	got := l.history
	if len(got) != 5 || got[0].Parts[0].Text != prompt.Omitted || got[1].Parts[0].Text != "a2" || got[2].Parts[0].Text != "q3" ||
		got[4].Parts[0].Text != "q4" || got[4].Role != llm.RoleUser {
		t.Errorf("halved: %+v", got)
	}
	l.history = []llm.Message{user("q1")}
	if l.halve() {
		t.Error("the question alone was halved")
	}
	l.history = []llm.Message{user(prompt.Omitted), bot("a1"), user("q2")}
	if !l.halve() || len(l.history) != 1 || l.history[0].Parts[len(l.history[0].Parts)-1].Text != "q2" {
		t.Errorf("halved: %+v", l.history)
	}
}

// TestSnapshotCatalogueHash holds the hash the worker compares Core's
// catalogue with to the snapshot the runtime is tested against.
func TestSnapshotCatalogueHash(t *testing.T) {
	b, err := os.ReadFile("../core/testdata/catalogue.sha256")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != SnapshotCatalogueHash {
		t.Errorf("the snapshot's hash is %s; SnapshotCatalogueHash is %s", got, SnapshotCatalogueHash)
	}
	raw, err := os.ReadFile("../core/testdata/catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := core.ParseCatalogue(raw)
	if err != nil || cat.Hash() != SnapshotCatalogueHash {
		t.Errorf("the snapshot hashes %v, %v", cat, err)
	}
}

// TestRowsWaitingForASlotAreWork: a poll whose rows all wait for a slot is
// not an empty poll, and does not put the next poll off; one whose rows
// are all held back, or all being answered already, is.
func TestRowsWaitingForASlotAreWork(t *testing.T) {
	sup, err := NewSupervisor(Options{Config: &config.Config{}, Store: memstore.New(), WorkerID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	rows := `{"conversations":[{"id":"x1","latest_opener_message_id":"m1"},{"id":"x2","latest_opener_message_id":"m2"}]}`
	a := newAgent(sup, &config.Agent{ID: "a"})
	a.client = core.NewClient(callerFunc(func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
		return &core.Envelope{Status: core.StatusExecuted, Result: json.RawMessage(rows)}, nil
	}))
	a.sched = newScheduler(1)
	a.answerCtx = context.Background()
	s := &Seat{a: a, id: "m", course: "c", log: discardLog(), heldBack: map[string]heldBack{}, failures: map[string]int{},
		eff: &config.Effective{Agent: config.Agent{Answer: config.Answer{MaxConcurrentPerCourse: 4}}}}
	empties := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.emptyPolls
	}

	// Another course's answer takes the agent's one slot: both rows wait.
	if !a.sched.tryStart("elsewhere", "x0", 4) {
		t.Fatal("no slot")
	}
	s.pollInboxOnce(context.Background())
	s.pollInboxOnce(context.Background())
	if n := empties(); n != 0 {
		t.Errorf("rows waiting for a slot counted as %d empty polls", n)
	}
	a.sched.done("elsewhere", "x0")

	// Both being answered here already: nothing new, an empty poll.
	a.sched = newScheduler(4)
	a.sched.tryStart("c", "x1", 4)
	a.sched.tryStart("c", "x2", 4)
	s.pollInboxOnce(context.Background())
	if n := empties(); n != 1 {
		t.Errorf("rows being answered already: %d empty polls, want 1", n)
	}

	// Both held back: an empty poll too.
	a.sched = newScheduler(4)
	later := time.Now().Add(time.Hour)
	s.holdBack("x1", later, "test")
	s.holdBack("x2", later, "test")
	s.pollInboxOnce(context.Background())
	if n := empties(); n != 2 {
		t.Errorf("rows held back: %d empty polls, want 2", n)
	}
}

// TestEventsAskedForDuringARead: events asked for at once while a read of
// them is under way, as when an answer begins to be written (eventsAtOnce),
// are read again at once once it is over: that read began before they were
// asked for, and cannot have the news they are read for. Taken as read by
// it, they would wait for events_s, while the answer, unwatched, went on
// with its question withdrawn.
func TestEventsAskedForDuringARead(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	sup, err := NewSupervisor(Options{Config: &config.Config{}, Store: memstore.New(), WorkerID: "w", Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	a := newAgent(sup, &config.Agent{ID: "a"})
	s := &Seat{a: a, id: "m", course: "c", log: discardLog(), wakeEvents: make(chan struct{}, 1),
		eff: &config.Effective{Agent: config.Agent{Polling: config.Polling{EventsS: 30}}}}
	a.client = core.NewClient(callerFunc(func(_ context.Context, tool string, _ json.RawMessage) (*core.Envelope, error) {
		if tool == "event_list" {
			advance(time.Second)
			s.eventsAtOnce()
			advance(time.Second)
		}
		return &core.Envelope{Status: core.StatusExecuted, Result: json.RawMessage(`{"events":[],"next_seq":0}`)}, nil
	}))
	s.pollEventsOnce(context.Background())
	if next := s.nextEvents(0.5, false); next.After(clock()) {
		t.Errorf("events asked for during a read are read %s after it", next.Sub(clock()))
	}
	// Read again, they are not asked for any more: events_s from then.
	a.client = core.NewClient(callerFunc(func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
		return &core.Envelope{Status: core.StatusExecuted, Result: json.RawMessage(`{"events":[],"next_seq":0}`)}, nil
	}))
	s.pollEventsOnce(context.Background())
	if next := s.nextEvents(0.5, false); next.Sub(clock()) != 30*time.Second {
		t.Errorf("events read again %s after a read that had them", next.Sub(clock()))
	}
}

// TestRetractionReadTwice: the retraction of an answer of the agent's,
// read a second time (the events cursor was not saved), keeps the note
// the first reading left.
func TestRetractionReadTwice(t *testing.T) {
	ctx := context.Background()
	st := memstore.New()
	sup, err := NewSupervisor(Options{Config: &config.Config{}, Store: st, WorkerID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Seat{a: newAgent(sup, &config.Agent{ID: "a"}), id: "m", course: "c", log: discardLog(),
		eff: &config.Effective{Agent: config.Agent{Memory: config.Memory{Enabled: true}}}}
	if err := st.AddNote(ctx, store.Note{AgentID: "a", MemberID: "m", ConversationID: "x", Kind: store.NoteAnswered,
		Text: answeredNote("p"), MessageID: "p"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.retracted(ctx, "x", "p"); err != nil {
			t.Fatal(err)
		}
	}
	notes, err := st.Notes(ctx, "a", "m", "x", 10)
	if err != nil || len(notes) != 1 || notes[0].Kind != store.NoteRetractedOwn || notes[0].MessageID != "p" {
		t.Errorf("memory after the retraction read twice: %+v, %v", notes, err)
	}
}

// TestBasePromptFollowsTheSeat: the built-in prompt is the one for the
// seat's kind as me_memberships last showed it, and a system_ref file's
// text replaces it whatever the kind.
func TestBasePromptFollowsTheSeat(t *testing.T) {
	s := &Seat{}
	if got := s.basePrompt(core.Membership{AnswersCourse: true}); got != prompt.Builtin(true) {
		t.Error("a course tutor's seat is not given the tutor's prompt")
	}
	if got := s.basePrompt(core.Membership{AnswersCourse: false}); got != prompt.Builtin(false) {
		t.Error("a seat that answers only its principal is given the tutor's prompt")
	}
	custom := "You are {{agent}}."
	s.custom = &custom
	if got := s.basePrompt(core.Membership{AnswersCourse: true}); got != custom {
		t.Errorf("system_ref's prompt replaced by %q", got)
	}
}
