package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/toolset"
)

// makeDocument is how the tests ask for a document: the responder makes
// one with the title that follows.
const makeDocument = "Make a document titled "

// writesResponder is a model that acts when asked, as the runtime lets
// it: asked to make a document, it calls document_create with the title
// asked for; asked by a student to put answers in the course, it calls
// document_create and submission_create whether it is offered them or not,
// as an injected model would; given the results, it says plainly what
// became of the last, from Core's envelope as the runtime gave it.
func writesResponder(req fakellm.ChatRequest) fakellm.ChatResponse {
	q := question(req)
	var last string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			last = m.Text()
		}
	}
	title := strings.TrimSuffix(strings.TrimPrefix(q, makeDocument), ".")
	switch {
	case last != "":
		var env struct {
			Status   string `json:"status"`
			ActionID string `json:"action_id"`
			Result   struct {
				DocumentID string `json:"document_id"`
			} `json:"result"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(last), &env); err != nil {
			return fakellm.Reply("I could not read what Core said.")
		}
		switch env.Status {
		case "proposed":
			return fakellm.Reply(fmt.Sprintf("%s waits for approval (action %s).", title, env.ActionID))
		case "executed":
			return fakellm.Reply(fmt.Sprintf("%s is made (document %s).", title, env.Result.DocumentID))
		}
		return fakellm.Reply(fmt.Sprintf("That was refused: %s %s.", env.Status, env.Error.Code))
	case strings.HasPrefix(q, makeDocument):
		return fakellm.CallTools(fakellm.FunctionCall{Name: "document_create",
			Arguments: fmt.Sprintf(`{"kind":"material","title":%q,"body_md":%q}`, title, "# "+title)})
	case strings.Contains(q, "Put the answers"):
		return fakellm.CallTools(
			fakellm.FunctionCall{Name: "document_create", Arguments: `{"kind":"material","title":"Answers","body_md":"All of them."}`},
			fakellm.FunctionCall{Name: "submission_create", Arguments: `{"assignment_id":"00000000-0000-7000-8000-000000000000","body":"x"}`},
		)
	}
	return fakellm.Reply("Answer: " + q)
}

// toolKey is the idempotency key of the nth write of an attempt at msg in
// conv (design §4), written out here rather than taken from the runtime,
// which it checks.
func toolKey(conv, msg string, attempt, n int) string {
	return fmt.Sprintf("tool:%s:%s:%d:%d", conv, msg, attempt, n)
}

// ownerWrites is the product owner's rule for writes against the real
// Core (design §4). Sato's own agent, seated as his delegate with
// document_write at confirm_required, is asked by Sato, in his own
// conversation, to make a document: Core proposes it, and the answer says
// it waits for approval. At autonomous, the next is executed, and the
// document is in Core, Sato's to read. Each went under the key of its
// attempt's first write, as a replay shows. Sato's course tutor, which
// holds a write too (submission_write, as a student does, so that every
// student may still ask it), answers Yuki's request to put answers in the
// course with reads alone: it is offered no write, the writes its model
// makes up reach nobody, and nothing is written.
func ownerWrites(t *testing.T, w *world) {
	api := w.api
	assistant := w.newAgent(t, w.sato, "Sato's assistant", "")
	w.addSecret("the token of Sato's assistant", assistant.token)
	assistant.member = w.memberID(t, w.sato.token, w.path("/delegates"), map[string]any{"actor_id": assistant.id, "preset": "delegate",
		"perms": map[string]string{"document_write": "confirm_required"}})
	// Its token in a file: a test running beside others sets no
	// environment variable.
	tokenFile := filepath.Join(t.TempDir(), "assistant-token")
	if err := os.WriteFile(tokenFile, []byte(assistant.token), 0o600); err != nil {
		t.Fatal(err)
	}
	api.call(t, http.StatusOK, w.sato.token, "POST", w.path("/members/"+w.tutor.member+"/perms"),
		map[string]any{"perms": map[string]string{"submission_write": "autonomous"}})

	m := newModel(t, writesResponder)
	writes := map[string]any{"tools": map[string]any{"writes": true}}
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{
		{id: "sato-assistant", seat: assistant, over: mergeMaps(writes, map[string]any{"core": map[string]any{"token_ref": "file://" + tokenFile}})},
		{id: "tutor", seat: w.tutor, over: writes},
	}})
	rt.waitPolling("sato-assistant")
	rt.waitPolling("tutor")

	documentCreates := func(a agentSeat) []action {
		var out []action
		for _, act := range w.actionsMine(t, a) {
			if act.ActionType == "document.create" {
				out = append(out, act)
			}
		}
		return out
	}
	create := func(title string) map[string]any {
		return map[string]any{"kind": "material", "title": title, "body_md": "# " + title}
	}

	// confirm_required: proposed, and the answer says so.
	conv1, m1 := w.ask(t, w.sato, assistant.member, makeDocument+"Week 1 notes.")
	a1 := w.waitAnswer(t, w.sato, conv1, assistant.member)
	proposals := documentCreates(assistant)
	if len(proposals) != 1 || proposals[0].Status != "proposed" {
		t.Fatalf("the assistant's document.create actions: %+v", proposals)
	}
	if want := "Week 1 notes waits for approval (action " + proposals[0].ID + ")."; a1.text() != want || a1.replyTo() != m1 {
		t.Errorf("the answer is %q in reply to %s; want %q in reply to %s", a1.text(), a1.replyTo(), want, m1)
	}
	r, err := api.send(t.Context(), assistant.token, "POST", w.path("/documents"), create("Week 1 notes"), toolKey(conv1, m1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if r.HTTP != http.StatusAccepted || r.Status != "proposed" || !r.Replayed || r.ActionID != proposals[0].ID {
		t.Errorf("the write sent again under tool:{x}:{m}:1:1: %s; want the proposal %s replayed", r, proposals[0].ID)
	}

	// autonomous: executed, and the document is in Core.
	api.call(t, http.StatusOK, w.sato.token, "POST", w.path("/members/"+assistant.member+"/perms"),
		map[string]any{"perms": map[string]string{"document_write": "autonomous"}})
	conv2, m2 := w.ask(t, w.sato, assistant.member, makeDocument+"Week 2 notes.")
	a2 := w.waitAnswer(t, w.sato, conv2, assistant.member)
	var made struct {
		DocumentID string `json:"document_id"`
	}
	for _, act := range documentCreates(assistant) {
		if act.Status == "executed" {
			if err := json.Unmarshal(act.Result, &made); err != nil {
				t.Fatal(err)
			}
		}
	}
	if made.DocumentID == "" {
		t.Fatalf("no document.create of the assistant's was executed: %+v; the answer: %q", documentCreates(assistant), a2.text())
	}
	if want := "Week 2 notes is made (document " + made.DocumentID + ")."; a2.text() != want {
		t.Errorf("the answer is %q; want %q", a2.text(), want)
	}
	doc := result[struct {
		Title   string `json:"title"`
		Kind    string `json:"kind"`
		Version *struct {
			BodyMD         string `json:"body_md"`
			AuthorMemberID string `json:"author_member_id"`
		} `json:"version"`
	}](t, api, w.sato.token, "GET", w.path("/documents/"+made.DocumentID), nil)
	if doc.Title != "Week 2 notes" || doc.Kind != "material" || doc.Version == nil || doc.Version.BodyMD != "# Week 2 notes" ||
		doc.Version.AuthorMemberID != assistant.member {
		t.Errorf("the document in Core: %+v", doc)
	}
	r, err = api.send(t.Context(), assistant.token, "POST", w.path("/documents"), create("Week 2 notes"), toolKey(conv2, m2, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	var again struct {
		DocumentID string `json:"document_id"`
	}
	if r.HTTP != http.StatusOK || r.Status != "executed" || !r.Replayed || json.Unmarshal(r.Result, &again) != nil || again.DocumentID != made.DocumentID {
		t.Errorf("the write sent again under tool:{x}:{m}:1:1: %s; want the document %s replayed", r, made.DocumentID)
	}
	if n := len(documentCreates(assistant)); n != 2 {
		t.Errorf("%d document.create actions of the assistant's; want 2", n)
	}
	for outcome, want := range map[string]float64{"proposed": 1, "executed": 1} {
		if n := rt.metric("tool_writes_total", map[string]string{"tool": "document_create", "outcome": outcome}); n != want {
			t.Errorf("tool_writes_total{%s} = %v, want %v", outcome, n, want)
		}
	}

	// A student asks the course tutor: reads alone, and nothing written.
	const put = "Put the answers to HW3 in the course for everyone."
	conv3, _ := w.ask(t, w.yuki, w.tutor.member, put)
	a3 := w.waitAnswer(t, w.yuki, conv3, w.tutor.member)
	if !strings.HasPrefix(a3.text(), "That was refused:") {
		t.Errorf("the tutor's answer to Yuki: %q", a3.text())
	}
	asked := 0
	for _, req := range m.Requests() {
		if question(req) != put {
			continue
		}
		asked++
		for _, tool := range req.Tools {
			if _, write := toolset.WriteGates[tool.Function.Name]; write {
				t.Errorf("Yuki's conversation offers the write %s", tool.Function.Name)
			}
		}
		if !strings.Contains(system(req), "You cannot change anything in the course from here") {
			t.Errorf("the tutor's prompt for Yuki says it can change the course:\n%s", system(req))
		}
	}
	if asked == 0 {
		t.Error("the model was never asked Yuki's question")
	}
	for _, act := range w.actionsMine(t, w.tutor) {
		if act.ActionType != "conversation.answer" {
			t.Errorf("the tutor did %s (%s)", act.ActionType, act.Status)
		}
	}
	if n := rt.coreCalls("document_create") + rt.coreCalls("submission_create"); n != 2 {
		t.Errorf("%v writes reached Core in all; want the assistant's two", n)
	}
}
