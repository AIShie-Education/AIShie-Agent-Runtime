package safety

import (
	"regexp"
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
// judged as the href markdown-it's normalizeLink makes of it, which parses
// it as mdurl does, percent-encodes every character that is not ASCII and
// those of "<>[\]^`{|} and the space, and puts a host that is not ASCII
// into punycode. It carries data when that href would have a query or a
// fragment, any percent-encoding, user information, a scheme other than
// http, https or mailto, a host label (in punycode) or path segment with
// dataRun characters of data, or, for mailto, a name that does; or when it
// is longer than maxURLBytes.
func carries(u string) bool {
	if len(u) > maxURLBytes {
		return true
	}
	p := parseMDURL(u)
	if p.proto != "" && !isASCIIAlpha(p.proto[0]) {
		// No scheme to a browser, which reads it all as a relative path.
		return unsafe(u, false) || pathCarries(u)
	}
	proto := strings.ToLower(p.proto)
	switch proto {
	case "", "http:", "https:", "mailto:":
	default:
		return true
	}
	if p.hasAuth && proto != "mailto:" || unsafe(p.auth, false) || unsafe(p.rest, false) {
		return true
	}
	// normalizeLink puts a host into punycode only after these, written so;
	// any other host that is not ASCII is percent-encoded.
	recode := p.proto == "" || p.proto == "http:" || p.proto == "https:" || p.proto == "mailto:"
	if len(p.hostname) > maxHostBytes || unsafe(p.hostname, recode) {
		return true
	}
	if proto == "mailto:" {
		// An address is judged whole: '/' does not break its runs.
		addr := punycodeHost(p.hostname) + p.rest
		if p.hasAuth {
			addr = p.auth + "@" + addr
		}
		if p.slashes {
			addr = "//" + addr
		}
		for _, part := range strings.FieldsFunc(addr, func(r rune) bool { return r == ',' || r == '@' || r == '.' || r == ';' }) {
			if longestDataRun(part) >= dataRun {
				return true
			}
		}
	}
	if proto == "http:" || proto == "https:" || proto == "" && p.slashes {
		// A browser reads the host up to the next '/', '?' or '#': what
		// mdurl split off the host joins it again, and with no host read
		// at all (http:u@host, ///u@host) it takes one after any further
		// slashes, user information and all.
		rest := p.rest
		if p.hostname == "" {
			rest = strings.TrimLeft(rest, "/")
		}
		tail := rest
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			tail, rest = rest[:i], rest[i:]
		} else {
			rest = ""
		}
		if strings.Contains(tail, "@") {
			return true
		}
		for _, label := range strings.FieldsFunc(punycodeHost(p.hostname)+tail, func(r rune) bool { return r == '.' || r == ':' }) {
			if longestDataRun(label) >= dataRun {
				return true
			}
		}
		return pathCarries(rest)
	}
	for _, label := range strings.Split(punycodeHost(p.hostname), ".") {
		if longestDataRun(label) >= dataRun {
			return true
		}
	}
	return pathCarries(p.rest)
}

// maxHostBytes is mdurl's longest host; a longer one it drops, and a
// browser then reads the path as the host.
const maxHostBytes = 255

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

// mdURL is a URL as mdurl's parse (url, true) splits it, which is how
// markdown-it's normalizeLink reads a link.
type mdURL struct {
	proto    string // with its ':', as written; "" for none
	slashes  bool
	hasAuth  bool
	auth     string
	hostname string // without its port or an IPv6 address's brackets
	rest     string // the path, query and fragment
}

var (
	mdProtocolRe = regexp.MustCompile(`(?i)^[a-z0-9.+-]+:`)
	mdPortRe     = regexp.MustCompile(`:[0-9]*$`)
	mdHostPartRe = regexp.MustCompile(`^[+a-z0-9A-Z_-]{0,63}$`)
)

// mdNonHostChars end a host.
const mdNonHostChars = "%/?;#'{}|\\^`<>\" \r\n\t"

// isMDSlashed are mdurl's slashedProtocol, as written.
func isMDSlashed(proto string) bool {
	switch proto {
	case "http:", "https:", "ftp:", "gopher:", "file:":
		return true
	}
	return false
}

func parseMDURL(url string) mdURL {
	var p mdURL
	rest := jsTrim(url)
	if m := mdProtocolRe.FindString(rest); m != "" {
		p.proto, rest = m, rest[len(m):]
	}
	hostless := p.proto == "javascript:"
	slashes := strings.HasPrefix(rest, "//")
	if slashes && !hostless {
		rest, p.slashes = rest[2:], true
	}
	if hostless || !slashes && (p.proto == "" || isMDSlashed(p.proto)) {
		p.rest = rest
		return p
	}
	hostEnd := strings.IndexAny(rest, "/?#")
	atSign := strings.LastIndexByte(rest, '@')
	if hostEnd >= 0 {
		atSign = strings.LastIndexByte(rest[:hostEnd], '@')
	}
	if atSign >= 0 {
		p.hasAuth, p.auth, rest = true, rest[:atSign], rest[atSign+1:]
	}
	hostEnd = strings.IndexAny(rest, mdNonHostChars)
	if hostEnd < 0 {
		hostEnd = len(rest)
	}
	if hostEnd > 0 && rest[hostEnd-1] == ':' {
		hostEnd--
	}
	host := rest[:hostEnd]
	rest = rest[hostEnd:]
	host = strings.TrimSuffix(host, mdPortRe.FindString(host))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		p.hostname, p.rest = host[1:len(host)-1], rest
		return p
	}
	// A label with a character but letters, digits, '+', '_', '-' and
	// characters that are not ASCII ends the host there; the rest of it is
	// read as the start of the path.
	parts := strings.Split(host, ".")
	for i, part := range parts {
		if part == "" || mdHostPartRe.MatchString(asciiStandIns(part)) {
			continue
		}
		valid := len(part) - len(strings.TrimLeftFunc(part, isHostPartChar))
		valid = min(valid, 63)
		notHost := append([]string{part[valid:]}, parts[i+1:]...)
		rest = strings.Join(notHost, ".") + rest
		host = strings.Join(append(parts[:i:i], part[:valid]), ".")
		break
	}
	p.hostname, p.rest = host, rest
	return p
}

// asciiStandIns is part with each UTF-16 code unit that is not ASCII
// replaced by 'x', as mdurl tests a host label.
func asciiStandIns(part string) string {
	var b strings.Builder
	for _, r := range part {
		switch {
		case r >= 0x10000:
			b.WriteString("xx")
		case r >= utf8.RuneSelf:
			b.WriteByte('x')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isHostPartChar(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '+' || r == '_' || r == '-'
}

func pathCarries(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if longestDataRun(seg) >= dataRun {
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
