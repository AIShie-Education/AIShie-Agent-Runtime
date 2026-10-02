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

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
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
	// FileID and Position are the file's id and place among its
	// version's files, where Core gives them (AIShie-Core #49): what
	// FileIDArg names it by.
	FileID      string `json:"file_id,omitempty"`
	Position    int    `json:"position,omitempty"`
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
	// ConvertedTo is what the file was converted to (pdf, pptx or xlsx),
	// by LibreOffice here or, for its PDF, Core's rendition of it, when
	// what is given is of that: the PDF as a file part, or the runtime's
	// text of what was made.
	ConvertedTo string `json:"converted_to,omitempty"`
	// Conversion says where the file's conversion stands, when what it is
	// converted to is not given (yet): ConversionInProgress or
	// ConversionBusy, and AskAgain is the call that asks for it again;
	// ConversionFailed.
	Conversion string `json:"conversion,omitempty"`
	// OCR says where the runtime's OCR of a file with no text of its own
	// stands, when it gives none of it (yet): OCRInProgress or OCRBusy,
	// and AskAgain is the call that asks for it again; OCRFailed or
	// OCRUnavailable.
	OCR      string    `json:"ocr,omitempty"`
	AskAgain *nextPart `json:"ask_again,omitempty"`
	// TextSource says whose file_text is where it is the version's text
	// version, Core's (textversion.go): "AI transcription (<model>)", or
	// "edited by staff".
	TextSource string `json:"text_source,omitempty"`
	// Part and Parts: a text too long for one result is given in parts,
	// and file_text is part Part of Parts; so is a PDF of more pages than
	// one file part holds, and the file part is its pages of part Part.
	// PartHolds says which slides, pages or sheets it holds, and NextPart
	// is the call that reads the next. All empty when the file is given
	// whole. PartHolds alone says which pages are given of those the model
	// asked for (FilePagesArg).
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
	// title is what the file is called: its name, where Core gives one,
	// and else the document's title.
	url, title, contentType string
	byteSize                int64
	// fileID and position are the file's id and place among its
	// version's files, where Core lists them (version.files, AIShie-Core
	// #49), "" and 0 where it gives the version's one file alone; several
	// is that the version holds more than one, so that a call that reads
	// more of this one names it (FileIDArg).
	fileID   string
	position int
	several  bool
	// documentID, versionID and checksum name the document and the
	// version Core gave, as its result does: what a part of its text is
	// asked for by, and kept under.
	documentID, versionID, checksum string
	// text is the version's text version as Core showed it beside the
	// version, nil where it has none; courseID the course the call is
	// in, which its parts are read in.
	text     *core.TextView
	courseID string
	// rendition is the file's PDF rendition as Core showed it, nil where
	// it has none (rendition.go): done, its URL is a credential for the
	// PDF, as url is for the file. renditionMissed says Core's PDF was
	// tried in this call and not had, and is not tried again in it.
	rendition       *core.RenditionView
	renditionMissed bool
	// first and last are the pages of the file the model asked for
	// (FilePagesArg), 0 for none.
	first, last int
	// attachmentID and messageID name, in place of a document's version,
	// a file a message of the conversation carries (attachments.go): its
	// parts are asked for of AttachmentTool. noTool is that the model has
	// no tool to ask with (tools mode none): no call is named.
	attachmentID, messageID string
	noTool                  bool
}

// tool is the tool the model reads more of the file with.
func (d *docFile) tool() string {
	if d.attachmentID != "" {
		return AttachmentTool
	}
	return FilePartTool
}

// partArg is the argument of d.tool that names a part of the file.
func (d *docFile) partArg() string {
	if d.attachmentID != "" {
		return AttachmentPartArg
	}
	return FilePartArg
}

// pagesArg is the argument of d.tool that names pages of the file.
func (d *docFile) pagesArg() string {
	if d.attachmentID != "" {
		return AttachmentPagesArg
	}
	return FilePagesArg
}

// again is the call that reads part (0 for none: the file as first asked
// for) of the file again, of the version Core gave, or the file of the
// message: the model makes it as it is. nil where the model has no tool to.
func (d *docFile) again(part int) *nextPart {
	if d.noTool {
		return nil
	}
	if d.attachmentID != "" {
		args := map[string]any{"attachment_id": d.attachmentID}
		if part > 0 {
			args[AttachmentPartArg] = part
		}
		return &nextPart{Tool: AttachmentTool, Arguments: args}
	}
	args := map[string]any{"document_id": d.documentID}
	if part > 0 {
		args[FilePartArg] = part
	}
	if d.versionID != "" {
		args["version_id"] = d.versionID
	}
	if d.several && d.fileID != "" {
		args[FileIDArg] = d.fileID
	}
	return &nextPart{Tool: FilePartTool, Arguments: args}
}

