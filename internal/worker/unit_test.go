package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store/memstore"
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
