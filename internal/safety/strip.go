package safety

import (
	"sort"
	"strings"
)

// edit replaces text[start:end] of one inline run with repl.
type edit struct {
	start, end int
	repl       string
}

// pass strips s once: it reads s as the renderer does, and takes out every
// link and image whose URL carries data, every reference definition that
// does, and the tags design §7 names. Code and TeX are never touched.
func pass(s string, rep *Report) string {
	doc := parseBlocks(s)
	out := &rewrite{src: s, deleted: make([]bool, len(s)), inserts: map[int]string{}}
	for _, d := range doc.defs {
		if carries(d.dest) {
			rep.LinksRemoved++
			out.removeDefinition(d)
		}
	}
	for _, t := range doc.inlines {
		out.apply(t, inlineEdits(t.s, doc.refs, rep))
	}
	return out.String()
}

// inlineEdits finds what to take out of one inline run.
func inlineEdits(src string, refs map[string]string, rep *Report) []edit {
	res := parseInline(src, refs)
	var edits, urls []edit
	keepText := func(f found) {
		edits = append(edits, edit{f.start, f.textStart, ""}, edit{f.textEnd, f.end, ""})
	}
	for _, f := range res.found {
		switch f.kind {
		case kLink, kDeadLink:
			if f.kind == kDeadLink || carries(f.url) {
				rep.LinksRemoved++
				keepText(f)
			}
		case kImage, kDeadImage:
			if f.kind == kDeadImage || carries(f.url) {
				rep.ImagesRemoved++
				if strings.TrimSpace(src[f.textStart:f.textEnd]) == "" {
					edits = append(edits, edit{f.start, f.end, ImageRemoved})
				} else {
					keepText(f)
				}
			}
		case kAutolink, kLinkify, kDeadAutolink:
			if f.kind == kDeadAutolink || carries(f.url) {
				urls = append(urls, edit{f.start, f.end, LinkRemoved})
			}
		}
	}
	linkify := linkifyTest(src)
	for _, p := range joinPieces(res.pieces) {
		if p.inLink || !linkify {
			continue
		}
		t := src[p.start:p.end]
		linkifyCandidates(t, func(start, end int, url string) {
			if !carries(url) {
				return
			}
			// What is judged may take in emphasis delimiters around the
			// link (see linkifyCandidates); they stay, and are no link.
			s, e := start, end
			for s < e && isDelimiter(t[s]) {
				s++
			}
			for e > s && isDelimiter(t[e-1]) {
				e--
			}
			if s == e {
				s, e = start, end
			}
			urls = append(urls, edit{p.start + s, p.start + e, LinkRemoved})
		})
	}
	urls = mergeEdits(urls)
	rep.LinksRemoved += len(urls)
	edits = append(edits, urls...)
	// A tag taken out counts once, as a link or an image, whatever URLs
	// in it were counted already.
	openA := 0
	htmlTags(src, res.code, &openA, func(start, end int, repl string, counts int) {
		for _, u := range urls {
			if u.start >= start && u.end <= end {
				rep.LinksRemoved--
			}
		}
		switch counts {
		case countLink:
			rep.LinksRemoved++
		case countImage:
			rep.ImagesRemoved++
		}
		edits = append(edits, edit{start, end, repl})
	})
	return mergeEdits(edits)
}

func isDelimiter(c byte) bool { return c == '*' || c == '_' || c == '~' }

// joinPieces joins pieces that touch and share a link level: the pending
// text on either side of an emphasis or strikethrough delimiter, and the
// delimiter itself.
func joinPieces(ps []piece) []piece {
	sort.SliceStable(ps, func(a, b int) bool { return ps[a].start < ps[b].start })
	var out []piece
	for _, p := range ps {
		if n := len(out); n > 0 && out[n-1].end == p.start && out[n-1].inLink == p.inLink {
			out[n-1].end = p.end
			continue
		}
		out = append(out, p)
	}
	return out
}

// mergeEdits sorts edits and joins those that overlap, keeping the first
// one's replacement: removing more is safe, and nothing is written twice.
func mergeEdits(es []edit) []edit {
	if len(es) < 2 {
		return es
	}
	sort.SliceStable(es, func(a, b int) bool { return es[a].start < es[b].start })
	out := es[:1]
	for _, e := range es[1:] {
		last := &out[len(out)-1]
		if e.start < last.end {
			last.end = max(last.end, e.end)
			continue
		}
		out = append(out, e)
	}
	return out
}

// rewrite collects deletions and insertions in the body.
type rewrite struct {
	src     string
	deleted []bool
	inserts map[int]string
}

// apply maps edits of an inline run back to the body. What the run read
// from the body is deleted, but its line breaks, which keep the block
// quote markers and indentation of the lines after them in place.
func (w *rewrite) apply(t text, edits []edit) {
	for _, e := range edits {
		at := -1
		for k := e.start; k < e.end; k++ {
			o := t.off[k]
			if o < 0 {
				continue
			}
			if at < 0 {
				at = o
			}
			if t.s[k] != '\n' {
				w.deleted[o] = true
			}
		}
		if e.repl == "" {
			continue
		}
		if at < 0 {
			at = w.nearest(t, e.start)
		}
		w.inserts[at] += e.repl
	}
}

// nearest is the body offset of the first byte at or after k that came
// from the body, or of the last before it.
func (w *rewrite) nearest(t text, k int) int {
	for i := k; i < len(t.off); i++ {
		if t.off[i] >= 0 {
			return t.off[i]
		}
	}
	for i := min(k, len(t.off)) - 1; i >= 0; i-- {
		if t.off[i] >= 0 {
			return t.off[i] + 1
		}
	}
	return len(w.src)
}

// removeDefinition deletes a definition's lines: whole lines at the top
// level or in a block quote, and only the content of a list item's first
// line, whose marker stays. (The item's content then begins further left,
// so an indented code block under it may keep a few more spaces: anything
// put in the definition's place would be a paragraph, which would take
// that code in as text.)
func (w *rewrite) removeDefinition(d *definition) {
	for _, l := range d.lines {
		lineStart, from, to := l[0], l[1], l[2]
		if strings.Trim(w.src[lineStart:from], " \t>") == "" {
			from = lineStart
			if to < len(w.src) && w.src[to] == '\n' {
				to++
			}
		}
		for i := from; i < to; i++ {
			w.deleted[i] = true
		}
	}
}

func (w *rewrite) String() string {
	var b strings.Builder
	b.Grow(len(w.src))
	for i := 0; i <= len(w.src); i++ {
		if s, ok := w.inserts[i]; ok {
			b.WriteString(s)
		}
		if i < len(w.src) && !w.deleted[i] {
			b.WriteByte(w.src[i])
		}
	}
	return b.String()
}
