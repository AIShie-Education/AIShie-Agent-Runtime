package ocr

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Engine runs the OCR programs on one file at a time (Recognize). It is
// safe for concurrent use: each file has a directory of its own.
type Engine struct {
	cfg                          Config
	tesseract, pdftoppm, prlimit string
	// version is tesseract's own first line, "tesseract 5.3.4".
	version string
	// observe, when set, is told how long each step of a page took.
	observe func(step string, took time.Duration)
}

// NewEngine finds the programs cfg names, or looks for them on PATH, and
// checks that tesseract has cfg's languages. Without them it is
// ErrUnavailable, saying what is missing: OCR is then off.
func NewEngine(ctx context.Context, cfg Config) (*Engine, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	e := &Engine{cfg: cfg}
	var missing []string
	for _, p := range []struct {
		name string
		at   *string
		dest *string
	}{
		{"tesseract", &cfg.Tesseract, &e.tesseract},
		{"pdftoppm", &cfg.PDFToPPM, &e.pdftoppm},
		{"prlimit", &cfg.Prlimit, &e.prlimit},
	} {
		path := *p.at
		if path == "" {
			path = p.name
		}
		found, err := exec.LookPath(path)
		if err != nil {
			missing = append(missing, p.name)
			continue
		}
		*p.dest = found
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s not installed", ErrUnavailable, strings.Join(missing, ", "))
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out bytes.Buffer
	dir, err := os.MkdirTemp(cfg.TempDir, "aishie-ocr-")
	if err != nil {
		return nil, fmt.Errorf("%w: no temporary directory: %w", ErrUnavailable, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	lim := e.limits()
	if err := e.run(ctx, 10*time.Second, lim, dir, &out, e.tesseract, "--version"); err != nil {
		return nil, fmt.Errorf("%w: tesseract --version: %w", ErrUnavailable, err)
	}
	e.version = firstLine(out.String())
	out.Reset()
	if err := e.run(ctx, 10*time.Second, lim, dir, &out, e.tesseract, "--list-langs"); err != nil {
		return nil, fmt.Errorf("%w: tesseract --list-langs: %w", ErrUnavailable, err)
	}
	have := map[string]bool{}
	for _, l := range strings.Split(out.String(), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var lacking []string
	for _, l := range strings.Split(cfg.Languages, "+") {
		if !have[l] {
			lacking = append(lacking, l)
		}
	}
	if len(lacking) > 0 {
		return nil, fmt.Errorf("%w: tesseract has no %s data", ErrUnavailable, strings.Join(lacking, ", "))
	}
	return e, nil
}

// Describe names the engine as a file's text records what recognized it:
// the program, its version, the languages and the resolution.
func (e *Engine) Describe() string {
	return fmt.Sprintf("%s %s %ddpi", e.version, e.cfg.Languages, e.cfg.DPI)
}

// Config is the engine's configuration, its defaults filled in.
func (e *Engine) Config() Config { return e.cfg }

// Observe has f told how long each step of each page takes: render
// (pdftoppm) and recognize (tesseract). Set it before the engine is used.
func (e *Engine) Observe(f func(step string, took time.Duration)) { e.observe = f }

// limits are what each run of a program is held to.
func (e *Engine) limits() limits {
	return limits{memory: int64(e.cfg.MemoryMB) << 20, cpu: e.cfg.PageTimeout, fileMax: 256 << 20}
}

// Most text kept of one page, and of a file: a page of dense Chinese is a
// few kilobytes.
const (
	maxPageText = 64 << 10
	maxText     = 2 << 20
)

// noTextOnPage stands for a page OCR found nothing on.
const noTextOnPage = "[no text found on this page]"

// Recognize reads the text of data: a PDF, whose pages is how many pages
// it has (0 when that is not known), page by page, or an image. progress,
// when not nil, is told after each page how many are done, of how many
// will be read. A page that cannot be rendered or read, or that runs out
// of its time, is said so and the rest go on; the file's time running out
// ends the reading, keeping what was read. An error is the context's, or
// ErrTooLarge, or ErrMalformed when nothing at all could be read.
func (e *Engine) Recognize(ctx context.Context, data []byte, kind Kind, pages int, progress func(done, of int)) (*Result, error) {
	if kind == Image {
		if err := e.checkImage(data); err != nil {
			return nil, err
		}
	}
	dir, err := os.MkdirTemp(e.cfg.TempDir, "aishie-ocr-")
	if err != nil {
		return nil, fmt.Errorf("ocr: a private directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if kind == Image {
		return e.recognizeImage(ctx, dir, data)
	}
	return e.recognizePDF(ctx, dir, data, pages, progress)
}

func (e *Engine) recognizeImage(ctx context.Context, dir string, data []byte) (*Result, error) {
	in := filepath.Join(dir, "in"+imageExt(data))
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, fmt.Errorf("ocr: writing the file: %w", err)
	}
	text, err := e.recognizePage(ctx, dir, in)
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, errProgramTimeout):
		return nil, fmt.Errorf("%w: recognizing it took longer than the runtime allows", ErrMalformed)
	case err != nil:
		return nil, fmt.Errorf("%w: tesseract could not read it", ErrMalformed)
	}
	res := &Result{Text: text, Pages: 1, Of: 1}
	if text == "" {
		res.Empty = 1
	}
	return res, nil
}

func (e *Engine) recognizePDF(ctx context.Context, dir string, data []byte, pages int, progress func(done, of int)) (*Result, error) {
	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, fmt.Errorf("ocr: writing the file: %w", err)
	}
	n := e.cfg.MaxPages
	if pages > 0 {
		n = min(pages, e.cfg.MaxPages)
	}
	res := &Result{Of: pages}
	var b strings.Builder
	var failedPages, timedOut []int
	stopped := false
	for i := 1; i <= n; i++ {
		if ctx.Err() != nil {
			stopped = true
			break
		}
		text, err := e.renderAndRecognize(ctx, dir, in, i)
		if err != nil && ctx.Err() != nil {
			stopped = true
			break
		}
		if errors.Is(err, errPastLastPage) {
			break
		}
		if b.Len() >= maxText {
			res.Notes = append(res.Notes, "the rest of it is not given: its text passed what the runtime keeps")
			break
		}
		res.Pages++
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		res.Sections = append(res.Sections, Section{N: i, Offset: b.Len()})
		b.WriteString("## Page " + strconv.Itoa(i) + "\n")
		switch {
		case errors.Is(err, errProgramTimeout):
			timedOut = append(timedOut, i)
			b.WriteString("[this page took longer to recognize than the runtime allows]")
		case err != nil:
			failedPages = append(failedPages, i)
			b.WriteString("[this page could not be read]")
		case text == "":
			res.Empty++
			b.WriteString(noTextOnPage)
		default:
			b.WriteString(text)
		}
		if progress != nil {
			progress(res.Pages, n)
		}
	}
	res.Failed = len(failedPages) + len(timedOut)
	if res.Pages == 0 || res.Failed == res.Pages {
		if stopped {
			return nil, context.DeadlineExceeded
		}
		return nil, fmt.Errorf("%w: its pages could not be rendered or recognized", ErrMalformed)
	}
	res.Text = strings.TrimRight(b.String(), "\n")
	if stopped {
		res.Notes = append(res.Notes, fmt.Sprintf("only its first %d pages were recognized: recognizing it took longer than the runtime allows", res.Pages))
	} else if pages > n {
		res.Notes = append(res.Notes, fmt.Sprintf("only its first %d pages of %d were recognized", n, pages))
	}
	if len(failedPages) > 0 {
		res.Notes = append(res.Notes, "pages "+joinInts(failedPages)+" could not be read")
	}
	if len(timedOut) > 0 {
		res.Notes = append(res.Notes, "pages "+joinInts(timedOut)+" took too long to recognize")
	}
	if res.Empty > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("OCR found no text on %d of its pages", res.Empty))
	}
	return res, nil
}

