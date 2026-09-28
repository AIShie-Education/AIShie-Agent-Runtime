// Package doctext reads the text of a course document's file, so that a
// model that cannot read the file itself is given what it says (the
// runtime's docs/design.md §4, Core's docs/agent-runtime.md §3.1 rule 6).
// The runtime reads the file, never the model: it reads PowerPoint, Word and
// Excel files (Office Open XML: .pptx, .docx, .xlsx and their macro-enabled
// and template forms) with archive/zip and encoding/xml, and PDF with a
// reader of its own; nothing here runs a program or touches the filesystem.
//
// Every file is taken to be hostile. What reading one may cost is bounded
// (Limits): the bytes decompressed in all and from any one entry or stream,
// the entries of an archive, the objects of a PDF and its pages, how deep
// XML and PDF objects nest, the XML tokens and PDF operators read, and the
// text made; and the call's context bounds the time, checked as it goes. A
// file past a limit is refused (ErrLimit), a damaged one is refused
// (ErrMalformed), an encrypted one is refused (ErrEncrypted) unless it is a
// PDF anyone may open, and one in an older binary Office format is not read
// (ErrOldFormat). Relationships to anything outside the file are never
// followed.
package doctext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Format is a kind of file this package reads.
type Format string

// The formats read.
const (
	PPTX Format = "pptx"
	DOCX Format = "docx"
	XLSX Format = "xlsx"
	PDF  Format = "pdf"
)

// Errors of Extract. Each is wrapped with what went wrong, in words fit for
// the model: never the file's bytes.
var (
	// ErrOldFormat is a file in an older binary Office format (.doc, .ppt,
	// .xls), which the runtime does not read.
	ErrOldFormat = errors.New("doctext: an older binary Office format")
	// ErrEncrypted is a file that needs a password to open.
	ErrEncrypted = errors.New("doctext: the file is password-protected")
	// ErrLimit is a file past what the runtime reads (Limits).
	ErrLimit = errors.New("doctext: the file is past what the runtime reads")
	// ErrMalformed is a file that is damaged, or not what it says it is.
	ErrMalformed = errors.New("doctext: the file could not be read")
)

// Limits bound what reading one file may cost.
type Limits struct {
	// MaxInflated is the bytes decompressed in all: an archive's entries
	// and a PDF's streams.
	MaxInflated int64
	// MaxEntry is the bytes of any one entry or stream, decompressed.
	MaxEntry int64
	// MaxEntries is the entries an archive may hold.
	MaxEntries int
	// MaxDepth is how deep XML elements, and PDF arrays and dictionaries,
	// may nest.
	MaxDepth int
	// MaxTokens is the XML tokens, or PDF tokens and operators, read in
	// all.
	MaxTokens int
	// MaxText is the bytes of text made; the rest of the file is not read,
	// and the text says so.
	MaxText int
	// MaxRows and MaxCols are the rows and columns read of each sheet of a
	// workbook; the text says what was left out.
	MaxRows, MaxCols int
	// MaxParts is the slides, pages or sheets read; the text says what was
	// left out.
	MaxParts int
	// MaxObjects is the objects a PDF's cross-reference may name.
	MaxObjects int
}

// DefaultLimits are the runtime's: generous for a course's slides, papers
// and sheets, and far below what would hurt a worker.
func DefaultLimits() Limits {
	return Limits{
		MaxInflated: 64 << 20,
		MaxEntry:    32 << 20,
		MaxEntries:  10000,
		MaxDepth:    256,
		MaxTokens:   8_000_000,
		MaxText:     2 << 20,
		MaxRows:     500,
		MaxCols:     50,
		MaxParts:    2000,
		MaxObjects:  500_000,
	}
}

// Result is a file's text as the runtime read it.
type Result struct {
	Format Format
	// Text is the file's text, with its structure kept as plain Markdown:
	// each slide, page or sheet under a heading of its own.
	Text string
	// Parts is the slides, pages or sheets read, and Of how many the file
	// has (the same unless MaxParts cut it).
	Parts, Of int
	// Images and Charts are how many of each the text names only, as
	// [image] and [chart].
	Images, Charts int
	// Notes say what the text leaves out, for the model: a sheet cut to its
	// first rows, pages with no text.
	Notes []string
	// Unreadable, for a PDF, says why its text does not read as text: "" when
	// it does; UnreadableNoText or UnreadableUnmapped when not.
	Unreadable string
}

// Why a PDF's text does not read (Result.Unreadable).
const (
	// UnreadableNoText: the pages hold no text, as a scanned document's.
	UnreadableNoText = "no_text"
	// UnreadableUnmapped: the text does not map to characters: its fonts
	// have no map to Unicode, or a broken one.
	UnreadableUnmapped = "unmapped"
)

