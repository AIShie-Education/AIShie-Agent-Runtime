package storetest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

func testCursors(t *testing.T, open Opener) {
	t.Run("empty, set, overwritten", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		if got, err := s.Cursor(ctx, "a1", "m1", store.CursorEvents); err != nil || got != "" {
			t.Fatalf("Cursor before any = %q, %v; want empty", got, err)
		}
		for _, v := range []string{"17", "42", ""} {
			if err := s.SetCursor(ctx, "a1", "m1", store.CursorEvents, v); err != nil {
				t.Fatal(err)
			}
			if got, err := s.Cursor(ctx, "a1", "m1", store.CursorEvents); err != nil || got != v {
				t.Fatalf("Cursor after SetCursor(%q) = %q, %v", v, got, err)
			}
		}
	})

	t.Run("one per agent, seat and kind", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		cursors := []struct{ agent, member, kind, value string }{
			{"a1", "m1", store.CursorEvents, "10"},
			{"a1", "m1", store.CursorActions, "act-7"},
			{"a1", "m2", store.CursorEvents, "20"},
			{"a2", "m1", store.CursorEvents, "30"},
		}
		for _, c := range cursors {
			if err := s.SetCursor(ctx, c.agent, c.member, c.kind, c.value); err != nil {
				t.Fatal(err)
			}
		}
		for _, c := range append(cursors, struct{ agent, member, kind, value string }{"a2", "m2", store.CursorEvents, ""}) {
			got, err := s.Cursor(ctx, c.agent, c.member, c.kind)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.value {
				t.Errorf("Cursor(%s, %s, %s) = %q, want %q", c.agent, c.member, c.kind, got, c.value)
			}
		}
	})

	t.Run("refuses a cursor without an agent, seat or kind", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for _, c := range [][3]string{{"", "m1", "events"}, {"a1", "", "events"}, {"a1", "m1", ""}} {
			if err := s.SetCursor(ctx, c[0], c[1], c[2], "1"); err == nil {
				t.Errorf("SetCursor(%q, %q, %q) was taken", c[0], c[1], c[2])
			}
		}
	})
}

// note is a note in (agent, member, conv) about msg, written at created.
func note(agent, member, conv, msg, text string, created time.Time) store.Note {
	return store.Note{
		AgentID:        agent,
		MemberID:       member,
		ConversationID: conv,
		Kind:           store.NoteAnswered,
		Text:           text,
		MessageID:      msg,
		CreatedAt:      created,
	}
}

// addNotes writes ns and fails the test if that does not work.
func addNotes(t *testing.T, s store.Store, ns ...store.Note) {
	t.Helper()
	for _, n := range ns {
		if err := s.AddNote(t.Context(), n); err != nil {
			t.Fatalf("AddNote(%+v): %v", n, err)
		}
	}
}

// texts reads a conversation's notes, at most limit, as their texts.
func texts(t *testing.T, s store.Store, agent, member, conv string, limit int) []string {
	t.Helper()
	ns, err := s.Notes(t.Context(), agent, member, conv, limit)
	if err != nil {
		t.Fatalf("Notes(%s, %s, %s): %v", agent, member, conv, err)
	}
	out := make([]string, len(ns))
	for i, n := range ns {
		if n.AgentID != agent || n.MemberID != member || n.ConversationID != conv {
			t.Errorf("Notes(%s, %s, %s) returned a note of (%s, %s, %s)", agent, member, conv, n.AgentID, n.MemberID, n.ConversationID)
		}
		out[i] = n.Text
	}
	return out
}

