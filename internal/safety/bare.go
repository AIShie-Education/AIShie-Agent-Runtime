package safety

import "strings"

// bare reads a URL written as plain text at i, which a renderer with GitHub's
// extension or a linkifier makes a link: one beginning http://, https://,
// ftp://, mailto: or xmpp:, www., or // (a linkifier's protocol-relative
// link), or a domain name followed by a path, query or fragment. It runs, as
// GitHub's renderer reads it, to the next white space or '<', over any code
// span or bracket in between (but not past a ']' closing a bracket still
// open), less the trailing punctuation the renderer leaves out. One that
// carries context becomes LinkRemoved, and bare reports true with its end;
// otherwise it reports false with where reading goes on: after the start it
// recognised, or at i+1.
func (in *inliner) bare(i int) (int, bool) {
	s := in.s
	prefix, skip, judge := urlStart(s, i)
	if !judge {
		return i + max(skip, 1), false
	}
	end := i
	for end < len(s) && !isSpace(s[end]) && s[end] != '<' && (s[end] != ']' || len(in.openers) == 0) {
		end++
	}
	end = trimTrail(s, i, end)
	// Nothing legitimate is as long as maxURLBytes, and length is room for
	// data; judging only what is shorter also keeps a long run of URL
	// starts from being read once for each.
	if end-i > maxURLBytes || end > i+skip && carriesContext(prefix+s[i:end]) {
		in.rep.LinksRemoved++
		in.edits = append(in.edits, edit{i, end, LinkRemoved})
		return end, true
	}
	return i + skip, false
}

// maxURLBytes is the longest bare URL judged on its merits; a longer one
// is taken to carry context.
const maxURLBytes = 2048

var schemeStarts = []string{"https://", "http://", "ftp://", "mailto:", "xmpp:"}

// urlStart reports whether a bare URL begins at i, with the prefix that
// makes it absolute and the length of the start it recognised. A domain
// name with no path, query or fragment after it is no link to judge, but
// its length is reported so that it is not read again.
func urlStart(s string, i int) (prefix string, skip int, judge bool) {
	rest := s[i:]
	for _, st := range schemeStarts {
		if len(rest) >= len(st) && strings.EqualFold(rest[:len(st)], st) {
			return "", len(st), true
		}
	}
	prev := byte(' ')
	if i > 0 {
		prev = s[i-1]
	}
	if len(rest) >= 4 && strings.EqualFold(rest[:4], "www.") && !isAlnum(prev) {
		return "http://", 4, true
	}
	if strings.HasPrefix(rest, "//") && len(rest) > 2 && isAlnum(rest[2]) && strings.IndexByte(" \t\n([{<\"'", prev) >= 0 {
		return "https:", 2, true
	}
	// A domain follows anything but a letter, digit, '.', '-' or '@'. An
	// '_' or '*' before it may be emphasis, which leaves the domain at the
	// start of a text of its own, where a linkifier finds it.
	if isAlnum(s[i]) && !isAlnum(prev) && strings.IndexByte(".-@", prev) < 0 {
		if n := domainLen(rest); n > 0 {
			if n < len(rest) && (strings.IndexByte("/?#", rest[n]) >= 0 || rest[n] == ':' && n+1 < len(rest) && rest[n+1] >= '0' && rest[n+1] <= '9') {
				return "http://", n, true
			}
			return "", n, false
		}
	}
	return "", 0, false
}

// domainLen is the length of the domain name at the start of s: labels of
// letters, digits and '-', at least two, the last of two or more letters.
func domainLen(s string) int {
	labels, i, lastStart := 0, 0, 0
	for {
		j := i
		for j < len(s) && (isAlnum(s[j]) || s[j] == '-') {
			j++
		}
		if j == i {
			break
		}
		labels++
		lastStart = i
		if j < len(s) && s[j] == '.' && j+1 < len(s) && isAlnum(s[j+1]) {
			i = j + 1
			continue
		}
		i = j
		break
	}
	if labels < 2 || i-lastStart < 2 {
		return 0
	}
	for _, c := range []byte(s[lastStart:i]) {
		if !isAlpha(c) {
			return 0
		}
	}
	return i
}

// trimTrail leaves out of s[start:end] the trailing characters GitHub's
// renderer does not count as part of a bare URL: punctuation, a ')' with
// no '(' to match, and an entity reference or ';'; and an Ellipsis.
func trimTrail(s string, start, end int) int {
	for end > start {
		// The ellipsis a cut adds is not part of the URL it may follow.
		if strings.HasSuffix(s[start:end], Ellipsis) {
			end -= len(Ellipsis)
			continue
		}
		switch c := s[end-1]; {
		case strings.IndexByte("?!.,:*_~'\"", c) >= 0:
			end--
		case c == ')':
			if strings.Count(s[start:end], ")") <= strings.Count(s[start:end], "(") {
				return end
			}
			end--
		case c == ';':
			j := end - 2
			for j >= start && isAlnum(s[j]) {
				j--
			}
			if j >= start && j < end-2 && s[j] == '&' {
				end = j
			} else {
				end--
			}
		default:
			return end
		}
	}
	return end
}

// email reads an email address around the '@' at i, which renderers make a
// mailto: link, and strips it if the address spells out data.
func (in *inliner) email(i int) (int, bool) {
	s := in.s
	start := i
	for start > in.textFrom && (isAlnum(s[start-1]) || strings.IndexByte("._+-", s[start-1]) >= 0) {
		start--
	}
	end := i + 1
	for end < len(s) && (isAlnum(s[end]) || strings.IndexByte("._-", s[end]) >= 0) {
		end++
	}
	for end > i+1 && strings.IndexByte("._-", s[end-1]) >= 0 {
		end--
	}
	if start == i || !strings.Contains(s[i+1:end], ".") {
		return 0, false
	}
	if !carriesContext("mailto:" + s[start:end]) {
		return end, true
	}
	in.rep.LinksRemoved++
	in.edits = append(in.edits, edit{start, end, LinkRemoved})
	return end, true
}
