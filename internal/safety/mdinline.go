package safety

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// kind is what a found construct is.
type kind int

const (
	kLink     kind = iota // [text](destination), or a reference to a definition
	kImage                // ![alt](source)
	kAutolink             // <scheme:…> or <address>
	kLinkify              // a URL linkify made a link of while the paragraph was read
	// The dead kinds are what the renderer makes no link of only because
	// its validateLink refuses the scheme (javascript:, vbscript:, file:,
	// data: but for images): stripped all the same, should that check
	// change.
	kDeadLink
	kDeadImage
	kDeadAutolink
)

// found is a link or image the inline parser found.
type found struct {
	kind       kind
	start, end int // the construct, in the inline text
	// textStart and textEnd are a link's text or an image's alt text.
	textStart, textEnd int
	// url is where it leads, as the renderer reads it: a destination with
	// its escapes and entities undone, or a URL as written.
	url string
}

// piece is text the renderer's linkify reads: a run of plain text between
// other tokens (code, TeX, links, escapes, entities, line breaks).
// Emphasis delimiters are pieces of their own, joined to their neighbours
// afterwards.
type piece struct {
	start, end int
	inLink     bool // inside a link's text, where linkify makes no link
	// afterSpecial is true for text right after an escape or an entity,
	// where linkify drops a link that begins the text.
	afterSpecial bool
}

// inlineResult is what parseInline found.
type inlineResult struct {
	found  []found
	pieces []piece
	// code are the spans of code and TeX, where nothing is to be changed.
	code [][2]int
}

// parseInline reads one inline run as markdown-it's inline parser does,
// with the frontend's $…$ rule; refs are the document's definitions.
func parseInline(src string, refs map[string]string) *inlineResult {
	st := &inlineState{src: src, posMax: len(src), refs: refs, special: -1, cache: map[int]int{}, mathNone: map[[2]int]int{}, res: &inlineResult{}}
	st.tokenize()
	return st.res
}

// inlineState is markdown-it's StateInline, less what only rendering needs.
type inlineState struct {
	src         string
	pos, posMax int
	level       int
	linkLevel   int
	// pending is the text not yet pushed as a token: src[pendingFrom:]
	// for pendingLen bytes, pendingUnits UTF-16 code units long (what
	// the linkify rule measures).
	pendingFrom, pendingLen, pendingUnits int
	// special is where the last escape or entity ended.
	special          int
	cache            map[int]int
	lastRuns         map[int]int
	backticksScanned bool
	mathNone         map[[2]int]int
	refs             map[string]string
	res              *inlineResult
}

func (st *inlineState) addPending(from, to int) {
	if st.pendingLen == 0 {
		st.pendingFrom = from
	}
	st.pendingLen += to - from
	st.pendingUnits += utf16Len(st.src[from:to])
}

// trimPending drops n bytes of ASCII from the end of the pending text.
func (st *inlineState) trimPending(n int) {
	n = min(n, st.pendingLen)
	st.pendingLen -= n
	st.pendingUnits -= n
}

// pushPending ends the pending text, as pushing any token does.
func (st *inlineState) pushPending() {
	if st.pendingLen > 0 {
		st.res.pieces = append(st.res.pieces, piece{start: st.pendingFrom, end: st.pendingFrom + st.pendingLen, inLink: st.linkLevel > 0, afterSpecial: st.pendingFrom == st.special})
	}
	st.pendingLen, st.pendingUnits = 0, 0
}

// pushSpecial ends the pending text for an escape or entity ending at end.
func (st *inlineState) pushSpecial(end int) {
	st.pushPending()
	st.special = end
}

func (st *inlineState) record(f found) { st.res.found = append(st.res.found, f) }

// The inline rules, in markdown-it's order with the TeX plugin's after
// escape. html_inline is off.
const nInlineRules = 12

