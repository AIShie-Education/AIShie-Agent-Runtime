package worker

import (
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
)

func TestConversationPolicy(t *testing.T) {
	cat, err := core.ParseCatalogue([]byte(`{"tools":[
		{"name":"course_materials.search","kind":"read","input_schema":{"type":"object"}},
		{"name":"document.text","kind":"read","input_schema":{"type":"object"}},
		{"name":"member.update_perms","kind":"write","input_schema":{"type":"object"}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	principal := "principal"
	m := core.Membership{MemberID: "self", PrincipalMemberID: &principal, Perms: map[string]string{
		"document_read": core.LevelAutonomous, "member_manage": core.LevelAutonomous, "action_decide": core.LevelConfirmRequired,
	}}
	a := &config.Agent{Hosted: &config.Hosted{}}
	eff := &config.Effective{Agent: config.Agent{Tools: config.Tools{Writes: true}}}
	student := ForConversation(cat, a, eff, m, "someone-else")
	if student.AllowWrites || student.Seat.OpenerMemberID != "someone-else" || len(student.AllowedTools) != 2 {
		t.Errorf("student policy = %+v", student)
	}
	owner := ForConversation(cat, a, eff, m, principal)
	if !owner.AllowWrites || len(owner.AllowedTools) != 3 || owner.Seat.SelfMemberID != "self" || owner.Seat.ActionDecideLevel != core.LevelConfirmRequired {
		t.Errorf("owner policy = %+v", owner)
	}
}
