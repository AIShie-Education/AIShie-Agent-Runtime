package toolset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// corePDF is the PDF Core made of a file: n pages, each saying it is
// Core's, so that it is never taken for LibreOffice's here (pdfOf).
func corePDF(n int) []byte {
	var pages []doctexttest.PDFPage
	for i := 1; i <= n; i++ {
		pages = append(pages, doctexttest.PDFPage{Lines: []string{fmt.Sprintf("Core's page %d", i)}})
	}
	return doctexttest.PDF(pages...)
}

// renditionWorld is a file whose PDF Core made, as a test's Core and file
// server give it: document_get lists it as the one file of the version, and
// conversation_attachment gives it as a message's, each with its
// rendition, a URL of its own once it is done; document_file gives it
// again, with a fresh URL. The file server serves the file, and the PDF at
// the URL of the latest token alone: a URL handed out stale is refused, as
// one whose time is up is.
type renditionWorld struct {
	srv  *httptest.Server
	file []byte
	ct   string
	pdf  []byte

	mu sync.Mutex
	// state is the rendition's, "" for none; expires what Core says of
	// its URL, the zero time for nothing; pdfStatus what the file server
	// answers for the PDF, 0 for the PDF; stale how many of the URLs
	// handed out next have lapsed already, as one read a while ago has.
	state     string
	expires   time.Time
	pdfStatus int
	stale     int
	// pdfSize is the PDF's size as Core says it, 0 for its own.
	pdfSize int
	// later are the fields Core's answers after the first give otherwise,
	// as for another file: another version's, other bytes.
	later map[string]any
	token int
	hits  map[string]int
	asked []string
	// urls are every URL handed out, which nothing given the model, nor
	// any log, may hold.
	urls []string
}

func newRenditionWorld(t *testing.T, file []byte, ct string, pdf []byte) *renditionWorld {
	w := &renditionWorld{file: file, ct: ct, pdf: pdf, state: core.RenditionDone, hits: map[string]int{}}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.hits[r.URL.Path]++
		switch {
		case r.URL.RawQuery != signature:
			http.Error(rw, "unsigned", http.StatusForbidden)
		case r.URL.Path == "/file":
			_, _ = rw.Write(w.file)
		case r.URL.Path != fmt.Sprintf("/pdf/%d", w.token):
			http.Error(rw, "the URL has expired", http.StatusForbidden)
		case w.pdfStatus != 0:
			http.Error(rw, "no", w.pdfStatus)
		default:
			rw.Header().Set("Content-Type", "application/pdf")
			_, _ = rw.Write(w.pdf)
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *renditionWorld) set(f func(w *renditionWorld)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w)
}

// fetched is how many times the file server was asked for the file and
// for any URL of the PDF.
func (w *renditionWorld) fetched() (file, pdf int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for path, n := range w.hits {
		if strings.HasPrefix(path, "/pdf/") {
			pdf += n
		}
	}
	return w.hits["/file"], pdf
}

func (w *renditionWorld) tools() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.asked...)
}

// rendition is the rendition as Core shows it now, with a new URL; nil for
// none. Called with the lock held.
func (w *renditionWorld) rendition() map[string]any {
	if w.state == "" {
		return nil
	}
	v := map[string]any{"state": w.state}
	if w.state == core.RenditionDone {
		token := w.token
		if w.stale > 0 {
			w.stale, token = w.stale-1, token-1
		}
		u := fmt.Sprintf("%s/pdf/%d?%s", w.srv.URL, token, signature)
		w.urls = append(w.urls, u)
		size := len(w.pdf)
		if w.pdfSize != 0 {
			size = w.pdfSize
		}
		v["page_count"], v["byte_size"], v["download_url"] = 3, size, u
		if !w.expires.IsZero() {
			v["download_expires_at"] = w.expires
		}
	}
	return v
}

