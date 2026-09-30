package office

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
)

// The converter's tests run a fake soffice, a shell script written for each
// test, under the real prlimit: what LibreOffice does is the test's, and how
// it is run (its arguments, profile, limits, environment, directory, the
// kill at its timeout) is the converter's own.

// fakeSoffice is a soffice that answers --version, and otherwise reads its
// arguments as LibreOffice does ($in, $out the output directory, $filter,
// $profile the profile's directory), writes them to log ($log), and runs
// convert, a shell snippet.
func fakeSoffice(log, convert string) string {
	return `#!/bin/sh
log=` + log + `
case "$1" in --version) echo 'LibreOffice 9.9.9.9 fake'; exit 0;; esac
in=; out=; filter=; profile=
while [ $# -gt 0 ]; do
  case "$1" in
    --outdir) out=$2; shift;;
    --convert-to) filter=$2; shift;;
    -env:UserInstallation=file://*) profile=${1#-env:UserInstallation=file://};;
    -*) ;;
    *) in=$1;;
  esac
  shift
done
printf '%s\n' "$filter" > ` + log + `/filter
printf '%s\n' "$in" > ` + log + `/in
cp "$profile/user/registrymodifications.xcu" ` + log + `/profile.xcu 2>/dev/null
cp "$in" ` + log + `/input
` + convert + "\n"
}