// readNext says how the model reads part k of the file: the call next_part
// names, or, with no tool to, that it cannot here.
func (d *docFile) readNext(k int) string {
	if d.noTool {
		return fmt.Sprintf("; part %d cannot be read here", k)
	}
	what := "this version"
	switch {
	case d.attachmentID != "":
		what = "this file"
	case d.several && d.fileID != "":
		what = "this file of this version"
	}
	return fmt.Sprintf("; to read part %d, call %s with next_part's arguments, which name %s", k, d.tool(), what)
}

// docVersion is a document_get result's version, as Core described it:
// the document, the version, and its files, in order.
type docVersion struct {
	documentID, versionID, title string
	files                        []*docFile
}

// documentVersion finds the files of a document_get result: its version's
// files (version.files, each with its id, place, name, type, size,
// checksum, download_url, text version and rendition) where Core lists them
// (AIShie-Core #49); where it does not (a Core before it), the version's
// one file, its download_url, with the document's title and the version's
// content type, size, checksum and text version. nil when Core gave no
// version.
func documentVersion(result any) *docVersion {
	m, _ := result.(map[string]any)
	version, _ := m["version"].(map[string]any)
	if version == nil {
		return nil
	}
	v := &docVersion{}
	v.title, _ = m["title"].(string)
	v.documentID, _ = m["id"].(string)
	v.versionID, _ = version["id"].(string)
	if files, listed := version["files"].([]any); listed {
		for i, raw := range files {
			f, _ := raw.(map[string]any)
			u, _ := f["download_url"].(string)
			if f == nil || u == "" {
				continue
			}
			d := v.file(u, f)
			d.fileID, _ = f["id"].(string)
			d.title, _ = f["filename"].(string)
			if d.title == "" {
				d.title = v.title
			}
			d.position = i + 1
			if n, ok := f["position"].(json.Number); ok {
				if p, err := n.Int64(); err == nil && p > 0 {
					d.position = int(p)
				}
			}
			v.files = append(v.files, d)
		}
		for _, d := range v.files {
			d.several = len(v.files) > 1
		}
		return v
	}
	if u, _ := version["download_url"].(string); u != "" {
		d := v.file(u, version)
		d.title = v.title
		v.files = []*docFile{d}
	}
	return v
}

// file is the file of v at url that f describes, as a version or one of
// its files describes it: its type, size, checksum, text version and
// rendition.
func (v *docVersion) file(url string, f map[string]any) *docFile {
	d := &docFile{url: url, documentID: v.documentID, versionID: v.versionID}
	d.checksum, _ = f["checksum"].(string)
	d.contentType, _ = f["content_type"].(string)
	if n, ok := f["byte_size"].(json.Number); ok {
		d.byteSize, _ = n.Int64()
	}
	if t, ok := f["text"].(map[string]any); ok {
		var tv core.TextView
		if json.Unmarshal([]byte(encodeJSON(t)), &tv) == nil && tv.Status != "" {
			d.text = &tv
		}
	}
	if rn, ok := f["rendition"].(map[string]any); ok {
		var rv core.RenditionView
		if json.Unmarshal([]byte(encodeJSON(rn)), &rv) == nil && rv.State != "" {
			d.rendition = &rv
		}
	}
	return d
}

// byID is v's file of id, nil for none.
func (v *docVersion) byID(id string) *docFile {
	for _, d := range v.files {
		if d.fileID != "" && strings.EqualFold(d.fileID, id) {
			return d
		}
	}
	return nil
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
	// kindConvert is an Office file the runtime has LibreOffice convert
	// (Runner.Office): a presentation or a document, whose PDF (Core's,
	// where Core made one) is what a model that takes files sees, and a
	// workbook the runtime reads only as LibreOffice converts it (.xls,
	// .ods); and, where LibreOffice does not convert here, a presentation
	// or a document whose PDF Core made, to a model that takes files
	// (Runner.rendered).
	kindConvert
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
	if strings.HasPrefix(mediaType, "text/") || strings.HasSuffix(mediaType, "+json") || textApplications[mediaType] ||
		strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+xml") {
		return kindText
	}
	return kindOther
}

