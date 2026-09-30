package toolset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/ocr"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// Office converts Office files for the models (design §4, Files):
// presentations and documents to PDF, whose pages a model that takes files
// sees as they look, older and OpenDocument decks to PowerPoint's format,
// and older and OpenDocument workbooks to Excel's; and it cuts ranges of
// pages from PDFs, which a long one is given in. *office.Service is the
// worker's.
type Office interface {
	// Available reports whether it converts files here, and if not, why.
	Available() (bool, string)
	// Convert is the file whose checksum is sum, data, of format f,
	// converted to target, or where its conversion stands, as
	// office.Service.Convert says.
	Convert(ctx context.Context, sum string, f office.Format, to office.Target, data []byte) office.State
	// Cuts reports whether it cuts PDFs here.
	Cuts() bool
	// Range is pages first to last of pdf, named sum, as a PDF of their own.
	Range(ctx context.Context, sum string, pdf []byte, first, last int) ([]byte, error)
	// Pick is the pages of pdf given, in order, as a PDF of their own.
	Pick(ctx context.Context, pdf []byte, pages []int) ([]byte, error)
}

// What fileRecord.Conversion says of a file's conversion when what it is
// converted to is not given (yet): in progress or not started (ask again,
// with AskAgain's arguments), or failed.
const (
	ConversionInProgress = "in_progress"
	ConversionBusy       = "busy"
	ConversionFailed     = "failed"
)

// maxPictured bounds the slides of a deck whose pictures OCR reads: OCR's
// own bound on a file's pages, by default.
const maxPictured = 40

// ocrMark begins, in a slide's text, what OCR read of the slide as drawn.
const ocrMark = "[OCR of the slide as drawn]"

// targetWords names what a file is converted to, in a note.
var targetWords = map[office.Target]string{office.ToPDF: "PDF", office.ToPPTX: "PowerPoint's format", office.ToXLSX: "Excel's format"}

// giveConverted gives an Office file the runtime converts (kindConvert),
// data, of media type mt, as the runtime converts it:
//
//   - a workbook as the runtime's text of LibreOffice's Excel form of it;
//   - a presentation or a document, to a model that takes files, as
//     LibreOffice's PDF of it (givePDFFile), in parts of its pages where it
//     has more than a part holds, and a deck's speaker notes beside it as
//     text;
//   - otherwise, or where its PDF is past what the model's provider takes,
//     or is not made (yet), as text (giveConvertedText).
func (r Runner) giveConverted(ctx context.Context, g given, d *docFile, mt string, data []byte, part int) given {
	f, _ := office.FormatOf(mt)
	sum := checksum(data)
	if _, err := doctext.Sniff(data); errors.Is(err, doctext.ErrEncrypted) {
		// An Office Open XML file encrypted with a password, which
		// LibreOffice cannot open either.
		g.rec.Note = notePassword
		return g
	}
	if f.Family == office.Workbook || !r.FileInput {
		return r.giveConvertedText(ctx, g, d, mt, sum, data, nil, "")
	}
	st := r.Office.Convert(ctx, sum, f, office.ToPDF, data)
	if st.Status != office.StatusDone {
		// Its text meanwhile, where the runtime reads it without the PDF;
		// the PDF on asking again, where it is being made.
		why := r.conversionNote(g.rec, d, st, office.ToPDF)
		return r.giveConvertedText(ctx, g, d, mt, sum, data, nil, why)
	}
	pdf := st.Out
	if past := r.bytesPast(int64(len(pdf.Data)), "its PDF is"); past != "" {
		return r.giveConvertedText(ctx, g, d, mt, sum, data, pdf.Data, past)
	}
	p := pdfFile{data: pdf.Data, sum: sum + "/pdf", pages: pdf.Pages, unit: doctext.SectionPage, converted: true, capped: pdf.Capped}
	if f.Family == office.Slides {
		p.unit = doctext.SectionSlide
		rd, st := r.deckReading(ctx, d, mt, sum, data)
		switch {
		case rd != nil && rd.res != nil:
			p.slides = rd.res.Slides
		case rd != nil:
			p.notesWhy = strings.TrimPrefix(extractNote(rd.err), notGiven)
		default:
			p.notesWhy = r.conversionNote(&fileRecord{}, d, st, office.ToPPTX)
		}
	}
	if gf, ok := r.givePDFFile(ctx, g, d, p, part); ok {
		return gf
	}
	past := r.pagesPast(pdf.Pages, "its PDF has")
	if past == "" {
		past = "its pages could not be cut into parts"
	}
	return r.giveConvertedText(ctx, g, d, mt, sum, data, pdf.Data, past)
}

