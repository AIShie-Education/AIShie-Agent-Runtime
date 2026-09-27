package safety

import (
	"html"
	"regexp"
	"strings"
)

// The renderer runs with html off: a tag in an answer is shown as text, and
// a URL in it is a link only where linkify makes one, which Body judges as
// it judges any other. Body still takes out a tag whose URL carries data,
// as design §7 asks, so that the student is not shown markup built to
// exfiltrate, and so that nothing is left to follow should the renderer
// ever render HTML: <a href> leaves its text (its </a> goes too), <img>
// its alt text, and any other such tag nothing.

var (
	closeTagRe  = regexp.MustCompile(`^</([A-Za-z][A-Za-z0-9-]*)[ \t\n]*>`)
	tagNameRe   = regexp.MustCompile(`^<([A-Za-z][A-Za-z0-9-]*)`)
	attrNameRe  = regexp.MustCompile(`^[A-Za-z_:][A-Za-z0-9_.:-]*`)
	cssURLRe    = regexp.MustCompile(`(?i)url\(\s*(?:"([^"]*)"|'([^']*)'|([^)]*))\s*\)`)
	htmlSpaceRe = regexp.MustCompile(`[\t\n\r]`)
)

// urlAttrs are the HTML attributes that hold a URL a browser may fetch or
// follow.
var urlAttrs = map[string]bool{
	"href": true, "src": true, "srcset": true, "action": true, "formaction": true, "data": true,
	"poster": true, "background": true, "ping": true, "cite": true, "longdesc": true, "lowsrc": true,
	"dynsrc": true, "codebase": true, "archive": true, "manifest": true, "icon": true, "profile": true,
	"usemap": true, "classid": true, "xlink:href": true, "imagesrcset": true,
}

// What a tag taken out counts as in the Report.
const (
	countNone = iota
	countLink
	countImage
)

// htmlTags finds in the inline run t the tags to take out, but those that
// begin in code or TeX, and calls emit with each one's span, what replaces
// it, and what it counts as. openA counts the <a> tags taken out whose
// </a> is still to come.
func htmlTags(t string, code [][2]int, openA *int, emit func(start, end int, repl string, counts int)) {
	inCode := func(i int) bool {
		for _, c := range code {
			if i >= c[0] && i < c[1] {
				return true
			}
		}
		return false
	}
	for i := strings.IndexByte(t, '<'); i >= 0; {
		s := t[i:]
		n := 1
		if inCode(i) {
			// Nothing in code is changed.
		} else if m := closeTagRe.FindStringSubmatch(s); m != nil {
			if strings.EqualFold(m[1], "a") && *openA > 0 {
				*openA--
				emit(i, i+len(m[0]), "", countNone)
				n = len(m[0])
			}
		} else if name, attrs, length, ok := openTag(s); ok && attrsCarry(attrs) {
			n = length
			switch strings.ToLower(name) {
			case "a":
				*openA++
				emit(i, i+n, "", countLink)
			case "img", "image":
				alt := strings.TrimSpace(attrs["alt"])
				if alt == "" {
					alt = ImageRemoved
				}
				emit(i, i+n, alt, countImage)
			default:
				emit(i, i+n, "", countLink)
			}
		}
		j := strings.IndexByte(t[i+n:], '<')
		if j < 0 {
			return
		}
		i += n + j
	}
}

// openTag reads an HTML open tag at the start of s and returns its name,
// its attributes by lower-case name, and its length.
func openTag(s string) (name string, attrs map[string]string, n int, ok bool) {
	m := tagNameRe.FindStringSubmatch(s)
	if m == nil {
		return "", nil, 0, false
	}
	name, i := m[1], len(m[0])
	attrs = map[string]string{}
	for {
		j := skipHTMLSpace(s, i)
		if j < len(s) && s[j] == '>' {
			return name, attrs, j + 1, true
		}
		if j+1 < len(s) && s[j] == '/' && s[j+1] == '>' {
			return name, attrs, j + 2, true
		}
		if j == i {
			return "", nil, 0, false // an attribute must follow white space
		}
		an := attrNameRe.FindString(s[j:])
		if an == "" {
			return "", nil, 0, false
		}
		i = j + len(an)
		value := ""
		if k := skipHTMLSpace(s, i); k < len(s) && s[k] == '=' {
			k = skipHTMLSpace(s, k+1)
			if k >= len(s) {
				return "", nil, 0, false
			}
			switch q := s[k]; q {
			case '"', '\'':
				e := strings.IndexByte(s[k+1:], q)
				if e < 0 {
					return "", nil, 0, false
				}
				value, i = s[k+1:k+1+e], k+2+e
			default:
				e := k
				for e < len(s) && !isHTMLSpace(s[e]) && strings.IndexByte("\"'=<>`", s[e]) < 0 {
					e++
				}
				if e == k {
					return "", nil, 0, false
				}
				value, i = s[k:e], e
			}
		}
		key := strings.ToLower(an)
		if prev, dup := attrs[key]; dup {
			// Browsers take the first; both are judged.
			value = prev + " " + value
		}
		attrs[key] = value
	}
}

func isHTMLSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r' }

func skipHTMLSpace(s string, i int) int {
	for i < len(s) && isHTMLSpace(s[i]) {
		i++
	}
	return i
}

// attrsCarry reports whether a URL in a tag's attributes carries data: a
// URL attribute, each candidate of a srcset, each url() in a style. A
// value is judged as written and as a browser would read it, its entities
// decoded and its tabs and line breaks dropped.
func attrsCarry(attrs map[string]string) bool {
	judge := func(v string) bool { return carries(v) || carries(attrURL(v)) }
	for k, v := range attrs {
		switch {
		case k == "srcset" || k == "imagesrcset":
			for _, cand := range strings.Split(attrURL(v), ",") {
				if f := strings.Fields(cand); len(f) > 0 && judge(f[0]) {
					return true
				}
			}
		case urlAttrs[k]:
			if judge(v) {
				return true
			}
		case k == "style":
			for _, m := range cssURLRe.FindAllStringSubmatch(attrURL(v), -1) {
				if judge(m[1] + m[2] + m[3]) {
					return true
				}
			}
		}
	}
	return false
}

// attrURL is an attribute's value as a browser reads it as a URL.
func attrURL(v string) string {
	return strings.TrimFunc(htmlSpaceRe.ReplaceAllString(html.UnescapeString(v), ""), func(r rune) bool { return r <= ' ' })
}