// Call answers document_get, document_file and conversation_attachment.
func (w *renditionWorld) Call(_ context.Context, tool string, _ json.RawMessage) (*core.Envelope, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, tool)
	f := map[string]any{"id": fileSlides, "position": 1, "filename": "Week 3.pptx", "content_type": w.ct, "byte_size": len(w.file),
		"checksum": checksum(w.file), "download_url": w.srv.URL + "/file?" + signature}
	if v := w.rendition(); v != nil {
		f["rendition"] = v
	}
	var res any
	switch tool {
	case "document_get":
		res = map[string]any{"id": docID, "kind": "material", "title": "Week 3", "status": "active",
			"version": map[string]any{"id": version1, "seq": 1, "published": true, "files": []any{f}}}
	case core.ToolDocumentFile:
		f["document_id"], f["version_id"], f["seq"], f["published"], f["expires_at"] = docID, version1, 1, true, time.Now().Add(15*time.Minute)
		res = f
	case core.ToolAttachment:
		f["id"], f["conversation_id"], f["message_id"], f["message_seq"], f["created_at"] = fileHandout, thisConv, question, 1, "2026-09-30T10:00:00Z"
		res = f
	default:
		return &core.Envelope{Status: core.StatusError, Error: &core.Error{Code: core.CodeInvalidArgument, Message: "not this"}}, nil
	}
	if len(w.asked) > 1 {
		for k, v := range w.later {
			f[k] = v
		}
	}
	b, _ := json.Marshal(res)
	return executed(string(b)), nil
}

// runner is a runner of the world's Core and file server, with o.
func (w *renditionWorld) runner(o Office, fileInput bool) Runner {
	return Runner{Client: core.NewClient(w), Files: NewHTTPFetcher(w.srv.Client()), Texts: NewTextCache(0), Office: o,
		FileInput: fileInput, Conversation: thisConv}
}

// holdsNoURL fails t if s holds a URL of the PDF handed out, the
// signature of any, or the PDF's path on the file server.
func (w *renditionWorld) holdsNoURL(t *testing.T, what, s string) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, u := range append(w.urls, signature, "/pdf/", w.srv.URL) {
		if strings.Contains(s, u) {
			t.Errorf("%s holds %q:\n%s", what, u, s)
		}
	}
}

// TestRenditionGivenAsItsPDF: a deck whose PDF Core made is given to a
// model that takes files as that PDF, its speaker notes beside it as
// file_text, with no conversion of the runtime's own: LibreOffice is never
// asked, the deck being read for its notes as it is. The PDF is fetched
// once, and kept by the deck's checksum: a later call, and a part of it
// asked for, fetches nothing. Neither its URL nor its path reaches the
// model.
func TestRenditionGivenAsItsPDF(t *testing.T) {
	deck := lectureDeck(3)
	w := newRenditionWorld(t, deck, doctexttest.PPTXType, corePDF(3))
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
	r := w.runner(o, true)
	parts, err := delegateSet(t).Run(context.Background(), r, courseID, []llm.Part{call("d", "document_get", firstPart)})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[1].File == nil || !bytes.Equal(parts[1].File.Data, w.pdf) || parts[1].File.Name != "Week 3.pptx.pdf" {
		t.Fatalf("the model was given %+v", parts)
	}
	w.holdsNoURL(t, "the result", parts[0].Content)
	c := contentOf(t, parts[0])
	rec := c["file"].(map[string]any)
	if rec["given_as"] != givenFile || rec["converted_to"] != "pdf" || c["file_text"] != "## Slide 1\nNotes: Say 1.\n\n## Slide 3\nNotes: Say 3." {
		t.Errorf("record %v, file_text %q", rec, c["file_text"])
	}
	wantNote(t, rec, "its 3 slides as they look")
	if converted, _, _ := o.record(); len(converted) != 0 {
		t.Errorf("LibreOffice was asked %v", converted)
	}

	_, file := getDoc(t, r, firstPart)
	if file == nil || !bytes.Equal(file.Data, w.pdf) {
		t.Errorf("asked again: %+v", file)
	}
	if _, pdfs := w.fetched(); pdfs != 1 || len(o.fetches()) != 1 {
		t.Errorf("Core's PDF fetched %d times", pdfs)
	}
	if converted, _, _ := o.record(); len(converted) != 0 {
		t.Errorf("LibreOffice was asked %v", converted)
	}
}

