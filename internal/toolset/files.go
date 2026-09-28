package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/ocr"
)

// FileFetcher fetches a document's file from the short-lived URL Core gave
// for it (document_get's version.download_url, §2.3), so that the runtime,
// not the model, reads it (rule 6).
type FileFetcher interface {
	// Fetch reads the file at url, refusing one of more than maxBytes with
	// ErrTooLarge. Its errors never hold the URL: it is a credential.
	Fetch(ctx context.Context, url string, maxBytes int64) (*FetchedFile, error)
}

// FetchedFile is a file as fetched.
type FetchedFile struct {
	// ContentType is what the server said the file is; "" if nothing.
	ContentType string
	Data        []byte
}

// ErrTooLarge is a file larger than the runtime reads.
var ErrTooLarge = errors.New("toolset: the file is larger than the runtime reads")

// FetchError is a file server's refusal: its HTTP status, and nothing of the
// URL.
type FetchError struct {
	Status int
}

func (e *FetchError) Error() string {
	return fmt.Sprintf("toolset: the file server answered HTTP %d", e.Status)
}

// HTTPFetcher fetches files over HTTP.
type HTTPFetcher struct {
	client *http.Client
}

// NewHTTPFetcher fetches with client, which carries the egress proxy
// (§8.3: the host of download_url is Core's own, or its object store);
// nil means http.DefaultClient. Timeouts come from the call's context.
func NewHTTPFetcher(client *http.Client) *HTTPFetcher {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPFetcher{client: client}
}

// Fetch GETs url, which must be http or https, and reads at most maxBytes:
// a Content-Length above it is refused before the body is read, and a body
// that runs past it is cut off and refused.
func (f *HTTPFetcher) Fetch(ctx context.Context, rawURL string, maxBytes int64) (*FetchedFile, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("toolset: the file's URL is not an http or https URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("toolset: the file's URL does not make a request")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, withoutURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &FetchError{Status: resp.StatusCode}
	}
	if resp.ContentLength > maxBytes {
		return nil, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, withoutURL(err)
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrTooLarge
	}
	return &FetchedFile{ContentType: resp.Header.Get("Content-Type"), Data: data}, nil
}

// withoutURL drops the URL net/http puts in its errors: a presigned URL
// carries its signature in the query string.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("toolset: fetching the file: %s: %w", ue.Op, ue.Err)
	}
	return fmt.Errorf("toolset: fetching the file: %w", err)
}

// How a file was given to the model, as the result records it.
const (
	givenFile = "file"
	givenText = "text"
	givenNot  = "not_given"
)

// fileRecord is what became of a document's file, in the result the model
// reads: it knows what the document holds even when it cannot read it.
type fileRecord struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	ByteSize    int64  `json:"byte_size"`
	// GivenAs is file (a file part after the results), text (file_text in
	// the result) or not_given.
	GivenAs string `json:"given_as"`
	// ExtractedFrom is the format the runtime read the text from (pptx,
	// docx, xlsx or pdf) when the text is not the file's own but the
	// runtime's reading of it; ExtractedOCR when its OCR recognized it.
	ExtractedFrom string `json:"extracted_from,omitempty"`
	// OCR says where the runtime's OCR of a file with no text of its own
	// stands, when it gives none of it (yet): OCRInProgress or OCRBusy,
	// and AskAgain is the call that asks for it again; OCRFailed or
	// OCRUnavailable.
	OCR      string    `json:"ocr,omitempty"`
	AskAgain *nextPart `json:"ask_again,omitempty"`
	// Part and Parts: a text too long for one result is given in parts,
	// and file_text is part Part of Parts; PartHolds says which slides,
	// pages or sheets it holds, and NextPart is the call that reads the
	// next. All empty when the text is given whole.
	Part      int       `json:"part,omitempty"`
	Parts     int       `json:"parts,omitempty"`
	PartHolds string    `json:"part_holds,omitempty"`
	NextPart  *nextPart `json:"next_part,omitempty"`
	// Note says why the file was not given; or, of text the runtime
	// extracted, what it holds and leaves out, and which part it is.
	Note string `json:"note,omitempty"`
}

