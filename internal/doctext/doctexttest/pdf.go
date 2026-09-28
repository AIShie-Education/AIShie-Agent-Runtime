package doctexttest

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" //nolint:gosec // PDF's standard security handler, to make files the reader must open.
	"crypto/rc4" //nolint:gosec // idem.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
)

// PDFPage is one page of a PDF: what it draws, in this order.
type PDFPage struct {
	// Lines are lines of text in Helvetica (WinAnsiEncoding), one under
	// the other.
	Lines []string
	// CJK are lines in a composite font (Identity-H) whose ToUnicode CMap
	// maps each code to its character, as Word and PowerPoint write CJK.
	CJK []string
	// Image draws a picture (a scanned page is one and no text).
	Image bool
	// Hidden are lines drawn invisibly (text rendering mode 3), as a
	// scan's recognized text is.
	Hidden []string
	// Form are lines drawn inside a form XObject.
	Form []string
	// Raw is content added as it is; F1 is Helvetica and F2 the composite
	// font (whose codes CJKCodes gives).
	Raw string
}

// PDFOptions say how a PDF is written.
type PDFOptions struct {
	// Compress writes its streams with FlateDecode.
	Compress bool
	// ObjectStreams keeps its objects in an object stream, and its
	// cross-reference in a stream (PDF 1.5).
	ObjectStreams bool
	// Encrypt is "" (none), "rc4" (128-bit, revision 3) or "aes256"
	// (revision 6), with the empty owner password "owner" and the user
	// password UserPassword ("" for a file anyone may open).
	Encrypt      string
	UserPassword string
	// BrokenToUnicode gives the composite font a ToUnicode CMap that maps
	// every code to 《, as broken writers do; NoToUnicode gives it none.
	BrokenToUnicode, NoToUnicode bool
	// NestedPages puts the pages under an intermediate Pages node.
	NestedPages bool
}

// PDF is a PDF of pages, uncompressed.
func PDF(pages ...PDFPage) []byte { return PDFWith(PDFOptions{}, pages...) }

// PDFLines is a one-page PDF of lines.
func PDFLines(lines ...string) []byte { return PDF(PDFPage{Lines: lines}) }

// CJKCodes are the codes the composite font gives the characters of
// lines, in the order first met from 1, and the text of each.
func CJKCodes(lines ...string) map[rune]int {
	codes := map[rune]int{}
	for _, l := range lines {
		for _, r := range l {
			if _, ok := codes[r]; !ok {
				codes[r] = len(codes) + 1
			}
		}
	}
	return codes
}

type pdfObj struct {
	body   string
	stream []byte
	// plain streams are never encrypted (a cross-reference stream).
	plain bool
}

type pdfWriter struct {
	o    PDFOptions
	objs []pdfObj
}

func (w *pdfWriter) add(body string) int {
	w.objs = append(w.objs, pdfObj{body: body})
	return len(w.objs)
}

func (w *pdfWriter) addStream(dict string, data []byte) int {
	w.objs = append(w.objs, pdfObj{body: dict, stream: data})
	return len(w.objs)
}

func (w *pdfWriter) set(n int, body string) { w.objs[n-1].body = body }

func escPDF(s string) string {
	return strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(s)
}

func hexCodes(codes map[rune]int, s string) string {
	var sb strings.Builder
	sb.WriteString("<")
	for _, r := range s {
		fmt.Fprintf(&sb, "%04X", codes[r])
	}
	sb.WriteString(">")
	return sb.String()
}