// TestRenditionNotThere: where Core has no PDF of a deck to give (none at
// all, a Core from before renditions; one queued, being made, failed or
// skipped), the deck is converted by LibreOffice here, as before, and Core's
// URL of a PDF is never asked for.
func TestRenditionNotThere(t *testing.T) {
	for _, state := range []string{"", core.RenditionQueued, core.RenditionClaimed, core.RenditionFailed, core.RenditionSkipped} {
		t.Run(state, func(t *testing.T) {
			w := newRenditionWorld(t, lectureDeck(3), doctexttest.PPTXType, corePDF(3))
			w.set(func(w *renditionWorld) { w.state = state })
			o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
			_, file := getDoc(t, w.runner(o, true), firstPart)
			if file == nil || !bytes.Equal(file.Data, pdfOf(3).Data) {
				t.Errorf("the model was given %+v", file)
			}
			if converted, _, _ := o.record(); fmt.Sprint(converted) != "[pptx>pdf]" {
				t.Errorf("LibreOffice was asked %v", converted)
			}
			if _, pdfs := w.fetched(); pdfs != 0 || len(o.fetches()) != 0 {
				t.Errorf("a PDF was fetched %d times", pdfs)
			}
		})
	}
}

// TestRenditionURLLapsed: a URL of Core's PDF that the file server
// refuses, as it does once its time is up, is asked of Core again
// (document_file), and the PDF fetched from the fresh one; one Core says
// has run out (download_expires_at) is not tried at all. Where the fresh
// URL is refused too, or the PDF cannot be fetched, the deck is converted
// here, and what was said of the fetch holds no URL.
func TestRenditionURLLapsed(t *testing.T) {
	deck := lectureDeck(3)
	w := newRenditionWorld(t, deck, doctexttest.PPTXType, corePDF(3))
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
	r := w.runner(o, true)
	// A model reading the document_get it made before: the URL Core gave
	// then has lapsed since.
	w.set(func(w *renditionWorld) { w.stale = 1 })
	_, file := getDoc(t, r, firstPart)
	if file == nil || !bytes.Equal(file.Data, w.pdf) {
		t.Fatalf("the model was given %+v", file)
	}
	if got := fmt.Sprint(w.tools()); got != "[document_get document_file]" {
		t.Errorf("Core was asked %s", got)
	}
	if _, pdfs := w.fetched(); pdfs != 2 {
		t.Errorf("the PDF's URLs fetched %d times", pdfs)
	}

	w = newRenditionWorld(t, deck, doctexttest.PPTXType, corePDF(3))
	w.set(func(w *renditionWorld) { w.expires = time.Now().Add(10 * time.Second) })
	o = &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
	_, file = getDoc(t, w.runner(o, true), firstPart)
	if file == nil || !bytes.Equal(file.Data, w.pdf) {
		t.Fatalf("the model was given %+v", file)
	}
	if got := fmt.Sprint(w.tools()); got != "[document_get document_file]" {
		t.Errorf("Core was asked %s", got)
	}
	if _, pdfs := w.fetched(); pdfs != 1 {
		t.Errorf("the PDF's URLs fetched %d times: one past its time was tried", pdfs)
	}

	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		w = newRenditionWorld(t, deck, doctexttest.PPTXType, corePDF(3))
		w.set(func(w *renditionWorld) { w.pdfStatus = status })
		o = &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
		parts, err := delegateSet(t).Run(context.Background(), w.runner(o, true), courseID, []llm.Part{call("d", "document_get", firstPart)})
		if err != nil {
			t.Fatal(err)
		}
		if len(parts) != 2 || !bytes.Equal(parts[1].File.Data, pdfOf(3).Data) {
			t.Errorf("HTTP %d: the model was given %+v", status, parts)
		}
		if converted, _, _ := o.record(); fmt.Sprint(converted) != "[pptx>pdf]" {
			t.Errorf("HTTP %d: LibreOffice was asked %v", status, converted)
		}
		want := map[int]string{http.StatusForbidden: "[document_get document_file]", http.StatusInternalServerError: "[document_get]"}[status]
		if got := fmt.Sprint(w.tools()); got != want {
			t.Errorf("HTTP %d: Core was asked %s", status, got)
		}
		w.holdsNoURL(t, "the result", parts[0].Content)
		for _, err := range o.fetches() {
			if err == nil {
				t.Errorf("HTTP %d: fetched", status)
				continue
			}
			w.holdsNoURL(t, "the fetch's error", err.Error())
		}
	}
}

