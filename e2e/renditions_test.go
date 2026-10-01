package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/fakellm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
)

// renditionWait is how long a file waits for its PDF rendition: the
// runtime converts what the other worlds queued first, oldest first, a few
// seconds each.
const renditionWait = 4 * time.Minute

// TestRenditionsAgainstCore is the runtime's binary making the PDF
// renditions of the Office files the real Core keeps (AIShie-Core's
// migration 0026), in a world of its own, run alone once the scenarios are
// over: Core's queue of renditions is the whole site's, and the runtime
// converts the other worlds' Office files too, first, which is no matter
// now. It needs a Core with renditions, and LibreOffice here; without
// either it is skipped, saying so (OFFICE_PDF_REQUIRED=1 fails it without
// LibreOffice).
func TestRenditionsAgainstCore(t *testing.T) {
	c, root := liveCore(t)
	cat, err := core.FetchCatalogue(t.Context(), c.hc, c.base)
	if err != nil {
		t.Fatal(err)
	}
	if !core.HasRenditions(cat) {
		t.Skip("the Core under test makes no PDF renditions (agent_runtime.rendition_*): it is older than AIShie-Core's migration 0026")
	}
	if _, err := office.NewConverter(t.Context(), office.Config{TempDir: t.TempDir()}); errors.Is(err, office.ErrUnavailable) {
		if os.Getenv("OFFICE_PDF_REQUIRED") == "1" {
			t.Fatalf("OFFICE_PDF_REQUIRED is set, and LibreOffice is not available: %v", err)
		}
		t.Skipf("LibreOffice is not installed here, so the runtime makes no PDF renditions and none is tried (the image, and CI's, have it): %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t, c, root, "renditions")
	t.Run("renditions", func(t *testing.T) { renditions(t, w) })
	t.Run("no-token-in-any-log", func(t *testing.T) { noTokenInAnyLog(t, []*world{w}) })
}

// The Office files of the world: a Word handout in Chinese, a deck of two
// slides, and a Word file cut short, which no program opens.
var (
	renditionHandout = doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "第五週：雜湊表", Heading: 1},
		{Text: "Open addressing and chaining."}}})
	renditionDeck = doctexttest.PPTX(doctexttest.Slide{Title: "Hashing", Body: []doctexttest.Bullet{{Text: "Buckets"}}},
		doctexttest.Slide{Title: "探測", Body: []doctexttest.Bullet{{Text: "線性探測"}}})
	renditionBroken = renditionHandout[:200]
)

// renditionView is a file's rendition as its reader is shown it.
type renditionView struct {
	Rendition *core.RenditionView `json:"rendition"`
}