// recoverPanics turns a reader's panic into ErrMalformed; the fuzz tests
// turn it off, to see what they find.
var recoverPanics = true

// Extract reads data as a file of format f, within lim and ctx.
func Extract(ctx context.Context, data []byte, f Format, lim Limits) (res *Result, err error) {
	b := newBudget(ctx, lim)
	if recoverPanics {
		defer func() {
			// The readers check what they index; a mistake of theirs on a
			// file made to find one is the file's fault, and must not take
			// the worker down.
			if r := recover(); r != nil {
				res, err = nil, fmt.Errorf("%w: it is malformed", ErrMalformed)
			}
		}()
	}
	switch f {
	case PPTX:
		return readPPTX(data, b)
	case DOCX:
		return readDOCX(data, b)
	case XLSX:
		return readXLSX(data, b)
	case PDF:
		return readPDF(data, b)
	}
	return nil, fmt.Errorf("doctext: %q is not a format read here", f)
}

// media types of the formats read, by format: the Office Open XML types,
// with their macro-enabled and template forms, which hold the same parts.
var mediaTypes = map[string]Format{
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": PPTX,
	"application/vnd.openxmlformats-officedocument.presentationml.slideshow":    PPTX,
	"application/vnd.openxmlformats-officedocument.presentationml.template":     PPTX,
	"application/vnd.ms-powerpoint.presentation.macroenabled.12":                PPTX,
	"application/vnd.ms-powerpoint.slideshow.macroenabled.12":                   PPTX,
	"application/vnd.ms-powerpoint.template.macroenabled.12":                    PPTX,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   DOCX,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.template":   DOCX,
	"application/vnd.ms-word.document.macroenabled.12":                          DOCX,
	"application/vnd.ms-word.template.macroenabled.12":                          DOCX,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         XLSX,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.template":      XLSX,
	"application/vnd.ms-excel.sheet.macroenabled.12":                            XLSX,
	"application/vnd.ms-excel.template.macroenabled.12":                         XLSX,
	"application/pdf": PDF,
}

// canonical is each format's media type, as a sniffed file is recorded.
var canonical = map[Format]string{
	PPTX: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	DOCX: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	XLSX: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	PDF:  "application/pdf",
}

// FormatOf is the format of a media type (type/subtype, lower case), and
// whether it is one read here.
func FormatOf(mediaType string) (Format, bool) {
	f, ok := mediaTypes[mediaType]
	return f, ok
}

// MediaType is f's media type.
func (f Format) MediaType() string { return canonical[f] }

// OldOffice reports whether mediaType is an older binary Office format's.
func OldOffice(mediaType string) bool {
	switch mediaType {
	case "application/msword", "application/vnd.ms-powerpoint", "application/vnd.ms-excel", "application/vnd.ms-office",
		"application/x-msword", "application/x-mspowerpoint", "application/x-msexcel", "application/x-ole-storage":
		return true
	}
	return false
}

// Sniff says what data is, when nothing else does: a PDF, or an Office Open
// XML file by the part its package names as the document; "" when neither.
// An older binary Office file is ErrOldFormat, and one that is an Office
// Open XML file encrypted with a password (which is kept in the same
// container) is ErrEncrypted.
func Sniff(data []byte) (Format, error) {
	if isPDF(data) {
		return PDF, nil
	}
	if isCFB(data) {
		return "", cfbError(data)
	}
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return "", nil
	}
	p, err := openOPC(data, newBudget(context.Background(), DefaultLimits()))
	if err != nil {
		return "", nil
	}
	return p.format(), nil
}

// isPDF reports whether data begins as a PDF does: %PDF- within its first
// kilobyte, where readers look for it.
func isPDF(data []byte) bool {
	return bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-"))
}

// cfbMagic begins a Compound File Binary file: the container of the older
// Office formats, and of an encrypted Office Open XML file.
var cfbMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

func isCFB(data []byte) bool { return bytes.HasPrefix(data, cfbMagic) }

// cfbError is what a Compound File Binary file is to the runtime: an Office
// Open XML file encrypted with a password keeps its package in a stream
// named EncryptedPackage (MS-OFFCRYPTO), whose name the container's
// directory holds in UTF-16; anything else is an older binary format.
func cfbError(data []byte) error {
	if bytes.Contains(data, utf16LE("EncryptedPackage")) {
		return fmt.Errorf("%w: it is encrypted with a password", ErrEncrypted)
	}
	return fmt.Errorf("%w: it is an older binary Office file", ErrOldFormat)
}