func (st *inlineState) rule(i int, silent bool) bool {
	switch i {
	case 0:
		return st.text(silent)
	case 1:
		return st.linkify(silent)
	case 2:
		return st.newline(silent)
	case 3:
		return st.escape(silent)
	case 4:
		return st.math(silent)
	case 5:
		return st.backticks(silent)
	case 6:
		return st.strikethrough(silent)
	case 7:
		return st.emphasis(silent)
	case 8:
		return st.link(silent)
	case 9:
		return st.image(silent)
	case 10:
		return st.autolink(silent)
	case 11:
		return st.entity(silent)
	}
	return false
}

// tokenize is ParserInline.tokenize.
func (st *inlineState) tokenize() {
	end := st.posMax
	for st.pos < end {
		prev := st.pos
		ok := false
		if st.level < maxNesting {
			for i := 0; i < nInlineRules && !ok; i++ {
				ok = st.rule(i, false)
			}
		}
		if ok {
			if st.pos <= prev {
				st.pos = prev + 1 // markdown-it throws here
			}
			if st.pos >= end {
				break
			}
			continue
		}
		_, n := utf8.DecodeRuneInString(st.src[st.pos:])
		st.addPending(st.pos, st.pos+n)
		st.pos += n
	}
	st.pushPending()
}

// skipToken is ParserInline.skipToken: it moves past whatever begins at
// pos, reading it silently, and remembers where that ends.
func (st *inlineState) skipToken() {
	pos := st.pos
	if end, ok := st.cache[pos]; ok {
		st.pos = end
		return
	}
	ok := false
	if st.level < maxNesting {
		for i := 0; i < nInlineRules && !ok; i++ {
			st.level++
			ok = st.rule(i, true)
			st.level--
		}
	} else {
		st.pos = st.posMax
	}
	if !ok {
		st.pos++
	}
	st.cache[pos] = st.pos
}

func isTerminatorChar(c byte) bool {
	switch c {
	case '\n', '!', '#', '$', '%', '&', '*', '+', '-', ':', '<', '=', '>', '@', '[', '\\', ']', '^', '_', '`', '{', '}', '~':
		return true
	}
	return false
}

func (st *inlineState) text(silent bool) bool {
	pos := st.pos
	for pos < st.posMax && !isTerminatorChar(st.src[pos]) {
		pos++
	}
	if pos == st.pos {
		return false
	}
	if !silent {
		st.addPending(st.pos, pos)
	}
	st.pos = pos
	return true
}

func isSchemeChar(c byte) bool { return isAlnum(c) || c == '+' || c == '-' || c == '.' }

// linkify is markdown-it's inline linkify rule: at "://", a scheme just
// before it in the pending text that linkify-it takes makes a link.
func (st *inlineState) linkify(silent bool) bool {
	if st.linkLevel > 0 {
		return false
	}
	pos, max := st.pos, st.posMax
	if pos+3 > max || st.src[pos] != ':' || st.src[pos+1] != '/' || st.src[pos+2] != '/' {
		return false
	}
	protoMin := pos - min(10, st.pendingUnits, pos)
	protoStart := pos
	for protoStart > protoMin && isSchemeChar(st.src[protoStart-1]) {
		protoStart--
	}
	if protoStart == pos || !isASCIIAlpha(st.src[protoStart]) {
		return false
	}
	protoLength := pos - protoStart
	n := linkifyMatchAtStart(st.src[protoStart:])
	if n == 0 {
		return false
	}
	url := strings.TrimRight(st.src[protoStart:protoStart+n], "*")
	if len(url) <= protoLength {
		return false
	}
	// validateLink passes every scheme linkify-it knows.
	if !silent {
		st.trimPending(protoLength)
		st.pushPending()
		st.record(found{kind: kLinkify, start: protoStart, end: protoStart + len(url), url: url})
	}
	st.pos += len(url) - protoLength
	return true
}

func (st *inlineState) newline(silent bool) bool {
	pos := st.pos
	if st.src[pos] != '\n' {
		return false
	}
	if !silent {
		// A line's trailing spaces are dropped: one before a soft break,
		// all of them before a hard one.
		end := st.pendingFrom + st.pendingLen
		if st.pendingLen > 0 && st.src[end-1] == ' ' {
			if st.pendingLen >= 2 && st.src[end-2] == ' ' {
				ws := st.pendingLen - 2
				for ws >= 1 && st.src[st.pendingFrom+ws-1] == ' ' {
					ws--
				}
				st.trimPending(st.pendingLen - ws)
			} else {
				st.trimPending(1)
			}
		}
		st.pushPending()
	}
	pos++
	for pos < st.posMax && isSpaceTab(st.src[pos]) {
		pos++
	}
	st.pos = pos
	return true
}

