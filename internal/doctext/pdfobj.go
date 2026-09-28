package doctext

import (
	"strconv"
)

// PDF's objects (ISO 32000-1 §7.3), as the reader holds them: nil for
// null, bool, int, float64, pdfName, pdfString (its bytes), pdfArray,
// pdfDict, pdfRef, *pdfStream; and pdfKeyword for an operator of a content
// stream, or a keyword out of place.

type (
	pdfName    string
	pdfString  string
	pdfKeyword string
	pdfArray   []any
	pdfDict    map[pdfName]any
	pdfRef     struct{ num, gen int }
)

// pdfStream is a stream: its dictionary and its bytes as the file holds
// them, still encoded (and encrypted, when the file is); num and gen are
// those of the object that holds it, which its decryption key depends on.
type pdfStream struct {
	dict     pdfDict
	raw      []byte
	num, gen int
}

// token kinds.
type tokKind uint8

const (
	tkEOF tokKind = iota
	tkInt
	tkReal
	tkName
	tkString
	tkKeyword
	tkArrayOpen
	tkArrayClose
	tkDictOpen
	tkDictClose
	tkBraceOpen
	tkBraceClose
)

type token struct {
	kind tokKind
	s    string
	i    int
	f    float64
}

// lexer reads PDF's tokens from b; every token spends one from the budget.
type lexer struct {
	b   []byte
	pos int
	bud *budget
	err error
}

func isWhite(c byte) bool {
	return c == 0 || c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == ' '
}

func isDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func (l *lexer) skipSpace() {
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		switch {
		case isWhite(c):
			l.pos++
		case c == '%':
			for l.pos < len(l.b) && l.b[l.pos] != '\n' && l.b[l.pos] != '\r' {
				l.pos++
			}
		default:
			return
		}
	}
}

// next is the next token; tkEOF at the end, or once the budget is spent
// (l.err then says so).
func (l *lexer) next() token {
	if l.err != nil {
		return token{kind: tkEOF}
	}
	if l.bud != nil {
		if err := l.bud.tick(); err != nil {
			l.err = err
			return token{kind: tkEOF}
		}
	}
	l.skipSpace()
	// A lone ) or >, which begins no token, is passed over.
	for l.pos < len(l.b) && (l.b[l.pos] == ')' || l.b[l.pos] == '>' && (l.pos+1 >= len(l.b) || l.b[l.pos+1] != '>')) {
		l.pos++
		l.skipSpace()
	}
	if l.pos >= len(l.b) {
		return token{kind: tkEOF}
	}
	c := l.b[l.pos]
	switch c {
	case '[':
		l.pos++
		return token{kind: tkArrayOpen}
	case ']':
		l.pos++
		return token{kind: tkArrayClose}
	case '{':
		l.pos++
		return token{kind: tkBraceOpen}
	case '}':
		l.pos++
		return token{kind: tkBraceClose}
	case '(':
		l.pos++
		return token{kind: tkString, s: l.literal()}
	case '<':
		if l.pos+1 < len(l.b) && l.b[l.pos+1] == '<' {
			l.pos += 2
			return token{kind: tkDictOpen}
		}
		l.pos++
		return token{kind: tkString, s: l.hex()}
	case '>':
		l.pos += 2
		return token{kind: tkDictClose}
	case '/':
		l.pos++
		return token{kind: tkName, s: l.name()}
	}
	start := l.pos
	for l.pos < len(l.b) && !isWhite(l.b[l.pos]) && !isDelim(l.b[l.pos]) {
		l.pos++
	}
	word := l.b[start:l.pos]
	if t, ok := number(word); ok {
		return t
	}
	return token{kind: tkKeyword, s: string(word)}
}

// number reads word as a number: an integer, or a real, as PDF writes
// them (no exponent); a sign written twice, as some writers do, counts
// once.
func number(word []byte) (token, bool) {
	if len(word) == 0 || len(word) > 64 {
		return token{}, false
	}
	i := 0
	neg := false
	for i < len(word) && (word[i] == '+' || word[i] == '-') {
		neg = neg || word[i] == '-'
		i++
	}
	digits, dot := 0, false
	for j := i; j < len(word); j++ {
		switch c := word[j]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' && !dot:
			dot = true
		default:
			return token{}, false
		}
	}
	if digits == 0 {
		return token{}, false
	}
	s := string(word[i:])
	if !dot {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n > 1<<53 {
			return token{kind: tkReal, f: 0}, true
		}
		if neg {
			n = -n
		}
		return token{kind: tkInt, i: int(n), f: float64(n)}, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		f = 0
	}
	if neg {
		f = -f
	}
	return token{kind: tkReal, f: f}, true
}

