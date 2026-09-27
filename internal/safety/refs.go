package safety

import (
	"strings"
)

// definitions are the body's link reference definitions, by normalised
// label.
type definitions struct {
	clean   map[string]bool
	removed map[string]bool
}

// defined reports whether label names a definition.
func (d *definitions) defined(label string) bool {
	k := normalizeLabel(label)
	return d.clean[k] || d.removed[k]
}

// stripped reports whether label's uses are to become plain text: its
// definitions were all removed. (With one removed and one kept, the
// renderer finds the kept one.)
func (d *definitions) stripped(label string) bool {
	k := normalizeLabel(label)
	return d.removed[k] && !d.clean[k]
}

// normalizeLabel matches labels as CommonMark does, near enough: case
// folded, white space collapsed. A use it fails to match keeps its
// brackets, and with its definition removed the renderer links nothing.
func normalizeLabel(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// collectDefinitions finds every link reference definition among the lines
// that are not code, and marks for removal those whose destination carries
// context. It reads more lines as definitions than CommonMark does (inside
// block quotes and list items, anywhere in a paragraph), since a line that
// is not one but starts "[label]: url-with-data" loses nothing it should
// keep. A line that starts like a definition but does not parse as one is
// removed too when its first word, the would-be destination, carries
// context: whatever the renderer makes of it, that URL goes.
func collectDefinitions(lines []line, rep *Report) *definitions {
	d := &definitions{clean: map[string]bool{}, removed: map[string]bool{}}
	for i := 0; i < len(lines); i++ {
		if lines[i].code || lines[i].drop {
			continue
		}
		label, rest, ok := definitionStart(lines[i].text)
		if !ok {
			continue
		}
		def, ok := parseDefinition(lines, i, rest)
		if !ok {
			continue
		}
		k := normalizeLabel(label)
		if !carriesContext(def.dest) {
			if def.valid {
				d.clean[k] = true
			}
			continue
		}
		d.removed[k] = true
		rep.LinksRemoved++
		for j := i; j <= def.last; j++ {
			lines[j].drop = true
		}
		i = def.last
	}
	return d
}

// definitionStart reads "[label]:" at the start of s, after any block
// quote markers, list markers and indentation, and returns the label and
// what follows the colon.
func definitionStart(s string) (label, rest string, ok bool) {
	t := stripContainers(s)
	if !strings.HasPrefix(t, "[") {
		return "", "", false
	}
	end, label, ok := linkLabel(t, 0)
	if !ok || strings.TrimSpace(label) == "" || end >= len(t) || t[end] != ':' {
		return "", "", false
	}
	return label, t[end+1:], true
}

// stripContainers removes the block quote and list markers and the
// indentation that begin s.
func stripContainers(s string) string {
	for {
		t := strings.TrimLeft(s, " \t")
		switch {
		case strings.HasPrefix(t, ">"):
			s = t[1:]
		case len(t) > 1 && strings.IndexByte("-+*", t[0]) >= 0 && (t[1] == ' ' || t[1] == '\t'):
			s = t[2:]
		default:
			if n := orderedMarker(t); n > 0 {
				s = t[n:]
				continue
			}
			return t
		}
	}
}

// orderedMarker is the length of an ordered list marker ("12. ") at the
// start of s, or 0.
func orderedMarker(s string) int {
	i := 0
	for i < len(s) && i < 9 && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i+1 >= len(s) || (s[i] != '.' && s[i] != ')') || (s[i+1] != ' ' && s[i+1] != '\t') {
		return 0
	}
	return i + 2
}

// definition is a parsed link reference definition.
type definition struct {
	dest string
	// last is the index of its last line.
	last int
	// valid is false for a line that only starts like a definition.
	valid bool
}

// parseDefinition reads the destination and optional title of a
// definition whose "[label]:" is on line i, followed by rest. The
// destination may be on the next line, and the title on the line after
// the destination; the title may run over several lines.
func parseDefinition(lines []line, i int, rest string) (definition, bool) {
	last := i
	if isBlank(rest) {
		// The destination is on the next line.
		if i+1 >= len(lines) || lines[i+1].code || isBlank(lines[i+1].text) {
			return definition{}, false
		}
		last = i + 1
		rest = stripContainers(lines[last].text)
	}
	rest = strings.TrimLeft(rest, " \t")
	dest, after, ok := definitionDest(rest)
	if !ok {
		// Not a destination CommonMark reads; judge the first word.
		first := strings.Fields(rest)
		if len(first) == 0 {
			return definition{}, false
		}
		return definition{dest: strings.Trim(first[0], "<>"), last: last}, true
	}
	def := definition{dest: dest, last: last, valid: true}
	spaced := strings.TrimLeft(after, " \t")
	switch {
	case spaced == "":
		// A title may follow on the next line.
		if l := last + 1; l < len(lines) && !lines[l].code {
			if t := strings.TrimLeft(stripContainers(lines[l].text), " \t"); t != "" && strings.IndexByte(`"'(`, t[0]) >= 0 {
				if end, ok := titleEnd(lines, l, t); ok {
					def.last = end
				}
			}
		}
	case len(spaced) < len(after) && strings.IndexByte(`"'(`, spaced[0]) >= 0:
		// A title on the same line, after white space.
		end, ok := titleEnd(lines, last, spaced)
		if !ok {
			def.valid = false
		} else {
			def.last = end
		}
	default:
		def.valid = false
	}
	return def, true
}

// definitionDest reads a destination at the start of s: <…>, or a run
// without spaces or control characters whose parentheses balance.
func definitionDest(s string) (dest, after string, ok bool) {
	if strings.HasPrefix(s, "<") {
		for j := 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case '<':
				return "", "", false
			case '>':
				return s[1:j], s[j+1:], true
			}
		}
		return "", "", false
	}
	depth, j := 0, 0
	for ; j < len(s); j++ {
		c := s[j]
		if c == '\\' && j+1 < len(s) && isASCIIPunct(s[j+1]) {
			j++
			continue
		}
		if c <= ' ' || c == 0x7f {
			break
		}
		if c == '(' {
			depth++
		}
		if c == ')' {
			if depth == 0 {
				break
			}
			depth--
		}
	}
	if j == 0 || depth != 0 {
		return "", "", false
	}
	return s[:j], s[j:], true
}

// titleEnd finds where a title that begins t, on line l, closes, and
// reports the line it closes on; nothing but white space may follow it.
func titleEnd(lines []line, l int, t string) (int, bool) {
	closer := t[0]
	if closer == '(' {
		closer = ')'
	}
	s := t[1:]
	for ; l < len(lines); l++ {
		for j := 0; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case closer:
				return l, isBlank(s[j+1:])
			}
		}
		if l+1 >= len(lines) || lines[l+1].code || isBlank(lines[l+1].text) {
			return 0, false
		}
		s = lines[l+1].text
	}
	return 0, false
}
