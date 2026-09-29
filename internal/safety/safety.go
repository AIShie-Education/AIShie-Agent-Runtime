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
// Answers are shown by AIShie-Frontend (src/utils/markdown.ts):
// markdown-it 15 with html off, linkify on (linkify-it 6.1.0 and its
// defaults) and TeX through KaTeX with trust off. Body reads Markdown
// exactly as that renderer does, with a port of its parsing rules
// (mdblock.go, mdinline.go, linkify.go), so that it strips every link the
// renderer would make and leaves code, TeX and everything else as it was.
// A change to that renderer or its options must be matched here.
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
// Every link and image the renderer would make whose URL carries context
// is stripped: one whose href would have a query string, a fragment, user
// information, a scheme other than http, https and mailto (data:,
// javascript:, file:, …), any percent-encoding (the renderer encodes
// spaces, characters that are not ASCII and some punctuation), a path
// segment or host label holding 32 or more characters of [A-Za-z0-9+/=_-]
// (data spelled out), or, for mailto, an address that does (see carries).
// The URL is judged as the renderer reads it: escapes and entities undone
// in a destination, as written in a bare URL. A link keeps its text
// ([text](url) becomes text); an image becomes its alt text, or
// ImageRemoved without one; a bare URL, an email address or an autolink
// <url> becomes LinkRemoved; a reference definition ([x]: url) is removed
// and its uses become plain text; an HTML <a href> becomes its text, an
// <img> its alt, and any other tag with such a URL goes. A link the
// renderer refuses only for its scheme (javascript: and the like) is
// stripped too. Code spans, fenced and indented code blocks and TeX are
// left untouched: nothing in them is clickable.
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
	// markdown-it's normalize rule: its line endings, and no NUL.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\x00", "\uFFFD")
	// Twice what Core takes is more than any answer that can be posted;
	// reading no more bounds the work a hostile body can cause.
	capped := false
	if utf8.RuneCountInString(s) > maxInputChars {
		s, capped = s[:runeOffset(s, maxInputChars)], true
	}
	s = clean(s, &rep)
	if capped && s != "" && utf8.RuneCountInString(s) < maxChars {
		s = clean(s+Ellipsis, &rep)
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
		s = clean(cut(s, limit), &rep)
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

// clean strips s and trims it until neither changes it. Trimming can
// change how the renderer reads what is left (a U+3000 at the end of a
// table's delimiter row keeps it from being one; trimmed, the row makes a
// table whose cells cut a code span open), so what is trimmed is read
// again.
func clean(s string, rep *Report) string {
	for range maxPasses {
		t := trim(strip(s, rep))
		if t == s {
			return t
		}
		s = t
	}
	return ""
}

// trim removes trailing white space and the blank lines (spaces and tabs
// only, as the renderer reads them) at the start. The first line with text
// keeps its indentation, which may make it code. A body of white space
// alone is empty: Core refuses it.
func trim(s string) string {
	s = strings.TrimRightFunc(s, unicode.IsSpace)
	for {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 || strings.Trim(s[:nl], " \t") != "" {
			break
		}
		s = s[nl+1:]
	}
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}
