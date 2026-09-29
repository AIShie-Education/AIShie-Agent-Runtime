package bedrock

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
)

// Converse's limits on files in one request, from AWS's documentation of
// ImageBlock and DocumentBlock. A file past them is described in text
// rather than sent, since Converse would refuse the whole call.
const (
	maxImages        = 20
	maxImageBytes    = 3_750_000
	maxDocuments     = 5
	maxDocumentBytes = 4_500_000
	// maxDocumentName bounds a document's name, which the model reads.
	maxDocumentName = 100
)

// imageFormats and documentFormats are the formats Converse takes, by MIME
// type.
var (
	imageFormats = map[string]string{
		"image/png": "png", "image/jpeg": "jpeg", "image/jpg": "jpeg", "image/gif": "gif", "image/webp": "webp",
	}
	documentFormats = map[string]string{
		"application/pdf":          "pdf",
		"application/msword":       "doc",
		"application/vnd.ms-excel": "xls",
		"text/csv":                 "csv",
		"text/html":                "html",
		"text/plain":               "txt",
		"text/markdown":            "md",
		"text/x-markdown":          "md",

		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       "xlsx",
	}
	// extensionFormats name a file's format from its name when its MIME
	// type does not (application/octet-stream).
	extensionFormats = map[string]string{
		".png": "png", ".jpg": "jpeg", ".jpeg": "jpeg", ".gif": "gif", ".webp": "webp",
		".pdf": "pdf", ".csv": "csv", ".doc": "doc", ".docx": "docx", ".xls": "xls", ".xlsx": "xlsx",
		".html": "html", ".htm": "html", ".txt": "txt", ".md": "md", ".markdown": "md",
	}
)

func isImageFormat(format string) bool {
	switch format {
	case "png", "jpeg", "gif", "webp":
		return true
	}
	return false
}

// fileState counts the files of one request and the document names given
// so far, which Converse requires to be unique.
type fileState struct {
	images, documents int
	names             map[string]bool
}

func newFileState() *fileState { return &fileState{names: map[string]bool{}} }

// fileBlocks are a file as Converse takes it: an image or document block
// when the model takes files and the file fits, its text when it is text,
// and a sentence saying it was left out otherwise. The model never gets a
// URL.
//
// An image or document comes after a line of text naming the file: an
// image has no name of its own in Converse, a document's name is only
// what AWS allows of it, and a message holding a document with no text
// block beside it may be refused.
func (a *Adapter) fileBlocks(f *llm.File, st *fileState) []block {
	b := a.fileBlock(f, st)
	if b.Image == nil && b.Document == nil {
		return []block{b}
	}
	label := fmt.Sprintf("[The file %q follows.]", f.Name)
	if b.Document != nil {
		label = fmt.Sprintf("[The file %q follows, as the document %q.]", f.Name, b.Document.Name)
	}
	return []block{{Text: label}, b}
}

// fileBlock is the block fileBlocks labels, or the text standing for it.
func (a *Adapter) fileBlock(f *llm.File, st *fileState) block {
	mime := mediaType(f.MIME)
	if len(f.Data) == 0 {
		return block{Text: fileNote(f, mime, "it is empty")}
	}
	if !a.caps.FileInput {
		if text, ok := asText(f, mime); ok {
			return block{Text: text}
		}
		return block{Text: fileNote(f, mime, "this model is not given files")}
	}
	format := fileFormat(f, mime)
	switch {
	case isImageFormat(format):
		if len(f.Data) > maxImageBytes {
			return block{Text: fileNote(f, mime, "an image may be at most 3.75 MB")}
		}
		if st.images >= maxImages {
			return block{Text: fileNote(f, mime, "the model takes at most 20 images at once")}
		}
		st.images++
		return block{Image: &imageBlock{Format: format, Source: source{Bytes: f.Data}}}
	case format != "":
		if len(f.Data) > maxDocumentBytes || st.documents >= maxDocuments {
			if text, ok := asText(f, mime); ok {
				return block{Text: text}
			}
			if len(f.Data) > maxDocumentBytes {
				return block{Text: fileNote(f, mime, "a document may be at most 4.5 MB")}
			}
			return block{Text: fileNote(f, mime, "the model takes at most 5 documents at once")}
		}
		st.documents++
		return block{Document: &documentBlock{Format: format, Name: st.uniqueName(f.Name), Source: source{Bytes: f.Data}}}
	}
	if text, ok := asText(f, mime); ok {
		return block{Text: text}
	}
	return block{Text: fileNote(f, mime, "the model does not take files of this type")}
}

// mediaType is a MIME type without its parameters, lower-cased.
func mediaType(mime string) string {
	t, _, _ := strings.Cut(mime, ";")
	return strings.ToLower(strings.TrimSpace(t))
}

// fileFormat is the Converse format of f, by MIME type, then by the name's
// extension; "" when Converse takes no such file.
func fileFormat(f *llm.File, mime string) string {
	if format, ok := imageFormats[mime]; ok {
		return format
	}
	if format, ok := documentFormats[mime]; ok {
		return format
	}
	return extensionFormats[strings.ToLower(path.Ext(f.Name))]
}

// asText is a text file's content with its name, for a model given no
// files; ok is false when f is not text.
func asText(f *llm.File, mime string) (string, bool) {
	textual := strings.HasPrefix(mime, "text/")
	switch mime {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml":
		textual = true
	}
	if !textual || !utf8.Valid(f.Data) {
		return "", false
	}
	return fmt.Sprintf("[The file %q follows.]\n%s", f.Name, f.Data), true
}

// fileNote is the sentence a file is replaced with when it cannot be given.
func fileNote(f *llm.File, mime, why string) string {
	if mime == "" {
		mime = "of unknown type"
	}
	return fmt.Sprintf("[The file %q (%s) is not included: %s.]", f.Name, mime, why)
}

// uniqueName is a document's name as Converse takes it, and not given
// before in this request. AWS allows only letters, digits, single spaces,
// hyphens, parentheses and square brackets, and the name reaches the model,
// so anything else becomes a space; the extension is dropped, since the
// format says it.
func (st *fileState) uniqueName(name string) string {
	base := sanitiseName(name)
	unique := base
	for n := 2; st.names[unique]; n++ {
		unique = fmt.Sprintf("%s (%d)", base, n)
	}
	st.names[unique] = true
	return unique
}

func sanitiseName(name string) string {
	if ext := path.Ext(name); ext != "" && ext != name {
		name = strings.TrimSuffix(name, ext)
	}
	var b strings.Builder
	space := false
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '(' || r == ')' || r == '[' || r == ']'
		if !ok {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
		if b.Len() >= maxDocumentName {
			break
		}
	}
	if b.Len() == 0 {
		return "document"
	}
	return b.String()
}
