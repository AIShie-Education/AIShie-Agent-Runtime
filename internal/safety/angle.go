package safety

import (
	"regexp"
	"strings"
)

var (
	// CommonMark's autolinks: <scheme:…> and <address@host>.
	autolinkRe      = regexp.MustCompile(`^<([A-Za-z][A-Za-z0-9+.-]{1,31}:[^\x00-\x20<>]*)>`)
	looseAutolinkRe = regexp.MustCompile(`^<[A-Za-z][A-Za-z0-9+.-]{1,31}:[^\n<>]*>`)
	emailAutolinkRe = regexp.MustCompile(`^<([a-zA-Z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*)>`)
	closeTagRe      = regexp.MustCompile(`^</([A-Za-z][A-Za-z0-9-]*)[ \t\n]*>`)
	tagNameRe       = regexp.MustCompile(`^<([A-Za-z][A-Za-z0-9-]*)`)
	attrNameRe      = regexp.MustCompile(`^[A-Za-z_:][A-Za-z0-9_.:-]*`)
	cssURLRe        = regexp.MustCompile(`(?i)url\(\s*(?:"([^"]*)"|'([^']*)'|([^)]*))\s*\)`)
)

// urlAttrs are the HTML attributes that hold a URL a browser may fetch or
// follow.
var urlAttrs = map[string]bool{
	"href": true, "src": true, "srcset": true, "action": true, "formaction": true, "data": true,
	"poster": true, "background": true, "ping": true, "cite": true, "longdesc": true, "lowsrc": true,
	"dynsrc": true, "codebase": true, "archive": true, "manifest": true, "icon": true, "profile": true,
	"usemap": true, "classid": true, "xlink:href": true,
}

// angle reads what begins with the '<' at i: an autolink, which is stripped
// if its URL carries context, or an HTML tag, comment, processing
// instruction, declaration or CDATA section. A tag with a URL that carries
// context is removed: <a> keeps its text (its </a> goes too), <img> becomes
// its alt text. It reports false when nothing begins at i, and for a
// construct that spans lines or holds a '|', whose inside is then read as
// text: a block starting on a later line, or a table cell, could cut it
// short, leaving what it seemed to hide as live text.
func (in *inliner) angle(i int) (int, bool) {
	s := in.s[i:]
	if m := autolinkRe.FindStringSubmatch(s); m != nil {
		return in.autolink(i, len(m[0]), m[1]), true
	}
	if m := emailAutolinkRe.FindStringSubmatch(s); m != nil {
		return in.autolink(i, len(m[0]), "mailto:"+m[1]), true
	}
	// <scheme:… > with a space or another character CommonMark refuses in
	// an autolink is no autolink to CommonMark, but a laxer renderer may
	// take it for one: its URL is judged all the same.
	if loc := looseAutolinkRe.FindStringIndex(s); loc != nil && carriesContext(s[1:loc[1]-1]) {
		return in.autolink(i, loc[1], s[1:loc[1]-1]), true
	}
	if m := closeTagRe.FindStringSubmatch(s); m != nil {
		if strings.EqualFold(m[1], "a") && in.openA > 0 {
			in.openA--
			in.edits = append(in.edits, edit{i, i + len(m[0]), ""})
			return i + len(m[0]), true
		}
		return in.opaque(i, len(m[0]))
	}
	for _, d := range [...]struct{ open, close string }{
		{"<!---->", ""}, {"<!-->", ""}, {"<!--", "-->"}, {"<?", "?>"}, {"<![CDATA[", "]]>"},
	} {
		if !strings.HasPrefix(s, d.open) {
			continue
		}
		if d.close == "" {
			return in.opaque(i, len(d.open))
		}
		if j := strings.Index(s[len(d.open):], d.close); j >= 0 {
			return in.opaque(i, len(d.open)+j+len(d.close))
		}
		return 0, false
	}
	if len(s) > 2 && s[1] == '!' && isAlpha(s[2]) {
		if j := strings.IndexByte(s, '>'); j >= 0 {
			return in.opaque(i, j+1)
		}
		return 0, false
	}
	name, attrs, n, ok := openTag(s)
	if !ok {
		return 0, false
	}
	if !attrsCarry(attrs) {
		return in.opaque(i, n)
	}
	switch strings.ToLower(name) {
	case "a":
		in.openA++
		in.rep.LinksRemoved++
		in.edits = append(in.edits, edit{i, i + n, ""})
	case "img", "image":
		in.rep.ImagesRemoved++
		alt := strings.TrimSpace(attrs["alt"])
		if alt == "" {
			alt = ImageRemoved
		}
		in.edits = append(in.edits, edit{i, i + n, alt})
	default:
		in.rep.LinksRemoved++
		in.edits = append(in.edits, edit{i, i + n, ""})
	}
	return i + n, true
}

// autolink judges an autolink of n bytes at i to url.
func (in *inliner) autolink(i, n int, url string) int {
	if carriesContext(url) {
		in.rep.LinksRemoved++
		in.edits = append(in.edits, edit{i, i + n, LinkRemoved})
	}
	return i + n
}

// opaque is a construct of n bytes at i that is left as it is: skipped when
// it is on one line and holds no '|', else read on from just after its '<'.
func (in *inliner) opaque(i, n int) (int, bool) {
	if strings.ContainsAny(in.s[i:i+n], "\n|") {
		return 0, false
	}
	return i + n, true
}

// openTag reads an HTML open tag at the start of s, as CommonMark defines
// one, and returns its name, its attributes by lower-case name, and its
// length.
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
				for e < len(s) && !isSpace(s[e]) && strings.IndexByte("\"'=<>`", s[e]) < 0 {
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

func skipHTMLSpace(s string, i int) int {
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	return i
}

// attrsCarry reports whether any URL in a tag's attributes carries
// context: a URL attribute, each candidate of a srcset, and each url() in
// a style.
func attrsCarry(attrs map[string]string) bool {
	for k, v := range attrs {
		switch {
		case k == "srcset" || k == "imagesrcset":
			for _, cand := range strings.Split(v, ",") {
				if f := strings.Fields(cand); len(f) > 0 && carriesContext(f[0]) {
					return true
				}
			}
		case urlAttrs[k]:
			if carriesContext(v) {
				return true
			}
		case k == "style":
			for _, m := range cssURLRe.FindAllStringSubmatch(normalizeURL(v), -1) {
				if carriesContext(m[1] + m[2] + m[3]) {
					return true
				}
			}
		}
	}
	return false
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
