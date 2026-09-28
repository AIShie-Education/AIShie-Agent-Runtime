package doctext

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
)

// An Office Open XML file is a package (Open Packaging Conventions, ECMA-376
// part 2): a zip archive of XML parts, tied together by relationships. The
// package is read from memory, one part at a time, within the budget: the
// archive's entries are never written anywhere, so a name is only ever a key
// here, and one that would climb out of the package (absolute, or with ..)
// makes the whole file malformed.

// opc is an opened package.
type opc struct {
	b *budget
	// parts are the archive's entries by name, in lower case: part names
	// are case-insensitive.
	parts map[string]*zip.File
	types *contentTypes
}

// openOPC opens data as a package, refusing an archive of more entries than
// the limit, an entry encrypted, a name that is not a plain relative path,
// and a name given twice.
func openOPC(data []byte, b *budget) (*opc, error) {
	if isCFB(data) {
		return nil, cfbError(data)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, malformedf("it is not a zip archive")
	}
	if zr == nil || errors.Is(err, zip.ErrInsecurePath) {
		return nil, malformedf("an entry of its archive has an unsafe name")
	}
	if len(zr.File) > b.lim.MaxEntries {
		return nil, limitf("its archive holds more than %d entries", b.lim.MaxEntries)
	}
	p := &opc{b: b, parts: make(map[string]*zip.File, len(zr.File))}
	for _, f := range zr.File {
		if !safeName(f.Name) {
			return nil, malformedf("an entry of its archive has an unsafe name")
		}
		if f.Flags&0x1 != 0 {
			return nil, errEncryptedEntry
		}
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		name := strings.ToLower(f.Name)
		if _, dup := p.parts[name]; dup {
			return nil, malformedf("its archive names an entry twice")
		}
		p.parts[name] = f
	}
	return p, nil
}

// errEncryptedEntry is ErrEncrypted, said of a zip archive.
var errEncryptedEntry = fmt.Errorf("%w: an entry of its archive is encrypted", ErrEncrypted)

// safeName reports whether name is a plain relative path within the
// archive: not empty, not absolute, no backslash, NUL or .. element, and
// already clean.
func safeName(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") {
		return false
	}
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || path.Clean(trimmed) != trimmed {
		return false
	}
	for _, el := range strings.Split(trimmed, "/") {
		if el == ".." || el == "." {
			return false
		}
	}
	return true
}

// has reports whether the package holds a part named name.
func (p *opc) has(name string) bool {
	_, ok := p.parts[strings.ToLower(name)]
	return ok
}

// open reads the part named name: its bytes decompressed, at most what the
// budget leaves for one entry, each byte spent from the budget. A part the
// package does not hold is nil and no error.
func (p *opc) open(name string) (io.ReadCloser, error) {
	f := p.parts[strings.ToLower(name)]
	if f == nil {
		return nil, nil
	}
	if f.UncompressedSize64 > uint64(p.b.left()) { //nolint:gosec // left is never negative.
		return nil, limitf("a part of it decompresses to more than the runtime reads of one (%d MiB)", p.b.lim.MaxEntry>>20)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, malformedf("a part of it cannot be decompressed")
	}
	return &partReader{rc: rc, b: p.b, left: p.b.left()}, nil
}

// partReader reads a part within the budget: past it, the read fails with
// ErrLimit, whatever the archive said of the part's size.
type partReader struct {
	rc   io.ReadCloser
	b    *budget
	left int64
}

func (r *partReader) Read(buf []byte) (int, error) {
	if r.left <= 0 {
		// One byte more than allowed tells a part at the limit from one
		// past it.
		var one [1]byte
		if n, _ := r.rc.Read(one[:]); n > 0 {
			return 0, limitf("a part of it decompresses to more than the runtime reads")
		}
		return 0, io.EOF
	}
	if int64(len(buf)) > r.left {
		buf = buf[:r.left]
	}
	n, err := r.rc.Read(buf)
	r.left -= int64(n)
	if berr := r.b.inflate(int64(n)); berr != nil {
		return n, berr
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, malformedf("a part of it cannot be decompressed")
	}
	return n, err
}

