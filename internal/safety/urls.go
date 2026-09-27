package safety

import (
	"html"
	"net/url"
	"strings"
)

// dataRun is how many characters of [A-Za-z0-9+/=_-] in a row make a path
// segment, host label or address look like data spelled out (design §7).
const dataRun = 32

// carriesContext reports whether a link to raw could take data somewhere:
// raw as written, and as a browser would read it once Markdown's escapes
// and HTML's entities are undone and the tabs and newlines it drops are
// gone. Either reading carrying context is enough.
func carriesContext(raw string) bool {
	return carries(raw, false) || carries(normalizeURL(raw), true)
}

// normalizeURL is raw as a browser would follow it.
func normalizeURL(raw string) string {
	s := html.UnescapeString(unescapeMarkdown(raw))
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	return strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
}

// unescapeMarkdown removes the backslashes that escape ASCII punctuation.
func unescapeMarkdown(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && isASCIIPunct(s[i+1]) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func carries(s string, normalized bool) bool {
	// A query, a fragment, percent-encoding: context, wherever they are.
	if strings.ContainsAny(s, "?#%") {
		return true
	}
	// A backslash left once escapes are undone is a '/' to a browser, and
	// can move the host: never a link that carries nothing.
	if normalized && strings.Contains(s, `\`) {
		return true
	}
	scheme, rest, ok := splitScheme(s)
	if !ok {
		if strings.HasPrefix(s, "//") {
			return webCarries("https:" + s)
		}
		return pathCarries(s)
	}
	switch strings.ToLower(scheme) {
	case "http", "https":
		return webCarries(s)
	case "mailto":
		return mailtoCarries(rest)
	}
	return true
}

// splitScheme splits a URL's scheme from the rest, if it has one: letters,
// digits, '+', '.' and '-', beginning with a letter, before a ':' that
// comes before any '/'.
func splitScheme(s string) (scheme, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isAlpha(c):
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '.' || c == '-'):
		case c == ':' && i > 0:
			return s[:i], s[i+1:], true
		default:
			return "", s, false
		}
	}
	return "", s, false
}

// webCarries judges an http or https URL: user information, an opaque
// form (http:host), or a path segment or host label that spells out data.
func webCarries(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.User != nil {
		return true
	}
	for _, label := range strings.Split(u.Hostname(), ".") {
		if longestDataRun(label) >= dataRun {
			return true
		}
	}
	return pathCarries(u.Path)
}

func pathCarries(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if longestDataRun(seg) >= dataRun {
			return true
		}
	}
	return false
}

// mailtoCarries judges a mailto: URL's addresses, whose parts may spell
// out data as a path does.
func mailtoCarries(addrs string) bool {
	for _, part := range strings.FieldsFunc(addrs, func(r rune) bool { return r == ',' || r == '@' || r == '.' || r == ';' }) {
		if longestDataRun(part) >= dataRun {
			return true
		}
	}
	return false
}

// longestDataRun is the longest run of [A-Za-z0-9+/=_-] in s.
func longestDataRun(s string) int {
	longest, run := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isAlnum(c) || c == '+' || c == '/' || c == '=' || c == '_' || c == '-' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}