// docFile is a document_get result's file, as Core described it.
type docFile struct {
	url, title, contentType string
	byteSize                int64
	// documentID, versionID and checksum name the document and the
	// version Core gave, as its result does: what a part of its text is
	// asked for by, and kept under.
	documentID, versionID, checksum string
}

// documentFile finds the file of a document_get result: its version's
// download_url, with the document's title and the version's content type
// and size. nil when there is none.
func documentFile(result any) *docFile {
	m, _ := result.(map[string]any)
	version, _ := m["version"].(map[string]any)
	u, _ := version["download_url"].(string)
	if u == "" {
		return nil
	}
	d := &docFile{url: u}
	d.title, _ = m["title"].(string)
	d.documentID, _ = m["id"].(string)
	d.versionID, _ = version["id"].(string)
	d.checksum, _ = version["checksum"].(string)
	d.contentType, _ = version["content_type"].(string)
	if n, ok := version["byte_size"].(json.Number); ok {
		d.byteSize, _ = n.Int64()
	}
	return d
}

// kinds of file, by what the model can be given.
type fileKind int

const (
	kindOther fileKind = iota
	// kindText is given as text, to any model.
	kindText
	// kindImage is given as a file part, to a model that takes files: the
	// types every adapter's API takes as images.
	kindImage
	// kindPDF is given as a file part to a model that takes files and
	// whose provider takes one of its size and pages, and otherwise as the
	// text the runtime reads from it.
	kindPDF
	// kindOffice is a PowerPoint, Word or Excel file (Office Open XML),
	// given as the text the runtime reads from it, to any model.
	kindOffice
	// kindOldOffice is an older binary Office file, which is not read.
	kindOldOffice
	// kindUnknown is a file of no type, or of one that says nothing (an
	// octet stream, a zip archive): fetched, and known by what it holds.
	kindUnknown
)

func classify(mediaType string) fileKind {
	switch mediaType {
	case "application/json", "application/markdown", "application/x-markdown":
		return kindText
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return kindImage
	case "application/pdf":
		return kindPDF
	case "", "application/octet-stream", "binary/octet-stream", "application/zip", "application/x-zip-compressed", "application/x-zip":
		return kindUnknown
	}
	if f, ok := doctext.FormatOf(mediaType); ok && f != doctext.PDF {
		return kindOffice
	}
	if doctext.OldOffice(mediaType) {
		return kindOldOffice
	}
	if strings.HasPrefix(mediaType, "text/") || strings.HasSuffix(mediaType, "+json") {
		return kindText
	}
	return kindOther
}

// mediaType is a content type's type/subtype, lower case, without
// parameters; "" when there is none that parses.
func mediaType(contentType string) string {
	if contentType == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return strings.ToLower(mt)
}

// extractTimeout bounds the reading of one file's text, within the
// answer's own time.
const extractTimeout = 20 * time.Second

// Notes of a file not given, for the model.
const (
	notGiven      = "the file could not be given to the model: "
	noteOldOffice = notGiven + "it is in an older Office format (.ppt, .doc or .xls) that the runtime cannot read; " +
		"ask for it as .pptx, .docx or .xlsx, or as a PDF"
	notePassword  = notGiven + "it is password-protected; ask for a copy without a password"
	noteNoFiles   = notGiven + "this model does not take files"
	noteNoText    = "the PDF has no text to read: it looks scanned, or like pictures of text"
	noteUnmapped  = "the PDF's text cannot be read: its fonts do not map to text"
	askSelectable = "; ask for a version with selectable text"
	noteMalformed = notGiven + "it could not be read: it is damaged, or not the kind of file it says it is"
)

// given is what the model is given of a document's file: the record the
// result carries, and the text or the file part, if any.
type given struct {
	rec *fileRecord
	// text is the file's text, whole, and sections where its slides,
	// pages or sheets begin: the result gives it whole or a part of it.
	text     string
	sections []doctext.Section
	file     *llm.File
}

