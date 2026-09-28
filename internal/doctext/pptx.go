package doctext

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A presentation's text: its slides in the order presentation.xml lists
// them (sldIdLst, each through the presentation's relationships to its
// part), each under "## Slide N" with its title, then the text of its
// shapes in the order they are drawn: a paragraph a line, bulleted where
// the slide's body bullets it, indented by its level; tables as Markdown;
// pictures as [image], charts as [chart] (with their title), SmartArt as
// its points; the slide's speaker notes after it. The slide number, date
// and footer placeholders are left out.

// slide is one slide's text as read.
type slide struct {
	title  string
	lines  []string
	notes  []string
	hidden bool
}

// pptxReader reads the parts of one presentation.
type pptxReader struct {
	p   *opc
	res *Result
}

func readPPTX(data []byte, b *budget) (*Result, error) {
	p, err := openOPC(data, b)
	if err != nil {
		return nil, err
	}
	main := p.mainPart("ppt/presentation.xml")
	if !p.has(main) {
		return nil, malformedf("it holds no presentation")
	}
	prels, err := p.rels(main)
	if err != nil {
		return nil, err
	}
	ids, err := slideIDs(p, main)
	if err != nil {
		return nil, err
	}
	byID := relMap(prels)
	var parts []string
	for _, id := range ids {
		if r, ok := byID[id]; ok && r.is("slide") && r.target != "" && p.has(r.target) {
			parts = append(parts, r.target)
		}
	}
	r := &pptxReader{p: p, res: &Result{Format: PPTX, Of: len(parts)}}
	out := newTextOut(b.lim.MaxText)
	for i, part := range parts {
		if i == b.lim.MaxParts {
			r.res.Notes = append(r.res.Notes, fmt.Sprintf("only its first %d slides of %d are given", i, len(parts)))
			break
		}
		if out.cut {
			break
		}
		head := "## Slide " + strconv.Itoa(i+1)
		s, err := r.slide(part)
		switch {
		case err != nil && cutShort(err) && r.res.Parts > 0:
			r.res.Notes = append(r.res.Notes, restNote(err))
			r.res.Text = out.done()
			return r.res, nil
		case errors.Is(err, ErrMalformed):
			// One slide damaged: the others are read.
			r.res.Parts++
			out.para()
			out.line(head)
			out.line("[this slide could not be read]")
			continue
		case err != nil:
			return nil, err
		}
		r.res.Parts++
		if s.hidden {
			head += " (hidden)"
		}
		if s.title != "" {
			head += ": " + s.title
		}
		out.para()
		out.line(head)
		for _, l := range s.lines {
			out.line(l)
		}
		if len(s.notes) > 0 {
			out.line("Notes: " + strings.Join(s.notes, "\n"))
		}
	}
	r.res.Text = out.done()
	return r.res, nil
}

// slideIDs are the relationship ids of the slides, in the order the
// presentation lists them.
func slideIDs(p *opc, main string) ([]string, error) {
	rc, err := p.open(main)
	if rc == nil || err != nil {
		return nil, err
	}
	defer closeQuietly(rc)
	x := newXReader(rc, p.b)
	var ids []string
	for {
		tok, err := x.token()
		if errors.Is(err, io.EOF) {
			return ids, nil
		}
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sldId" {
			if id := relAttr(se, "id"); id != "" {
				ids = append(ids, id)
			}
		}
	}
}

// shapeReader reads one slide's, or notes slide's, shape tree.
type shapeReader struct {
	r     *pptxReader
	x     *xreader
	part  string
	rels  map[string]rel
	notes bool
	s     *slide
}

func (r *pptxReader) slide(part string) (*slide, error) {
	s := &slide{}
	rs, err := r.p.rels(part)
	if err != nil {
		return nil, err
	}
	if err := r.shapes(part, rs, false, s); err != nil {
		return nil, err
	}
	for _, rl := range rs {
		if rl.is("notesSlide") && rl.target != "" {
			nrs, err := r.p.rels(rl.target)
			if err != nil {
				return nil, err
			}
			notes := &slide{}
			if err := r.shapes(rl.target, nrs, true, notes); err != nil {
				return nil, err
			}
			s.notes = notes.lines
			break
		}
	}
	return s, nil
}

// shapes reads the shape tree of the part into s.
func (r *pptxReader) shapes(part string, rs []rel, notes bool, s *slide) error {
	rc, err := r.p.open(part)
	if rc == nil || err != nil {
		return err
	}
	defer closeQuietly(rc)
	sr := &shapeReader{r: r, x: newXReader(rc, r.p.b), part: part, rels: relMap(rs), notes: notes, s: s}
	for {
		tok, err := sr.x.token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "sld":
			s.hidden = attr(se, "show") == "0"
		case "spTree":
			if err := sr.x.children(sr.shape); err != nil {
				return err
			}
			return nil
		}
	}
}

