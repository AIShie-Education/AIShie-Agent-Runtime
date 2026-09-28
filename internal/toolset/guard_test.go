package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolschema"
)

// The seats of a guard's tests: the agent's own, its principal's, someone
// who opened a conversation with an agent nobody owns, a student, another
// agent of the principal's, and an agent of the student's.
const (
	selfSeat      = "0192f3c1-5e1f-7000-8000-000000000001"
	principalSeat = "0192f3c1-5e1f-7000-8000-000000000002"
	openerSeat    = "0192f3c1-5e1f-7000-8000-000000000003"
	studentSeat   = "0192f3c1-5e1f-7000-8000-000000000004"
	someActor     = "0192f3c1-5e1f-7000-8000-000000000005"
	siblingSeat   = "0192f3c1-5e1f-7000-8000-000000000006"
	studentsAgent = "0192f3c1-5e1f-7000-8000-000000000007"
)

// registrarSet is the set of a seat that manages members, in a
// conversation with writes.
func registrarSet(t testing.TB) *Set {
	t.Helper()
	s, err := snapshot(t).Build(registrarPerms, config.Tools{Writes: true}, ReadWrite, toolschema.OpenAIStrict, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Writes(); !slices.Equal(got, managingWrites) {
		t.Fatalf("the registrar's set offers the writes %v", got)
	}
	return s
}

// roles is a Core whose member_get shows each seat's role, as
// "role" or "role/principal" for a delegate, and member_list the seats of
// a role, in id order; it executes every other call.
func roles(byID map[string]string) *fakeCore {
	view := func(id, seat string) string {
		role, principal, _ := strings.Cut(seat, "/")
		out := `{"id":"` + id + `","role":"` + role + `","perms":{}`
		if principal != "" {
			out += `,"principal_member_id":"` + principal + `"`
		}
		return out + "}"
	}
	return &fakeCore{respond: func(_ context.Context, tool string, args json.RawMessage) (*core.Envelope, error) {
		var a struct {
			MemberID string `json:"member_id"`
			Role     string `json:"role"`
		}
		_ = json.Unmarshal(args, &a)
		a.MemberID = strings.ToLower(a.MemberID) // Core reads a UUID in any case
		switch tool {
		case "member_get":
			seat, ok := byID[a.MemberID]
			if !ok {
				return &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeNotFound, Message: "no such member in this course"}}, nil
			}
			if seat == "denied" {
				return &core.Envelope{Status: core.StatusDenied, Error: &core.Error{Code: core.CodeForbidden, Message: "not permitted"}}, nil
			}
			return executed(view(a.MemberID, seat)), nil
		case "member_list":
			var members []string
			for _, id := range sortedKeys(byID) {
				if role, _, _ := strings.Cut(byID[id], "/"); role == a.Role {
					members = append(members, view(id, byID[id]))
				}
			}
			return executed(`{"members":[` + strings.Join(members, ",") + `]}`), nil
		}
		return &core.Envelope{Status: core.StatusExecuted, ActionID: "a-" + tool, Result: json.RawMessage(`{"ok":true}`)}, nil
	}}
}

func toolsCalled(f *fakeCore) []string {
	var out []string
	for _, c := range f.recorded() {
		out = append(out, c.tool)
	}
	return out
}

