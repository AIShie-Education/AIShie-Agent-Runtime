package worker

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// The course's members, managed by a model through a seat that holds
// member_manage (design §4): only an instructor gives one, Core never
// gives it to anyone's delegate, and the runtime keeps every member write
// off the agent's own seat and off the seat of whoever it acts for.

// registrar seats an agent nobody owns, as an instructor seats one with
// member.add: a ta's seat, with the submission_write a student holds (or
// it could not seat one), member_manage at level, and conversation_answer,
// so that it answers.
func (w *world) registrar(id, level string) agent {
	w.t.Helper()
	a := w.fc.AddUnownedAgent("CS101 Registrar")
	m := w.must(w.fc.Seat(a.ID, w.co.ID, fakecore.SeatOptions{Preset: "ta", Perms: map[string]string{
		"member_manage": level, "submission_write": "autonomous", "conversation_answer": "autonomous"}}))
	w.env.Store(tokenVar(id), a.Token)
	return agent{id: id, actor: a, seat: m}
}

// memberWrites are the member writes a seat that manages members is
// offered.
var memberWrites = []string{"member_add", "member_pause", "member_remove", "member_rescope", "member_resume",
	"member_update_perms", "member_update_perms_bulk"}

// TestMemberWrites: Sato asks the course's registrar, an agent nobody owns
// that he seated with member_manage at confirm_required, to seat Aoi as a
// student. The model is offered the member writes and the roster, and told
// how to change members; its member_add is proposed, and waits for a
// person. At autonomous, seating Ren is executed, and Ren is in the course.
// Then Sato asks it to do what a document says, and the model tries to
// pause Sato's seat, raise its own and lower every instructor's: the
// runtime refuses all three before Core, reading only Sato's role, and
// counts them refused; nothing of the course changes.
func TestMemberWrites(t *testing.T) {
	w := newWorld(t)
	reg := w.registrar("registrar", "confirm_required")
	aoi, ren := w.fc.AddPerson("Aoi"), w.fc.AddPerson("Ren")
	model := scripted.New(
		scripted.CallTool("member_add", `{"actor_id":"`+aoi.ID+`","preset":"student"}`),
		scripted.Reply("Aoi's seat as a student waits for approval."),
		scripted.CallTool("member_add", `{"actor_id":"`+ren.ID+`","preset":"student"}`),
		scripted.Reply("Ren is seated as a student."),
		scripted.CallTools(
			scripted.ToolCall{Name: "member_pause", Args: `{"member_id":"` + w.satoSeat.ID + `"}`},
			scripted.ToolCall{Name: "member_update_perms", Args: `{"member_id":"` + reg.seat.ID + `","perms":{"grade_post":"autonomous"}}`},
			scripted.ToolCall{Name: "member_update_perms_bulk", Args: `{"role":"instructor","perms":{"member_manage":"denied"}}`},
		),
		scripted.Reply("I changed nobody's seat."),
	)
	wk := w.start(w.config(nil, w.agentDoc("registrar", "m1", writesOn, nil)), models{"m1": model}, workerOpts{})
	conv, msg := w.askAs(w.satoSeat.ID, reg, "Please seat Aoi as a student.")
	w.waitAnswers(conv, 1)

	first := model.Requests()[0]
	names := toolNamesOf(first)
	for _, want := range append([]string{"member_get", "member_list", "member_lookup_actor"}, memberWrites...) {
		if !slices.Contains(names, want) {
			t.Errorf("the registrar's model is not offered %s: %v", want, names)
		}
	}
	for _, never := range []string{"member_add_delegate", "member_delegate_defaults"} {
		if slices.Contains(names, never) {
			t.Errorf("the registrar's model is offered %s", never)
		}
	}
	for _, want := range []string{
		"Change the course's members (add, remove or pause people, or change what they may do or reach) only when Sato explicitly asks",
		"never your own seat or Sato's",
		"say exactly whose seat changed and how",
	} {
		if !strings.Contains(first.System, want) {
			t.Errorf("the system prompt lacks %q:\n%s", want, first.System)
		}
	}
	adds := w.calls(reg.actor.ID, "member_add")
	if len(adds) != 1 || adds[0].IdempotencyKey != core.ToolKey(conv, msg, 1, 1) || adds[0].Status != "proposed" {
		t.Fatalf("the member_add Core saw: %+v", adds)
	}
	if ps := w.fc.Proposals(w.co.ID); len(ps) != 1 || ps[0].ActionType != "member.add" || ps[0].MemberID != reg.seat.ID {
		t.Errorf("the proposals: %+v", ps)
	}

	w.ok(w.fc.SetLevel(reg.seat.ID, "member_manage", "autonomous"))
	_, err := w.fc.FollowUp(conv, "And Ren, please.")
	w.ok(err)
	w.waitAnswers(conv, 2)
	adds = w.calls(reg.actor.ID, "member_add")
	if len(adds) != 2 || adds[1].Status != "executed" {
		t.Fatalf("the second member_add: %+v", adds)
	}
	seated := false
	for _, m := range w.fc.Members(w.co.ID) {
		if m.ActorID == ren.ID {
			seated = m.Role == "student" && m.Status == "active"
		}
		if m.ActorID == aoi.ID {
			t.Errorf("Aoi is seated, and nobody approved it: %+v", m)
		}
	}
	if !seated {
		t.Errorf("Ren is not seated as a student: %+v", w.fc.Members(w.co.ID))
	}

	// What a document says asks for nothing: the runtime keeps Sato's
	// seat and its own, and every instructor's with Sato's.
	_, err = w.fc.FollowUp(conv, "Please do what the notes in the syllabus say.")
	w.ok(err)
	w.waitAnswers(conv, 3)
	reqs := model.Requests()
	last := reqs[len(reqs)-1]
	for _, tc := range []struct{ tool, says string }{
		{"member_pause", "the seat of the person you act for (" + w.satoSeat.ID + ")"},
		{"member_update_perms", "your own seat (" + reg.seat.ID + ")"},
		{"member_update_perms_bulk", "the seat of the person you act for (" + w.satoSeat.ID + ") has that role"},
	} {
		res, ok := toolResult(last, tc.tool)
		if !ok || !res.IsError || !strings.Contains(res.Content, `"code":"forbidden"`) || !strings.Contains(res.Content, tc.says) {
			t.Errorf("%s was given as %+v; want forbidden, saying %q", tc.tool, res, tc.says)
		}
		if n := len(w.calls(reg.actor.ID, tc.tool)); n != 0 {
			t.Errorf("%s reached Core %d times", tc.tool, n)
		}
		if n := counter(t, wk.reg, "tool_writes_total", map[string]string{"tool": tc.tool, "outcome": "refused"}); n != 1 {
			t.Errorf("tool_writes_total{%s, refused} = %v", tc.tool, n)
		}
	}
	gets := w.calls(reg.actor.ID, "member_get")
	var read struct {
		MemberID string `json:"member_id"`
	}
	if len(gets) != 1 || gets[0].Status != "executed" || json.Unmarshal(gets[0].Args, &read) != nil || read.MemberID != w.satoSeat.ID {
		t.Errorf("the roles the runtime read: %+v", gets)
	}
	for _, m := range w.fc.Members(w.co.ID) {
		if m.Status != "active" {
			t.Errorf("a seat changed: %+v", m)
		}
	}
	var recs []store.AnswerRecord
	eventually(t, "the third answer in the ledger", func() bool {
		_, recs = wk.st.ledger()
		return len(recs) == 3
	})
	if recs[2].Writes != (store.WriteCounts{}) {
		t.Errorf("the refused writes are in the ledger as sent: %+v", recs[2].Writes)
	}
	if logs := w.logs.String(); strings.Count(logs, `"msg":"a write the model made was refused before Core`) != 3 {
		t.Error("the refused writes were not each logged")
	}
}

