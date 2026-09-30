package toolset

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
)

// A file's text too long for one result is given in parts (design §4,
// Files): the text is cut, the same way every time, into parts that each
// fit a result, on the slides, pages or sheets doctext says begin where,
// else on paragraphs, lines, and at worst characters; each part is given
// with its number, how many there are, which slides, pages or sheets it
// holds, and the call that reads the next. The model asks for a part by
// FilePartArg, an argument the runtime adds to document_get and takes out
// again before the call reaches Core: Core is asked every time, with the
// caller's own token, so a part is given only of a version the caller may
// still read.

// FilePartTool is the tool the runtime adds FilePartArg to.
const FilePartTool = "document_get"

// FilePartArg is the runtime's own argument of FilePartTool: which part of
// the file's text to give, from 1.
const FilePartArg = "file_part"

// FilePagesArg is the runtime's other argument of FilePartTool: pages of
// the file itself to give, as a PDF of their own, where the version's text
// version is what the model reads (textversion.go).
const FilePagesArg = "file_pages"

// filePartProperty is FilePartArg as the model is shown it.
var filePartProperty = map[string]any{
	"type":    []any{"null", "integer"},
	"minimum": 1,
	"description": "which part of the file to read, from 1, when it is too long for one result (a range of its text, or of a PDF's " +
		"pages): file.parts says how many there are and file.next_part is the call that reads the next; omit it for the first",
}

// filePagesProperty is FilePagesArg as the model is shown it.
var filePagesProperty = map[string]any{
	"type": []any{"null", "string"},
	"description": "pages of the file itself to see, as a PDF of their own, such as \"3\" or \"3-5\" (at most " +
		strconv.Itoa(maxFilePages) + " at a time): where file.text_source says file_text is the document's text version, to check a " +
		"page, a figure or a formula against the file; omit it to read the text",
}

// maxFilePages bounds the pages FilePagesArg asks for at once; so does
// what the model's provider takes in one file (Runner.partPages).
const maxFilePages = 10

