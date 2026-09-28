package doctext

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf16"
)

// A page's text (ISO 32000-1 §9.4, §14.6): its content stream run for its
// text operators alone, with the graphics state they need (the current
// transformation matrix, and the text state); forms drawn with Do run as
// part of the page, nesting at most maxForms deep and never into a form
// they are already in; images only counted. Each string shown is a run with
// where it begins and ends on the page, and runs are joined in the order
// drawn: a new line where one begins below (or above) the last by more than
// half its size, a space where there is a gap of more than a fifth of it
// (more, between two ideographs, which are set apart to justify a line).
// A marked-content sequence's ActualText replaces the glyphs it covers, as
// ligatures and symbols are written in tagged files. Text drawn invisibly
// is text all the same: it is how a scanned page's recognized text is kept.

const (
	maxForms    = 12
	maxOperands = 1024
	maxGStack   = 256
)

type matrix [6]float64

var identity = matrix{1, 0, 0, 1, 0, 0}

// mul is m then n: the matrix of m followed by n.
func (m matrix) mul(n matrix) matrix {
	return matrix{
		m[0]*n[0] + m[1]*n[2], m[0]*n[1] + m[1]*n[3],
		m[2]*n[0] + m[3]*n[2], m[2]*n[1] + m[3]*n[3],
		m[4]*n[0] + m[5]*n[2] + n[4], m[4]*n[1] + m[5]*n[3] + n[5],
	}
}

func (m matrix) apply(x, y float64) (float64, float64) {
	return x*m[0] + y*m[2] + m[4], x*m[1] + y*m[3] + m[5]
}

type gstate struct {
	ctm                          matrix
	font                         *pdfFont
	size, cs, ws, scale, leading float64
	rise                         float64
}

// run is one string shown: its text, where it begins and ends on the page,
// and its size there.
type run struct {
	text           string
	x0, y0, x1, y1 float64
	size           float64
	vertical       bool
}

// pageText joins a page's runs.
type pageText struct {
	sb     strings.Builder
	last   *run
	glyphs int
	// unmapped are the glyphs no map gave text.
	unmapped int
	images   int
	max      int
	full     bool
}

func (pt *pageText) add(r run) {
	if r.text == "" || pt.full {
		return
	}
	if p := pt.last; p != nil {
		sep := separator(p, &r)
		if r.text == "" {
			return
		}
		pt.sb.WriteString(sep)
	}
	pt.sb.WriteString(r.text)
	if pt.sb.Len() > pt.max {
		pt.full = true
	}
	pt.last = &r
}

// separator is what goes between the run before, p, and r.
func separator(p, r *run) string {
	h := math.Max(math.Max(p.size, r.size), 1)
	along, across := r.x0-p.x1, r.y0-p.y1
	if p.vertical && r.vertical {
		along, across = p.y1-r.y0, r.x0-p.x1
	}
	switch {
	case math.Abs(across) > 0.5*h:
		return "\n"
	case r.text == p.text && math.Abs(r.x0-p.x0) < 0.1*h && math.Abs(r.y0-p.y0) < 0.1*h:
		// The same text again, on itself: drawn twice to look bold.
		r.text = ""
		return ""
	case strings.HasSuffix(p.text, " ") || strings.HasPrefix(r.text, " "):
		return ""
	case along > 0.2*h:
		if ideograph(lastRune(p.text)) && ideograph(firstRune(r.text)) && along < 0.8*h {
			return ""
		}
		return " "
	case along < -2*h:
		// Back along the line: another column, or text set apart.
		return " "
	}
	return ""
}

func lastRune(s string) rune {
	r := []rune(s)
	if len(r) == 0 {
		return 0
	}
	return r[len(r)-1]
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

// ideograph reports whether r is written without spaces between its
// neighbours: Han, kana, Hangul, and their punctuation.
func ideograph(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Bopomofo) ||
		r >= 0x3000 && r <= 0x303F || r >= 0xFF00 && r <= 0xFFEF
}

