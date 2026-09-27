package safety

import (
	"net"
	"strings"
	"unicode/utf8"
)

// This file is linkify-it 6.1.0 as markdown-it 15 uses it: its default
// options, so the schemas http:, https:, ftp:, // and mailto:, and email
// addresses without a scheme ("fuzzy" emails), but no bare domains
// (fuzzyLink is off) and no user information in a URL (urlAuth is off).
// Its patterns are regular expressions with look-arounds, written out here
// as the matching they do, alternative by alternative, so that a URL ends
// exactly where linkify-it ends it.

// linkifyMatchAtStart is linkify-it's matchAtStart for a text that begins
// with an ASCII letter, as markdown-it's inline linkify rule calls it: the
// length of the link a schema at its start makes, or 0.
func linkifyMatchAtStart(t string) int {
	for _, sc := range [...]string{"http:", "https:", "ftp:", "mailto:"} {
		if !hasPrefixFold(t, sc) {
			continue
		}
		l := &linker{t: t}
		var n int
		if sc == "mailto:" {
			n = l.mailtoValidator(len(sc))
		} else {
			n = l.httpValidator(len(sc))
		}
		if n == 0 {
			return 0
		}
		return len(sc) + n
	}
	return 0
}

// linker matches linkify-it's patterns in t. Loose, a host may also end
// at a delimiter (see linkifyCandidates). memo remembers where src_path's
// repetitions starting at a position end, which depends on nothing else,
// so that many URL starts in one text cost no more than one.
type linker struct {
	t     string
	loose bool
	memo  []int
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// linkifyCandidates calls emit for every link linkify-it could make in the
// text t of one inline run's pieces, and for some it could not: Body takes
// the reading in which more is a link. markdown-it runs linkify over each
// text token, and a matched emphasis or strikethrough delimiter ends one;
// t joins them. So a schema, a "//" or an address is taken wherever a
// token could begin, whatever precedes it, and a host may end at a
// delimiter as it would at the end of a token (loose); the rest of each
// match, and so its end, is linkify-it's.
//
// afterSpecial says t follows an escape or an entity: linkify then drops a
// link that begins the text token, and so is a link at 0 dropped here.
func linkifyCandidates(t string, afterSpecial bool, emit func(start, end int, url string)) {
	if afterSpecial {
		inner := emit
		emit = func(start, end int, url string) {
			if start > 0 {
				inner(start, end, url)
			}
		}
	}
	l := &linker{t: t, loose: true, memo: make([]int, len(t)+1)}
	for i := range l.memo {
		l.memo[i] = -1
	}
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case 'h', 'H', 'f', 'F', 'm', 'M':
			// markdown-it's inline rule takes a scheme after anything but a
			// scheme character, linkify-it's after punctuation or a space.
			if i > 0 && (isAlnum(t[i-1]) || t[i-1] == '+') {
				continue
			}
			for _, sc := range [...]string{"http:", "https:", "ftp:", "mailto:"} {
				if !hasPrefixFold(t[i:], sc) {
					continue
				}
				var n int
				if sc == "mailto:" {
					n = l.mailtoValidator(i + len(sc))
				} else {
					n = l.httpValidator(i + len(sc))
				}
				if n > 0 {
					end := i + len(sc) + n
					emit(i, end, t[i:end])
				}
			}
		case '/':
			if i+1 >= len(t) || t[i+1] != '/' {
				continue
			}
			if i > 0 {
				// linkify-it refuses "//" after ':' or '/', and takes it
				// only after punctuation or a space, or where a token
				// begins (after a delimiter).
				if p, _ := utf8.DecodeLastRuneInString(t[:i]); p == ':' || p == '/' || isPseudoLetter(p) && !strikeEnds(t, i) {
					continue
				}
			}
			if n := l.relativeValidator(i + 2); n > 0 {
				end := i + 2 + n
				emit(i, end, t[i:end])
			}
			i++
		case '@':
			end, ok := l.fuzzyMailHost(i + 1)
			if !ok {
				continue
			}
			if start := mailNameStart(t, i); start < i {
				emit(start, end, "mailto:"+t[start:end])
			}
		}
	}
}

