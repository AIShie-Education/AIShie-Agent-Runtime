package toolset

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
)

// What an answer relied on (Core's docs/agent-runtime.md §2.10; design
// §5.3, What the answer relied on). conversation_answer takes the course
// materials an answer relied on, which Core keeps with it and shows its
// readers. Nothing tells the runtime what a model's answer rests on, so it
// says what the model was given: a course's material, instructions or
// rubric counts as relied on when the model's own document_get of it, in
// the answer's loop, gave the model some of what it holds (its own text,
// or a file's text or pages), in the order it was given. Each names the
// version Core gave, and the file where the call gave one file's content
// (the call's file_id, or a version of one file); a page or a slide where
// the call asked for that one page (file_pages) and was given it alone,
// or where the call is one a search hit on that page named; and the part
// of the file's text version, as document_text numbers its parts, where
// the runtime read the text in Core's parts and gave text of one of them
// alone. The runtime's own file_part is never a part.
//
// What it does not count: a document listed, or read and given nothing
// of (refused, purged, a file not given and no text of the version's
// own); the reads the runtime makes itself (a search's scope, a text
// version's parts); anything read for another attempt or another
// question; and a search's hits, whose excerpts are cut, and which the
// model is told to read before it relies on them: a hit counts once the
// model makes the call it names.

// Sources is one answer's account of the course's materials its model
// was given, in order (sources, above). The loop keeps one for the whole
// answer, across its turns, and Run adds to it between them, never two
// Runs at once, as Writes. Its zero value is ready; nil keeps nothing.
type Sources struct {
	read []core.Source
	// hits are where the passages of the search hits given are, by the
	// call that reads each.
	hits map[readKey]hitWhere
	// glimpsed is that the model was given what no source names: a
	// search's hits, or a reading of a course's material Core named no
	// version of.
	glimpsed bool
}

// List is what the answer relied on, for conversation_answer's sources:
// the materials given, in the order they were given, at most
// core.MaxSources; none, an empty list, where the model was given nothing
// of the course's materials; and nil, which says nothing, where it was
// given some that no source names (a search's hits it did not read) and
// nothing else.
func (s *Sources) List() []core.Source {
	switch {
	case s == nil:
		return nil
	case len(s.read) > 0:
		return slices.Clone(s.read)
	case s.glimpsed:
		return nil
	}
	return []core.Source{}
}

// materialGiven is what one call gave the model of the course's
// materials: a document_get's version, and its file, page or part
// (source); a reading that names none (unnamed); or a search's hits.
type materialGiven struct {
	source  *core.Source
	unnamed bool
	hits    []hitAt
}

// readKey is a document_get call as the model makes it: the version and
// the file it names, and the part (none for the first) or the pages of
// it.
type readKey struct {
	document, version, file string
	part, first, last       int
}

// hitWhere is the page or the slide a hit's passage is on (kind, n; ""
// and 0 where it is on neither); several is that the call that reads it
// reads hits on more than one.
type hitWhere struct {
	kind    string
	n       int
	several bool
}

// hitAt is a search hit given: the call that reads it, and where its
// passage is.
type hitAt struct {
	key  readKey
	kind string
	n    int
}

// add adds what the call key gave the model, rd, in the order the calls
// were made: its hits, kept by the call that reads each; and its source,
// with the page or the slide of the hits that call reads, where they are
// all on one and the call named none, unless it is there already or
// there are core.MaxSources.
func (s *Sources) add(key readKey, rd *materialGiven) {
	if s == nil || rd == nil {
		return
	}
	for _, h := range rd.hits {
		if s.hits == nil {
			s.hits = map[readKey]hitWhere{}
		}
		w, seen := s.hits[h.key]
		switch {
		case !seen:
			s.hits[h.key] = hitWhere{kind: h.kind, n: h.n}
		case w.kind != h.kind || w.n != h.n:
			w.several = true
			s.hits[h.key] = w
		}
	}
	s.glimpsed = s.glimpsed || rd.unnamed || len(rd.hits) > 0
	if rd.source == nil {
		return
	}
	src := *rd.source
	if w, ok := s.hits[key]; ok && !w.several && src.FileID != "" && src.Page == 0 && src.Slide == 0 {
		src.Page, src.Slide = locator(w.kind, w.n)
	}
	if len(s.read) < core.MaxSources && !slices.Contains(s.read, src) {
		s.read = append(s.read, src)
	}
}

// locator is the page or the slide n of a section of kind, as a source
// names it: a sheet, or anything else, is neither.
func locator(kind string, n int) (page, slide int) {
	switch kind {
	case doctext.SectionPage:
		return n, 0
	case doctext.SectionSlide:
		return 0, n
	}
	return 0, 0
}

// callKey is a prepared document_get's call as the model made it, its
// file arguments taken out (fa): what a hit's read call is matched by.
func callKey(args json.RawMessage, fa fileArgs) readKey {
	var a struct {
		DocumentID string `json:"document_id"`
		VersionID  string `json:"version_id"`
	}
	_ = json.Unmarshal(args, &a)
	return newReadKey(a.DocumentID, a.VersionID, fa.fileID, fa.part, fa.first, fa.last)
}