// TestRenditionURLOfAnotherFile: a fresh URL Core gives on being asked
// again is taken for the file alone: one of another version, or of other
// bytes, or of another message's file, is never fetched, and the file is
// converted here.
func TestRenditionURLOfAnotherFile(t *testing.T) {
	for _, later := range []map[string]any{
		{"version_id": "0190a1b2-0000-7000-8000-0000000000ff"},
		{"checksum": checksum([]byte("another file"))},
	} {
		w := newRenditionWorld(t, lectureDeck(3), doctexttest.PPTXType, corePDF(3))
		w.set(func(w *renditionWorld) { w.stale, w.later = 1, later })
		o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
		_, file := getDoc(t, w.runner(o, true), firstPart)
		if file == nil || !bytes.Equal(file.Data, pdfOf(3).Data) {
			t.Errorf("%v: the model was given %+v", later, file)
		}
		if got := fmt.Sprint(w.tools()); got != "[document_get document_file]" {
			t.Errorf("%v: Core was asked %s", later, got)
		}
		if _, pdfs := w.fetched(); pdfs != 1 {
			t.Errorf("%v: the PDF's URLs fetched %d times", later, pdfs)
		}
		if converted, _, _ := o.record(); fmt.Sprint(converted) != "[pptx>pdf]" {
			t.Errorf("%v: LibreOffice was asked %v", later, converted)
		}
	}

	for _, later := range []map[string]any{{"id": fileSlides}, {"checksum": checksum([]byte("another file"))}} {
		w := newRenditionWorld(t, lectureDeck(3), doctexttest.PPTXType, corePDF(3))
		w.set(func(w *renditionWorld) { w.stale, w.later = 1, later })
		o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
		_, _, file := readAttachment(t, w.runner(o, true), idArgs(fileHandout))
		if file == nil || !bytes.Equal(file.Data, pdfOf(3).Data) {
			t.Errorf("an attachment, %v: the model was given %+v", later, file)
		}
		if got := fmt.Sprint(w.tools()); got != "[conversation_attachment conversation_attachment]" {
			t.Errorf("an attachment, %v: Core was asked %s", later, got)
		}
		if _, pdfs := w.fetched(); pdfs != 1 {
			t.Errorf("an attachment, %v: the PDF's URLs fetched %d times", later, pdfs)
		}
	}
}

// TestRenditionTriedOnce: Core's PDF that could not be had is not fetched
// again in the same call, where the runtime then reads the file's text
// from its PDF (an OpenDocument text, LibreOffice's PDF of it being made),
// or picks the slides OCR reads from it (a deck, with no LibreOffice).
func TestRenditionTriedOnce(t *testing.T) {
	odt := "application/vnd.oasis.opendocument.text"
	for status, want := range map[int]int{http.StatusForbidden: 2, http.StatusInternalServerError: 1} {
		w := newRenditionWorld(t, []byte("PK\x03\x04odt"), odt, corePDF(3))
		w.set(func(w *renditionWorld) { w.pdfStatus = status })
		o := &stubOffice{convert: func(office.Format, office.Target) office.State { return office.State{Status: office.StatusPending} }}
		c, file := getDoc(t, w.runner(o, true), firstPart)
		if file != nil || c["file"].(map[string]any)["conversion"] != ConversionInProgress {
			t.Errorf("HTTP %d: %v", status, c["file"])
		}
		if _, pdfs := w.fetched(); pdfs != want || len(o.fetches()) != 1 {
			t.Errorf("HTTP %d: the PDF's URLs fetched %d times, taken %d times", status, pdfs, len(o.fetches()))
		}
		if n := strings.Count(fmt.Sprint(w.tools()), core.ToolDocumentFile); n != want-1 {
			t.Errorf("HTTP %d: Core asked again %d times", status, n)
		}
	}

	w := newRenditionWorld(t, lectureDeck(3), doctexttest.PPTXType, corePDF(3))
	w.set(func(w *renditionWorld) { w.pdfStatus = http.StatusInternalServerError })
	r := w.runner(&stubOffice{off: "LibreOffice is not installed"}, true)
	r.OCR = &fakeOCR{respond: func(_ int, data func(context.Context) ([]byte, error)) ocr.State {
		_, _ = data(context.Background())
		return ocr.State{Status: ocr.StatusBusy, Why: "no"}
	}}
	if c, file := getDoc(t, r, firstPart); file != nil || c["file"].(map[string]any)["given_as"] != givenText {
		t.Errorf("a deck, with no LibreOffice: %v", c["file"])
	}
	if _, pdfs := w.fetched(); pdfs != 1 {
		t.Errorf("a deck, with no LibreOffice: the PDF's URL fetched %d times", pdfs)
	}
}