// PDFWith is a PDF of pages, written as o says.
func PDFWith(o PDFOptions, pages ...PDFPage) []byte {
	w := &pdfWriter{o: o}
	catalog := w.add("")
	pagesObj := w.add("")
	helv := w.add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	var all []string
	for _, p := range pages {
		all = append(all, p.CJK...)
		all = append(all, p.Raw)
	}
	codes := CJKCodes(all...)
	var cmap strings.Builder
	cmap.WriteString("/CIDInit /ProcSet findresource begin 12 dict begin begincmap /CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n" +
		"/CMapName /Adobe-Identity-UCS def /CMapType 2 def 1 begincodespacerange <0000> <FFFF> endcodespacerange\n")
	rs := make([]rune, 0, len(codes))
	for r := range codes {
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool { return codes[rs[i]] < codes[rs[j]] })
	for i := 0; i < len(rs); i += 100 {
		chunk := rs[i:min(i+100, len(rs))]
		fmt.Fprintf(&cmap, "%d beginbfchar\n", len(chunk))
		for _, r := range chunk {
			dst := r
			if o.BrokenToUnicode {
				dst = '《'
			}
			var u strings.Builder
			for _, unit := range utf16.Encode([]rune{dst}) {
				fmt.Fprintf(&u, "%04X", unit)
			}
			fmt.Fprintf(&cmap, "<%04X> <%s>\n", codes[r], u.String())
		}
		cmap.WriteString("endbfchar\n")
	}
	cmap.WriteString("endcmap CMapName currentdict /CMap defineresource pop end end\n")
	toU := ""
	if !o.NoToUnicode {
		toU = fmt.Sprintf(" /ToUnicode %d 0 R", w.addStream("", []byte(cmap.String())))
	}
	desc := w.add("<< /Type /FontDescriptor /FontName /MingLiU /Flags 4 /FontBBox [0 -200 1000 800] /ItalicAngle 0 /Ascent 800 /Descent -200 /CapHeight 700 /StemV 80 >>")
	cid := w.add(fmt.Sprintf("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /MingLiU /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor %d 0 R /DW 1000 /CIDToGIDMap /Identity >>", desc))
	cjk := w.add(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /MingLiU /Encoding /Identity-H /DescendantFonts [%d 0 R]%s >>", cid, toU))
	image := w.addStream("<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /DCTDecode >>",
		[]byte("\xff\xd8\xff\xe0 not a real JPEG, never decoded \xff\xd9"))
	fonts := fmt.Sprintf("/Font << /F1 %d 0 R /F2 %d 0 R >>", helv, cjk)
	var kids []string
	for _, p := range pages {
		var c strings.Builder
		y := 760
		if len(p.Lines) > 0 {
			fmt.Fprintf(&c, "BT /F1 12 Tf 72 %d Td", y)
			for i, l := range p.Lines {
				if i > 0 {
					c.WriteString(" 0 -16 Td")
				}
				c.WriteString(" (" + escPDF(l) + ") Tj")
			}
			c.WriteString(" ET\n")
			y -= 16*len(p.Lines) + 20
		}
		if len(p.CJK) > 0 {
			fmt.Fprintf(&c, "BT /F2 14 Tf 72 %d Td", y)
			for i, l := range p.CJK {
				if i > 0 {
					c.WriteString(" 0 -18 Td")
				}
				c.WriteString(" " + hexCodes(codes, l) + " Tj")
			}
			c.WriteString(" ET\n")
			y -= 18*len(p.CJK) + 20
		}
		xobjs := ""
		if p.Image {
			fmt.Fprintf(&c, "q 468 0 0 600 72 %d cm /Im1 Do Q\n", max(y-600, 20))
			xobjs += fmt.Sprintf(" /Im1 %d 0 R", image)
		}
		if len(p.Hidden) > 0 {
			fmt.Fprintf(&c, "BT 3 Tr /F1 12 Tf 72 %d Td", y)
			for i, l := range p.Hidden {
				if i > 0 {
					c.WriteString(" 0 -16 Td")
				}
				c.WriteString(" (" + escPDF(l) + ") Tj")
			}
			c.WriteString(" ET\n")
		}
		if len(p.Form) > 0 {
			var fc strings.Builder
			fc.WriteString("BT /F1 12 Tf 0 0 Td")
			for i, l := range p.Form {
				if i > 0 {
					fc.WriteString(" 0 -16 Td")
				}
				fc.WriteString(" (" + escPDF(l) + ") Tj")
			}
			fc.WriteString(" ET")
			form := w.addStream(fmt.Sprintf("<< /Type /XObject /Subtype /Form /BBox [0 0 600 800] /Matrix [1 0 0 1 72 300] /Resources << %s >> >>", fonts),
				[]byte(fc.String()))
			c.WriteString("q /Fm1 Do Q\n")
			xobjs += fmt.Sprintf(" /Fm1 %d 0 R", form)
		}
		c.WriteString(p.Raw)
		content := w.addStream("", []byte(c.String()))
		res := "<< " + fonts
		if xobjs != "" {
			res += " /XObject <<" + xobjs + " >>"
		}
		res += " >>"
		page := w.add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Resources %s /Contents %d 0 R >>", pagesObj, res, content))
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	if o.NestedPages {
		mid := w.add("")
		w.set(mid, fmt.Sprintf("<< /Type /Pages /Parent %d 0 R /Kids [%s] /Count %d >>", pagesObj, strings.Join(kids, " "), len(kids)))
		for _, k := range kids {
			var n int
			_, _ = fmt.Sscanf(k, "%d", &n)
			w.set(n, strings.Replace(w.objs[n-1].body, fmt.Sprintf("/Parent %d 0 R", pagesObj), fmt.Sprintf("/Parent %d 0 R", mid), 1))
		}
		kids = []string{fmt.Sprintf("%d 0 R", mid)}
	}
	w.set(pagesObj, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	w.set(catalog, fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pagesObj))
	return w.bytes()
}

// bytes writes the file: its objects, then its cross-reference and
// trailer; encrypted and compressed as the options say.
func (w *pdfWriter) bytes() []byte {
	id := []byte("0123456789abcdef")
	var enc *encrypter
	encRef := 0
	if w.o.Encrypt != "" {
		enc = newEncrypter(w.o.Encrypt, w.o.UserPassword, id)
		encRef = w.add(enc.dict)
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(w.objs)+3)
	inStm := map[int]int{}
	var stmObjs []int
	if w.o.ObjectStreams {
		for i, o := range w.objs {
			if o.stream == nil && i+1 != encRef {
				stmObjs = append(stmObjs, i+1)
			}
		}
	}
	for i, o := range w.objs {
		n := i + 1
		if o.stream == nil && w.o.ObjectStreams && n != encRef {
			continue
		}
		offsets[n] = buf.Len()
		if o.stream == nil {
			fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", n, o.body)
			continue
		}
		data, filter := o.stream, ""
		if w.o.Compress {
			data, filter = deflate(data), " /Filter /FlateDecode"
		}
		if enc != nil && !o.plain {
			data = enc.encrypt(n, data)
		}
		dict := strings.TrimSuffix(strings.TrimSpace(o.body), ">>")
		if dict == "" {
			dict = "<<"
		}
		fmt.Fprintf(&buf, "%d 0 obj\n%s /Length %d%s >>\nstream\n", n, dict, len(data), filter)
		buf.Write(data)
		buf.WriteString("\nendstream\nendobj\n")
	}
	trailer := fmt.Sprintf("/Root 1 0 R /ID [<%x> <%x>]", id, id)
	if enc != nil {
		trailer += fmt.Sprintf(" /Encrypt %d 0 R", encRef)
	}
	if !w.o.ObjectStreams {
		x := buf.Len()
		fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(w.objs)+1)
		for n := 1; n <= len(w.objs); n++ {
			fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[n])
		}
		fmt.Fprintf(&buf, "trailer\n<< /Size %d %s >>\nstartxref\n%d\n%%%%EOF\n", len(w.objs)+1, trailer, x)
		return buf.Bytes()
	}
	// The objects in one object stream, then a cross-reference stream.
	var head, body strings.Builder
	for i, n := range stmObjs {
		inStm[n] = i
		fmt.Fprintf(&head, "%d %d ", n, body.Len())
		body.WriteString(w.objs[n-1].body + "\n")
	}
	stmNum := len(w.objs) + 1
	stmData := []byte(head.String() + body.String())
	first := head.Len()
	stmData = deflate(stmData)
	if enc != nil {
		stmData = enc.encrypt(stmNum, stmData)
	}
	offsets[stmNum] = buf.Len()
	fmt.Fprintf(&buf, "%d 0 obj\n<< /Type /ObjStm /N %d /First %d /Filter /FlateDecode /Length %d >>\nstream\n", stmNum, len(stmObjs), first, len(stmData))
	buf.Write(stmData)
	buf.WriteString("\nendstream\nendobj\n")
	xrefNum := stmNum + 1
	offsets[xrefNum] = buf.Len()
	var rows []byte
	for n := 0; n <= xrefNum; n++ {
		row := make([]byte, 7)
		switch idx, ok := inStm[n]; {
		case n == 0:
			row[5], row[6] = 0xFF, 0xFF
		case ok:
			row[0] = 2
			binary.BigEndian.PutUint32(row[1:5], uint32(stmNum)) //nolint:gosec // a test's few objects.
			binary.BigEndian.PutUint16(row[5:7], uint16(idx))    //nolint:gosec // idem.
		default:
			row[0] = 1
			binary.BigEndian.PutUint32(row[1:5], uint32(offsets[n])) //nolint:gosec // idem.
		}
		rows = append(rows, row...)
	}
	// PNG's Up predictor, as Acrobat writes cross-reference streams.
	var pred []byte
	prev := make([]byte, 7)
	for i := 0; i < len(rows); i += 7 {
		pred = append(pred, 2)
		for k := range 7 {
			pred = append(pred, rows[i+k]-prev[k])
		}
		prev = rows[i : i+7]
	}
	xdata := deflate(pred)
	fmt.Fprintf(&buf, "%d 0 obj\n<< /Type /XRef /Size %d /W [1 4 2] /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns 7 >> %s /Length %d >>\nstream\n",
		xrefNum, xrefNum+1, trailer, len(xdata))
	buf.Write(xdata)
	fmt.Fprintf(&buf, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", offsets[xrefNum])
	return buf.Bytes()
}

func deflate(data []byte) []byte {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	_, _ = zw.Write(data)
	_ = zw.Close()
	return b.Bytes()
}

// encrypter encrypts a file's streams as the standard security handler
// does.
type encrypter struct {
	method string
	key    []byte
	dict   string
}

var pad = []byte{
	0x28, 0xBF, 0x4E, 0x5E, 0x4E, 0x75, 0x8A, 0x41, 0x64, 0x00, 0x4E, 0x56, 0xFF, 0xFA, 0x01, 0x08,
	0x2E, 0x2E, 0x00, 0xB6, 0xD0, 0x68, 0x3E, 0x80, 0x2F, 0x0C, 0xA9, 0xFE, 0x64, 0x53, 0x69, 0x7A,
}

func padded(pw string) []byte { return append([]byte(pw), pad...)[:32] }

func rc4x(key, data []byte) []byte {
	c, _ := rc4.NewCipher(key) //nolint:gosec // PDF's RC4.
	out := make([]byte, len(data))
	c.XORKeyStream(out, data)
	return out
}

func xorAll(key []byte, b byte) []byte {
	out := make([]byte, len(key))
	for i := range key {
		out[i] = key[i] ^ b
	}
	return out
}

func newEncrypter(method, user string, id []byte) *encrypter {
	p := int32(-3904)
	if method == "aes256" {
		fileKey := bytes.Repeat([]byte{0x5A}, 32)
		vs, ks := []byte("uvsaltuv"), []byte("uksaltuk")
		u := append(append(hash6([]byte(user), vs, nil), vs...), ks...)
		ue := aesWrap(hash6([]byte(user), ks, nil), fileKey)
		ovs, oks := []byte("ovsaltov"), []byte("oksaltok")
		o := append(append(hash6([]byte("owner"), ovs, u), ovs...), oks...)
		oe := aesWrap(hash6([]byte("owner"), oks, u), fileKey)
		dict := fmt.Sprintf("<< /Filter /Standard /V 5 /R 6 /Length 256 /CF << /StdCF << /CFM /AESV3 /AuthEvent /DocOpen /Length 32 >> >> "+
			"/StmF /StdCF /StrF /StdCF /O <%x> /U <%x> /OE <%x> /UE <%x> /P %d /Perms <%x> >>", o, u, oe, ue, p, bytes.Repeat([]byte{1}, 16))
		return &encrypter{method: method, key: fileKey, dict: dict}
	}
	// Revision 3, 128 bits: O from the owner's password, then the key and
	// U from the user's.
	h := md5.Sum(padded("owner")) //nolint:gosec // Algorithm 3.
	okey := h[:]
	for range 50 {
		s := md5.Sum(okey[:16]) //nolint:gosec // Algorithm 3.
		okey = s[:]
	}
	o := rc4x(okey[:16], padded(user))
	for i := 1; i <= 19; i++ {
		o = rc4x(xorAll(okey[:16], byte(i)), o)
	}
	pb := make([]byte, 4)
	binary.LittleEndian.PutUint32(pb, uint32(p)) //nolint:gosec // P is a signed 32-bit field.
	kh := md5.New()                              //nolint:gosec // Algorithm 2.
	kh.Write(padded(user))
	kh.Write(o)
	kh.Write(pb)
	kh.Write(id)
	key := kh.Sum(nil)
	for range 50 {
		s := md5.Sum(key[:16]) //nolint:gosec // Algorithm 2.
		key = s[:]
	}
	key = key[:16]
	uh := md5.New() //nolint:gosec // Algorithm 5.
	uh.Write(pad)
	uh.Write(id)
	u := rc4x(key, uh.Sum(nil))
	for i := 1; i <= 19; i++ {
		u = rc4x(xorAll(key, byte(i)), u)
	}
	u = append(u, bytes.Repeat([]byte{0}, 16)...)
	dict := fmt.Sprintf("<< /Filter /Standard /V 2 /R 3 /Length 128 /O <%x> /U <%x> /P %d >>", o, u, p)
	return &encrypter{method: method, key: key, dict: dict}
}

func (e *encrypter) encrypt(num int, data []byte) []byte {
	if e.method == "aes256" {
		block, _ := aes.NewCipher(e.key)
		n := aes.BlockSize - len(data)%aes.BlockSize
		plain := append(bytes.Clone(data), bytes.Repeat([]byte{byte(n)}, n)...)
		iv := []byte("0123456789ABCDEF")
		out := make([]byte, len(plain))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, plain)
		return append(bytes.Clone(iv), out...)
	}
	h := md5.New() //nolint:gosec // Algorithm 1.
	h.Write(e.key)
	h.Write([]byte{byte(num), byte(num >> 8), byte(num >> 16), 0, 0}) //nolint:gosec // Algorithm 1's low bytes.
	return rc4x(h.Sum(nil)[:16], data)
}

func aesWrap(kek, key []byte) []byte {
	block, _ := aes.NewCipher(kek)
	out := make([]byte, len(key))
	cipher.NewCBCEncrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(out, key)
	return out
}

// hash6 is ISO 32000-2's Algorithm 2.B.
func hash6(pw, salt, udata []byte) []byte {
	k := sha256.Sum256(append(append(append([]byte(nil), pw...), salt...), udata...))
	key := k[:]
	for i := 0; ; i++ {
		k1 := bytes.Repeat(append(append(append([]byte(nil), pw...), key...), udata...), 64)
		block, _ := aes.NewCipher(key[:16])
		e := make([]byte, len(k1))
		cipher.NewCBCEncrypter(block, key[16:32]).CryptBlocks(e, k1)
		sum := 0
		for _, b := range e[:16] {
			sum += int(b)
		}
		switch sum % 3 {
		case 0:
			s := sha256.Sum256(e)
			key = s[:]
		case 1:
			s := sha512.Sum384(e)
			key = s[:]
		default:
			s := sha512.Sum512(e)
			key = s[:]
		}
		if i >= 63 && int(e[len(e)-1]) <= i+1-32 {
			break
		}
	}
	return key[:32]
}