func (st *inlineState) escape(silent bool) bool {
	pos, max := st.pos, st.posMax
	if st.src[pos] != '\\' {
		return false
	}
	pos++
	if pos >= max {
		return false
	}
	switch st.src[pos] {
	case '\n':
		if !silent {
			st.pushPending()
		}
		pos++
		for pos < max && isSpaceTab(st.src[pos]) {
			pos++
		}
		st.pos = pos
		return true
	case ' ':
		if !silent {
			st.pushSpecial(pos)
		}
		st.pos = pos
		return true
	}
	_, n := utf8.DecodeRuneInString(st.src[pos:])
	if !silent {
		st.pushSpecial(pos + n)
	}
	st.pos = pos + n
	return true
}

// math is the frontend's $…$ and $$…$$ within a paragraph (Pandoc's rules:
// an opening $ is not followed by a space; a closing one is not preceded by
// one nor followed by a digit; the first unescaped $ after an opening one
// closes it). KaTeX, with trust off, makes no link of what is inside.
func (st *inlineState) math(silent bool) bool {
	src, posMax := st.src, st.posMax
	start := st.pos
	if src[start] != '$' {
		return false
	}
	n := 1
	if start+1 < len(src) && src[start+1] == '$' {
		n = 2
	}
	display := n == 2
	key := [2]int{n, posMax}
	none, known := st.mathNone[key]
	literal := func() bool {
		if !silent {
			st.addPending(start, start+n)
		}
		st.pos = start + n
		return true
	}
	if !display && !mathCanOpen(src, start, posMax) {
		return false
	}
	if known && start > none {
		return literal()
	}
	open := start + n
	delim := src[start : start+n]
	end := open
	for {
		i := strings.Index(src[end:], delim)
		if i < 0 || end+i >= posMax {
			st.mathNone[key] = start
			return literal()
		}
		end += i
		if !mathEscaped(src, end, start) {
			break
		}
		end++
	}
	if !display && !mathCanClose(src, end, posMax) {
		return literal()
	}
	content := src[open:end]
	if jsTrim(content) == "" || strings.IndexByte(content, '`') >= 0 {
		return literal()
	}
	if !silent {
		st.pushPending()
		st.res.code = append(st.res.code, [2]int{start, end + n})
	}
	st.pos = end + n
	return true
}

func isMathSpace(c int) bool { return c == ' ' || c == '\t' || c == '\n' }

func byteAt(s string, i int) int {
	if i < 0 || i >= len(s) {
		return -1
	}
	return int(s[i])
}

func mathCanOpen(src string, pos, max int) bool {
	next := -1
	if pos+1 < max {
		next = byteAt(src, pos+1)
	}
	return next != -1 && !isMathSpace(next)
}

func mathCanClose(src string, pos, max int) bool {
	prev, next := -1, -1
	if pos > 0 {
		prev = byteAt(src, pos-1)
	}
	if pos+1 < max {
		next = byteAt(src, pos+1)
	}
	return prev != -1 && !isMathSpace(prev) && (next < '0' || next > '9')
}

func mathEscaped(src string, pos, from int) bool {
	slashes := 0
	for i := pos - 1; i > from && src[i] == '\\'; i-- {
		slashes++
	}
	return slashes%2 == 1
}

func buildLastRuns(src string) map[int]int {
	runs := map[int]int{}
	for pos := 0; ; {
		i := strings.IndexByte(src[pos:], '`')
		if i < 0 {
			return runs
		}
		start := pos + i
		pos = start + 1
		for pos < len(src) && src[pos] == '`' {
			pos++
		}
		runs[pos-start] = start
	}
}