func utf16LE(s string) []byte {
	out := make([]byte, 0, 2*len(s))
	for i := 0; i < len(s); i++ {
		out = append(out, s[i], 0)
	}
	return out
}

// budget is what one extraction may still spend, and its context.
type budget struct {
	ctx      context.Context
	lim      Limits
	inflated int64
	tokens   int
}

func newBudget(ctx context.Context, lim Limits) *budget {
	d := DefaultLimits()
	if lim.MaxInflated <= 0 {
		lim.MaxInflated = d.MaxInflated
	}
	if lim.MaxEntry <= 0 {
		lim.MaxEntry = d.MaxEntry
	}
	lim.MaxEntry = min(lim.MaxEntry, lim.MaxInflated)
	if lim.MaxEntries <= 0 {
		lim.MaxEntries = d.MaxEntries
	}
	if lim.MaxDepth <= 0 {
		lim.MaxDepth = d.MaxDepth
	}
	if lim.MaxTokens <= 0 {
		lim.MaxTokens = d.MaxTokens
	}
	if lim.MaxText <= 0 {
		lim.MaxText = d.MaxText
	}
	if lim.MaxRows <= 0 {
		lim.MaxRows = d.MaxRows
	}
	if lim.MaxCols <= 0 {
		lim.MaxCols = d.MaxCols
	}
	if lim.MaxParts <= 0 {
		lim.MaxParts = d.MaxParts
	}
	if lim.MaxObjects <= 0 {
		lim.MaxObjects = d.MaxObjects
	}
	return &budget{ctx: ctx, lim: lim}
}

// inflate spends n bytes decompressed.
func (b *budget) inflate(n int64) error {
	b.inflated += n
	if b.inflated > b.lim.MaxInflated {
		return limitf("it decompresses to more than %d MiB", b.lim.MaxInflated>>20)
	}
	return nil
}

// left is what may still be decompressed from one entry or stream.
func (b *budget) left() int64 {
	return max(min(b.lim.MaxEntry, b.lim.MaxInflated-b.inflated), 0)
}

// tick spends one token, and checks the context every so often.
func (b *budget) tick() error {
	b.tokens++
	if b.tokens > b.lim.MaxTokens {
		return limitf("it holds more than %d elements", b.lim.MaxTokens)
	}
	if b.tokens&1023 == 0 {
		return b.alive()
	}
	return nil
}

// alive is the context's error: the time for reading the file is spent.
func (b *budget) alive() error {
	if err := b.ctx.Err(); err != nil {
		return fmt.Errorf("reading the file took too long: %w", err)
	}
	return nil
}

// cutShort reports whether err ends the reading of a file whose text so
// far is still worth giving: a limit met, or the time spent.
func cutShort(err error) bool {
	return errors.Is(err, ErrLimit) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// restNote is the note of a file whose reading err ended.
func restNote(err error) string {
	if errors.Is(err, ErrLimit) {
		return "the rest of it is not given: " + strings.TrimPrefix(err.Error(), ErrLimit.Error()+": ")
	}
	return "the rest of it is not given: reading it took longer than the runtime allows"
}

func limitf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrLimit, fmt.Sprintf(format, args...))
}

func malformedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// textOut is the text being made, cut at MaxText on a rune boundary.
type textOut struct {
	b   strings.Builder
	max int
	cut bool
	// blanks is how many newlines end what is written.
	blanks int
}

func newTextOut(max int) *textOut { return &textOut{max: max, blanks: 2} }

// write adds s, or as much of it as fits.
func (t *textOut) write(s string) {
	if t.cut || s == "" {
		return
	}
	if room := t.max - t.b.Len(); len(s) > room {
		n := room
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s, t.cut = s[:n], true
	}
	t.b.WriteString(s)
	n := len(s) - len(strings.TrimRight(s, "\n"))
	if n == len(s) {
		t.blanks += n
	} else {
		t.blanks = n
	}
}

// line adds s as a line of its own.
func (t *textOut) line(s string) {
	if t.blanks == 0 {
		t.write("\n")
	}
	t.write(s)
	t.write("\n")
}

// para leaves one empty line before what comes next.
func (t *textOut) para() {
	for t.blanks < 2 && t.b.Len() > 0 {
		t.write("\n")
	}
}

// done is the text, with the mark of the cut if there was one.
func (t *textOut) done() string {
	s := strings.TrimRight(t.b.String(), "\n")
	if t.cut {
		s += fmt.Sprintf("\n\n[The rest of the file is not given: its text passed the %d KiB the runtime reads.]", t.max>>10)
	}
	return s
}
