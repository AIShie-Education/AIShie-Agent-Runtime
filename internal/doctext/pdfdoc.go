package doctext

import (
	"bytes"
	"errors"
	"regexp"
	"slices"
)

// A PDF's structure (ISO 32000-1 §7.5): its cross-reference, read from the
// end of the file through every earlier section it names (tables and
// streams, and object streams for the objects kept in them); or, when that
// cannot be read, rebuilt by finding every object in the file, as readers
// do with a damaged file. Objects are read when first asked for and kept;
// a reference to an object being read (a cycle) is null, and so is one to
// an object that is not there. The first limit met, or the end of the
// time, is kept in err and ends the reading.

// xentry is where an object is: at an offset of the file, or in an object
// stream (found there by its number); free when a later section deleted
// it.
type xentry struct {
	off   int
	stm   int
	inStm bool
	free  bool
}

type pdfDoc struct {
	data    []byte
	bud     *budget
	xref    map[int]xentry
	trailer pdfDict
	objs    map[int]any
	loading map[int]bool
	stms    map[int]*objStream
	crypt   *pdfCrypt
	fonts   map[any]*pdfFont
	// forms are forms' content decoded, by object number: a form drawn on
	// every page is decoded once.
	forms map[int][]byte
	err   error
}

// objStream is an object stream, decoded: where each of its objects begins.
type objStream struct {
	data  []byte
	offs  map[int]int
	first int
}

// fail keeps the first error that ends the reading.
func (d *pdfDoc) fail(err error) {
	if d.err == nil && err != nil {
		d.err = err
	}
}

