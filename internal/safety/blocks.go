package safety

import (
	"regexp"
	"strings"
)

// line is one line of the body.
type line struct {
	text string // without its newline
	nl   bool   // a newline followed it
	code bool   // inside a fenced or indented code block, fences included
	drop bool   // a reference definition being removed
}

func splitLines(s string) []line {
	var lines []line
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, line{text: s})
			break
		}
		lines = append(lines, line{text: s[:i], nl: true})
		s = s[i+1:]
	}
	return lines
}

func isBlank(s string) bool { return strings.TrimLeft(s, " \t") == "" }

// indentCols is the width of s's leading spaces and tabs, a tab reaching
// the next multiple of four columns, as CommonMark counts it.
func indentCols(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ':
			n++
		case '\t':
			n += 4 - n%4
		default:
			return n
		}
	}
	return n
}

var (
	listItemRe = regexp.MustCompile(`^ {0,3}(?:[-+*]|[0-9]{1,9}[.)])(?:[ \t]|$)`)
	headingRe  = regexp.MustCompile(`^#{1,6}(?:[ \t]|$)`)
	// HTML blocks whose end is a line holding their end condition.
	htmlRawRe   = regexp.MustCompile(`(?i)^ {0,3}<(?:script|pre|style|textarea)(?:[ \t>]|$)`)
	htmlRawEnd  = regexp.MustCompile(`(?i)</(?:script|pre|style|textarea)>`)
	htmlStartRe = regexp.MustCompile(`^ {0,3}<(?:!--|\?|![A-Za-z]|!\[CDATA\[|/?[A-Za-z])`)
)

// htmlBlock is the kind of HTML block a line is in, by how it ends.
type htmlBlock int

const (
	htmlNone    htmlBlock = iota
	htmlRaw               // <script>, <pre>, <style>, <textarea>: to the closing tag
	htmlComment           // <!-- … -->
	htmlPI                // <? … ?>
	htmlDecl              // <!X … >
	htmlCDATA             // <![CDATA[ … ]]>
	htmlToBlank           // any other tag: to a blank line
)

func htmlStart(s string) htmlBlock {
	switch {
	case htmlRawRe.MatchString(s):
		return htmlRaw
	case !htmlStartRe.MatchString(s):
		return htmlNone
	}
	t := strings.TrimLeft(s, " ")
	switch {
	case strings.HasPrefix(t, "<!--"):
		return htmlComment
	case strings.HasPrefix(t, "<?"):
		return htmlPI
	case strings.HasPrefix(t, "<![CDATA["):
		return htmlCDATA
	case strings.HasPrefix(t, "<!"):
		return htmlDecl
	}
	return htmlToBlank
}

// ends reports whether line s holds the end of an HTML block of kind k.
func (k htmlBlock) ends(s string) bool {
	switch k {
	case htmlRaw:
		return htmlRawEnd.MatchString(s)
	case htmlComment:
		return strings.Contains(s, "-->")
	case htmlPI:
		return strings.Contains(s, "?>")
	case htmlDecl:
		return strings.Contains(s, ">")
	case htmlCDATA:
		return strings.Contains(s, "]]>")
	}
	return false
}

// openingFence reports whether s opens a fenced code block: at most three
// columns of indentation, then three or more backticks or tildes; a
// backtick fence's info string holds no backtick.
func openingFence(s string) (ch byte, n int, ok bool) {
	t := strings.TrimLeft(s, " \t")
	if indentCols(s) > 3 || len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return 0, 0, false
	}
	ch = t[0]
	n = runLen(t, 0, ch)
	if n < 3 || (ch == '`' && strings.IndexByte(t[n:], '`') >= 0) {
		return 0, 0, false
	}
	return ch, n, true
}

// closingFence reports whether s closes a fence of ch at least n long.
func closingFence(s string, ch byte, n int) bool {
	t := strings.TrimLeft(s, " \t")
	if indentCols(s) > 3 || len(t) < n || t[0] != ch {
		return false
	}
	m := runLen(t, 0, ch)
	return m >= n && isBlank(t[m:])
}

func runLen(s string, i int, ch byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == ch {
		n++
	}
	return n
}

// classify marks the lines that are code: fenced and indented code blocks.
// It keeps to the cases where every CommonMark renderer agrees, and calls
// anything else text, so that a link is never left alone for looking like
// code to Body and not to the renderer:
//   - A fence opened with indentation (inside a list item) ends at a line
//     indented less than it, since that line ends the item and the fence.
//   - Indented code is recognised only after a blank line, and never while a
//     list may be open, since there four spaces are a paragraph of the item.
//   - Nothing inside an HTML block is code; neither is a fence or indented
//     code inside a block quote.
func classify(lines []line) {
	var (
		fence       byte // the fence's character while in a fenced block
		fenceLen    int
		fenceIndent int
		indented    bool
		inList      bool
		html        htmlBlock
		prevBlank   = true
	)
	for i := range lines {
		l := &lines[i]
		t := l.text
		blank := isBlank(t)
		ind := indentCols(t)
		if fence != 0 {
			switch {
			case closingFence(t, fence, fenceLen):
				l.code, fence, prevBlank = true, 0, false
				continue
			case fenceIndent > 0 && !blank && ind < fenceIndent:
				fence = 0 // the list item ended, and the fence with it
			default:
				l.code = true
				continue
			}
		}
		if indented {
			if blank || ind >= 4 {
				l.code = true
				continue
			}
			indented = false
		}
		if html != htmlNone {
			switch {
			case blank && html == htmlToBlank:
				html = htmlNone
			case html.ends(t):
				html = htmlNone
			}
			prevBlank = blank
			continue
		}
		if blank {
			prevBlank = true
			continue
		}
		if ch, n, ok := openingFence(t); ok {
			fence, fenceLen, fenceIndent = ch, n, ind
			l.code, prevBlank = true, false
			continue
		}
		if ind >= 4 && prevBlank && !inList {
			indented, l.code = true, true
			continue
		}
		if ind <= 3 {
			if k := htmlStart(t); k != htmlNone {
				if !k.ends(t) {
					html = k
				}
				prevBlank = false
				continue
			}
			switch {
			case listItemRe.MatchString(t):
				inList = true
			case ind == 0 && (prevBlank || headingRe.MatchString(t)):
				// A line at the margin after a blank line, or a heading,
				// is outside any list.
				inList = false
			}
		}
		prevBlank = false
	}
}
