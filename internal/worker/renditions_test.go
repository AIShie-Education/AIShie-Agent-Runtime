package worker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/fakecore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// heldConverter is LibreOffice as the worker's tests hold it: each file
// converted to out, counted.
type heldConverter struct {
	calls atomic.Int32
	out   []byte
}

func (c *heldConverter) Convert(context.Context, []byte, office.Format, office.Target) (*office.Output, error) {
	c.calls.Add(1)
	return &office.Output{Data: c.out, Pages: 1}, nil
}

func (c *heldConverter) Describe() string { return "LibreOffice 0.0 held" }

// TestModelsReadCoreRenditions: Sato's lecture deck and the deck Yuki
// attaches to her question have PDFs Core made; her handout's is still
// queued. Her own agent's model, which takes files, is given the question's
// deck as Core's PDF with the question, and the lecture's as Core's PDF
// when it reads the document, LibreOffice never run for either; the
// handout, read next, is converted here, as before. No log of the worker's
// holds a URL of Core's, of a file or of a PDF.
func TestModelsReadCoreRenditions(t *testing.T) {
	w := newWorld(t)
	pdfOf := func(says string) []byte { return doctexttest.PDF(doctexttest.PDFPage{Lines: []string{says}}) }
	lecture := doctexttest.PPTX(doctexttest.Slide{Title: "Week 5: Hashing", Body: []doctexttest.Bullet{{Text: "Buckets"}}, Notes: "Ask about collisions."})
	mine := doctexttest.PPTX(doctexttest.Slide{Title: "My slides", Body: []doctexttest.Bullet{{Text: "Chaining"}}})
	handout := doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "Lab 5", Heading: 1}}})
	docID, files, err := w.fc.AddFiles(w.co.ID, "Week 5", "", fakecore.File{Filename: "week5.pptx", ContentType: doctexttest.PPTXType, Data: lecture})
	w.ok(err)
	w.ok(w.fc.RenderFile(files[0], pdfOf("Core's PDF of the lecture"), 1))
	handoutID, err := w.fc.AddFile(w.co.ID, "Lab 5 handout", doctexttest.DOCXType, handout)
	w.ok(err)

	yuki := w.ownAgent("yuki-helper", 0)
	m := scripted.New(
		scripted.CallTools(scripted.ToolCall{Name: "document_get", Args: `{"document_id":"` + docID + `"}`}),
		scripted.CallTools(scripted.ToolCall{Name: "document_get", Args: `{"document_id":"` + handoutID + `"}`}),
		scripted.Reply("Your slides read well."))
	// The question, and its deck's PDF, are there before the worker is.
	conv, _, attached := w.askWithFiles(0, yuki, "Do my slides read well?",
		fakecore.File{Filename: "mine.pptx", ContentType: doctexttest.PPTXType, Data: mine})
	w.ok(w.fc.RenderFile(attached[0], pdfOf("Core's PDF of Yuki's deck"), 1))

	conv2 := &heldConverter{out: pdfOf("LibreOffice's PDF here")}
	ctx, cancel := context.WithCancel(context.Background())
	svc := office.NewService(ctx, office.ServiceOptions{Converter: conv2, Log: slog.New(slog.NewJSONHandler(w.logs, nil))})
	t.Cleanup(func() { cancel(); svc.Wait() })
	w.start(w.config(nil, w.agentDoc("yuki-helper", "files", nil, nil)), models{"files": m}, workerOpts{edit: func(o *Options) { o.Office = svc }})
	if a := w.waitAnswers(conv, 1); a[0].Body != "Your slides read well." {
		t.Errorf("the answer: %q", a[0].Body)
	}
	if err := m.Err(); err != nil {
		t.Fatal(err)
	}
	reqs := m.Requests()
	if len(reqs) != 3 {
		t.Fatalf("the model was called %d times", len(reqs))
	}
	if _, given := blocksOf(t, questionOf(t, reqs[0])); len(given) != 1 || !bytes.Equal(given[0].Data, pdfOf("Core's PDF of Yuki's deck")) {
		t.Errorf("the question's deck was given as %+v", given)
	}
	for i, want := range []string{"Core's PDF of the lecture", "LibreOffice's PDF here"} {
		res, given := resultsOf(t, reqs[i+1])
		if len(res) != 1 || res[0].File.GivenAs != "file" || len(given) != 1 || !bytes.Equal(given[0].Data, pdfOf(want)) {
			t.Errorf("read %d: %+v, given %d files", i+1, res, len(given))
		}
	}
	if n := conv2.calls.Load(); n != 1 {
		t.Errorf("LibreOffice was run %d times, for more than the handout", n)
	}
	logs := w.logs.String()
	if !strings.Contains(logs, `"msg":"office: Core's PDF of a file"`) {
		t.Errorf("the log does not say Core's PDFs were taken:\n%s", logs)
	}
	for _, never := range []string{"/v1/blobs/", "mine.pptx", "week5.pptx"} {
		if strings.Contains(logs, never) {
			t.Errorf("the log holds %q", never)
		}
	}
}
