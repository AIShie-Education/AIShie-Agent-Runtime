package e2e

import (
	"bytes"
	"context"
	"errors"
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
