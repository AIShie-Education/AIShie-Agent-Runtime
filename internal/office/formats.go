package office

import (
	"bytes"
	"encoding/binary"
	"strings"
)

// Family is what an Office file holds, which says what it is converted to.
type Family string

// The families.
const (
	// Slides is a presentation: its PDF, one page a slide, is what a model
	// that takes files sees; its slides' text and speaker notes are read
	// from its PowerPoint form.
	Slides Family = "slides"
	// Document is a word processor's document: its PDF is what a model
	// that takes files sees.
	Document Family = "document"
	// Workbook is a spreadsheet: always its text, rows as CSV, read from
	// its Excel form; converted to PDF only for a rendition (ToRendition).
	Workbook Family = "workbook"
	// Drawing is a drawing of OpenDocument's (.odg), converted to PDF only
	// for a rendition (package rendition): no model is given one.
	Drawing Family = "drawing"
)

// Format is an Office file's format, as the runtime converts it.
type Format struct {
	// Ext is the extension LibreOffice is given the file under.
	Ext    string
	Family Family
	// OOXML is an Office Open XML file (.pptx, .docx, .xlsx and their
	// kinds), which the runtime also reads itself (package doctext); the
	// others it reads only as LibreOffice converts them.
	OOXML bool
}

// formats are the media types converted, by their type/subtype.
var formats = map[string]Format{
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {"pptx", Slides, true},
	"application/vnd.openxmlformats-officedocument.presentationml.slideshow":    {"ppsx", Slides, true},
	"application/vnd.openxmlformats-officedocument.presentationml.template":     {"potx", Slides, true},
	"application/vnd.ms-powerpoint.presentation.macroenabled.12":                {"pptm", Slides, true},
	"application/vnd.ms-powerpoint.slideshow.macroenabled.12":                   {"ppsm", Slides, true},
	"application/vnd.ms-powerpoint.template.macroenabled.12":                    {"potm", Slides, true},
	"application/vnd.ms-powerpoint":                                             {"ppt", Slides, false},
	"application/mspowerpoint":                                                  {"ppt", Slides, false},
	"application/x-mspowerpoint":                                                {"ppt", Slides, false},
	"application/vnd.oasis.opendocument.presentation":                           {"odp", Slides, false},
	"application/vnd.oasis.opendocument.presentation-template":                  {"otp", Slides, false},

	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {"docx", Document, true},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.template": {"dotx", Document, true},
	"application/vnd.ms-word.document.macroenabled.12":                        {"docm", Document, true},
	"application/vnd.ms-word.template.macroenabled.12":                        {"dotm", Document, true},
	"application/msword":                               {"doc", Document, false},
	"application/x-msword":                             {"doc", Document, false},
	"application/vnd.oasis.opendocument.text":          {"odt", Document, false},
	"application/vnd.oasis.opendocument.text-template": {"ott", Document, false},
	"application/rtf":                                  {"rtf", Document, false},
	"application/x-rtf":                                {"rtf", Document, false},
	"text/rtf":                                         {"rtf", Document, false},

	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":    {"xlsx", Workbook, true},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.template": {"xltx", Workbook, true},
	"application/vnd.ms-excel.sheet.macroenabled.12":                       {"xlsm", Workbook, true},
	"application/vnd.ms-excel.template.macroenabled.12":                    {"xltm", Workbook, true},
	"application/vnd.ms-excel":                                             {"xls", Workbook, false},
	"application/x-msexcel":                                                {"xls", Workbook, false},
	"application/vnd.oasis.opendocument.spreadsheet":                       {"ods", Workbook, false},
	"application/vnd.oasis.opendocument.spreadsheet-template":              {"ots", Workbook, false},
}

// FormatOf is the Office format of a media type (type/subtype, lower case),
// and whether it is one.
func FormatOf(mediaType string) (Format, bool) {
	f, ok := formats[mediaType]
	return f, ok
}

// Media types Sniff gives, one a format, as a file of no telling type is
// recorded.
const (
	PPTType = "application/vnd.ms-powerpoint"
	DOCType = "application/msword"
	XLSType = "application/vnd.ms-excel"
	RTFType = "application/rtf"
)

// cfbMagic begins a Compound File Binary file: the container of the older
// Office formats.
var cfbMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// Sniff is the media type of an Office file that nothing else names, by
// what it holds: an older binary Office file by the stream its container's
// directory names (PowerPoint Document, WordDocument, Workbook or Book, in
// UTF-16), an OpenDocument file by its mimetype entry, which the format puts
// first in its zip and stores as it is, and an RTF file by its first bytes;
// "" when it is none of these. An Office Open XML file is doctext's to
// sniff.
func Sniff(data []byte) string {
	switch {
	case bytes.HasPrefix(data, cfbMagic):
		for _, s := range []struct{ stream, mt string }{
			{"PowerPoint Document", PPTType}, {"WordDocument", DOCType}, {"Workbook", XLSType}, {"Book", XLSType},
		} {
			if bytes.Contains(data, utf16LE(s.stream)) {
				return s.mt
			}
		}
	case bytes.HasPrefix(data, []byte(`{\rtf`)):
		return RTFType
	case bytes.HasPrefix(data, []byte("PK\x03\x04")) && len(data) >= 30:
		// The first entry's local header: stored as it is (method 0), its
		// name mimetype, its bytes right after the name and extra field.
		method := binary.LittleEndian.Uint16(data[8:10])
		name := int(binary.LittleEndian.Uint16(data[26:28]))
		extra := int(binary.LittleEndian.Uint16(data[28:30]))
		start := 30 + name + extra
		if method != 0 || name != len("mimetype") || len(data) < start || string(data[30:30+name]) != "mimetype" {
			return ""
		}
		rest := data[start:min(len(data), start+128)]
		for mt := range formats {
			if strings.HasPrefix(mt, "application/vnd.oasis.opendocument.") && bytes.HasPrefix(rest, []byte(mt)) &&
				(len(rest) == len(mt) || !isTypeChar(rest[len(mt)])) {
				return mt
			}
		}
	}
	return ""
}

func isTypeChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '+'
}

func utf16LE(s string) []byte {
	out := make([]byte, 0, 2*len(s))
	for i := 0; i < len(s); i++ {
		out = append(out, s[i], 0)
	}
	return out
}
