// Package ocr recognizes the text of course documents that have none of
// their own to read (docs/design.md §4, Files): a scanned PDF, one whose
// fonts map to nothing, and an image, for the models that cannot take the
// file itself. Much of a school's material in China is scanned, and many of
// the models it uses take no files.
//
// It runs two programs as subprocesses: pdftoppm (poppler-utils), which
// renders a PDF's pages, and tesseract, which recognizes them, in Chinese
// (simplified and traditional) and English by default. Every file is taken
// to be hostile: the programs run one page at a time, each under a hard
// timeout that kills its process group, with prlimit (util-linux) holding
// its memory, CPU time, files written and open, and core dumps before the
// program runs; with no network, nothing of the runtime's environment but
// a PATH, one thread (OMP_THREAD_LIMIT), a lower priority, and a private
// temporary directory removed when the file is done. At most MaxPages
// pages are read, rendered at DPI and never past MaxSide pixels a side;
// an image past MaxPixels is not read at all.
//
// Engine runs the programs; Service is the worker's: at most Concurrency
// files at once, a bounded queue, and the text kept in the store by the
// file's checksum, so that a file is recognized once, in the background,
// and every later question reads what was kept.
package ocr

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind is what a file to recognize is.
type Kind string

// The kinds recognized.
const (
	PDF   Kind = "pdf"
	Image Kind = "image"
)

// Config is OCR's settings (the environment's OCR_*, docs/deploying.md).
// A zero field is its default.
type Config struct {
	// Mode is ModeAuto (on when the programs are there), ModeOn (they must
	// be) or ModeOff.
	Mode string
	// Languages are tesseract's, joined by +.
	Languages string
	// MaxPages is the pages of a PDF recognized; the rest are said to be
	// left out.
	MaxPages int
	// DPI is the resolution a PDF's pages are rendered at.
	DPI int
	// MaxSide is the most pixels a rendered page has a side: past it, the
	// page is cut.
	MaxSide int
	// MaxPixels is the largest image recognized, in pixels.
	MaxPixels int64
	// PageTimeout bounds one program's run on one page; Timeout a whole
	// file's.
	PageTimeout, Timeout time.Duration
	// MemoryMB is each program's address space.
	MemoryMB int
	// Concurrency is how many files are recognized at once, per process;
	// Queue how many may wait for a turn. A file waiting holds its bytes,
	// so the two bound the memory OCR takes besides its programs': at
	// most Concurrency+Queue files of the largest size fetched.
	Concurrency, Queue int
	// Wait is how long the first question about a file waits for it to be
	// recognized before it is told to ask again; never past the answer's
	// own time.
	Wait time.Duration
	// TempDir is where each file's private directory is made; the system's
	// when "".
	TempDir string
	// Tesseract, PDFToPPM and Prlimit are the programs; found on PATH when
	// "".
	Tesseract, PDFToPPM, Prlimit string
}

// Modes of Config.
const (
	ModeAuto = "auto"
	ModeOn   = "on"
	ModeOff  = "off"
)

// Defaults of Config.
const (
	DefaultLanguages   = "chi_sim+chi_tra+eng"
	DefaultMaxPages    = 40
	DefaultDPI         = 300
	DefaultMaxSide     = 5000
	DefaultMaxPixels   = 40_000_000
	DefaultPageTimeout = 90 * time.Second
	DefaultTimeout     = 15 * time.Minute
	DefaultMemoryMB    = 1024
	DefaultConcurrency = 1
	DefaultQueue       = 8
	DefaultWait        = 5 * time.Second
)

// WithDefaults is c with its zero fields set to their defaults.
func (c Config) WithDefaults() Config {
	if c.Mode == "" {
		c.Mode = ModeAuto
	}
	if c.Languages == "" {
		c.Languages = DefaultLanguages
	}
	if c.MaxPages <= 0 {
		c.MaxPages = DefaultMaxPages
	}
	if c.DPI <= 0 {
		c.DPI = DefaultDPI
	}
	if c.MaxSide <= 0 {
		c.MaxSide = DefaultMaxSide
	}
	if c.MaxPixels <= 0 {
		c.MaxPixels = DefaultMaxPixels
	}
	if c.PageTimeout <= 0 {
		c.PageTimeout = DefaultPageTimeout
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
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
	if c.Wait < 0 {
		c.Wait = 0
	} else if c.Wait == 0 {
		c.Wait = DefaultWait
	}
	return c
}

// Check says what is wrong with c's values, if anything.
func (c Config) Check() error {
	var errs []error
	switch c.Mode {
	case "", ModeAuto, ModeOn, ModeOff:
	default:
		errs = append(errs, fmt.Errorf("ocr: mode %q is not auto, on or off", c.Mode))
	}
	for _, l := range strings.Split(c.Languages, "+") {
		if c.Languages != "" && !validLanguage(l) {
			errs = append(errs, fmt.Errorf("ocr: %q is not a language name such as chi_sim", l))
		}
	}
	if c.DPI != 0 && (c.DPI < 72 || c.DPI > 600) {
		errs = append(errs, fmt.Errorf("ocr: a resolution of %d dpi is not between 72 and 600", c.DPI))
	}
	if c.MaxPages < 0 || c.MaxPages > 2000 {
		errs = append(errs, fmt.Errorf("ocr: %d pages is not between 1 and 2000", c.MaxPages))
	}
	if c.Concurrency < 0 || c.Concurrency > 8 {
		errs = append(errs, fmt.Errorf("ocr: %d at once is not between 1 and 8", c.Concurrency))
	}
	if c.MemoryMB != 0 && c.MemoryMB < 128 {
		errs = append(errs, fmt.Errorf("ocr: %d MB is too little for tesseract", c.MemoryMB))
	}
	return errors.Join(errs...)
}

// validLanguage reports whether l is a name tesseract's languages have: a
// traineddata file's, letters, digits and _, so that it is no path.
func validLanguage(l string) bool {
	if l == "" || len(l) > 32 {
		return false
	}
	return strings.IndexFunc(l, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_'
	}) < 0
}

// Result is what was recognized of a file.
type Result struct {
	// Text is the text recognized: a PDF's pages each under "## Page N",
	// an image's as it is.
	Text string
	// Sections are where each page's heading begins in Text.
	Sections []Section
	// Pages is the pages recognized, of Of the file has (0 when that is
	// not known).
	Pages, Of int
	// Empty is the pages OCR found no text on, Failed those it could not
	// render or read.
	Empty, Failed int
	// Notes say what the text leaves out, for the model.
	Notes []string
}

// Section is where a page's heading begins in a Result's Text.
type Section struct {
	N, Offset int
}

// Errors of Recognize, besides the context's.
var (
	// ErrTooLarge is a file past what OCR reads: an image of more pixels
	// than MaxPixels.
	ErrTooLarge = errors.New("ocr: the file is larger than the runtime recognizes")
	// ErrMalformed is a file the programs could not read at all.
	ErrMalformed = errors.New("ocr: the file could not be read")
	// ErrUnavailable is OCR that cannot run here: its programs, or their
	// languages, are not installed.
	ErrUnavailable = errors.New("ocr: not available")
)