// TestRenditionPartsApart: the parts of a file's PDF are kept by which
// PDF they are cut from, Core's or LibreOffice's, as the two may break
// their pages apart differently: a deck given in parts of LibreOffice's
// PDF, then of Core's once its rendition is done, is cut from each under
// a name of its own.
func TestRenditionPartsApart(t *testing.T) {
	deck := lectureDeck(25)
	w := newRenditionWorld(t, deck, doctexttest.PPTXType, corePDF(25))
	w.set(func(w *renditionWorld) { w.state = core.RenditionQueued })
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(25)}}
	r := w.runner(o, true)
	if c, file := getDoc(t, r, firstPart); file == nil || c["file"].(map[string]any)["parts"] != float64(3) {
		t.Fatalf("LibreOffice's PDF: %v", c["file"])
	}
	w.set(func(w *renditionWorld) { w.state = core.RenditionDone })
	if c, file := getDoc(t, r, firstPart); file == nil || c["file"].(map[string]any)["parts"] != float64(3) {
		t.Fatalf("Core's PDF: %v", c["file"])
	}
	if _, pdfs := w.fetched(); pdfs != 1 {
		t.Errorf("Core's PDF fetched %d times", pdfs)
	}
	sum := checksum(deck)
	if got := fmt.Sprint(o.rangeNames()); got != fmt.Sprint([]string{sum + "/pdf", sum + "/rendition"}) {
		t.Errorf("parts cut under %s", got)
	}
}

// TestRenditionPastTheFetch: Core's PDF larger than the runtime fetches of
// a file is not fetched, nor read past that where Core said it was
// smaller: the deck is converted here.
func TestRenditionPastTheFetch(t *testing.T) {
	deck := lectureDeck(3)
	for _, said := range []int{0, 100} {
		w := newRenditionWorld(t, deck, doctexttest.PPTXType, append(corePDF(3), bytes.Repeat([]byte("\n"), len(deck))...))
		w.set(func(w *renditionWorld) { w.pdfSize = said })
		o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
		r := w.runner(o, true)
		r.MaxFileBytes = int64(len(deck))
		_, file := getDoc(t, r, firstPart)
		if file == nil || !bytes.Equal(file.Data, pdfOf(3).Data) {
			t.Errorf("Core said %d bytes: the model was given %+v", said, file)
		}
		if _, pdfs := w.fetched(); pdfs != min(said, 1) {
			t.Errorf("Core said %d bytes: the PDF was fetched %d times", said, pdfs)
		}
		if errs := o.fetches(); said != 0 && (len(errs) != 1 || !errors.Is(errs[0], ErrTooLarge)) {
			t.Errorf("Core said %d bytes: the fetches said %v", said, errs)
		}
	}
}