// interp runs content streams.
type interp struct {
	d     *pdfDoc
	pt    *pageText
	gs    gstate
	stack []gstate
	tm    matrix
	tlm   matrix
	// forms are the forms being drawn, by object number.
	forms map[int]bool
	depth int
	// actual is the marked-content sequences open, and whether each gives
	// ActualText (then the text, and whether it was given).
	actual []actualText
}

type actualText struct {
	has, given bool
	text       string
}

// runPage runs a page's content, and returns its text.
func (d *pdfDoc) runPage(p pdfPage, maxText int) *pageText {
	it := &interp{d: d, pt: &pageText{max: maxText}, forms: map[int]bool{}}
	it.gs = gstate{ctm: identity, scale: 1}
	var content []byte
	add := func(v any) {
		s, ok := d.resolve(v).(*pdfStream)
		if !ok {
			return
		}
		data, err := d.decode(s)
		if err != nil {
			d.failLimit(err)
			return
		}
		content = append(content, data...)
		content = append(content, '\n')
	}
	switch c := d.resolve(p.dict["Contents"]).(type) {
	case *pdfStream:
		add(c)
	case pdfArray:
		for _, e := range c {
			add(e)
		}
	}
	it.run(content, p.res)
	return it.pt
}

// run runs a content stream with its resources.
func (it *interp) run(content []byte, res pdfDict) {
	d := it.d
	p := &parser{lx: lexer{b: content, bud: d.bud}, maxDepth: 32}
	var ops []any
	for d.err == nil && !it.pt.full {
		v, ok := p.object(0)
		if !ok {
			break
		}
		kw, isOp := v.(pdfKeyword)
		if !isOp {
			if len(ops) < maxOperands {
				ops = append(ops, v)
			}
			continue
		}
		if kw == "BI" {
			it.inlineImage(p)
			ops = ops[:0]
			continue
		}
		it.op(string(kw), ops, res)
		ops = ops[:0]
	}
	if p.lx.err != nil {
		d.fail(p.lx.err)
	}
}