// giveFile fetches a document's file and says how the model gets it (rule
// 6): text as text; an image as a file part, to a model that takes files;
// a PowerPoint, Word or Excel file as the text the runtime reads from it;
// a PDF as a file part to a model that takes files and whose provider
// takes one of its size and pages, and otherwise as its text, when that
// reads as text. A file of no type, or of one that says nothing, is known
// by what it holds. Anything else is not given, with a note saying why.
// What was read for a model given the text is kept (Runner.Texts), and a
// later call for the same version, a later part of it, reads it there
// without fetching the file again.
func (r Runner) giveFile(ctx context.Context, d *docFile) given {
	rec := &fileRecord{Name: d.title, ContentType: d.contentType, ByteSize: d.byteSize, GivenAs: givenNot}
	g := given{rec: rec}
	mt := mediaType(d.contentType)
	kind := classify(mt)
	if why := r.refusal(mt, kind); why != "" {
		rec.Note = why
		return g
	}
	if r.Files == nil {
		rec.Note = notGiven + "files are not fetched here"
		return g
	}
	if d.byteSize > r.MaxFileBytes {
		rec.Note = r.tooLarge()
		return g
	}
	key := r.textKey(d)
	if kept := r.Texts.get(key); kept != nil && !r.givesFile(classify(kept.mt)) {
		if kind == kindUnknown {
			rec.ContentType = kept.mt
		}
		return r.giveReading(ctx, g, d, kept, "", nil)
	}
	f, err := r.Files.Fetch(ctx, d.url, r.MaxFileBytes)
	switch {
	case errors.Is(err, ErrTooLarge):
		rec.Note = r.tooLarge()
		return g
	case err != nil:
		rec.Note = notGiven + "it could not be fetched"
		return g
	}
	rec.ByteSize = int64(len(f.Data))
	if kind == kindUnknown {
		var why string
		mt, kind, why = sniff(f)
		rec.ContentType = mt
		if why == "" {
			why = r.refusal(mt, kind)
		}
		if why != "" {
			rec.Note = why
			return g
		}
	}
	past := ""
	switch {
	case kind == kindImage && r.FileInput:
		rec.GivenAs = givenFile
		g.file = &llm.File{Name: fileName(d.title, mt), MIME: mt, Data: f.Data}
		return g
	case kind == kindPDF && r.FileInput:
		// A PDF is a file part where the model takes files and its
		// provider takes a PDF of its size and pages; one that needs a
		// password to open is given to no model, as no provider reads it
		// either.
		ctx, cancel := context.WithTimeout(ctx, extractTimeout)
		past, err = r.pastLimits(ctx, f.Data)
		cancel()
		if errors.Is(err, doctext.ErrEncrypted) {
			rec.Note = notePassword
			return g
		}
		if past == "" {
			rec.GivenAs = givenFile
			g.file = &llm.File{Name: fileName(d.title, "application/pdf"), MIME: "application/pdf", Data: f.Data}
			return g
		}
	}
	rd := r.Texts.get(key)
	if rd == nil {
		var keep bool
		rd, keep = r.read(ctx, mt, kind, f.Data)
		if keep {
			r.Texts.put(key, rd)
		}
	}
	return r.giveReading(ctx, g, d, rd, past, f.Data)
}

// givesFile reports whether a file of kind may be given to this model as
// a file part, which takes its bytes, not its text: an image or a PDF, to
// a model that takes files.
func (r Runner) givesFile(kind fileKind) bool {
	return (kind == kindImage || kind == kindPDF) && r.FileInput
}

// read reads the text of a file of media type mt, of kind (text, a PDF or
// an Office file), within extractTimeout; keep is false when the reading
// ran out of time or was cancelled, which another call may yet finish.
func (r Runner) read(ctx context.Context, mt string, kind fileKind, data []byte) (rd *fileReading, keep bool) {
	rd = &fileReading{mt: mt, size: int64(len(data)), sum: checksum(data)}
	var format doctext.Format
	switch kind {
	case kindText:
		rd.res = &doctext.Result{Text: strings.ToValidUTF8(string(data), "\uFFFD")}
		return rd, true
	case kindPDF:
		format = doctext.PDF
	case kindOffice:
		format, _ = doctext.FormatOf(mt)
	case kindImage:
		// An image has no text to read but OCR's (giveOCR): what is kept
		// is its checksum, which OCR's text is kept by.
		return rd, true
	default:
		rd.err = fmt.Errorf("%w: it is not a file the runtime reads the text of", doctext.ErrMalformed)
		return rd, true
	}
	ctx, cancel := context.WithTimeout(ctx, extractTimeout)
	defer cancel()
	rd.res, rd.err = doctext.Extract(ctx, data, format, r.DocLimits)
	return rd, !isContextError(rd.err)
}

