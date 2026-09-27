package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/prompt"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/store"
)

// TestOwnAgentAnswersEndToEnd is the handout's §2.6: Yuki asks her own
// agent, and its answer arrives under answer:X:M1:1, with the model's
// words, a ledger row per model call and one per answer, and a note in
// the conversation's memory.
func TestOwnAgentAnswersEndToEnd(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.Reply("You lost marks for not showing your working."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{})
	wk.waitState("yuki-helper", store.AgentRunning)

	conv, msg := w.ask(0, own, "Why did I lose marks on HW3?")
	got := w.waitAnswers(conv, 1)
	if got[0].IdempotencyKey != core.AnswerKey(conv, msg, 1) || got[0].InReplyTo != msg {
		t.Errorf("answer key %q in reply to %q, want %q to %s", got[0].IdempotencyKey, got[0].InReplyTo, core.AnswerKey(conv, msg, 1), msg)
	}
	if got[0].Body != "You lost marks for not showing your working." {
		t.Errorf("body %q", got[0].Body)
	}
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	req := lastRequest(t, model)
	if n := len(req.Messages); n != 1 || req.Messages[0].Role != llm.RoleUser || req.Messages[0].Parts[0].Text != "Why did I lose marks on HW3?" {
		t.Errorf("the model was given %+v", req.Messages)
	}
	if !strings.Contains(req.System, "Yuki") || !strings.Contains(req.System, "CS101") {
		t.Errorf("the system prompt does not name the asker and the course:\n%s", req.System)
	}

	eventually(t, "the ledger's answer row", func() bool { return len(wk.st.outcomes(conv)) == 1 })
	calls, recs := wk.st.ledger()
	if len(calls) != 1 || calls[0].ConversationID != conv || calls[0].MessageID != msg || calls[0].Input == 0 || calls[0].ID == "" {
		t.Errorf("model calls in the ledger: %+v", calls)
	}
	if r := recs[0]; r.Outcome != store.OutcomePosted || !r.Billable || r.Key != core.AnswerKey(conv, msg, 1) || r.Turns != 1 || r.PromptHash == "" {
		t.Errorf("answer row %+v", r)
	}
	at, err := wk.st.Attempt(context.Background(), "yuki-helper", core.AnswerKey(conv, msg, 1))
	if err != nil || at.State != store.AttemptExecuted || at.PostedMessageID != got[0].ID || at.Kind != kindModel {
		t.Errorf("attempt %+v, %v", at, err)
	}
	notes, err := wk.st.Notes(context.Background(), "yuki-helper", own.seat.ID, conv, 10)
	if err != nil || len(notes) != 1 || notes[0].Kind != store.NoteAnswered || notes[0].MessageID != got[0].ID {
		t.Errorf("memory %+v, %v", notes, err)
	}
}

// TestTutorKeepsEachStudentToTheirOwn is §2.7 and §2.5: a course tutor
// answers two students, and neither's words reach the other's prompt.
func TestTutorKeepsEachStudentToTheirOwn(t *testing.T) {
	w := newWorld(t)
	tu := w.tutor("cs101-tutor")
	model := scripted.New(scripted.Reply("Answer one."), scripted.Reply("Answer two."))
	w.start(w.config(nil, w.agentDoc("cs101-tutor", "m1", nil, nil)), models{"m1": model}, workerOpts{})

	yukiConv, _ := w.ask(0, tu, "Yuki asks: what is recursion, really?")
	kenConv, _ := w.ask(1, tu, "Ken asks: how do loops end?")
	w.waitAnswers(yukiConv, 1)
	w.waitAnswers(kenConv, 1)
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	reqs := model.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model calls, want 2", len(reqs))
	}
	for _, r := range reqs {
		text := requestText(r)
		yuki, ken := strings.Contains(text, "Yuki asks"), strings.Contains(text, "Ken asks")
		if yuki == ken {
			t.Errorf("a request carries Yuki's words %v and Ken's %v:\n%s", yuki, ken, text)
		}
		if yuki && !strings.Contains(r.System, "Yuki") || ken && !strings.Contains(r.System, "Ken") {
			t.Errorf("the system prompt names the wrong asker:\n%s", r.System)
		}
		// A tutor reads the material, and nobody's work.
		var names []string
		for _, tool := range r.Tools {
			names = append(names, tool.Name)
		}
		if strings.Contains(strings.Join(names, ","), "submission") || strings.Contains(strings.Join(names, ","), "grade") || len(names) == 0 {
			t.Errorf("the tutor is offered %v", names)
		}
	}
}

