package doctext

import (
	"slices"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// CMaps (ISO 32000-1 §9.7.5, §9.10.3): how a font's codes are split out of
// a string, and what text each stands for. A ToUnicode CMap maps codes to
// text (bfchar, bfrange); an embedded encoding CMap gives the codes' lengths
// (codespacerange). Adobe's predefined CMaps are known by name: the Unicode
// ones (Uni…-UCS2, -UTF16, -UTF8, -UTF32) whose codes are the text, and the
// ones of a national character set (GBK, Big Five, Shift-JIS, EUC), whose
// codes are read as that character set; Identity's are glyphs, and say
// nothing of the text without a ToUnicode CMap.

// codeRange is a codespace range: codes of n bytes, each byte between
// lo's and hi's.
type codeRange struct {
	n      int
	lo, hi [4]byte
}

// cmapRange maps the codes of n bytes from lo to hi: to dst, its last
// UTF-16 unit counted up from lo; or to arr, one text each.
type cmapRange struct {
	n      int
	lo, hi uint32
	dst    []uint16
	arr    []string
}

// cmap is a CMap as read.
type cmap struct {
	spaces []codeRange
	single map[uint64]string
	ranges []cmapRange
	// use is the predefined CMap it names with usecmap.
	use string
	// vertical: its WMode is 1.
	vertical bool
}

// maxCMapEntries bounds the mappings one CMap may hold.
const maxCMapEntries = 1 << 18

// parseCMap reads a CMap stream's data.
func parseCMap(data []byte, b *budget) *cmap {
	m := &cmap{single: map[uint64]string{}}
	p := &parser{lx: lexer{b: data, bud: b}, maxDepth: 4}
	var ops []any
	entries := 0
	for entries < maxCMapEntries {
		v, ok := p.object(0)
		if !ok {
			break
		}
		kw, isOp := v.(pdfKeyword)
		if !isOp {
			if len(ops) < 1<<16 {
				ops = append(ops, v)
			}
			continue
		}
		switch kw {
		case "endcodespacerange":
			for i := 0; i+1 < len(ops); i += 2 {
				lo, _ := ops[i].(pdfString)
				hi, _ := ops[i+1].(pdfString)
				if n := len(lo); n >= 1 && n <= 4 && len(hi) == n {
					r := codeRange{n: n}
					copy(r.lo[:], lo)
					copy(r.hi[:], hi)
					m.spaces = append(m.spaces, r)
				}
			}
		case "endbfchar":
			for i := 0; i+1 < len(ops); i += 2 {
				src, _ := ops[i].(pdfString)
				if n := len(src); n >= 1 && n <= 4 {
					m.single[key(codeOf(src), n)] = textOfDst(ops[i+1])
					entries++
				}
			}
		case "endbfrange":
			for i := 0; i+2 < len(ops); i += 3 {
				lo, _ := ops[i].(pdfString)
				hi, _ := ops[i+1].(pdfString)
				n := len(lo)
				if n < 1 || n > 4 || len(hi) != n || codeOf(hi) < codeOf(lo) {
					continue
				}
				r := cmapRange{n: n, lo: codeOf(lo), hi: codeOf(hi)}
				switch dst := ops[i+2].(type) {
				case pdfString:
					r.dst = utf16Units(string(dst))
				case pdfArray:
					for _, e := range dst {
						r.arr = append(r.arr, textOfDst(e))
					}
				default:
					continue
				}
				m.ranges = append(m.ranges, r)
				entries++
			}
		case "usecmap":
			if len(ops) > 0 {
				m.use = asName(ops[len(ops)-1])
			}
		case "def":
			if len(ops) >= 2 && asName(ops[len(ops)-2]) == "WMode" {
				if n, ok := asInt(ops[len(ops)-1]); ok && n == 1 {
					m.vertical = true
				}
			}
		}
		ops = ops[:0]
	}
	sort.SliceStable(m.ranges, func(i, j int) bool {
		if m.ranges[i].n != m.ranges[j].n {
			return m.ranges[i].n < m.ranges[j].n
		}
		return m.ranges[i].lo < m.ranges[j].lo
	})
	return m
}

func key(code uint32, n int) uint64 { return uint64(n)<<32 | uint64(code) } //nolint:gosec // n is a code's length, 1 to 4.

func codeOf(s pdfString) uint32 {
	var c uint32
	for i := 0; i < len(s) && i < 4; i++ {
		c = c<<8 | uint32(s[i])
	}
	return c
}

// textOfDst is a ToUnicode destination as text: a UTF-16BE string, or a
// glyph's name (which some writers give).
func textOfDst(v any) string {
	switch x := v.(type) {
	case pdfString:
		return string(utf16.Decode(utf16Units(string(x))))
	case pdfName:
		return glyphText(string(x))
	}
	return ""
}

func utf16Units(s string) []uint16 {
	u := make([]uint16, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		u = append(u, uint16(s[i])<<8|uint16(s[i+1]))
	}
	if len(s)%2 == 1 {
		// A single byte, as broken maps write: taken as the code point.
		u = append(u, uint16(s[len(s)-1]))
	}
	return u
}

// lookup is the text the code of n bytes stands for, and whether the map
// has it.
func (m *cmap) lookup(code uint32, n int) (string, bool) {
	if s, ok := m.single[key(code, n)]; ok {
		return s, true
	}
	i := sort.Search(len(m.ranges), func(i int) bool {
		r := m.ranges[i]
		return r.n > n || r.n == n && r.lo > code
	})
	for j := i - 1; j >= 0 && j >= i-4; j-- {
		r := m.ranges[j]
		if r.n != n || code < r.lo || code > r.hi {
			continue
		}
		off := code - r.lo
		if r.arr != nil {
			if int(off) < len(r.arr) {
				return r.arr[off], true
			}
			return "", false
		}
		if len(r.dst) == 0 {
			return "", false
		}
		u := slices.Clone(r.dst)
		u[len(u)-1] += uint16(off) //nolint:gosec // a range's offset stays within its last unit, as the specification has it.
		return string(utf16.Decode(u)), true
	}
	return "", false
}

// split is the length of the code at the start of s by the codespace
// ranges: the shortest range that matches; with none, the shortest range's
// length (the code is then not in the map); 0 when m has no ranges.
func (m *cmap) split(s []byte) int {
	shortest := 0
	for n := 1; n <= 4 && n <= len(s); n++ {
		for _, r := range m.spaces {
			if r.n != n {
				continue
			}
			if shortest == 0 {
				shortest = n
			}
			match := true
			for i := range n {
				if s[i] < r.lo[i] || s[i] > r.hi[i] {
					match = false
					break
				}
			}
			if match {
				return n
			}
		}
	}
	if shortest == 0 {
		for _, r := range m.spaces {
			if shortest == 0 || r.n < shortest {
				shortest = r.n
			}
		}
	}
	return min(shortest, len(s))
}

// predefined is one of Adobe's CMaps by name: how it splits codes, and the
// text a code stands for when the codes are Unicode's or a character
// set's.
type predefined struct {
	split  func(s []byte) int
	decode func(code []byte) (string, bool)
	// vertical: the CMap's name ends in -V (or is V).
	vertical bool
}

func twoBytes([]byte) int { return 2 }

// predefinedCMap is the predefined CMap named name, and whether it is
// one known here.
func predefinedCMap(name string) (predefined, bool) {
	vertical := strings.HasSuffix(name, "-V") || name == "V"
	p := predefined{vertical: vertical}
	switch {
	case name == "Identity-H" || name == "Identity-V":
		p.split = twoBytes
	case strings.HasPrefix(name, "Uni") && strings.Contains(name, "UCS2"):
		p.split, p.decode = twoBytes, decodeUTF16
	case strings.HasPrefix(name, "Uni") && strings.Contains(name, "UTF16"):
		p.split = func(s []byte) int {
			if len(s) >= 4 && s[0] >= 0xD8 && s[0] <= 0xDB {
				return 4
			}
			return 2
		}
		p.decode = decodeUTF16
	case strings.HasPrefix(name, "Uni") && strings.Contains(name, "UTF8"):
		p.split = func(s []byte) int {
			switch c := s[0]; {
			case c < 0xC0:
				return 1
			case c < 0xE0:
				return 2
			case c < 0xF0:
				return 3
			}
			return 4
		}
		p.decode = func(code []byte) (string, bool) { return string(code), utf8.Valid(code) }
	case strings.HasPrefix(name, "Uni") && strings.Contains(name, "UTF32"):
		p.split = func([]byte) int { return 4 }
		p.decode = func(code []byte) (string, bool) {
			c := codeOf(pdfString(code))
			if c > utf8.MaxRune {
				return "", false
			}
			r := rune(c)
			return string(r), utf8.ValidRune(r)
		}
	case strings.HasPrefix(name, "GB") && !strings.HasPrefix(name, "GBT"):
		p.split = func(s []byte) int {
			switch {
			case s[0] < 0x81:
				return 1
			case len(s) >= 4 && s[1] >= 0x30 && s[1] <= 0x39:
				return 4
			}
			return 2
		}
		p.decode = charset(simplifiedchinese.GB18030)
	case strings.Contains(name, "B5"):
		p.split = func(s []byte) int {
			if s[0] < 0x81 {
				return 1
			}
			return 2
		}
		p.decode = charset(traditionalchinese.Big5)
	case strings.Contains(name, "RKSJ"):
		p.split = func(s []byte) int {
			if c := s[0]; c >= 0x81 && c <= 0x9F || c >= 0xE0 && c <= 0xFC {
				return 2
			}
			return 1
		}
		p.decode = charset(japanese.ShiftJIS)
	case name == "EUC-H" || name == "EUC-V":
		p.split = func(s []byte) int {
			switch c := s[0]; {
			case c == 0x8F:
				return 3
			case c == 0x8E || c >= 0xA1:
				return 2
			}
			return 1
		}
		p.decode = charset(japanese.EUCJP)
	case name == "H" || name == "V":
		// JIS X 0208 in its 7-bit form: EUC-JP less its high bits.
		p.split = twoBytes
		p.decode = func(code []byte) (string, bool) {
			return charset(japanese.EUCJP)([]byte{code[0] | 0x80, code[len(code)-1] | 0x80})
		}
	case strings.HasPrefix(name, "KSC"):
		if name == "KSC-H" || name == "KSC-V" {
			p.split = twoBytes
			p.decode = func(code []byte) (string, bool) {
				return charset(korean.EUCKR)([]byte{code[0] | 0x80, code[len(code)-1] | 0x80})
			}
			break
		}
		p.split = func(s []byte) int {
			if s[0] < 0x81 {
				return 1
			}
			return 2
		}
		p.decode = charset(korean.EUCKR)
	default:
		return predefined{}, false
	}
	return p, true
}

func decodeUTF16(code []byte) (string, bool) {
	s := string(utf16.Decode(utf16Units(string(code))))
	return s, !strings.ContainsRune(s, utf8.RuneError)
}

// charset decodes a code as a character set does.
func charset(e encoding.Encoding) func(code []byte) (string, bool) {
	return func(code []byte) (string, bool) {
		out, err := e.NewDecoder().Bytes(code)
		if err != nil || len(out) == 0 || strings.ContainsRune(string(out), utf8.RuneError) {
			return "", false
		}
		return string(out), true
	}
}
