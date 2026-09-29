package safety

import (
	"regexp"
	"strconv"
	"strings"
)

// This file and mdinline.go read Markdown exactly as AIShie-Frontend
// renders an answer (src/utils/markdown.ts): markdown-it 15 with html off,
// linkify on and the frontend's TeX plugin (src/utils/markdownMath.ts).
// They are a port of markdown-it's own rules, kept close to its source so
// that the two can be compared line by line, and they report only what
// Body needs: the inline text each block holds, where each link, image and
// URL is in it, and the link reference definitions. Code blocks, code spans
// and TeX are what the renderer makes of them and nothing is looked for in
// them, since nothing in them is clickable.

// maxNesting is markdown-it's default: deeper blocks are not rendered, and
// deeper inline constructs are read as text.
const maxNesting = 100

// document is what the block parser found in a body.
type document struct {
	src string
	// inlines are the inline runs of paragraphs, headings and table cells.
	inlines []text
	// defs are the link reference definitions, in order.
	defs []*definition
	// refs maps a normalized label to the destination of its first
	// definition; nil when there is none, as markdown-it's env.references.
	refs map[string]string
}

// definition is a link reference definition ([label]: destination "title").
type definition struct {
	dest string // unescaped, as the renderer reads it
	// lines are its lines: each line's start, and where its content (after
	// any block quote marker and indentation) begins and ends.
	lines [][3]int
}

// parseBlocks reads src into blocks as markdown-it's block parser does.
// src has \n line endings and no NUL (markdown-it's normalize rule).
func parseBlocks(src string) *document {
	doc := &document{src: src}
	if src == "" {
		return doc
	}
	s := newBlockState(src, doc)
	s.tokenize(0, s.lineMax)
	return doc
}

// blockState is markdown-it's StateBlock.
type blockState struct {
	src                                     string
	bMarks, eMarks, tShift, sCount, bsCount []int
	lineStarts                              []int
	blkIndent, line, lineMax, listIndent    int
	parentType                              string
	level                                   int
	doc                                     *document
	// mathNone is the TeX plugin's noBlockClose: the lines a $$ found no
	// closing $$ in, by end line and indentation.
	mathNone map[[2]int][2]int
}

func newBlockState(src string, doc *document) *blockState {
	s := &blockState{src: src, listIndent: -1, parentType: "root", doc: doc, mathNone: map[[2]int][2]int{}}
	start, indent, offset, indentFound := 0, 0, 0, false
	for pos := 0; pos < len(src); pos++ {
		ch := src[pos]
		if !indentFound {
			if isSpaceTab(ch) {
				indent++
				if ch == '\t' {
					offset += 4 - offset%4
				} else {
					offset++
				}
				continue
			}
			indentFound = true
		}
		if ch == '\n' || pos == len(src)-1 {
			if ch != '\n' {
				pos++
			}
			s.bMarks = append(s.bMarks, start)
			s.eMarks = append(s.eMarks, pos)
			s.tShift = append(s.tShift, indent)
			s.sCount = append(s.sCount, offset)
			s.bsCount = append(s.bsCount, 0)
			indentFound, indent, offset, start = false, 0, 0, pos+1
		}
	}
	s.bMarks = append(s.bMarks, len(src))
	s.eMarks = append(s.eMarks, len(src))
	s.tShift = append(s.tShift, 0)
	s.sCount = append(s.sCount, 0)
	s.bsCount = append(s.bsCount, 0)
	s.lineMax = len(s.bMarks) - 1
	s.lineStarts = append([]int(nil), s.bMarks...)
	return s
}

// at is the byte at pos, or -1 past the end (JavaScript's NaN).
func (s *blockState) at(pos int) int {
	if pos < 0 || pos >= len(s.src) {
		return -1
	}
	return int(s.src[pos])
}

func (s *blockState) isEmpty(line int) bool { return s.bMarks[line]+s.tShift[line] >= s.eMarks[line] }

