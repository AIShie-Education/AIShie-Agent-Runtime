package ocr

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The engine's tests run fake programs, shell scripts written for each
// test, under the real prlimit: what the programs do is the test's, and
// how they are run (their limits, their environment, their directory, the
// kill at their timeout) is the engine's own.

// fakeLangs is what the fake tesseract lists.
const fakeLangs = `printf 'List of available languages in "/fake/" (4):\nchi_sim\nchi_tra\neng\nosd\n'`

// fakeTesseract is a tesseract that answers --version and --list-langs,
// and runs page, a shell snippet whose $1 is the image, on anything else.
func fakeTesseract(page string) string {
	return "#!/bin/sh\ncase \"$1\" in\n--version) echo 'tesseract 0.0-fake'; echo ' leptonica'; exit 0;;\n--list-langs) " +
		fakeLangs + "; exit 0;;\nesac\n" + page + "\n"
}

// fakePDFToPPM renders a PDF of pages pages: page.png in the prefix's
// place, holding its page's number, and pdftoppm's own refusal of a page
// past the last. page, when not "", runs first, with $N the page.
func fakePDFToPPM(pages int, page string) string {
	return fmt.Sprintf(`#!/bin/sh
N=0; while [ $# -gt 2 ]; do if [ "$1" = -f ]; then N=$2; fi; shift; done
prefix=$2
if [ "$N" -gt %d ]; then echo "Wrong page range given: the first page ($N) can not be after the last page (%d)." >&2; exit 99; fi
%s
echo "page $N" > "$prefix.png"
`, pages, pages, page)
}