// backticks is a code span: the next run of as many backticks closes it,
// anywhere in the inline run.
func (st *inlineState) backticks(silent bool) bool {
	start := st.pos
	if st.src[start] != '`' {
		return false
	}
	max := st.posMax
	pos := start + 1
	for pos < max && st.src[pos] == '`' {
		pos++
	}
	openerLength := pos - start
	if !st.backticksScanned {
		st.lastRuns = buildLastRuns(st.src)
		st.backticksScanned = true
	}
	if last, ok := st.lastRuns[openerLength]; ok && last >= pos {
		for matchEnd := pos; ; {
			i := strings.IndexByte(st.src[matchEnd:], '`')
			if i < 0 || matchEnd+i >= max {
				break
			}
			matchStart := matchEnd + i
			matchEnd = matchStart + 1
			for matchEnd < len(st.src) && st.src[matchEnd] == '`' {
				matchEnd++
			}
			if matchEnd > max {
				break
			}
			if matchEnd-matchStart == openerLength {
				if !silent {
					st.pushPending()
					st.res.code = append(st.res.code, [2]int{start, matchEnd})
				}
				st.pos = matchEnd
				return true
			}
		}
	}
	if !silent {
		st.addPending(start, pos)
	}
	st.pos = pos
	return true
}

// delimiterRun is the length of the run of c at pos, within posMax.
func (st *inlineState) delimiterRun(c byte) int {
	n := 0
	for st.pos+n < st.posMax && st.src[st.pos+n] == c {
		n++
	}
	return n
}

// strikethrough and emphasis push their delimiters as text tokens of their
// own, which ends the pending text; linkify reads them joined to their
// neighbours when they are left unmatched, and apart when they are
// matched. Body joins them either way and allows for the split (see
// linkifyCandidates).
func (st *inlineState) strikethrough(silent bool) bool {
	if silent || st.src[st.pos] != '~' {
		return false
	}
	n := st.delimiterRun('~')
	if n < 2 {
		return false
	}
	st.delimiter(n)
	return true
}

func (st *inlineState) emphasis(silent bool) bool {
	if silent || st.src[st.pos] != '_' && st.src[st.pos] != '*' {
		return false
	}
	st.delimiter(st.delimiterRun(st.src[st.pos]))
	return true
}

func (st *inlineState) delimiter(n int) {
	st.pushPending()
	st.res.pieces = append(st.res.pieces, piece{start: st.pos, end: st.pos + n, inLink: st.linkLevel > 0, afterSpecial: st.pos == st.special})
	st.pos += n
}

// parseLinkLabel finds the ']' that ends the label whose '[' is at start,
// skipping what other rules take whole (code spans, autolinks, TeX, and
// with disableNested false, links), or returns -1.
func (st *inlineState) parseLinkLabel(start int, disableNested bool) int {
	max, oldPos := st.posMax, st.pos
	st.pos = start + 1
	level := 1
	found := false
	for st.pos < max {
		marker := st.src[st.pos]
		if marker == ']' {
			level--
			if level == 0 {
				found = true
				break
			}
		}
		prevPos := st.pos
		st.skipToken()
		if marker == '[' {
			if prevPos == st.pos-1 {
				level++
			} else if disableNested {
				st.pos = oldPos
				return -1
			}
		}
	}
	labelEnd := -1
	if found {
		labelEnd = st.pos
	}
	st.pos = oldPos
	return labelEnd
}

// skipSpaceNL skips spaces, tabs and line endings.
func (st *inlineState) skipSpaceNL(pos, max int) int {
	for pos < max && (isSpaceTab(st.src[pos]) || st.src[pos] == '\n') {
		pos++
	}
	return pos
}

// inlineTail reads the rest of an inline link or image after its
// destination ends at pos: an optional title, then ')'. It returns where the
// link ends, or -1.
func (st *inlineState) inlineTail(pos int) int {
	max := st.posMax
	start := pos
	pos = st.skipSpaceNL(pos, max)
	if title := parseLinkTitle(st.src, pos, max, nil); pos < max && start != pos && title.ok {
		pos = st.skipSpaceNL(title.pos, max)
	}
	if pos >= max || st.src[pos] != ')' {
		return -1
	}
	return pos + 1
}