// errPastLastPage is a page past a PDF's last, when its pages were not
// known.
var errPastLastPage = errors.New("ocr: past the last page")

// renderAndRecognize renders page i of the PDF in to an image, with
// pdftoppm, and recognizes it; the image is removed after.
func (e *Engine) renderAndRecognize(ctx context.Context, dir, in string, i int) (string, error) {
	page := strconv.Itoa(i)
	side := strconv.Itoa(e.cfg.MaxSide)
	start := time.Now()
	var out bytes.Buffer
	err := e.run(ctx, e.cfg.PageTimeout, e.limits(), dir, &out, e.pdftoppm,
		"-f", page, "-l", page, "-r", strconv.Itoa(e.cfg.DPI), "-gray", "-png", "-singlefile",
		"-x", "0", "-y", "0", "-W", side, "-H", side, in, filepath.Join(dir, "page"))
	e.took("render", start)
	img := filepath.Join(dir, "page.png")
	defer func() { _ = os.Remove(img) }()
	if err != nil {
		var pe *programError
		if errors.As(err, &pe) && pe.status == 99 && strings.Contains(pe.stderr, "Wrong page range") {
			return "", errPastLastPage
		}
		return "", err
	}
	if _, err := os.Stat(img); err != nil {
		return "", &programError{name: "pdftoppm", status: 0, stderr: "no image"}
	}
	return e.recognizePage(ctx, dir, img)
}

// recognizePage runs tesseract on one image, and cleans what it read.
func (e *Engine) recognizePage(ctx context.Context, dir, img string) (string, error) {
	start := time.Now()
	var out bytes.Buffer
	err := e.run(ctx, e.cfg.PageTimeout, e.limits(), dir, &out, e.tesseract, img, "stdout", "-l", e.cfg.Languages, "--psm", "3")
	e.took("recognize", start)
	if err != nil {
		return "", err
	}
	return cleanText(out.Bytes(), maxPageText), nil
}

func (e *Engine) took(step string, start time.Time) {
	if e.observe != nil {
		e.observe(step, time.Since(start))
	}
}

// cleanText is what tesseract printed, made text a model reads: valid
// UTF-8, no control characters but newlines (its form feed between pages
// among them), no trailing spaces, at most one empty line in a row, and at
// most max bytes, cut on a rune.
func cleanText(raw []byte, max int) string {
	s := strings.ToValidUTF8(string(raw), "�")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case unicode.IsControl(r), r == '\uFEFF':
			return -1
		}
		return r
	}, s)
	var lines []string
	blank := 0
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64<<10), len(s)+1)
	for sc.Scan() {
		l := strings.TrimRightFunc(sc.Text(), unicode.IsSpace)
		if l == "" {
			blank++
			continue
		}
		if blank > 0 && len(lines) > 0 {
			lines = append(lines, "")
		}
		blank = 0
		lines = append(lines, l)
	}
	out := strings.Join(lines, "\n")
	if len(out) > max {
		n := max
		for n > 0 && !utf8.RuneStart(out[n]) {
			n--
		}
		out = out[:n]
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(s)
}

func joinInts(ns []int) string {
	ss := make([]string, len(ns))
	for i, n := range ns {
		ss[i] = strconv.Itoa(n)
	}
	if len(ss) > 10 {
		ss = append(slices.Clone(ss[:10]), "…")
	}
	return strings.Join(ss, ", ")
}