// giveConvertedText gives an Office file the runtime converts as text:
// a deck's slides and notes as the runtime reads them from its PowerPoint
// form (the file's own, or LibreOffice's of an older or OpenDocument deck),
// with what OCR read of the slides that show pictures or charts
// (giveSlides); a Word document's text as the runtime reads it; another
// document's as the runtime reads LibreOffice's PDF of it, page by page,
// with OCR where that has no text to read; a workbook's as the runtime
// reads LibreOffice's Excel form of it. pdf is LibreOffice's PDF of it, when
// made; why says why the PDF is not given to a model that takes files ("":
// the model takes none). What was read is kept (Runner.Texts).
func (r Runner) giveConvertedText(ctx context.Context, g given, d *docFile, mt, sum string, data, pdf []byte, why string) given {
	rec := g.rec
	f, _ := office.FormatOf(mt)
	key := r.textKey(d)
	rd := r.Texts.get(key)
	if rd == nil || rd.fam == "" || rd.sum != sum {
		var st office.State
		rd, st = r.readConverted(ctx, d, mt, sum, data, pdf)
		if rd == nil {
			note := r.conversionNote(rec, d, st, readTarget(f))
			if why != "" && why != note {
				note = why + ", and " + strings.TrimPrefix(note, "the runtime ")
			}
			rec.Note = notGiven + note + askLater(rec)
			return g
		}
	}
	g = r.giveConvertedReading(ctx, g, d, rd, why, data)
	if rec.GivenAs == givenText && askLater(rec) != "" {
		// Its PDF is being made: the model sees it on asking again.
		rec.Note += askLater(rec) + " to see its pages as they look"
	}
	return g
}

// readTarget is what a file of format f is converted to for its text to be
// read.
func readTarget(f office.Format) office.Target {
	switch f.Family {
	case office.Workbook:
		return office.ToXLSX
	case office.Slides:
		return office.ToPPTX
	}
	return office.ToPDF
}

// readConverted reads the text of an Office file the runtime converts,
// data, whose checksum is sum: a PowerPoint or Word file's own, and
// another's in what LibreOffice makes of it (readTarget), converting it
// now; a Word file of little but pictures in its PDF, as another
// document's. pdf is LibreOffice's PDF of it, when made. It is nil, with
// where the conversion stands, when what it needs is not made (yet). What
// was read is kept under the version (Runner.Texts), unless the reading ran
// out of time.
func (r Runner) readConverted(ctx context.Context, d *docFile, mt, sum string, data, pdf []byte) (*fileReading, office.State) {
	f, _ := office.FormatOf(mt)
	rd := &fileReading{mt: mt, size: int64(len(data)), sum: sum, fam: f.Family}
	to := office.Target("")
	if !f.OOXML {
		to = readTarget(f)
	}
	for {
		src, format := data, doctext.Format("")
		switch {
		case to == "":
			format, _ = doctext.FormatOf(mt)
		case to == office.ToPDF && pdf != nil:
			src, format = pdf, doctext.PDF
		default:
			st := r.Office.Convert(ctx, sum, f, to, data)
			if st.Status != office.StatusDone {
				return nil, st
			}
			src = st.Out.Data
			format = map[office.Target]doctext.Format{office.ToPDF: doctext.PDF, office.ToPPTX: doctext.PPTX, office.ToXLSX: doctext.XLSX}[to]
		}
		rd.of = to
		xctx, cancel := context.WithTimeout(ctx, extractTimeout)
		rd.res, rd.err = doctext.Extract(xctx, src, format, r.DocLimits)
		cancel()
		if to != "" || f.Family != office.Document || rd.err != nil || !picturesOnly(rd.res) {
			break
		}
		// A Word file of little but pictures (scanned pages, as a school's
		// often are): its PDF is read instead, which OCR recognizes where
		// it has no text.
		to = office.ToPDF
	}
	if !isContextError(rd.err) {
		r.Texts.put(r.textKey(d), rd)
	}
	return rd, office.State{Status: office.StatusDone}
}