// openPDF reads data's structure, and its encryption: a file that needs a
// password is ErrEncrypted.
func openPDF(data []byte, b *budget) (*pdfDoc, error) {
	if !isPDF(data) {
		return nil, malformedf("it is not a PDF")
	}
	d := &pdfDoc{data: data, bud: b, xref: map[int]xentry{}, trailer: pdfDict{}, objs: map[int]any{},
		loading: map[int]bool{}, stms: map[int]*objStream{}, fonts: map[any]*pdfFont{}, forms: map[int][]byte{}}
	ok := d.readXref()
	if d.err != nil {
		return nil, d.err
	}
	if ok {
		// A cross-reference that cannot even give the encryption
		// dictionary is rebuilt, as a damaged one is.
		if err := d.openCrypt(); errors.Is(err, ErrEncrypted) {
			return nil, err
		} else if err != nil {
			ok = false
		}
	}
	if !ok || d.catalog() == nil {
		if d.err != nil {
			return nil, d.err
		}
		if err := d.rebuild(); err != nil {
			return nil, err
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	if d.catalog() == nil {
		return nil, malformedf("it has no document catalogue")
	}
	return d, nil
}

// openCrypt reads the trailer's encryption dictionary, if it has one, and
// opens the file with it; what was read before the key was known is read
// again, decrypted.
func (d *pdfDoc) openCrypt() error {
	d.crypt = nil
	enc := d.trailer["Encrypt"]
	if enc == nil {
		return nil
	}
	dict, _ := d.resolve(enc).(pdfDict)
	if dict == nil {
		return malformedf("its encryption dictionary cannot be read")
	}
	var id0 []byte
	if ids, ok := d.resolve(d.trailer["ID"]).(pdfArray); ok && len(ids) > 0 {
		s, _ := ids[0].(pdfString)
		id0 = []byte(s)
	}
	c, err := newCrypt(dict, id0)
	if err != nil {
		return err
	}
	if r, ok := enc.(pdfRef); ok {
		c.skip = r.num
	}
	d.crypt = c
	d.objs, d.stms, d.fonts, d.forms = map[int]any{}, map[int]*objStream{}, map[any]*pdfFont{}, map[int][]byte{}
	return nil
}

// catalog is the document catalogue, nil when there is none.
func (d *pdfDoc) catalog() pdfDict {
	c, _ := d.resolve(d.trailer["Root"]).(pdfDict)
	return c
}

// startxrefRE finds the offset of the last cross-reference section.
var startxrefRE = regexp.MustCompile(`startxref\s+(\d{1,12})`)

// readXref reads the cross-reference from the file's last startxref back
// through every section it names; false when it cannot.
func (d *pdfDoc) readXref() bool {
	tail := d.data[max(0, len(d.data)-64<<10):]
	ms := startxrefRE.FindAllSubmatchIndex(tail, -1)
	if len(ms) == 0 {
		return false
	}
	m := ms[len(ms)-1]
	off, ok := asInt(mustNumber(tail[m[2]:m[3]]))
	if !ok {
		return false
	}
	seen := map[int]bool{}
	first := true
	for off >= 0 && off < len(d.data) && !seen[off] && len(seen) < 256 {
		seen[off] = true
		trailer, ok := d.xrefSection(off)
		if !ok {
			return false
		}
		if first {
			d.trailer, first = trailer, false
		} else {
			for k, v := range trailer {
				if _, has := d.trailer[k]; !has {
					d.trailer[k] = v
				}
			}
		}
		// A hybrid file's table names a stream with the same objects
		// compressed.
		if stm, ok := asInt(trailer["XRefStm"]); ok && !seen[stm] {
			seen[stm] = true
			if _, ok := d.xrefSection(stm); !ok {
				return false
			}
		}
		prev, ok := asInt(trailer["Prev"])
		if !ok {
			break
		}
		off = prev
	}
	return d.err == nil
}

func mustNumber(b []byte) any {
	t, ok := number(b)
	if !ok || t.kind != tkInt {
		return nil
	}
	return t.i
}

// xrefSection reads the section at off, a table or a stream, into the
// cross-reference (entries already there, from later sections, stay), and
// returns its trailer.
func (d *pdfDoc) xrefSection(off int) (pdfDict, bool) {
	p := d.parserAt(off, true)
	t := p.lx.next()
	if t.kind == tkKeyword && t.s == "xref" {
		return d.xrefTable(p)
	}
	if t.kind != tkInt {
		return nil, false
	}
	v, _, ok := d.indirectAfter(p, t.i, -1)
	s, isStream := v.(*pdfStream)
	if !ok || !isStream || asName(s.dict["Type"]) != "XRef" {
		return nil, false
	}
	return s.dict, d.xrefStream(s)
}

// xrefTable reads a table's subsections, then its trailer.
func (d *pdfDoc) xrefTable(p *parser) (pdfDict, bool) {
	for {
		t := p.lx.next()
		if t.kind == tkKeyword && t.s == "trailer" {
			v, _ := p.object(0)
			tr, ok := v.(pdfDict)
			return tr, ok
		}
		if t.kind != tkInt {
			return nil, false
		}
		count := p.lx.next()
		if count.kind != tkInt || count.i < 0 || t.i < 0 {
			return nil, false
		}
		for n := range count.i {
			o, g, kind := p.lx.next(), p.lx.next(), p.lx.next()
			if o.kind != tkInt || g.kind != tkInt || kind.kind != tkKeyword {
				return nil, false
			}
			if !d.addEntry(t.i+n, xentry{off: o.i, free: kind.s == "f"}) {
				return nil, false
			}
		}
	}
}

// addEntry puts an entry in the cross-reference unless a later section
// gave the object one; false past MaxObjects.
func (d *pdfDoc) addEntry(num int, e xentry) bool {
	if num < 0 {
		return true
	}
	if _, has := d.xref[num]; has {
		return true
	}
	if len(d.xref) >= d.bud.lim.MaxObjects {
		d.fail(limitf("it holds more than %d objects", d.bud.lim.MaxObjects))
		return false
	}
	d.xref[num] = e
	return true
}

// xrefStream reads a cross-reference stream's entries.
func (d *pdfDoc) xrefStream(s *pdfStream) bool {
	data, err := d.decode(s)
	if err != nil {
		d.failLimit(err)
		return false
	}
	w, _ := s.dict["W"].(pdfArray)
	if len(w) != 3 {
		return false
	}
	var widths [3]int
	sum := 0
	for i := range 3 {
		n, ok := asInt(w[i])
		if !ok || n < 0 || n > 8 {
			return false
		}
		widths[i], sum = n, sum+n
	}
	if sum == 0 {
		return false
	}
	size, _ := asInt(s.dict["Size"])
	index := pdfArray{0, size}
	if ix, ok := s.dict["Index"].(pdfArray); ok {
		index = ix
	}
	pos := 0
	for i := 0; i+1 < len(index); i += 2 {
		start, ok1 := asInt(index[i])
		count, ok2 := asInt(index[i+1])
		if !ok1 || !ok2 || start < 0 || count < 0 {
			return false
		}
		for n := range count {
			if pos+sum > len(data) {
				return true
			}
			f := [3]int{1, 0, 0}
			for k := range 3 {
				if widths[k] == 0 {
					continue
				}
				v := 0
				for _, c := range data[pos : pos+widths[k]] {
					v = v<<8 | int(c)
				}
				f[k] = v
				pos += widths[k]
			}
			var e xentry
			switch f[0] {
			case 0:
				e.free = true
			case 1:
				e.off = f[1]
			case 2:
				e.inStm, e.stm = true, f[1]
			default:
				continue
			}
			if !d.addEntry(start+n, e) {
				return false
			}
		}
	}
	return true
}

// failLimit keeps err when it ends the reading (a limit, or the time); a
// damaged stream only loses what it held.
func (d *pdfDoc) failLimit(err error) {
	if errors.Is(err, ErrLimit) || d.bud.alive() != nil {
		d.fail(err)
	}
}

// objRE finds "n g obj" wherever an object begins.
var objRE = regexp.MustCompile(`(?:^|[^0-9])(\d{1,10})[ \t\r\n\f\x00]+(\d{1,5})[ \t\r\n\f\x00]+obj\b`)

// rebuild makes the cross-reference again from the objects found in the
// file, the last of each number winning as an update's would, and the
// trailer from the trailers found and the cross-reference streams; then,
// the file opened with its encryption if it has one, the objects of the
// object streams found are added, and the catalogue is found by its type
// when no trailer names it.
func (d *pdfDoc) rebuild() error {
	d.xref, d.objs, d.stms, d.crypt = map[int]xentry{}, map[int]any{}, map[int]*objStream{}, nil
	trailer := d.trailer
	d.trailer = pdfDict{}
	for _, m := range objRE.FindAllSubmatchIndex(d.data, -1) {
		num, ok := asInt(mustNumber(d.data[m[2]:m[3]]))
		if !ok {
			continue
		}
		if _, has := d.xref[num]; !has && len(d.xref) >= d.bud.lim.MaxObjects {
			return limitf("it holds more than %d objects", d.bud.lim.MaxObjects)
		}
		d.xref[num] = xentry{off: m[2]}
	}
	for k, v := range trailer {
		d.trailer[k] = v
	}
	for i := 0; ; {
		j := bytes.Index(d.data[i:], []byte("trailer"))
		if j < 0 {
			break
		}
		p := d.parserAt(i+j+len("trailer"), true)
		if v, _ := p.object(0); v != nil {
			if tr, ok := v.(pdfDict); ok {
				for k, v := range tr {
					d.trailer[k] = v
				}
			}
		}
		i += j + len("trailer")
	}
	nums := make([]int, 0, len(d.xref))
	for n := range d.xref {
		nums = append(nums, n)
	}
	slices.Sort(nums)
	var objStms []int
	for _, n := range nums {
		if d.err != nil {
			return d.err
		}
		s, ok := d.load(n).(*pdfStream)
		if !ok {
			continue
		}
		switch asName(s.dict["Type"]) {
		case "XRef":
			for _, k := range []pdfName{"Root", "Encrypt", "ID", "Info"} {
				if v, has := s.dict[k]; has {
					if _, set := d.trailer[k]; !set {
						d.trailer[k] = v
					}
				}
			}
		case "ObjStm":
			objStms = append(objStms, n)
		}
	}
	if err := d.openCrypt(); err != nil {
		return err
	}
	for _, n := range objStms {
		if os := d.objStream(n); os != nil {
			for obj := range os.offs {
				if _, has := d.xref[obj]; !has && len(d.xref) < d.bud.lim.MaxObjects {
					d.xref[obj] = xentry{inStm: true, stm: n}
				}
			}
		}
	}
	if d.catalog() == nil {
		for _, n := range nums {
			if c, ok := d.load(n).(pdfDict); ok && asName(c["Type"]) == "Catalog" {
				d.trailer["Root"] = pdfRef{num: n}
				break
			}
		}
	}
	return d.err
}

// parserAt is a parser of the file from off.
func (d *pdfDoc) parserAt(off int, refs bool) *parser {
	return &parser{lx: lexer{b: d.data, pos: off, bud: d.bud}, maxDepth: min(d.bud.lim.MaxDepth, 128), refs: refs}
}

// resolve follows v while it is a reference: the object it names, or nil.
func (d *pdfDoc) resolve(v any) any {
	for range 16 {
		r, ok := v.(pdfRef)
		if !ok {
			return v
		}
		v = d.load(r.num)
	}
	return nil
}

// maxLoading is how many objects may be being read at once.
const maxLoading = 32

// load is the object numbered num, read once and kept; nil when there is
// none, it cannot be read, or it is being read (a reference that loops).
func (d *pdfDoc) load(num int) any {
	if v, ok := d.objs[num]; ok {
		return v
	}
	e, ok := d.xref[num]
	// An object read while reading another (a stream's length, an object
	// stream) may lead to a third, and so on: never further than
	// maxLoading, and never back to one being read.
	if !ok || e.free || d.loading[num] || len(d.loading) >= maxLoading || d.err != nil {
		return nil
	}
	d.loading[num] = true
	defer delete(d.loading, num)
	var v any
	if e.inStm {
		v = d.fromObjStream(e.stm, num)
	} else {
		p := d.parserAt(e.off, true)
		gen := 0
		if t := p.lx.next(); t.kind == tkInt {
			v, gen, _ = d.indirectAfter(p, t.i, num)
		}
		if p.lx.err != nil {
			d.fail(p.lx.err)
		}
		if d.crypt != nil && num != d.crypt.skip {
			v = d.crypt.strings(v, num, gen)
		}
	}
	if d.err != nil {
		return nil
	}
	d.objs[num] = v
	return v
}

// indirectAfter reads the rest of an indirect object whose number, num,
// was the last token: its generation, obj, the object, and a stream's
// bytes; and returns the object and its generation. want is the number it
// must have (-1 for any).
func (d *pdfDoc) indirectAfter(p *parser, num, want int) (any, int, bool) {
	if want >= 0 && num != want {
		return nil, 0, false
	}
	g := p.lx.next()
	if g.kind != tkInt || p.lx.next().s != "obj" {
		return nil, 0, false
	}
	v, ok := p.object(0)
	if !ok {
		return nil, 0, false
	}
	dict, isDict := v.(pdfDict)
	if !isDict {
		return v, g.i, true
	}
	save := p.lx.pos
	if t := p.lx.next(); t.kind != tkKeyword || t.s != "stream" {
		p.lx.pos = save
		return dict, g.i, true
	}
	start := p.lx.pos
	if start < len(d.data) && d.data[start] == '\r' {
		start++
	}
	if start < len(d.data) && d.data[start] == '\n' {
		start++
	}
	end := -1
	if n, ok := asInt(d.lengthOf(dict["Length"], num)); ok && n >= 0 && start+n <= len(d.data) {
		rest := bytes.TrimLeft(d.data[start+n:min(len(d.data), start+n+64)], " \t\r\n\f\x00")
		if bytes.HasPrefix(rest, []byte("endstream")) {
			end = start + n
		}
	}
	if end < 0 {
		i := bytes.Index(d.data[start:], []byte("endstream"))
		if i < 0 {
			return nil, 0, false
		}
		end = start + i
		switch {
		case end-2 >= start && d.data[end-2] == '\r' && d.data[end-1] == '\n':
			end -= 2
		case end-1 >= start && (d.data[end-1] == '\n' || d.data[end-1] == '\r'):
			end--
		}
	}
	return &pdfStream{dict: dict, raw: d.data[start:end], num: num, gen: g.i}, g.i, true
}

// lengthOf is a stream's /Length: direct, or an object of its own (which a
// stream may not be, nor the stream itself).
func (d *pdfDoc) lengthOf(v any, self int) any {
	if r, ok := v.(pdfRef); ok {
		if r.num == self {
			return nil
		}
		v = d.load(r.num)
	}
	if _, isStream := v.(*pdfStream); isStream {
		return nil
	}
	return v
}

// objStream is object stream num, decoded, with its index read.
func (d *pdfDoc) objStream(num int) *objStream {
	if os, ok := d.stms[num]; ok {
		return os
	}
	d.stms[num] = nil
	if e := d.xref[num]; e.inStm {
		return nil
	}
	s, ok := d.load(num).(*pdfStream)
	if !ok {
		return nil
	}
	data, err := d.decode(s)
	if err != nil {
		d.failLimit(err)
		return nil
	}
	n, _ := asInt(s.dict["N"])
	first, _ := asInt(s.dict["First"])
	if n <= 0 || first < 0 || first > len(data) || n > len(data) {
		return nil
	}
	os := &objStream{data: data, first: first, offs: map[int]int{}}
	p := &parser{lx: lexer{b: data[:first], bud: d.bud}, maxDepth: 1}
	for range n {
		o, off := p.lx.next(), p.lx.next()
		if o.kind != tkInt || off.kind != tkInt {
			break
		}
		if _, dup := os.offs[o.i]; !dup {
			os.offs[o.i] = off.i
		}
	}
	d.stms[num] = os
	return os
}

// fromObjStream is object num, from object stream stm.
func (d *pdfDoc) fromObjStream(stm, num int) any {
	os := d.objStream(stm)
	if os == nil {
		return nil
	}
	off, ok := os.offs[num]
	if !ok || os.first+off < 0 || os.first+off >= len(os.data) {
		return nil
	}
	p := &parser{lx: lexer{b: os.data, pos: os.first + off, bud: d.bud}, maxDepth: min(d.bud.lim.MaxDepth, 128), refs: true}
	v, _ := p.object(0)
	if p.lx.err != nil {
		d.fail(p.lx.err)
	}
	return v
}

// dict and array resolve v as a dictionary, an array.
func (d *pdfDoc) dict(v any) pdfDict {
	switch x := d.resolve(v).(type) {
	case pdfDict:
		return x
	case *pdfStream:
		return x.dict
	}
	return nil
}

func (d *pdfDoc) array(v any) pdfArray {
	a, _ := d.resolve(v).(pdfArray)
	return a
}

// pdfPage is a page, with the resources it inherits.
type pdfPage struct {
	dict pdfDict
	res  pdfDict
}

// pages are the document's pages, in order, at most max of them, and how
// many it has in all: the page tree walked depth first, a node met twice
// (a tree that loops) passed over, its resources passed down.
func (d *pdfDoc) pages(max int) ([]pdfPage, int) {
	type frame struct {
		node  any
		res   pdfDict
		depth int
	}
	cat := d.catalog()
	stack := []frame{{node: cat["Pages"]}}
	seen := map[int]bool{}
	var out []pdfPage
	total := 0
	for len(stack) > 0 && d.err == nil {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if r, ok := f.node.(pdfRef); ok {
			if seen[r.num] {
				continue
			}
			seen[r.num] = true
		}
		node := d.dict(f.node)
		if node == nil || f.depth > 64 {
			continue
		}
		if err := d.bud.tick(); err != nil {
			d.fail(err)
			break
		}
		res := f.res
		if r := d.dict(node["Resources"]); r != nil {
			res = r
		}
		kids := d.array(node["Kids"])
		if typ := asName(node["Type"]); typ == "Pages" || typ != "Page" && kids != nil && node["Contents"] == nil {
			for i := len(kids) - 1; i >= 0; i-- {
				stack = append(stack, frame{node: kids[i], res: res, depth: f.depth + 1})
			}
			continue
		}
		total++
		if len(out) < max {
			out = append(out, pdfPage{dict: node, res: res})
		}
	}
	return out, total
}
