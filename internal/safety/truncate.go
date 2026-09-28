package safety

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// cut shortens s to at most limit characters, Ellipsis included. It cuts
// at the last paragraph break, else after the last sentence's end, else at
// the last space, within the limit, and only where that keeps at least
// half of what fits; else it cuts at the limit itself, never inside a
// character or between a character and what combines with it.
func cut(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	if limit <= 1 {
		return Ellipsis
	}
	keep := limit - 1 // characters before the ellipsis
	head := s[:runeOffset(s, keep)]
	least := runeOffset(s, keep/2)
	// A paragraph cut ends with the ellipsis on a line of its own, so
	// that it never joins a closing fence or a table.
	if limit > 3 {
		paraHead := s[:runeOffset(s, limit-3)]
		if i := strings.LastIndex(paraHead, "\n\n"); i >= least {
			return strings.TrimRightFunc(s[:i], unicode.IsSpace) + "\n\n" + Ellipsis
		}
	}
	at := lastSentenceEnd(s, len(head))
	if at < least {
		at = strings.LastIndexAny(head, " \t\n")
	}
	if at < least {
		at = clusterBoundary(s, len(head))
	}
	return strings.TrimRightFunc(s[:at], unicode.IsSpace) + Ellipsis
}

// runeOffset is the byte offset of s's n-th character, or len(s).
func runeOffset(s string, n int) int {
	for i := range s {
		if n == 0 {
			return i
		}
		n--
	}
	return len(s)
}

// sentence enders that need white space after them, and those that do not
// (CJK full stops end a sentence whatever follows).
const (
	asciiEnders = ".!?"
	closers     = `"')]”’」』）】`
)

var cjkEnders = []string{"。", "！", "？", "．", "｡"}

// lastSentenceEnd is the offset just after the last sentence end before
// limit, or -1: '.', '!' or '?' (and any closing quotes or brackets after
// it) followed by white space, or a CJK full stop.
func lastSentenceEnd(s string, limit int) int {
	best := -1
	for _, e := range cjkEnders {
		if i := strings.LastIndex(s[:limit], e); i >= 0 {
			best = max(best, i+len(e))
		}
	}
	for i := limit - 1; i > best; i-- {
		if strings.IndexByte(asciiEnders, s[i]) < 0 {
			continue
		}
		j := i + 1
		for j < limit {
			r, n := utf8.DecodeRuneInString(s[j:])
			if !strings.ContainsRune(closers, r) {
				break
			}
			j += n
		}
		if j <= limit && j < len(s) && isHTMLSpace(s[j]) {
			return j
		}
	}
	return best
}

// clusterBoundary moves the offset at back to where no character that
// combines with the one before it is cut off: combining marks, joiners,
// variation selectors, emoji modifiers and tags, the second of a pair of
// regional indicators (a flag), Hangul vowels and finals.
func clusterBoundary(s string, at int) int {
	start := at
	for at > 0 {
		r, _ := utf8.DecodeRuneInString(s[at:])
		prev, n := utf8.DecodeLastRuneInString(s[:at])
		switch {
		case at < len(s) && extends(r), prev == '‍':
			at -= n
		case isRegionalIndicator(prev) && regionalRunBefore(s, at)%2 == 1 && at < len(s) && isRegionalIndicator(r):
			at -= n
		default:
			return at
		}
	}
	return start // nothing but one cluster: cut where asked
}

func extends(r rune) bool {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc):
		return true
	case r == '‍', r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF: // joiner, variation selectors
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF, r >= 0xE0020 && r <= 0xE007F: // skin tones, tags
		return true
	case r >= 0x1160 && r <= 0x11FF, r >= 0xD7B0 && r <= 0xD7FF: // Hangul vowels and finals
		return true
	case r == 0xFF9E, r == 0xFF9F: // half-width sound marks
		return true
	}
	return false
}

func isRegionalIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// regionalRunBefore counts the regional indicators just before at.
func regionalRunBefore(s string, at int) int {
	n := 0
	for at > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:at])
		if !isRegionalIndicator(r) {
			break
		}
		n++
		at -= size
	}
	return n
}