// picturesOnly reports whether a document's text is little but its
// pictures: some, and fewer than 200 letters or digits besides, as doctext
// judges a PDF's text that does not read.
func picturesOnly(res *doctext.Result) bool {
	if res.Images == 0 {
		return false
	}
	n := 0
	for _, c := range strings.ReplaceAll(res.Text, "[image]", "") {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			n++
		}
	}
	return n < 200
}

// deckReading is the runtime's reading of a deck's slides, for the notes
// given beside its PDF: kept, or read now from the deck itself or from
// LibreOffice's PowerPoint form of it. nil, with where the conversion
// stands, when that form is not made (yet).
func (r Runner) deckReading(ctx context.Context, d *docFile, mt, sum string, data []byte) (*fileReading, office.State) {
	if rd := r.Texts.get(r.textKey(d)); rd != nil && rd.fam == office.Slides && rd.sum == sum {
		return rd, office.State{Status: office.StatusDone}
	}
	return r.readConverted(ctx, d, mt, sum, data, nil)
}

// giveConvertedReading gives the text of an Office file the runtime
// converts, as rd read it; past says why a model that takes files is not
// given its PDF. data is the file's bytes when they were fetched for this
// call; nil when rd was kept.
func (r Runner) giveConvertedReading(ctx context.Context, g given, d *docFile, rd *fileReading, past string, data []byte) given {
	rec := g.rec
	if rd.of != "" {
		rec.ConvertedTo = string(rd.of)
	}
	if rd.err != nil {
		rec.Note = extractNote(rd.err)
		if past != "" {
			rec.Note = notGiven + past + ", and " + strings.TrimPrefix(rec.Note, notGiven)
		}
		return g
	}
	switch {
	case rd.fam == office.Slides:
		// The text in its pictures is read unless the model is to see
		// them in its PDF on asking again, or its PDF could not be made.
		g = r.giveSlides(ctx, g, d, rd, data, rec.Conversion == "")
	case rd.of == office.ToPDF:
		g = r.giveDocumentPDFText(ctx, g, d, rd, data)
	default:
		g = r.extracted(g, rd.res)
	}
	if rec.GivenAs != givenText {
		if past != "" && !strings.HasPrefix(rec.Note, past) {
			rec.Note = strings.TrimPrefix(rec.Note, notGiven)
			rec.Note = notGiven + past + ", and " + rec.Note
		}
		return g
	}
	if rd.of != "" && rd.of != office.ToPDF {
		rec.Note = "LibreOffice converted it to " + targetWords[rd.of] + ": " + rec.Note
	}
	if past != "" {
		rec.Note = "it is given as the runtime's text of it, since " + past + "; " + rec.Note
	}
	return g
}

// giveDocumentPDFText gives the text of LibreOffice's PDF of a document,
// as rd read it, where that reads as text; where it does not, what OCR
// recognizes of the PDF (giveOCR).
func (r Runner) giveDocumentPDFText(ctx context.Context, g given, d *docFile, rd *fileReading, data []byte) given {
	res := rd.res
	if res.Unreadable != "" {
		why := "LibreOffice's PDF of it has no text to read: it looks scanned, or like pictures of text"
		if res.Unreadable == doctext.UnreadableUnmapped {
			why = "the text of LibreOffice's PDF of it cannot be read: its fonts do not map to text"
		}
		if !r.FileInput {
			why = noteNoFiles + ", and " + why
		} else {
			why = notGiven + why
		}
		return r.giveOCR(ctx, g, rd, ocrFile{kind: ocr.PDF, pages: res.Of, why: why, ask: askSelectable,
			sum: derivedSum("pdf", rd.sum, nil), data: r.convertedPDF(d, rd, data, nil), again: askAgain(d)})
	}
	g = r.extracted(g, res)
	g.rec.Note = "LibreOffice converted it to PDF: " + g.rec.Note
	return g
}