// linkifyTest is linkify-it's test, which markdown-it's linkify runs over
// a whole inline run before it looks for links in the run's text tokens:
// if it fails, the run has none. It reports whether a schema that follows
// the start, a separator, punctuation (not '_'), a control character, '<',
// '>' or '｜' begins a link, or an address follows its name as linkify-it
// takes one. It is true in some runs where linkify-it's is not (it looks at
// every schema, and at names of any length), never the other way. The run
// is read whole, with its delimiters, as linkify-it reads it.
func linkifyTest(t string) bool {
	l := &linker{t: t}
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch c {
		case 'h', 'H', 'f', 'F', 'm', 'M', '/':
		case '@':
			if _, ok := l.fuzzyMailHost(i + 1); !ok {
				continue
			}
			for s := mailNameStart(t, i); s < i; s++ {
				if t[s] == '.' {
					continue
				}
				if p, _ := utf8.DecodeLastRuneInString(t[:s]); s == 0 || isMailBoundary(p) {
					return true
				}
			}
			continue
		default:
			continue
		}
		if i > 0 {
			if p, _ := utf8.DecodeLastRuneInString(t[:i]); p == '_' || isPseudoLetter(p) {
				continue
			}
		}
		for _, sc := range [...]string{"http:", "https:", "ftp:", "//", "mailto:"} {
			if !hasPrefixFold(t[i:], sc) {
				continue
			}
			var n int
			switch sc {
			case "mailto:":
				n = l.mailtoValidator(i + len(sc))
			case "//":
				if i == 0 || t[i-1] != ':' && t[i-1] != '/' {
					n = l.relativeValidator(i + 2)
				}
			default:
				n = l.httpValidator(i + len(sc))
			}
			if n > 0 {
				return true
			}
		}
	}
	return false
}

// httpValidator matches "//", a host and port, and a path at pos: the
// length matched, or 0.
func (l *linker) httpValidator(pos int) int {
	if !strings.HasPrefix(l.t[pos:], "//") {
		return 0
	}
	end, ok := l.urlHostPort(pos + 2)
	if !ok {
		return 0
	}
	return l.linkPath(end) - pos
}

// relativeValidator matches what follows "//" at pos: localhost, an IPv6
// address, or a domain of two or more labels; a port; a path.
func (l *linker) relativeValidator(pos int) int {
	end, ok := l.relativeHostPort(pos)
	if !ok {
		return 0
	}
	return l.linkPath(end) - pos
}

// mailtoValidator matches an address at pos: a name, '@', a host.
func (l *linker) mailtoValidator(pos int) int {
	n := mailNameLen(l.t, pos)
	if n == 0 || pos+n >= len(l.t) || l.t[pos+n] != '@' {
		return 0
	}
	end, ok := l.mailHost(pos + n + 1)
	if !ok {
		return 0
	}
	return end - pos
}

func isMailNameChar(c byte) bool {
	return isAlnum(c) || strings.IndexByte("-!#$%&'*+/=?^_`{|}~", c) >= 0
}

// mailNameLen is the length of linkify-it's src_mail_name at pos: a name
// character, then up to 63 more or dots each followed by one. A longer
// run is no name, since what follows it is not '@'.
func mailNameLen(t string, pos int) int {
	if pos >= len(t) || !isMailNameChar(t[pos]) {
		return 0
	}
	i := pos + 1
	for i < len(t) && i-pos < 64 {
		if isMailNameChar(t[i]) || t[i] == '.' && i+1 < len(t) && isMailNameChar(t[i+1]) {
			i++
			continue
		}
		break
	}
	return i - pos
}

// mailNameStart is where the name of an address whose '@' is at at may
// begin, or at if nowhere. linkify-it's name is the run of name characters
// before the '@', at most 64 long, after a space, a control character,
// '<', '>', '｜', '"' or '(', or at the start of a text token. The start
// taken here is the earliest in the run that follows one of those, or a
// delimiter (after which a token may begin), whatever the length.
func mailNameStart(t string, at int) int {
	s := at
	for s > 0 {
		c := t[s-1]
		if isMailNameChar(c) || c == '.' && s < at && isMailNameChar(t[s]) {
			s--
			continue
		}
		break
	}
	for ; s < at; s++ {
		if t[s] == '.' {
			continue
		}
		if s == 0 {
			return s
		}
		if p, _ := utf8.DecodeLastRuneInString(t[:s]); isMailBoundary(p) || p == '*' || p == '_' || strikeEnds(t, s) {
			return s
		}
	}
	return at
}

