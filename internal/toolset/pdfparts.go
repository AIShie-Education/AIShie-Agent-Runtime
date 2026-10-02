package toolset

import (
	"context"
	"fmt"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/office"
)

// A PDF given as a file part is its pages as images and text to the
// model's provider, a slide or a page of a lecture a few thousand input
// tokens, which every later turn of the answer sends again. So a PDF of
// more pages than a part holds (Runner.PartPages) is given in parts of its
// pages, each a PDF of its own, cut by Runner.Office, as a long text is in
// parts of it: the first says how many there are and which pages each
// holds, and next_part is the call that reads the next.

const pdfMIME = "application/pdf"

// partPages is how many pages of a PDF one file part holds: PartPages,
// within what the model's provider takes in one file.
func (r Runner) partPages() int {
	n := r.PartPages
	if n <= 0 {
		n = office.DefaultPartPages
	}
	if lim := r.PDFLimits.PDFPages; lim > 0 {
		n = min(n, lim)
	}
	return n
}

// cuts reports whether this runner cuts PDFs into parts of their pages.
func (r Runner) cuts() bool { return r.Office != nil && r.Office.Cuts() }

// pdfFile is a PDF to give a model that takes files.
type pdfFile struct {
	data []byte
	// sum names it, for the parts of it kept (Office.Range).
	sum string
	// pages it has, 0 when they could not be counted; unit is what a page
	// of it is, doctext.SectionSlide for a deck's.
	pages int
	unit  string
	// converted says it is a PDF made of the file, Core's or
	// LibreOffice's; capped that it stopped at the pages a PDF given is
	// let have (OFFICE_PDF_MAX_PAGES), and the file may have more.
	converted, capped bool
	// slides are a deck's slides as the runtime read them, whose speaker
	// notes are given beside the pages that show them; notesWhy says why
	// they are not, when the deck could not be read.
	slides   []doctext.SlideInfo
	notesWhy string
}

// givePDFFile gives p to the model as a file part: whole when it has at
// most a part's pages, or when the runner cuts no PDF; otherwise part k of
// it (part, from 1; 0 is 1), pages (k-1)·n+1 to k·n, a PDF of its own. A
// deck's speaker notes of the slides given are file_text beside it. ok is
// false when p is not given as a file (the caller gives its text): whole,
// it has more pages than the model's provider takes, or its pages could not
// be cut.
func (r Runner) givePDFFile(ctx context.Context, g given, d *docFile, p pdfFile, part int) (given, bool) {
	if d.first > 0 {
		return r.givePDFPages(ctx, g, d, p)
	}
	rec := g.rec
	per := r.partPages()
	if p.pages <= per || !r.cuts() {
		if r.pagesPast(p.pages, "") != "" {
			return g, false
		}
		rec.GivenAs = givenFile
		g.file, g.filePages = &llm.File{Name: fileName(d.title, pdfMIME), MIME: pdfMIME, Data: p.data}, max(p.pages, 1)
		if p.converted {
			rec.ConvertedTo = "pdf"
			rec.Note = "LibreOffice converted it to PDF, which is given: " + looks(p)
		}
		notesBeside(&g, p, 1, max(p.pages, len(p.slides)))
		return g, true
	}
	parts := (p.pages + per - 1) / per
	k := max(part, 1)
	rec.Parts = parts
	if k > parts {
		rec.Note = fmt.Sprintf("the file is given as a PDF in %d parts of its %ss: there is no part %d; ask for %s from 1 to %d",
			parts, p.unit, k, d.partArg(), parts)
		return g, true
	}
	first, last := (k-1)*per+1, min(k*per, p.pages)
	b, err := r.Office.Range(ctx, p.sum, p.data, first, last)
	if err != nil {
		rec.Parts = 0
		return g, false
	}
	rec.Part, rec.PartHolds = k, pageRange(p.unit, first, last)
	if k < parts {
		rec.NextPart = d.again(k + 1)
	}
	var b2 strings.Builder
	if p.converted {
		rec.ConvertedTo = "pdf"
		b2.WriteString("LibreOffice converted it to PDF, which is given: " + looks(p) + "; ")
	}
	if k == 1 {
		fmt.Fprintf(&b2, "it is given in %d parts of %d %ss each, so that no one result costs the model too much: this is part 1, %s", parts, per, p.unit, rec.PartHolds)
		if parts <= maxPartsListed {
			var rest []string
			for n := 2; n <= parts; n++ {
				rest = append(rest, fmt.Sprintf("%d is %s", n, pageRange(p.unit, (n-1)*per+1, min(n*per, p.pages))))
			}
			b2.WriteString("; of the others, " + strings.Join(rest, ", "))
		}
	} else {
		fmt.Fprintf(&b2, "the file part is part %d of %d: %s", k, parts, rec.PartHolds)
	}
	if k < parts {
		b2.WriteString(d.readNext(k + 1))
	} else {
		b2.WriteString("; it is the last")
	}
	rec.Note = b2.String()
	if past := r.bytesPast(int64(len(b)), "its "+rec.PartHolds+" are"); past != "" {
		rec.Note = notGiven + past + "; " + rec.Note
		return g, true
	}
	rec.GivenAs = givenFile
	g.file, g.filePages = &llm.File{Name: fileName(partName(d.title, rec.PartHolds), pdfMIME), MIME: pdfMIME, Data: b}, last-first+1
	notesBeside(&g, p, first, last)
	return g, true
}

