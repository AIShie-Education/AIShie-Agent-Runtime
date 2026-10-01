package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/toolset"
)

// foundHit is a hit of a course_materials_search's result, as a test
// reads it.
type foundHit struct {
	DocumentID string `json:"document_id"`
	VersionID  string `json:"version_id"`
	FileID     string `json:"file_id"`
	Where      string `json:"where"`
	Excerpt    string `json:"excerpt"`
}

// searchHits are the hits of the search's result in the last of a model's
// requests.
func searchHits(t *testing.T, reqs []*llm.Request) []foundHit {
	t.Helper()
	if len(reqs) == 0 {
		t.Fatal("the model was not called")
	}
	p, ok := toolResult(reqs[len(reqs)-1], toolset.SearchTool)
	if !ok || p.IsError {
		t.Fatalf("no search result: %+v", p)
	}
	var res struct {
		Result struct {
			Hits []foundHit `json:"hits"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(p.Content), &res); err != nil {
		t.Fatalf("the search's result: %v\n%s", err, p.Content)
	}
	return res.Result.Hits
}

// TestModelsSearchTheCourseMaterials: Yuki's own agent and Sato's are
// offered the search of the course's materials, and told of it; asked the
// same question, each finds the passage of the published deck that
// answers it, in Traditional Chinese, and Sato's alone, which reads
// drafts as he does, the draft of the exam's answers, though the index is
// the course's, which both share. Each search is counted. Then the draft
// is purged in Core, and its passages go from the index as Sato's seat
// reads the news.
func TestModelsSearchTheCourseMaterials(t *testing.T) {
	w := newWorld(t)
	deck := doctexttest.PPTX(
		doctexttest.Slide{Title: "Week 3: Sorting", Body: []doctexttest.Bullet{{Text: "Merge sort splits the list in two"}}},
		doctexttest.Slide{Title: "排序的複雜度", Body: []doctexttest.Bullet{{Text: "合併排序：O(n log n)"}}},
	)
	deckID, err := w.fc.AddFile(w.co.ID, "Week 3 slides", doctexttest.PPTXType, deck)
	w.ok(err)
	draftID, draftFiles, err := w.fc.AddDraftFiles(w.co.ID, "Exam answers", "",
		fakecore.File{ContentType: "text/markdown", Filename: "answers.md", Data: []byte("# 期末考答案\n\n第一題：合併排序的複雜度是 O(n log n)。")})
	w.ok(err)

	yuki := w.ownAgent("yuki-helper", 0)
	sato := w.ownerAgent("sato-helper", map[string]string{"document_read_draft": "autonomous"})
	search := scripted.ToolCall{Name: toolset.SearchTool, Args: `{"query":"合併排序的複雜度"}`}
	ym := scripted.New(scripted.CallTools(search), scripted.Reply("合併排序：O(n log n)"))
	sm := scripted.New(scripted.CallTools(search), scripted.Reply("合併排序：O(n log n)"))
	st := memstore.New()
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "y", nil, nil), w.agentDoc("sato-helper", "s", nil, nil)),
		models{"y": ym, "s": sm}, workerOpts{store: st})
	c1, _ := w.ask(0, yuki, "排序的複雜度是多少？")
	w.waitAnswers(c1, 1)
	c2, _ := w.askAs(w.satoSeat.ID, sato, "排序的複雜度是多少？")
	w.waitAnswers(c2, 1)
	for _, m := range []*scripted.Adapter{ym, sm} {
		if err := m.Err(); err != nil {
			t.Fatal(err)
		}
		first := m.Requests()[0]
		if !strings.Contains(first.System, toolset.SearchTool+" finds the passages of the course's documents") ||
			!strings.Contains(strings.Join(toolNamesOf(first), " "), toolset.SearchTool) {
			t.Errorf("the model is not offered the search, or not told of it: %v", toolNamesOf(first))
		}
	}

	yh := searchHits(t, ym.Requests())
	if len(yh) == 0 || yh[0].DocumentID != deckID || yh[0].Where != "slide 2" || !strings.Contains(yh[0].Excerpt, "合併排序") {
		t.Errorf("Yuki's agent's hits: %+v", yh)
	}
	for _, h := range yh {
		if h.DocumentID == draftID || strings.Contains(h.Excerpt, "期末考") {
			t.Errorf("Yuki's agent found the draft: %+v", h)
		}
	}
	sh := searchHits(t, sm.Requests())
	var draft *foundHit
	for i, h := range sh {
		if h.DocumentID == draftID {
			draft = &sh[i]
		}
	}
	if draft == nil || draft.FileID != draftFiles[0] || !strings.Contains(draft.Excerpt, "第一題") {
		t.Fatalf("Sato's agent's hits: %+v", sh)
	}
	if n := counter(t, wk.reg, "search_requests_total", map[string]string{"result": "hits"}); n != 2 {
		t.Errorf("search_requests_total{hits} = %v", n)
	}
	if !strings.Contains(w.logs.String(), "the course's materials were searched") || strings.Contains(w.logs.String(), "合併排序的複雜度") {
		t.Errorf("the searches' log lines: missing, or holding the query")
	}

	key := store.SearchFileKey{VersionID: draft.VersionID, Key: draft.FileID}
	if have, err := st.UseSearchFiles(context.Background(), w.co.ID, []store.SearchFileKey{key}, time.Time{}); err != nil || len(have) != 1 {
		t.Fatalf("the index holds the draft: %v %v", have, err)
	}
	w.ok(w.fc.PurgeVersion(draftID))
	eventually(t, "the purged draft dropped from the index", func() bool {
		have, err := st.UseSearchFiles(context.Background(), w.co.ID, []store.SearchFileKey{key}, time.Time{})
		return err == nil && len(have) == 0
	})
}