// strikeEnds reports whether a run of two or more '~', a strikethrough's
// delimiter, ends at i: a single '~' is none.
func strikeEnds(t string, i int) bool { return i >= 2 && t[i-1] == '~' && t[i-2] == '~' }

func isMailBoundary(r rune) bool {
	return r == '<' || r == '>' || r == '｜' || r == '"' || r == '(' || isZCc(r)
}

// urlHostPort is linkify-it's url_host_port: an IPv6 address in brackets,
// or up to 11 domain labels; a port; the host terminator.
func (l *linker) urlHostPort(p int) (int, bool) {
	t := l.t
	if end, ok := ipv6Host(t, p, "["); ok {
		if e, ok := l.portTerminator(end); ok {
			return e, true
		}
	}
	final := func(q int) (int, bool) {
		for _, e := range domainEnds(t, q) {
			if r, ok := l.portTerminator(e); ok {
				return r, true
			}
		}
		return 0, false
	}
	return domains(t, p, 0, 0, 10, final)
}

// relativeHostPort is the host of a "//" link: localhost, an IPv6 address
// in brackets, or two to eleven labels, the last a domain root; a port; the
// host terminator.
func (l *linker) relativeHostPort(p int) (int, bool) {
	t := l.t
	if hasPrefixFold(t[p:], "localhost") {
		if e, ok := l.portTerminator(p + len("localhost")); ok {
			return e, true
		}
	}
	if end, ok := ipv6Host(t, p, "["); ok {
		if e, ok := l.portTerminator(end); ok {
			return e, true
		}
	}
	return domains(t, p, 0, 1, 10, rootThen(t, l.portTerminator))
}

// mailHost is the host of a mailto: address: an IPv6 address, or up to
// five labels; the host terminator; no port.
func (l *linker) mailHost(p int) (int, bool) {
	t := l.t
	term := func(e int) (int, bool) {
		if l.hostTerminator(e) {
			return e, true
		}
		return 0, false
	}
	if end, ok := ipv6Host(t, p, "[IPv6:"); ok {
		if e, ok := term(end); ok {
			return e, true
		}
	}
	final := func(q int) (int, bool) {
		for _, e := range domainEnds(t, q) {
			if r, ok := term(e); ok {
				return r, true
			}
		}
		return 0, false
	}
	return domains(t, p, 0, 0, 4, final)
}

// fuzzyMailHost is the host after an '@' of an address with no scheme: an
// IPv6 address, or two to five labels, the last a domain root; the host
// terminator.
func (l *linker) fuzzyMailHost(p int) (int, bool) {
	t := l.t
	term := func(e int) (int, bool) {
		if l.hostTerminator(e) {
			return e, true
		}
		return 0, false
	}
	if end, ok := ipv6Host(t, p, "[IPv6:"); ok {
		if e, ok := term(end); ok {
			return e, true
		}
	}
	return domains(t, p, 0, 1, 4, rootThen(t, term))
}

// rootThen matches a domain root at q, then then.
func rootThen(t string, then func(int) (int, bool)) func(int) (int, bool) {
	return func(q int) (int, bool) {
		for _, e := range rootEnds(t, q) {
			if r, ok := then(e); ok {
				return r, true
			}
		}
		return 0, false
	}
}

// domains matches (domain '.'){min,max} and then final, as a regular
// expression does: as many labels as it can, backing off.
func domains(t string, p, count, minRep, maxRep int, final func(int) (int, bool)) (int, bool) {
	if count < maxRep {
		for _, e := range domainEnds(t, p) {
			if e < len(t) && t[e] == '.' {
				if r, ok := domains(t, e+1, count+1, minRep, maxRep, final); ok {
					return r, true
				}
			}
		}
	}
	if count >= minRep {
		return final(p)
	}
	return 0, false
}

// xnEnds are the ends of an "xn--" label at p, longest first.
func xnEnds(t string, p int) []int {
	if !hasPrefixFold(t[p:], "xn--") {
		return nil
	}
	i := p + 4
	for i < len(t) && i-(p+4) < 59 && (isAlnum(t[i]) || t[i] == '-') {
		i++
	}
	var ends []int
	for e := i; e > p+4; e-- {
		ends = append(ends, e)
	}
	return ends
}