// TestRunGuardsSeats: a member write never changes the agent's own seat,
// nor its principal's, nor the opener's, however the id is written; each is
// an is_error result that reaches nobody, takes no number and is counted
// as guarded. A write that names anyone else's seat, read first to be no
// agent of the principal's, or none, is sent as any other, and so is every
// other write in the same turn.
func TestRunGuardsSeats(t *testing.T) {
	f := roles(map[string]string{studentSeat: "student"})
	w := keys(1, 10)
	r := runner(f)
	r.Writes, r.Guard = w, SeatGuard{Self: selfSeat, Principal: principalSeat, Opener: principalSeat}
	parts, err := registrarSet(t).Run(context.Background(), r, courseID, []llm.Part{
		call("c1", "member_pause", `{"member_id":"`+selfSeat+`"}`),
		call("c2", "member_update_perms", `{"member_id":"`+strings.ToUpper(principalSeat)+`","perms":{"grade_post":"denied"}}`),
		call("c3", "member_remove", `{"member_id":"`+studentSeat+`"}`),
		call("c4", "member_rescope", `{"member_id":"`+principalSeat+`","student_scope":"listed","listed_students":[]}`),
		call("c5", "member_add", `{"actor_id":"`+someActor+`","preset":"student"}`),
		call("c6", "member_resume", `{"member_id":"`+selfSeat+`"}`),
		call("c7", "member_rescope", `{"member_id":"`+studentSeat+`","listed_students":["`+principalSeat+`"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"your own seat", "the person you act for", "", "the person you act for", "", "your own seat", ""} {
		p := parts[i]
		if want == "" {
			if p.IsError {
				t.Errorf("%s was refused: %s", p.CallID, p.Content)
			}
			continue
		}
		code, msg := errorOf(t, p)
		if !p.IsError || code != core.CodeForbidden || !strings.Contains(msg, want) || !strings.Contains(msg, "nothing was changed") {
			t.Errorf("%s: %s; want forbidden, saying %q", p.CallID, p.Content, want)
		}
	}
	got := toolsCalled(f)
	slices.Sort(got)
	if !slices.Equal(got, []string{"member_add", "member_get", "member_get", "member_remove", "member_rescope"}) {
		t.Errorf("Core was called with %v; want the three writes of other seats, and the student's seat read twice", got)
	}
	for _, c := range f.recorded() {
		for _, id := range []string{selfSeat, principalSeat} {
			if c.args["member_id"] == id {
				t.Errorf("%s reached Core for %s", c.tool, id)
			}
		}
	}
	if want := []string{"member_pause", "member_update_perms", "member_rescope", "member_resume"}; !slices.Equal(w.Guarded, want) {
		t.Errorf("guarded %v, want %v", w.Guarded, want)
	}
	if w.Sent() != 3 || len(w.Records) != 3 || len(w.Refused) != 0 {
		t.Errorf("sent %d, records %+v, refused %v: a guarded write takes no number and spends nothing", w.Sent(), w.Records, w.Refused)
	}
	for _, rec := range w.Records {
		if rec.N < 1 || rec.N > 3 {
			t.Errorf("a write sent is numbered %d", rec.N)
		}
	}
}

// TestRunGuardsTheOpener: an agent nobody owns acts for whoever opened the
// conversation, whose seat it never changes either.
func TestRunGuardsTheOpener(t *testing.T) {
	f := roles(map[string]string{principalSeat: "instructor"})
	r := runner(f)
	r.Writes, r.Guard = keys(1, 10), SeatGuard{Self: selfSeat, Opener: openerSeat}
	parts, err := registrarSet(t).Run(context.Background(), r, courseID, []llm.Part{
		call("c1", "member_remove", `{"member_id":"`+openerSeat+`"}`),
		call("c2", "member_pause", `{"member_id":"`+principalSeat+`"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, msg := errorOf(t, parts[0]); code != core.CodeForbidden || !strings.Contains(msg, "the person you act for ("+openerSeat+")") {
		t.Errorf("the opener's seat removed: %s", parts[0].Content)
	}
	if parts[1].IsError {
		t.Errorf("a seat that is nobody's the agent acts for: %s", parts[1].Content)
	}
	if got := toolsCalled(f); !slices.Equal(got, []string{"member_get", "member_pause"}) {
		t.Errorf("Core was called with %v", got)
	}
}

// TestRunGuardsTheirOtherAgents: a member write never changes the seat of
// another agent of the person the agent acts for (a delegate whose
// principal is theirs), which the runtime reads with member_get first, as
// member_set_role's a sibling's role; nor one whose seat it cannot read. An
// agent of someone else, a student and a seat not in the course (Core then
// says so) are changed as asked. A change to every seat of a role is made
// only when member_list shows no agent of theirs with it.
func TestRunGuardsTheirOtherAgents(t *testing.T) {
	const hidden, gone = "0192f3c1-5e1f-7000-8000-000000000008", "0192f3c1-5e1f-7000-8000-000000000009"
	f := roles(map[string]string{principalSeat: "instructor", studentSeat: "student", siblingSeat: "assistant/" + principalSeat,
		studentsAgent: "assistant/" + studentSeat, hidden: "denied", selfSeat: "assistant/" + principalSeat})
	w := keys(1, 10)
	r := runner(f)
	r.Writes, r.Guard = w, SeatGuard{Self: selfSeat, Principal: principalSeat, Opener: principalSeat}
	bulk := func(id, role string) llm.Part {
		return call(id, "member_update_perms_bulk", `{"role":"`+role+`","perms":{"document_write":"denied"}}`)
	}
	parts, err := registrarSet(t).Run(context.Background(), r, courseID, []llm.Part{
		call("c1", "member_set_role", `{"member_id":"`+strings.ToUpper(siblingSeat)+`","role":"student"}`),
		call("c2", "member_pause", `{"member_id":"`+studentsAgent+`"}`),
		call("c3", "member_set_role", `{"member_id":"`+studentSeat+`","role":"ta"}`),
		call("c4", "member_update_perms", `{"member_id":"`+hidden+`","perms":{"grade_post":"denied"}}`),
		call("c5", "member_remove", `{"member_id":"`+gone+`"}`),
		bulk("c6", "assistant"),
		bulk("c7", "student"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"another agent of the person you act for (" + strings.ToUpper(siblingSeat) + ")", "", "",
		"could not read whose seat it is", "", "the seat of another agent of the person you act for (" + siblingSeat + ") has that role", ""} {
		p := parts[i]
		if want == "" {
			if p.IsError {
				t.Errorf("%s was refused: %s", p.CallID, p.Content)
			}
			continue
		}
		code, msg := errorOf(t, p)
		if !p.IsError || code != core.CodeForbidden || !strings.Contains(msg, want) || !strings.Contains(msg, "nothing was changed") {
			t.Errorf("%s: %s; want forbidden, saying %q", p.CallID, p.Content, want)
		}
	}
	if want := []string{"member_set_role", "member_update_perms", "member_update_perms_bulk"}; !slices.Equal(w.Guarded, want) {
		t.Errorf("guarded %v, want %v", w.Guarded, want)
	}
	for _, c := range f.recorded() {
		if c.tool != "member_get" && c.tool != "member_list" && (sameID(fmt.Sprint(c.args["member_id"]), siblingSeat) || c.args["member_id"] == hidden) {
			t.Errorf("%s reached Core for %v", c.tool, c.args["member_id"])
		}
		if c.tool == "member_list" && (c.args["role"] == nil || c.priority != core.PriorityAnswer) {
			t.Errorf("the roster was read as %+v", c)
		}
	}
	if w.Sent() != 4 {
		t.Errorf("sent %d writes, want the four of other seats", w.Sent())
	}
}

// TestRunGuardsRoleWideChanges: a change to every seat of a role is made
// only when the seat of the person the agent acts for does not have that
// role, which the runtime reads with member_get, as the agent; when it
// cannot read it, nothing is changed either. The agent's own seat is not
// read: Core leaves it out of such a change.
func TestRunGuardsRoleWideChanges(t *testing.T) {
	bulk := func(id, role string) llm.Part {
		return call(id, "member_update_perms_bulk", `{"role":"`+role+`","perms":{"agent_delegate":"denied"}}`)
	}
	f := roles(map[string]string{principalSeat: "instructor", selfSeat: "student"})
	w := keys(1, 10)
	r := runner(f)
	r.Writes, r.Guard = w, SeatGuard{Self: selfSeat, Principal: principalSeat, Opener: principalSeat}
	parts, err := registrarSet(t).Run(context.Background(), r, courseID, []llm.Part{bulk("c1", "instructor"), bulk("c2", "student")})
	if err != nil {
		t.Fatal(err)
	}
	if code, msg := errorOf(t, parts[0]); code != core.CodeForbidden || !strings.Contains(msg, "has that role") ||
		!strings.Contains(msg, "one at a time, with member_update_perms") {
		t.Errorf("every instructor's seat, Sato's among them: %s", parts[0].Content)
	}
	if parts[1].IsError {
		t.Errorf("every student's seat: %s", parts[1].Content)
	}
	calls := f.recorded()
	if got := toolsCalled(f); !slices.Equal(got, []string{"member_get", "member_get", "member_list", "member_update_perms_bulk"}) {
		t.Fatalf("Core was called with %v", got)
	}
	for _, c := range calls[:2] {
		if c.args["member_id"] != principalSeat || c.args["course_id"] != courseID || c.priority != core.PriorityAnswer {
			t.Errorf("the role was read as %+v", c)
		}
	}
	if !slices.Equal(w.Guarded, []string{"member_update_perms_bulk"}) || w.Sent() != 1 {
		t.Errorf("guarded %v, sent %d", w.Guarded, w.Sent())
	}

	// The opener's seat, which Core does not show the agent: refused.
	f = roles(map[string]string{principalSeat: "instructor"})
	r = runner(f)
	r.Writes, r.Guard = keys(1, 10), SeatGuard{Self: selfSeat, Opener: openerSeat}
	parts, err = registrarSet(t).Run(context.Background(), r, courseID, []llm.Part{bulk("c1", "ta")})
	if err != nil {
		t.Fatal(err)
	}
	if code, msg := errorOf(t, parts[0]); code != core.CodeForbidden || !strings.Contains(msg, "could not read whether the seat of the person you act for") {
		t.Errorf("a role Core does not show: %s", parts[0].Content)
	}
	if got := toolsCalled(f); !slices.Equal(got, []string{"member_get"}) {
		t.Errorf("Core was called with %v", got)
	}

	// Core not reached for the role: refused, and the answer goes on.
	f = &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
		return nil, &core.TransientError{Status: 503, Err: errors.New("unavailable")}
	}}
	r = runner(f)
	r.Writes, r.Guard = keys(1, 10), SeatGuard{Self: selfSeat, Opener: openerSeat}
	parts, err = registrarSet(t).Run(context.Background(), r, courseID, []llm.Part{bulk("c1", "ta")})
	if err != nil || !parts[0].IsError || !strings.Contains(parts[0].Content, "could not read") {
		t.Errorf("Core unreachable for the role: %v %+v", err, parts)
	}

	// Core refusing the token while the role is read: the answer stops.
	f = &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
		return nil, core.ErrUnauthenticated
	}}
	r = runner(f)
	r.Writes, r.Guard = keys(1, 10), SeatGuard{Self: selfSeat, Opener: openerSeat}
	if parts, err = registrarSet(t).Run(context.Background(), r, courseID, []llm.Part{bulk("c1", "ta")}); !errors.Is(err, core.ErrUnauthenticated) || parts != nil {
		t.Errorf("a 401 while the role was read: %v %+v", err, parts)
	}

	// The answer's time running out while the role is read: it stops.
	ctx, cancel := context.WithCancel(context.Background())
	f = &fakeCore{respond: func(context.Context, string, json.RawMessage) (*core.Envelope, error) {
		cancel()
		return nil, &core.TransientError{Err: context.Canceled}
	}}
	r = runner(f)
	r.Writes, r.Guard = keys(1, 10), SeatGuard{Self: selfSeat, Opener: openerSeat}
	if parts, err = registrarSet(t).Run(ctx, r, courseID, []llm.Part{bulk("c1", "ta")}); !errors.Is(err, context.Canceled) || parts != nil {
		t.Errorf("the context done while the role was read: %v %+v", err, parts)
	}

	// Nobody to act for but itself (an opener who is the agent cannot be,
	// but a guard may say so): nothing to read, and the change is sent.
	f = roles(nil)
	r = runner(f)
	r.Writes, r.Guard = keys(1, 10), SeatGuard{Self: selfSeat, Opener: selfSeat}
	if parts, err = registrarSet(t).Run(context.Background(), r, courseID, []llm.Part{bulk("c1", "student")}); err != nil || parts[0].IsError {
		t.Errorf("a role-wide change with no one's seat to keep: %v %+v", err, parts)
	}
	if got := toolsCalled(f); !slices.Equal(got, []string{"member_update_perms_bulk"}) {
		t.Errorf("Core was called with %v", got)
	}
}

// TestRunMemberWritesNeedAGuard: a runner that does not say which seats to
// keep refuses every member write, and nothing else.
func TestRunMemberWritesNeedAGuard(t *testing.T) {
	f := &fakeCore{}
	r := runner(f)
	r.Writes = keys(1, 10)
	s, err := snapshot(t).Build(map[string]string{"member_manage": "autonomous", "document_write": "autonomous"},
		config.Tools{Writes: true}, ReadWrite, toolschema.OpenAIStrict, nil)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := s.Run(context.Background(), r, courseID, []llm.Part{
		call("c1", "member_add", `{"actor_id":"`+someActor+`","preset":"student"}`),
		createDoc("c2", "Notes"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, msg := errorOf(t, parts[0]); code != core.CodeForbidden || !strings.Contains(msg, "does not know which seats") {
		t.Errorf("a member write without a guard: %s", parts[0].Content)
	}
	if parts[1].IsError {
		t.Errorf("a document write without a guard: %s", parts[1].Content)
	}
	if got := toolsCalled(f); !slices.Equal(got, []string{"document_create"}) {
		t.Errorf("Core was called with %v", got)
	}
}

// TestSeatGuardCheck is the guard on arguments no tool of the pinned
// catalogue takes yet: a list of seats, any one of which is kept, refuses
// the whole write.
func TestSeatGuardCheck(t *testing.T) {
	g := SeatGuard{Self: selfSeat, Principal: principalSeat, Opener: principalSeat}
	c := core.NewClient(roles(map[string]string{studentSeat: "student", siblingSeat: "assistant/" + principalSeat}))
	for args, want := range map[string]string{
		`{"member_ids":["` + studentSeat + `","` + principalSeat + `"]}`: "the person you act for",
		`{"member_ids":["` + studentSeat + `"," ` + selfSeat + ` "]}`:    "your own seat",
		`{"member_ids":["` + studentSeat + `","` + siblingSeat + `"]}`:   "another agent of the person you act for",
		`{"member_ids":["` + studentSeat + `"]}`:                         "",
		`{"member_id":""}`:                                               "",
		`{"member_ids":"not a list"}`:                                    "could not read which seats",
	} {
		got, err := g.check(context.Background(), c, courseID, "member_future", json.RawMessage(args))
		if err != nil || (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %q, %v; want %q", args, got, err, want)
		}
	}
	if got := (SeatGuard{Self: selfSeat, Principal: principalSeat, Opener: strings.ToUpper(principalSeat)}).people(); !slices.Equal(got, []string{principalSeat}) {
		t.Errorf("the people of a delegate's guard: %v", got)
	}
}