func (s *blockState) skipEmptyLines(from int) int {
	for ; from < s.lineMax; from++ {
		if s.bMarks[from]+s.tShift[from] < s.eMarks[from] {
			break
		}
	}
	return from
}

func (s *blockState) skipSpaces(pos int) int {
	for pos < len(s.src) && isSpaceTab(s.src[pos]) {
		pos++
	}
	return pos
}

func (s *blockState) skipSpacesBack(pos, min int) int {
	if pos <= min {
		return pos
	}
	for pos > min {
		pos--
		if !isSpaceTab(s.src[pos]) {
			return pos + 1
		}
	}
	return pos
}

func (s *blockState) skipChars(pos int, c byte) int {
	for pos < len(s.src) && s.src[pos] == c {
		pos++
	}
	return pos
}

func (s *blockState) skipCharsBack(pos int, c byte, min int) int {
	if pos <= min {
		return pos
	}
	for pos > min {
		pos--
		if s.src[pos] != c {
			return pos + 1
		}
	}
	return pos
}

// getLines is lines [begin, end), less indent columns of indentation each.
func (s *blockState) getLines(begin, end, indent int, keepLastLF bool) text {
	if begin >= end {
		return text{}
	}
	parts := make([]text, 0, end-begin)
	for line := begin; line < end; line++ {
		lineIndent := 0
		lineStart := s.bMarks[line]
		first := lineStart
		last := s.eMarks[line]
		if line+1 < end || keepLastLF {
			last++
		}
		last = min(last, len(s.src))
	indentation:
		for first < last && lineIndent < indent {
			ch := s.src[first]
			switch {
			case ch == '\t':
				lineIndent += 4 - (lineIndent+s.bsCount[line])%4
			case ch == ' ':
				lineIndent++
			case first-lineStart < s.tShift[line]:
				lineIndent++
			default:
				break indentation
			}
			first++
		}
		if lineIndent > indent {
			parts = append(parts, madeUp(strings.Repeat(" ", lineIndent-indent)))
		}
		parts = append(parts, sourceText(s.src, first, last))
	}
	return joinText(parts)
}

// tokenize is ParserBlock.tokenize.
func (s *blockState) tokenize(startLine, endLine int) {
	line := startLine
	for line < endLine {
		line = s.skipEmptyLines(line)
		s.line = line
		if line >= endLine || s.sCount[line] < s.blkIndent {
			break
		}
		if s.level >= maxNesting {
			s.line = endLine
			break
		}
		prev := s.line
		s.rules(line, endLine)
		if s.line <= prev {
			// markdown-it throws here; no rule can leave a line unread.
			s.line = prev + 1
		}
		line = s.line
		if line < endLine && s.isEmpty(line) {
			line++
			s.line = line
		}
	}
}

// rules tries the block rules in markdown-it's order, with the TeX
// plugin's math_block before fence. html_block is off (html: false).
func (s *blockState) rules(line, end int) bool {
	return s.table(line, end, false) || s.code(line, end) || s.mathBlock(line, end, false) ||
		s.fence(line, end, false) || s.blockquote(line, end, false) || s.hr(line, end, false) ||
		s.list(line, end, false) || s.reference(line, end, false) || s.heading(line, end, false) ||
		s.lheading(line, end) || s.paragraph(line, end)
}

// terminates reports whether a rule of chain (the rules that may cut a
// block of that kind short) starts at line.
func (s *blockState) terminates(chain string, line, end int) bool {
	switch chain {
	case "paragraph", "reference":
		return s.table(line, end, true) || s.mathBlock(line, end, true) || s.fence(line, end, true) ||
			s.blockquote(line, end, true) || s.hr(line, end, true) || s.list(line, end, true) || s.heading(line, end, true)
	case "blockquote":
		return s.mathBlock(line, end, true) || s.fence(line, end, true) || s.blockquote(line, end, true) ||
			s.hr(line, end, true) || s.list(line, end, true) || s.heading(line, end, true)
	case "list":
		return s.mathBlock(line, end, true) || s.fence(line, end, true) || s.blockquote(line, end, true) || s.hr(line, end, true)
	}
	return false
}

