package safety

import (
	"sort"
	"strings"
)

// edit replaces s[start:end] with repl.
type edit struct {
	start, end int
	repl       string
}

// opener is a '[' or '![' that may begin a link or an image.
type opener struct {
	pos   int  // the '['
	image bool // preceded by '!'
	mark  int  // len(edits) when it was seen
}

// inliner rewrites one paragraph's inline links, left to right, as a
// CommonMark parser reads them: code spans, autolinks and HTML tags are
// taken where they begin, and a ']' closes the nearest '[' still open. It
// does not stop at CommonMark's rule that a link holds no link: every
// destination it finds is judged, whether or not the renderer would make it
// a link.
type inliner struct {
	s       string
	defs    *definitions
	rep     *Report
	edits   []edit
	openers []opener
	// openA counts <a> tags removed whose </a> is still to be removed.
	openA int
	// textFrom is where the text being scanned began: after the last
	// construct or edit. An email address is looked for back to it.
	textFrom int
}

// inline returns paragraph s with its links and images made safe.
func inline(s string, defs *definitions, rep *Report) string {
	in := &inliner{s: s, defs: defs, rep: rep}
	in.run()
	return apply(s, in.edits)
}

func (in *inliner) run() {
	s := in.s
	for i := 0; i < len(s); {
		next, construct := in.step(i)
		if construct {
			in.textFrom = next
		}
		i = next
	}
}

// step reads what begins at i and returns where reading goes on, and
// whether a construct (anything but plain text) ended there.
func (in *inliner) step(i int) (int, bool) {
	s := in.s
	switch c := s[i]; c {
	case '\\':
		if i+1 < len(s) && isASCIIPunct(s[i+1]) {
			return i + 2, false
		}
	case '`':
		n := runLen(s, i, '`')
		if end, ok := codeSpan(s, i, n); ok {
			return end, true
		}
		return i + n, false
	case '<':
		if end, ok := in.angle(i); ok {
			return end, true
		}
	case '!':
		if i+1 < len(s) && s[i+1] == '[' {
			in.openers = append(in.openers, opener{pos: i + 1, image: true, mark: len(in.edits)})
			return i + 2, true
		}
	case '[':
		in.openers = append(in.openers, opener{pos: i, mark: len(in.edits)})
		return i + 1, true
	case ']':
		return in.closeBracket(i), true
	case '@':
		if end, ok := in.email(i); ok {
			return end, true
		}
	}
	return in.bare(i)
}

// codeSpan finds the end of a code span opened by n backticks at i: the
// next run of exactly n backticks. A span is taken only within one line and
// without a '|': across a line, a block the renderer starts there (a
// heading, a list item) would end the paragraph and the span with it, and
// a '|' splits a table row into cells before any span is read. Either way
// the renderer would see no span, and what Body left alone as code would be
// clickable.
func codeSpan(s string, i, n int) (int, bool) {
	for j := i + n; j < len(s); {
		switch s[j] {
		case '\n', '|':
			return 0, false
		case '`':
			m := runLen(s, j, '`')
			if m == n {
				return j + m, true
			}
			j += m
			continue
		}
		j++
	}
	return 0, false
}

// closeBracket handles the ']' at c: the nearest open '[' becomes a link or
// an image if an inline destination, or a reference to a definition,
// follows; the link is stripped if its URL carries context.
func (in *inliner) closeBracket(c int) int {
	if len(in.openers) == 0 {
		return c + 1
	}
	o := in.openers[len(in.openers)-1]
	in.openers = in.openers[:len(in.openers)-1]
	s := in.s
	text := s[o.pos+1 : c]
	if c+1 < len(s) && s[c+1] == '(' {
		if dest, end, ok := inlineTail(s, c+1); ok {
			if carriesContext(dest) {
				in.strip(o, c, end, text)
				return end
			}
			// A link kept is read on inside its destination and title:
			// a renderer that parses it otherwise sees them as text.
			return c + 1
		}
	}
	if c+1 < len(s) && s[c+1] == '[' {
		if end, label, ok := linkLabel(s, c+1); ok {
			if strings.TrimSpace(label) == "" {
				label = text // [text][] names itself
			}
			if in.defs.defined(label) {
				if in.defs.stripped(label) {
					in.strip(o, c, end, text)
				}
				return end
			}
			return c + 1
		}
	}
	if in.defs.defined(text) {
		if in.defs.stripped(text) {
			in.strip(o, c, c+1, text)
		}
	}
	return c + 1
}

