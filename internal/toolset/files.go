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

	"github.com/AIShiteru-LMS/AIShie-Agent-Runtime/internal/llm"
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
	// Note says why it was not given.
	Note string `json:"note,omitempty"`
}

// docFile is a document_get result's file, as Core described it.
type docFile struct {
	url, title, contentType string
	byteSize                int64
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
	// kindFile is given as a file part, to a model that takes files: the
	// types every adapter's API takes as documents or images.
	kindFile
)

func classify(mediaType string) fileKind {
	switch mediaType {
	case "application/json", "application/markdown", "application/x-markdown":
		return kindText
	case "application/pdf", "image/png", "image/jpeg", "image/gif", "image/webp":
		return kindFile
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

// giveFile fetches a document's file and says how the model gets it: text
// for a text file, a file part for a PDF or an image when the model takes
// files, and otherwise not at all, with a note saying why. A file whose
// type Core did not record is fetched to find out.
func (r Runner) giveFile(ctx context.Context, d *docFile) (*fileRecord, string, *llm.File) {
	rec := &fileRecord{Name: d.title, ContentType: d.contentType, ByteSize: d.byteSize, GivenAs: givenNot}
	mt := mediaType(d.contentType)
	if mt != "" {
		if why := r.refusal(mt); why != "" {
			rec.Note = why
			return rec, "", nil
		}
	}
	if r.Files == nil {
		rec.Note = "the file could not be given to the model: files are not fetched here"
		return rec, "", nil
	}
	if d.byteSize > r.MaxFileBytes {
		rec.Note = r.tooLarge()
		return rec, "", nil
	}
	f, err := r.Files.Fetch(ctx, d.url, r.MaxFileBytes)
	switch {
	case errors.Is(err, ErrTooLarge):
		rec.Note = r.tooLarge()
		return rec, "", nil
	case err != nil:
		rec.Note = "the file could not be given to the model: it could not be fetched"
		return rec, "", nil
	}
	rec.ByteSize = int64(len(f.Data))
	if mt == "" {
		mt = mediaType(f.ContentType)
		if mt == "" || mt == "application/octet-stream" {
			mt = mediaType(http.DetectContentType(f.Data))
		}
		rec.ContentType = mt
		if why := r.refusal(mt); why != "" {
			rec.Note = why
			return rec, "", nil
		}
	}
	if classify(mt) == kindText {
		text := strings.ToValidUTF8(string(f.Data), "�")
		if text == "" {
			rec.Note = "the file is empty"
			return rec, "", nil
		}
		rec.GivenAs = givenText
		return rec, text, nil
	}
	rec.GivenAs = givenFile
	return rec, "", &llm.File{Name: fileName(d.title, mt), MIME: mt, Data: f.Data}
}

// refusal says why a file of media type mt cannot be given to this model,
// or "" when it can.
func (r Runner) refusal(mt string) string {
	switch classify(mt) {
	case kindOther:
		return "the file could not be given to the model: " + mt + " files are not read here"
	case kindFile:
		if !r.FileInput {
			return "the file could not be given to the model: this model does not take files"
		}
	}
	return ""
}

func (r Runner) tooLarge() string {
	size := fmt.Sprintf("%d bytes", r.MaxFileBytes)
	if r.MaxFileBytes%(1<<20) == 0 {
		size = fmt.Sprintf("%d MiB", r.MaxFileBytes>>20)
	}
	return "the file could not be given to the model: it is larger than the " + size + " the runtime reads"
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
