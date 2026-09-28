package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm/scripted"
)

// fileResult is what a document_get result says of its file: the record
// and the text given.
type fileResult struct {
	File struct {
		Name          string `json:"name"`
		GivenAs       string `json:"given_as"`
		ExtractedFrom string `json:"extracted_from"`
		Note          string `json:"note"`
	} `json:"file"`
	FileText string `json:"file_text"`
}

// resultsOf are the tool results of the last message of a model request.
func resultsOf(t *testing.T, req *llm.Request) ([]fileResult, []*llm.File) {
	t.Helper()
	var out []fileResult
	var files []*llm.File
	for _, p := range req.Messages[len(req.Messages)-1].Parts {
		switch p.Type {
		case llm.PartToolResult:
			var r fileResult
			if err := json.Unmarshal([]byte(p.Content), &r); err != nil {
				t.Fatalf("a result that is not JSON: %v", err)
			}
			out = append(out, r)
		case llm.PartFile:
			files = append(files, p.File)
		}
	}
	return out, files
}

// TestCourseDocumentsReachTheModel: the files of a course's documents reach
// the model as the runtime reads them, whatever the model. A deck of
// slides is the runtime's text of it, to a model that takes files as to
// one that does not. A PDF of more pages than the model's provider takes
// (its adapter's FileLimits) is its text; to a model that takes no files,
// its text too, and a scanned one is not given, with the reason. The model
// answers from what it was given.
func TestCourseDocumentsReachTheModel(t *testing.T) {
	w := newWorld(t)
	deck := doctexttest.PPTX(
		doctexttest.Slide{Title: "Week 3: Sorting", Body: []doctexttest.Bullet{{Text: "Merge sort splits the list in two"}, {Text: "then merges the halves", Level: 1}}},
		doctexttest.Slide{Title: "排序的複雜度", Body: []doctexttest.Bullet{{Text: "合併排序：O(n log n)"}}, Notes: "Ask who has seen quicksort."},
	)
	deckID, err := w.fc.AddFile(w.co.ID, "Week 3 slides", doctexttest.PPTXType, deck)
	w.ok(err)
	readingID, err := w.fc.AddFile(w.co.ID, "Reading 3", "application/pdf", doctexttest.PDF(
		doctexttest.PDFPage{Lines: []string{"Reading 3: stable sorting"}}, doctexttest.PDFPage{CJK: []string{"穩定排序保留相等元素的次序"}}))
	w.ok(err)
	scanID, err := w.fc.AddFile(w.co.ID, "Old handout", "application/pdf", doctexttest.PDF(doctexttest.PDFPage{Image: true}))
	w.ok(err)
	get := func(id string) scripted.ToolCall {
		return scripted.ToolCall{Name: "document_get", Args: `{"document_id":"` + id + `"}`}
	}

	yuki, ken := w.ownAgent("yuki-helper", 0), w.ownAgent("ken-helper", 1)
	// Yuki's model takes files, a PDF of one page at most; Ken's takes none.
	takesFiles := scripted.New(scripted.CallTools(get(deckID), get(readingID)), scripted.Reply("Merge sort splits the list in two.")).
		WithFileLimits(llm.FileLimits{PDFPages: 1})
	noFiles := scripted.New(scripted.CallTools(get(deckID), get(readingID), get(scanID)), scripted.Reply("合併排序：O(n log n)")).
		WithCapabilities(llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true})
	w.start(w.config(nil, w.agentDoc("yuki-helper", "files", nil, nil), w.agentDoc("ken-helper", "text", nil, nil)),
		models{"files": takesFiles, "text": noFiles}, workerOpts{})
	c1, _ := w.ask(0, yuki, "What do the week 3 slides say about merge sort?")
	c2, _ := w.ask(1, ken, "排序的複雜度是多少？")
	if a := w.waitAnswers(c1, 1); a[0].Body != "Merge sort splits the list in two." {
		t.Errorf("Yuki's answer: %q", a[0].Body)
	}
	if a := w.waitAnswers(c2, 1); a[0].Body != "合併排序：O(n log n)" {
		t.Errorf("Ken's answer: %q", a[0].Body)
	}
	for _, m := range []*scripted.Adapter{takesFiles, noFiles} {
		if err := m.Err(); err != nil {
			t.Fatal(err)
		}
	}

	wantDeck := "## Slide 1: Week 3: Sorting\n- Merge sort splits the list in two\n  - then merges the halves\n\n" +
		"## Slide 2: 排序的複雜度\n- 合併排序：O(n log n)\nNotes: Ask who has seen quicksort."
	for who, m := range map[string]*scripted.Adapter{"Yuki": takesFiles, "Ken": noFiles} {
		reqs := m.Requests()
		if len(reqs) != 2 {
			t.Fatalf("%s's model was called %d times", who, len(reqs))
		}
		res, files := resultsOf(t, reqs[1])
		if len(files) != 0 {
			t.Errorf("%s's model was given files: %+v", who, files)
		}
		if d := res[0]; d.File.GivenAs != "text" || d.File.ExtractedFrom != "pptx" || d.FileText != wantDeck ||
			!strings.Contains(d.File.Note, "the runtime's text of its 2 slides, with their speaker notes") {
			t.Errorf("%s's deck: %+v", who, d)
		}
		if r := res[1]; r.File.GivenAs != "text" || r.File.ExtractedFrom != "pdf" ||
			r.FileText != "## Page 1\nReading 3: stable sorting\n\n## Page 2\n穩定排序保留相等元素的次序" {
			t.Errorf("%s's reading: %+v", who, r)
		}
		if who == "Yuki" && !strings.Contains(res[1].File.Note, "since it has 2 pages, more than the 1 this model takes in a file") {
			t.Errorf("Yuki's reading says %q", res[1].File.Note)
		}
	}
	res, _ := resultsOf(t, noFiles.Requests()[1])
	if s := res[2]; s.File.GivenAs != "not_given" || s.FileText != "" ||
		s.File.Note != "the file could not be given to the model: this model does not take files, and the PDF has no text to read: "+
			"it looks scanned, or like pictures of text; ask for a version with selectable text" {
		t.Errorf("Ken's scan: %+v", s)
	}
}