// TestToolCalls runs the model's calls as the toolset says: course_id is
// the conversation's whatever the model wrote, a tool outside the toolset
// is refused with no call to Core, calls in one turn run together and come
// back in order, and a document's file reaches the model as a file.
func TestToolCalls(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	elsewhere := "0192f3c1-7d2e-7c3a-9b1f-2a4c6e8f0a1b"
	model := scripted.New(
		scripted.CallTools(
			scripted.ToolCall{Name: "assignment_list", Args: `{"course_id":"` + elsewhere + `"}`},
			scripted.ToolCall{Name: "document_list", Args: `{}`},
		),
		scripted.CallTool("member_add", `{"actor_id":"`+elsewhere+`","preset":"instructor"}`),
		scripted.CallTool("document_get", `{"document_id":"`+w.co.SlidesID+`"}`),
		scripted.Reply("The slides say so."),
	)
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil)), models{"m1": model}, workerOpts{})
	conv, _ := w.ask(0, own, "What do the lecture slides say?")
	w.waitAnswers(conv, 1)
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}

	lists := w.calls(own.actor.ID, "assignment_list")
	if len(lists) != 1 || !strings.Contains(string(lists[0].Args), w.co.ID) || strings.Contains(string(lists[0].Args), elsewhere) {
		t.Errorf("assignment_list reached Core as %v", lists)
	}
	if n := len(w.calls(own.actor.ID, "member_add")); n != 0 {
		t.Errorf("member_add reached Core %d times", n)
	}
	reqs := model.Requests()
	if len(reqs) != 4 {
		t.Fatalf("%d model calls, want 4", len(reqs))
	}
	// The results of the parallel calls, in the order called.
	res := reqs[1].Messages[len(reqs[1].Messages)-1]
	if res.Role != llm.RoleTool || len(res.Parts) != 2 || res.Parts[0].Name != "assignment_list" || res.Parts[1].Name != "document_list" ||
		res.Parts[0].IsError || res.Parts[1].IsError {
		t.Errorf("the parallel calls' results: %+v", res.Parts)
	}
	refused := reqs[2].Messages[len(reqs[2].Messages)-1].Parts
	if len(refused) != 1 || !refused[0].IsError || !strings.Contains(refused[0].Content, "no such tool here") {
		t.Errorf("member_add's result: %+v", refused)
	}
	var file *llm.File
	for _, p := range reqs[3].Messages[len(reqs[3].Messages)-1].Parts {
		if p.Type == llm.PartFile {
			file = p.File
		}
		if strings.Contains(p.Content, "download_url") {
			t.Error("the model was given the file's URL")
		}
	}
	if file == nil || file.MIME != "application/pdf" || !strings.HasPrefix(string(file.Data), "%PDF") {
		t.Errorf("the model was not given the slides as a file: %+v", file)
	}
}

