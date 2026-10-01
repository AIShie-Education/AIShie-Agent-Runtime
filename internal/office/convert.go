package office

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/sandbox"
)

// Converter runs LibreOffice on one file at a time (Convert). It is safe
// for concurrent use: each file has a directory and a profile of its own.
type Converter struct {
	cfg              Config
	soffice, prlimit string
	// version is LibreOffice's own, "LibreOffice 25.2.3.2".
	version string
}

// NewConverter finds soffice and prlimit, as cfg names them or on PATH,
// and asks LibreOffice its version. Without them it is ErrUnavailable,
// saying what is missing: conversion is then off.
func NewConverter(ctx context.Context, cfg Config) (*Converter, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	found, missing := sandbox.Find(map[string]string{"soffice": cfg.Soffice, "prlimit": cfg.Prlimit})
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s not installed", ErrUnavailable, strings.Join(sorted(missing), ", "))
	}
	c := &Converter{cfg: cfg, soffice: found["soffice"], prlimit: found["prlimit"]}
	dir, err := os.MkdirTemp(cfg.TempDir, "aishie-office-")
	if err != nil {
		return nil, fmt.Errorf("%w: no temporary directory: %w", ErrUnavailable, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	var out bytes.Buffer
	if err := c.run(ctx, 30*time.Second, dir, &out, "--version"); err != nil {
		return nil, fmt.Errorf("%w: soffice --version: %w", ErrUnavailable, err)
	}
	f := strings.Fields(out.String())
	if len(f) < 2 || !strings.Contains(f[0], "Office") {
		return nil, fmt.Errorf("%w: soffice --version does not say it is LibreOffice", ErrUnavailable)
	}
	c.version = f[0] + " " + f[1]
	return c, nil
}

// Describe names the converter: LibreOffice and its version.
func (c *Converter) Describe() string { return c.version }

// WithTimeout is a copy of c, the same LibreOffice in the same sandbox,
// whose conversions may each take timeout (at least 5 s): the renditions'
// own, beside the conversions for the models.
func (c *Converter) WithTimeout(timeout time.Duration) *Converter {
	cp := *c
	cp.cfg.Timeout = max(timeout, 5*time.Second)
	return &cp
}

// Config is the converter's configuration, its defaults filled in.
func (c *Converter) Config() Config { return c.cfg }

// profile is the settings a fresh LibreOffice profile starts with, so that
// a hostile file reaches nothing outside itself: links to anything outside
// the document are blocked, whatever it links (a picture on a web server
// or in a local file); should anything be fetched all the same, its proxy
// is one nothing listens at; active content (OLE objects, DDE links) and
// macros are off. Links a document keeps (a section linked to a file, an
// external reference) are not updated either: LibreOffice's default is to
// ask, which a conversion answers no.
const profile = `<?xml version="1.0" encoding="UTF-8"?>
<oor:items xmlns:oor="http://openoffice.org/2001/registry" xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="BlockUntrustedRefererLinks" oor:op="fuse"><value>true</value></prop></item>
<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="DisableActiveContent" oor:op="fuse"><value>true</value></prop></item>
<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="DisableMacrosExecution" oor:op="fuse"><value>true</value></prop></item>
<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop></item>
<item oor:path="/org.openoffice.Inet/Settings"><prop oor:name="ooInetProxyType" oor:op="fuse"><value>2</value></prop></item>
<item oor:path="/org.openoffice.Inet/Settings"><prop oor:name="ooInetNoProxy" oor:op="fuse"><value></value></prop></item>
<item oor:path="/org.openoffice.Inet/Settings"><prop oor:name="ooInetHTTPProxyName" oor:op="fuse"><value>127.0.0.1</value></prop></item>
<item oor:path="/org.openoffice.Inet/Settings"><prop oor:name="ooInetHTTPProxyPort" oor:op="fuse"><value>9</value></prop></item>
<item oor:path="/org.openoffice.Inet/Settings"><prop oor:name="ooInetHTTPSProxyName" oor:op="fuse"><value>127.0.0.1</value></prop></item>
<item oor:path="/org.openoffice.Inet/Settings"><prop oor:name="ooInetHTTPSProxyPort" oor:op="fuse"><value>9</value></prop></item>
<item oor:path="/org.openoffice.Inet/Settings"><prop oor:name="ooInetFTPProxyName" oor:op="fuse"><value>127.0.0.1</value></prop></item>
<item oor:path="/org.openoffice.Inet/Settings"><prop oor:name="ooInetFTPProxyPort" oor:op="fuse"><value>9</value></prop></item>
</oor:items>
`

// maxImageDPI is the resolution the pictures of a PDF made are brought
// down to, when they have more: enough for OCR, and far smaller.
const maxImageDPI = 300

// filter is the export filter and its options LibreOffice converts a file
// of family to target with. A presentation's PDF has a page for every
// slide, the hidden ones too, so that its pages are numbered as the
// runtime's text numbers the slides; neither has notes pages.
func (c *Converter) filter(family Family, to Target) string {
	switch to {
	case ToPPTX:
		return "pptx:Impress MS PowerPoint 2007 XML"
	case ToXLSX:
		return "xlsx:Calc MS Excel 2007 XML"
	}
	return pdfFilter(family, c.cfg.MaxPages)
}

// pdfFilter is the PDF export filter of LibreOffice's application that
// opens a file of family (Writer's, Impress's, Calc's or Draw's), and its
// options: pictures brought down to maxImageDPI, and, with pages above 0,
// only the first pages pages. A presentation's PDF has a page for every
// slide, the hidden ones too, and no notes pages.
func pdfFilter(family Family, pages int) string {
	var opts []string
	if pages > 0 {
		opts = append(opts, `"PageRange":{"type":"string","value":"1-`+strconv.Itoa(pages)+`"}`)
	}
	opts = append(opts,
		`"ReduceImageResolution":{"type":"boolean","value":"true"}`,
		`"MaxImageResolution":{"type":"long","value":"`+strconv.Itoa(maxImageDPI)+`"}`)
	name := "writer_pdf_Export"
	switch family {
	case Slides:
		name = "impress_pdf_Export"
		opts = append(opts, `"ExportHiddenSlides":{"type":"boolean","value":"true"}`, `"ExportNotesPages":{"type":"boolean","value":"false"}`)
	case Workbook:
		name = "calc_pdf_Export"
	case Drawing:
		name = "draw_pdf_Export"
	}
	return "pdf:" + name + ":{" + strings.Join(opts, ",") + "}"
}

// Convert converts data, a file of format f, to target, within the
// configured timeout and ctx. Its error is ErrMalformed (LibreOffice could
// not open or convert it), ErrTooLarge, ErrTimeout, or ctx's own.
func (c *Converter) Convert(ctx context.Context, data []byte, f Format, to Target) (*Output, error) {
	return c.convert(ctx, data, f, string(to), c.filter(f.Family, to), maxOutput)
}

// Rendition converts data, a file of format f of any family, to a PDF of
// every page it has, as a file's PDF rendition is made (package
// rendition): no page left out, whatever Config.MaxPages says, and no
// larger than maxBytes (ErrTooLarge past it), within the configured
// timeout and ctx. Its Pages are the PDF's, counted. Its errors are
// Convert's.
func (c *Converter) Rendition(ctx context.Context, data []byte, f Format, maxBytes int64) (*Output, error) {
	out, err := c.convert(ctx, data, f, string(ToPDF), pdfFilter(f.Family, 0), maxBytes)
	if out != nil {
		out.Capped = false
	}
	return out, err
}

// convert has LibreOffice convert data, of format f, with filter, to a file
// of the extension ext, of at most maxOut bytes.
func (c *Converter) convert(ctx context.Context, data []byte, f Format, ext, filter string, maxOut int64) (*Output, error) {
	if !validExt(f.Ext) || !validExt(ext) {
		return nil, fmt.Errorf("office: %q is not a format's extension", f.Ext)
	}
	if !container(data) {
		// LibreOffice would take it for text, or a web page, and make a
		// PDF of that.
		return nil, fmt.Errorf("%w: it is not an Office file", ErrMalformed)
	}
	dir, err := os.MkdirTemp(c.cfg.TempDir, "aishie-office-")
	if err != nil {
		return nil, fmt.Errorf("office: a private directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	prof := filepath.Join(dir, "profile")
	for _, d := range []string{filepath.Join(prof, "user"), filepath.Join(dir, "out")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("office: a private directory: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(prof, "user", "registrymodifications.xcu"), []byte(profile), 0o600); err != nil {
		return nil, fmt.Errorf("office: the profile: %w", err)
	}
	in := filepath.Join(dir, "in."+f.Ext)
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, fmt.Errorf("office: writing the file: %w", err)
	}
	outDir := filepath.Join(dir, "out")
	err = c.run(ctx, c.cfg.Timeout, dir, nil,
		"-env:UserInstallation="+(&url.URL{Scheme: "file", Path: prof}).String(),
		"--headless", "--invisible", "--norestore", "--nolockcheck", "--nologo", "--nodefault", "--nofirststartwizard",
		"--convert-to", filter, "--outdir", outDir, in)
	var pe *sandbox.ProgramError
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, sandbox.ErrTimeout):
		return nil, ErrTimeout
	case errors.As(err, &pe):
		return nil, fmt.Errorf("%w: LibreOffice ended with status %d", ErrMalformed, pe.Status)
	case err != nil:
		return nil, err
	}
	// LibreOffice ends well when it could not open the file too, and makes
	// nothing.
	out := filepath.Join(outDir, "in."+ext)
	st, err := os.Stat(out)
	switch {
	case err != nil || st.Size() == 0:
		return nil, fmt.Errorf("%w: LibreOffice made nothing of it", ErrMalformed)
	case st.Size() > maxOut:
		return nil, ErrTooLarge
	}
	b, err := os.ReadFile(out) //nolint:gosec // a path of the converter's own making.
	if err != nil {
		return nil, fmt.Errorf("office: reading what LibreOffice made: %w", err)
	}
	o := &Output{Data: b}
	if ext == string(ToPDF) {
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		n, err := doctext.PDFPages(pctx, b, doctext.Limits{MaxInflated: max(maxOut, doctext.DefaultLimits().MaxInflated)})
		if err != nil {
			return nil, fmt.Errorf("%w: its PDF does not read: %w", ErrMalformed, err)
		}
		o.Pages, o.Capped = n, n >= c.cfg.MaxPages
	}
	return o, nil
}

// run runs soffice with args in dir, as package sandbox holds a program:
// its memory, twice its time in CPU (LibreOffice's threads draw on it
// together), 256 MB a file, 1,024 open files (fonts and libraries), the
// headless drawing it needs and nothing else of the runtime's.
func (c *Converter) run(ctx context.Context, timeout time.Duration, dir string, out io.Writer, args ...string) error {
	return sandbox.Run(ctx, sandbox.Cmd{
		Prlimit: c.prlimit, Path: c.soffice, Args: args, Dir: dir, Env: []string{"SAL_USE_VCLPLUGIN=svp"}, Timeout: timeout,
		Limits: sandbox.Limits{Memory: int64(c.cfg.MemoryMB) << 20, CPU: 2 * timeout, FileSize: 256 << 20, Files: 1024},
		Stdout: out,
	})
}

// IsContainer reports whether data is held as an Office file is: a zip
// (Office Open XML, OpenDocument), a Compound File Binary file (the older
// formats), or RTF. LibreOffice converts nothing else (ErrMalformed).
func IsContainer(data []byte) bool { return container(data) }

// container reports whether data is held as an Office file is: a zip
// (Office Open XML, OpenDocument), a Compound File Binary file (the older
// formats), or RTF.
func container(data []byte) bool {
	return bytes.HasPrefix(data, []byte("PK\x03\x04")) || bytes.HasPrefix(data, cfbMagic) || bytes.HasPrefix(data, []byte(`{\rtf`))
}

// validExt reports whether ext is a format's extension: letters alone, so
// that it names no path.
func validExt(ext string) bool {
	if ext == "" || len(ext) > 8 {
		return false
	}
	return strings.IndexFunc(ext, func(r rune) bool { return r < 'a' || r > 'z' }) < 0
}