// shape reads one child of a shape tree or a group.
func (sr *shapeReader) shape(se xml.StartElement) error {
	x := sr.x
	switch se.Name.Local {
	case "sp":
		return sr.textShape()
	case "grpSp":
		return x.children(sr.shape)
	case "graphicFrame":
		return sr.graphicFrame()
	case "pic":
		if !sr.notes {
			sr.image()
		}
		return x.skip()
	case "AlternateContent":
		return x.firstChoice(sr.shape)
	}
	return x.skip()
}

func (sr *shapeReader) image() {
	sr.r.res.Images++
	sr.s.lines = append(sr.s.lines, "[image]")
}

// para is a paragraph of a text body: its text, its level, and whether it
// says it is bulleted (1), says it is not (-1), or leaves it to the slide
// (0).
type para struct {
	text   string
	lvl    int
	bullet int
}

// textShape reads a shape (p:sp): its placeholder's kind, then its text.
func (sr *shapeReader) textShape() error {
	x := sr.x
	var ph string
	isPh := false
	var paras []para
	err := x.children(func(se xml.StartElement) error {
		switch se.Name.Local {
		case "nvSpPr":
			return x.children(func(se xml.StartElement) error {
				if se.Name.Local != "nvPr" {
					return x.skip()
				}
				return x.children(func(se xml.StartElement) error {
					if se.Name.Local == "ph" {
						isPh, ph = true, attr(se, "type")
					}
					return x.skip()
				})
			})
		case "txBody":
			var err error
			paras, err = sr.txBody()
			return err
		}
		return x.skip()
	})
	if err != nil {
		return err
	}
	if isPh && ph == "" {
		ph = "obj"
	}
	switch ph {
	case "sldNum", "dt", "ftr", "hdr", "sldImg":
		return nil
	case "title", "ctrTitle":
		if !sr.notes {
			var parts []string
			for _, p := range paras {
				if t := oneLine(p.text); t != "" {
					parts = append(parts, t)
				}
			}
			if t := strings.Join(parts, " "); sr.s.title == "" {
				sr.s.title = t
				return nil
			}
		}
	}
	if sr.notes && ph != "body" && ph != "obj" {
		return nil
	}
	body := !sr.notes && (ph == "body" || ph == "obj")
	for _, p := range paras {
		text := strings.TrimSpace(strings.ReplaceAll(p.text, "\r", ""))
		if text == "" {
			continue
		}
		if p.bullet > 0 || body && p.bullet == 0 {
			text = strings.Repeat("  ", p.lvl) + "- " + strings.ReplaceAll(text, "\n", "\n"+strings.Repeat("  ", p.lvl+1))
		}
		sr.s.lines = append(sr.s.lines, text)
	}
	return nil
}

// txBody reads a text body's paragraphs.
func (sr *shapeReader) txBody() ([]para, error) {
	x := sr.x
	var out []para
	err := x.children(func(se xml.StartElement) error {
		if se.Name.Local != "p" {
			return x.skip()
		}
		p, err := sr.paragraph()
		out = append(out, p)
		return err
	})
	return out, err
}

// paragraph reads a DrawingML paragraph (a:p).
func (sr *shapeReader) paragraph() (para, error) {
	x := sr.x
	var p para
	var sb strings.Builder
	var run func(se xml.StartElement) error
	run = func(se xml.StartElement) error {
		switch se.Name.Local {
		case "pPr":
			if lvl, err := strconv.Atoi(attr(se, "lvl")); err == nil && lvl >= 0 && lvl < 9 {
				p.lvl = lvl
			}
			return x.children(func(se xml.StartElement) error {
				switch se.Name.Local {
				case "buNone":
					p.bullet = -1
				case "buChar", "buAutoNum", "buBlip":
					p.bullet = 1
				}
				return x.skip()
			})
		case "br":
			sb.WriteByte('\n')
			return x.skip()
		case "AlternateContent":
			return x.firstChoice(run)
		case "endParaRPr":
			return x.skip()
		}
		// Runs, fields, and formulas (a14:m): their text.
		return x.collect(&sb)
	}
	err := x.children(run)
	p.text = sb.String()
	return p, err
}

// graphicFrame reads a frame: a table, a chart, SmartArt, or an embedded
// object.
func (sr *shapeReader) graphicFrame() error {
	x := sr.x
	return x.children(func(se xml.StartElement) error {
		if se.Name.Local != "graphic" {
			return x.skip()
		}
		return x.children(func(se xml.StartElement) error {
			if se.Name.Local != "graphicData" {
				return x.skip()
			}
			uri := attr(se, "uri")
			return x.children(func(se xml.StartElement) error {
				switch {
				case se.Name.Local == "tbl":
					rows, err := sr.table()
					if err != nil {
						return err
					}
					if !emptyTable(rows) {
						sr.s.lines = append(sr.s.lines, markdownTable(rows)...)
					}
					return nil
				case se.Name.Local == "chart" && strings.HasSuffix(uri, "/chart"):
					sr.chart(relAttr(se, "id"))
					return x.skip()
				case se.Name.Local == "relIds" && strings.HasSuffix(uri, "/diagram"):
					err := sr.diagram(relAttr(se, "dm"))
					if err != nil {
						return err
					}
					return x.skip()
				case se.Name.Local == "AlternateContent":
					return x.firstChoice(func(se xml.StartElement) error {
						if se.Name.Local == "pic" {
							sr.image()
						}
						return x.skip()
					})
				}
				return x.skip()
			})
		})
	})
}