func (s *blockState) addInline(t text) { s.doc.inlines = append(s.doc.inlines, t) }

var tableAlignRe = regexp.MustCompile(`^:?-+:?$`)

func (s *blockState) lineText(line int) text {
	return sourceText(s.src, s.bMarks[line]+s.tShift[line], s.eMarks[line])
}

// escapedSplit splits a table row at each '|' not after a backslash, and
// drops the backslash before an escaped one.
func escapedSplit(t text) []text {
	var (
		result  []text
		current []text
	)
	lastPos, isEscaped := 0, false
	for pos := 0; pos < len(t.s); pos++ {
		ch := t.s[pos]
		if ch == '|' {
			if !isEscaped {
				result = append(result, joinText(append(current, t.slice(lastPos, pos))))
				current, lastPos = nil, pos+1
			} else {
				current = append(current, t.slice(lastPos, pos-1))
				lastPos = pos
			}
		}
		isEscaped = ch == '\\'
	}
	return append(result, joinText(append(current, t.slice(lastPos, len(t.s)))))
}

func trimEmptyEnds(cols []text) []text {
	if len(cols) > 0 && cols[0].s == "" {
		cols = cols[1:]
	}
	if len(cols) > 0 && cols[len(cols)-1].s == "" {
		cols = cols[:len(cols)-1]
	}
	return cols
}

func (s *blockState) table(startLine, endLine int, silent bool) bool {
	if startLine+2 > endLine {
		return false
	}
	nextLine := startLine + 1
	if s.sCount[nextLine] < s.blkIndent || s.sCount[nextLine]-s.blkIndent >= 4 {
		return false
	}
	pos := s.bMarks[nextLine] + s.tShift[nextLine]
	if pos >= s.eMarks[nextLine] {
		return false
	}
	firstCh := s.src[pos]
	pos++
	if firstCh != '|' && firstCh != '-' && firstCh != ':' {
		return false
	}
	if pos >= s.eMarks[nextLine] {
		return false
	}
	secondCh := s.src[pos]
	pos++
	if secondCh != '|' && secondCh != '-' && secondCh != ':' && !isSpaceTab(secondCh) {
		return false
	}
	if firstCh == '-' && isSpaceTab(secondCh) {
		return false
	}
	for ; pos < s.eMarks[nextLine]; pos++ {
		ch := s.src[pos]
		if ch != '|' && ch != '-' && ch != ':' && !isSpaceTab(ch) {
			return false
		}
	}
	columns := strings.Split(s.lineText(startLine+1).s, "|")
	aligns := 0
	for i, c := range columns {
		t := jsTrim(c)
		if t == "" {
			if i == 0 || i == len(columns)-1 {
				continue
			}
			return false
		}
		if !tableAlignRe.MatchString(t) {
			return false
		}
		aligns++
	}
	header := jsTrimText(s.lineText(startLine))
	if !strings.Contains(header.s, "|") || s.sCount[startLine]-s.blkIndent >= 4 {
		return false
	}
	cols := trimEmptyEnds(escapedSplit(header))
	columnCount := len(cols)
	if columnCount == 0 || columnCount != aligns {
		return false
	}
	if silent {
		return true
	}
	oldParentType := s.parentType
	s.parentType = "table"
	for _, c := range cols {
		s.addInline(jsTrimText(c))
	}
	autocompleted := 0
	for nextLine = startLine + 2; nextLine < endLine; nextLine++ {
		if s.sCount[nextLine] < s.blkIndent || s.terminates("blockquote", nextLine, endLine) {
			break
		}
		row := jsTrimText(s.lineText(nextLine))
		if row.s == "" || s.sCount[nextLine]-s.blkIndent >= 4 {
			break
		}
		cols = trimEmptyEnds(escapedSplit(row))
		autocompleted += columnCount - len(cols)
		if autocompleted > 65536 {
			break
		}
		// Cells past the header's count are not rendered.
		for i := 0; i < columnCount && i < len(cols); i++ {
			s.addInline(jsTrimText(cols[i]))
		}
	}
	s.parentType = oldParentType
	s.line = nextLine
	return true
}