func (r *partReader) Close() error { return r.rc.Close() }

// closeQuietly closes what was read from memory: its error says nothing.
func closeQuietly(c io.Closer) { _ = c.Close() }

// rel is a relationship of a part to another.
type rel struct {
	id, typ string
	// target is the part it names, resolved within the package; "" for a
	// target outside the package (TargetMode External), which is never
	// followed, or one that would climb out of it.
	target string
}

// is reports whether the relationship is of the kind named by the last
// element of its type: slide, notesSlide, chart, image, ….
func (r rel) is(kind string) bool { return strings.HasSuffix(r.typ, "/"+kind) }

// rels are the relationships of the part named source ("" for the package
// itself), in the order written: none when it has no relationships part.
func (p *opc) rels(source string) ([]rel, error) {
	dir, base := path.Split(source)
	name := dir + "_rels/" + base + ".rels"
	rc, err := p.open(name)
	if rc == nil || err != nil {
		return nil, err
	}
	defer closeQuietly(rc)
	var out []rel
	x := newXReader(rc, p.b)
	for {
		tok, err := x.token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Relationship" {
			continue
		}
		r := rel{id: attr(se, "Id"), typ: attr(se, "Type")}
		if !strings.EqualFold(attr(se, "TargetMode"), "External") {
			r.target = resolve(dir, attr(se, "Target"))
		}
		out = append(out, r)
	}
}

// relMap is rels by id.
func relMap(rs []rel) map[string]rel {
	m := make(map[string]rel, len(rs))
	for _, r := range rs {
		if _, dup := m[r.id]; !dup {
			m[r.id] = r
		}
	}
	return m
}

// resolve is target, a relationship's target written relative to the
// directory dir of its source (or from the package's root when it begins
// with /), as a part name: "" when it is not a path within the package.
func resolve(dir, target string) string {
	if target == "" {
		return ""
	}
	if u, err := url.PathUnescape(target); err == nil {
		target = u
	}
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if strings.Contains(target, ":") || strings.Contains(target, "\\") {
		// A scheme (http:, file:) is outside the package, whatever its
		// TargetMode says.
		return ""
	}
	var joined string
	if strings.HasPrefix(target, "/") {
		joined = path.Clean(strings.TrimLeft(target, "/"))
	} else {
		joined = path.Clean(path.Join(dir, target))
	}
	if joined == "." || joined == ".." || strings.HasPrefix(joined, "../") || strings.HasPrefix(joined, "/") {
		return ""
	}
	return joined
}

// mainPart is the part the package names as its document (the
// officeDocument relationship of the package), or def when it names none.
func (p *opc) mainPart(def string) string {
	rs, err := p.rels("")
	if err != nil {
		return def
	}
	for _, r := range rs {
		if r.is("officeDocument") && r.target != "" && p.has(r.target) {
			return r.target
		}
	}
	return def
}

// format is what the package is, by its document's content type, or by
// the part each application names its document when the package does not
// say; "" when it is none of the three.
func (p *opc) format() Format {
	main := p.mainPart("")
	if main != "" {
		ct := p.contentType(main)
		switch {
		case strings.Contains(ct, "presentationml"):
			return PPTX
		case strings.Contains(ct, "wordprocessingml"):
			return DOCX
		case strings.Contains(ct, "spreadsheetml"):
			return XLSX
		}
	}
	for _, c := range []struct {
		part string
		f    Format
	}{{"ppt/presentation.xml", PPTX}, {"word/document.xml", DOCX}, {"xl/workbook.xml", XLSX}} {
		if p.has(c.part) {
			return c.f
		}
	}
	return ""
}