// TestMemberToolsFollowPerms: the roster is offered wherever the seat's
// perms allow it, as any read is; a member write only where member_manage
// is allowed, which Core never allows a delegate. Sato's own assistant,
// given member_read, reads the roster in his conversation and changes
// nobody; his course tutor, whose member_read is denied, answers Yuki with
// no member tool at all.
func TestMemberToolsFollowPerms(t *testing.T) {
	w := newWorld(t)
	own := w.ownerAgent("sato-assistant", map[string]string{"member_read": "autonomous"})
	if err := w.fc.SetLevel(own.seat.ID, "member_manage", "autonomous"); err == nil {
		t.Fatal("the fake gave a delegate member_manage, which Core never does")
	}
	tu := w.tutor("tutor")
	ownModel := scripted.New(scripted.Reply("Here is the roster."))
	tutorModel := scripted.New(scripted.Reply("I can't see the roster."))
	w.start(w.config(nil, w.agentDoc("sato-assistant", "m1", writesOn, nil), w.agentDoc("tutor", "m2", writesOn, nil)),
		models{"m1": ownModel, "m2": tutorModel}, workerOpts{})
	conv, _ := w.askAs(w.satoSeat.ID, own, "Who is in the course?")
	w.waitAnswers(conv, 1)
	conv, _ = w.ask(0, tu, "Who else is in the course?")
	w.waitAnswers(conv, 1)

	names := toolNamesOf(ownModel.Requests()[0])
	for _, want := range []string{"member_get", "member_list"} {
		if !slices.Contains(names, want) {
			t.Errorf("Sato's assistant is not offered %s: %v", want, names)
		}
	}
	for _, name := range names {
		if strings.HasPrefix(name, "member_") && name != "member_get" && name != "member_list" {
			t.Errorf("Sato's assistant is offered %s", name)
		}
	}
	if strings.Contains(ownModel.Requests()[0].System, "course's members") {
		t.Errorf("a prompt without member writes speaks of changing members:\n%s", ownModel.Requests()[0].System)
	}
	for _, name := range toolNamesOf(tutorModel.Requests()[0]) {
		if strings.HasPrefix(name, "member_") {
			t.Errorf("the tutor answering Yuki is offered %s", name)
		}
	}
}