// giveReading gives a file's text as rd read it; past, for a PDF, says why
// the model's provider does not take it as a file, when that is why it is
// given as text. data is the file's bytes when they were fetched for this
// call, which OCR is started with; nil when rd was kept, and OCR, should it
// need them, fetches them again.
func (r Runner) giveReading(ctx context.Context, g given, d *docFile, rd *fileReading, past string, data []byte) given {
	rec := g.rec
	rec.ByteSize = rd.size
	kind := classify(rd.mt)
	switch {
	case kind == kindImage:
		return r.giveOCR(ctx, g, d, rd, ocrFile{kind: ocr.Image, why: noteNoFiles}, data)
	case kind == kindText && rd.res != nil:
		if rd.res.Text == "" {
			rec.Note = "the file is empty"
			return g
		}
		rec.GivenAs = givenText
		g.text = rd.res.Text
		return g
	case kind == kindPDF:
		return r.givePDFText(ctx, g, d, rd, past, data)
	case rd.err != nil:
		rec.Note = extractNote(rd.err)
		return g
	}
	return r.extracted(g, rd.res)
}

// givePDFText gives a PDF's text, where that reads as text: to a model
// that takes no files, or past what its provider takes (past says why);
// where it does not, what OCR recognizes of it (giveOCR).
func (r Runner) givePDFText(ctx context.Context, g given, d *docFile, rd *fileReading, past string, data []byte) given {
	rec := g.rec
	res, err := rd.res, rd.err
	switch {
	case err != nil && past != "":
		rec.Note = notGiven + past + ", and " + strings.TrimPrefix(extractNote(err), notGiven)
		return g
	case err != nil:
		rec.Note = extractNote(err)
		return g
	case res.Unreadable != "":
		why := noteUnmapped
		if res.Unreadable == doctext.UnreadableNoText {
			why = noteNoText
		}
		if past != "" {
			why = notGiven + past + ", and " + why
		} else {
			why = noteNoFiles + ", and " + why
		}
		return r.giveOCR(ctx, g, d, rd, ocrFile{kind: ocr.PDF, pages: res.Of, why: why, ask: askSelectable}, data)
	}
	g = r.extracted(g, res)
	if past != "" {
		rec.Note = "the PDF is given as the runtime's text of it, since " + past + "; " + rec.Note
	}
	return g
}

// sniff is what a fetched file of no telling type is: by its first bytes
// and the package it holds (PDF, Office Open XML, an older Office file);
// then by what the file server said; then by Go's sniffing. why is set
// when that alone says it is not given.
func sniff(f *FetchedFile) (mt string, kind fileKind, why string) {
	format, err := doctext.Sniff(f.Data)
	switch {
	case errors.Is(err, doctext.ErrEncrypted):
		return "application/x-ole-storage", kindOldOffice, notePassword
	case errors.Is(err, doctext.ErrOldFormat):
		return "application/x-ole-storage", kindOldOffice, noteOldOffice
	case format != "":
		mt = format.MediaType()
		return mt, classify(mt), ""
	}
	if mt = mediaType(f.ContentType); classify(mt) != kindUnknown {
		return mt, classify(mt), ""
	}
	mt = mediaType(http.DetectContentType(f.Data))
	if classify(mt) == kindUnknown {
		return mt, kindOther, ""
	}
	return mt, classify(mt), ""
}

// refusal says why a file of media type mt, of kind, cannot be given to
// this model, or "" when it can, or might be once read: an image to a
// model that takes no files, only where OCR reads it.
func (r Runner) refusal(mt string, kind fileKind) string {
	switch kind {
	case kindOther:
		return notGiven + mt + " files are not read here"
	case kindOldOffice:
		return noteOldOffice
	case kindImage:
		if ok, why := r.ocrAvailable(); !r.FileInput && !ok {
			return noteNoFiles + noOCR(why)
		}
	}
	return ""
}