// contentTypes is [Content_Types].xml: a part's type by its name, or by its
// extension.
type contentTypes struct {
	byName, byExt map[string]string
}

// contentType is the type the package gives the part named name; "" when
// it gives none.
func (p *opc) contentType(name string) string {
	if p.types == nil {
		p.types = &contentTypes{byName: map[string]string{}, byExt: map[string]string{}}
		if rc, err := p.open("[Content_Types].xml"); rc != nil && err == nil {
			x := newXReader(rc, p.b)
			for {
				tok, err := x.token()
				if err != nil {
					break
				}
				se, ok := tok.(xml.StartElement)
				if !ok {
					continue
				}
				switch se.Name.Local {
				case "Override":
					p.types.byName[strings.ToLower(strings.TrimLeft(attr(se, "PartName"), "/"))] = strings.ToLower(attr(se, "ContentType"))
				case "Default":
					p.types.byExt[strings.ToLower(attr(se, "Extension"))] = strings.ToLower(attr(se, "ContentType"))
				}
			}
			closeQuietly(rc)
		}
	}
	if ct, ok := p.types.byName[strings.ToLower(name)]; ok {
		return ct
	}
	return p.types.byExt[strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))]
}

// xreader reads XML tokens within the budget: each token spends one, and
// elements may nest at most MaxDepth deep. encoding/xml expands no entity
// but XML's own five, so a document type's declarations cannot grow the
// text; a character set other than UTF-8 is refused.
type xreader struct {
	d     *xml.Decoder
	b     *budget
	depth int
}

func newXReader(r io.Reader, b *budget) *xreader {
	d := xml.NewDecoder(r)
	d.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(label, "utf-8") || strings.EqualFold(label, "utf8") {
			return input, nil
		}
		return nil, malformedf("a part of it is in the character set %q", cut(label, 20))
	}
	return &xreader{d: d, b: b}
}

// token is the next token; io.EOF at the end. CharData is copied, so it
// stays valid.
func (x *xreader) token() (xml.Token, error) {
	tok, err := x.d.Token()
	if err != nil {
		switch {
		case errors.Is(err, io.EOF):
			return nil, io.EOF
		case errors.Is(err, ErrLimit), errors.Is(err, ErrMalformed), errors.Is(err, ErrEncrypted):
			return nil, err
		}
		return nil, malformedf("a part of it is not well-formed XML")
	}
	switch t := tok.(type) {
	case xml.StartElement:
		x.depth++
		if x.depth > x.b.lim.MaxDepth {
			return nil, limitf("its XML nests more than %d deep", x.b.lim.MaxDepth)
		}
	case xml.EndElement:
		x.depth--
	case xml.CharData:
		tok = xml.CharData(bytes.Clone(t))
	}
	if err := x.b.tick(); err != nil {
		return nil, err
	}
	return tok, nil
}

// skip reads to the end of the element whose start was the last token.
func (x *xreader) skip() error {
	for depth := 1; depth > 0; {
		tok, err := x.token()
		if err != nil {
			return eofMalformed(err)
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

// eofMalformed is err, with an end of file inside an element made the
// malformation it is.
func eofMalformed(err error) error {
	if errors.Is(err, io.EOF) {
		return malformedf("a part of it ends inside an element")
	}
	return err
}

// attr is the value of the attribute of se named local with no namespace,
// or with any namespace when none has none.
func attr(se xml.StartElement, local string) string {
	var any string
	for _, a := range se.Attr {
		if a.Name.Local != local {
			continue
		}
		if a.Name.Space == "" {
			return a.Value
		}
		if any == "" {
			any = a.Value
		}
	}
	return any
}

// relAttr is the value of se's attribute local in the namespace of
// relationships (r:id, r:embed): the id of a relationship of the part.
func relAttr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local && strings.HasSuffix(a.Name.Space, "/relationships") {
			return a.Value
		}
	}
	return ""
}

// cut shortens s to at most n bytes, on a rune boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