// hitKey is the call a hit's read names, as callKey makes the model's.
func hitKey(read *nextPart) readKey {
	str := func(k string) string { s, _ := read.Arguments[k].(string); return s }
	part, _ := read.Arguments[FilePartArg].(int)
	first, last, _ := pageSpan(str(FilePagesArg))
	return newReadKey(str("document_id"), str("version_id"), str(FileIDArg), part, first, last)
}

func newReadKey(document, version, file string, part, first, last int) readKey {
	if part <= 1 {
		part = 0
	}
	return readKey{document: strings.ToLower(document), version: strings.ToLower(version), file: strings.ToLower(file),
		part: part, first: first, last: last}
}

// materialOf is the version a document_get result gives of a course's
// material, instructions or rubric, as a source naming it alone, and
// whether the result holds its own text (body_md); nil for a document of
// another kind (a student's work, a grader's feedback), for a version or a
// document purged, and where Core gave no version. unnamed is that it
// gave one Core named no version of.
func materialOf(v any) (src *core.Source, body, unnamed bool) {
	m, _ := v.(map[string]any)
	version, _ := m["version"].(map[string]any)
	kind, _ := m["kind"].(string)
	if version == nil || kind != "material" && kind != "instructions" && kind != "rubric" {
		return nil, false, false
	}
	if p, _ := m["purged_at"].(string); p != "" {
		return nil, false, false
	}
	if p, ok := version["purged"].(map[string]any); ok && p != nil {
		return nil, false, false
	}
	text, _ := version["body_md"].(string)
	body = strings.TrimSpace(text) != ""
	documentID, _ := m["id"].(string)
	versionID, _ := version["id"].(string)
	if documentID == "" || versionID == "" {
		return nil, body, true
	}
	return &core.Source{DocumentID: documentID, VersionID: versionID}, body, false
}

// versionRead is what a document_get of the version src names gave the
// model where no file of it was given: the version, where its own text
// was (given); nothing otherwise.
func versionRead(src *core.Source, given, unnamed bool) *materialGiven {
	switch {
	case src != nil && given:
		return &materialGiven{source: src}
	case unnamed && given:
		return &materialGiven{unnamed: true}
	}
	return nil
}

// fileRead is what a document_get of the version src names gave the
// model where it gave g of the version's file d, asked for as fa: the
// version, its file where Core names it and g gave the file's content,
// the page or the slide asked for where g gave it alone (given.unit), and
// the part of its text version as Core numbers them (corePart); as
// versionRead where g gave nothing of the file.
func (r Runner) fileRead(src *core.Source, body, unnamed bool, d *docFile, g given, fa fileArgs) *materialGiven {
	if g.rec == nil || g.rec.GivenAs == givenNot {
		return versionRead(src, body, unnamed)
	}
	if src == nil {
		return versionRead(nil, true, unnamed)
	}
	s := *src
	if d.fileID == "" {
		// A Core before a version's files had ids: the version alone.
		return &materialGiven{source: &s}
	}
	s.FileID = d.fileID
	if fa.first > 0 && fa.first == fa.last {
		s.Page, s.Slide = locator(g.unit, fa.first)
	}
	s.Part = r.corePart(g)
	return &materialGiven{source: &s}
}

// corePart is the part of the file's text version, as document_text
// numbers its parts, that holds every byte of the text g gave the model:
// where the runtime read the text version in Core's parts itself
// (readTextParts) and gave text of one of them alone. 0 otherwise: the
// text version came whole beside the version, or what was given runs
// over two of Core's parts, or the file was not given as its text
// version. The runtime's own part (file_part, Part) is a range of the
// text as the runtime cuts it, which corePart only places.
func (r Runner) corePart(g given) int {
	rec := g.rec
	if rec == nil || rec.GivenAs != givenText || len(g.coreEnds) == 0 {
		return 0
	}
	lo, hi := 0, len(g.text)
	if rec.Part > 0 {
		parts := splitText(g.text, g.sections, r.partBudget())
		if rec.Part > len(parts) {
			return 0
		}
		lo, hi = parts[rec.Part-1].start, parts[rec.Part-1].end
	}
	lo, hi = lo+g.base, hi+g.base
	start := 0
	for i, end := range g.coreEnds {
		if lo >= start && hi <= end {
			return i + 1
		}
		start = end
	}
	return 0
}

// hitOf is h as a source may take it: the call that reads it, and the
// page or the slide its passage is on, where it is on one.
func hitOf(h hit, read *nextPart) hitAt {
	at := hitAt{key: hitKey(read)}
	if h.p.N > 0 && (h.p.Kind == doctext.SectionPage || h.p.Kind == doctext.SectionSlide) {
		at.kind, at.n = h.p.Kind, h.p.N
	}
	return at
}