// TestRenditionOfAnAttachment: a deck a message carries, whose PDF Core
// made, is given as that PDF, as AttachmentTool reads it and with the
// question; a lapsed URL is asked of conversation_attachment again.
func TestRenditionOfAnAttachment(t *testing.T) {
	w := newRenditionWorld(t, lectureDeck(3), doctexttest.PPTXType, corePDF(3))
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
	r := w.runner(o, true)
	res, content, file := readAttachment(t, r, idArgs(fileHandout))
	if file == nil || !bytes.Equal(file.Data, w.pdf) || content["file"].(map[string]any)["converted_to"] != "pdf" {
		t.Fatalf("the model was given %+v, %s", file, res.Content)
	}
	w.holdsNoURL(t, "the result", res.Content)

	w = newRenditionWorld(t, lectureDeck(3), doctexttest.PPTXType, corePDF(3))
	o = &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
	r = w.runner(o, true)
	w.set(func(w *renditionWorld) { w.stale = 1 })
	given := r.GiveAttachments(context.Background(), courseID, []MessageFile{{Attachment: core.Attachment{ID: fileHandout,
		Filename: "Week 3.pptx", ContentType: doctexttest.PPTXType}, MessageID: question, MessageSeq: 1}}, false)
	parts := given[question]
	if len(parts) != 2 || parts[1].File == nil || !bytes.Equal(parts[1].File.Data, w.pdf) {
		t.Fatalf("the question was given %+v", parts)
	}
	w.holdsNoURL(t, "the question's block", parts[0].Text)
	if got := fmt.Sprint(w.tools()); got != "[conversation_attachment conversation_attachment]" {
		t.Errorf("Core was asked %s", got)
	}
	if converted, _, _ := o.record(); len(converted) != 0 {
		t.Errorf("LibreOffice was asked %v", converted)
	}
}

// TestRenditionReadByOCR: to a model that takes no files, a deck is its
// text, and the slides that show pictures are picked for OCR out of Core's
// PDF of it, LibreOffice never asked; a Word 97 document, which the
// runtime reads only in its PDF, is read in Core's.
func TestRenditionReadByOCR(t *testing.T) {
	w := newRenditionWorld(t, lectureDeck(3), doctexttest.PPTXType, corePDF(3))
	o := &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
	r := w.runner(o, false)
	var picked []byte
	r.OCR = &fakeOCR{respond: func(_ int, data func(context.Context) ([]byte, error)) ocr.State {
		picked, _ = data(context.Background())
		return ocrOfSlides()
	}}
	c, file := getDoc(t, r, firstPart)
	if file != nil || !strings.Contains(c["file_text"].(string), ocrMark) || string(picked) != "%PDF-picked" {
		t.Errorf("record %v, OCR given %q", c["file"], picked)
	}
	if converted, _, picks := o.record(); len(converted) != 0 || fmt.Sprint(picks) != "[[1 3]]" {
		t.Errorf("LibreOffice was asked %v, slides picked %v", converted, picks)
	}
	if _, pdfs := w.fetched(); pdfs != 1 {
		t.Errorf("Core's PDF fetched %d times", pdfs)
	}

	w = newRenditionWorld(t, cfbOf("WordDocument"), "application/msword", corePDF(2))
	o = &stubOffice{out: map[office.Target]*office.Output{office.ToPDF: pdfOf(3)}}
	c, _ = getDoc(t, w.runner(o, false), firstPart)
	if c["file_text"] != "## Page 1\nCore's page 1\n\n## Page 2\nCore's page 2" {
		t.Errorf("a Word 97 document: %v %q", c["file"], c["file_text"])
	}
	if converted, _, _ := o.record(); len(converted) != 0 {
		t.Errorf("LibreOffice was asked %v", converted)
	}
}