// fakeEngine is an engine on scripts for tesseract and pdftoppm, and the
// real prlimit, or the test is skipped where there is none.
func fakeEngine(t *testing.T, cfg Config, tesseract, pdftoppm string) *Engine {
	t.Helper()
	if _, err := exec.LookPath("prlimit"); err != nil {
		t.Skip("prlimit (util-linux) is not installed")
	}
	bin := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg.Tesseract = write("tesseract", tesseract)
	cfg.PDFToPPM = write("pdftoppm", pdftoppm)
	if cfg.TempDir == "" {
		cfg.TempDir = t.TempDir()
	}
	e, err := NewEngine(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// readsPage is a tesseract that reads what the fake pdftoppm drew.
const readsPage = `echo "text of $(cat "$1")"`

// empty is a private directory that must be left empty.
func empty(t *testing.T, dir string) {
	t.Helper()
	left, err := os.ReadDir(dir)
	if err != nil || len(left) != 0 {
		t.Errorf("left behind in %s: %v %v", dir, left, err)
	}
}

// TestEngineReadsPages: a PDF's pages are rendered and recognized one at a
// time, up to MaxPages, each under its heading, progress told after each,
// the rest said to be left out, and nothing left in the temporary
// directory; a PDF whose pages were not known is read until pdftoppm says
// the last is past.
func TestEngineReadsPages(t *testing.T) {
	e := fakeEngine(t, Config{MaxPages: 2}, fakeTesseract(readsPage), fakePDFToPPM(3, ""))
	var told []string
	res, err := e.Recognize(t.Context(), []byte("%PDF-1.4"), PDF, 3, func(done, of int) { told = append(told, fmt.Sprint(done, "/", of)) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "## Page 1\ntext of page 1\n\n## Page 2\ntext of page 2" || res.Pages != 2 || res.Of != 3 {
		t.Errorf("result %+v", res)
	}
	if len(res.Sections) != 2 || res.Sections[1].N != 2 || !strings.HasPrefix(res.Text[res.Sections[1].Offset:], "## Page 2") {
		t.Errorf("sections %+v", res.Sections)
	}
	if strings.Join(told, " ") != "1/2 2/2" || strings.Join(res.Notes, "; ") != "only its first 2 pages of 3 were recognized" {
		t.Errorf("progress %v, notes %v", told, res.Notes)
	}
	empty(t, e.cfg.TempDir)

	e = fakeEngine(t, Config{}, fakeTesseract(readsPage), fakePDFToPPM(3, ""))
	res, err = e.Recognize(t.Context(), []byte("%PDF-1.4"), PDF, 0, nil)
	if err != nil || res.Pages != 3 || res.Of != 0 || len(res.Notes) != 0 || !strings.HasSuffix(res.Text, "text of page 3") {
		t.Errorf("pages not known: %+v %v", res, err)
	}
	if e.Describe() != "tesseract 0.0-fake chi_sim+chi_tra+eng 300dpi" {
		t.Errorf("described as %q", e.Describe())
	}
}

// TestEngineRunsProgramsApart: a program runs in the file's private
// directory, with nothing of the runtime's environment but a PATH, its
// HOME and TMPDIR there, one thread, a lower priority, and prlimit's
// limits on it; and the directory is gone after.
func TestEngineRunsProgramsApart(t *testing.T) {
	t.Setenv("AISHIE_TEST_SECRET", "ais_Secret0123456789")
	page := `echo "cwd=$(pwd)"; env; echo "nice=$(cut -d' ' -f19 /proc/self/stat)"; ` +
		`grep -E '^Max (address space|file size|open files|core file size|cpu time)' /proc/self/limits`
	e := fakeEngine(t, Config{MemoryMB: 256}, fakeTesseract(page), fakePDFToPPM(1, ""))
	res, err := e.Recognize(t.Context(), []byte("%PDF-1.4"), PDF, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := res.Text
	if strings.Contains(text, "AISHIE_TEST_SECRET") || strings.Contains(text, "ais_Secret") {
		t.Errorf("the runtime's environment reached the program:\n%s", text)
	}
	for _, want := range []string{"OMP_THREAD_LIMIT=1", "LC_ALL=C", "cwd=" + e.cfg.TempDir + "/aishie-ocr-", "HOME=" + e.cfg.TempDir + "/aishie-ocr-",
		"TMPDIR=" + e.cfg.TempDir + "/aishie-ocr-", "Max address space " + strconv.Itoa(256<<20), "Max file size " + strconv.Itoa(256<<20),
		"Max open files 256", "Max core file size 0"} {
		if !strings.Contains(squeezeSpaces(text), want) {
			t.Errorf("the program's lot lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "nice=10") && !strings.Contains(text, "nice=19") {
		t.Errorf("the program runs at the runtime's own priority:\n%s", text)
	}
	if n := strings.Count(text, "\n") + 1; n > 20 {
		// PATH, HOME, TMPDIR, OMP_THREAD_LIMIT, LC_ALL, and the shell's own.
		t.Logf("%d lines of environment", n)
	}
	empty(t, e.cfg.TempDir)
}

func squeezeSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestEnginePageTimeout: a page whose program outlives its time is killed,
// the whole of its process group, said so, and the pages after it are
// read; a file whose own time runs out keeps the pages read, said so.
func TestEnginePageTimeout(t *testing.T) {
	pids := t.TempDir()
	page := `if grep -q 'page 2' "$1"; then sleep 30 & echo $! > ` + pids + `/sleeper; wait; fi; ` + readsPage
	e := fakeEngine(t, Config{PageTimeout: 300 * time.Millisecond}, fakeTesseract(page), fakePDFToPPM(3, ""))
	start := time.Now()
	res, err := e.Recognize(t.Context(), []byte("%PDF-1.4"), PDF, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %s: the page's time did not end it", took)
	}
	if res.Pages != 3 || res.Failed != 1 || !strings.Contains(res.Text, "## Page 2\n[this page took longer to recognize") ||
		!strings.Contains(res.Text, "text of page 3") || !strings.Contains(strings.Join(res.Notes, "; "), "pages 2 took too long") {
		t.Errorf("result %+v", res)
	}
	if raw, err := os.ReadFile(filepath.Join(pids, "sleeper")); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		deadline := time.Now().Add(5 * time.Second)
		for pid > 0 && alive(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if pid > 0 && alive(pid) {
			t.Errorf("the page's child %d outlived its kill", pid)
		}
	}

	slow := `sleep 0.3; ` + readsPage
	e = fakeEngine(t, Config{}, fakeTesseract(slow), fakePDFToPPM(20, ""))
	ctx, cancel := context.WithTimeout(t.Context(), 800*time.Millisecond)
	defer cancel()
	res, err = e.Recognize(ctx, []byte("%PDF-1.4"), PDF, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pages == 0 || res.Pages >= 20 || !strings.Contains(strings.Join(res.Notes, "; "), "took longer than the runtime allows") {
		t.Errorf("the file's time spent: %+v", res)
	}
	empty(t, e.cfg.TempDir)
}

// alive reports whether a process is still there (and not a zombie).
func alive(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(raw))
	return len(fields) > 2 && fields[2] != "Z"
}

// TestEngineMemoryLimit: a program that asks for more memory than it may
// is refused it by the kernel, and its page is said not to be read.
func TestEngineMemoryLimit(t *testing.T) {
	page := `dd if=/dev/zero of=/dev/null bs=400M count=1 2>/dev/null || exit 1; ` + readsPage
	e := fakeEngine(t, Config{MemoryMB: 128}, fakeTesseract(page), fakePDFToPPM(1, ""))
	if _, err := e.Recognize(t.Context(), []byte("%PDF-1.4"), PDF, 1, nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("a page past the memory limit: %v", err)
	}
	e = fakeEngine(t, Config{MemoryMB: 1024}, fakeTesseract(page), fakePDFToPPM(1, ""))
	if res, err := e.Recognize(t.Context(), []byte("%PDF-1.4"), PDF, 1, nil); err != nil || res.Pages != 1 || res.Failed != 0 {
		t.Errorf("the same page within the limit: %+v %v", res, err)
	}
}

// TestEngineFailures: a PDF none of whose pages renders is ErrMalformed;
// one page that does not render is said so; an image past MaxPixels, or
// whose size cannot be read, is refused before any program runs.
func TestEngineFailures(t *testing.T) {
	e := fakeEngine(t, Config{}, fakeTesseract(readsPage), fakePDFToPPM(3, `exit 1`))
	if _, err := e.Recognize(t.Context(), []byte("%PDF-1.4"), PDF, 3, nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("no page renders: %v", err)
	}
	e = fakeEngine(t, Config{}, fakeTesseract(readsPage), fakePDFToPPM(3, `if [ "$N" = 2 ]; then exit 1; fi`))
	res, err := e.Recognize(t.Context(), []byte("%PDF-1.4"), PDF, 3, nil)
	if err != nil || res.Failed != 1 || !strings.Contains(res.Text, "## Page 2\n[this page could not be read]") ||
		!strings.Contains(strings.Join(res.Notes, "; "), "pages 2 could not be read") {
		t.Errorf("page 2 does not render: %+v %v", res, err)
	}

	ran := filepath.Join(t.TempDir(), "ran")
	e = fakeEngine(t, Config{MaxPixels: 1 << 20}, fakeTesseract(`touch `+ran+`; `+readsPage), fakePDFToPPM(1, ""))
	huge := pngHeader(100_000, 100_000)
	if _, err := e.Recognize(t.Context(), huge, Image, 0, nil); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a PNG of 100,000 pixels a side: %v", err)
	}
	if _, err := e.Recognize(t.Context(), pngHeader(2000, 2000), Image, 0, nil); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a PNG of 4 megapixels past 1: %v", err)
	}
	if _, err := e.Recognize(t.Context(), []byte("not an image"), Image, 0, nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("no image at all: %v", err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("tesseract ran on an image refused")
	}
	if res, err := e.Recognize(t.Context(), pngHeader(100, 100), Image, 0, nil); err != nil || res.Pages != 1 {
		t.Errorf("a small image: %+v %v", res, err)
	}
}

// pngHeader is the start of a PNG of w×h grey pixels: what DecodeConfig
// reads, its header's checksum right.
func pngHeader(w, h uint32) []byte {
	ihdr := []byte("IHDR")
	ihdr = binary.BigEndian.AppendUint32(ihdr, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 0, 0, 0, 0)
	b := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0d"), ihdr...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(ihdr))
}

// TestWebPSize reads the canvas of each kind of WebP from its header.
func TestWebPSize(t *testing.T) {
	riff := func(chunk string, body []byte) []byte {
		b := []byte("RIFF\x00\x00\x00\x00WEBP" + chunk + "\x00\x00\x00\x00")
		return append(b, append(body, make([]byte, 16)...)...)
	}
	vp8x := riff("VP8X", []byte{0, 0, 0, 0, 0x1f, 0x03, 0x00, 0xdf, 0x01, 0x00})   // 800×480
	vp8l := riff("VP8L", []byte{0x2f, 0x1f, 0xc0, 0x77, 0x00})                     // 32×480
	vp8 := riff("VP8 ", []byte{0, 0, 0, 0x9d, 0x01, 0x2a, 0x40, 0x01, 0xf0, 0x00}) // 320×240
	for _, tc := range []struct {
		data []byte
		w, h int
	}{{vp8x, 800, 480}, {vp8l, 32, 480}, {vp8, 320, 240}} {
		w, h, err := imageSize(tc.data)
		if err != nil || w != tc.w || h != tc.h {
			t.Errorf("%q: %d×%d %v, want %d×%d", tc.data[12:16], w, h, err, tc.w, tc.h)
		}
	}
	if _, _, err := imageSize(riff("ALPH", nil)); err == nil {
		t.Error("a WebP with no image chunk first has a size")
	}
}

// TestCleanText: what tesseract prints is made text a model reads.
func TestCleanText(t *testing.T) {
	raw := []byte("\ufeff第一章  \t\n\n\n\n第二\x01章\r\n\x0c\xff\xfe end   \n\n")
	if got := cleanText(raw, 1<<10); got != "第一章\n\n第二章\n\ufffd end" {
		t.Errorf("cleaned %q", got)
	}
	if got := cleanText([]byte("第一章第二章"), 7); got != "第一" {
		t.Errorf("cut %q", got)
	}
}

// TestEngineUnavailable: without its programs, or without tesseract's
// languages, OCR is off, saying what is missing.
func TestEngineUnavailable(t *testing.T) {
	_, err := NewEngine(t.Context(), Config{Tesseract: "/nonexistent/tesseract", PDFToPPM: "/nonexistent/pdftoppm"})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "tesseract, pdftoppm") {
		t.Errorf("no programs: %v", err)
	}
	if _, err := exec.LookPath("prlimit"); err != nil {
		t.Skip("prlimit (util-linux) is not installed")
	}
	bin := t.TempDir()
	tess := filepath.Join(bin, "tesseract")
	if err := os.WriteFile(tess, []byte("#!/bin/sh\ncase \"$1\" in --version) echo 'tesseract 0'; exit 0;; esac\nprintf 'List (1):\\neng\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = NewEngine(t.Context(), Config{Tesseract: tess, PDFToPPM: "/bin/true", TempDir: t.TempDir()})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no chi_sim, chi_tra data") {
		t.Errorf("no Chinese data: %v", err)
	}
	if _, err := NewEngine(t.Context(), Config{Languages: "../etc/passwd"}); err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("a language that is a path: %v", err)
	}
}
