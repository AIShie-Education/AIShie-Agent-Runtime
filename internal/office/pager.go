package office

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/sandbox"
)

// Pager cuts PDFs with poppler's programs: a range of pages (pdftocairo),
// which a long PDF is given to a model in, and pages picked out (pdfseparate
// and pdfunite), which OCR reads. It is safe for concurrent use.
type Pager struct {
	cfg                                      Config
	pdftocairo, pdfseparate, pdfunite, prlim string
}

// pageTimeout bounds one cut: they take a second or two.
const pageTimeout = time.Minute

// NewPager finds the programs, as cfg names them or on PATH; without them it
// is ErrUnavailable, saying which are missing, and PDFs are given whole.
func NewPager(cfg Config) (*Pager, error) {
	cfg = cfg.WithDefaults()
	found, missing := sandbox.Find(map[string]string{
		"pdftocairo": cfg.PDFToCairo, "pdfseparate": cfg.PDFSeparate, "pdfunite": cfg.PDFUnite, "prlimit": cfg.Prlimit,
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s not installed", ErrUnavailable, strings.Join(sorted(missing), ", "))
	}
	return &Pager{cfg: cfg, pdftocairo: found["pdftocairo"], pdfseparate: found["pdfseparate"], pdfunite: found["pdfunite"],
		prlim: found["prlimit"]}, nil
}

// Range is the PDF of pages first to last of pdf, as pdftocairo draws them
// again: text as text, pictures as they were, each font once (the pages
// pdfseparate cuts each carry every font the file has, and ten of them
// joined are ten times the whole). Pages past the last are not there.
func (p *Pager) Range(ctx context.Context, pdf []byte, first, last int) ([]byte, error) {
	if first < 1 || last < first {
		return nil, fmt.Errorf("office: pages %d to %d are no range", first, last)
	}
	return p.cut(ctx, pdf, func(dir, in string) (string, error) {
		out := filepath.Join(dir, "range.pdf")
		return out, p.run(ctx, dir, p.pdftocairo, "-pdf", "-f", strconv.Itoa(first), "-l", strconv.Itoa(last), in, out)
	})
}

// Pick is a PDF of the pages given, in their order, for OCR: each page as
// pdfseparate cuts it, joined by pdfunite. Its fonts are copied once a
// page, which makes it larger than its pages need, and only rendered.
func (p *Pager) Pick(ctx context.Context, pdf []byte, pages []int) ([]byte, error) {
	if len(pages) == 0 {
		return nil, errors.New("office: no pages to pick")
	}
	return p.cut(ctx, pdf, func(dir, in string) (string, error) {
		args := make([]string, 0, len(pages)+1)
		for i, n := range pages {
			if n < 1 {
				return "", fmt.Errorf("office: page %d is no page", n)
			}
			one := filepath.Join(dir, "p"+strconv.Itoa(i)+".pdf")
			if err := p.run(ctx, dir, p.pdfseparate, "-f", strconv.Itoa(n), "-l", strconv.Itoa(n), in, one); err != nil {
				return "", err
			}
			args = append(args, one)
		}
		out := filepath.Join(dir, "picked.pdf")
		return out, p.run(ctx, dir, p.pdfunite, append(args, out)...)
	})
}

// cut writes pdf to a private directory, has make cut it there, and reads
// what it made.
func (p *Pager) cut(ctx context.Context, pdf []byte, make func(dir, in string) (string, error)) ([]byte, error) {
	dir, err := os.MkdirTemp(p.cfg.TempDir, "aishie-pages-")
	if err != nil {
		return nil, fmt.Errorf("office: a private directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, pdf, 0o600); err != nil {
		return nil, fmt.Errorf("office: writing the file: %w", err)
	}
	out, err := make(dir, in)
	var pe *sandbox.ProgramError
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, sandbox.ErrTimeout):
		return nil, ErrTimeout
	case errors.As(err, &pe):
		return nil, fmt.Errorf("%w: %s ended with status %d", ErrMalformed, filepath.Base(pe.Name), pe.Status)
	case err != nil:
		return nil, err
	}
	st, err := os.Stat(out)
	switch {
	case err != nil || st.Size() == 0:
		return nil, fmt.Errorf("%w: its pages made nothing", ErrMalformed)
	case st.Size() > maxOutput:
		return nil, ErrTooLarge
	}
	return os.ReadFile(out) //nolint:gosec // a path of the pager's own making.
}

// run runs one of poppler's programs as package sandbox holds it.
func (p *Pager) run(ctx context.Context, dir, name string, args ...string) error {
	return sandbox.Run(ctx, sandbox.Cmd{Prlimit: p.prlim, Path: name, Args: args, Dir: dir, Timeout: pageTimeout,
		Limits: sandbox.Limits{Memory: int64(p.cfg.MemoryMB) << 20, CPU: pageTimeout, FileSize: 256 << 20}})
}

func sorted(ss []string) []string {
	slices.Sort(ss)
	return ss
}