func emptyTable(rows [][]string) bool {
	for _, r := range rows {
		if !emptyRow(r) {
			return false
		}
	}
	return true
}

// table reads a DrawingML table (a:tbl): its rows of cells, a cell merged
// into another left empty.
func (sr *shapeReader) table() ([][]string, error) {
	x := sr.x
	var rows [][]string
	err := x.children(func(se xml.StartElement) error {
		if se.Name.Local != "tr" {
			return x.skip()
		}
		var row []string
		err := x.children(func(se xml.StartElement) error {
			if se.Name.Local != "tc" {
				return x.skip()
			}
			merged := attr(se, "hMerge") == "1" || attr(se, "vMerge") == "1"
			var lines []string
			err := x.children(func(se xml.StartElement) error {
				if se.Name.Local != "txBody" {
					return x.skip()
				}
				paras, err := sr.txBody()
				for _, p := range paras {
					if t := strings.TrimSpace(p.text); t != "" {
						lines = append(lines, t)
					}
				}
				return err
			})
			if merged {
				lines = nil
			}
			row = append(row, strings.Join(lines, "\n"))
			return err
		})
		rows = append(rows, row)
		return err
	})
	return rows, err
}

// chart names a chart, by its title when it has one.
func (sr *shapeReader) chart(id string) {
	sr.r.res.Charts++
	title := ""
	if rl, ok := sr.rels[id]; ok && rl.is("chart") && rl.target != "" {
		title, _ = chartTitle(sr.r.p, rl.target)
	}
	if title != "" {
		sr.s.lines = append(sr.s.lines, "[chart: "+title+"]")
		return
	}
	sr.s.lines = append(sr.s.lines, "[chart]")
}

// chartTitle is the title of the chart part (c:chart/c:title), "" when it
// has none; the chart's data is not read.
func chartTitle(p *opc, part string) (string, error) {
	rc, err := p.open(part)
	if rc == nil || err != nil {
		return "", err
	}
	defer closeQuietly(rc)
	x := newXReader(rc, p.b)
	var stack []string
	for {
		tok, err := x.token()
		if err != nil {
			return "", nil
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "plotArea" {
				return "", nil
			}
			if t.Name.Local == "title" && len(stack) > 0 && stack[len(stack)-1] == "chart" {
				var sb strings.Builder
				// A title given by reference caches its text in c:v.
				err := x.children(func(se xml.StartElement) error { return collectTitle(x, se, &sb) })
				return oneLine(sb.String()), err
			}
			stack = append(stack, t.Name.Local)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
}

func collectTitle(x *xreader, se xml.StartElement, sb *strings.Builder) error {
	switch se.Name.Local {
	case "v", "t":
		t, err := x.textOfAll()
		sb.WriteString(t)
		return err
	case "txPr", "spPr", "layout", "overlay":
		return x.skip()
	}
	return x.children(func(se xml.StartElement) error { return collectTitle(x, se, sb) })
}

// textOfAll is all the character data of the element whose start was the
// last token.
func (x *xreader) textOfAll() (string, error) {
	var sb strings.Builder
	for depth := 1; depth > 0; {
		tok, err := x.token()
		if err != nil {
			return sb.String(), eofMalformed(err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			sb.Write(t)
		}
	}
	return sb.String(), nil
}

// diagram gives the points of a SmartArt diagram's data part, a line each.
func (sr *shapeReader) diagram(id string) error {
	rl, ok := sr.rels[id]
	if !ok || !rl.is("diagramData") || rl.target == "" {
		return nil
	}
	rc, err := sr.r.p.open(rl.target)
	if rc == nil || err != nil {
		return err
	}
	defer closeQuietly(rc)
	x := newXReader(rc, sr.r.p.b)
	for {
		tok, err := x.token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "pt" {
			continue
		}
		// Points of type node (the default) hold what the diagram says;
		// the rest are its connections and presentation.
		if typ := attr(se, "type"); typ != "" && typ != "node" {
			if err := x.skip(); err != nil {
				return err
			}
			continue
		}
		t, err := x.textOf()
		if err != nil {
			return err
		}
		if t = oneLine(t); t != "" {
			sr.s.lines = append(sr.s.lines, "- "+t)
		}
	}
}
