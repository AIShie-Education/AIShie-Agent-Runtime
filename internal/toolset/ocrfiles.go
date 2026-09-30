package toolset

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// OCR recognizes the text of a file that has none of its own to read, for
// a model that cannot take the file itself (design §4, Files): a scanned
// PDF, one whose fonts map to nothing, an image. *ocr.Service is the
// worker's.
type OCR interface {
	// Available reports whether it recognizes anything here, and if not,
	// why, in words for the model.
	Available() (bool, string)
	// Text is the file's text, or where its recognition stands, as
	// ocr.Service.Text says: the file is the one whose checksum is sum,
	// and data gives its bytes should it need recognizing.
	Text(ctx context.Context, sum string, kind ocr.Kind, pages int, data func(context.Context) ([]byte, error)) ocr.State
}

// What fileRecord.OCR says of the runtime's OCR of a file not given, for
// the model: in progress or not started (ask again, with AskAgain's
// arguments), failed, or not here at all.
const (
	OCRInProgress  = "in_progress"
	OCRBusy        = "busy"
	OCRFailed      = "failed"
	OCRUnavailable = "unavailable"
)

// ExtractedOCR is fileRecord.ExtractedFrom of a text the runtime's OCR
// recognized.
const ExtractedOCR = "ocr"

// ocrFile is a file that has no text of its own to give the model, which
// OCR may recognize.
type ocrFile struct {
	kind ocr.Kind
	// pages are a PDF's, as doctext counted them; 0 when not known.
	pages int
	// why is why the file is not given as it is: the note when OCR gives
	// nothing, and what it is said to be given for when it gives text.
	why string
	// ask is what the person may do instead, at the end of a note of no
	// text: "; ask for a version with selectable text".
	ask string
	// sum is what OCR keeps the text by: the file's checksum, or one of
	// the runtime's own, made from it, for what it made of the file (the
	// PDF of a document LibreOffice converted, the slides of a deck that
	// show pictures), which is recognized once for every copy of the file.
	sum string
	// data gives the bytes to recognize, should OCR need them.
	data func(context.Context) ([]byte, error)
	// again is the call that asks for the file again (ask_again).
	again *nextPart
}

// ocrAvailable reports whether this runner recognizes text, and if not,
// why.
func (r Runner) ocrAvailable() (bool, string) {
	if r.OCR == nil {
		return false, ""
	}
	return r.OCR.Available()
}

// noOCR is what a note says of there being no OCR here, and why.
func noOCR(why string) string {
	s := "; the runtime has no OCR here to recognize its text"
	if why != "" {
		s += " (" + why + ")"
	}
	return s
}

// recognize is what OCR recognized of f, rd's file or what the runtime made
// of it: kept (Runner.Texts, under f.sum, as the store keeps it, so that its
// parts are read from the store once), or where its recognition stands, as
// ocr.Service.Text says; res is set when it is done, and holds no text when
// OCR found none.
func (r Runner) recognize(ctx context.Context, rd *fileReading, f ocrFile) (*doctext.Result, ocr.State) {
	if ok, why := r.ocrAvailable(); !ok {
		return nil, ocr.State{Status: ocr.StatusOff, Why: why}
	}
	// A text recognized in other languages than OCR's now is not given.
	key := "ocr\x00" + f.sum
	if l, ok := r.OCR.(interface{ Languages() string }); ok {
		key += "\x00" + l.Languages()
	}
	if kept := r.kept(key); kept != nil && kept.res != nil {
		return kept.res, ocr.State{Status: ocr.StatusDone}
	}
	st := r.OCR.Text(ctx, f.sum, f.kind, f.pages, f.data)
	if st.Status != ocr.StatusDone {
		return nil, st
	}
	res := ocrResult(st.Text)
	if strings.TrimSpace(res.Text) != "" {
		r.keep(key, &fileReading{mt: rd.mt, size: rd.size, sum: rd.sum, res: res})
	}
	return res, st
}

