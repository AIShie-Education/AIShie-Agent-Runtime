package doctext

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A Word document's text: its body in order, a paragraph a line, headings
// as # to ######, list items as - or 1. indented by their level, tables as
// Markdown, text boxes after the paragraph that holds them, pictures as
// [image] and charts as [chart]; its footnotes and endnotes after the body,
// each once, where the body marks them [^n] and [^en]; then its headers and
// footers, each text once however many sections repeat it. Deleted text
// and field codes are left out, and so are comments.

// wstyle is what a paragraph style says of a paragraph's structure.
type wstyle struct {
	name, basedOn string
	outline       int
	numID, ilvl   string
}

// docxReader reads one document's parts.
type docxReader struct {
	p        *opc
	res      *Result
	styles   map[string]*wstyle
	bullets  map[string]map[string]bool // numId → ilvl → bulleted
	counters map[string]*[9]int
}

// sink takes a document's lines, as the body or a text box writes them.
type sink interface {
	line(s string)
	para()
}

// lines is a sink that keeps the lines, for a text box or a table's cell.
type lines struct{ l []string }

func (ls *lines) line(s string) { ls.l = append(ls.l, s) }
func (ls *lines) para()         {}

func readDOCX(data []byte, b *budget) (*Result, error) {
	p, err := openOPC(data, b)
	if err != nil {
		return nil, err
	}
	main := p.mainPart("word/document.xml")
	if !p.has(main) {
		return nil, malformedf("it holds no document")
	}
	rs, err := p.rels(main)
	if err != nil {
		return nil, err
	}
	r := &docxReader{p: p, res: &Result{Format: DOCX, Parts: 1, Of: 1}, counters: map[string]*[9]int{}}
	for _, rl := range rs {
		switch {
		case rl.is("styles") && rl.target != "":
			if r.styles, err = readStyles(p, rl.target); err != nil {
				return nil, err
			}
		case rl.is("numbering") && rl.target != "":
			if r.bullets, err = readNumbering(p, rl.target); err != nil {
				return nil, err
			}
		}
	}
	out := newTextOut(b.lim.MaxText)
	if err := r.part(main, "body", out); err != nil {
		if !cutShort(err) || out.b.Len() == 0 {
			return nil, err
		}
		r.res.Notes = append(r.res.Notes, restNote(err))
		out.finish(r.res)
		return r.res, nil
	}
	for _, kind := range []string{"footnotes", "endnotes"} {
		for _, rl := range rs {
			if rl.is(kind) && rl.target != "" {
				if err := r.notes(rl.target, kind, out); err != nil {
					r.res.Notes = append(r.res.Notes, "its "+kind+" are not given: they could not be read")
					if cutShort(err) {
						out.finish(r.res)
						return r.res, nil
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, kind := range []string{"header", "footer"} {
		for _, rl := range rs {
			if !rl.is(kind) || rl.target == "" || seen[rl.target] {
				continue
			}
			seen[rl.target] = true
			var ls lines
			if err := r.part(rl.target, "hdr ftr", &ls); err != nil {
				if cutShort(err) {
					out.finish(r.res)
					return r.res, nil
				}
				continue
			}
			text := oneLine(strings.Join(ls.l, " "))
			if text == "" || seen[kind+"\x00"+text] {
				continue
			}
			seen[kind+"\x00"+text] = true
			out.para()
			out.line(strings.ToUpper(kind[:1]) + kind[1:] + ": " + text)
		}
	}
	out.finish(r.res)
	return r.res, nil
}

// part reads the part's block content, found in its element named one of
// roots (w:body, w:hdr, w:ftr), into s.
func (r *docxReader) part(name, roots string, s sink) error {
	rc, err := r.p.open(name)
	if rc == nil || err != nil {
		return err
	}
	defer closeQuietly(rc)
	x := newXReader(rc, r.p.b)
	for {
		tok, err := x.token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if se, ok := tok.(xml.StartElement); ok && strings.Contains(" "+roots+" ", " "+se.Name.Local+" ") {
			return r.blocks(x, s)
		}
	}
}

// blocks reads the block content of the element whose start was the last
// token: paragraphs, tables, and the content controls and custom XML that
// hold more of them.
func (r *docxReader) blocks(x *xreader, s sink) error {
	var block func(se xml.StartElement) error
	block = func(se xml.StartElement) error {
		switch se.Name.Local {
		case "p":
			return r.paragraph(x, s)
		case "tbl":
			rows, err := r.table(x)
			if err != nil {
				return err
			}
			if !emptyTable(rows) {
				s.para()
				for _, l := range markdownTable(rows) {
					s.line(l)
				}
				s.para()
			}
			return nil
		case "sdt":
			return x.children(func(se xml.StartElement) error {
				if se.Name.Local == "sdtContent" {
					return x.children(block)
				}
				return x.skip()
			})
		case "customXml", "ins", "moveTo":
			return x.children(block)
		case "AlternateContent":
			return x.firstChoice(block)
		}
		return x.skip()
	}
	return x.children(block)
}

// wpara is a paragraph as read.
type wpara struct {
	sb           strings.Builder
	style        string
	outline      int
	numID, ilvl  string
	after        lines
	sawNumbering bool
}

// paragraph reads a paragraph (w:p) and writes it to s: as a heading, a
// list item, or a line.
func (r *docxReader) paragraph(x *xreader, s sink) error {
	p := &wpara{outline: -1}
	var inline func(se xml.StartElement) error
	inline = func(se xml.StartElement) error {
		switch se.Name.Local {
		case "pPr":
			return r.pPr(x, p)
		case "r":
			return x.children(func(se xml.StartElement) error { return r.run(x, se, p) })
		case "hyperlink", "ins", "moveTo", "smartTag", "customXml", "fldSimple", "bdo", "dir":
			return x.children(inline)
		case "sdt":
			return x.children(func(se xml.StartElement) error {
				if se.Name.Local == "sdtContent" {
					return x.children(inline)
				}
				return x.skip()
			})
		case "AlternateContent":
			return x.firstChoice(inline)
		case "oMath", "oMathPara":
			return x.collect(&p.sb)
		}
		// del, moveFrom: text taken out; the rest holds no text.
		return x.skip()
	}
	if err := x.children(inline); err != nil {
		return err
	}
	r.write(p, s)
	return nil
}

// pPr reads a paragraph's properties: its style, list and outline level.
func (r *docxReader) pPr(x *xreader, p *wpara) error {
	return x.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "pStyle":
			p.style = attr(se, "val")
		case "outlineLvl":
			if n, err := strconv.Atoi(attr(se, "val")); err == nil {
				p.outline = n
			}
		case "numPr":
			p.sawNumbering = true
			return x.children(func(se xml.StartElement) error {
				switch se.Name.Local {
				case "numId":
					p.numID = attr(se, "val")
				case "ilvl":
					p.ilvl = attr(se, "val")
				}
				return x.skip()
			})
		}
		return x.skip()
	})
}

// run reads one child of a run (w:r).
func (r *docxReader) run(x *xreader, se xml.StartElement, p *wpara) error {
	switch se.Name.Local {
	case "t":
		t, err := x.textOfAll()
		p.sb.WriteString(t)
		return err
	case "tab", "ptab":
		p.sb.WriteByte('\t')
	case "br", "cr":
		p.sb.WriteByte('\n')
	case "noBreakHyphen":
		p.sb.WriteByte('-')
	case "footnoteReference":
		p.sb.WriteString("[^" + attr(se, "id") + "]")
	case "endnoteReference":
		p.sb.WriteString("[^e" + attr(se, "id") + "]")
	case "drawing", "pict", "object":
		return r.drawing(x, p)
	case "AlternateContent":
		return x.firstChoice(func(se xml.StartElement) error { return r.run(x, se, p) })
	}
	return x.skip()
}

// drawing reads a drawing, a VML picture or an embedded object in a run:
// its pictures as [image], its charts as [chart], its text boxes' content
// after the paragraph.
func (r *docxReader) drawing(x *xreader, p *wpara) error {
	image := false
	for depth := 1; depth > 0; {
		tok, err := x.token()
		if err != nil {
			return eofMalformed(err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "txbxContent":
				if err := r.blocks(x, &p.after); err != nil {
					return err
				}
				continue
			case "chart":
				if id := relAttr(t, "id"); id != "" {
					r.res.Charts++
					p.sb.WriteString(" [chart] ")
				}
			case "pic", "imagedata":
				image = true
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	if image {
		r.res.Images++
		p.sb.WriteString(" [image] ")
	}
	return nil
}

// headingRE is the name Word gives its heading styles, in any language's
// document: "heading 1" to "heading 9".
var headingRE = regexp.MustCompile(`(?i)^heading\s*([1-9])$`)

// heading is the paragraph's heading level, 1 to 9, from its own outline
// level or its style's (followed through what it is based on); 0 when it
// is no heading.
func (r *docxReader) heading(p *wpara) int {
	if p.outline >= 0 && p.outline < 9 {
		return p.outline + 1
	}
	id := p.style
	for range 10 {
		st := r.styles[id]
		if st == nil {
			break
		}
		if st.outline >= 0 && st.outline < 9 {
			return st.outline + 1
		}
		if m := headingRE.FindStringSubmatch(st.name); m != nil {
			return int(m[1][0] - '0')
		}
		if strings.EqualFold(st.name, "title") {
			return 1
		}
		id = st.basedOn
	}
	if m := headingRE.FindStringSubmatch(p.style); m != nil {
		return int(m[1][0] - '0')
	}
	return 0
}

// list is the paragraph's list and level, from itself or its style: ok is
// false when it is no list item (numId 0 takes it out of one).
func (r *docxReader) list(p *wpara) (numID string, lvl int, ok bool) {
	numID, ilvl := p.numID, p.ilvl
	if !p.sawNumbering {
		id := p.style
		for range 10 {
			st := r.styles[id]
			if st == nil {
				break
			}
			if st.numID != "" {
				numID, ilvl = st.numID, st.ilvl
				break
			}
			id = st.basedOn
		}
	}
	if numID == "" || numID == "0" {
		return "", 0, false
	}
	lvl, _ = strconv.Atoi(ilvl)
	return numID, min(max(lvl, 0), 8), true
}

// write writes a paragraph read.
func (r *docxReader) write(p *wpara, s sink) {
	text := strings.TrimSpace(strings.ReplaceAll(p.sb.String(), "\r", ""))
	if text != "" {
		switch h, numID, lvl, isList := r.heading(p), "", 0, false; {
		case h > 0:
			s.para()
			s.line(strings.Repeat("#", min(h, 6)) + " " + oneLine(text))
			s.para()
		default:
			if numID, lvl, isList = r.list(p); isList {
				indent := strings.Repeat("  ", lvl)
				s.line(indent + r.marker(numID, lvl) + strings.ReplaceAll(text, "\n", "\n"+indent+"  "))
			} else {
				s.line(text)
			}
		}
	}
	for _, l := range p.after.l {
		s.line(l)
	}
}

// marker is a list item's marker: - for a bulleted level, else its number
// among the items of its list at its level, counted as Word counts them.
func (r *docxReader) marker(numID string, lvl int) string {
	if bullet, known := r.bullets[numID][strconv.Itoa(lvl)]; !known || bullet {
		return "- "
	}
	c := r.counters[numID]
	if c == nil {
		c = &[9]int{}
		r.counters[numID] = c
	}
	c[lvl]++
	for i := lvl + 1; i < len(c); i++ {
		c[i] = 0
	}
	return strconv.Itoa(c[lvl]) + ". "
}

// table reads a table (w:tbl): its rows of cells, a cell spanning several
// columns followed by empty ones, one merged into the cell above left
// empty, and a table in a cell read as its text.
func (r *docxReader) table(x *xreader) ([][]string, error) {
	var rows [][]string
	var row func(se xml.StartElement) error
	var cells []string
	var cell func(se xml.StartElement) error
	cell = func(se xml.StartElement) error {
		switch se.Name.Local {
		case "tc":
			span, merged := 1, false
			var ls lines
			err := x.children(func(se xml.StartElement) error {
				if se.Name.Local != "tcPr" {
					var block func(se xml.StartElement) error
					block = func(se xml.StartElement) error {
						switch se.Name.Local {
						case "p":
							return r.paragraph(x, &ls)
						case "tbl":
							inner, err := r.table(x)
							for _, rw := range inner {
								if !emptyRow(rw) {
									ls.line(strings.Join(rw, " "))
								}
							}
							return err
						case "sdt":
							return x.children(func(se xml.StartElement) error {
								if se.Name.Local == "sdtContent" {
									return x.children(block)
								}
								return x.skip()
							})
						case "customXml":
							return x.children(block)
						}
						return x.skip()
					}
					return block(se)
				}
				return x.children(func(se xml.StartElement) error {
					switch se.Name.Local {
					case "gridSpan":
						if n, err := strconv.Atoi(attr(se, "val")); err == nil && n > 1 {
							span = min(n, 64)
						}
					case "vMerge":
						merged = attr(se, "val") != "restart"
					}
					return x.skip()
				})
			})
			text := strings.Join(ls.l, "\n")
			if merged {
				text = ""
			}
			cells = append(cells, text)
			for i := 1; i < span; i++ {
				cells = append(cells, "")
			}
			return err
		case "sdt":
			return x.children(func(se xml.StartElement) error {
				if se.Name.Local == "sdtContent" {
					return x.children(cell)
				}
				return x.skip()
			})
		case "customXml":
			return x.children(cell)
		}
		return x.skip()
	}
	row = func(se xml.StartElement) error {
		switch se.Name.Local {
		case "tr":
			cells = nil
			err := x.children(cell)
			rows = append(rows, cells)
			return err
		case "sdt":
			return x.children(func(se xml.StartElement) error {
				if se.Name.Local == "sdtContent" {
					return x.children(row)
				}
				return x.skip()
			})
		case "customXml":
			return x.children(row)
		}
		return x.skip()
	}
	err := x.children(row)
	return rows, err
}

// notes writes the footnotes or endnotes of the part: each that is a note,
// not a separator, as [^n]: its text.
func (r *docxReader) notes(part, kind string, out sink) error {
	rc, err := r.p.open(part)
	if rc == nil || err != nil {
		return err
	}
	defer closeQuietly(rc)
	x := newXReader(rc, r.p.b)
	type note struct {
		id   int
		text string
	}
	var got []note
	for {
		tok, err := x.token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "footnote" && se.Name.Local != "endnote" {
			continue
		}
		id, err := strconv.Atoi(attr(se, "id"))
		if typ := attr(se, "type"); err != nil || typ != "" && typ != "normal" {
			if err := x.skip(); err != nil {
				return err
			}
			continue
		}
		var ls lines
		if err := r.blocks(x, &ls); err != nil {
			return err
		}
		if t := strings.TrimSpace(strings.Join(ls.l, " ")); t != "" {
			got = append(got, note{id, t})
		}
	}
	sort.SliceStable(got, func(i, j int) bool { return got[i].id < got[j].id })
	prefix := "^"
	if kind == "endnotes" {
		prefix = "^e"
	}
	if len(got) > 0 {
		out.para()
	}
	for _, n := range got {
		out.line(fmt.Sprintf("[%s%d]: %s", prefix, n.id, oneLine(n.text)))
	}
	return nil
}

// readStyles reads the paragraph styles: each one's name, what it is based
// on, and the outline level and list it gives.
func readStyles(p *opc, part string) (map[string]*wstyle, error) {
	rc, err := p.open(part)
	if rc == nil || err != nil {
		return nil, err
	}
	defer closeQuietly(rc)
	x := newXReader(rc, p.b)
	styles := map[string]*wstyle{}
	for {
		tok, err := x.token()
		if errors.Is(err, io.EOF) {
			return styles, nil
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "style" {
			continue
		}
		st := &wstyle{outline: -1}
		id := attr(se, "styleId")
		err = x.children(func(se xml.StartElement) error {
			switch se.Name.Local {
			case "name":
				st.name = attr(se, "val")
			case "basedOn":
				st.basedOn = attr(se, "val")
			case "pPr":
				p := &wpara{outline: -1}
				if err := (&docxReader{}).pPr(x, p); err != nil {
					return err
				}
				st.outline, st.numID, st.ilvl = p.outline, p.numID, p.ilvl
				return nil
			}
			return x.skip()
		})
		if err != nil {
			return nil, err
		}
		if _, dup := styles[id]; !dup {
			styles[id] = st
		}
	}
}

// readNumbering reads which levels of which lists are bulleted: numbering
// .xml's lists (w:num) name their abstract definitions, whose levels
// (w:lvl) say their format (w:numFmt).
func readNumbering(p *opc, part string) (map[string]map[string]bool, error) {
	rc, err := p.open(part)
	if rc == nil || err != nil {
		return nil, err
	}
	defer closeQuietly(rc)
	x := newXReader(rc, p.b)
	abstract := map[string]map[string]bool{}
	nums := map[string]string{}
	for {
		tok, err := x.token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "abstractNum":
			id := attr(se, "abstractNumId")
			levels := map[string]bool{}
			err := x.children(func(se xml.StartElement) error {
				if se.Name.Local != "lvl" {
					return x.skip()
				}
				ilvl := attr(se, "ilvl")
				return x.children(func(se xml.StartElement) error {
					if se.Name.Local == "numFmt" {
						levels[ilvl] = attr(se, "val") == "bullet" || attr(se, "val") == "none"
					}
					return x.skip()
				})
			})
			if err != nil {
				return nil, err
			}
			abstract[id] = levels
		case "num":
			id := attr(se, "numId")
			err := x.children(func(se xml.StartElement) error {
				if se.Name.Local == "abstractNumId" {
					nums[id] = attr(se, "val")
				}
				return x.skip()
			})
			if err != nil {
				return nil, err
			}
		}
	}
	out := make(map[string]map[string]bool, len(nums))
	for num, abs := range nums {
		if levels := abstract[abs]; levels != nil {
			out[num] = levels
		}
	}
	return out, nil
}
