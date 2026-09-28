package ocr

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"testing"
)

// A4 at 300 dpi, as a scanner makes a page.
const (
	pageWidth  = 2480
	pageHeight = 3508
	pageDPI    = 300
)

// drawPage is a scanned page: white, with lines of text drawn in Unifont's
// glyphs at scale times their size (3: 48 pixels a line, some 12 points at
// 300 dpi), a line's worth of space between lines.
func drawPage(t testing.TB, scale int, lines ...string) *image.Gray {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, pageWidth, pageHeight))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	y := 300
	for _, line := range lines {
		x := 200
		for _, r := range line {
			if r == ' ' {
				x += 8 * scale
				continue
			}
			hex, ok := glyphs[r]
			if !ok {
				t.Fatalf("no glyph for %q", r)
			}
			width := 16
			if len(hex) == 32 {
				width = 8
			}
			digits := width / 4
			for row := range 16 {
				bits, err := strconv.ParseUint(hex[row*digits:(row+1)*digits], 16, 32)
				if err != nil {
					t.Fatal(err)
				}
				for col := range width {
					if bits&(1<<(width-1-col)) == 0 {
						continue
					}
					for dy := range scale {
						for dx := range scale {
							img.SetGray(x+col*scale+dx, y+row*scale+dy, color.Gray{})
						}
					}
				}
			}
			x += width * scale
		}
		y += 16 * scale * 2
	}
	return img
}

func pngOf(t testing.TB, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// scannedPDF is a PDF whose pages are the images, each drawn over the
// whole page at pageDPI, as a scanner makes one: no text at all.
func scannedPDF(t testing.TB, pages ...*image.Gray) []byte {
	t.Helper()
	var objs []string
	add := func(s string) int { objs = append(objs, s); return len(objs) }
	catalog := add("") // filled in once the pages are known
	tree := add("")
	var kids []string
	for _, img := range pages {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		if _, err := zw.Write(img.Pix); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		im := add(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8 "+
			"/Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream", w, h, z.Len(), z.Bytes()))
		pw, ph := float64(w)*72/pageDPI, float64(h)*72/pageDPI
		draw := fmt.Sprintf("q %.2f 0 0 %.2f 0 0 cm /Im0 Do Q", pw, ph)
		content := add(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(draw), draw))
		page := add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 %.2f %.2f] /Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>",
			tree, pw, ph, im, content))
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	objs[catalog-1] = fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", tree)
	objs[tree-1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", joinStrings(kids), len(kids))
	var b bytes.Buffer
	b.WriteString("%PDF-1.5\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, catalog, xref)
	return b.Bytes()
}

func joinStrings(ss []string) string {
	var b bytes.Buffer
	for i, s := range ss {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
	}
	return b.String()
}
