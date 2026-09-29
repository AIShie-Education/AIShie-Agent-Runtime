package doctext

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
)

// Files made to hurt a reader: each is read within its limits and its
// time, and refused or read for what it is.

// rawPDF is a PDF of objs (numbered from 1, the first the catalogue) with
// a correct cross-reference table; tail is added to the trailer.
func rawPDF(tail string, objs ...string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R %s >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, tail, x)
	return b.Bytes()
}

func stream(dict, data string) string {
	return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(data), data)
}

const (
	catalog   = "<< /Type /Catalog /Pages 2 0 R >>"
	helvetica = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"
)

// hello is a page's objects: 3 the page, 4 its content, 5 its font.
func hello(page string) []string {
	return []string{page, stream("", "BT /F1 12 Tf 72 700 Td (Hello) Tj ET"), helvetica}
}

// flateBomb is a Flate stream of n zero bytes.
func flateBomb(n int) []byte {
	var b bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&b, zlib.BestCompression)
	chunk := make([]byte, 1<<20)
	for n > 0 {
		k := min(n, len(chunk))
		_, _ = zw.Write(chunk[:k])
		n -= k
	}
	_ = zw.Close()
	return b.Bytes()
}

// hostilePDFs are the PDFs of TestHostilePDF, for the fuzzers' seeds too.
func hostilePDFs() [][]byte {
	var out [][]byte
	for _, h := range hostile() {
		out = append(out, h.data)
	}
	return out
}

type hostileCase struct {
	name string
	data []byte
	// want is what the reading must come to: "text:…" text holding it,
	// or the error it must be.
	want string
}