// withFilePart is Core's input schema of FilePartTool with FilePartArg and
// FilePagesArg added, as the model is shown it. Core's schema naming
// either itself is an error: the two would be one argument.
func withFilePart(schema json.RawMessage) (json.RawMessage, error) {
	v, err := decodeJSON(schema)
	if err != nil {
		return nil, fmt.Errorf("toolset: %s's schema: %w", FilePartTool, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("toolset: %s's schema is not an object", FilePartTool)
	}
	props, _ := m["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		m["properties"] = props
	}
	for _, arg := range []string{FilePartArg, FilePagesArg} {
		if _, taken := props[arg]; taken {
			return nil, fmt.Errorf("toolset: Core's %s now takes %s itself, an argument the runtime adds for reading a file", FilePartTool, arg)
		}
	}
	props[FilePartArg] = filePartProperty
	props[FilePagesArg] = filePagesProperty
	return json.RawMessage(encodeJSON(m)), nil
}

// errFilePart is a FilePartArg that is not a whole number from 1.
var errFilePart = errors.New("toolset: " + FilePartArg + " must be a whole number from 1")

// fileArgs are what the model asked of a document's file by the runtime's
// own arguments: the part of it (FilePartArg), 0 for none, or its pages
// first to last (FilePagesArg), 0 for none; and the course the call is
// in, which a text version's parts are read in.
type fileArgs struct {
	part        int
	first, last int
	courseID    string
}

// filePagesError is a FilePagesArg that names no pages, or too many.
type filePagesError struct{ most int }

func (e filePagesError) Error() string {
	return fmt.Sprintf("toolset: %s names pages of the file, such as \"3\" or \"3-5\", at most %d at a time", FilePagesArg, e.most)
}

// takeFileArgs takes FilePartArg and FilePagesArg out of a call's
// arguments, which must be a JSON object (or nothing): the part asked for,
// or the pages, at most most of them (null is none), and the arguments
// without them, for Core.
func takeFileArgs(args json.RawMessage, most int) (fileArgs, json.RawMessage, error) {
	var fa fileArgs
	if len(strings.TrimSpace(string(args))) == 0 {
		return fa, args, nil
	}
	v, err := decodeJSON(args)
	if err != nil {
		return fa, args, nil // prepare says what is wrong with them
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fa, args, nil
	}
	rawPart, hasPart := m[FilePartArg]
	rawPages, hasPages := m[FilePagesArg]
	if !hasPart && !hasPages {
		return fa, args, nil
	}
	delete(m, FilePartArg)
	delete(m, FilePagesArg)
	out := json.RawMessage(encodeJSON(m))
	if rawPart != nil {
		num, ok := rawPart.(json.Number)
		if !ok {
			return fa, out, errFilePart
		}
		n, err := num.Int64()
		if err != nil || n < 1 || n > 1<<20 {
			return fa, out, errFilePart
		}
		fa.part = int(n)
	}
	if rawPages != nil {
		s, _ := rawPages.(string)
		first, last, ok := pageSpan(s)
		if !ok || last-first+1 > most {
			return fa, out, filePagesError{most}
		}
		fa.first, fa.last = first, last
	}
	if fa.part > 0 && fa.first > 0 {
		return fa, out, errBothFileArgs
	}
	return fa, out, nil
}

// errBothFileArgs is a call that asks for a part of the file's text and
// pages of the file at once.
var errBothFileArgs = errors.New("toolset: " + FilePartArg + " and " + FilePagesArg + " are not asked for together")

// pageSpan reads pages as FilePagesArg names them: "3", or "3-5" (with a
// dash of any kind, and spaces), from 1.
func pageSpan(s string) (first, last int, ok bool) {
	s = strings.TrimSpace(s)
	a, b, span := strings.Cut(s, "-")
	if !span {
		for _, dash := range []string{"–", "—", "~", "～"} {
			if a, b, span = strings.Cut(s, dash); span {
				break
			}
		}
	}
	if !span {
		b = a
	}
	first, err1 := strconv.Atoi(strings.TrimSpace(a))
	last, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || first < 1 || last < first || last > 1<<20 {
		return 0, 0, false
	}
	return first, last, true
}

// textPart is one part of a text: text[start:end].
type textPart struct{ start, end int }

// partBudget is how many bytes of text, written as a JSON string, one part
// holds: the result's limit less what the rest of the result may take,
// its envelope and the file's record. It depends on the limit alone, so
// that a text is cut the same way at every call.
func (r Runner) partBudget() int {
	reserve := min(8<<10, r.MaxResultBytes/4)
	return max(r.MaxResultBytes-reserve, minTextRoom)
}

// splitText cuts text into parts of at most budget bytes each, written as
// a JSON string without its quotes, which together are text exactly,
// nothing lost and nothing given twice. Each ends, in this order of
// preference, where a section begins (a slide, a page, a sheet), after an
// empty line, after a line, or between two characters: the first of these
// that fills at least half the budget, else the last that fits.
func splitText(text string, sections []doctext.Section, budget int) []textPart {
	budget = max(budget, 16)
	var parts []textPart
	for start := 0; start < len(text); {
		// The first section that may end this part: one beginning after
		// its start.
		k := sort.Search(len(sections), func(i int) bool { return sections[i].Offset > start })
		type cutAt struct{ pos, size int }
		var unit, para, line, char cutAt
		size, i := 0, start
		for i < len(text) {
			c, w := utf8.DecodeRuneInString(text[i:])
			n := escapedLen(c, w)
			if size+n > budget {
				break
			}
			size += n
			i += w
			char = cutAt{i, size}
			if c == '\n' {
				line = cutAt{i, size}
				if i-start >= 2 && text[i-2] == '\n' {
					para = cutAt{i, size}
				}
			}
			for k < len(sections) && sections[k].Offset < i {
				k++
			}
			if k < len(sections) && sections[k].Offset == i {
				unit = cutAt{i, size}
			}
		}
		if i == len(text) {
			parts = append(parts, textPart{start, len(text)})
			break
		}
		end := char.pos
		for _, c := range []cutAt{unit, para, line} {
			if c.pos > start && c.size >= budget/2 {
				end = c.pos
				break
			}
		}
		if end <= start {
			// Not reached for a budget of 16 bytes or more: no one
			// character is written in more than 6.
			_, w := utf8.DecodeRuneInString(text[start:])
			end = start + w
		}
		parts = append(parts, textPart{start, end})
		start = end
	}
	return parts
}

// escapedLen is how many bytes a character, w bytes of UTF-8, takes in a
// JSON string as encodeJSON writes one: HTML's characters as they are.
func escapedLen(c rune, w int) int {
	switch {
	case c == utf8.RuneError && w == 1:
		return len(`�`)
	case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t':
		return 2
	case c < 0x20, c == ' ', c == ' ':
		return 6
	}
	return w
}

// escapedLenOf is escapedLen of every character of s.
func escapedLenOf(s string) int {
	n := 0
	for i := 0; i < len(s); {
		c, w := utf8.DecodeRuneInString(s[i:])
		n += escapedLen(c, w)
		i += w
	}
	return n
}

// partHolds says which slides, pages or sheets p holds of a text whose
// sections are these: "slides 3–7", "the end of slide 8", "" when the text
// has none.
func partHolds(p textPart, sections []doctext.Section, textLen int) string {
	if len(sections) == 0 {
		return ""
	}
	// The section p begins in (the first, if p begins before any), and
	// the one it ends in.
	i := max(sort.Search(len(sections), func(i int) bool { return sections[i].Offset > p.start })-1, 0)
	j := max(sort.Search(len(sections), func(i int) bool { return sections[i].Offset >= p.end })-1, i)
	endOf := func(k int) int {
		if k+1 < len(sections) {
			return sections[k+1].Offset
		}
		return textLen
	}
	fromMiddle := p.start > sections[i].Offset
	toMiddle := p.end < endOf(j)
	name := func(k int) string { return sections[k].Kind + " " + fmt.Sprint(sections[k].N) }
	switch {
	case i == j && fromMiddle && toMiddle:
		return "part of " + name(i)
	case i == j && fromMiddle:
		return "the end of " + name(i)
	case i == j && toMiddle:
		return "the start of " + name(i)
	case i == j:
		return name(i)
	case !fromMiddle && !toMiddle && sections[i].Kind == sections[j].Kind:
		return fmt.Sprintf("%ss %d–%d", sections[i].Kind, sections[i].N, sections[j].N)
	}
	from, to := name(i), name(j)
	if fromMiddle {
		from = "the end of " + from
	}
	if toMiddle {
		to = "the start of " + to
	}
	return from + " to " + to
}

// nextPart is the call that reads a part of a file's text: the model makes
// it as it is.
type nextPart struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// pageText gives part k (from 1; 0 is 1) of a text too long for the room
// a result leaves: rec says which part it is, how many there are, what it
// holds and how to read the next, and the part's text is returned. A part
// that does not exist gives no text, and rec says which do.
func (r Runner) pageText(rec *fileRecord, d *docFile, text string, sections []doctext.Section, k int) string {
	parts := splitText(text, sections, r.partBudget())
	k = max(k, 1)
	rec.Parts = len(parts)
	if k > len(parts) {
		rec.GivenAs, rec.ExtractedFrom = givenNot, ""
		if len(parts) == 1 {
			rec.Note = fmt.Sprintf("the file's text is one part: there is no part %d; call %s without %s to read it", k, FilePartTool, FilePartArg)
		} else {
			rec.Note = fmt.Sprintf("the file's text has %d parts: there is no part %d; ask for %s from 1 to %d", len(parts), k, FilePartArg, len(parts))
		}
		return ""
	}
	p := parts[k-1]
	rec.Part = k
	rec.PartHolds = partHolds(p, sections, len(text))
	var b strings.Builder
	if k == 1 {
		fmt.Fprintf(&b, "the text is too long for one result, so it is given in %d parts; file_text is part 1", len(parts))
	} else {
		fmt.Fprintf(&b, "file_text is part %d of %d", k, len(parts))
	}
	if rec.PartHolds != "" {
		b.WriteString(": " + rec.PartHolds)
	}
	if k == 1 && len(sections) > 0 && len(parts) <= maxPartsListed {
		var rest []string
		for n := 2; n <= len(parts); n++ {
			rest = append(rest, fmt.Sprintf("%d is %s", n, partHolds(parts[n-1], sections, len(text))))
		}
		b.WriteString("; of the others, " + strings.Join(rest, ", "))
	}
	if k < len(parts) {
		args := map[string]any{"document_id": d.documentID, FilePartArg: k + 1}
		if d.versionID != "" {
			args["version_id"] = d.versionID
		}
		rec.NextPart = &nextPart{Tool: FilePartTool, Arguments: args}
		fmt.Fprintf(&b, "; to read part %d, call %s with next_part's arguments, which name this version", k+1, FilePartTool)
	} else {
		b.WriteString("; it is the last")
	}
	if rec.Note != "" {
		rec.Note += "; "
	}
	rec.Note += b.String()
	return text[p.start:p.end]
}

// maxPartsListed bounds the parts the first part lists by what they hold.
const maxPartsListed = 20