func (st *inlineState) link(silent bool) bool {
	src := st.src
	if src[st.pos] != '[' {
		return false
	}
	oldPos, max := st.pos, st.posMax
	labelStart := st.pos + 1
	labelEnd := st.parseLinkLabel(st.pos, true)
	if labelEnd < 0 {
		return false
	}
	pos := labelEnd + 1
	parseReference := true
	href := ""
	dead := -1
	deadURL := ""
	if pos < max && src[pos] == '(' {
		parseReference = false
		pos = st.skipSpaceNL(pos+1, max)
		if pos >= max {
			return false
		}
		res := parseLinkDestination(src, pos, st.posMax)
		if res.ok {
			if validateLink(res.str) {
				href, pos = res.str, res.pos
			} else if end := st.inlineTail(res.pos); end >= 0 {
				dead, deadURL = end, res.str
			}
			start := pos
			pos = st.skipSpaceNL(pos, max)
			if title := parseLinkTitle(src, pos, st.posMax, nil); pos < max && start != pos && title.ok {
				pos = st.skipSpaceNL(title.pos, max)
			}
		}
		if pos >= max || src[pos] != ')' {
			parseReference = true
		}
		pos++
	}
	if parseReference {
		if st.refs == nil {
			st.recordDead(silent, kDeadLink, oldPos, dead, labelStart, labelEnd, deadURL)
			return false
		}
		label := ""
		if pos < max && src[pos] == '[' {
			start := pos + 1
			if p := st.parseLinkLabel(pos, false); p >= 0 {
				label, pos = src[start:p], p+1
			} else {
				pos = labelEnd + 1
			}
		} else {
			pos = labelEnd + 1
		}
		if label == "" {
			label = src[labelStart:labelEnd]
		}
		dest, ok := st.refs[normalizeReference(label)]
		if !ok {
			st.pos = oldPos
			st.recordDead(silent, kDeadLink, oldPos, dead, labelStart, labelEnd, deadURL)
			return false
		}
		href = dest
	}
	if !silent {
		st.pushPending()
		st.record(found{kind: kLink, start: oldPos, end: pos, textStart: labelStart, textEnd: labelEnd, url: href})
		st.pos, st.posMax = labelStart, labelEnd
		st.linkLevel++
		st.level++
		st.tokenize()
		st.level--
		st.linkLevel--
	}
	st.pos, st.posMax = pos, max
	return true
}

func (st *inlineState) recordDead(silent bool, k kind, start, end, textStart, textEnd int, url string) {
	if silent || end < 0 {
		return
	}
	// The '[' of a dead image is read again as a link, dead too.
	if n := len(st.res.found); k == kDeadLink && n > 0 && st.res.found[n-1].kind == kDeadImage && st.res.found[n-1].start == start-1 {
		return
	}
	st.record(found{kind: k, start: start, end: end, textStart: textStart, textEnd: textEnd, url: url})
}

func (st *inlineState) image(silent bool) bool {
	src := st.src
	oldPos, max := st.pos, st.posMax
	if src[st.pos] != '!' || st.pos+1 >= len(src) || src[st.pos+1] != '[' {
		return false
	}
	labelStart := st.pos + 2
	labelEnd := st.parseLinkLabel(st.pos+1, false)
	if labelEnd < 0 {
		return false
	}
	pos := labelEnd + 1
	href := ""
	if pos < max && src[pos] == '(' {
		pos = st.skipSpaceNL(pos+1, max)
		if pos >= max {
			return false
		}
		res := parseLinkDestination(src, pos, st.posMax)
		if res.ok {
			if validateLink(res.str) {
				href, pos = res.str, res.pos
			} else if end := st.inlineTail(res.pos); end >= 0 {
				st.recordDead(silent, kDeadImage, oldPos, end, labelStart, labelEnd, res.str)
			}
		}
		start := pos
		pos = st.skipSpaceNL(pos, max)
		if title := parseLinkTitle(src, pos, st.posMax, nil); pos < max && start != pos && title.ok {
			pos = st.skipSpaceNL(title.pos, max)
		}
		if pos >= max || src[pos] != ')' {
			st.pos = oldPos
			return false
		}
		pos++
	} else {
		if st.refs == nil {
			return false
		}
		label := ""
		if pos < max && src[pos] == '[' {
			start := pos + 1
			if p := st.parseLinkLabel(pos, false); p >= 0 {
				label, pos = src[start:p], p+1
			} else {
				pos = labelEnd + 1
			}
		} else {
			pos = labelEnd + 1
		}
		if label == "" {
			label = src[labelStart:labelEnd]
		}
		dest, ok := st.refs[normalizeReference(label)]
		if !ok {
			st.pos = oldPos
			return false
		}
		href = dest
	}
	if !silent {
		st.pushPending()
		st.record(found{kind: kImage, start: oldPos, end: pos, textStart: labelStart, textEnd: labelEnd, url: href})
	}
	st.pos, st.posMax = pos, max
	return true
}

