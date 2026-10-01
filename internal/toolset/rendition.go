package toolset

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// Core keeps one PDF of every Office and OpenDocument file, its rendition
// (design §13), made once for the whole site, and the one people see in
// the viewer. Where document_get, document_file or conversation_attachment
// say a presentation's or a document's rendition is done, the PDF the
// runtime gives a model of it, and reads its text and OCR from, is that one
// (pdfOf): fetched by the runtime from the short-lived URL Core gives for
// it, as the file is, at most MaxFileBytes, and kept by the file's
// checksum as LibreOffice's PDF is, its pages capped as LibreOffice's are
// (office.Service.TakeRendition). The runtime converts the file with
// LibreOffice itself only where Core has no PDF of it to give: its
// rendition queued or being made, failed or skipped, a file Core does not
// convert, a Core from before renditions, or a PDF that could not be
// fetched. Where LibreOffice does not convert here at all, a model that
// takes files is still given the PDF Core made (rendered).
//
// The URL is a credential for the PDF: no model is given it (Core's result
// is stripped of every download_url), and no error the runtime keeps or
// logs holds it. One that has lapsed, by its time or by the server's
// refusal, is asked of Core again, with the caller's own token.

// renditionMargin is how long before Core says a rendition's URL stops
// working it is taken to have lapsed: a fetch takes a moment to begin.
const renditionMargin = 30 * time.Second

// errRenditionNotFetched is Core's PDF of a file not fetched, for a reason
// other than its size or the server's status: whatever the fetcher said is
// left out, as it may name the URL.
var errRenditionNotFetched = errors.New("toolset: Core's PDF of the file could not be fetched")

// renditionDone reports whether Core says d's file has a PDF rendition
// done.
func (d *docFile) renditionDone() bool {
	return d.rendition != nil && d.rendition.State == core.RenditionDone
}

// pdfOf is the PDF of d's file, data, whose checksum is sum, of format f:
// Core's rendition of it where Core has one to give (renditionPDF), and
// otherwise LibreOffice's (Office.Convert), or where that conversion
// stands.
func (r Runner) pdfOf(ctx context.Context, d *docFile, sum string, f office.Format, data []byte) office.State {
	if out := r.renditionPDF(ctx, d, sum); out != nil {
		return office.State{Status: office.StatusDone, Out: out}
	}
	return r.Office.Convert(ctx, sum, f, office.ToPDF, data)
}

// renditionPDF is Core's PDF of d's file, whose checksum is sum, where
// Core says its rendition is done and the PDF is within what the runtime
// fetches of a file: the PDF kept of the file, or the one fetched now; nil
// where there is none, or it could not be had, which is not tried again
// in the call (a document's text read after its PDF, OCR's pages picked
// from it).
func (r Runner) renditionPDF(ctx context.Context, d *docFile, sum string) *office.Output {
	if d == nil || d.renditionMissed || !d.renditionDone() || r.Office == nil || r.Files == nil || d.rendition.ByteSize > r.MaxFileBytes {
		return nil
	}
	out, err := r.Office.TakeRendition(ctx, sum, func(ctx context.Context) ([]byte, error) {
		return r.fetchRendition(ctx, d)
	})
	if err != nil {
		d.renditionMissed = true
		return nil
	}
	return out
}

// fetchRendition fetches Core's PDF of d's file, at most MaxFileBytes,
// from the URL Core gave for it; where that has lapsed, by its time or by
// the file server's refusal, from a fresh one (freshRendition), once. Its
// errors are ErrTooLarge, a *FetchError, which holds the server's status
// alone, or errRenditionNotFetched: never the fetcher's own, nor the URL.
func (r Runner) fetchRendition(ctx context.Context, d *docFile) ([]byte, error) {
	v, fresh := d.rendition, false
	if v.DownloadURL == "" || v.DownloadExpiresAt != nil && !time.Now().Before(v.DownloadExpiresAt.Add(-renditionMargin)) {
		if v, fresh = r.freshRendition(ctx, d), true; v == nil {
			return nil, errRenditionNotFetched
		}
	}
	f, err := r.Files.Fetch(ctx, v.DownloadURL, r.MaxFileBytes)
	var fe *FetchError
	if !fresh && errors.As(err, &fe) && lapsed(fe.Status) {
		if v = r.freshRendition(ctx, d); v == nil {
			return nil, &FetchError{Status: fe.Status}
		}
		f, err = r.Files.Fetch(ctx, v.DownloadURL, r.MaxFileBytes)
	}
	switch {
	case errors.Is(err, ErrTooLarge):
		return nil, ErrTooLarge
	case errors.As(err, &fe):
		return nil, &FetchError{Status: fe.Status}
	case err != nil:
		return nil, errRenditionNotFetched
	}
	return f.Data, nil
}

// lapsed reports whether a file server's status says a short-lived URL is
// no longer good: refused, or not there.
func lapsed(status int) bool {
	return status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusGone
}

// freshRendition is d's file's rendition read again of Core, with the
// caller's own token, for a fresh URL: a version's file by document_file,
// where Core names it by its id, a message's by conversation_attachment.
// nil where it cannot be read, is no longer done, or is not of the file d
// is (another version, other bytes). d keeps it for what comes after.
func (r Runner) freshRendition(ctx context.Context, d *docFile) *core.RenditionView {
	if r.Client == nil || d.courseID == "" {
		return nil
	}
	var v *core.RenditionView
	switch {
	case d.attachmentID != "":
		att, err := r.Client.Attachment(ctx, d.courseID, d.attachmentID)
		if err != nil || !strings.EqualFold(att.ID, d.attachmentID) || att.Checksum != nil && d.checksum != "" && *att.Checksum != d.checksum {
			return nil
		}
		v = att.Rendition
	case d.fileID != "" && d.documentID != "":
		f, err := r.Client.DocumentFile(ctx, d.courseID, d.documentID, d.fileID)
		if err != nil || f.VersionID != "" && d.versionID != "" && f.VersionID != d.versionID ||
			f.Checksum != "" && d.checksum != "" && f.Checksum != d.checksum {
			return nil
		}
		v = f.Rendition
	}
	if v == nil || v.State != core.RenditionDone || v.DownloadURL == "" {
		return nil
	}
	d.rendition = v
	return v
}

// rendered reports whether d, of media type mt, is a presentation or a
// document whose PDF Core has made, to be given as a file the runtime
// converts (kindConvert) to a model that takes files, where LibreOffice
// does not convert here: Core's PDF of it, and its text where that PDF
// cannot be given. To a model that takes none it is what it was.
func (r Runner) rendered(d *docFile, mt string) bool {
	f, ok := office.FormatOf(mt)
	return ok && (f.Family == office.Slides || f.Family == office.Document) && r.FileInput && d != nil && d.renditionDone() &&
		r.Office != nil && r.Files != nil
}

// kindFor is what the runner makes of d, a file of media type mt: its
// kind (kindOf), but for a presentation or a document whose PDF Core has
// made, where LibreOffice does not convert here (rendered), which is
// kindConvert.
func (r Runner) kindFor(d *docFile, mt string) fileKind {
	if k := r.kindOf(mt); k == kindConvert || !r.rendered(d, mt) {
		return k
	}
	return kindConvert
}
