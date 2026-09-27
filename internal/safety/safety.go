// Package safety makes what a model wrote safe to post as an answer (Core's
// docs/agent-runtime.md §6.1 "Exfiltration through Markdown"; design §7).
//
// Core's frontend loads no image from another origin and opens links with
// rel="noopener noreferrer nofollow", but a click still sends the URL. An
// injection that gets the model to put what it read into a URL, as a query,
// a fragment, a path that spells it out, or a scheme that runs code, would
// take it to whoever owns the URL. So every link and image whose URL carries
// context is stripped, and one that carries nothing stays as written.
//
// The renderer is Markdown with GitHub's extensions (bare URLs become links)
// or something close to it. Where renderers differ, Body takes the reading
// in which more is a link: stripping a link that would not have been one
// costs a little text, and missing one that would have been costs the data.
// Only code is left alone, since nothing in it is clickable, and only where
// every renderer agrees that it is code.
package safety

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// CoreMaxChars is the most characters Core takes in a message body. Core
// counts characters as Unicode code points (utf8.RuneCountInString in its
// internal/tools/conversation.go), and so does Body.
const CoreMaxChars = 20000

// What stripped links and images become.
const (
	// LinkRemoved replaces a bare URL or an autolink that carried context.
	LinkRemoved = "[link removed]"
	// ImageRemoved replaces an image that carried context and had no alt
	// text.
	ImageRemoved = "[image]"
	// Ellipsis ends a body that was cut.
	Ellipsis = "…"
)

// Report says what Body did.
type Report struct {
	// LinksRemoved and ImagesRemoved count the links and images stripped:
	// each inline link, reference definition, use of a removed definition,
	// autolink, bare URL and HTML tag.
	LinksRemoved  int
	ImagesRemoved int
	// Truncated is true when the body was cut to fit.
	Truncated bool
	// Empty is true when nothing is left to post.
	Empty bool
}

// Body returns text made safe to post and cut to at most maxChars
// characters, with a report of what was done.
//
// Every link and image whose URL carries context is stripped: one with a
// query string, a fragment, user information, a scheme other than http,
// https and mailto (data:, javascript:, file:, …), any percent-encoding, a
// path segment or host label holding 32 or more characters of
// [A-Za-z0-9+/=_-] (data spelled out), or, for mailto, an address that
// does. HTML entities, backslash escapes and the tabs and newlines browsers
// drop are undone before a URL is judged, so that none of them hides
// anything. A link keeps its text ([text](url) becomes text); an image
// becomes its alt text, or ImageRemoved without one; a bare URL or an
// autolink <url> becomes LinkRemoved; a reference definition ([x]: url) is
// removed and its uses become plain text; an HTML <a href> becomes its
// text, an <img> its alt, and any other tag with such a URL goes. Inline
// code spans and fenced and indented code blocks are left untouched: they
// are not clickable. That holds only where every renderer agrees they are
// code: a span on one line with no '|' (a table cell's boundary), a block
// not cut short by the list item or HTML block it is in (see classify).
// Elsewhere what looks like code is stripped like text.
//
// Line endings become \n. A body longer than maxChars (at most, and by
// default, CoreMaxChars) is cut at its last paragraph break, else its last
// sentence end, else its last space, within the limit, and Ellipsis is
// added within the limit too. Leading blank lines and trailing space are
// trimmed; Empty is set when nothing is left, and the body is then "".
func Body(text string, maxChars int) (string, Report) {
	if maxChars <= 0 || maxChars > CoreMaxChars {
		maxChars = CoreMaxChars
	}
	var rep Report
	s := strings.ToValidUTF8(text, "\uFFFD")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	// Twice what Core takes is more than any answer that can be posted;
	// reading no more bounds the work a hostile body can cause.
	capped := false
	if utf8.RuneCountInString(s) > maxInputChars {
		s, capped = s[:runeOffset(s, maxInputChars)], true
	}
	s = trim(strip(s, &rep))
	if capped && s != "" && utf8.RuneCountInString(s) < maxChars {
		s += Ellipsis
		rep.Truncated = true
	}
	// Cutting can open a code span or fence it closed, exposing what was
	// code, so what is cut is stripped again; stripping can make it longer,
	// so the limit is lowered each round until the two agree.
	for reserve := 0; utf8.RuneCountInString(s) > maxChars; reserve = 2*reserve + 16 {
		rep.Truncated = true
		limit := maxChars - reserve
		if limit < 1 {
			s = ""
			break
		}
		s = trim(strip(cut(s, limit), &rep))
	}
	rep.Empty = s == ""
	return s, rep
}

// maxInputChars is as much of a body as Body reads.
const maxInputChars = 2 * CoreMaxChars

// maxPasses bounds strip. Each pass takes out at least one construct, and
// only removing one can make another; a body that is still changing after
// this many passes was built to defeat them, and nothing of it is posted.
const maxPasses = 8

// strip runs pass until nothing changes.
func strip(s string, rep *Report) string {
	for range maxPasses {
		out := pass(s, rep)
		if out == s {
			return out
		}
		s = out
	}
	return ""
}

// trim removes trailing white space and leading blank lines. The first
// line with text keeps its indentation, which may make it code.
func trim(s string) string {
	s = strings.TrimRightFunc(s, unicode.IsSpace)
	for {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 || strings.TrimSpace(s[:nl]) != "" {
			break
		}
		s = s[nl+1:]
	}
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

// pass strips s once: it finds the code, removes the reference definitions
// that carry context, and rewrites each paragraph's inline links.
func pass(s string, rep *Report) string {
	lines := splitLines(s)
	classify(lines)
	defs := collectDefinitions(lines, rep)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(lines); {
		l := lines[i]
		switch {
		case l.drop:
			i++
		case l.code || isBlank(l.text):
			b.WriteString(l.text)
			if l.nl {
				b.WriteByte('\n')
			}
			i++
		default:
			// A paragraph: the text lines up to a blank line or code,
			// without the definitions removed from among them.
			var para []string
			nl := false
			for ; i < len(lines) && !lines[i].code && !isBlank(lines[i].text); i++ {
				if !lines[i].drop {
					para = append(para, lines[i].text)
					nl = lines[i].nl
				}
			}
			b.WriteString(inline(strings.Join(para, "\n"), defs, rep))
			if nl {
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}
