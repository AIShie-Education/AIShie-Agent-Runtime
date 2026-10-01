package rendition

import (
	"maps"
	"slices"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// contractTable is the table of Core's contract for renditions (§1, Which
// files), row by row: extensions and the Office or OpenDocument type
// declared for them, as Core's migration 0026 has it.
var contractTable = []struct {
	exts  []string
	types []string
}{
	{[]string{"doc", "dot"}, []string{"application/msword"}},
	{[]string{"docx"}, []string{"application/vnd.openxmlformats-officedocument.wordprocessingml.document"}},
	{[]string{"docm"}, []string{"application/vnd.ms-word.document.macroenabled.12"}},
	{[]string{"dotx"}, []string{"application/vnd.openxmlformats-officedocument.wordprocessingml.template"}},
	{[]string{"xls", "xlt"}, []string{"application/vnd.ms-excel"}},
	{[]string{"xlsx"}, []string{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}},
	{[]string{"xlsm"}, []string{"application/vnd.ms-excel.sheet.macroenabled.12"}},
	{[]string{"xltx"}, []string{"application/vnd.openxmlformats-officedocument.spreadsheetml.template"}},
	{[]string{"ppt", "pps", "pot"}, []string{"application/vnd.ms-powerpoint"}},
	{[]string{"pptx"}, []string{"application/vnd.openxmlformats-officedocument.presentationml.presentation"}},
	{[]string{"pptm"}, []string{"application/vnd.ms-powerpoint.presentation.macroenabled.12"}},
	{[]string{"ppsx"}, []string{"application/vnd.openxmlformats-officedocument.presentationml.slideshow"}},
	{[]string{"potx"}, []string{"application/vnd.openxmlformats-officedocument.presentationml.template"}},
	{[]string{"odt"}, []string{"application/vnd.oasis.opendocument.text"}},
	{[]string{"ods"}, []string{"application/vnd.oasis.opendocument.spreadsheet"}},
	{[]string{"odp"}, []string{"application/vnd.oasis.opendocument.presentation"}},
	{[]string{"odg"}, []string{"application/vnd.oasis.opendocument.graphics"}},
	{[]string{"rtf"}, []string{"application/rtf", "text/rtf"}},
}

// contractGeneric are the contract's generic types, taken with any
// extension of the table.
var contractGeneric = []string{"application/octet-stream", "application/zip", "application/x-zip-compressed", "application/vnd.ms-office"}

// TestTableIsTheContracts: the runtime's table is the contract's, no more
// and no less: the same extensions, the same Office types and the same
// generic ones; and every listed type goes with every listed extension.
func TestTableIsTheContracts(t *testing.T) {
	var exts, officeTypes []string
	for _, row := range contractTable {
		exts = append(exts, row.exts...)
		officeTypes = append(officeTypes, row.types...)
	}
	slices.Sort(exts)
	if got := slices.Sorted(maps.Keys(Extensions)); !slices.Equal(got, exts) {
		t.Errorf("the extensions are %v; the contract's %v", got, exts)
	}
	if got := slices.Sorted(slices.Values(OfficeTypes)); !slices.Equal(got, slices.Sorted(slices.Values(officeTypes))) {
		t.Errorf("the Office types are %v; the contract's %v", got, officeTypes)
	}
	if !slices.Equal(slices.Sorted(slices.Values(GenericTypes)), slices.Sorted(slices.Values(contractGeneric))) {
		t.Errorf("the generic types are %v; the contract's %v", GenericTypes, contractGeneric)
	}
	for _, ext := range exts {
		for _, ty := range append(slices.Clone(officeTypes), contractGeneric...) {
			if !Convertible("file."+ext, ty) {
				t.Errorf("a .%s declared %s is not converted", ext, ty)
			}
		}
	}
}

// TestConvertibleExamples holds Convertible to the contract's examples,
// and to how Core's SQL reads a name and a type: the extension after the
// last dot, case aside, none with a slash or nothing after the dot; the
// type before its parameters, spaces trimmed, case aside.
func TestConvertibleExamples(t *testing.T) {
	for _, tc := range []struct {
		name, ct string
		want     bool
	}{
		// The contract's.
		{"Lecture 3.PPTX", "application/vnd.openxmlformats-officedocument.presentationml.presentation", true},
		{"essay.docx", "application/octet-stream", true},
		{"marks.csv", "application/vnd.ms-excel", false},
		{"a.docx", "text/html", false},
		{"slides.pdf", "application/pdf", false},
		{"photo.png", "image/png", false},
		{"notes.txt", "text/plain", false},
		{"code.zip", "application/zip", false},
		{"week.pps", "application/vnd.ms-powerpoint", true},
		{"grades.xlsm", "application/vnd.ms-excel", true},
		{"第四週 handout.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", true},
		// A PDF, a picture, a text or an archive by its name is never one,
		// whatever it is declared.
		{"slides.pdf", "application/octet-stream", false},
		{"photo.png", "application/vnd.ms-powerpoint", false},
		{"notes.txt", "application/msword", false},
		// The type's parameters, spaces and case.
		{"a.doc", "application/msword; charset=binary", true},
		{"a.doc", "  Application/MSWord ;x=y", true},
		{"a.doc", "\tapplication/msword", false},
		{"a.RTF", "text/rtf", true},
		{"a.odg", "application/vnd.oasis.opendocument.graphics", true},
		{"a.odg", "application/vnd.oasis.opendocument.graphics-template", false},
		{"a.ott", "application/vnd.oasis.opendocument.text", false},
		{"a.xltm", "application/vnd.ms-excel", false},
		// The name's extension.
		{"docx", "application/octet-stream", false},
		{".docx", "application/octet-stream", true},
		{"a.docx.", "application/octet-stream", false},
		{"a.tar.docx", "application/zip", true},
		{"a.docx.zip", "application/zip", false},
		{"a.do/cx", "application/octet-stream", false},
		{`dir.docx\x`, "application/octet-stream", false},
		{"a.docx ", "application/octet-stream", false},
		{"", "application/octet-stream", false},
		{"a.docx", "", false},
	} {
		if got := Convertible(tc.name, tc.ct); got != tc.want {
			t.Errorf("Convertible(%q, %q) = %v, want %v", tc.name, tc.ct, got, tc.want)
		}
	}
}

// TestFormatOf: each extension is converted from the format of its family,
// given to LibreOffice under its own extension, an Office Open XML one
// known as such.
func TestFormatOf(t *testing.T) {
	for name, want := range map[string]office.Format{
		"a.DOC":  {Ext: "doc", Family: office.Document},
		"a.docx": {Ext: "docx", Family: office.Document, OOXML: true},
		"a.pps":  {Ext: "pps", Family: office.Slides},
		"a.potx": {Ext: "potx", Family: office.Slides, OOXML: true},
		"a.xlt":  {Ext: "xlt", Family: office.Workbook},
		"a.ods":  {Ext: "ods", Family: office.Workbook},
		"a.odg":  {Ext: "odg", Family: office.Drawing},
		"a.rtf":  {Ext: "rtf", Family: office.Document},
	} {
		if got, ok := FormatOf(name); !ok || got != want {
			t.Errorf("FormatOf(%q) = %+v, %v; want %+v", name, got, ok, want)
		}
	}
	if _, ok := FormatOf("a.pdf"); ok {
		t.Error("a PDF has a format to be converted from")
	}
	for ext, f := range Extensions {
		if f.Ext != ext {
			t.Errorf("a .%s is given to LibreOffice as a .%s", ext, f.Ext)
		}
	}
}
