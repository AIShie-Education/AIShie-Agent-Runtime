package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
)

// searchQuestionPrefix asks a model to search the course's materials for
// the words after it.
const searchQuestionPrefix = "Search the course's materials for: "

// searchResponder is a model asked searchQuestionPrefix and some words: it
// searches the course's materials for them, reads the first hit with the
// call the hit names, and answers with each hit's document and where it
// is, then whether what the call gave holds the words. Any other question
// it answers as DefaultResponder does.
func searchResponder(req fakellm.ChatRequest) fakellm.ChatResponse {
	q, ok := strings.CutPrefix(question(req), searchQuestionPrefix)
	if !ok {
		return fakellm.DefaultResponder(req)
	}
	var results []string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			results = append(results, m.Text())
		}
	}
	if len(results) == 0 {
		args, _ := json.Marshal(map[string]string{"query": q})
		return fakellm.CallTools(fakellm.FunctionCall{Name: toolset.SearchTool, Arguments: string(args)})
	}
	var found struct {
		Result struct {
			Hits []struct {
				Title string `json:"title"`
				Where string `json:"where"`
				Read  struct {
					Tool      string         `json:"tool"`
					Arguments map[string]any `json:"arguments"`
				} `json:"read"`
			} `json:"hits"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(results[0]), &found)
	hits := found.Result.Hits
	if len(hits) == 0 {
		return fakellm.Reply("Nothing found.")
	}
	if len(results) == 1 {
		args, _ := json.Marshal(hits[0].Read.Arguments)
		return fakellm.CallTools(fakellm.FunctionCall{Name: hits[0].Read.Tool, Arguments: string(args)})
	}
	var said []string
	for _, h := range hits {
		said = append(said, strings.TrimSpace(h.Title+" "+h.Where))
	}
	read := "the read does not hold it"
	if strings.Contains(results[1], q) {
		read = "the read holds it"
	}
	return fakellm.Reply(strings.Join(said, ", ") + " | " + read)
}

// searchOfTheMaterials: Sato puts up the week's slides, published, and a
// draft of the exam's answers. Sato asks his agent, and then Yuki her own,
// to search the course's materials for a phrase in Traditional Chinese
// that both hold; the two agents run in one runtime, whose index of the
// course's materials they share. Sato's, which reads drafts as he does,
// finds the slide and the draft, which the index then holds; Yuki's finds
// the slide that says it and reads it with the call its hit names, and
// nothing of the draft. An administrator then purges the draft's version,
// and the runtime drops it from its index as Sato's agent reads the
// course's news.
func searchOfTheMaterials(t *testing.T, w *world) {
	if !hasFiles(t, w) {
		return
	}
	assistant := w.newAgent(t, w.sato, "Sato's assistant")
	assistant.member = w.memberID(t, w.sato.token, w.path("/delegates"), map[string]any{"actor_id": assistant.id, "preset": "delegate",
		"perms": map[string]string{"document_read_draft": "autonomous"}})
	m := newModel(t, searchResponder)
	long := map[string]any{"budgets": map[string]any{"per_answer": map[string]any{"wall_clock_s": 120}}}
	rt := w.startRuntime(t, m, runtimeConf{agents: []agentConf{
		{id: "yuki-helper", seat: w.own, over: long}, {id: "sato-assistant", seat: assistant, over: long}}})
	rt.waitPolling("yuki-helper")
	rt.waitPolling("sato-assistant")

	slides := doctexttest.PPTX(
		doctexttest.Slide{Title: "Week 3: Sorting", Body: []doctexttest.Bullet{{Text: "Merge sort splits the list in two"}}},
		doctexttest.Slide{Title: "排序的複雜度", Body: []doctexttest.Bullet{{Text: "合併排序：O(n log n)"}}},
	)
	w.uploadFiles(t, w.sato, "Week 3 slides", "", attachedFile{"week3.pptx", doctexttest.PPTXType, slides})
	draft, draftVersion, draftFiles := w.uploadDraft(t, w.sato, "Exam answers", "",
		attachedFile{"answers.md", "text/markdown", []byte("# 期末考答案\n\n第一題：排序的複雜度是 O(n log n)。")})

	// Sato's first, so that the index holds the draft when Yuki's
	// searches.
	satos, _ := w.ask(t, w.sato, assistant.member, searchQuestionPrefix+"排序的複雜度")
	if a := w.waitAnswer(t, w.sato, satos, assistant.member); !strings.Contains(a.text(), "Exam answers") ||
		!strings.Contains(a.text(), "Week 3 slides slide 2") || !strings.HasSuffix(a.text(), "| the read holds it") {
		t.Errorf("Sato's agent's answer is %q", a.text())
	}
	key := store.SearchFileKey{VersionID: draftVersion, Key: draftFiles[0]}
	if have, err := rt.st.UseSearchFiles(t.Context(), w.course, []store.SearchFileKey{key}, time.Time{}); err != nil || len(have) != 1 {
		t.Fatalf("the index holds the draft: %v %v", have, err)
	}
	yukis, _ := w.ask(t, w.yuki, w.own.member, searchQuestionPrefix+"排序的複雜度")
	if a := w.waitAnswer(t, w.yuki, yukis, w.own.member); a.text() != "Week 3 slides slide 2 | the read holds it" {
		t.Errorf("Yuki's agent's answer is %q", a.text())
	}

	w.api.call(t, http.StatusOK, w.admin.token, "POST", w.path("/documents/"+draft+"/purge"),
		map[string]any{"version_id": draftVersion, "reason": "uploaded by mistake"})
	eventually(t, answerWait, "the purged draft dropped from the runtime's index", func() bool {
		have, err := rt.st.UseSearchFiles(context.Background(), w.course, []store.SearchFileKey{key}, time.Time{})
		return err == nil && len(have) == 0
	})
}