// textApplications are the types of files that are text, as a program's
// source, a configuration or data in a text format is, which a browser or
// a server calls application/…: given as text, as text/… is, and so is an
// application/…+xml. (Code of no telling type, application/octet-stream,
// is known by its bytes: sniff. A picture drawn in XML, image/svg+xml, is
// not text to read.)
var textApplications = map[string]bool{
	"application/xml": true, "application/javascript": true, "application/x-javascript": true, "application/ecmascript": true,
	"application/typescript": true, "application/x-typescript": true, "application/x-sh": true, "application/x-shellscript": true,
	"application/x-python": true, "application/x-python-code": true, "application/x-yaml": true, "application/yaml": true,
	"application/toml": true, "application/x-toml": true, "application/sql": true, "application/x-sql": true,
	"application/x-tex": true, "application/x-latex": true, "application/x-httpd-php": true, "application/x-php": true,
	"application/x-perl": true, "application/x-ruby": true, "application/x-ndjson": true, "application/graphql": true,
	"application/x-subrip": true, "application/csv": true,
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
	noteNoImages  = notGiven + "this model cannot see images"
	noteNoText    = "the PDF has no text to read: it looks scanned, or like pictures of text"
	noteUnmapped  = "the PDF's text cannot be read: its fonts do not map to text"
	askSelectable = "; ask for a version with selectable text"
	noteMalformed = notGiven + "it could not be read: it is damaged, or not the kind of file it says it is"
	// noteNotFetched and noteTooSlow say why a file was not given this
	// time, which another time may: the search reads it again (search.go).
	noteNotFetched = notGiven + "it could not be fetched"
	noteTooSlow    = notGiven + "reading it took longer than the runtime allows"
)

// given is what the model is given of a document's file: the record the
// result carries, and the text or the file part, if any.
type given struct {
	rec *fileRecord
	// text is the file's text, whole, and sections where its slides,
	// pages or sheets begin: the result gives it whole or a part of it.
	text     string
	sections []doctext.Section
	// aside is text given beside a file part (a deck's speaker notes,
	// which its PDF does not show): whole, cut short should it not fit,
	// never in parts.
	aside bool
	file  *llm.File
	// filePages is how many pages file shows: a PDF's (one when they are
	// not known), an image's one.
	filePages int
	// pages is the file's pages given as the model asked (FilePagesArg),
	// or why none are.
	pages bool
	// unit is what the pages the model asked for are, page or slide (as
	// doctext names them), where they are given as it asked: as a PDF of
	// their own, or the text of them alone (pagesOfText); "" otherwise.
	unit string
	// coreEnds are where each part of the file's text version ends in
	// text, as document_text gave them, where the runtime read it in
	// Core's parts (readTextParts); and base where text begins in that
	// text version, once it is the text of the pages asked for alone. The
	// answer's sources name Core's part by them (corePart).
	coreEnds []int
	base     int
}