// fakeConverter is a converter on a fake soffice and the real prlimit, or
// the test is skipped where there is none.
func fakeConverter(t *testing.T, cfg Config, convert string) (*Converter, string) {
	t.Helper()
	if _, err := exec.LookPath("prlimit"); err != nil {
		t.Skip("prlimit (util-linux) is not installed")
	}
	bin, log := t.TempDir(), t.TempDir()
	cfg.Soffice = filepath.Join(bin, "soffice")
	if err := os.WriteFile(cfg.Soffice, []byte(fakeSoffice(log, convert)), 0o700); err != nil {
		t.Fatal(err)
	}
	if cfg.TempDir == "" {
		cfg.TempDir = t.TempDir()
	}
	c, err := NewConverter(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, log
}

func readLog(t *testing.T, log, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(log, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// empty fails unless dir is empty: every private directory gone.
func empty(t *testing.T, dir string) {
	t.Helper()
	left, err := os.ReadDir(dir)
	if err != nil || len(left) != 0 {
		t.Errorf("left behind in the temporary directory: %v %v", left, err)
	}
}

// TestConvert: a presentation is converted to PDF by LibreOffice's Impress
// filter, a page a slide, hidden ones too, at most MaxPages; a document by
// Writer's; an older presentation to PowerPoint's format; the file is given
// under its format's extension, and the profile LibreOffice starts from
// blocks links, has a proxy that is not there, and no active content or
// macros. The pages of a PDF made are counted, and the private directory
// is gone after.
func TestConvert(t *testing.T) {
	fixtures := t.TempDir()
	three := doctexttest.PDF(doctexttest.PDFPage{Lines: []string{"one"}}, doctexttest.PDFPage{Lines: []string{"two"}}, doctexttest.PDFPage{Lines: []string{"three"}})
	if err := os.WriteFile(filepath.Join(fixtures, "made"), three, 0o600); err != nil {
		t.Fatal(err)
	}
	c, log := fakeConverter(t, Config{MaxPages: 3}, `case "$filter" in pdf*) ext=pdf;; pptx*) ext=pptx;; esac; cp `+fixtures+`/made "$out/in.$ext"`)
	if c.Describe() != "LibreOffice 9.9.9.9" {
		t.Errorf("described as %q", c.Describe())
	}
	deck := doctexttest.PPTX(doctexttest.Slide{Title: "x"})
	out, err := c.Convert(t.Context(), deck, Format{"pptx", Slides, true}, ToPDF)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Data, three) || out.Pages != 3 || !out.Capped {
		t.Errorf("output of %d bytes, %d pages, capped %v", len(out.Data), out.Pages, out.Capped)
	}
	if got := readLog(t, log, "input"); got != string(deck) {
		t.Error("LibreOffice was not given the file")
	}
	if in := readLog(t, log, "in"); !strings.HasSuffix(strings.TrimSpace(in), "/in.pptx") {
		t.Errorf("the file is given as %q", in)
	}
	filter := readLog(t, log, "filter")
	for _, want := range []string{`pdf:impress_pdf_Export:{`, `"PageRange":{"type":"string","value":"1-3"}`,
		`"ExportHiddenSlides":{"type":"boolean","value":"true"}`, `"ExportNotesPages":{"type":"boolean","value":"false"}`,
		`"MaxImageResolution":{"type":"long","value":"300"}`} {
		if !strings.Contains(filter, want) {
			t.Errorf("the filter %q lacks %q", filter, want)
		}
	}
	prof := readLog(t, log, "profile.xcu")
	for _, want := range []string{`"BlockUntrustedRefererLinks" oor:op="fuse"><value>true`, `"DisableActiveContent" oor:op="fuse"><value>true`,
		`"DisableMacrosExecution" oor:op="fuse"><value>true`, `"ooInetProxyType" oor:op="fuse"><value>2`,
		`"ooInetHTTPProxyName" oor:op="fuse"><value>127.0.0.1`, `"ooInetHTTPSProxyPort" oor:op="fuse"><value>9`} {
		if !strings.Contains(prof, want) {
			t.Errorf("the profile lacks %s", want)
		}
	}

	if _, err := c.Convert(t.Context(), []byte("{\\rtf1 x}"), Format{"rtf", Document, false}, ToPDF); err != nil {
		t.Fatal(err)
	}
	if filter := readLog(t, log, "filter"); !strings.HasPrefix(filter, "pdf:writer_pdf_Export:{") || strings.Contains(filter, "Hidden") {
		t.Errorf("a document's filter %q", filter)
	}
	if in := readLog(t, log, "in"); !strings.HasSuffix(strings.TrimSpace(in), "/in.rtf") {
		t.Errorf("the file is given as %q", in)
	}
	if _, err := c.Convert(t.Context(), cfbMagic, Format{"ppt", Slides, false}, ToPPTX); err != nil {
		t.Fatal(err)
	}
	if filter := readLog(t, log, "filter"); strings.TrimSpace(filter) != "pptx:Impress MS PowerPoint 2007 XML" {
		t.Errorf("an older presentation's filter %q", filter)
	}
	if _, err := c.Convert(t.Context(), deck, Format{"../x", Slides, true}, ToPDF); err == nil {
		t.Error("an extension that is a path was taken")
	}
	if _, err := c.Convert(t.Context(), []byte("<html><img src=http://example.invalid/x></html>"), Format{"doc", Document, false}, ToPDF); !errors.Is(err, ErrMalformed) {
		t.Errorf("a web page given as a Word file: %v", err)
	}
	empty(t, c.cfg.TempDir)
}

// TestConvertFails: a file LibreOffice makes nothing of (it ends well all
// the same), or one it fails on, is ErrMalformed; one that takes longer
// than the timeout is ErrTimeout, its process group killed; one whose PDF
// does not read is ErrMalformed. Nothing is left behind.
func TestConvertFails(t *testing.T) {
	deck := doctexttest.PPTX(doctexttest.Slide{Title: "x"})
	f := Format{"pptx", Slides, true}
	c, _ := fakeConverter(t, Config{}, `echo 'Error: source file could not be loaded' >&2`)
	if _, err := c.Convert(t.Context(), deck, f, ToPDF); !errors.Is(err, ErrMalformed) {
		t.Errorf("nothing made: %v", err)
	}
	empty(t, c.cfg.TempDir)

	c, _ = fakeConverter(t, Config{}, `exit 81`)
	if _, err := c.Convert(t.Context(), deck, f, ToPDF); !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "status 81") {
		t.Errorf("a failure: %v", err)
	}
	c, _ = fakeConverter(t, Config{}, `echo 'not a PDF' > "$out/in.pdf"`)
	if _, err := c.Convert(t.Context(), deck, f, ToPDF); !errors.Is(err, ErrMalformed) {
		t.Errorf("a PDF that does not read: %v", err)
	}

	pids := t.TempDir()
	c, _ = fakeConverter(t, Config{Timeout: 5 * time.Second}, `sleep 60 & echo $! > `+pids+`/sleeper; wait`)
	c.cfg.Timeout = 300 * time.Millisecond
	start := time.Now()
	if _, err := c.Convert(t.Context(), deck, f, ToPDF); !errors.Is(err, ErrTimeout) {
		t.Errorf("a conversion past its time: %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s: the timeout did not end it", took)
	}
	if raw, err := os.ReadFile(filepath.Join(pids, "sleeper")); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		deadline := time.Now().Add(5 * time.Second)
		for pid > 0 && alive(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if pid > 0 && alive(pid) {
			t.Errorf("LibreOffice's child %d outlived its kill", pid)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Convert(ctx, deck, f, ToPDF); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled conversion: %v", err)
	}
	empty(t, c.cfg.TempDir)
}

// TestConvertRunsApart: LibreOffice runs in the file's private directory,
// with nothing of the runtime's environment but a PATH, its HOME and TMPDIR
// there, the headless drawing it needs, a lower priority, and prlimit's
// limits on it.
func TestConvertRunsApart(t *testing.T) {
	t.Setenv("AISHIE_TEST_SECRET", "ais_Secret0123456789")
	c, log := fakeConverter(t, Config{MemoryMB: 1024}, `{ echo "cwd=$(pwd)"; env; echo "nice=$(cut -d' ' -f19 /proc/self/stat)"; `+
		`grep -E '^Max (address space|file size|open files|core file size|cpu time)' /proc/self/limits; } > "$log/lot"; exit 1`)
	if _, err := c.Convert(t.Context(), []byte("PK\x03\x04"), Format{"docx", Document, true}, ToPDF); !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}
	lot := readLog(t, log, "lot")
	if strings.Contains(lot, "AISHIE_TEST_SECRET") || strings.Contains(lot, "ais_Secret") {
		t.Errorf("the runtime's environment reached LibreOffice:\n%s", lot)
	}
	private := c.cfg.TempDir + "/aishie-office-"
	for _, want := range []string{"SAL_USE_VCLPLUGIN=svp", "LC_ALL=C", "cwd=" + private, "HOME=" + private, "TMPDIR=" + private,
		"Max address space " + strconv.Itoa(1024<<20), "Max file size " + strconv.Itoa(256<<20), "Max open files 1024", "Max core file size 0"} {
		if !strings.Contains(strings.Join(strings.Fields(lot), " "), want) {
			t.Errorf("LibreOffice's lot lacks %q:\n%s", want, lot)
		}
	}
	if !strings.Contains(lot, "nice=10") && !strings.Contains(lot, "nice=19") {
		t.Errorf("LibreOffice runs at the runtime's own priority:\n%s", lot)
	}
	empty(t, c.cfg.TempDir)
}

// alive reports whether a process is still there (and not a zombie).
func alive(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	f := strings.Fields(string(raw))
	return len(f) > 2 && f[2] != "Z"
}

func TestNewConverterUnavailable(t *testing.T) {
	_, err := NewConverter(t.Context(), Config{Soffice: filepath.Join(t.TempDir(), "soffice")})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "soffice not installed") {
		t.Errorf("no soffice: %v", err)
	}
	if _, err := NewConverter(t.Context(), Config{Mode: "sometimes"}); err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("a bad mode: %v", err)
	}
	if _, err := NewPager(Config{PDFToCairo: filepath.Join(t.TempDir(), "pdftocairo")}); !errors.Is(err, ErrUnavailable) ||
		!strings.Contains(err.Error(), "pdftocairo not installed") {
		t.Errorf("no pdftocairo: %v", err)
	}
}

func TestConfigCheck(t *testing.T) {
	if err := (Config{}).Check(); err != nil {
		t.Errorf("the defaults: %v", err)
	}
	for _, c := range []Config{{Mode: "yes"}, {MaxPages: 5000}, {Timeout: time.Second}, {Concurrency: 9}, {MemoryMB: 100}} {
		if err := c.Check(); err == nil {
			t.Errorf("%+v passes", c)
		}
	}
	d := Config{}.WithDefaults()
	if d.Mode != ModeAuto || d.Timeout != DefaultTimeout || d.MaxPages != DefaultMaxPages || d.Concurrency != 1 || d.CacheBytes != DefaultCacheBytes {
		t.Errorf("defaults %+v", d)
	}
}