// domainEnds are where linkify-it's src_domain at p may end, in the order
// its alternatives try them: an xn-- label; one pseudo-letter; a
// pseudo-letter, then up to 61 pseudo-letters or '-', then a pseudo-letter,
// longest first.
func domainEnds(t string, p int) []int {
	ends := xnEnds(t, p)
	r, n := runeAt(t, p)
	if n == 0 || !isPseudoLetter(r) {
		return ends
	}
	ends = append(ends, p+n)
	// Runes of pseudo-letters and '-' after the first, at most 62.
	var stops []int
	i := p + n
	for k := 0; k < 62 && i < len(t); k++ {
		r, m := runeAt(t, i)
		if r != '-' && !isPseudoLetter(r) {
			break
		}
		i += m
		if r != '-' {
			stops = append(stops, i)
		}
	}
	for k := len(stops) - 1; k >= 0; k-- {
		ends = append(ends, stops[k])
	}
	return ends
}

// rootEnds are where src_domain_root at p may end: an xn-- label, or one
// to 63 pseudo-letters, longest first.
func rootEnds(t string, p int) []int {
	ends := xnEnds(t, p)
	var stops []int
	i := p
	for k := 0; k < 63 && i < len(t); k++ {
		r, m := runeAt(t, i)
		if !isPseudoLetter(r) {
			break
		}
		i += m
		stops = append(stops, i)
	}
	for k := len(stops) - 1; k >= 0; k-- {
		ends = append(ends, stops[k])
	}
	return ends
}

// ipv6Host matches open (a '[' or "[IPv6:"), an IPv6 address and ']'.
func ipv6Host(t string, p int, open string) (int, bool) {
	if !hasPrefixFold(t[p:], open) {
		return 0, false
	}
	start := p + len(open)
	j := strings.IndexByte(t[start:], ']')
	if j <= 0 || j > 45 {
		return 0, false
	}
	addr := t[start : start+j]
	if !strings.Contains(addr, ":") || strings.Trim(addr, "0123456789abcdefABCDEF:.") != "" || net.ParseIP(addr) == nil {
		return 0, false
	}
	return start + j + 1, true
}

// portTerminator matches an optional port at e, then the host terminator.
// A port is 1 to 4 digits, or 5 no larger than 65535; a longer run, or one
// the pattern refuses, leaves ":<digit>" after the host, which the
// terminator refuses.
func (l *linker) portTerminator(e int) (int, bool) {
	t := l.t
	if e < len(t) && t[e] == ':' {
		d := 0
		for e+1+d < len(t) && isDigit(t[e+1+d]) {
			d++
		}
		if d > 0 && validPort(t[e+1:e+1+d]) && l.hostTerminator(e+1+d) {
			return e + 1 + d, true
		}
	}
	if l.hostTerminator(e) {
		return e, true
	}
	return 0, false
}

func validPort(digits string) bool {
	switch {
	case len(digits) <= 4:
		return true
	case len(digits) > 5:
		return false
	case digits[0] >= '1' && digits[0] <= '5':
		return true
	}
	return digits[0] == '6' && digits <= "65535"
}

// hostTerminator is linkify-it's src_host_terminator at e: the end, or a
// separator, punctuation or control character, but not '-', '_', ':'
// before a digit, ".-", or '.' before anything but the end, punctuation or
// a separator. Loose, it also takes a delimiter (*, _, ~), which may end
// a text token.
func (l *linker) hostTerminator(e int) bool {
	t := l.t
	if e >= len(t) {
		return true
	}
	r, _ := runeAt(t, e)
	if l.loose && (r == '*' || r == '_' || r == '~' && e+1 < len(t) && t[e+1] == '~') {
		return true
	}
	if r != '<' && r != '>' && r != '｜' && !isZPCc(r) {
		return false
	}
	switch r {
	case '-', '_':
		return false
	case ':':
		return e+1 >= len(t) || !isDigit(t[e+1])
	case '.':
		if e+1 >= len(t) {
			return true
		}
		next, _ := runeAt(t, e+1)
		return next != '-' && isZPCc(next)
	}
	return true
}