// code is an indented code block: nothing in it is read.
func (s *blockState) code(startLine, endLine int) bool {
	if s.sCount[startLine]-s.blkIndent < 4 {
		return false
	}
	nextLine, last := startLine+1, startLine+1
	for nextLine < endLine {
		if s.isEmpty(nextLine) {
			nextLine++
			continue
		}
		if s.sCount[nextLine]-s.blkIndent >= 4 {
			nextLine++
			last = nextLine
			continue
		}
		break
	}
	s.line = last
	return true
}

// mathBlock is the frontend's $$ display: KaTeX makes no link of it.
func (s *blockState) mathBlock(startLine, endLine int, silent bool) bool {
	if s.sCount[startLine]-s.blkIndent >= 4 {
		return false
	}
	pos := s.bMarks[startLine] + s.tShift[startLine]
	max := s.eMarks[startLine]
	if pos+2 > max || s.src[pos:pos+2] != "$$" {
		return false
	}
	pos += 2
	first := jsTrim(s.src[pos:max])
	if strings.Contains(strings.TrimSuffix(first, "$$"), "$$") {
		return false
	}
	line := startLine
	var body string
	if strings.HasSuffix(first, "$$") {
		body = strings.TrimSuffix(first, "$$")
	} else {
		where := [2]int{endLine, s.blkIndent}
		if none, ok := s.mathNone[where]; ok && none[0] < startLine && startLine < none[1] {
			return false
		}
		var lines []string
		if first != "" {
			lines = append(lines, first)
		}
		closed := false
		for line++; line < endLine; line++ {
			if s.isEmpty(line) {
				break
			}
			pos = s.bMarks[line] + s.tShift[line]
			max = s.eMarks[line]
			if s.sCount[line] < s.blkIndent {
				break
			}
			t := s.src[pos:max]
			if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
				break
			}
			if e := strings.TrimRightFunc(t, isJSSpace); strings.HasSuffix(e, "$$") {
				lines = append(lines, strings.TrimSuffix(e, "$$"))
				closed = true
				break
			}
			lines = append(lines, t)
		}
		if !closed {
			s.mathNone[where] = [2]int{startLine, line}
			return false
		}
		body = strings.Join(lines, "\n")
	}
	if jsTrim(body) == "" {
		return false
	}
	if silent {
		return true
	}
	s.line = line + 1
	return true
}

// fence is a fenced code block: nothing in it is read.
func (s *blockState) fence(startLine, endLine int, silent bool) bool {
	pos := s.bMarks[startLine] + s.tShift[startLine]
	max := s.eMarks[startLine]
	if s.sCount[startLine]-s.blkIndent >= 4 || pos+3 > max {
		return false
	}
	marker := s.src[pos]
	if marker != '~' && marker != '`' {
		return false
	}
	mem := pos
	pos = s.skipChars(pos, marker)
	length := pos - mem
	if length < 3 {
		return false
	}
	if marker == '`' && strings.IndexByte(s.src[pos:max], '`') >= 0 {
		return false
	}
	if silent {
		return true
	}
	nextLine := startLine
	haveEnd := false
	for {
		nextLine++
		if nextLine >= endLine {
			break
		}
		pos = s.bMarks[nextLine] + s.tShift[nextLine]
		mem = pos
		max = s.eMarks[nextLine]
		if pos < max && s.sCount[nextLine] < s.blkIndent {
			break
		}
		if s.at(pos) != int(marker) || s.sCount[nextLine]-s.blkIndent >= 4 {
			continue
		}
		pos = s.skipChars(pos, marker)
		if pos-mem < length {
			continue
		}
		if pos = s.skipSpaces(pos); pos < max {
			continue
		}
		haveEnd = true
		break
	}
	s.line = nextLine
	if haveEnd {
		s.line++
	}
	return true
}

