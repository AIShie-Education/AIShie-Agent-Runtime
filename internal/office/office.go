// Package office turns course documents into what the runtime gives the
// models (docs/design.md §4, Files): presentations and documents
// (PowerPoint, Word, OpenDocument and RTF files) into PDFs, whose pages a
// model that takes files sees as they look, pictures, charts, diagrams and
// formulas with them; older and OpenDocument presentations into
// PowerPoint's own format, whose slides and speaker notes the runtime reads
// as text; older and OpenDocument workbooks into Excel's, which it reads as
// CSV. And it cuts a PDF into ranges of its pages, which a long one is given
// in, and picks pages out of one for OCR.
//
// It runs LibreOffice (soffice --headless --convert-to) and poppler's
// pdftocairo, pdfseparate and pdfunite as subprocesses, held as package
// sandbox holds them: their memory, CPU time, files written and open bound
// by prlimit, a hard timeout that kills the whole process group, nothing of
// the runtime's environment, a private directory removed when the file is
// done. LibreOffice starts from a fresh profile each time, in that
// directory, which turns off what would reach out of the file: links to
// anything outside the document are blocked (it fetches no picture a
// document links to), and its proxy is one that is not there, should
// anything try; active content (OLE, DDE) and macros are off.
//
// Converter runs LibreOffice, Pager poppler's programs; Service is the
// worker's: conversions run in the background, at most Concurrency at once,
// each file converted once, and kept in memory by its checksum.
package office

import (
	"errors"
	"fmt"
	"time"
)

// Config is the settings of the conversions (the environment's OFFICE_PDF*,
// docs/deploying.md). A zero field is its default.
type Config struct {
	// Mode is ModeAuto (on when LibreOffice is there), ModeOn (it must be)
	// or ModeOff.
	Mode string
	// Timeout bounds one conversion.
	Timeout time.Duration
	// MaxPages is the most pages a PDF made has: a document of more is
	// given its first MaxPages.
	MaxPages int
	// MemoryMB is each program's address space.
	MemoryMB int
	// Concurrency is how many files are converted at once, per process;
	// Queue how many may wait for a turn (each holds its bytes).
	Concurrency, Queue int
	// CacheBytes bounds what the conversions kept in memory take.
	CacheBytes int64
	// TempDir is where each file's private directory is made; the system's
	// when "".
	TempDir string
	// Soffice, PDFToCairo, PDFSeparate, PDFUnite and Prlimit are the
	// programs; found on PATH when "".
	Soffice, PDFToCairo, PDFSeparate, PDFUnite, Prlimit string
}

// Modes of Config.
const (
	ModeAuto = "auto"
	ModeOn   = "on"
	ModeOff  = "off"
)

// Defaults of Config.
const (
	DefaultTimeout     = 2 * time.Minute
	DefaultMaxPages    = 300
	DefaultMemoryMB    = 2048
	DefaultConcurrency = 1
	DefaultQueue       = 8
	DefaultCacheBytes  = 64 << 20
)

// WithDefaults is c with its zero fields set to their defaults.
func (c Config) WithDefaults() Config {
	if c.Mode == "" {
		c.Mode = ModeAuto
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxPages <= 0 {
		c.MaxPages = DefaultMaxPages
	}
	if c.MemoryMB <= 0 {
		c.MemoryMB = DefaultMemoryMB
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.Queue <= 0 {
		c.Queue = DefaultQueue
	}
	if c.CacheBytes <= 0 {
		c.CacheBytes = DefaultCacheBytes
	}
	return c
}

// Check says what is wrong with c's values, if anything.
func (c Config) Check() error {
	var errs []error
	switch c.Mode {
	case "", ModeAuto, ModeOn, ModeOff:
	default:
		errs = append(errs, fmt.Errorf("office: mode %q is not auto, on or off", c.Mode))
	}
	if c.MaxPages < 0 || c.MaxPages > 2000 {
		errs = append(errs, fmt.Errorf("office: %d pages is not between 1 and 2000", c.MaxPages))
	}
	if c.Timeout < 0 || c.Timeout > 0 && c.Timeout < 5*time.Second {
		errs = append(errs, fmt.Errorf("office: a timeout of %s is less than LibreOffice takes to start", c.Timeout))
	}
	if c.Concurrency < 0 || c.Concurrency > 4 {
		errs = append(errs, fmt.Errorf("office: %d at once is not between 1 and 4", c.Concurrency))
	}
	if c.MemoryMB != 0 && c.MemoryMB < 512 {
		errs = append(errs, fmt.Errorf("office: %d MB is too little for LibreOffice", c.MemoryMB))
	}
	return errors.Join(errs...)
}

// DefaultPartPages is how many pages of a PDF a model is given as one file
// part, when it has more (PDF_PART_PAGES): ten slides of a lecture are some
// twenty to thirty thousand input tokens, a fifth of an answer's budget,
// which every later turn of the answer sends again.
const DefaultPartPages = 10

// Target is what a file is converted to.
type Target string

// The targets.
const (
	ToPDF  Target = "pdf"
	ToPPTX Target = "pptx"
	ToXLSX Target = "xlsx"
)

// Output is a file converted.
type Output struct {
	Data []byte
	// Pages is a PDF's pages; Capped says it stopped at Config.MaxPages,
	// and the file may have more.
	Pages  int
	Capped bool
	// Rendition says it is Core's PDF rendition of the file
	// (Service.TakeRendition), not LibreOffice's here.
	Rendition bool
}

// Errors of Convert, Range, Pick and TakeRendition, besides the context's.
var (
	// ErrUnavailable is conversion that cannot run here: LibreOffice, or
	// poppler's programs, or prlimit, are not installed.
	ErrUnavailable = errors.New("office: not available")
	// ErrMalformed is a file LibreOffice could not open or convert:
	// damaged, protected by a password, or not what it says it is.
	ErrMalformed = errors.New("office: the file could not be converted")
	// ErrTooLarge is a conversion whose output passes what the runtime
	// keeps.
	ErrTooLarge = errors.New("office: the converted file is larger than the runtime keeps")
	// ErrTimeout is a conversion that took longer than Config.Timeout.
	ErrTimeout = errors.New("office: the conversion took longer than the runtime allows")
	// ErrRendition is Core's PDF of a file that Service.TakeRendition
	// does not take: not fetched, not a PDF that reads, or of more pages
	// than are kept where none are cut.
	ErrRendition = errors.New("office: Core's PDF of the file is not taken")
)

// maxOutput bounds a converted file, and a PDF's range: past it, the
// conversion is ErrTooLarge. It is above every provider's limit on a PDF
// but Gemini's, and a text is read from it within doctext's own bounds.
const maxOutput = 64 << 20