// giveSlides gives a deck's slides and speaker notes as rd read them; and,
// where pictures says so, what OCR reads of the slides that show pictures
// or charts (at most maxPictured), each after the slide's own text, under
// ocrMark: the slides as LibreOffice draws them, picked out of its PDF of
// the deck, recognized once in the background and kept as any OCR is. While
// OCR reads them, or where it cannot, the slides are given without it, and
// the note says so.
func (r Runner) giveSlides(ctx context.Context, g given, d *docFile, rd *fileReading, data []byte, pictures bool) given {
	rec := g.rec
	res := rd.res
	g = r.extracted(g, res)
	if rec.GivenAs != givenText || !pictures {
		return g
	}
	var shown []int
	for _, s := range res.Slides {
		if s.Pictures > 0 {
			shown = append(shown, s.N)
		}
	}
	if len(shown) == 0 {
		return g
	}
	cut := ""
	if len(shown) > maxPictured {
		cut = fmt.Sprintf(" (of its first %d slides that show them; the rest are not read)", maxPictured)
		shown = shown[:maxPictured]
	}
	which := slidesWords(shown)
	f := ocrFile{kind: ocr.PDF, pages: len(shown), sum: derivedSum("slides", rd.sum, shown), again: askAgain(d),
		data: r.convertedPDF(d, rd, data, shown)}
	read, st := r.recognize(ctx, rd, f)
	if read != nil {
		text, sections := mergeSlides(res, read, shown)
		g.text, g.sections = text, sections
		rec.Note = strings.Replace(rec.Note, "are only named, as [image] and [chart]", "are named, as [image] and [chart]", 1) +
			"; the text in the pictures and charts of " + which + cut + " is what the runtime's OCR read of those slides as LibreOffice draws them, " +
			"after " + ocrMark + " in each: it may hold recognition errors, and holds the slide's own text again"
		return g
	}
	switch st.Status {
	case ocr.StatusPending:
		rec.OCR, rec.AskAgain = OCRInProgress, f.again
		rec.Note += "; the runtime is reading the text in the pictures and charts of " + which + cut + " now (OCR): to have it too, call " +
			FilePartTool + " again with ask_again's arguments in a minute or so"
	case ocr.StatusBusy:
		rec.OCR, rec.AskAgain = OCRBusy, f.again
		rec.Note += "; the runtime could not start reading the text in the pictures and charts of " + which + " (OCR) just now: " + st.Why +
			"; call " + FilePartTool + " again with ask_again's arguments in a few minutes to try again"
	case ocr.StatusFailed:
		rec.OCR = OCRFailed
		rec.Note += "; the runtime's OCR could not read the text in its pictures and charts: " + st.Why
	default:
		rec.Note += "; the text in its pictures and charts is not read" + strings.TrimSuffix(strings.Replace(noOCR(st.Why), "; the runtime has", ": the runtime has", 1), " to recognize its text")
	}
	return g
}

// convertedPDF gives OCR the bytes of LibreOffice's PDF of rd's file, or of
// the pages given of it: the file's bytes (data, or the file fetched again,
// which must be the file rd read) converted, which the conversion keeps.
func (r Runner) convertedPDF(d *docFile, rd *fileReading, data []byte, pages []int) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		src, err := r.refetch(d, rd, data)(ctx)
		if err != nil {
			return nil, err
		}
		f, _ := office.FormatOf(rd.mt)
		st := r.Office.Convert(ctx, rd.sum, f, office.ToPDF, src)
		if st.Status != office.StatusDone {
			return nil, errors.New("toolset: the file's PDF is not made: " + string(st.Status))
		}
		if pages == nil {
			return st.Out.Data, nil
		}
		return r.Office.Pick(ctx, st.Out.Data, pages)
	}
}