func (s *blockState) blockquote(startLine, endLine int, silent bool) bool {
	pos := s.bMarks[startLine] + s.tShift[startLine]
	oldLineMax := s.lineMax
	if s.sCount[startLine]-s.blkIndent >= 4 || s.at(pos) != '>' {
		return false
	}
	if silent {
		return true
	}
	var oldBMarks, oldBSCount, oldSCount, oldTShift []int
	oldParentType := s.parentType
	s.parentType = "blockquote"
	lastLineEmpty := false
	nextLine := startLine
	for ; nextLine < endLine; nextLine++ {
		isOutdented := s.sCount[nextLine] < s.blkIndent
		pos = s.bMarks[nextLine] + s.tShift[nextLine]
		max := s.eMarks[nextLine]
		if pos >= max {
			break
		}
		if s.src[pos] == '>' && !isOutdented {
			pos++
			initial := s.sCount[nextLine] + 1
			spaceAfterMarker, adjustTab := false, false
			switch s.at(pos) {
			case ' ':
				pos++
				initial++
				spaceAfterMarker = true
			case '\t':
				spaceAfterMarker = true
				if (s.bsCount[nextLine]+initial)%4 == 3 {
					pos++
					initial++
				} else {
					adjustTab = true
				}
			}
			offset := initial
			oldBMarks = append(oldBMarks, s.bMarks[nextLine])
			s.bMarks[nextLine] = pos
			for ; pos < max; pos++ {
				ch := s.src[pos]
				if !isSpaceTab(ch) {
					break
				}
				if ch == '\t' {
					adj := 0
					if adjustTab {
						adj = 1
					}
					offset += 4 - (offset+s.bsCount[nextLine]+adj)%4
				} else {
					offset++
				}
			}
			lastLineEmpty = pos >= max
			oldBSCount = append(oldBSCount, s.bsCount[nextLine])
			s.bsCount[nextLine] = s.sCount[nextLine] + 1
			if spaceAfterMarker {
				s.bsCount[nextLine]++
			}
			oldSCount = append(oldSCount, s.sCount[nextLine])
			s.sCount[nextLine] = offset - initial
			oldTShift = append(oldTShift, s.tShift[nextLine])
			s.tShift[nextLine] = pos - s.bMarks[nextLine]
			continue
		}
		if lastLineEmpty {
			break
		}
		if s.terminates("blockquote", nextLine, endLine) {
			s.lineMax = nextLine
			if s.blkIndent != 0 {
				oldBMarks = append(oldBMarks, s.bMarks[nextLine])
				oldBSCount = append(oldBSCount, s.bsCount[nextLine])
				oldTShift = append(oldTShift, s.tShift[nextLine])
				oldSCount = append(oldSCount, s.sCount[nextLine])
				s.sCount[nextLine] -= s.blkIndent
			}
			break
		}
		oldBMarks = append(oldBMarks, s.bMarks[nextLine])
		oldBSCount = append(oldBSCount, s.bsCount[nextLine])
		oldTShift = append(oldTShift, s.tShift[nextLine])
		oldSCount = append(oldSCount, s.sCount[nextLine])
		s.sCount[nextLine] = -1
	}
	oldIndent := s.blkIndent
	s.blkIndent = 0
	s.level++
	s.tokenize(startLine, nextLine)
	s.level--
	s.lineMax = oldLineMax
	s.parentType = oldParentType
	for i := range oldTShift {
		s.bMarks[i+startLine] = oldBMarks[i]
		s.tShift[i+startLine] = oldTShift[i]
		s.sCount[i+startLine] = oldSCount[i]
		s.bsCount[i+startLine] = oldBSCount[i]
	}
	s.blkIndent = oldIndent
	return true
}