// TestMovedOn: the opener writes while the answer is being written; Core
// refuses the answer as moved_on, and the newer message is answered under
// its own key (§2.6).
func TestMovedOn(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	var conv atomic.Value
	model := scripted.New(
		scripted.Reply("An answer to the first question.").Then(func(*llm.Request) {
			if _, err := w.fc.FollowUp(conv.Load().(string), "And when is HW4 due?"); err != nil {
				t.Error(err)
			}
		}),
		scripted.Reply("HW4 is due Friday."),
	)
	cfg := w.config(nil, w.agentDoc("yuki-helper", "m1", nil, nil))
	c, m1 := w.ask(0, own, "Why did I lose marks on HW3?")
	conv.Store(c)
	wk := w.start(cfg, models{"m1": model}, workerOpts{})
	got := w.waitAnswers(c, 1)
	msgs := w.fc.Messages(c)
	m2 := msgs[1].ID
	if got[0].InReplyTo != m2 || got[0].IdempotencyKey != core.AnswerKey(c, m2, 1) || got[0].Body != "HW4 is due Friday." {
		t.Errorf("the answer %+v, want one to %s under %s", got[0], m2, core.AnswerKey(c, m2, 1))
	}
	if err := model.Err(); err != nil {
		t.Fatal(err)
	}
	// The second call was given both questions.
	if text := requestText(lastRequest(t, model)); !strings.Contains(text, "HW3") || !strings.Contains(text, "HW4") {
		t.Errorf("the second request:\n%s", text)
	}
	at, err := wk.st.Attempt(context.Background(), "yuki-helper", core.AnswerKey(c, m1, 1))
	if err != nil || at.State != store.AttemptFailed || at.Reason != core.ReasonMovedOn {
		t.Errorf("the first attempt %+v, %v", at, err)
	}
	eventually(t, "two ledger rows", func() bool { return len(wk.st.outcomes(c)) == 2 })
	if o := wk.st.outcomes(c); o[0] != store.OutcomeDropped || o[1] != store.OutcomePosted {
		t.Errorf("outcomes %v", o)
	}
}

// TestRESTTransport: an agent connected over REST answers as one over MCP.
func TestRESTTransport(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	model := scripted.New(scripted.CallTool("assignment_list", `{}`), scripted.Reply("Over REST."))
	over := map[string]any{"core": map[string]any{"transport": "rest"}}
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, nil)), models{"m1": model}, workerOpts{})
	conv, msg := w.ask(0, own, "Which transport?")
	got := w.waitAnswers(conv, 1)
	if got[0].Body != "Over REST." || got[0].IdempotencyKey != core.AnswerKey(conv, msg, 1) {
		t.Errorf("answer %+v", got[0])
	}
	for _, c := range w.calls(own.actor.ID, "") {
		if c.Transport != "rest" {
			t.Fatalf("a call over %s", c.Transport)
		}
	}
	if len(w.calls(own.actor.ID, "assignment_list")) != 1 {
		t.Error("the model's call did not reach Core")
	}
}

// TestPromptFiles: the agent's system_ref replaces the built-in prompt, and
// the course's prompt_append_ref follows it, both read relative to the
// agent's file, with {{agent}}, {{asker}} and {{course}} filled in.
func TestPromptFiles(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	w.ok(os.MkdirAll(filepath.Join(w.dir, "prompts"), 0o700))
	w.ok(os.WriteFile(filepath.Join(w.dir, "prompts", "own.md"), []byte("You are {{agent}}, helping {{asker}} in {{course}}. CUSTOM-BASE."), 0o600))
	w.ok(os.WriteFile(filepath.Join(w.dir, "prompts", "style.md"), []byte("Answer in haiku. COURSE-STYLE."), 0o600))
	model := scripted.New(scripted.Reply("Five seven five."))
	over := map[string]any{"prompt": map[string]any{"system_ref": "prompts/own.md"}}
	courses := map[string]any{w.co.ID: map[string]any{"prompt_append_ref": "prompts/style.md"}}
	w.start(w.config(nil, w.agentDoc("yuki-helper", "m1", over, courses)), models{"m1": model}, workerOpts{})
	conv, _ := w.ask(0, own, "A poem, please.")
	w.waitAnswers(conv, 1)
	sys := lastRequest(t, model).System
	base, style := strings.Index(sys, "CUSTOM-BASE."), strings.Index(sys, "COURSE-STYLE.")
	if base < 0 || style < base || !strings.Contains(sys, "helping Yuki in CS101") || strings.Contains(sys, "{{") {
		t.Errorf("the system prompt:\n%s", sys)
	}
	if !strings.Contains(prompt.Builtin(false), "the personal assistant of") || strings.Contains(sys, "the personal assistant of") {
		t.Errorf("the built-in prompt is still there:\n%s", sys)
	}
}