// literal reads a literal string's bytes after its (.
func (l *lexer) literal() string {
	var out []byte
	depth := 1
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		l.pos++
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return string(out)
			}
		case '\\':
			if l.pos >= len(l.b) {
				return string(out)
			}
			e := l.b[l.pos]
			l.pos++
			switch e {
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			case 'b':
				c = '\b'
			case 'f':
				c = '\f'
			case '\r':
				if l.pos < len(l.b) && l.b[l.pos] == '\n' {
					l.pos++
				}
				continue
			case '\n':
				continue
			default:
				if e >= '0' && e <= '7' {
					v := int(e - '0')
					for k := 0; k < 2 && l.pos < len(l.b) && l.b[l.pos] >= '0' && l.b[l.pos] <= '7'; k++ {
						v = v*8 + int(l.b[l.pos]-'0')
						l.pos++
					}
					// Three octal digits may pass 255: the high bit goes,
					// as readers take it.
					c = byte(v & 0xFF)
				} else {
					c = e
				}
			}
		case '\r':
			// An end of line in a string is a line feed, however written.
			if l.pos < len(l.b) && l.b[l.pos] == '\n' {
				l.pos++
			}
			c = '\n'
		}
		out = append(out, c)
	}
	return string(out)
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// hex reads a hexadecimal string's bytes after its <.
func (l *lexer) hex() string {
	var out []byte
	var hi byte
	half := false
	for l.pos < len(l.b) {
		c := l.b[l.pos]
		l.pos++
		if c == '>' {
			break
		}
		v, ok := unhex(c)
		if !ok {
			continue
		}
		if half {
			out = append(out, hi<<4|v)
		} else {
			hi = v
		}
		half = !half
	}
	if half {
		out = append(out, hi<<4)
	}
	return string(out)
}

// name reads a name after its /, with its #xx escapes.
func (l *lexer) name() string {
	var out []byte
	for l.pos < len(l.b) && !isWhite(l.b[l.pos]) && !isDelim(l.b[l.pos]) {
		c := l.b[l.pos]
		l.pos++
		if c == '#' && l.pos+1 < len(l.b) {
			h, ok1 := unhex(l.b[l.pos])
			lo, ok2 := unhex(l.b[l.pos+1])
			if ok1 && ok2 {
				c = h<<4 | lo
				l.pos += 2
			}
		}
		out = append(out, c)
	}
	return string(out)
}

// parser reads objects from a lexer, arrays and dictionaries nesting at
// most maxDepth deep. refs: "n g R" is a reference (off for a content
// stream, where it never is).
type parser struct {
	lx       lexer
	maxDepth int
	refs     bool
}

// errDepth is set on the lexer when objects nest past the limit.
func (p *parser) tooDeep() {
	if p.lx.err == nil {
		p.lx.err = limitf("its objects nest more than %d deep", p.maxDepth)
	}
}

// object reads one object; ok is false at the end, or at a token that
// begins none (a closing bracket), which is consumed.
func (p *parser) object(depth int) (any, bool) {
	t := p.lx.next()
	return p.from(t, depth)
}

func (p *parser) from(t token, depth int) (any, bool) {
	switch t.kind {
	case tkEOF:
		return nil, false
	case tkInt:
		if p.refs {
			save := p.lx.pos
			if t2 := p.lx.next(); t2.kind == tkInt && t2.i >= 0 {
				if t3 := p.lx.next(); t3.kind == tkKeyword && t3.s == "R" {
					return pdfRef{t.i, t2.i}, true
				}
			}
			p.lx.pos = save
		}
		return t.i, true
	case tkReal:
		return t.f, true
	case tkName:
		return pdfName(t.s), true
	case tkString:
		return pdfString(t.s), true
	case tkArrayOpen:
		if depth >= p.maxDepth {
			p.tooDeep()
			return nil, false
		}
		var arr pdfArray
		for {
			t := p.lx.next()
			if t.kind == tkArrayClose || t.kind == tkEOF {
				return arr, true
			}
			if t.kind == tkDictClose {
				continue
			}
			v, ok := p.from(t, depth+1)
			if !ok && p.lx.err != nil {
				return arr, true
			}
			arr = append(arr, v)
		}
	case tkDictOpen:
		if depth >= p.maxDepth {
			p.tooDeep()
			return nil, false
		}
		d := pdfDict{}
		for {
			t := p.lx.next()
			if t.kind == tkDictClose || t.kind == tkEOF {
				return d, true
			}
			if t.kind != tkName {
				// A key that is no name: passed over, as readers do.
				continue
			}
			vt := p.lx.next()
			if vt.kind == tkDictClose {
				d[pdfName(t.s)] = nil
				return d, true
			}
			v, _ := p.from(vt, depth+1)
			d[pdfName(t.s)] = v
		}
	case tkKeyword:
		switch t.s {
		case "true":
			return true, true
		case "false":
			return false, true
		case "null":
			return nil, true
		}
		return pdfKeyword(t.s), true
	case tkBraceOpen, tkBraceClose:
		return pdfKeyword(map[tokKind]string{tkBraceOpen: "{", tkBraceClose: "}"}[t.kind]), true
	}
	return nil, false
}

// Accessors that take whatever a file put where a type was due.

func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case float64:
		if x >= -1<<53 && x <= 1<<53 {
			return int(x), true
		}
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func asName(v any) string {
	n, _ := v.(pdfName)
	return string(n)
}