// pastLimits says why a PDF is past what the model's provider takes as a
// file (r.PDFLimits), or "" when it is not. Its pages are counted only
// when there is a limit on them; a PDF whose pages cannot be counted is
// taken to be within it, as the provider may yet read it.
func (r Runner) pastLimits(ctx context.Context, data []byte) (string, error) {
	lim := r.PDFLimits
	if lim.PDFBytes > 0 && int64(len(data)) > lim.PDFBytes {
		return fmt.Sprintf("it is %s, more than the %s this model takes as a file", sizeOf(int64(len(data))), sizeOf(lim.PDFBytes)), nil
	}
	if lim.PDFPages <= 0 {
		return "", nil
	}
	pages, err := doctext.PDFPages(ctx, data, r.DocLimits)
	switch {
	case errors.Is(err, doctext.ErrEncrypted):
		return "", err
	case err == nil && pages > lim.PDFPages:
		return fmt.Sprintf("it has %d pages, more than the %d this model takes in a file", pages, lim.PDFPages), nil
	}
	return "", nil
}

// extracted gives a file's text as doctext read it, and says what it
// holds and leaves out.
func (r Runner) extracted(g given, res *doctext.Result) given {
	rec := g.rec
	if strings.TrimSpace(res.Text) == "" {
		rec.Note = notGiven + "it holds no text"
		return g
	}
	rec.GivenAs, rec.ExtractedFrom = givenText, string(res.Format)
	var holds string
	switch res.Format {
	case doctext.PPTX:
		holds = "the runtime's text of its " + plural(res.Parts, "slide") + ", with their speaker notes"
	case doctext.DOCX:
		holds = "the runtime's text of the document, its headings, lists and tables in Markdown, then its notes, headers and footers"
	case doctext.XLSX:
		holds = "the runtime's text of its " + plural(res.Parts, "sheet") + ", each sheet's rows as CSV, values as stored"
	case doctext.PDF:
		holds = "the runtime's text of its " + plural(res.Parts, "page") + ", without their pictures or layout"
	}
	var named []string
	if res.Images > 0 {
		named = append(named, plural(res.Images, "image"))
	}
	if res.Charts > 0 {
		named = append(named, plural(res.Charts, "chart"))
	}
	if len(named) > 0 && res.Format != doctext.PDF {
		holds += "; its " + strings.Join(named, " and ") + " are only named, as [image] and [chart]"
	}
	rec.Note = strings.Join(append([]string{holds}, res.Notes...), "; ")
	g.text, g.sections = res.Text, res.Sections
	return g
}

// extractNote is why a file whose text could not be read is not given.
func extractNote(err error) string {
	switch {
	case errors.Is(err, doctext.ErrEncrypted):
		return notePassword
	case errors.Is(err, doctext.ErrOldFormat):
		return noteOldOffice
	case errors.Is(err, doctext.ErrLimit):
		return notGiven + "it is larger or more complex than the runtime reads (" +
			strings.TrimPrefix(err.Error(), doctext.ErrLimit.Error()+": ") + ")"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return notGiven + "reading it took longer than the runtime allows"
	}
	return noteMalformed
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (r Runner) tooLarge() string {
	return notGiven + "it is larger than the " + sizeOf(r.MaxFileBytes) + " the runtime reads"
}

// sizeOf is n bytes as a person says it: whole MiB when it is, bytes
// otherwise.
func sizeOf(n int64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	if n > 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

// fileName names a file part: the document's title, with the extension its
// type has if the title lacks it (some APIs read the type from the name).
func fileName(title, mt string) string {
	if title == "" {
		title = "document"
	}
	exts := map[string][]string{
		"application/pdf": {".pdf"}, "image/png": {".png"}, "image/jpeg": {".jpg", ".jpeg"},
		"image/gif": {".gif"}, "image/webp": {".webp"},
	}[mt]
	lower := strings.ToLower(title)
	for _, e := range exts {
		if strings.HasSuffix(lower, e) {
			return title
		}
	}
	if len(exts) > 0 {
		return title + exts[0]
	}
	return title
}