// mergeSlides is a deck's text (res) with what OCR read of the slides
// shown (read, its pages those slides, in order) after each one's own
// text and before its notes, and the slides' sections where they now
// begin. A page OCR found nothing on, or could not read, adds nothing.
func mergeSlides(res, read *doctext.Result, shown []int) (string, []doctext.Section) {
	byslide := map[int]string{}
	for i, sec := range read.Sections {
		end := len(read.Text)
		if i+1 < len(read.Sections) {
			end = read.Sections[i+1].Offset
		}
		body := read.Text[sec.Offset:end]
		_, body, _ = strings.Cut(body, "\n")
		body = strings.TrimSpace(body)
		if body == "" || !strings.Contains(body, "\n") && strings.HasPrefix(body, "[") && strings.HasSuffix(body, "]") {
			continue
		}
		if sec.N >= 1 && sec.N <= len(shown) {
			byslide[shown[sec.N-1]] = body
		}
	}
	notes := map[int]string{}
	for _, s := range res.Slides {
		notes[s.N] = s.Notes
	}
	if len(res.Sections) == 0 {
		return res.Text, nil
	}
	var b strings.Builder
	b.WriteString(res.Text[:res.Sections[0].Offset])
	sections := make([]doctext.Section, 0, len(res.Sections))
	for i, sec := range res.Sections {
		end := len(res.Text)
		if i+1 < len(res.Sections) {
			end = res.Sections[i+1].Offset
		}
		sections = append(sections, doctext.Section{Kind: sec.Kind, N: sec.N, Offset: b.Len()})
		body := res.Text[sec.Offset:end]
		ocrText, ok := byslide[sec.N]
		if !ok || sec.Kind != doctext.SectionSlide {
			b.WriteString(body)
			continue
		}
		own := strings.TrimRight(body, "\n")
		tail := body[len(own):]
		block := ocrMark + "\n" + ocrText
		if n := "Notes: " + notes[sec.N]; notes[sec.N] != "" && strings.HasSuffix(own, "\n"+n) {
			b.WriteString(strings.TrimSuffix(own, n) + block + "\n" + n + tail)
			continue
		}
		b.WriteString(own + "\n" + block + tail)
	}
	return b.String(), sections
}

// slidesWords names slides: "slide 3", "slides 1, 3 and 7".
func slidesWords(ns []int) string {
	if len(ns) == 1 {
		return "slide " + strconv.Itoa(ns[0])
	}
	ss := make([]string, len(ns))
	for i, n := range ns {
		ss[i] = strconv.Itoa(n)
	}
	return "slides " + strings.Join(ss[:len(ss)-1], ", ") + " and " + ss[len(ss)-1]
}

// derivedSum is the checksum OCR keeps what the runtime made of a file by:
// sha256 of what it is (kind, and pages when some are picked) and the
// file's own checksum, as "sha256:<hex>", so that it is the same for every
// copy of the file and every conversion of it, and names nothing else.
func derivedSum(kind, sum string, pages []int) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "aishie-office\x00%s\x00%s\x00%v", kind, sum, pages)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// conversionNote says why a file's conversion to target gives nothing
// (yet), and records where it stands: in progress or not started, when
// AskAgain is the call that asks again; failed; or no conversion here.
func (r Runner) conversionNote(rec *fileRecord, d *docFile, st office.State, to office.Target) string {
	what := targetWords[to]
	switch st.Status {
	case office.StatusPending:
		rec.Conversion, rec.AskAgain = ConversionInProgress, askAgain(d)
		return "the runtime is converting it to " + what + " now (LibreOffice)"
	case office.StatusBusy:
		rec.Conversion, rec.AskAgain = ConversionBusy, askAgain(d)
		return "the runtime could not start converting it to " + what + " just now: " + st.Why
	case office.StatusFailed:
		rec.Conversion = ConversionFailed
		return "LibreOffice could not convert it to " + what + ": " + st.Why
	}
	return "the runtime cannot convert it to " + what + " here: " + st.Why
}

// askLater is what a note asks of the model when a conversion it needs is
// in progress or not started: to call again.
func askLater(rec *fileRecord) string {
	switch rec.Conversion {
	case ConversionInProgress:
		return "; call " + FilePartTool + " again with ask_again's arguments in a minute or so"
	case ConversionBusy:
		return "; call " + FilePartTool + " again with ask_again's arguments in a few minutes to try again"
	}
	return ""
}
