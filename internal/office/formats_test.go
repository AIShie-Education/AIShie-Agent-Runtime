package office

import (
	"archive/zip"
	"bytes"
	"hash/crc32"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext/doctexttest"
)

func TestFormatOf(t *testing.T) {
	tests := map[string]Format{
		doctexttest.PPTXType:                              {"pptx", Slides, true},
		"application/vnd.ms-powerpoint":                   {"ppt", Slides, false},
		"application/vnd.oasis.opendocument.presentation": {"odp", Slides, false},
		doctexttest.DOCXType:                              {"docx", Document, true},
		"application/msword":                              {"doc", Document, false},
		"text/rtf":                                        {"rtf", Document, false},
		doctexttest.XLSXType:                              {"xlsx", Workbook, true},
		"application/vnd.oasis.opendocument.spreadsheet":  {"ods", Workbook, false},
	}
	for mt, want := range tests {
		if got, ok := FormatOf(mt); !ok || got != want {
			t.Errorf("FormatOf(%q) = %+v %v, want %+v", mt, got, ok, want)
		}
	}
	for _, mt := range []string{"application/pdf", "text/plain", "application/zip", "application/vnd.ms-office", ""} {
		if f, ok := FormatOf(mt); ok {
			t.Errorf("FormatOf(%q) = %+v", mt, f)
		}
	}
	for mt, f := range formats {
		if !validExt(f.Ext) {
			t.Errorf("%s's extension %q", mt, f.Ext)
		}
	}
}

// odf is an OpenDocument package as the format has it: its mimetype entry
// first, stored as it is, then parts (a content.xml of nothing when none
// are given).
func odf(t *testing.T, mt string, extra []byte, parts ...[2]string) []byte {
	t.Helper()
	if len(parts) == 0 {
		parts = [][2]string{{"content.xml", "<x/>"}}
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	// Its sizes in its header, with no data descriptor after, as
	// LibreOffice wants it.
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "mimetype", Method: zip.Store, Extra: extra,
		CRC32: crc32.ChecksumIEEE([]byte(mt)), CompressedSize64: uint64(len(mt)), UncompressedSize64: uint64(len(mt))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(mt)); err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		w, err := zw.Create(p[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(p[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSniff(t *testing.T) {
	cfb := func(stream string) []byte {
		return append(append(append([]byte(nil), cfbMagic...), make([]byte, 504)...), utf16LE(stream)...)
	}
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"a PowerPoint 97 file", cfb("PowerPoint Document"), PPTType},
		{"a Word 97 file", cfb("WordDocument"), DOCType},
		{"an Excel 97 file", cfb("Workbook"), XLSType},
		{"an Excel 5 file", cfb("Book"), XLSType},
		{"a container of nothing known", cfb("Contents"), ""},
		{"RTF", []byte(`{\rtf1\ansi Hello}`), RTFType},
		{"an OpenDocument presentation", odf(t, "application/vnd.oasis.opendocument.presentation", nil), "application/vnd.oasis.opendocument.presentation"},
		{"an OpenDocument text", odf(t, "application/vnd.oasis.opendocument.text", nil), "application/vnd.oasis.opendocument.text"},
		{"an OpenDocument text template", odf(t, "application/vnd.oasis.opendocument.text-template", nil), "application/vnd.oasis.opendocument.text-template"},
		{"an OpenDocument spreadsheet with an extra field", odf(t, "application/vnd.oasis.opendocument.spreadsheet", []byte{0xfe, 0xca, 0, 0}),
			"application/vnd.oasis.opendocument.spreadsheet"},
		{"an OpenDocument type unknown", odf(t, "application/vnd.oasis.opendocument.graphics", nil), ""},
		{"an Office Open XML file", doctexttest.PPTX(doctexttest.Slide{Title: "x"}), ""},
		{"a zip of other things", doctexttest.Zip([2]string{"mimetype", "application/vnd.oasis.opendocument.text"}), ""},
		{"a PDF", doctexttest.PDFLines("x"), ""},
		{"nothing", nil, ""},
		{"a zip's header cut short", []byte("PK\x03\x04\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x08\x00\xff\xff"), ""},
	}
	for _, tc := range tests {
		if got := Sniff(tc.data); got != tc.want {
			t.Errorf("%s: Sniff = %q, want %q", tc.name, got, tc.want)
		}
	}
}