// strip removes the link or image from opener o to end, whose text ends at
// the ']' at c: a link keeps its text, an image its alt text or
// ImageRemoved.
func (in *inliner) strip(o opener, c, end int, text string) {
	start := o.pos
	if o.image {
		start--
		in.rep.ImagesRemoved++
		if strings.TrimSpace(text) == "" {
			in.edits = append(in.edits[:o.mark], edit{start, end, ImageRemoved})
			return
		}
	} else {
		in.rep.LinksRemoved++
	}
	in.edits = append(in.edits, edit{start, o.pos + 1, ""}, edit{c, end, ""})
}

// inlineTail reads an inline link's "(destination "title")" from the '('
// at p, as CommonMark does, and returns the destination as written and the
// end of the ')'.
func inlineTail(s string, p int) (dest string, end int, ok bool) {
	i := skipSpace(s, p+1)
	if i < len(s) && s[i] == '<' {
		j := i + 1
		for ; j < len(s) && s[j] != '>'; j++ {
			switch s[j] {
			case '\n', '<':
				return "", 0, false
			case '\\':
				j++
			}
		}
		if j >= len(s) {
			return "", 0, false
		}
		dest, i = s[i+1:j], j+1
	} else {
		j, depth := i, 0
	dest:
		for ; j < len(s); j++ {
			switch c := s[j]; {
			case c == '\\' && j+1 < len(s) && isASCIIPunct(s[j+1]):
				j++
			case c <= ' ' || c == 0x7f:
				break dest
			case c == '(':
				depth++
			case c == ')':
				if depth == 0 {
					break dest
				}
				depth--
			}
		}
		if depth != 0 {
			return "", 0, false
		}
		dest, i = s[i:j], j
	}
	k := skipSpace(s, i)
	if k > i && k < len(s) && (s[k] == '"' || s[k] == '\'' || s[k] == '(') {
		closer := s[k]
		if closer == '(' {
			closer = ')'
		}
		m := k + 1
		for ; m < len(s) && s[m] != closer; m++ {
			switch {
			case s[m] == '\\':
				m++
			case closer == ')' && s[m] == '(':
				return "", 0, false
			}
		}
		if m >= len(s) {
			return "", 0, false
		}
		k = skipSpace(s, m+1)
	}
	if k < len(s) && s[k] == ')' {
		return dest, k + 1, true
	}
	return "", 0, false
}

// skipSpace skips spaces and tabs, and at most one line ending among them.
func skipSpace(s string, i int) int {
	nl := false
	for ; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t':
		case '\n':
			if nl {
				return i
			}
			nl = true
		default:
			return i
		}
	}
	return i
}

// linkLabel reads a link label "[…]" from the '[' at p: no unescaped
// bracket inside, at most 999 characters. An empty label is ok: "[]".
func linkLabel(s string, p int) (end int, label string, ok bool) {
	for j := p + 1; j < len(s) && j-p <= 1000; j++ {
		switch s[j] {
		case '\\':
			j++
		case '[':
			return 0, "", false
		case ']':
			return j + 1, s[p+1 : j], true
		}
	}
	return 0, "", false
}

// apply makes the edits, which do not overlap, to s.
func apply(s string, edits []edit) string {
	if len(edits) == 0 {
		return s
	}
	sort.SliceStable(edits, func(a, b int) bool { return edits[a].start < edits[b].start })
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, e := range edits {
		if e.start < last {
			continue // cannot happen; never write text twice
		}
		b.WriteString(s[last:e.start])
		b.WriteString(e.repl)
		last = e.end
	}
	b.WriteString(s[last:])
	return b.String()
}

func isASCIIPunct(c byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\v' || c == '\r'
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
