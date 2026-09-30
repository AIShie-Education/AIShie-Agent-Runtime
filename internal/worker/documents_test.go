package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
)

// fileResult is what a document_get result says of its file: the record
// and the text given.
type fileResult struct {
	File struct {
		Name          string `json:"name"`
		GivenAs       string `json:"given_as"`
		ExtractedFrom string `json:"extracted_from"`
		Note          string `json:"note"`
		Part          int    `json:"part"`
		Parts         int    `json:"parts"`
		PartHolds     string `json:"part_holds"`
		NextPart      *struct {
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"next_part"`
	} `json:"file"`
	FileText string `json:"file_text"`
	// Files are a version of several files': each's record and text.
	Files []struct {
		FileID   string `json:"file_id"`
		Name     string `json:"name"`
		GivenAs  string `json:"given_as"`
		FileText string `json:"file_text"`
	} `json:"files"`
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
			"it looks scanned, or like pictures of text; the runtime has no OCR here to recognize its text; ask for a version with selectable text" {
		t.Errorf("Ken's scan: %+v", s)
	}
}

// countBlobs counts the files fetched from the fake Core through the
// worker's client.
type countBlobs struct {
	next  http.RoundTripper
	blobs atomic.Int32
}

func (c *countBlobs) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.Path, "/v1/blobs/") {
		c.blobs.Add(1)
	}
	return c.next.RoundTrip(req)
}

// TestLongDeckReadInParts: a lecture's deck too long for one result
// reaches a model that takes no files in parts, end to end. The model
// reads the first, which says how many there are, then asks for each next
// part with the call the part before names, and answers once it has read
// the last. The parts are the runtime's text of the deck, nothing lost at
// their edges and nothing given twice; the worker fetched and read the
// file once for all of them.
func TestLongDeckReadInParts(t *testing.T) {
	w := newWorld(t)
	var slides []doctexttest.Slide
	for i := range 38 {
		slides = append(slides, doctexttest.Slide{
			Title: fmt.Sprintf("第%d講 Sorting", i+1),
			Body:  []doctexttest.Bullet{{Text: fmt.Sprintf("Point %d: merge sort splits the list in two", i+1)}, {Text: "穩定排序", Level: 1}},
			Notes: strings.Repeat(fmt.Sprintf("Ask the class about slide %d. ", i+1), 45),
		})
	}
	deck := doctexttest.PPTX(slides...)
	want, err := doctext.Extract(context.Background(), deck, doctext.PPTX, doctext.Limits{})
	w.ok(err)
	id, err := w.fc.AddFile(w.co.ID, "Week 3 slides", doctexttest.PPTXType, deck)
	w.ok(err)

	// follow reads the part the last result gave and asks for the next,
	// or answers when it was the last.
	var parts []fileResult
	follow := func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		res, _ := resultsOf(t, req)
		if len(res) != 1 {
			return nil, fmt.Errorf("%d results", len(res))
		}
		parts = append(parts, res[0])
		if n := res[0].File.NextPart; n != nil {
			return scripted.CallTool(n.Tool, string(n.Arguments))(context.Background(), req)
		}
		return scripted.Reply("Slide 38 says the list is split in two.")(context.Background(), req)
	}
	m := scripted.New(scripted.CallTool("document_get", `{"document_id":"`+id+`"}`), follow, follow, follow, follow).
		WithCapabilities(llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true})
	counter := &countBlobs{next: http.DefaultTransport}
	yuki := w.ownAgent("yuki-helper", 0)
	w.start(w.config(nil, w.agentDoc("yuki-helper", "text", nil, nil)), models{"text": m},
		workerOpts{edit: func(o *Options) {
			o.HTTPClient = &http.Client{Timeout: 5 * time.Second, Transport: counter}
			o.HostedHTTPClient = o.HTTPClient
		}})
	c, _ := w.ask(0, yuki, "What does the last slide of week 3 say?")
	if a := w.waitAnswers(c, 1); a[0].Body != "Slide 38 says the list is split in two." {
		t.Errorf("the answer: %q", a[0].Body)
	}
	w.ok(m.Err())

	if len(parts) < 3 || parts[0].File.Parts != len(parts) || !strings.Contains(parts[0].File.Note, fmt.Sprintf("given in %d parts", len(parts))) {
		t.Fatalf("%d parts read; the first says %+v", len(parts), parts[0].File)
	}
	var got strings.Builder
	for i, p := range parts {
		if p.File.GivenAs != "text" || p.File.ExtractedFrom != "pptx" || p.File.Part != i+1 || !strings.HasPrefix(p.FileText, "## Slide ") {
			t.Errorf("part %d: %+v", i+1, p.File)
		}
		got.WriteString(p.FileText)
	}
	if got.String() != want.Text {
		t.Errorf("the parts are not the deck's text: %d bytes of %d", got.Len(), len(want.Text))
	}
	if n := counter.blobs.Load(); n != 1 {
		t.Errorf("the deck was fetched %d times, want once", n)
	}
}

// gatedRecognizer recognizes a file as text once released, counting its
// runs.
type gatedRecognizer struct {
	release chan struct{}
	runs    atomic.Int32
	text    string
}

func (g *gatedRecognizer) Recognize(ctx context.Context, _ []byte, _ ocr.Kind, pages int, progress func(done, of int)) (*ocr.Result, error) {
	g.runs.Add(1)
	progress(0, pages)
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	progress(pages, pages)
	return &ocr.Result{Text: g.text, Sections: []ocr.Section{{N: 1}}, Pages: 1, Of: 1}, nil
}

func (*gatedRecognizer) Describe() string { return "gated 1.0" }

// TestScanReadByOCR: a scanned handout reaches a model that takes no files
// as what OCR recognized of it, end to end. The first question starts its
// recognition, in the background, and the model is told it is in
// progress, with the call to ask again; asked again once it is done, the
// model is given the text, marked as OCR's, and answers from it. The file
// was fetched once and recognized once, and its text is kept in the store
// by its checksum.
func TestScanReadByOCR(t *testing.T) {
	w := newWorld(t)
	scan := doctexttest.PDF(doctexttest.PDFPage{Image: true})
	scanID, err := w.fc.AddFile(w.co.ID, "Old handout", "application/pdf", scan)
	w.ok(err)
	rec := &gatedRecognizer{release: make(chan struct{}), text: "## Page 1\n期中考試範圍：第一章到第五章"}
	st := memstore.New()
	svc := ocr.NewService(t.Context(), ocr.ServiceOptions{Recognizer: rec, Config: ocr.Config{Wait: time.Second}, Store: st, Holder: "w1"})
	t.Cleanup(svc.Wait)

	var results []fileResult
	var askAgain string
	again := func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		res, _ := resultsOf(t, req)
		results = append(results, res...)
		var raw struct {
			File struct {
				OCR      string `json:"ocr"`
				AskAgain *struct {
					Arguments json.RawMessage `json:"arguments"`
				} `json:"ask_again"`
			} `json:"file"`
		}
		_ = json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Parts[0].Content), &raw)
		if raw.File.OCR != "in_progress" || raw.File.AskAgain == nil {
			return nil, fmt.Errorf("the first result: %+v", res)
		}
		askAgain = string(raw.File.AskAgain.Arguments)
		close(rec.release)
		return scripted.CallTool("document_get", askAgain)(context.Background(), req)
	}
	answer := func(_ context.Context, req *llm.Request) (*llm.Response, error) {
		res, _ := resultsOf(t, req)
		results = append(results, res...)
		return scripted.Reply("第一章到第五章")(context.Background(), req)
	}
	m := scripted.New(scripted.CallTool("document_get", `{"document_id":"`+scanID+`"}`), again, answer).
		WithCapabilities(llm.Capabilities{ParallelToolCalls: true, ToolChoiceNone: true})
	counter := &countBlobs{next: http.DefaultTransport}
	ken := w.ownAgent("ken-helper", 1)
	w.start(w.config(nil, w.agentDoc("ken-helper", "text", nil, nil)), models{"text": m},
		workerOpts{edit: func(o *Options) {
			o.OCR = svc
			o.HTTPClient = &http.Client{Timeout: 5 * time.Second, Transport: counter}
			o.HostedHTTPClient = o.HTTPClient
		}})
	c, _ := w.ask(1, ken, "期中考範圍是什麼？")
	if a := w.waitAnswers(c, 1); a[0].Body != "第一章到第五章" {
		t.Errorf("Ken's answer: %q", a[0].Body)
	}
	w.ok(m.Err())
	if len(results) != 2 {
		t.Fatalf("%d results", len(results))
	}
	if r := results[0]; r.File.GivenAs != "not_given" || r.FileText != "" || !strings.Contains(r.File.Note, "the runtime is recognizing its text now (OCR)") {
		t.Errorf("the first result: %+v", r)
	}
	if !strings.Contains(askAgain, scanID) {
		t.Errorf("ask_again %s", askAgain)
	}
	if r := results[1]; r.File.GivenAs != "text" || r.File.ExtractedFrom != "ocr" || r.FileText != rec.text ||
		!strings.Contains(r.File.Note, "may hold recognition errors") {
		t.Errorf("asked again: %+v", r)
	}
	if n := counter.blobs.Load(); n != 1 || rec.runs.Load() != 1 {
		t.Errorf("fetched %d times, recognized %d times; want once each", n, rec.runs.Load())
	}
	sum := sha256.Sum256(scan)
	if kept, err := st.OCRText(t.Context(), "sha256:"+hex.EncodeToString(sum[:])); err != nil || kept.Text != rec.text || kept.Engine != "gated 1.0" {
		t.Errorf("kept %+v %v", kept, err)
	}
}

// TestTextVersionReachesTheModel: a version whose text version is done
// (here the course's staff wrote it) reaches the model as that text, marked
// as the staff's, in place of the runtime's reading of the file; the
// worker keeps it, and drops it as Core's event says the text changed, the
// model then given the new text.
func TestTextVersionReachesTheModel(t *testing.T) {
	w := newWorld(t)
	docID, err := w.fc.AddFile(w.co.ID, "Reading 3", "application/pdf", doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"Reading 3"}}))
	w.ok(err)
	w.ok(w.fc.EditText(docID, w.satoSeat.ID, "## 第 1 頁\n\nStable sorts keep equal keys in order."))
	get := scripted.ToolCall{Name: "document_get", Args: `{"document_id":"` + docID + `"}`}
	yuki := w.ownAgent("yuki-helper", 0)
	m := scripted.New(scripted.CallTools(get), scripted.Reply("They keep equal keys in order."), scripted.CallTools(get),
		scripted.Reply("Now they say otherwise."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "files", nil, nil)), models{"files": m}, workerOpts{})
	c1, _ := w.ask(0, yuki, "What do stable sorts do?")
	w.waitAnswers(c1, 1)
	res, files := resultsOf(t, m.Requests()[1])
	if d := res[0]; len(files) != 0 || d.File.GivenAs != "text" || d.FileText != "## 第 1 頁\n\nStable sorts keep equal keys in order." ||
		!strings.Contains(d.File.Note, "written or corrected by the course's staff") {
		t.Fatalf("the text version: %+v, %d files", d, len(files))
	}
	if st := wk.sup.texts.Stats(); st.Readings != 1 {
		t.Fatalf("the text version is not kept: %+v", st)
	}
	w.ok(w.fc.EditText(docID, w.satoSeat.ID, "## 第 1 頁\n\nStable sorts may reorder equal keys."))
	eventually(t, "the text kept dropped on the event", func() bool { return wk.sup.texts.Stats().Readings == 0 })
	c2, _ := w.ask(0, yuki, "Are you sure?")
	w.waitAnswers(c2, 1)
	res, _ = resultsOf(t, m.Requests()[3])
	if d := res[0]; d.FileText != "## 第 1 頁\n\nStable sorts may reorder equal keys." {
		t.Errorf("the new text: %+v", d)
	}
}

// TestTextVersionsOfFilesReachTheModel: a version of two files, each with
// a text of its own the course's staff wrote, reaches the model file by
// file under each file's name; the worker keeps each text by its file, and
// drops only the file's whose text Core's event says changed, the model
// then given its new text beside the other's, kept.
func TestTextVersionsOfFilesReachTheModel(t *testing.T) {
	w := newWorld(t)
	docID, ids, err := w.fc.AddFiles(w.co.ID, "Week 3", "",
		fakecore.File{Filename: "slides.pdf", ContentType: "application/pdf", Data: doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"Slides"}})},
		fakecore.File{Filename: "handout.txt", ContentType: "text/plain", Data: []byte("The handout.")})
	w.ok(err)
	w.ok(w.fc.EditFileText(docID, ids[0], w.satoSeat.ID, "## 第 1 頁\n\nThe slides' text."))
	w.ok(w.fc.EditFileText(docID, ids[1], w.satoSeat.ID, "## 第 1 頁\n\nThe handout's text."))
	get := scripted.ToolCall{Name: "document_get", Args: `{"document_id":"` + docID + `"}`}
	yuki := w.ownAgent("yuki-helper", 0)
	m := scripted.New(scripted.CallTools(get), scripted.Reply("Read."), scripted.CallTools(get), scripted.Reply("Read again."))
	wk := w.start(w.config(nil, w.agentDoc("yuki-helper", "files", nil, nil)), models{"files": m}, workerOpts{})
	c1, _ := w.ask(0, yuki, "What is in week 3?")
	w.waitAnswers(c1, 1)
	res, files := resultsOf(t, m.Requests()[1])
	if d := res[0]; len(files) != 0 || len(d.Files) != 2 || d.Files[0].Name != "slides.pdf" || d.Files[0].FileText != "## 第 1 頁\n\nThe slides' text." ||
		d.Files[1].FileID != ids[1] || d.Files[1].FileText != "## 第 1 頁\n\nThe handout's text." {
		t.Fatalf("the files' texts: %+v, %d files", d, len(files))
	}
	if st := wk.sup.texts.Stats(); st.Readings != 2 {
		t.Fatalf("the texts are not kept: %+v", st)
	}
	w.ok(w.fc.EditFileText(docID, ids[1], w.satoSeat.ID, "## 第 1 頁\n\nThe handout, corrected."))
	eventually(t, "the handout's text dropped on its event", func() bool { return wk.sup.texts.Stats().Readings == 1 })
	c2, _ := w.ask(0, yuki, "And now?")
	w.waitAnswers(c2, 1)
	res, _ = resultsOf(t, m.Requests()[3])
	if d := res[0]; len(d.Files) != 2 || d.Files[0].FileText != "## 第 1 頁\n\nThe slides' text." ||
		d.Files[1].FileText != "## 第 1 頁\n\nThe handout, corrected." {
		t.Errorf("the new text: %+v", d)
	}
}