// giveFile fetches a document's file and says how the model gets it (rule
// 6): text as text; an image as a file part, to a model that takes files;
// a presentation or a document as its PDF, Core's or LibreOffice's, where
// the runtime converts them or Core made its PDF (giveConverted), and
// otherwise a PowerPoint, Word or Excel file as the text the runtime reads
// from it; a PDF as a file part to a model that takes files and whose
// provider takes one of its size, in parts of its pages when it has more
// than a part holds, and otherwise as its text, when that reads as text. A
// file of no type, or of one that says nothing, is known by what it holds.
// Anything else is not given, with a note saying why. part is the part the
// model asked for, 0 for none. What was read for a model given the text is
// kept (Runner.Texts), and a later call for the same version, a later part
// of it, reads it there without fetching the file again.
func (r Runner) giveFile(ctx context.Context, d *docFile, part int) given {
	rec := &fileRecord{FileID: d.fileID, Position: d.position, Name: d.title, ContentType: d.contentType, ByteSize: d.byteSize, GivenAs: givenNot}
	g := given{rec: rec}
	// The version's text version first, where it is done, unless the
	// model asked for pages of the file, which only a model that takes
	// files is given.
	if d.first == 0 || !r.FileInput {
		d.first, d.last = 0, 0
		if gt, ok := r.giveTextVersion(ctx, g, d); ok {
			return gt
		}
	}
	mt := mediaType(d.contentType)
	kind := r.kindFor(d, mt)
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
	if kept := r.kept(key); kept != nil && !r.givesFile(d, kept.mt) {
		if kind == kindUnknown {
			rec.ContentType = kept.mt
		}
		return r.giveReading(ctx, g, d, kept, "", nil)
	}
	f, err := r.fetch(ctx, d)
	switch {
	case errors.Is(err, ErrTooLarge):
		rec.Note = r.tooLarge()
		return g
	case err != nil:
		rec.Note = noteNotFetched
		return g
	}
	rec.ByteSize = int64(len(f.Data))
	if kind == kindUnknown {
		var why string
		mt, kind, why = r.sniff(d, f)
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
		g.file, g.filePages = &llm.File{Name: fileName(d.title, mt), MIME: mt, Data: f.Data}, 1
		return g
	case kind == kindConvert:
		return r.giveConverted(ctx, g, d, mt, f.Data, part)
	case kind == kindPDF && r.FileInput:
		// A PDF is a file part where the model takes files and its
		// provider takes a PDF of its size and pages, in parts of its
		// pages where it has more than a part holds; one that needs a
		// password to open is given to no model, as no provider reads it
		// either.
		pctx, cancel := context.WithTimeout(ctx, extractTimeout)
		pages, err := doctext.PDFPages(pctx, f.Data, r.DocLimits)
		cancel()
		if errors.Is(err, doctext.ErrEncrypted) {
			rec.Note = notePassword
			return g
		}
		// Pages asked for are cut from it, whatever its size.
		whole := r.bytesPast(int64(len(f.Data)), "")
		if whole == "" || d.first > 0 {
			var ok bool
			if g, ok = r.givePDFFile(ctx, g, d, pdfFile{data: f.Data, sum: checksum(f.Data), pages: pages, unit: doctext.SectionPage}, part); ok {
				return g
			}
		}
		if past = whole; past == "" {
			if past = r.pagesPast(pages, ""); past == "" {
				past = "its pages could not be cut into parts"
			}
		}
	}
	rd := r.kept(key)
	if rd == nil {
		var keep bool
		rd, keep = r.read(ctx, mt, kind, f.Data)
		if keep {
			r.keep(key, rd)
		}
	}
	return r.giveReading(ctx, g, d, rd, past, f.Data)
}

// fetch fetches d's file from the URL Core gave for it; where that has
// lapsed (the file server refuses it), a file of a version Core names by
// its id is fetched once more from a fresh URL, which document_file gives
// with the caller's own token, as it gives the version.
func (r Runner) fetch(ctx context.Context, d *docFile) (*FetchedFile, error) {
	f, err := r.Files.Fetch(ctx, d.url, r.MaxFileBytes)
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Status != http.StatusForbidden && fe.Status != http.StatusNotFound && fe.Status != http.StatusGone ||
		d.fileID == "" || d.attachmentID != "" || r.Client == nil || d.courseID == "" || d.documentID == "" {
		return f, err
	}
	fresh, ferr := r.Client.DocumentFile(ctx, d.courseID, d.documentID, d.fileID)
	if ferr != nil || fresh.DownloadURL == "" || fresh.VersionID != "" && d.versionID != "" && fresh.VersionID != d.versionID {
		return f, err
	}
	d.url = fresh.DownloadURL
	return r.Files.Fetch(ctx, d.url, r.MaxFileBytes)
}

// noPages says why the pages of a file the model asked for
// (FilePagesArg) were not given.
func (r Runner) noPages(rec *fileRecord) string {
	switch {
	case !r.FileInput:
		return "this model does not take files"
	case rec.GivenAs == givenFile:
		return "the file is given whole"
	case rec.GivenAs == givenText:
		return "the file's pages cannot be given as a PDF here, so its text is given"
	}
	return "the file's pages cannot be given here"
}