// renditions: the runtime's binary runs with nothing configured but Core
// and its own credential, and makes PDF renditions, as it does by default.
// Sato puts up a material of a Word handout, a Word file cut short and a
// text; Yuki asks the course's tutor with her deck attached, declared of
// no particular type. The handout's and the deck's renditions are done in
// Core, each a PDF Yuki opens from where she reads its file, with no
// credential but its URL, inline, named as the file with .pdf, whose pages
// are the rendition's and whose Chinese reads; the file cut short is
// failed, conversion_failed, with no PDF; the text has none; Ken, who reads
// neither Yuki's conversation nor its files, is shown nothing of the deck's.
// The binary's log names the renditions by their ids, never a file's name
// or a URL, and it stops when it is told to.
func renditions(t *testing.T, w *world) {
	bin := buildBinary(t, moduleRoot(t))
	stop := w.runRenditions(t, bin)

	docxType := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	doc, files := w.uploadFiles(t, w.sato, "Week 5", "",
		attachedFile{"第五週 handout.docx", docxType, renditionHandout},
		attachedFile{"broken.docx", docxType, renditionBroken},
		attachedFile{"reading.txt", "text/plain", []byte("Chapter 5.\n")})
	w.hostedBefore(t, w.tutor)
	conv, _ := w.askWithFiles(t, w.yuki, w.tutor.member, "Do my slides on hashing read well?",
		attachedFile{"第五週 slides.pptx", "application/octet-stream", renditionDeck})
	msgs := result[struct {
		Messages []struct {
			Attachments []struct {
				ID string `json:"id"`
			} `json:"attachments"`
		} `json:"messages"`
	}](t, w.api, w.yuki.token, "GET", w.path("/conversations/"+conv+"/messages"), nil)
	if len(msgs.Messages) != 1 || len(msgs.Messages[0].Attachments) != 1 {
		t.Fatalf("Yuki's question carries %+v", msgs.Messages)
	}
	deck := msgs.Messages[0].Attachments[0].ID

	fileOf := func(id string) *core.RenditionView {
		t.Helper()
		return result[renditionView](t, w.api, w.yuki.token, "GET", w.path("/documents/"+doc+"/files/"+id), nil).Rendition
	}
	deckOf := func() *core.RenditionView {
		t.Helper()
		return result[renditionView](t, w.api, w.yuki.token, "GET", w.path("/conversation-attachments/"+deck), nil).Rendition
	}
	finished := func(r *core.RenditionView) bool {
		return r != nil && r.State != core.RenditionQueued && r.State != core.RenditionClaimed
	}
	if r := fileOf(files[2]); r != nil {
		t.Errorf("a text has a rendition: %+v", r)
	}
	var handout, broken, slides *core.RenditionView
	eventually(t, renditionWait, "the renditions finished", func() bool {
		handout, broken, slides = fileOf(files[0]), fileOf(files[1]), deckOf()
		return finished(handout) && finished(broken) && finished(slides)
	})

	if broken.State != core.RenditionFailed || broken.Reason != core.RenditionConversionFailed || broken.DownloadURL != "" || broken.PageCount != 0 {
		t.Errorf("the Word file cut short: %+v", broken)
	}
	for _, f := range []struct {
		what, name string
		r          *core.RenditionView
		pages      int
		says       []string
	}{
		{"the handout", "%E7%AC%AC%E4%BA%94%E9%80%B1%20handout.pdf", handout, 1, []string{"第五週：雜湊表", "Open addressing"}},
		{"Yuki's deck", "%E7%AC%AC%E4%BA%94%E9%80%B1%20slides.pdf", slides, 2, []string{"Hashing", "線性探測"}},
	} {
		if f.r.State != core.RenditionDone || f.r.PageCount != f.pages || f.r.ByteSize < 100 || f.r.DownloadURL == "" {
			t.Errorf("%s's rendition: %+v", f.what, f.r)
			continue
		}
		w.addSecret(f.what+"'s PDF's URL", f.r.DownloadURL)
		pdf, header := getPDF(t, f.r.DownloadURL)
		if int64(len(pdf)) != f.r.ByteSize || !bytes.HasPrefix(pdf, []byte("%PDF-")) || header.Get("Content-Type") != "application/pdf" ||
			header.Get("Content-Disposition") != "inline; filename*=utf-8''"+f.name {
			t.Errorf("%s's PDF: %d bytes, %v", f.what, len(pdf), header)
			continue
		}
		res, err := doctext.Extract(t.Context(), pdf, doctext.PDF, doctext.Limits{})
		if err != nil || res.Parts != f.pages {
			t.Errorf("%s's PDF does not read: %v %+v", f.what, err, res)
			continue
		}
		for _, want := range f.says {
			if !strings.Contains(strings.Join(strings.Fields(res.Text), ""), strings.Join(strings.Fields(want), "")) {
				t.Errorf("%s's PDF does not say %q:\n%s", f.what, want, res.Text)
			}
		}
	}
	w.api.call(t, http.StatusNotFound, w.ken.token, "GET", w.path("/conversation-attachments/"+deck), nil)

	log := stop()
	for _, never := range []string{"handout", "slides.pptx", "broken.docx", "第五週", "/v1/blobs/", w.api.svc} {
		if strings.Contains(log, never) {
			t.Errorf("the binary's log holds %q", never)
		}
	}
	for _, want := range []string{`"renditions":"on, 2 at once`, `"msg":"a file's PDF rendition"`, `"status":"done"`, `"reason":"conversion_failed"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the binary's log does not say %s:\n%s", want, tail(redact.String(log), 60))
		}
	}
}

// runRenditions runs the binary's run with nothing configured but Core,
// its own credential in Core (CORE_SERVICE_CREDENTIAL, read from the
// environment), no agent (CONFIG an empty directory) and two files
// converted at once; RENDITIONS is left unset, as a server's is. stop sends it SIGTERM, fails t unless it ends 0 within
// its grace, and returns its log, which the world keeps.
func (w *world) runRenditions(t *testing.T, bin string) (stop func() string) {
	t.Helper()
	cmd := exec.Command(bin, "run")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"HTTP_ADDR=127.0.0.1:0",
		"CONFIG=" + t.TempDir(),
		"CORE_BASE_URL=" + w.api.base,
		"CORE_SERVICE_CREDENTIAL=env://E2E_SERVICE_CREDENTIAL",
		"E2E_SERVICE_CREDENTIAL=" + w.api.svc,
		"RENDITIONS_CONCURRENCY=2",
		"SHUTDOWN_GRACE=5s",
	}
	out := &logBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	w.addLog("the binary's run (renditions)", out.String)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	stop = func() string {
		t.Helper()
		if stopped {
			return out.String()
		}
		stopped = true
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("aishie-runtime run ended: %v\n%s", err, tail(redact.String(out.String()), 60))
			}
		case <-time.After(time.Minute):
			_ = cmd.Process.Kill()
			<-done
			t.Errorf("aishie-runtime run did not stop on SIGTERM:\n%s", tail(redact.String(out.String()), 60))
		}
		return out.String()
	}
	t.Cleanup(func() { stop() })
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(out.String(), "aishie-runtime started") {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("aishie-runtime run ended as it started: %v\n%s", err, tail(redact.String(out.String()), 60))
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("aishie-runtime run did not start:\n%s", tail(redact.String(out.String()), 60))
		}
	}
	return stop
}

// getPDF GETs a rendition's URL as a browser's PDF viewer does: with no
// Authorization header.
func getPDF(t *testing.T, url string) ([]byte, http.Header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET the PDF: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the PDF: HTTP %d, %v", resp.StatusCode, err)
	}
	if n, _ := strconv.Atoi(resp.Header.Get("Content-Length")); n != len(b) {
		t.Errorf("the PDF's Content-Length %d, of %d bytes", n, len(b))
	}
	return b, resp.Header
}

// The lecture whose PDF Core keeps, and what Yuki asks of it.
const (
	renderedTitle    = "Week 7 lecture"
	renderedQuestion = "What do the slides of " + renderedTitle + " show, as the site shows them?"
)

// renderedSlides is the lecture's deck, its notes on the first slide, and
// renderedPDF the PDF Core keeps of it, each page saying it is Core's, so
// that it is never taken for one the runtime made.
var (
	renderedSlides = doctexttest.PPTX(doctexttest.Slide{Title: "Hashing", Body: []doctexttest.Bullet{{Text: "Buckets"}}, Notes: "Ask about collisions."},
		doctexttest.Slide{Title: "探測", Body: []doctexttest.Bullet{{Text: "線性探測"}}})
	renderedPDF = doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"Core's PDF of slide 1"}},
		doctexttest.PDFPage{Lines: []string{"Core's PDF of slide 2"}})
)

// renderedResponder is a model that takes files and reads the lecture to
// say what its slides show: it lists the course's documents, reads the
// lecture's, and answers with what it was given of it: the file's record,
// the text of the PDF it had as a file, and the text beside it.
func renderedResponder(req fakellm.ChatRequest) fakellm.ChatResponse {
	asked := false
	for _, m := range req.Messages {
		asked = asked || m.Role == "user" && m.Text() == renderedQuestion
	}
	if !asked {
		return fakellm.DefaultResponder(req)
	}
	var results []string
	var pdfText string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			results = append(results, m.Text())
		}
		parts, _ := m.Content.([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			file, _ := part["file"].(map[string]any)
			data, _ := file["file_data"].(string)
			if raw, ok := strings.CutPrefix(data, "data:application/pdf;base64,"); ok {
				if pdf, err := base64.StdEncoding.DecodeString(raw); err == nil {
					if res, err := doctext.Extract(context.Background(), pdf, doctext.PDF, doctext.Limits{}); err == nil {
						pdfText = strings.Join(strings.Fields(res.Text), " ")
					}
				}
			}
		}
	}
	switch len(results) {
	case 0:
		return fakellm.CallTools(fakellm.FunctionCall{Name: "document_list", Arguments: `{}`})
	case 1:
		var env struct {
			Result struct {
				Documents []struct {
					ID    string `json:"id"`
					Title string `json:"title"`
				} `json:"documents"`
			} `json:"result"`
		}
		_ = json.Unmarshal([]byte(results[0]), &env)
		for _, d := range env.Result.Documents {
			if d.Title == renderedTitle {
				return fakellm.CallTools(fakellm.FunctionCall{Name: "document_get", Arguments: fmt.Sprintf(`{"document_id":%q}`, d.ID)})
			}
		}
		return fakellm.Reply("There is no " + renderedTitle + ".")
	}
	var got struct {
		File struct {
			GivenAs     string `json:"given_as"`
			ConvertedTo string `json:"converted_to"`
		} `json:"file"`
		FileText string `json:"file_text"`
	}
	_ = json.Unmarshal([]byte(results[len(results)-1]), &got)
	return fakellm.Reply(fmt.Sprintf("Given as a %s of its %s: %s; beside it: %s", got.File.GivenAs, got.File.ConvertedTo, pdfText, got.FileText))
}

// slidesAsCoresPDF: Sato puts up his lecture's deck, and its PDF
// rendition is made in Core, here by the test as the site's runtime makes
// one (claimed with the runtime's own credential, its PDF put where Core
// says, and completed done). Yuki's own agent, whose model takes files,
// runs where LibreOffice converts nothing, and reads the lecture for her:
// its model is given Core's PDF of the deck, page for page, with the
// deck's speaker notes beside it. The PDF's URL reaches neither the model
// nor the runtime's log. Against a Core without renditions it is skipped.
func slidesAsCoresPDF(t *testing.T, w *world) {
	cat, err := core.FetchCatalogue(t.Context(), w.api.hc, w.api.base)
	if err != nil {
		t.Fatal(err)
	}
	if !core.HasRenditions(cat) {
		t.Skip("the Core under test makes no PDF renditions (agent_runtime.rendition_*): it is older than AIShie-Core's migration 0026")
	}
	_, files := w.uploadFiles(t, w.sato, renderedTitle, "", attachedFile{"week7.pptx", doctexttest.PPTXType, renderedSlides})
	w.renderAsTheRuntime(t, files[0], renderedPDF, 2)

	ctx, cancel := context.WithCancel(context.Background())
	off := office.NewService(ctx, office.ServiceOptions{Off: "LibreOffice is not installed"})
	t.Cleanup(func() { cancel(); off.Wait() })
	m := newModel(t, renderedResponder)
	takesFiles := map[string]any{"model": map[string]any{"capabilities": map[string]any{"file_input": true}}}
	rt := w.startRuntime(t, m, runtimeConf{office: off, agents: []agentConf{{id: "yuki-helper", seat: w.own, over: takesFiles}}})
	rt.waitPolling("yuki-helper")
	asked, msg := w.ask(t, w.yuki, w.own.member, renderedQuestion)
	answer := w.waitAnswer(t, w.yuki, asked, w.own.member)
	want := "Given as a file of its pdf: ## Page 1 Core's PDF of slide 1 ## Page 2 Core's PDF of slide 2; beside it: ## Slide 1\nNotes: Ask about collisions."
	if answer.text() != want || answer.replyTo() != msg {
		t.Errorf("the answer is %q in reply to %s; want %q in reply to %s", answer.text(), answer.replyTo(), want, msg)
	}
	for _, req := range m.Requests() {
		if strings.Contains(string(req.Raw), "/v1/blobs/") {
			t.Error("a download URL reached the model")
		}
	}
	if strings.Contains(rt.raw.String(), "/v1/blobs/") {
		t.Error("the runtime's log holds a download URL")
	}
}

// renderAsTheRuntime makes the PDF rendition of the version's file fileID
// pdf, of pages, as the site's runtime makes one, with its credential:
// claimed (any other file claimed on the way is left to its lease, as a
// claim that lapses is), its PDF put at the URL Core gives for it, and
// completed done.
func (w *world) renderAsTheRuntime(t *testing.T, fileID string, pdf []byte, pages int) {
	t.Helper()
	svc := w.runtimeService()
	deadline := time.Now().Add(renditionWait)
	for {
		claimed, err := svc.ClaimRenditions(t.Context(), core.MaxRenditionClaims, time.Minute, 5*time.Second)
		if err != nil {
			t.Fatalf("claiming renditions: %v", err)
		}
		for _, c := range claimed {
			if c.FileID != fileID {
				continue
			}
			w.addSecret("the URL of the claimed file", c.DownloadURL)
			cl := core.ClaimOfRendition(c)
			up, err := svc.RenditionUploadURL(t.Context(), cl)
			if err != nil {
				t.Fatalf("the rendition's upload URL: %v", err)
			}
			w.addSecret("the rendition's upload URL", up.UploadURL)
			w.addSecret("the rendition's upload token", up.UploadToken)
			ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, up.UploadURL, bytes.NewReader(pdf))
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			for k, v := range up.Headers {
				req.Header.Set(k, v)
			}
			resp, err := w.api.hc.Do(req)
			cancel()
			if err != nil {
				t.Fatalf("the PUT of the PDF: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				t.Fatalf("the PUT of the PDF: HTTP %d", resp.StatusCode)
			}
			done, err := svc.CompleteRendition(t.Context(), cl, core.RenditionKey(cl, 1),
				core.RenditionCompletion{Status: core.RenditionDone, UploadToken: up.UploadToken, PageCount: pages})
			if err != nil || done.State != core.RenditionDone {
				t.Fatalf("completing the rendition: %+v %v", done, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rendition of file %s was not claimed within %s", fileID, renditionWait)
		}
	}
}
