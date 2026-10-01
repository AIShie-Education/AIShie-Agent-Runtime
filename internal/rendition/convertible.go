// Package rendition is the runtime's renditions worker (docs/design.md
// §13), its side of Core's PDF renditions (AIShie-Core's migration 0026,
// its docs/schema.md §2.4 Renditions): every Office or OpenDocument file
// Core keeps, of a document's version of any kind or carried by a
// message, is previewed in the site as a PDF the site's agent runtime
// converts once, on the server. It holds Core's one table of which files
// are converted (Convertible), and the format each is converted from.
//
// The worker (Service) is on wherever LibreOffice converts here (package
// office, OFFICE_PDF) and Core is named, unless RENDITIONS=off: no AI, no
// cost and no switch of the site's. With the runtime's own credential in
// Core, the agent_runtime service's (CORE_SERVICE_CREDENTIAL), read at
// each call and paced with every other call of that service, it claims
// from Core's queue as many files as it converts at once
// (RENDITIONS_CONCURRENCY), waiting for one when none waits (a long
// poll), fetches each from its short-lived URL within MaxFileBytes,
// converts it with LibreOffice in the sandbox every conversion runs in
// (RENDITIONS_TIMEOUT), renews its claim every half of its lease
// (RENDITIONS_LEASE) meanwhile, uploads the PDF to the URL Core gives for
// the claim, and completes it: done, with its pages; or skipped
// (password_protected, unsupported, too_large) or failed
// (conversion_failed, timeout). Core saying the claim is lost, or the
// file gone, drops the work. Several processes may run it against one
// Core: Core never gives one file to two claims. Nothing it logs holds a
// file's name, its URL, the upload's, the credential, or what a file
// holds.
package rendition

import (
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// The one table of which files Core converts (Core's
// file_rendition_convertible, migration 0026, and docs/schema.md §2.4
// Renditions), which the runtime holds the same: a file is converted when
// its name's extension is one of Extensions and its declared type, without
// its parameters, is one of OfficeTypes or of GenericTypes. Both are asked,
// and any listed type goes with any listed extension: a .pps declared
// application/vnd.ms-powerpoint, a .xlsm declared application/vnd.ms-excel.
// A .csv a browser declares application/vnd.ms-excel is not converted, nor
// a .docx declared text/html; nor ever a PDF, a picture, a text or an
// archive.

// Extensions are the extensions of the files converted, lower case, each
// with the format LibreOffice is given the file in: the family that says
// which of its PDF exports makes the PDF, and whether it is an Office Open
// XML file (which a password keeps in a container of its own).
var Extensions = map[string]office.Format{
	"doc":  {Ext: "doc", Family: office.Document},
	"dot":  {Ext: "dot", Family: office.Document},
	"docx": {Ext: "docx", Family: office.Document, OOXML: true},
	"docm": {Ext: "docm", Family: office.Document, OOXML: true},
	"dotx": {Ext: "dotx", Family: office.Document, OOXML: true},
	"odt":  {Ext: "odt", Family: office.Document},
	"rtf":  {Ext: "rtf", Family: office.Document},

	"xls":  {Ext: "xls", Family: office.Workbook},
	"xlt":  {Ext: "xlt", Family: office.Workbook},
	"xlsx": {Ext: "xlsx", Family: office.Workbook, OOXML: true},
	"xlsm": {Ext: "xlsm", Family: office.Workbook, OOXML: true},
	"xltx": {Ext: "xltx", Family: office.Workbook, OOXML: true},
	"ods":  {Ext: "ods", Family: office.Workbook},

	"ppt":  {Ext: "ppt", Family: office.Slides},
	"pps":  {Ext: "pps", Family: office.Slides},
	"pot":  {Ext: "pot", Family: office.Slides},
	"pptx": {Ext: "pptx", Family: office.Slides, OOXML: true},
	"pptm": {Ext: "pptm", Family: office.Slides, OOXML: true},
	"ppsx": {Ext: "ppsx", Family: office.Slides, OOXML: true},
	"potx": {Ext: "potx", Family: office.Slides, OOXML: true},
	"odp":  {Ext: "odp", Family: office.Slides},

	"odg": {Ext: "odg", Family: office.Drawing},
}

// OfficeTypes are the Office, OpenDocument and RTF media types a converted
// file may be declared, lower case.
var OfficeTypes = []string{
	"application/msword",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.ms-word.document.macroenabled.12",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.template",
	"application/vnd.ms-excel",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.ms-excel.sheet.macroenabled.12",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.template",
	"application/vnd.ms-powerpoint",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/vnd.ms-powerpoint.presentation.macroenabled.12",
	"application/vnd.openxmlformats-officedocument.presentationml.slideshow",
	"application/vnd.openxmlformats-officedocument.presentationml.template",
	"application/vnd.oasis.opendocument.text",
	"application/vnd.oasis.opendocument.spreadsheet",
	"application/vnd.oasis.opendocument.presentation",
	"application/vnd.oasis.opendocument.graphics",
	"application/rtf",
	"text/rtf",
}

// GenericTypes say nothing in particular of the bytes, and are taken with
// any of Extensions: application/octet-stream; application/zip and
// application/x-zip-compressed, as some clients call an Office Open XML or
// OpenDocument file, which is a zip; application/vnd.ms-office, as some
// call an older Office file.
var GenericTypes = []string{
	"application/octet-stream",
	"application/zip",
	"application/x-zip-compressed",
	"application/vnd.ms-office",
}

// types are OfficeTypes and GenericTypes, as a set.
var types = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range append(append([]string(nil), OfficeTypes...), GenericTypes...) {
		m[t] = true
	}
	return m
}()

// Convertible reports whether Core converts the file named filename,
// declared contentType, to a PDF rendition: its extension is one of
// Extensions and its type one of OfficeTypes or GenericTypes, as Core's
// file_rendition_convertible has it.
func Convertible(filename, contentType string) bool {
	_, ok := Extensions[Extension(filename)]
	return ok && types[MediaType(contentType)]
}

// FormatOf is the format the file named filename is converted from, by its
// extension, and whether it is one Core converts. It says nothing of the
// declared type: Convertible does.
func FormatOf(filename string) (office.Format, bool) {
	f, ok := Extensions[Extension(filename)]
	return f, ok
}

// Extension is filename's extension as Core reads it
// (lower(substring(filename FROM '\.([^./\\]+)$'))): what follows its last
// dot, lower case, when that is not empty and holds no slash or backslash;
// "" otherwise.
func Extension(filename string) string {
	i := strings.LastIndexByte(filename, '.')
	if i < 0 {
		return ""
	}
	ext := filename[i+1:]
	if ext == "" || strings.ContainsAny(ext, `/\`) {
		return ""
	}
	return strings.ToLower(ext)
}

// MediaType is a declared content type as Core compares it
// (lower(btrim(split_part(content_type, ';', 1)))): what comes before its
// first semicolon, spaces trimmed from both ends, lower case.
func MediaType(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.Trim(mt, " "))
}
