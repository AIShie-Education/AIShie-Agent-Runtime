package transcribe

import (
	"strings"
	"unicode/utf8"
)

// What the model wrote of a range of pages is made the range's text
// (clean), and the ranges' texts one document's, cut to what Core keeps
// (fit).

// clean is a model's text of pages first on: without a code fence around
// the whole of it, which some models add whatever they are told, and
// beginning with the first page's heading, which it is given where the
// model wrote none; "" when the model wrote nothing.
func clean(text string, first int, slides bool) string {
	text = strings.TrimSpace(strings.ToValidUTF8(text, "�"))
	if open, rest, ok := strings.Cut(text, "\n"); ok && strings.HasPrefix(open, "```") && !strings.ContainsAny(strings.TrimPrefix(open, "```"), " `") {
		if body, ok := strings.CutSuffix(strings.TrimSpace(rest), "```"); ok {
			text = strings.TrimSpace(body)
		}
	}
	if text == "" {
		return ""
	}
	if line, _, _ := strings.Cut(text, "\n"); !isHeading(strings.TrimSpace(line)) {
		text = heading(first, slides) + "\n\n" + text
	}
	return text
}

// fit is a document's text within limit bytes: whole when it fits, and
// otherwise cut before the last page's heading that leaves room for
// TooLongLine after it (or, where no heading does, after the last whole
// line that does, or at a character), the line saying the rest is left
// out.
func fit(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	tail := "\n\n" + TooLongLine + "\n"
	room := limit - len(tail)
	if room <= 0 {
		return TooLongLine
	}
	cut := -1
	for i := strings.LastIndex(text[:room], "\n## "); i > 0; i = strings.LastIndex(text[:i], "\n## ") {
		if line, _, _ := strings.Cut(text[i+1:], "\n"); isHeading(line) {
			cut = i
			break
		}
	}
	if cut < 0 {
		cut = strings.LastIndexByte(text[:room], '\n')
	}
	if cut <= 0 {
		cut = room
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
	}
	return strings.TrimRight(text[:cut], "\n ") + tail
}