// TestRenditionWithoutLibreOffice: where LibreOffice does not convert
// here, a deck or an OpenDocument presentation whose PDF Core made is
// given to a model that takes files as that PDF all the same, the deck
// with its notes; to a model that takes none, and with no PDF of Core's,
// each is what it was.
func TestRenditionWithoutLibreOffice(t *testing.T) {
	o := &stubOffice{off: "LibreOffice is not installed"}
	w := newRenditionWorld(t, lectureDeck(3), doctexttest.PPTXType, corePDF(3))
	c, file := getDoc(t, w.runner(o, true), firstPart)
	if file == nil || !bytes.Equal(file.Data, w.pdf) || c["file_text"] != "## Slide 1\nNotes: Say 1.\n\n## Slide 3\nNotes: Say 3." {
		t.Errorf("a deck: %v %+v", c["file"], file)
	}

	odp := "application/vnd.oasis.opendocument.presentation"
	w = newRenditionWorld(t, []byte("PK\x03\x04odp"), odp, corePDF(3))
	c, file = getDoc(t, w.runner(o, true), firstPart)
	if file == nil || !bytes.Equal(file.Data, w.pdf) {
		t.Errorf("an OpenDocument deck: %v", c["file"])
	}
	wantNote(t, c["file"].(map[string]any), "its speaker notes are not given: the runtime cannot convert it to PowerPoint's format here")

	w = newRenditionWorld(t, lectureDeck(3), doctexttest.PPTXType, corePDF(3))
	c, file = getDoc(t, w.runner(o, false), firstPart)
	if rec := c["file"].(map[string]any); file != nil || rec["given_as"] != givenText || rec["extracted_from"] != "pptx" || rec["converted_to"] != nil {
		t.Errorf("a deck, to a model that takes no files: %v", rec)
	}
	w = newRenditionWorld(t, []byte("PK\x03\x04odp"), odp, corePDF(3))
	w.set(func(w *renditionWorld) { w.state = core.RenditionQueued })
	c, _ = getDoc(t, w.runner(o, true), firstPart)
	wantNote(t, c["file"].(map[string]any), odp+" files are not read here")
	if converted, _, _ := o.record(); len(converted) != 0 {
		t.Errorf("converted %v", converted)
	}
}

// TestRenditionNotLogged: the worker's conversions log a PDF of Core's
// taken, or not taken, by the start of the file's checksum and what became
// of it, never by its URL, whatever the fetch said; a PDF of Core's is
// kept as LibreOffice's is, which a conversion of the same file finds.
func TestRenditionNotLogged(t *testing.T) {
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conv := &countingConverter{out: pdfOf(3)}
	svc := office.NewService(ctx, office.ServiceOptions{Converter: conv, Log: slog.New(slog.NewJSONHandler(&logs, nil))})
	defer svc.Wait()
	for i, status := range []int{0, http.StatusForbidden, http.StatusInternalServerError} {
		w := newRenditionWorld(t, lectureDeck(3+i), doctexttest.PPTXType, corePDF(3))
		w.set(func(w *renditionWorld) { w.pdfStatus = status })
		parts, err := delegateSet(t).Run(context.Background(), w.runner(svc, true), courseID, []llm.Part{call("d", "document_get", firstPart)})
		if err != nil || len(parts) != 2 {
			t.Fatalf("HTTP %d: %v %+v", status, err, parts)
		}
		w.holdsNoURL(t, "the result", parts[0].Content)
		w.holdsNoURL(t, "the log", logs.String())
		want := pdfOf(3).Data
		if status == 0 {
			want = w.pdf
		}
		if !bytes.Equal(parts[1].File.Data, want) {
			t.Errorf("HTTP %d: the model was given another PDF", status)
		}
		if status == 0 {
			if st := svc.Convert(ctx, checksum(w.file), office.Format{Ext: "pptx", Family: office.Slides, OOXML: true}, office.ToPDF, w.file); st.Status != office.StatusDone ||
				!bytes.Equal(st.Out.Data, w.pdf) || conv.times() != 0 {
				t.Errorf("a conversion of the deck after: %+v, LibreOffice asked %d times", st, conv.times())
			}
		}
	}
	for _, want := range []string{`"outcome":"taken"`, `"outcome":"not_fetched"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not say %s:\n%s", want, logs.String())
		}
	}
}

// countingConverter is LibreOffice as a test has it: out, counted.
type countingConverter struct {
	mu    sync.Mutex
	calls int
	out   *office.Output
}

func (c *countingConverter) Convert(context.Context, []byte, office.Format, office.Target) (*office.Output, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.out, nil
}

func (c *countingConverter) times() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *countingConverter) Describe() string { return "LibreOffice 0.0 stub" }