var (
	autolinkRe      = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]{1,31}:[^<>\x00-\x20]*$`)
	emailAutolinkRe = regexp.MustCompile("^[a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$")
)

func (st *inlineState) autolink(silent bool) bool {
	pos := st.pos
	if st.src[pos] != '<' {
		return false
	}
	start, max := st.pos, st.posMax
	for {
		pos++
		if pos >= max || st.src[pos] == '<' {
			return false
		}
		if st.src[pos] == '>' {
			break
		}
	}
	url := st.src[start+1 : pos]
	switch {
	case autolinkRe.MatchString(url):
		if !validateLink(url) {
			if !silent {
				st.record(found{kind: kDeadAutolink, start: start, end: pos + 1, url: url})
			}
			return false
		}
	case emailAutolinkRe.MatchString(url):
		url = "mailto:" + url
	default:
		return false
	}
	if !silent {
		st.pushPending()
		st.record(found{kind: kAutolink, start: start, end: pos + 1, url: url})
	}
	st.pos = pos + 1
	return true
}

var (
	digitalEntityRe = regexp.MustCompile(`(?i)^&#(?:x[a-f0-9]{1,6}|[0-9]{1,7});`)
	namedEntityRe   = regexp.MustCompile(`(?i)^&[a-z][a-z0-9]{1,31};`)
)

func (st *inlineState) entity(silent bool) bool {
	pos := st.pos
	if st.src[pos] != '&' || pos+1 >= st.posMax {
		return false
	}
	m := ""
	if st.src[pos+1] == '#' {
		m = digitalEntityRe.FindString(st.src[pos:])
	} else if m = namedEntityRe.FindString(st.src[pos:]); m != "" {
		if _, ok := decodeNamedEntity(m); !ok {
			m = ""
		}
	}
	if m == "" {
		return false
	}
	if !silent {
		st.pushSpecial(pos + len(m))
	}
	st.pos += len(m)
	return true
}

// decodeNamedEntity decodes "&name;" only if the whole name is an HTML
// entity, as the entities package's decodeHTMLStrict does: Go's html
// package would also take a legacy prefix (&ampx; as &x;).
func decodeNamedEntity(m string) (string, bool) {
	d := html.UnescapeString(m)
	if d == m || strings.HasSuffix(d, ";") && d != ";" {
		return m, false
	}
	return d, true
}

type destResult struct {
	ok  bool
	pos int
	str string
}

// parseLinkDestination reads a destination at start: <…>, or a run
// without spaces or controls whose parentheses balance (at most 32 deep).
// Its str has escapes and entities undone.
func parseLinkDestination(str string, start, max int) destResult {
	pos := start
	if pos < len(str) && str[pos] == '<' {
		for pos++; pos < max; pos++ {
			switch str[pos] {
			case '\n', '<':
				return destResult{}
			case '>':
				return destResult{ok: true, pos: pos + 1, str: unescapeAll(str[start+1 : pos])}
			case '\\':
				if pos+1 < max {
					pos++
				}
			}
		}
		return destResult{}
	}
	level := 0
loop:
	for pos < max {
		c := str[pos]
		switch {
		case c <= ' ' || c == 0x7f:
			break loop
		case c == '\\' && pos+1 < max:
			if str[pos+1] == ' ' {
				pos++
			} else {
				pos += 2
			}
			continue
		case c == '(':
			if level++; level > 32 {
				return destResult{}
			}
		case c == ')':
			if level == 0 {
				break loop
			}
			level--
		}
		pos++
	}
	if start == pos || level != 0 {
		return destResult{}
	}
	return destResult{ok: true, pos: pos, str: unescapeAll(str[start:pos])}
}

