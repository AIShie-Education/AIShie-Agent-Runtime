package safety

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// text is a string the parser reads, with the offset in the body of each of
// its bytes: the renderer reads a paragraph without its block quote markers
// and indentation, and a table cell without the backslash of an escaped
// '|', so what it reads is not one slice of the body. A byte the parser made
// up (a space standing for part of a tab) has offset -1.
type text struct {
	s   string
	off []int
}

// sourceText is src[from:to], each byte at its own offset.
func sourceText(src string, from, to int) text {
	off := make([]int, to-from)
	for i := range off {
		off[i] = from + i
	}
	return text{s: src[from:to], off: off}
}

// madeUp is s, which the parser made up.
func madeUp(s string) text {
	off := make([]int, len(s))
	for i := range off {
		off[i] = -1
	}
	return text{s: s, off: off}
}

func (t text) slice(i, j int) text { return text{s: t.s[i:j], off: t.off[i:j]} }

func joinText(parts []text) text {
	n := 0
	for _, p := range parts {
		n += len(p.s)
	}
	var b strings.Builder
	b.Grow(n)
	off := make([]int, 0, n)
	for _, p := range parts {
		b.WriteString(p.s)
		off = append(off, p.off...)
	}
	return text{s: b.String(), off: off}
}

// asciiTrimText is markdown-it's asciiTrim: spaces, tabs and line endings
// off both ends, and no other white space.
func asciiTrimText(t text) text {
	start, end := 0, len(t.s)
	for start < end && isASCIITrimmable(t.s[start]) {
		start++
	}
	for end > start && isASCIITrimmable(t.s[end-1]) {
		end--
	}
	return t.slice(start, end)
}

func isASCIITrimmable(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// jsTrimText is JavaScript's String.prototype.trim, which markdown-it uses
// on table cells: every white space and line terminator off both ends.
func jsTrimText(t text) text {
	start, end := 0, len(t.s)
	for start < end {
		r, n := utf8.DecodeRuneInString(t.s[start:])
		if !isJSSpace(r) {
			break
		}
		start += n
	}
	for end > start {
		r, n := utf8.DecodeLastRuneInString(t.s[start:end])
		if !isJSSpace(r) {
			break
		}
		end -= n
	}
	return t.slice(start, end)
}

// jsTrim is JavaScript's trim on a plain string.
func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

// isJSSpace is JavaScript's white space and line terminators, what its \s
// and trim match.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// utf16Len is how many UTF-16 code units s is, which is what markdown-it's
// lengths count.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// The Unicode classes linkify-it's patterns are built from (uc.micro): Z
// is a separator, P punctuation, Cc a control character.
func isZ(r rune) bool    { return unicode.Is(unicode.Z, r) }
func isCc(r rune) bool   { return unicode.Is(unicode.Cc, r) }
func isZCc(r rune) bool  { return isZ(r) || isCc(r) }
func isZPCc(r rune) bool { return isZCc(r) || unicode.Is(unicode.P, r) }

// isPseudoLetter is linkify-it's src_pseudo_letter: anything but a
// separator, punctuation, a control character, '<', '>' or '｜'.
func isPseudoLetter(r rune) bool {
	return !isZPCc(r) && r != '<' && r != '>' && r != '｜'
}

// runeAt is the character at byte i of s, and its length; at the end it is
// -1 and 0.
func runeAt(s string, i int) (rune, int) {
	if i >= len(s) {
		return -1, 0
	}
	return utf8.DecodeRuneInString(s[i:])
}

func isASCIIAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isAlnum(c byte) bool      { return isASCIIAlpha(c) || isDigit(c) }

// isSpaceTab is markdown-it's isSpace: a space or a tab, nothing else.
func isSpaceTab(c byte) bool { return c == ' ' || c == '\t' }