// linkPath is the end of linkify-it's src_path at pos: '/', '?' or '#' and
// what may follow it, or a lone '/', or nothing.
func (l *linker) linkPath(pos int) int {
	t := l.t
	if pos >= len(t) {
		return pos
	}
	c := t[pos]
	if c != '/' && c != '?' && c != '#' {
		return pos
	}
	if end := l.steps(pos + 1); end > pos+1 {
		return end
	}
	if c == '/' {
		return pos + 1
	}
	return pos
}

// steps is where src_path's repetitions starting at i end.
func (l *linker) steps(i int) int {
	var visited []int
	j := i
	for {
		if l.memo != nil && l.memo[j] >= 0 {
			j = l.memo[j]
			break
		}
		k, ok := pathStep(l.t, j)
		if !ok {
			break
		}
		visited = append(visited, j)
		j = k
	}
	if l.memo != nil {
		for _, v := range visited {
			l.memo[v] = j
		}
	}
	return j
}

// pathStep is one repetition of src_path's alternatives at i.
func pathStep(t string, i int) (int, bool) {
	if i >= len(t) {
		return 0, false
	}
	next := func(k int) rune { // the rune at k; -1 at the end
		r, _ := runeAt(t, k)
		return r
	}
	switch c := t[i]; c {
	case '[':
		return nestedPair(t, i, '[', ']', 4)
	case '(':
		return nestedPair(t, i, '(', ')', 4)
	case '{':
		return nestedPair(t, i, '{', '}', 4)
	case '"':
		return quoted(t, i, '"')
	case '\'':
		if e, ok := quoted(t, i, '\''); ok {
			return e, true
		}
		if r := next(i + 1); r == '-' || r >= 0 && isPseudoLetter(r) {
			return i + 1, true
		}
		return 0, false
	case '.':
		d := runLen(t, i, '.')
		if d >= 2 && d <= 20 {
			j := i + d
			if j < len(t) && t[j] == ':' && j+1 < len(t) && isDotsEnd(t[j+1]) {
				return j + 2, true
			}
			if j < len(t) && isDotsEnd(t[j]) {
				return j + 1, true
			}
		}
		if r := next(i + 1); r >= 0 && r != '.' && !isZCc(r) {
			return i + 1, true
		}
		return 0, false
	case '-':
		return i + min(runLen(t, i, '-'), 20), true
	case ',', ';':
		if r := next(i + 1); r >= 0 && !isZCc(r) {
			return i + 1, true
		}
		return 0, false
	case '!':
		d := runLen(t, i, '!')
		if r := next(i + d); d <= 20 && r >= 0 && !isZCc(r) {
			return i + d, true
		}
		return 0, false
	case '?':
		if r := next(i + 1); r >= 0 && r != '?' && !isZCc(r) {
			return i + 1, true
		}
		return 0, false
	case '\\', '/', ':', '%', '@', '#', '&', '=', '_', '~', '*':
		return i + 1, true
	}
	r, n := runeAt(t, i)
	if isZPCc(r) || r == '<' || r == '>' || r == '｜' {
		return 0, false
	}
	return i + n, true
}

func isDotsEnd(c byte) bool { return isAlnum(c) || c == '%' || c == '/' || c == '&' }

func runLen(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// nestedPair matches open, up to 1000 characters or nested pairs (depth
// levels in all) with no separator or control character, and close.
func nestedPair(t string, i int, open, close byte, depth int) (int, bool) {
	count := 0
	for j := i + 1; j < len(t); {
		c := t[j]
		if c == close {
			return j + 1, true
		}
		if count >= 1000 {
			return 0, false
		}
		if c == open {
			if depth <= 1 {
				return 0, false
			}
			e, ok := nestedPair(t, j, open, close, depth-1)
			if !ok {
				return 0, false
			}
			j = e
			count++
			continue
		}
		r, n := runeAt(t, j)
		if isZCc(r) {
			return 0, false
		}
		j += n
		count += utf16Len(t[j-n : j])
	}
	return 0, false
}

// quoted matches q, 1 to 100 code units that are neither q nor a separator
// or control character, and q.
func quoted(t string, i int, q byte) (int, bool) {
	units := 0
	for j := i + 1; j < len(t); {
		if t[j] == q {
			return j + 1, units > 0
		}
		r, n := runeAt(t, j)
		if isZCc(r) {
			return 0, false
		}
		if units += utf16Len(t[j : j+n]); units > 100 {
			return 0, false
		}
		j += n
	}
	return 0, false
}