// givesFile reports whether d, a file of media type mt, may be given to
// this model as a file part, which takes its bytes, not its text: an image
// or a PDF, or a presentation or document the runtime converts to one, or
// whose PDF Core made, to a model that takes files.
func (r Runner) givesFile(d *docFile, mt string) bool {
	switch r.kindFor(d, mt) {
	case kindImage, kindPDF:
		return r.FileInput
	case kindConvert:
		f, _ := office.FormatOf(mt)
		return r.FileInput && f.Family != office.Workbook
	}
	return false
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
	if rd.fam != "" {
		return r.giveConvertedReading(ctx, g, d, rd, past, data)
	}
	kind := classify(rd.mt)
	switch {
	case kind == kindImage:
		return r.giveOCR(ctx, g, rd, ocrFile{kind: ocr.Image, why: noteNoImages, sum: rd.sum, data: r.refetch(d, rd, data), again: d.again(0)})
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
		return r.giveOCR(ctx, g, rd, ocrFile{kind: ocr.PDF, pages: res.Of, why: why, ask: askSelectable, sum: rd.sum, data: r.refetch(d, rd, data), again: d.again(0)})
	}
	g = r.extracted(g, res)
	if past != "" {
		rec.Note = "the PDF is given as the runtime's text of it, since " + past + "; " + rec.Note
	}
	return g
}

// sniff is what d, a fetched file f of no telling type, is: by its first
// bytes and the package it holds (PDF, Office Open XML, an older Office
// file, and, where the runtime converts them or Core made its PDF, which
// older Office file, an OpenDocument file or RTF); then by what the file
// server said; then by Go's sniffing. why is set when that alone says it is
// not given.
func (r Runner) sniff(d *docFile, f *FetchedFile) (mt string, kind fileKind, why string) {
	format, err := doctext.Sniff(f.Data)
	switch {
	case errors.Is(err, doctext.ErrEncrypted):
		return "application/x-ole-storage", kindOldOffice, notePassword
	case errors.Is(err, doctext.ErrOldFormat):
		if mt := office.Sniff(f.Data); mt != "" && r.kindFor(d, mt) == kindConvert {
			return mt, kindConvert, ""
		}
		return "application/x-ole-storage", kindOldOffice, noteOldOffice
	case format != "":
		mt = format.MediaType()
		return mt, r.kindFor(d, mt), ""
	}
	if mt := office.Sniff(f.Data); mt != "" && r.kindFor(d, mt) == kindConvert {
		return mt, kindConvert, ""
	}
	if mt = mediaType(f.ContentType); r.kindOf(mt) != kindUnknown {
		return mt, r.kindOf(mt), ""
	}
	mt = mediaType(http.DetectContentType(f.Data))
	if r.kindOf(mt) == kindUnknown {
		return mt, kindOther, ""
	}
	return mt, r.kindOf(mt), ""
}

// kindOf is what the runner makes of a file of media type mt: its kind
// (classify), but for an Office file the runtime converts (Runner.Office),
// which is kindConvert: a presentation or a document, and a workbook of a
// format but Excel's own; and an older Office file of no telling type,
// which is fetched to know which it is.
func (r Runner) kindOf(mt string) fileKind {
	if !r.converts() {
		return classify(mt)
	}
	if f, ok := office.FormatOf(mt); ok && (f.Family != office.Workbook || !f.OOXML) {
		return kindConvert
	}
	if doctext.OldOffice(mt) {
		return kindUnknown
	}
	return classify(mt)
}

// converts reports whether this runner has LibreOffice convert Office
// files.
func (r Runner) converts() bool {
	if r.Office == nil {
		return false
	}
	ok, _ := r.Office.Available()
	return ok
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
			return noteNoImages + noOCR(why)
		}
	}
	return ""
}

// bytesPast says why a PDF of n bytes is past what the model's provider
// takes as a file (r.PDFLimits), or "" when it is not; of names it (it,
// or its PDF).
func (r Runner) bytesPast(n int64, of string) string {
	if of == "" {
		of = "it is"
	}
	if lim := r.PDFLimits.PDFBytes; lim > 0 && n > lim {
		return fmt.Sprintf("%s %s, more than the %s this model takes as a file", of, sizeOf(n), sizeOf(lim))
	}
	return ""
}

// pagesPast says why a PDF of pages pages, given whole, is past what the
// model's provider takes in a file, or "" when it is not: a PDF whose
// pages cannot be counted (0) is taken to be within it, as the provider
// may yet read it.
func (r Runner) pagesPast(pages int, of string) string {
	if of == "" {
		of = "it has"
	}
	if lim := r.PDFLimits.PDFPages; lim > 0 && pages > lim {
		return fmt.Sprintf("%s %d pages, more than the %d this model takes in a file", of, pages, lim)
	}
	return ""
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
		return noteTooSlow
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