func hostile() []hostileCase {
	res := "/Resources << /Font << /F1 5 0 R >> >>"
	page := "<< /Type /Page /Parent 2 0 R " + res + " /Contents 4 0 R >>"
	var chain []string
	chain = append(chain, catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R "+res+" /Contents 6 0 R >>", "", helvetica)
	chain[3] = "null"
	for i := range 2000 {
		// Object 6+i, the page's content for i 0, has for its length
		// object 7+i: a stream whose length is the next.
		chain = append(chain, fmt.Sprintf("<< /Length %d 0 R >>\nstream\nBT /F1 12 Tf (deep) Tj ET\nendstream", 7+i))
	}
	forms := []string{catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /Resources << /XObject << /A 5 0 R >> >> /Contents 4 0 R >>", stream("", "/A Do")}
	for range 24 {
		// A draws B twice, B draws C twice, …: twenty-four deep.
		forms = append(forms, fmt.Sprintf("<< /Type /XObject /Subtype /Form /BBox [0 0 1 1] /Resources << /XObject << /N %d 0 R >> /Font << /F1 %d 0 R >> >> /Length %d >>\nstream\n%s\nendstream",
			len(forms)+2, 4+24+1, len("/N Do /N Do BT /F1 1 Tf (f) Tj ET"), "/N Do /N Do BT /F1 1 Tf (f) Tj ET"))
	}
	forms = append(forms, helvetica)
	bomb := flateBomb(96 << 20)
	return []hostileCase{
		{"a page tree whose kids hold itself", rawPDF("", append([]string{catalog, "<< /Type /Pages /Kids [2 0 R 3 0 R 2 0 R] /Count 5 >>"}, hello(page)...)...), "text:Hello"},
		{"a parent that loops", rawPDF("", catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 /Parent 6 0 R "+res+" >>",
			"<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>", stream("", "BT /F1 12 Tf (Inherited) Tj ET"), helvetica, "<< /Type /Pages /Parent 2 0 R >>"), "text:Inherited"},
		{"a stream that is its own length", rawPDF("", catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", page,
			"<< /Length 4 0 R >>\nstream\nBT /F1 12 Tf (Self) Tj ET\nendstream", helvetica), "text:Self"},
		{"lengths two thousand deep", rawPDF("", chain...), ""},
		{"arrays nested a hundred thousand deep", rawPDF("", catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", page,
			stream("", "BT /F1 12 Tf (Before) Tj "+strings.Repeat("[", 100000)+" ET"), helvetica), "text:Before"},
		{"dictionaries nested deep", rawPDF("", catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 /X "+strings.Repeat("<< /A ", 5000)+" >>", page,
			stream("", "BT /F1 12 Tf (Deep dict) Tj ET"), helvetica), ""},
		{"a Flate bomb", rawPDF("", catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", page,
			fmt.Sprintf("<< /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream", len(bomb), bomb), helvetica), "error:" + ErrLimit.Error()},
		{"an xref whose Prev is itself", func() []byte {
			d := rawPDF("", append([]string{catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"}, hello(page)...)...)
			x := bytes.LastIndex(d, []byte("startxref"))
			off := strings.TrimSpace(strings.Split(string(d[x+len("startxref"):]), "%%EOF")[0])
			return bytes.Replace(d, []byte("/Root 1 0 R"), []byte("/Root 1 0 R /Prev "+off), 1)
		}(), "text:Hello"},
		{"a startxref past the end", func() []byte {
			d := rawPDF("", append([]string{catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"}, hello(page)...)...)
			x := bytes.LastIndex(d, []byte("startxref"))
			return append(d[:x:x], []byte("startxref\n99999999\n%%EOF\n")...)
		}(), "text:Hello"},
		{"an object stream that claims a million objects", rawPDF("", catalog, "<< /Type /Pages /Kids [] /Count 0 >>",
			stream("/Type /ObjStm /N 1000000 /First 4", "1 0 << >>")), ""},
		{"forms that draw themselves", rawPDF("", catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 2 0 R /Resources << /XObject << /A 5 0 R >> /Font << /F1 6 0 R >> >> /Contents 4 0 R >>", stream("", "/A Do"),
			"<< /Type /XObject /Subtype /Form /BBox [0 0 1 1] /Resources << /XObject << /A 5 0 R >> /Font << /F1 6 0 R >> >> /Length 33 >>\nstream\nBT /F1 9 Tf (Loop) Tj ET /A Do\nendstream",
			helvetica), "text:Loop"},
		{"forms that draw each other twice, twenty-four deep", rawPDF("", forms...), ""},
		{"unbalanced parentheses by the megabyte", rawPDF("", catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", page,
			stream("", "BT /F1 12 Tf (Start) Tj ET "+strings.Repeat(")", 1<<20)+" BT (End) Tj ET"), helvetica), "text:Start"},
		{"an inline image that never ends", rawPDF("", catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", page,
			stream("", "BT /F1 12 Tf (Pic) Tj ET BI /W 1 /H 1 ID "+strings.Repeat("\x00EIX", 5000)), helvetica), "text:Pic"},
		{"encrypted for certificates", rawPDF("/Encrypt 6 0 R", append([]string{catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"},
			append(hello(page), "<< /Filter /Adobe.PubSec /V 4 /R 4 >>")...)...), "error:" + ErrEncrypted.Error()},
		{"an object number past any table", func() []byte {
			d := rawPDF("", append([]string{catalog, "<< /Type /Pages /Kids [3 0 R 999999999 0 R] /Count 2 >>"}, hello(page)...)...)
			return append(d, []byte("999999999 0 obj << /Type /Page >> endobj\n")...)
		}(), "text:Hello"},
		{"a ToUnicode range over every code", rawPDF("", catalog, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 2 0 R /Resources << /Font << /F2 5 0 R >> >> /Contents 4 0 R >>", stream("", "BT /F2 12 Tf <00410042FFFF> Tj ET"),
			"<< /Type /Font /Subtype /Type0 /BaseFont /X /Encoding /Identity-H /DescendantFonts [6 0 R] /ToUnicode 7 0 R >>",
			"<< /Type /Font /Subtype /CIDFontType2 /BaseFont /X /W [0 [1 2 3] 10 65535 500] >>",
			stream("", "1 begincodespacerange <0000> <FFFF> endcodespacerange 1 beginbfrange <0000> <FFFF> <0000> endbfrange")), "text:AB"},
		{"not a PDF", []byte("%PDF-1.7\nnothing at all"), "error:" + ErrMalformed.Error()},
	}
}

func TestHostilePDF(t *testing.T) {
	for _, h := range hostile() {
		t.Run(h.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			start := time.Now()
			res, err := Extract(ctx, h.data, PDF, DefaultLimits())
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("took %s", d)
			}
			switch {
			case strings.HasPrefix(h.want, "text:"):
				if err != nil || !strings.Contains(res.Text, strings.TrimPrefix(h.want, "text:")) {
					t.Errorf("err %v, text %q; want %s", err, text(res), h.want)
				}
			case strings.HasPrefix(h.want, "error:"):
				if err == nil || !strings.HasPrefix(err.Error(), strings.TrimPrefix(h.want, "error:")) {
					t.Errorf("err %v, text %q; want %s", err, text(res), h.want)
				}
			default:
				if err != nil && !errors.Is(err, ErrLimit) && !errors.Is(err, ErrMalformed) {
					t.Errorf("err %v", err)
				}
			}
			if _, err := PDFPages(ctx, h.data, DefaultLimits()); err != nil && !errors.Is(err, ErrLimit) &&
				!errors.Is(err, ErrMalformed) && !errors.Is(err, ErrEncrypted) {
				t.Errorf("PDFPages: %v", err)
			}
		})
	}
}

func text(r *Result) string {
	if r == nil {
		return ""
	}
	return cut(r.Text, 200)
}

// zipOf writes entries as they are given: names, methods and flags too.
func zipOf(t *testing.T, entries ...func(zw *zip.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		e(zw)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func entry(name, data string) func(zw *zip.Writer) {
	return func(zw *zip.Writer) {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		_, _ = w.Write([]byte(data))
	}
}

func TestHostileOffice(t *testing.T) {
	deck := doctexttest.PPTX(doctexttest.Slide{Title: "Fine"})
	zeros := strings.Repeat("\x00", 40<<20)
	bigSlide := `<p:sld xmlns:p="p" xmlns:a="a"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>` + strings.Repeat("x", 3<<20) + `</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`
	encrypted := func(zw *zip.Writer) {
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: "ppt/presentation.xml", Method: zip.Store, Flags: 0x1})
		_, _ = w.Write([]byte("<p:presentation/>"))
	}
	var many []func(zw *zip.Writer)
	for i := range 10001 {
		many = append(many, entry(fmt.Sprintf("x/%d", i), ""))
	}
	cases := []struct {
		name string
		data []byte
		f    Format
		want error
	}{
		{"an entry that decompresses past the limit", withPart(t, deck, "ppt/slides/slide1.xml", []byte(zeros)), PPTX, ErrLimit},
		{"a part whose size is lied about", zipOf(t, func(zw *zip.Writer) {
			// The central directory says the part is 100 bytes; reading
			// it finds out.
			var comp bytes.Buffer
			fw, _ := flate.NewWriter(&comp, flate.BestCompression)
			_, _ = fw.Write([]byte(zeros))
			_ = fw.Close()
			w, _ := zw.CreateRaw(&zip.FileHeader{Name: "ppt/presentation.xml", Method: zip.Deflate, CRC32: crc32.ChecksumIEEE([]byte(zeros)),
				CompressedSize64: uint64(comp.Len()), UncompressedSize64: 100})
			_, _ = w.Write(comp.Bytes())
		}), PPTX, ErrMalformed},
		{"more entries than the limit", zipOf(t, many...), PPTX, ErrLimit},
		{"an absolute name", zipOf(t, entry("/etc/passwd", "x"), entry("ppt/presentation.xml", "<p/>")), PPTX, ErrMalformed},
		{"a name that climbs out", zipOf(t, entry("ppt/../../x.xml", "x"), entry("ppt/presentation.xml", "<p/>")), PPTX, ErrMalformed},
		{"a backslash in a name", zipOf(t, entry(`ppt\slides\slide1.xml`, "x")), PPTX, ErrMalformed},
		{"a name given twice", zipOf(t, entry("ppt/presentation.xml", "<a/>"), entry("PPT/presentation.xml", "<b/>")), PPTX, ErrMalformed},
		{"an encrypted entry", zipOf(t, encrypted), PPTX, ErrEncrypted},
		{"an older binary Office file", append(bytes.Clone(cfbMagic), make([]byte, 600)...), DOCX, ErrOldFormat},
		{"an Office file encrypted with a password", append(append(bytes.Clone(cfbMagic), make([]byte, 600)...), utf16LE("EncryptedPackage")...), XLSX, ErrEncrypted},
		{"not a zip archive", []byte("PK\x03\x04 not really"), XLSX, ErrMalformed},
		{"XML nested past the limit", withPart(t, deck, "ppt/slides/slide1.xml", []byte(strings.Repeat("<a>", 300)+strings.Repeat("</a>", 300))), PPTX, ErrLimit},
		{"a slide of more text than is given", withPart(t, deck, "ppt/slides/slide1.xml", []byte(bigSlide)), PPTX, nil},
		{"a document type's entities", withPart(t, doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "x"}}}), "word/document.xml",
			[]byte(`<!DOCTYPE d [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;&a;&a;&a;">]><w:document xmlns:w="w"><w:body><w:p><w:r><w:t>&b;</w:t></w:r></w:p></w:body></w:document>`)), DOCX, ErrMalformed},
		{"a character set other than UTF-8", withPart(t, doctexttest.DOCX(doctexttest.Doc{Blocks: []doctexttest.Block{{Text: "x"}}}), "word/document.xml",
			[]byte(`<?xml version="1.0" encoding="Shift_JIS"?><w:document xmlns:w="w"><w:body/></w:document>`)), DOCX, ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			res, err := Extract(ctx, tc.data, tc.f, DefaultLimits())
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err %v", err)
				}
				if len(res.Text) > DefaultLimits().MaxText+200 {
					t.Errorf("%d bytes of text", len(res.Text))
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err %v, text %q; want %v", err, text(res), tc.want)
			}
		})
	}
}

// A file's reading ends with its context: a deadline already past stops
// it at once, whatever the file.
func TestExtractStopsAtItsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()
	big := make([][]any, 3000)
	for i := range big {
		big[i] = []any{"row", i, float64(i) / 3}
	}
	lim := DefaultLimits()
	lim.MaxRows = 5000
	for _, c := range []struct {
		f    Format
		data []byte
	}{
		{PDF, doctexttest.PDF(doctexttest.PDFPage{Lines: strings.Split(strings.Repeat("line\n", 3000), "\n")})},
		{XLSX, doctexttest.XLSX(doctexttest.Sheet{Name: "Big", Rows: big})},
	} {
		start := time.Now()
		_, err := Extract(ctx, c.data, c.f, lim)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
			t.Errorf("%s: err %v after %s; want the deadline, at once", c.f, err, time.Since(start))
		}
	}
}