func (s *blockState) hr(startLine, _ int, silent bool) bool {
	max := s.eMarks[startLine]
	if s.sCount[startLine]-s.blkIndent >= 4 {
		return false
	}
	pos := s.bMarks[startLine] + s.tShift[startLine]
	marker := s.at(pos)
	pos++
	if marker != '*' && marker != '-' && marker != '_' {
		return false
	}
	cnt := 1
	for ; pos < max; pos++ {
		ch := s.src[pos]
		if int(ch) != marker && !isSpaceTab(ch) {
			return false
		}
		if int(ch) == marker {
			cnt++
		}
	}
	if cnt < 3 {
		return false
	}
	if silent {
		return true
	}
	s.line = startLine + 1
	return true
}

func (s *blockState) skipBulletListMarker(line int) int {
	max := s.eMarks[line]
	pos := s.bMarks[line] + s.tShift[line]
	marker := s.at(pos)
	pos++
	if marker != '*' && marker != '-' && marker != '+' {
		return -1
	}
	if pos < max && !isSpaceTab(s.src[pos]) {
		return -1
	}
	return pos
}

func (s *blockState) skipOrderedListMarker(line int) int {
	start := s.bMarks[line] + s.tShift[line]
	max := s.eMarks[line]
	pos := start
	if pos+1 >= max {
		return -1
	}
	ch := s.src[pos]
	pos++
	if !isDigit(ch) {
		return -1
	}
	for {
		if pos >= max {
			return -1
		}
		ch = s.src[pos]
		pos++
		if isDigit(ch) {
			if pos-start >= 10 {
				return -1
			}
			continue
		}
		if ch == ')' || ch == '.' {
			break
		}
		return -1
	}
	if pos < max && !isSpaceTab(s.src[pos]) {
		return -1
	}
	return pos
}

func (s *blockState) list(startLine, endLine int, silent bool) bool {
	nextLine := startLine
	if s.sCount[nextLine]-s.blkIndent >= 4 {
		return false
	}
	if s.listIndent >= 0 && s.sCount[nextLine]-s.listIndent >= 4 && s.sCount[nextLine] < s.blkIndent {
		return false
	}
	isTerminatingParagraph := silent && s.parentType == "paragraph" && s.sCount[nextLine] >= s.blkIndent
	isOrdered := false
	posAfterMarker := s.skipOrderedListMarker(nextLine)
	if posAfterMarker >= 0 {
		isOrdered = true
		start := s.bMarks[nextLine] + s.tShift[nextLine]
		markerValue, _ := strconv.Atoi(s.src[start : posAfterMarker-1])
		if isTerminatingParagraph && markerValue != 1 {
			return false
		}
	} else if posAfterMarker = s.skipBulletListMarker(nextLine); posAfterMarker < 0 {
		return false
	}
	if isTerminatingParagraph && s.skipSpaces(posAfterMarker) >= s.eMarks[nextLine] {
		return false
	}
	if silent {
		return true
	}
	markerChar := s.src[posAfterMarker-1]
	oldParentType := s.parentType
	s.parentType = "list"
	s.level++
	for nextLine < endLine {
		pos := posAfterMarker
		max := s.eMarks[nextLine]
		initial := s.sCount[nextLine] + posAfterMarker - (s.bMarks[nextLine] + s.tShift[nextLine])
		offset := initial
		for ; pos < max; pos++ {
			if ch := s.src[pos]; ch == '\t' {
				offset += 4 - (offset+s.bsCount[nextLine])%4
				continue
			} else if ch != ' ' {
				break
			}
			offset++
		}
		contentStart := pos
		indentAfterMarker := offset - initial
		if contentStart >= max {
			indentAfterMarker = 1
		}
		if indentAfterMarker > 4 {
			indentAfterMarker = 1
		}
		indent := initial + indentAfterMarker
		s.level++
		oldTShift, oldSCount, oldListIndent := s.tShift[nextLine], s.sCount[nextLine], s.listIndent
		s.listIndent = s.blkIndent
		s.blkIndent = indent
		s.tShift[nextLine] = contentStart - s.bMarks[nextLine]
		s.sCount[nextLine] = offset
		if contentStart >= max && s.isEmpty(nextLine+1) {
			s.line = min(s.line+2, endLine)
		} else {
			s.tokenize(nextLine, endLine)
		}
		s.blkIndent = s.listIndent
		s.listIndent = oldListIndent
		s.tShift[nextLine] = oldTShift
		s.sCount[nextLine] = oldSCount
		s.level--
		nextLine = s.line
		if nextLine >= endLine || s.sCount[nextLine] < s.blkIndent || s.sCount[nextLine]-s.blkIndent >= 4 {
			break
		}
		if s.terminates("list", nextLine, endLine) {
			break
		}
		if isOrdered {
			posAfterMarker = s.skipOrderedListMarker(nextLine)
		} else {
			posAfterMarker = s.skipBulletListMarker(nextLine)
		}
		if posAfterMarker < 0 || markerChar != s.src[posAfterMarker-1] {
			break
		}
	}
	s.level--
	s.line = nextLine
	s.parentType = oldParentType
	return true
}

