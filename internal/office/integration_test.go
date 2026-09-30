package office

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
)

// realConverter is a Converter on the LibreOffice installed here, or the
// test is skipped: only when soffice or prlimit is not installed, and never
// with OFFICE_PDF_REQUIRED=1, as the image's test runs it.
func realConverter(t *testing.T) *Converter {
	t.Helper()
	c, err := NewConverter(t.Context(), Config{TempDir: t.TempDir()})
	if errors.Is(err, ErrUnavailable) {
		if os.Getenv("OFFICE_PDF_REQUIRED") == "1" {
			t.Fatalf("OFFICE_PDF_REQUIRED is set, and LibreOffice is not available: %v", err)
		}
		t.Skipf("LibreOffice is not installed: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// realPager is a Pager on poppler's programs installed here, or the test is
// skipped, as realConverter is.
func realPager(t *testing.T) *Pager {
	t.Helper()
	p, err := NewPager(Config{TempDir: t.TempDir()})
	if errors.Is(err, ErrUnavailable) {
		if os.Getenv("OFFICE_PDF_REQUIRED") == "1" {
			t.Fatalf("OFFICE_PDF_REQUIRED is set, and poppler's programs are not available: %v", err)
		}
		t.Skipf("poppler's programs are not installed: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// picture is a PNG of a dark bar on white, as a slide's screenshot is.
func picture(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 400, 300))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for y := 100; y < 140; y++ {
		for x := 40; x < 360; x++ {
			img.SetGray(x, y, color.Gray{})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pdfText is doctext's reading of a PDF, which must read.
func pdfText(t *testing.T, pdf []byte) *doctext.Result {
	t.Helper()
	res, err := doctext.Extract(context.Background(), pdf, doctext.PDF, doctext.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Unreadable != "" {
		t.Errorf("the PDF's text does not read (%s):\n%s", res.Unreadable, res.Text)
	}
	return res
}

// squeezed is s without its spaces, as a PDF's text may space characters.
func squeezed(s string) string { return strings.Join(strings.Fields(s), "") }

// lecture is a deck as a course's is: Chinese, a screenshot, speaker notes,
// a hidden slide, and a chart.
func lecture(t *testing.T) []byte {
	return doctexttest.PPTX(
		doctexttest.Slide{Title: "第三週：排序", Body: []doctexttest.Bullet{{Text: "合併排序 splits the list in two"}},
			Notes: "Ask who has seen quicksort.", Picture: picture(t)},
		doctexttest.Slide{Title: "Hidden aside", Hidden: true},
		doctexttest.Slide{Title: "Complexity", Chart: "n log n"},
	)
}

// TestConvertRealPresentations: LibreOffice makes a PDF of a deck, a page a
// slide (the hidden one too), whose Chinese reads as text and whose
// picture is drawn; ranges of its pages and pages picked out are PDFs of
// those pages alone. The deck saved by LibreOffice in the older and the
// OpenDocument formats converts to PowerPoint's again, its slides and
// notes read by doctext, and to the same PDF's pages.
func TestConvertRealPresentations(t *testing.T) {
	t.Parallel()
	c, p := realConverter(t), realPager(t)
	deck := lecture(t)
	out, err := c.Convert(t.Context(), deck, Format{"pptx", Slides, true}, ToPDF)
	if err != nil {
		t.Fatal(err)
	}
	if out.Pages != 3 || out.Capped {
		t.Errorf("a PDF of %d pages, capped %v; want one a slide", out.Pages, out.Capped)
	}
	res := pdfText(t, out.Data)
	for _, want := range []string{"第三週：排序", "合併排序", "Hidden aside", "Complexity"} {
		if !strings.Contains(squeezed(res.Text), squeezed(want)) {
			t.Errorf("the PDF's text lacks %q:\n%s", want, res.Text)
		}
	}
	if res.Images == 0 {
		t.Error("the slide's picture is not in its PDF")
	}
	if strings.Contains(res.Text, "quicksort") {
		t.Error("the PDF has the speaker notes' pages")
	}

	two, err := p.Range(t.Context(), out.Data, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := pdfText(t, two); got.Parts != 2 || strings.Contains(squeezed(got.Text), "第三週") || !strings.Contains(got.Text, "Complexity") {
		t.Errorf("pages 2 to 3: %d pages\n%s", got.Parts, got.Text)
	}
	if _, err := p.Range(t.Context(), out.Data, 4, 5); !errors.Is(err, ErrMalformed) {
		t.Errorf("pages past the last: %v", err)
	}
	// Drawn as pictures, for a model that takes no PDFs: one a page, of a
	// slide's size at the resolution asked for, never past 150 dpi.
	pics, err := p.Images(t.Context(), out.Data, 2, 3, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(pics) != 2 {
		t.Fatalf("%d pictures of pages 2 to 3", len(pics))
	}
	for i, b := range pics {
		img, err := png.DecodeConfig(bytes.NewReader(b))
		// A slide of 10 inches at 150 dpi.
		if err != nil || img.Width < 1000 || img.Width > 2200 {
			t.Errorf("picture %d: %+v, %v", i+1, img, err)
		}
	}
	if _, err := p.Images(t.Context(), out.Data, 4, 5, 150); !errors.Is(err, ErrMalformed) {
		t.Errorf("pictures past the last page: %v", err)
	}
	picked, err := p.Pick(t.Context(), out.Data, []int{3, 1})
	if err != nil {
		t.Fatal(err)
	}
	got := pdfText(t, picked)
	if got.Parts != 2 || !strings.HasPrefix(got.Text, "## Page 1\nComplexity") || !strings.Contains(squeezed(got.Text), "第三週") {
		t.Errorf("pages 3 and 1 picked: %d pages\n%s", got.Parts, got.Text)
	}

	for _, older := range []struct{ ext, filter string }{{"ppt", "ppt:MS PowerPoint 97"}, {"odp", "odp:impress8"}} {
		t.Run(older.ext, func(t *testing.T) {
			saved, err := c.convert(t.Context(), deck, Format{"pptx", Slides, true}, older.ext, older.filter)
			if err != nil {
				t.Fatal(err)
			}
			if mt := Sniff(saved.Data); mt == "" {
				t.Errorf("a %s LibreOffice saved is not known by what it holds", older.ext)
			}
			f := Format{older.ext, Slides, false}
			pptx, err := c.Convert(t.Context(), saved.Data, f, ToPPTX)
			if err != nil {
				t.Fatal(err)
			}
			res, err := doctext.Extract(context.Background(), pptx.Data, doctext.PPTX, doctext.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			// The hidden slide's title is its title placeholder in one
			// format, a line of it in the other.
			if res.Parts != 3 || !strings.Contains(res.Text, "## Slide 1: 第三週：排序") || !strings.Contains(res.Text, "Notes: Ask who has seen quicksort.") ||
				!strings.Contains(res.Text, "## Slide 2 (hidden)") || !strings.Contains(res.Text, "Hidden aside") {
				t.Errorf("its PowerPoint form reads:\n%s", res.Text)
			}
			pdf, err := c.Convert(t.Context(), saved.Data, f, ToPDF)
			if err != nil || pdf.Pages != 3 {
				t.Errorf("its PDF: %v", err)
			}
		})
	}
}

// TestConvertRealDocuments: a Word document, and the same saved by
// LibreOffice as a Word 97, an OpenDocument and an RTF file, each makes a
// PDF whose Chinese reads; a workbook saved as Excel 97 and OpenDocument
// converts to Excel's own format again, its cells read by doctext.
func TestConvertRealDocuments(t *testing.T) {
	t.Parallel()
	c := realConverter(t)
	doc := doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "期中考試範圍", Heading: 1}, {Text: "Chapters 1 to 5, with the exercises."}}})
	for _, older := range []struct{ ext, filter string }{{"docx", ""}, {"doc", "doc:MS Word 97"}, {"odt", "odt:writer8"}, {"rtf", "rtf:Rich Text Format"}} {
		t.Run(older.ext, func(t *testing.T) {
			data, ooxml := doc, true
			if older.filter != "" {
				saved, err := c.convert(t.Context(), doc, Format{"docx", Document, true}, older.ext, older.filter)
				if err != nil {
					t.Fatal(err)
				}
				data, ooxml = saved.Data, false
				if Sniff(data) == "" {
					t.Errorf("a %s LibreOffice saved is not known by what it holds", older.ext)
				}
			}
			out, err := c.Convert(t.Context(), data, Format{older.ext, Document, ooxml}, ToPDF)
			if err != nil {
				t.Fatal(err)
			}
			res := pdfText(t, out.Data)
			if out.Pages != 1 || !strings.Contains(squeezed(res.Text), "期中考試範圍") || !strings.Contains(squeezed(res.Text), "Chapters1to5") {
				t.Errorf("%d pages:\n%s", out.Pages, res.Text)
			}
		})
	}
	book := doctexttest.XLSX(doctexttest.Sheet{Name: "Quiz", Rows: [][]any{{"Question", "Points"}, {"排序", 5}}})
	for _, older := range []struct{ ext, filter string }{{"xls", "xls:MS Excel 97"}, {"ods", "ods:calc8"}} {
		t.Run(older.ext, func(t *testing.T) {
			saved, err := c.convert(t.Context(), book, Format{"xlsx", Workbook, true}, older.ext, older.filter)
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Convert(t.Context(), saved.Data, Format{older.ext, Workbook, false}, ToXLSX)
			if err != nil {
				t.Fatal(err)
			}
			res, err := doctext.Extract(context.Background(), out.Data, doctext.XLSX, doctext.Limits{})
			if err != nil || !strings.Contains(res.Text, "## Sheet 1: Quiz\nQuestion,Points\n排序,5") {
				t.Errorf("its Excel form reads %v:\n%+v", err, res)
			}
		})
	}
}

// TestConvertRealHostile: a document reaches nothing outside itself as
// LibreOffice converts it: a picture it links on a web server is not
// fetched, nor a section it links to a local file read; a file that is not
// what it says is ErrMalformed.
func TestConvertRealHostile(t *testing.T) {
	t.Parallel()
	c := realConverter(t)
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(picture(t))
	}))
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("ais_TopSecretToken0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:xlink="http://www.w3.org/1999/xlink" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0" office:version="1.2">
<office:body><office:text>
<text:p>Before the links.</text:p>
<text:section text:name="S1"><text:section-source xlink:href="file://%s" text:filter-name="Text"/><text:p>kept text</text:p></text:section>
<text:p><draw:frame svg:width="2cm" svg:height="2cm"><draw:image xlink:href="%s/pixel.png" xlink:type="simple" xlink:show="embed" xlink:actuate="onLoad"/></draw:frame></text:p>
<text:p>After the links.</text:p>
</office:text></office:body></office:document-content>`, secret, srv.URL)
	odt := odfWith(t, "application/vnd.oasis.opendocument.text", content)
	out, err := c.Convert(t.Context(), odt, Format{"odt", Document, false}, ToPDF)
	if err != nil {
		t.Fatal(err)
	}
	if res := pdfText(t, out.Data); !strings.Contains(res.Text, "After the links.") || strings.Contains(res.Text, "TopSecret") {
		t.Errorf("the linked document's PDF reads:\n%s", res.Text)
	}

	// A deck whose picture is linked on the web server, not embedded.
	linked := rewrite(t, doctexttest.PPTX(doctexttest.Slide{Title: "Linked", Images: 1}), map[string]func(string) string{
		"ppt/slides/slide1.xml": func(s string) string { return strings.Replace(s, `r:embed="rIdImg0"`, `r:link="rIdImg0"`, 1) },
		"ppt/slides/_rels/slide1.xml.rels": func(s string) string {
			return strings.Replace(s, `Target="../media/image1.png"`, `Target="`+srv.URL+`/linked.png" TargetMode="External"`, 1)
		},
	})
	if _, err := c.Convert(t.Context(), linked, Format{"pptx", Slides, true}, ToPDF); err != nil {
		t.Fatal(err)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("LibreOffice fetched what a document links %d times", n)
	}

	junk := bytes.Repeat([]byte("not an office file "), 100)
	if _, err := c.Convert(t.Context(), junk, Format{"pptx", Slides, true}, ToPDF); !errors.Is(err, ErrMalformed) {
		t.Errorf("a file that is not a deck: %v", err)
	}
	broken := append([]byte("PK\x03\x04"), junk...)
	if _, err := c.Convert(t.Context(), broken, Format{"pptx", Slides, true}, ToPDF); !errors.Is(err, ErrMalformed) {
		t.Errorf("a broken zip: %v", err)
	}
	empty(t, c.cfg.TempDir)
}

// odfWith is an OpenDocument package of content, with the manifest the
// format asks for.
func odfWith(t *testing.T, mt, content string) []byte {
	t.Helper()
	return odf(t, mt, nil, [2]string{"content.xml", content},
		[2]string{"META-INF/manifest.xml", `<?xml version="1.0" encoding="UTF-8"?><manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.2">` +
			`<manifest:file-entry manifest:full-path="/" manifest:media-type="` + mt + `"/><manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/></manifest:manifest>`})
}

// rewrite is the zip data with the parts named rewritten by their
// functions, the rest as they are.
func rewrite(t *testing.T, data []byte, parts map[string]func(string) string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]string
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if fn, ok := parts[f.Name]; ok {
			if s = fn(s); s == string(b) {
				t.Fatalf("%s is not rewritten", f.Name)
			}
		}
		out = append(out, [2]string{f.Name, s})
	}
	return doctexttest.Zip(out...)
}