func testMemory(t *testing.T, open Opener) {
	t.Run("Notes are the newest limit, oldest first", func(t *testing.T) {
		s := open(t)
		// Written out of order, so that the order is their age.
		for _, i := range []int{3, 0, 4, 1, 2} {
			addNotes(t, s, note("a1", "m1", "x1", "", string(rune('A'+i)), at(time.Duration(i)*time.Minute)))
		}
		for _, c := range []struct {
			limit int
			want  []string
		}{
			{3, []string{"C", "D", "E"}},
			{5, []string{"A", "B", "C", "D", "E"}},
			{50, []string{"A", "B", "C", "D", "E"}},
			{1, []string{"E"}},
			{0, nil},
			{-1, nil},
		} {
			if got := texts(t, s, "a1", "m1", "x1", c.limit); !slices.Equal(got, c.want) {
				t.Errorf("Notes(limit %d) = %q, want %q", c.limit, got, c.want)
			}
		}
	})

	t.Run("a note keeps what it was given, to the microsecond", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		n := store.Note{AgentID: "a1", MemberID: "m1", ConversationID: "x1", Kind: store.NoteRejected,
			Text: "Cite the syllabus, not the textbook.", MessageID: "q1", CreatedAt: base}
		addNotes(t, s, n)
		got, err := s.Notes(ctx, "a1", "m1", "x1", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("Notes = %+v, want one", got)
		}
		sameTime(t, "created_at", got[0].CreatedAt, base)
		got[0].CreatedAt, n.CreatedAt = time.Time{}, time.Time{}
		if got[0] != n {
			t.Errorf("Notes = %+v, want %+v", got[0], n)
		}
	})

	t.Run("notes written at one instant keep the order they were written in", func(t *testing.T) {
		s := open(t)
		for _, text := range []string{"first", "second", "third"} {
			addNotes(t, s, note("a1", "m1", "x1", "", text, base))
		}
		if got, want := texts(t, s, "a1", "m1", "x1", 10), []string{"first", "second", "third"}; !slices.Equal(got, want) {
			t.Errorf("Notes = %q, want %q", got, want)
		}
		if got, want := texts(t, s, "a1", "m1", "x1", 2), []string{"second", "third"}; !slices.Equal(got, want) {
			t.Errorf("Notes(limit 2) = %q, want %q", got, want)
		}
	})

	t.Run("a zero CreatedAt is the store's now", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		addNotes(t, s, note("a1", "m1", "x1", "", "old", base))
		before := time.Now()
		addNotes(t, s, note("a1", "m1", "x1", "", "now", time.Time{}))
		after := time.Now()
		got, err := s.Notes(ctx, "a1", "m1", "x1", 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Text != "now" {
			t.Fatalf("newest note = %+v, want the one written now", got)
		}
		recent(t, "created_at", got[0].CreatedAt, before, after)
	})

	t.Run("ForgetMessage removes only that message's notes", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		addNotes(t, s,
			note("a1", "m1", "x1", "q1", "about q1", at(0)),
			note("a1", "m1", "x1", "q2", "about q2", at(time.Minute)),
			note("a1", "m1", "x1", "", "about nothing", at(2*time.Minute)),
			note("a1", "m1", "x1", "q1", "about q1 again", at(3*time.Minute)),
			note("a1", "m1", "x2", "q1", "x2 about q1", at(0)),
			note("a1", "m2", "x1", "q1", "m2 about q1", at(0)),
			note("a2", "m1", "x1", "q1", "a2 about q1", at(0)),
		)
		if err := s.ForgetMessage(ctx, "a1", "m1", "x1", "q1"); err != nil {
			t.Fatal(err)
		}
		// No message id names no message: nothing more goes.
		if err := s.ForgetMessage(ctx, "a1", "m1", "x1", ""); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			agent, member, conv string
			want                []string
		}{
			{"a1", "m1", "x1", []string{"about q2", "about nothing"}},
			{"a1", "m1", "x2", []string{"x2 about q1"}},
			{"a1", "m2", "x1", []string{"m2 about q1"}},
			{"a2", "m1", "x1", []string{"a2 about q1"}},
		} {
			if got := texts(t, s, c.agent, c.member, c.conv, 10); !slices.Equal(got, c.want) {
				t.Errorf("Notes(%s, %s, %s) = %q, want %q", c.agent, c.member, c.conv, got, c.want)
			}
		}
	})

	t.Run("PurgeMember removes everything in that seat and nothing else", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		addNotes(t, s,
			note("a1", "m1", "x1", "q1", "m1 in x1", base),
			note("a1", "m1", "x2", "q2", "m1 in x2", base),
			note("a1", "m2", "x3", "q3", "m2 in x3", base),
			note("a2", "m1", "x1", "q1", "a2's m1 in x1", base),
		)
		put(t, s, answer("a1", "m1", "x1", "q1", 1, base))
		put(t, s, answer("a1", "m2", "x3", "q3", 1, base))
		put(t, s, answer("a2", "m1", "x1", "q1", 1, base))
		for _, c := range [][2]string{{"a1", "m1"}, {"a1", "m2"}, {"a2", "m1"}} {
			if err := s.SetCursor(ctx, c[0], c[1], store.CursorEvents, "5"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SeatSeen(ctx, store.SeatRef{AgentID: "a1", MemberID: "m1", CourseID: "course-1", SeenAt: base}); err != nil {
			t.Fatal(err)
		}

		if err := s.PurgeMember(ctx, "a1", "m1"); err != nil {
			t.Fatal(err)
		}

		for _, c := range []struct {
			agent, member, conv string
			want                []string
		}{
			{"a1", "m1", "x1", nil},
			{"a1", "m1", "x2", nil},
			{"a1", "m2", "x3", []string{"m2 in x3"}},
			{"a2", "m1", "x1", []string{"a2's m1 in x1"}},
		} {
			if got := texts(t, s, c.agent, c.member, c.conv, 10); !slices.Equal(got, c.want) {
				t.Errorf("Notes(%s, %s, %s) = %q, want %q", c.agent, c.member, c.conv, got, c.want)
			}
		}
		// The seat's answers held their bodies: they go too.
		if _, err := s.Attempt(ctx, "a1", "answer:x1:q1:1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the purged seat's attempt: err = %v, want ErrNotFound", err)
		}
		get(t, s, "a1", "answer:x3:q3:1")
		get(t, s, "a2", "answer:x1:q1:1")
		for _, c := range []struct{ agent, member, want string }{
			{"a1", "m1", ""}, {"a1", "m2", "5"}, {"a2", "m1", "5"},
		} {
			if got, err := s.Cursor(ctx, c.agent, c.member, store.CursorEvents); err != nil || got != c.want {
				t.Errorf("Cursor(%s, %s) = %q, %v; want %q", c.agent, c.member, got, err, c.want)
			}
		}
		// The seat's row is ForgetSeat's to remove.
		seats, err := s.KnownSeats(ctx, "a1")
		if err != nil {
			t.Fatal(err)
		}
		if len(seats) != 1 {
			t.Errorf("KnownSeats after PurgeMember = %+v, want the seat still known", seats)
		}
	})

	t.Run("PurgeAgent removes all an agent has but its ledger, and nothing of another's", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		// agt_1's neighbours: one whose id begins as its does, and one
		// whose id a LIKE pattern would take for it.
		agents := []string{"agt_1", "agt_10", "agtX1"}
		for _, a := range agents {
			addNotes(t, s, note(a, "m1", "x1", "q1", a+"'s note", base))
			put(t, s, answer(a, "m1", "x1", "q1", 1, base))
			if err := s.SetCursor(ctx, a, "m1", store.CursorEvents, "5"); err != nil {
				t.Fatal(err)
			}
			if err := s.SeatSeen(ctx, store.SeatRef{AgentID: a, MemberID: "m1", CourseID: "course-1", SeenAt: base}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetAgentState(ctx, store.AgentState{AgentID: a, State: store.AgentRunning, Worker: "w1", UpdatedAt: base}); err != nil {
				t.Fatal(err)
			}
			for _, lease := range []string{"agent:" + a, "conv:" + a + ":x1"} {
				if ok, err := s.AcquireLease(ctx, lease, "w1", time.Hour); err != nil || !ok {
					t.Fatalf("lease %s: %v %v", lease, ok, err)
				}
			}
			record(t, s, []store.LLMCall{call("c-"+a, base, scope{agent: a}, 10)}, []store.AnswerRecord{answerRecord("r-"+a, base, scope{agent: a}, true, 10)})
		}
		if err := s.PurgeAgent(ctx, "agt_1"); err != nil {
			t.Fatal(err)
		}
		if err := s.PurgeAgent(ctx, "agt_nothing"); err != nil {
			t.Errorf("an agent it holds nothing of: %v", err)
		}
		for _, a := range agents {
			purged := a == "agt_1"
			if got := texts(t, s, a, "m1", "x1", 10); (len(got) == 0) != purged {
				t.Errorf("%s's notes: %q", a, got)
			}
			if _, err := s.Attempt(ctx, a, "answer:x1:q1:1"); errors.Is(err, store.ErrNotFound) != purged {
				t.Errorf("%s's attempt: %v", a, err)
			}
			if c, _ := s.Cursor(ctx, a, "m1", store.CursorEvents); (c == "") != purged {
				t.Errorf("%s's cursor: %q", a, c)
			}
			if seats := knownSeats(t, s, a); (len(seats) == 0) != purged {
				t.Errorf("%s's seats: %+v", a, seats)
			}
			if _, err := s.AgentState(ctx, a); errors.Is(err, store.ErrNotFound) != purged {
				t.Errorf("%s's state: %v", a, err)
			}
			for _, lease := range []string{"agent:" + a, "conv:" + a + ":x1"} {
				// Another holder takes a lease only once it has gone.
				if ok, err := s.AcquireLease(ctx, lease, "w2", time.Hour); err != nil || ok != purged {
					t.Errorf("%s's lease %s taken by another: %v %v", a, lease, ok, err)
				}
			}
			// The ledger's ids and numbers stay.
			if sp := spend(t, s, store.SpendScope{AgentID: a}, base.Add(-time.Hour)); sp.Answers != 1 || sp.CostPUSD != 10 {
				t.Errorf("%s's ledger: %+v", a, sp)
			}
		}
		if err := s.PurgeAgent(ctx, ""); err == nil {
			t.Error("PurgeAgent of no agent was taken")
		}
	})

	t.Run("one conversation's, seat's or agent's notes are never another's", func(t *testing.T) {
		s := open(t)
		addNotes(t, s, note("a1", "m1", "x1", "q1", "secret", base))
		for _, c := range [][3]string{
			{"a1", "m1", "x2"}, // another conversation of the seat
			{"a1", "m2", "x1"}, // another seat, the same conversation id
			{"a2", "m1", "x1"}, // another agent, the same ids
		} {
			if got := texts(t, s, c[0], c[1], c[2], 10); len(got) != 0 {
				t.Errorf("Notes(%s, %s, %s) = %q, want none", c[0], c[1], c[2], got)
			}
		}
		if got := texts(t, s, "a1", "m1", "x1", 10); !slices.Equal(got, []string{"secret"}) {
			t.Errorf("Notes(a1, m1, x1) = %q, want its own", got)
		}
	})

	t.Run("refuses a note without an agent, seat or conversation", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		for _, n := range []store.Note{
			note("", "m1", "x1", "", "t", base),
			note("a1", "", "x1", "", "t", base),
			note("a1", "m1", "", "", "t", base),
		} {
			if err := s.AddNote(ctx, n); err == nil {
				t.Errorf("AddNote(%+v) was taken", n)
			}
		}
	})
}