// givePDFPages gives the pages of p the model asked for (FilePagesArg),
// d.first to d.last, as a PDF of their own, with a deck's speaker notes of
// them beside it; the whole PDF where the runner cuts none, and it is
// within what the model's provider takes, which names no page for the
// answer's sources (given.unit), every page being given. ok is false when
// they are not given as a file (the caller gives its text).
func (r Runner) givePDFPages(ctx context.Context, g given, d *docFile, p pdfFile) (given, bool) {
	rec := g.rec
	g.pages = true
	first, last := d.first, d.last
	if p.pages > 0 && first > p.pages {
		rec.Note = fmt.Sprintf("the file has %s: there is no %s %d; ask for %s from 1 to %d", plural(p.pages, p.unit), p.unit, first,
			d.pagesArg(), p.pages)
		return g, true
	}
	if p.pages > 0 {
		last = min(last, p.pages)
	}
	var b strings.Builder
	if p.converted {
		rec.ConvertedTo = "pdf"
		b.WriteString("LibreOffice converted it to PDF; ")
	}
	if !r.cuts() {
		if r.pagesPast(p.pages, "") != "" || r.bytesPast(int64(len(p.data)), "") != "" {
			g.pages = false
			return g, false
		}
		rec.GivenAs = givenFile
		b.WriteString("its pages cannot be cut here, so the whole PDF is given: see " + pageRange(p.unit, first, last) + " in it")
		rec.Note = b.String()
		g.file, g.filePages = &llm.File{Name: fileName(d.title, pdfMIME), MIME: pdfMIME, Data: p.data}, max(p.pages, 1)
		notesBeside(&g, p, first, last)
		return g, true
	}
	data, err := r.Office.Range(ctx, p.sum, p.data, first, last)
	if err != nil {
		g.pages = false
		return g, false
	}
	rec.PartHolds = pageRange(p.unit, first, last)
	fmt.Fprintf(&b, "the file's %s are given as a PDF of their own, as they look", rec.PartHolds)
	rec.Note = b.String()
	if past := r.bytesPast(int64(len(data)), "they are"); past != "" {
		rec.Note = notGiven + past + "; ask for fewer pages"
		return g, true
	}
	rec.GivenAs, g.unit = givenFile, p.unit
	g.file, g.filePages = &llm.File{Name: fileName(partName(d.title, rec.PartHolds), pdfMIME), MIME: pdfMIME, Data: data}, last-first+1
	notesBeside(&g, p, first, last)
	return g, true
}

// looks says what a PDF LibreOffice made shows.
func looks(p pdfFile) string {
	s := fmt.Sprintf("its %s as they look, pictures, charts and drawings with them", plural(p.pages, p.unit))
	if p.pages == 0 {
		s = "its pages as they look, pictures, charts and drawings with them"
	}
	if p.capped {
		s += fmt.Sprintf(" (the first %d, the most the runtime converts: it may have more)", p.pages)
	}
	return s
}

// pageRange names pages first to last: "slides 11–20", "page 7".
func pageRange(unit string, first, last int) string {
	if first == last {
		return fmt.Sprintf("%s %d", unit, first)
	}
	return fmt.Sprintf("%ss %d–%d", unit, first, last)
}

// notesBeside gives, beside a deck's PDF, the speaker notes of its slides
// first to last, which the PDF does not show, as file_text; or says why
// there are none to give.
func notesBeside(g *given, p pdfFile, first, last int) {
	rec := g.rec
	if p.notesWhy != "" {
		rec.Note += "; its speaker notes are not given: " + p.notesWhy
		return
	}
	var b strings.Builder
	var sections []doctext.Section
	for _, s := range p.slides {
		if s.N < first || s.N > last || s.Notes == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		sections = append(sections, doctext.Section{Kind: doctext.SectionSlide, N: s.N, Offset: b.Len()})
		fmt.Fprintf(&b, "## Slide %d\nNotes: %s", s.N, s.Notes)
	}
	if b.Len() == 0 {
		return
	}
	g.text, g.sections, g.aside = b.String(), sections, true
	rec.Note += "; file_text holds the speaker notes of these slides, which the PDF does not show"
}

// partName names pages of a file given as a PDF of their own: the file's
// name, without ".pdf" where it has it, and the pages.
func partName(title, holds string) string {
	if strings.HasSuffix(strings.ToLower(title), ".pdf") {
		title = title[:len(title)-len(".pdf")]
	}
	if title == "" {
		title = "document"
	}
	return title + " (" + holds + ")"
}