func (s *blockState) reference(startLine, _ int, silent bool) bool {
	pos := s.bMarks[startLine] + s.tShift[startLine]
	max := s.eMarks[startLine]
	nextLine := startLine + 1
	if s.sCount[startLine]-s.blkIndent >= 4 || s.at(pos) != '[' {
		return false
	}
	getNextLine := func(nl int) (string, bool) {
		endLine := s.lineMax
		if nl >= endLine || s.isEmpty(nl) {
			return "", false
		}
		isContinuation := s.sCount[nl]-s.blkIndent > 3 || s.sCount[nl] < 0
		if !isContinuation {
			oldParentType := s.parentType
			s.parentType = "reference"
			terminate := s.terminates("reference", nl, endLine)
			s.parentType = oldParentType
			if terminate {
				return "", false
			}
		}
		return s.src[s.bMarks[nl]+s.tShift[nl] : min(s.eMarks[nl]+1, len(s.src))], true
	}
	str := s.src[pos:min(max+1, len(s.src))]
	max = len(str)
	extend := func() {
		if lc, ok := getNextLine(nextLine); ok {
			str += lc
			max = len(str)
			nextLine++
		}
	}
	labelEnd := -1
	for pos = 1; pos < max; pos++ {
		switch str[pos] {
		case '[':
			return false
		case ']':
			labelEnd = pos
		case '\n':
			extend()
		case '\\':
			pos++
			if pos < max && str[pos] == '\n' {
				extend()
			}
		}
		if labelEnd >= 0 {
			break
		}
	}
	if labelEnd < 0 || labelEnd+1 >= len(str) || str[labelEnd+1] != ':' {
		return false
	}
	for pos = labelEnd + 2; pos < max; pos++ {
		ch := str[pos]
		if ch == '\n' {
			extend()
		} else if !isSpaceTab(ch) {
			break
		}
	}
	dest := parseLinkDestination(str, pos, max)
	if !dest.ok || !validateLink(dest.str) {
		return false
	}
	pos = dest.pos
	destEndPos, destEndLineNo := pos, nextLine
	start := pos
	for ; pos < max; pos++ {
		ch := str[pos]
		if ch == '\n' {
			extend()
		} else if !isSpaceTab(ch) {
			break
		}
	}
	title := parseLinkTitle(str, pos, max, nil)
	for title.canContinue {
		lc, ok := getNextLine(nextLine)
		if !ok {
			break
		}
		str += lc
		pos = max
		max = len(str)
		nextLine++
		title = parseLinkTitle(str, pos, max, &title)
	}
	hasTitle := false
	if pos < max && start != pos && title.ok {
		hasTitle = title.str != ""
		pos = title.pos
	} else {
		pos, nextLine = destEndPos, destEndLineNo
	}
	for pos < max && isSpaceTab(str[pos]) {
		pos++
	}
	if pos < max && str[pos] != '\n' && hasTitle {
		// Garbage after the title: the title is dropped and the
		// definition ends with its destination.
		pos, nextLine = destEndPos, destEndLineNo
		for pos < max && isSpaceTab(str[pos]) {
			pos++
		}
	}
	if pos < max && str[pos] != '\n' {
		return false
	}
	label := normalizeReference(str[1:labelEnd])
	if label == "" {
		return false
	}
	if silent {
		return true
	}
	if s.doc.refs == nil {
		s.doc.refs = map[string]string{}
	}
	if _, ok := s.doc.refs[label]; !ok {
		s.doc.refs[label] = dest.str
	}
	def := &definition{dest: dest.str}
	for l := startLine; l < nextLine; l++ {
		def.lines = append(def.lines, [3]int{s.lineStarts[l], s.bMarks[l] + s.tShift[l], s.eMarks[l]})
	}
	s.doc.defs = append(s.doc.defs, def)
	s.line = nextLine
	return true
}