// giveOCR gives the model the text OCR recognized of a file with none of
// its own (f, of rd), or says where its recognition stands: started now,
// in the background, and not done within what the question may wait, the
// note asks the model to call again in a minute, with the arguments
// AskAgain names. The text is given as any other the runtime read, in
// parts when it is long, marked ExtractedOCR, and the note says it may hold
// recognition errors.
func (r Runner) giveOCR(ctx context.Context, g given, rd *fileReading, f ocrFile) given {
	rec := g.rec
	res, st := r.recognize(ctx, rd, f)
	if res != nil {
		return recognized(g, res, f)
	}
	switch st.Status {
	case ocr.StatusPending:
		progress := ""
		if st.Of > 0 {
			progress = fmt.Sprintf(", %d of %d pages done", st.Done, st.Of)
		}
		rec.OCR, rec.AskAgain = OCRInProgress, f.again
		rec.Note = f.why + "; the runtime is recognizing its text now (OCR)" + progress +
			": to read it, " + callAgain(f.again, "in a minute or so")
	case ocr.StatusBusy:
		rec.OCR, rec.AskAgain = OCRBusy, f.again
		rec.Note = f.why + "; the runtime could not start recognizing its text (OCR) just now: " + st.Why +
			"; " + callAgain(f.again, "in a few minutes to try again")
	case ocr.StatusFailed:
		rec.OCR, rec.Note = OCRFailed, f.why+"; nor could the runtime's OCR recognize its text: "+st.Why+f.ask
	default:
		rec.OCR, rec.Note = OCRUnavailable, f.why+noOCR(st.Why)+f.ask
	}
	return g
}

// recognized gives what OCR recognized of a file, res, and says what it
// is: not the file's own text, and not to be trusted as that would be.
func recognized(g given, res *doctext.Result, f ocrFile) given {
	rec := g.rec
	since := strings.TrimPrefix(f.why, notGiven)
	if strings.TrimSpace(res.Text) == "" {
		rec.Note = f.why + "; the runtime's OCR found no text in it" + f.ask
		return g
	}
	rec.GivenAs, rec.ExtractedFrom = givenText, ExtractedOCR
	what := "the image"
	if f.kind == ocr.PDF {
		what = "its " + plural(res.Parts, "page")
	}
	holds := "the runtime's OCR of " + what + " (" + since + "): text recognized from pictures of it, " +
		"which may hold recognition errors (characters misread or missed, a simplified character in its traditional form, " +
		"lines out of order) and has no pictures or layout; where a figure, a name or a date matters, say it was read by OCR"
	rec.Note = strings.Join(append([]string{holds}, res.Notes...), "; ")
	g.text, g.sections = res.Text, res.Sections
	return g
}

// ocrResult is a text OCR recognized, as the rest of the runtime's
// readings are: its pages' sections, in order, with any out of order left
// out, which only a store holding something it should not would give.
func ocrResult(t *store.OCRText) *doctext.Result {
	res := &doctext.Result{Text: t.Text, Parts: t.Pages, Of: t.PagesOf, Notes: t.Notes}
	last := 0
	for _, s := range t.Sections {
		if s.Offset < last || s.Offset > len(t.Text) {
			continue
		}
		res.Sections = append(res.Sections, doctext.Section{Kind: doctext.SectionPage, N: s.N, Offset: s.Offset})
		last = s.Offset
	}
	return res
}

// callAgain says how the model asks for a file again, and when: the call
// ask_again names (np), or, where it has no tool to, that it cannot here.
func callAgain(np *nextPart, when string) string {
	if np == nil {
		return "it cannot be asked for again here"
	}
	return "call " + np.Tool + " again with ask_again's arguments " + when
}

// errNotTheFile is a file fetched again for OCR that is not the one read
// before: a new file under the version's name, which is never OCR's.
var errNotTheFile = errors.New("toolset: the file fetched again is not the one read before")

// refetch gives OCR the file's bytes: data, when this call fetched them;
// else the file fetched again from d's URL, which must be the file rd
// read, by its checksum.
func (r Runner) refetch(d *docFile, rd *fileReading, data []byte) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		if data != nil {
			return data, nil
		}
		if r.Files == nil {
			return nil, errors.New("toolset: files are not fetched here")
		}
		f, err := r.Files.Fetch(ctx, d.url, r.MaxFileBytes)
		if err != nil {
			return nil, err
		}
		if checksum(f.Data) != rd.sum {
			return nil, errNotTheFile
		}
		return f.Data, nil
	}
}