func num(ops []any, i int) float64 {
	if i < 0 || i >= len(ops) {
		return 0
	}
	f, _ := asFloat(ops[i])
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// last is the operand n from the end, with 1 the last.
func last(ops []any, n int) any {
	if len(ops) < n {
		return nil
	}
	return ops[len(ops)-n]
}

func (it *interp) op(op string, ops []any, res pdfDict) {
	gs := &it.gs
	n := len(ops)
	switch op {
	case "q":
		if len(it.stack) < maxGStack {
			it.stack = append(it.stack, *gs)
		}
	case "Q":
		if len(it.stack) > 0 {
			*gs = it.stack[len(it.stack)-1]
			it.stack = it.stack[:len(it.stack)-1]
		}
	case "cm":
		if n >= 6 {
			m := matrix{num(ops, n-6), num(ops, n-5), num(ops, n-4), num(ops, n-3), num(ops, n-2), num(ops, n-1)}
			gs.ctm = m.mul(gs.ctm)
		}
	case "BT":
		it.tm, it.tlm = identity, identity
	case "Tf":
		if n >= 2 {
			gs.size = num(ops, n-1)
			if fonts := it.d.dict(res["Font"]); fonts != nil {
				gs.font = it.d.font(fonts[pdfName(asName(last(ops, 2)))])
			}
		}
	case "Tc":
		gs.cs = num(ops, n-1)
	case "Tw":
		gs.ws = num(ops, n-1)
	case "Tz":
		gs.scale = num(ops, n-1) / 100
	case "TL":
		gs.leading = num(ops, n-1)
	case "Ts":
		gs.rise = num(ops, n-1)
	case "Td":
		it.moveLine(num(ops, n-2), num(ops, n-1))
	case "TD":
		gs.leading = -num(ops, n-1)
		it.moveLine(num(ops, n-2), num(ops, n-1))
	case "Tm":
		if n >= 6 {
			it.tlm = matrix{num(ops, n-6), num(ops, n-5), num(ops, n-4), num(ops, n-3), num(ops, n-2), num(ops, n-1)}
			it.tm = it.tlm
		}
	case "T*":
		it.moveLine(0, -gs.leading)
	case "Tj":
		if s, ok := last(ops, 1).(pdfString); ok {
			it.show([]byte(s))
		}
	case "'":
		it.moveLine(0, -gs.leading)
		if s, ok := last(ops, 1).(pdfString); ok {
			it.show([]byte(s))
		}
	case "\"":
		if n >= 3 {
			gs.ws, gs.cs = num(ops, n-3), num(ops, n-2)
		}
		it.moveLine(0, -gs.leading)
		if s, ok := last(ops, 1).(pdfString); ok {
			it.show([]byte(s))
		}
	case "TJ":
		arr, _ := last(ops, 1).(pdfArray)
		for _, e := range arr {
			switch x := e.(type) {
			case pdfString:
				it.show([]byte(x))
			case int, float64:
				adj, _ := asFloat(x)
				it.advance(-adj / 1000 * gs.size * gs.scale)
			}
		}
	case "Do":
		it.xobject(pdfName(asName(last(ops, 1))), res)
	case "BMC":
		it.actual = append(it.actual, actualText{})
	case "BDC":
		props := it.d.dict(last(ops, 1))
		if name, ok := last(ops, 1).(pdfName); ok {
			props = it.d.dict(it.d.dict(res["Properties"])[name])
		}
		a := actualText{}
		if s, ok := it.d.resolve(props["ActualText"]).(pdfString); ok && !it.inActual() {
			a.has, a.text = true, textString(string(s))
		}
		if len(it.actual) < maxGStack {
			it.actual = append(it.actual, a)
		}
	case "EMC":
		if k := len(it.actual); k > 0 {
			a := it.actual[k-1]
			it.actual = it.actual[:k-1]
			if a.has && !a.given {
				it.emit(a.text)
			}
		}
	}
}

func (it *interp) inActual() bool {
	for _, a := range it.actual {
		if a.has {
			return true
		}
	}
	return false
}

// moveLine starts a new line at an offset from the start of the last.
func (it *interp) moveLine(tx, ty float64) {
	it.tlm = matrix{1, 0, 0, 1, tx, ty}.mul(it.tlm)
	it.tm = it.tlm
}

// advance moves the text matrix along the line.
func (it *interp) advance(d float64) {
	if it.gs.font != nil && it.gs.font.vertical {
		it.tm = matrix{1, 0, 0, 1, 0, -d}.mul(it.tm)
		return
	}
	it.tm = matrix{1, 0, 0, 1, d, 0}.mul(it.tm)
}

// devicePoint is where the text matrix is on the page, and the size of the
// font there.
func (it *interp) devicePoint() (x, y, size float64) {
	m := it.tm.mul(it.gs.ctm)
	x, y = m.apply(0, it.gs.rise)
	size = math.Abs(it.gs.size) * math.Hypot(m[2], m[3])
	return x, y, size
}

// show shows a string in the current font.
func (it *interp) show(raw []byte) {
	f := it.gs.font
	if f == nil {
		f = &pdfFont{fm0: 0.001, dw: 1000, simple: standardEncoding}
	}
	x0, y0, size := it.devicePoint()
	var sb strings.Builder
	for _, g := range f.decode(raw) {
		it.pt.glyphs++
		if g.unmapped {
			it.pt.unmapped++
		}
		sb.WriteString(g.text)
		adv := g.w*it.gs.size + it.gs.cs
		if g.space {
			adv += it.gs.ws
		}
		if f.vertical {
			// A vertical font's glyphs advance down by a whole em unless its
			// metrics say otherwise; the default is what they say.
			adv = it.gs.size + it.gs.cs
		} else {
			adv *= it.gs.scale
		}
		it.advance(adv)
	}
	x1, y1, _ := it.devicePoint()
	if it.inActual() {
		for i := len(it.actual) - 1; i >= 0; i-- {
			if a := &it.actual[i]; a.has {
				if !a.given {
					a.given = true
					it.pt.add(run{text: a.text, x0: x0, y0: y0, x1: x1, y1: y1, size: size, vertical: f.vertical})
				}
				break
			}
		}
		return
	}
	it.pt.add(run{text: sb.String(), x0: x0, y0: y0, x1: x1, y1: y1, size: size, vertical: f.vertical})
}

// emit adds text where the text matrix is.
func (it *interp) emit(text string) {
	x, y, size := it.devicePoint()
	it.pt.add(run{text: text, x0: x, y0: y, x1: x, y1: y, size: size})
}

// xobject draws the XObject named name: a form's content, or an image
// counted.
func (it *interp) xobject(name pdfName, res pdfDict) {
	d := it.d
	ref := d.dict(res["XObject"])[name]
	s, ok := d.resolve(ref).(*pdfStream)
	if !ok {
		return
	}
	switch asName(s.dict["Subtype"]) {
	case "Image":
		it.pt.images++
		return
	case "Form":
	default:
		return
	}
	r, isRef := ref.(pdfRef)
	if it.depth >= maxForms || isRef && it.forms[r.num] {
		return
	}
	data, cached := d.forms[r.num]
	if !isRef || !cached {
		var err error
		if data, err = d.decode(s); err != nil {
			d.failLimit(err)
			return
		}
		if isRef {
			d.forms[r.num] = data
		}
	}
	if isRef {
		it.forms[r.num] = true
		defer delete(it.forms, r.num)
	}
	fres := d.dict(s.dict["Resources"])
	if fres == nil {
		fres = res
	}
	saved, savedTm, savedTlm := it.gs, it.tm, it.tlm
	if m := d.array(s.dict["Matrix"]); len(m) == 6 {
		fm := matrix{}
		for i := range 6 {
			fm[i], _ = asFloat(d.resolve(m[i]))
		}
		it.gs.ctm = fm.mul(it.gs.ctm)
	}
	it.depth++
	it.run(data, fres)
	it.depth--
	it.gs, it.tm, it.tlm = saved, savedTm, savedTlm
}

// inlineImage passes over an inline image (BI … ID data EI), counting it:
// its data ends at EI between white space.
func (it *interp) inlineImage(p *parser) {
	it.pt.images++
	for {
		t := p.lx.next()
		if t.kind == tkEOF {
			return
		}
		if t.kind == tkKeyword && t.s == "ID" {
			break
		}
	}
	b := p.lx.b
	i := p.lx.pos + 1
	for ; i+2 <= len(b); i++ {
		if b[i] == 'E' && b[i+1] == 'I' && isWhite(b[i-1]) && (i+2 == len(b) || isWhite(b[i+2]) || isDelim(b[i+2])) {
			p.lx.pos = i + 2
			return
		}
	}
	p.lx.pos = len(b)
}

// textString is a PDF text string as text: UTF-16BE after its byte order
// mark, UTF-8 after its (PDF 2.0), and otherwise PDFDocEncoding.
func textString(s string) string {
	switch {
	case strings.HasPrefix(s, "\xfe\xff"):
		return string(utf16.Decode(utf16Units(s[2:])))
	case strings.HasPrefix(s, "\xef\xbb\xbf"):
		return strings.ToValidUTF8(s[3:], "�")
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if t := pdfDocEncoding[s[i]]; t != "" {
			sb.WriteString(t)
		} else if s[i] == '\n' || s[i] == '\r' || s[i] == '\t' {
			sb.WriteByte(s[i])
		}
	}
	return sb.String()
}