func (s *blockState) heading(startLine, _ int, silent bool) bool {
	pos := s.bMarks[startLine] + s.tShift[startLine]
	max := s.eMarks[startLine]
	if s.sCount[startLine]-s.blkIndent >= 4 {
		return false
	}
	if s.at(pos) != '#' || pos >= max {
		return false
	}
	level := 1
	pos++
	ch := s.at(pos)
	for ch == '#' && pos < max && level <= 6 {
		level++
		pos++
		ch = s.at(pos)
	}
	if level > 6 || pos < max && !isSpaceTab(s.src[pos]) {
		return false
	}
	if silent {
		return true
	}
	max = s.skipSpacesBack(max, pos)
	if tmp := s.skipCharsBack(max, '#', pos); tmp > pos && isSpaceTab(s.src[tmp-1]) {
		max = tmp
	}
	s.line = startLine + 1
	s.addInline(asciiTrimText(sourceText(s.src, pos, max)))
	return true
}

func (s *blockState) lheading(startLine, endLine int) bool {
	if s.sCount[startLine]-s.blkIndent >= 4 {
		return false
	}
	oldParentType := s.parentType
	s.parentType = "paragraph"
	level := 0
	nextLine := startLine + 1
	for ; nextLine < endLine && !s.isEmpty(nextLine); nextLine++ {
		if s.sCount[nextLine]-s.blkIndent > 3 {
			continue
		}
		if s.sCount[nextLine] >= s.blkIndent {
			pos := s.bMarks[nextLine] + s.tShift[nextLine]
			max := s.eMarks[nextLine]
			if pos < max {
				if marker := s.src[pos]; marker == '-' || marker == '=' {
					pos = s.skipSpaces(s.skipChars(pos, marker))
					if pos >= max {
						level = 1
						break
					}
				}
			}
		}
		if s.sCount[nextLine] < 0 {
			continue
		}
		if s.terminates("paragraph", nextLine, endLine) {
			break
		}
	}
	if level == 0 {
		s.parentType = oldParentType
		return false
	}
	s.addInline(asciiTrimText(s.getLines(startLine, nextLine, s.blkIndent, false)))
	s.line = nextLine + 1
	s.parentType = oldParentType
	return true
}

func (s *blockState) paragraph(startLine, endLine int) bool {
	oldParentType := s.parentType
	nextLine := startLine + 1
	s.parentType = "paragraph"
	for ; nextLine < endLine && !s.isEmpty(nextLine); nextLine++ {
		if s.sCount[nextLine]-s.blkIndent > 3 || s.sCount[nextLine] < 0 {
			continue
		}
		if s.terminates("paragraph", nextLine, endLine) {
			break
		}
	}
	s.addInline(asciiTrimText(s.getLines(startLine, nextLine, s.blkIndent, false)))
	s.line = nextLine
	s.parentType = oldParentType
	return true
}
