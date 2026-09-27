package safety

import (
	"strings"
	"unicode/utf8"
)

// dataRun is how many characters of [A-Za-z0-9+/=_-] in a row make a path
// segment, host label or address look like data spelled out (design §7).
const dataRun = 32

// maxURLBytes is the longest URL judged on its merits: nothing a student
// needs is as long, and length is room for data.
const maxURLBytes = 2048

// safeURLChars are the characters besides letters and digits that
// markdown-it's normalizeLink leaves as they are (mdurl's encode). It
// percent-encodes every other one, and every character that is not ASCII
// but in a host, which it turns into punycode.
const safeURLChars = ";/?:@&=+$,-_.!~*'()#"

// carries reports whether a link to u could take data to whoever owns it
// (design §7). u is the URL as markdown-it reads it: a destination with its
// escapes and entities undone, or a URL as written in the text. It is
// judged as the href markdown-it's normalizeLink makes of it, which
// percent-encodes every character that is not ASCII and those of "<>[\]^`{|}
// and the space, and turns a host that is not ASCII into punycode. It
// carries data when that href would have a query or a fragment, any
// percent-encoding, user information, a scheme other than http, https or
// mailto, a host label (in punycode) or path segment with dataRun
// characters of data, or, for mailto, an address part that does; or when
// it is longer than maxURLBytes.
func carries(u string) bool {
	if len(u) > maxURLBytes {
		return true
	}
	scheme, rest, ok := splitScheme(u)
	if !ok {
		if strings.HasPrefix(u, "//") && !strings.HasPrefix(u, "///") {
			return webCarries(u[2:])
		}
		return unsafe(u, false) || pathCarries(u)
	}
	switch strings.ToLower(scheme) {
	case "http", "https":
		if strings.HasPrefix(rest, "//") && !strings.HasPrefix(rest, "///") {
			return webCarries(rest[2:])
		}
		// Without "//" the renderer encodes it all as a path, which a
		// browser then reads as a host: it is judged as both.
		return unsafe(rest, false) || webCarries(strings.TrimLeft(rest, "/"))
	case "mailto":
		return mailtoCarries(rest)
	}
	return true
}

// unsafe reports whether s holds '?', '#', '%', or a character markdown-it
// would percent-encode; with host, characters that are not ASCII are let
// through, since a host is put into punycode instead.
func unsafe(s string, host bool) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf && host {
			continue
		}
		if c == '?' || c == '#' || c == '%' || !isAlnum(c) && strings.IndexByte(safeURLChars, c) < 0 {
			return true
		}
	}
	return false
}

// splitScheme splits a URL's scheme from the rest, if it has one: a letter,
// then letters, digits, '+', '.' and '-', before a ':'.
func splitScheme(s string) (scheme, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isASCIIAlpha(c):
		case i > 0 && (isDigit(c) || c == '+' || c == '.' || c == '-'):
		case c == ':' && i > 0:
			return s[:i], s[i+1:], true
		default:
			return "", s, false
		}
	}
	return "", s, false
}

// webCarries judges what follows "//": an authority, which ends at the next
// '/', '?', '#' or '\', then the rest.
func webCarries(s string) bool {
	auth, tail := s, ""
	if i := strings.IndexAny(s, "/?#\\"); i >= 0 {
		auth, tail = s[:i], s[i:]
	}
	if strings.Contains(auth, "@") || unsafe(tail, false) {
		return true
	}
	host := auth
	if strings.HasPrefix(host, "[") {
		// An IPv6 address, which the renderer leaves as it is.
		if j := strings.IndexByte(host, ']'); j > 0 && strings.Trim(host[1:j], "0123456789abcdefABCDEF:.") == "" {
			host = host[j+1:]
		}
	}
	if unsafe(host, true) {
		return true
	}
	for _, label := range strings.FieldsFunc(punycodeHost(host), func(r rune) bool { return r == '.' || r == ':' }) {
		if longestDataRun(label) >= dataRun {
			return true
		}
	}
	return pathCarries(tail)
}

func pathCarries(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if longestDataRun(seg) >= dataRun {
			return true
		}
	}
	return false
}

// mailtoCarries judges a mailto: URL: its domain may be in punycode, and
// no part of an address may spell out data.
func mailtoCarries(rest string) bool {
	addr, tail := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		addr, tail = rest[:i], rest[i:]
	}
	name, host := addr, ""
	if at := strings.LastIndexByte(addr, '@'); at >= 0 {
		name, host = addr[:at], addr[at+1:]
	}
	if unsafe(name, false) || unsafe(host, true) || unsafe(tail, false) {
		return true
	}
	for _, part := range strings.FieldsFunc(name+"@"+punycodeHost(host)+tail, func(r rune) bool {
		return r == ',' || r == '@' || r == '.' || r == ';' || r == '/'
	}) {
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