type titleResult struct {
	ok, canContinue bool
	pos             int
	str             string
	marker          byte
}

// parseLinkTitle reads a title at start: "…", '…' or (…). With prev, it
// goes on with a title a reference definition began on an earlier line.
func parseLinkTitle(str string, start, max int, prev *titleResult) titleResult {
	pos := start
	var st titleResult
	if prev != nil {
		st.str, st.marker = prev.str, prev.marker
	} else {
		if pos >= max {
			return st
		}
		marker := str[pos]
		if marker != '"' && marker != '\'' && marker != '(' {
			return st
		}
		start++
		pos++
		if marker == '(' {
			marker = ')'
		}
		st.marker = marker
	}
	for pos < max {
		c := str[pos]
		switch {
		case c == st.marker:
			st.pos = pos + 1
			st.str += unescapeAll(str[start:pos])
			st.ok = true
			return st
		case c == '(' && st.marker == ')':
			return st
		case c == '\\' && pos+1 < max:
			pos++
		}
		pos++
	}
	st.canContinue = true
	st.str += unescapeAll(str[start:pos])
	return st
}

var (
	unescapeAllRe     = regexp.MustCompile("(?i)\\\\([!-/:-@\\[-`{-~])|&([a-z#][a-z0-9]{1,31});")
	digitalEntityTest = regexp.MustCompile(`(?i)^#(?:x[a-f0-9]{1,8}|[0-9]{1,8})$`)
)

// unescapeAll undoes backslash escapes and HTML entities, as markdown-it
// does to a destination or title.
func unescapeAll(s string) string {
	if !strings.ContainsAny(s, `\&`) {
		return s
	}
	return unescapeAllRe.ReplaceAllStringFunc(s, func(m string) string {
		if m[0] == '\\' {
			return m[1:]
		}
		name := m[1 : len(m)-1]
		if name[0] == '#' && digitalEntityTest.MatchString(name) {
			var code int64
			var err error
			if name[1] == 'x' || name[1] == 'X' {
				code, err = strconv.ParseInt(name[2:], 16, 64)
			} else {
				code, err = strconv.ParseInt(name[1:], 10, 64)
			}
			if err == nil && isValidEntityCode(code) {
				return string(rune(code)) // #nosec G115 -- at most 0x10FFFF, checked above.
			}
			return m
		}
		if name[0] == '#' {
			return html.UnescapeString(m)
		}
		d, _ := decodeNamedEntity(m)
		return d
	})
}

func isValidEntityCode(c int64) bool {
	switch {
	case c >= 0xD800 && c <= 0xDFFF, c >= 0xFDD0 && c <= 0xFDEF, c&0xFFFF == 0xFFFF, c&0xFFFF == 0xFFFE,
		c >= 0 && c <= 8, c == 11, c >= 14 && c <= 31, c >= 127 && c <= 159, c > 0x10FFFF:
		return false
	}
	return true
}

var (
	badProtoRe = regexp.MustCompile(`^(?:vbscript|javascript|file|data):`)
	goodDataRe = regexp.MustCompile(`^data:image/(?:gif|png|jpeg|webp);`)
)

// validateLink is markdown-it's: no javascript:, vbscript:, file: or
// data: but a few image types. What it refuses is no link at all.
func validateLink(url string) bool {
	s := strings.ToLower(jsTrim(url))
	if badProtoRe.MatchString(s) {
		return goodDataRe.MatchString(s)
	}
	return true
}

// normalizeReference matches labels as markdown-it does: trimmed, white
// space collapsed, case folded.
func normalizeReference(s string) string {
	s = strings.Join(strings.FieldsFunc(s, isJSSpace), " ")
	return strings.ToUpper(strings.ToLower(s))
}
