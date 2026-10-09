package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// The questions the group responder takes: the id of a group's work to
// grade, or of a group assignment whose peer evaluation to write.
const (
	gradeGroupWork  = "Grade the group's work "
	writePeerReview = "Write my peer evaluation for "
)

// modelEnvelope is the part of Core's envelope a scripted model reads.
type modelEnvelope struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
	Error  struct {
		Code    string `json:"code"`
		Details struct {
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

// toolResults are the results the runtime gave the model in this attempt,
// in order.
func toolResults(req fakellm.ChatRequest) []modelEnvelope {
	var out []modelEnvelope
	for _, m := range req.Messages {
		if m.Role == "tool" {
			var e modelEnvelope
			_ = json.Unmarshal([]byte(m.Text()), &e)
			out = append(out, e)
		}
	}
	return out
}

// groupResponder is a model asked about a group assignment, as Core's
// descriptions of the tools tell it to go about it. Asked to grade a
// group's work, it reads the course's group sets, then the work, whose
// members it finds by name there, and grades it once, taking two points
// off Ken's for missed meetings, and says each member's score. Asked to
// write a student's peer evaluation, it reads the peer form, whom the
// student evaluates in their task, and tries to write a sheet giving them
// the whole share, as no agent is let do; and says what became of it.
func groupResponder(req fakellm.ChatRequest) fakellm.ChatResponse {
	q := question(req)
	results := toolResults(req)
	last := modelEnvelope{}
	if len(results) > 0 {
		last = results[len(results)-1]
	}
	switch {
	case strings.HasPrefix(q, gradeGroupWork):
		work := strings.TrimSuffix(strings.TrimPrefix(q, gradeGroupWork), ".")
		switch len(results) {
		case 0:
			return fakellm.CallTools(fakellm.FunctionCall{Name: "group_set_list", Arguments: `{}`})
		case 1:
			if last.Status != "executed" {
				return fakellm.Reply(fmt.Sprintf("I could not read the groups: %s %s.", last.Status, last.Error.Code))
			}
			return fakellm.CallTools(fakellm.FunctionCall{Name: "submission_get", Arguments: fmt.Sprintf(`{"submission_id":%q}`, work)})
		case 2:
			var sub struct {
				Members []struct {
					MemberID    string `json:"member_id"`
					DisplayName string `json:"display_name"`
				} `json:"members"`
			}
			if last.Status != "executed" || json.Unmarshal(last.Result, &sub) != nil {
				return fakellm.Reply(fmt.Sprintf("I could not read the work: %s %s.", last.Status, last.Error.Code))
			}
			var adjustments []map[string]any
			for _, m := range sub.Members {
				if m.DisplayName == "Ken" {
					adjustments = append(adjustments, map[string]any{"student_member_id": m.MemberID, "kind": "delta", "points": "-2",
						"reason": "Missed two of the group's meetings."})
				}
			}
			args, _ := json.Marshal(map[string]any{"submission_id": work, "score": "18", "feedback": "A clear, well argued report.",
				"adjustments": adjustments})
			return fakellm.CallTools(fakellm.FunctionCall{Name: "grade_submit", Arguments: string(args)})
		}
		var graded struct {
			MemberGrades []struct {
				Score json.Number `json:"score"`
			} `json:"member_grades"`
		}
		if last.Status != "executed" || json.Unmarshal(last.Result, &graded) != nil {
			return fakellm.Reply(fmt.Sprintf("That was refused: %s %s %s.", last.Status, last.Error.Code, last.Error.Details.Reason))
		}
		var scores []string
		for _, g := range graded.MemberGrades {
			scores = append(scores, g.Score.String())
		}
		slices.Sort(scores)
		return fakellm.Reply("Graded: " + strings.Join(scores, ", ") + ".")

	case strings.HasPrefix(q, writePeerReview):
		assignment := strings.TrimSuffix(strings.TrimPrefix(q, writePeerReview), ".")
		switch len(results) {
		case 0:
			return fakellm.CallTools(fakellm.FunctionCall{Name: "peer_form_get", Arguments: fmt.Sprintf(`{"assignment_id":%q}`, assignment)})
		case 1:
			var form struct {
				Task struct {
					ToEvaluate []string `json:"to_evaluate"`
				} `json:"task"`
			}
			if last.Status != "executed" || json.Unmarshal(last.Result, &form) != nil || len(form.Task.ToEvaluate) != 1 {
				return fakellm.Reply(fmt.Sprintf("I could not read the form: %s %s.", last.Status, last.Error.Code))
			}
			return fakellm.CallTools(fakellm.FunctionCall{Name: "peer_review_submit", Arguments: fmt.Sprintf(
				`{"assignment_id":%q,"entries":[{"student_member_id":%q,"share":100}]}`, assignment, form.Task.ToEvaluate[0])})
		}
		return fakellm.Reply(fmt.Sprintf("That was refused: %s %s.", last.Status, last.Error.Code))
	}
	return fakellm.Reply("Answer: " + q)
}

// declared are the names of the tools a model request declares.
func declared(req fakellm.ChatRequest) []string {
	var out []string
	for _, t := range req.Tools {
		out = append(out, t.Function.Name)
	}
	return out
}

// groupWorkGradedByAnAgent is a group assignment against the real Core
// (design §4, the gates of group work): Sato puts Yuki and Ken in Group A
// of a group set, sets a group project of it with a peer form, and Yuki
// hands the group's work in. Sato's assistant, his delegate reaching every
// student with grade_submit autonomous, is offered the course's groups,
// the peer form and its results, and grading, and asked in Sato's own
// conversation to grade the work: it reads the groups and the work, and
// grades it once with Ken adjusted, under its attempt's key; Core holds a
// draft for each member, Ken's moved by the adjustment, saying who made
// it. Yuki's own agent, which holds submission_write, is offered her
// group's sign-up and the peer form, never a peer evaluation; asked by her
// to write hers, it reads the form, and its call of peer_review_submit is
// refused before Core, which records no sheet.
func groupWorkGradedByAnAgent(t *testing.T, w *world) {
	api := w.api
	set := result[struct {
		ID string `json:"id"`
	}](t, api, w.sato.token, "POST", w.path("/group-sets"), map[string]any{"name": "Project groups"}).ID
	groupA := result[struct {
		GroupIDs []string `json:"group_ids"`
	}](t, api, w.sato.token, "POST", w.path("/group-sets/"+set+"/groups"), map[string]any{"groups": []any{map[string]any{"name": "Group A"}}}).GroupIDs[0]
	result[struct{}](t, api, w.sato.token, "POST", w.path("/group-sets/"+set+"/members"), map[string]any{"placements": []any{
		map[string]any{"student_member_id": w.yuki.member, "group_id": groupA}, map[string]any{"student_member_id": w.ken.member, "group_id": groupA}}})
	project := result[struct {
		ID string `json:"id"`
	}](t, api, w.sato.token, "POST", w.path("/assignments"), map[string]any{"title": "Project", "points_possible": 20, "group_set_id": set}).ID
	api.call(t, http.StatusOK, w.sato.token, "POST", w.path("/assignments/"+project+"/publish"), nil)
	result[struct{}](t, api, w.sato.token, "POST", w.path("/assignments/"+project+"/peer-form"), map[string]any{"kind": "share",
		"opens": "on_hand_in", "closes_at": time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339), "weight": 20})
	work := result[struct {
		SubmissionID string `json:"submission_id"`
	}](t, api, w.yuki.token, "POST", w.path("/submissions"), map[string]any{"assignment_id": project, "body": "Our report."}).SubmissionID
	api.call(t, http.StatusOK, w.yuki.token, "POST", w.path("/submissions/"+work+"/submit"), nil)

	assistant := w.newAgent(t, w.sato, "Sato's assistant")
	assistant.member = w.memberID(t, w.sato.token, w.path("/delegates"), map[string]any{"actor_id": assistant.id, "preset": "delegate",
		"student_scope": "all", "perms": map[string]string{"grade_submit": "autonomous", "member_read": "autonomous"}})
	api.call(t, http.StatusOK, w.sato.token, "POST", w.path("/members/"+w.own.member+"/perms"),
		map[string]any{"perms": map[string]string{"submission_write": "confirm_required"}})

	m := newModel(t, groupResponder)
	writes := map[string]any{"tools": map[string]any{"writes": true}}
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{
		{id: "sato-assistant", seat: assistant, over: writes},
		{id: "yuki-own", seat: w.own, over: writes},
	}})
	rt.waitPolling("sato-assistant")
	rt.waitPolling("yuki-own")

	// The assistant grades the group's work once, Ken adjusted.
	conv, msg := w.ask(t, w.sato, assistant.member, gradeGroupWork+work+".")
	if a := w.waitAnswer(t, w.sato, conv, assistant.member); a.text() != "Graded: 16, 18." {
		t.Errorf("the assistant's answer is %q; want Ken's 16 and Yuki's 18", a.text())
	}
	var offered []string
	for _, req := range m.Requests() {
		if strings.HasPrefix(question(req), gradeGroupWork) {
			offered = declared(req)
			break
		}
	}
	for _, name := range []string{"group_set_list", "group_set_get", "peer_form_get", "peer_review_results", "grade_submit", "grade_adjust"} {
		if !slices.Contains(offered, name) {
			t.Errorf("the assistant is not offered %s: %v", name, offered)
		}
	}
	if slices.Contains(offered, "peer_review_submit") || slices.Contains(offered, "group_set_members") {
		t.Errorf("the assistant is offered a peer evaluation, or the forming of groups it may not do: %v", offered)
	}
	grades := result[struct {
		Grades []struct {
			StudentMemberID string      `json:"student_member_id"`
			Score           json.Number `json:"score"`
			State           string      `json:"state"`
			GraderMemberID  string      `json:"grader_member_id"`
			Group           *struct {
				GroupID    string      `json:"group_id"`
				Score      json.Number `json:"score"`
				Adjustment *struct {
					Kind       string      `json:"kind"`
					Points     json.Number `json:"points"`
					ByMemberID string      `json:"by_member_id"`
				} `json:"adjustment"`
			} `json:"group"`
		} `json:"grades"`
	}](t, api, w.sato.token, "GET", w.path("/grades?assignment_id="+project), nil).Grades
	if len(grades) != 2 {
		t.Fatalf("Core holds %d grades on the project: %+v", len(grades), grades)
	}
	for _, g := range grades {
		switch {
		case g.State != "draft" || g.GraderMemberID != assistant.member || g.Group == nil || g.Group.GroupID != groupA || g.Group.Score != "18":
			t.Errorf("a member's grade: %+v", g)
		case g.StudentMemberID == w.ken.member && (g.Score != "16" || g.Group.Adjustment == nil || g.Group.Adjustment.Kind != "delta" ||
			g.Group.Adjustment.Points != "-2" || g.Group.Adjustment.ByMemberID != assistant.member):
			t.Errorf("Ken's grade: %+v %+v", g, g.Group.Adjustment)
		case g.StudentMemberID == w.yuki.member && (g.Score != "18" || g.Group.Adjustment != nil):
			t.Errorf("Yuki's grade: %+v", g)
		}
	}
	// The grade went under the key of the attempt's first write: sent
	// again as the model sent it, Core replays it.
	sent := map[string]any{"submission_id": work, "score": "18", "feedback": "A clear, well argued report.",
		"adjustments": []any{map[string]any{"student_member_id": w.ken.member, "kind": "delta", "points": "-2",
			"reason": "Missed two of the group's meetings."}}}
	r, err := api.send(t.Context(), rt.token("sato-assistant"), "POST", w.path("/grades"), sent, toolKey(conv, msg, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if r.HTTP != http.StatusOK || r.Status != "executed" || !r.Replayed {
		t.Errorf("the grade sent again under tool:{x}:{m}:1:1: %s; want it replayed", r)
	}

	// Yuki's own agent writes no peer evaluation.
	conv2, _ := w.ask(t, w.yuki, w.own.member, writePeerReview+project+".")
	if a := w.waitAnswer(t, w.yuki, conv2, w.own.member); a.text() != "That was refused: error not_found." {
		t.Errorf("Yuki's agent's answer is %q; want its call refused before Core", a.text())
	}
	for _, req := range m.Requests() {
		if !strings.HasPrefix(question(req), writePeerReview) {
			continue
		}
		tools := declared(req)
		if !slices.Contains(tools, "peer_form_get") || !slices.Contains(tools, "group_sign_up") || slices.Contains(tools, "peer_review_submit") {
			t.Errorf("Yuki's agent is offered %v", tools)
		}
	}
	for _, act := range w.actionsMine(t, rt, "yuki-own") {
		if act.ActionType == "peer_review.submit" {
			t.Errorf("Core recorded a peer evaluation of Yuki's agent: %+v", act)
		}
	}
	sheets := result[struct {
		Groups []struct {
			Sheets []json.RawMessage `json:"sheets"`
		} `json:"groups"`
	}](t, api, w.sato.token, "GET", w.path("/assignments/"+project+"/peer-results"), nil).Groups
	if len(sheets) != 1 || len(sheets[0].Sheets) != 0 {
		t.Errorf("the peer results: %+v", sheets)
	}
	if n := rt.metric("tool_writes_total", map[string]string{"tool": "grade_submit", "outcome": "executed"}); n != 1 {
		t.Errorf("tool_writes_total{grade_submit, executed} = %v, want 1", n)
	}
}
