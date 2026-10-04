package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
)

// deleteAssignment is how the tests ask for an assignment to be deleted:
// the responder deletes the one whose id follows.
const deleteAssignment = "Delete the assignment "

// deletionResponder is a model asked to delete an assignment as Core's
// description of assignment_delete tells it to: it reads what would go
// first (assignment_delete_preview), sends the counts back unchanged as
// confirm, whatever the preview said it would be refused, and says what
// became of it, from Core's envelope as the runtime gave it.
func deletionResponder(req fakellm.ChatRequest) fakellm.ChatResponse {
	q := question(req)
	if !strings.HasPrefix(q, deleteAssignment) {
		return fakellm.Reply("Answer: " + q)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(q, deleteAssignment), ".")
	var results []string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			results = append(results, m.Text())
		}
	}
	var env struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
		Error  struct {
			Code    string `json:"code"`
			Details struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	switch {
	case len(results) == 0:
		return fakellm.CallTools(fakellm.FunctionCall{Name: "assignment_delete_preview", Arguments: fmt.Sprintf(`{"assignment_id":%q}`, id)})
	case json.Unmarshal([]byte(results[len(results)-1]), &env) != nil:
		return fakellm.Reply("I could not read what Core said.")
	case len(results) == 1:
		var preview struct {
			Counts json.RawMessage `json:"counts"`
		}
		if env.Status != "executed" || json.Unmarshal(env.Result, &preview) != nil {
			return fakellm.Reply(fmt.Sprintf("I could not read what would go: %s %s.", env.Status, env.Error.Code))
		}
		return fakellm.CallTools(fakellm.FunctionCall{Name: "assignment_delete",
			Arguments: fmt.Sprintf(`{"assignment_id":%q,"confirm":%s}`, id, preview.Counts)})
	}
	var done struct {
		Title string `json:"title"`
	}
	if env.Status == "executed" && json.Unmarshal(env.Result, &done) == nil {
		return fakellm.Reply(fmt.Sprintf("Deleted %s.", done.Title))
	}
	return fakellm.Reply(fmt.Sprintf("That was refused: %s %s %s.", env.Status, env.Error.Code, env.Error.Details.Reason))
}

// assignmentDeletedByAnAgent is an assignment deleted for good through a
// model (design §4), against the real Core: Sato's assistant, his
// delegate reaching every student, with assignment_write autonomous, is
// asked in Sato's own conversation to delete an unpublished quiz nobody
// has started on. It reads what would go, sends the counts back, and the
// quiz is deleted, under the key of its attempt's first write, which a
// replay shows; Core says of it after that it was deleted, by that
// action. Asked to delete a quiz Yuki handed work in on, it reads that
// Core would refuse it, and is refused: an agent deletes no work
// (people_only), and the quiz and Yuki's work stay.
func assignmentDeletedByAnAgent(t *testing.T, w *world) {
	api := w.api
	assistant := w.newAgent(t, w.sato, "Sato's assistant")
	assistant.member = w.memberID(t, w.sato.token, w.path("/delegates"), map[string]any{"actor_id": assistant.id, "preset": "delegate",
		"student_scope": "all", "perms": map[string]string{"assignment_write": "autonomous"}})
	quiz := func(title string, publish bool) string {
		id := result[struct {
			ID string `json:"id"`
		}](t, api, w.sato.token, "POST", w.path("/assignments"), map[string]any{"title": title, "points_possible": 10}).ID
		if publish {
			api.call(t, http.StatusOK, w.sato.token, "POST", w.path("/assignments/"+id+"/publish"), nil)
		}
		return id
	}
	empty, worked := quiz("Quiz A", false), quiz("Quiz B", true)
	work := result[struct {
		SubmissionID string `json:"submission_id"`
	}](t, api, w.yuki.token, "POST", w.path("/submissions"), map[string]any{"assignment_id": worked, "body": "My answers."}).SubmissionID
	api.call(t, http.StatusOK, w.yuki.token, "POST", w.path("/submissions/"+work+"/submit"), nil)

	m := newModel(t, deletionResponder)
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{
		{id: "sato-assistant", seat: assistant, over: map[string]any{"tools": map[string]any{"writes": true}}},
	}})
	rt.waitPolling("sato-assistant")
	deletions := func() []action {
		var out []action
		for _, act := range w.actionsMine(t, rt, "sato-assistant") {
			if act.ActionType == "assignment.delete" {
				out = append(out, act)
			}
		}
		return out
	}

	// Nobody has started on it: deleted.
	conv1, m1 := w.ask(t, w.sato, assistant.member, deleteAssignment+empty+".")
	if a := w.waitAnswer(t, w.sato, conv1, assistant.member); a.text() != "Deleted Quiz A." {
		t.Errorf("the answer is %q; want it deleted", a.text())
	}
	done := deletions()
	if len(done) != 1 || done[0].Status != "executed" {
		t.Fatalf("the assistant's assignment.delete actions: %+v", done)
	}
	gone := api.call(t, http.StatusNotFound, w.sato.token, "GET", w.path("/assignments/"+empty), nil)
	if gone.Error == nil || gone.Error.Details["reason"] != "deleted" || gone.Error.Details["by_action_id"] != done[0].ID {
		t.Errorf("Quiz A read after: %s; want it deleted by %s", gone, done[0].ID)
	}
	zero := map[string]int{"submissions": 0, "handed_in": 0, "drafts": 0, "missing": 0, "grades": 0, "posted": 0, "files": 0,
		"proposals": 0, "totals": 0}
	r, err := api.send(t.Context(), rt.token("sato-assistant"), "POST", w.path("/assignments/"+empty+"/delete"),
		map[string]any{"confirm": zero}, toolKey(conv1, m1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if r.HTTP != http.StatusOK || r.Status != "executed" || !r.Replayed || r.ActionID != done[0].ID {
		t.Errorf("the deletion sent again under tool:{x}:{m}:1:1: %s; want %s replayed", r, done[0].ID)
	}

	// Yuki handed work in: refused, and it stays.
	conv2, _ := w.ask(t, w.sato, assistant.member, deleteAssignment+worked+".")
	if a := w.waitAnswer(t, w.sato, conv2, assistant.member); a.text() != "That was refused: failed forbidden people_only." {
		t.Errorf("the answer is %q; want people_only", a.text())
	}
	var previewed bool
	for _, req := range m.Requests() {
		for _, msg := range req.Messages {
			if msg.Role == "tool" && strings.Contains(msg.Text(), `"refusal":"people_only"`) && strings.Contains(msg.Text(), `"handed_in":1`) {
				previewed = true
			}
		}
	}
	if !previewed {
		t.Error("the model was never shown that Core would refuse it, with Yuki's work counted")
	}
	if a := result[struct {
		Title string `json:"title"`
	}](t, api, w.sato.token, "GET", w.path("/assignments/"+worked), nil); a.Title != "Quiz B" {
		t.Errorf("Quiz B after: %+v", a)
	}
	result[struct{}](t, api, w.sato.token, "GET", w.path("/submissions/"+work), nil)
	for outcome, want := range map[string]float64{"executed": 1, "failed": 1} {
		if n := rt.metric("tool_writes_total", map[string]string{"tool": "assignment_delete", "outcome": outcome}); n != want {
			t.Errorf("tool_writes_total{%s} = %v, want %v", outcome, n, want)
		}
	}
}
