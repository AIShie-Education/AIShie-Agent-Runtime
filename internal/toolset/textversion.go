package toolset

import (
	"context"
	"strconv"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/core"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
)

// A version's text version (design §4, Files; Core #43) is the document's
// text as Core keeps it beside the file: an AI transcription of it, page by
// page under fixed headings (the runtime's transcriber's, package
// transcribe, or another's), or what the course's staff wrote or corrected.
// Where it is done, a model is given it in place of the runtime's own
// reading of the file, in parts as a long text is (file_part), and told
// whose it is; a model that takes files may ask for pages of the file
// itself (FilePagesArg) to check one against it. A text version not done
// (pending, working, failed, skipped) changes nothing: the file is given as
// before. Whether this runtime transcribes has no bearing on it.
//
// A text version read in parts (document_text, with the caller's own
// token, which reads the text exactly where it reads the version) is kept
// (Runner.Texts) under its version and revision, which every change of the
// text moves on; the worker drops a version's kept text as Core's events
// say it changed (DropText).

// maxTextParts bounds the parts of a text version read: Core keeps at most
// 2 MiB of text, in parts of at most 64 KiB.
const maxTextParts = 40

// textSource says whose a text version is, as the record says it.
func textSource(tv *core.TextView) string {
	if tv.Source == core.SourceStaff {
		return "edited by staff"
	}
	if tv.Model != "" {
		return "AI transcription (" + tv.Model + ")"
	}
	return "AI transcription"
}

// giveTextVersion gives the version's text version where it is done, and
// reports whether it did: not where the version has none, it is not done,
// or it cannot be read now, and the file is given as before.
func (r Runner) giveTextVersion(ctx context.Context, g given, d *docFile) (given, bool) {
	if d.text == nil || d.text.Status != core.TextDone || d.versionID == "" {
		return g, false
	}
	rd := r.textVersion(ctx, d)
	if rd == nil || rd.res == nil || strings.TrimSpace(rd.res.Text) == "" {
		return g, false
	}
	tv := rd.text
	rec := g.rec
	rec.GivenAs, rec.TextSource = givenText, textSource(tv)
	var b strings.Builder
	b.WriteString("file_text is the document's text version, which Core keeps beside the file: ")
	if tv.Source == core.SourceStaff {
		b.WriteString("written or corrected by the course's staff")
	} else {
		b.WriteString("an AI transcription of the file")
		if tv.Model != "" {
			b.WriteString(" by " + tv.Model)
		}
		b.WriteString(", which may hold mistakes")
	}
	if tv.Pages > 0 {
		b.WriteString(", of " + strconv.Itoa(tv.Pages) + " pages")
	}
	b.WriteString("; each page is under a heading of its own, \"## 第 N 頁\" (a slide's \"## 投影片 N\"), and each picture is described " +
		"in brackets, \"[圖：…]\"")
	if r.FileInput {
		b.WriteString("; to see pages of the file itself, to check one against the text, call " + FilePartTool + " again with " +
			FilePagesArg + ", such as \"3-5\"")
	}
	rec.Note = b.String()
	g.text, g.sections = rd.res.Text, rd.res.Sections
	return g, true
}

// textVersion is the version's text version, done, as kept, or read now:
// its body where Core gave it whole beside the version, and otherwise its
// parts, read at one revision (a text that changes while its parts are
// read is read again, once); nil where it cannot be read.
func (r Runner) textVersion(ctx context.Context, d *docFile) *fileReading {
	tv := d.text
	if rd := r.Texts.get(textVersionKey(d.versionID, tv.Revision)); rd != nil {
		return rd
	}
	body, view := "", tv
	if tv.Body != nil {
		v := *tv
		body, v.Body = *tv.Body, nil
		view = &v
	} else {
		var ok bool
		if body, view, ok = r.readTextParts(ctx, d); !ok {
			return nil
		}
	}
	rd := &fileReading{mt: "text/markdown", size: int64(len(body)), text: view,
		res: &doctext.Result{Text: body, Sections: headingSections(body)}}
	r.Texts.put(textVersionKey(d.versionID, view.Revision), rd)
	return rd
}

// readTextParts reads the version's text version a part at a time
// (document_text), at one revision: the text, and the text version it is.
func (r Runner) readTextParts(ctx context.Context, d *docFile) (string, *core.TextView, bool) {
	if r.Client == nil || d.courseID == "" || d.documentID == "" {
		return "", nil, false
	}
	for range 2 {
		var b strings.Builder
		var view *core.TextView
		again := false
		for part, parts := 1, 1; part <= parts; part++ {
			tp, err := r.Client.TextPart(ctx, d.courseID, d.documentID, d.versionID, part)
			if err != nil || tp.Text.Status != core.TextDone || tp.Parts < 1 || tp.Parts > maxTextParts {
				return "", nil, false
			}
			if part == 1 {
				parts, view = tp.Parts, &tp.Text
			} else if tp.Text.Revision != view.Revision {
				again = true
				break
			}
			if tp.Text.Body != nil {
				b.WriteString(*tp.Text.Body)
			}
		}
		if !again {
			v := *view
			v.Body = nil
			return b.String(), &v, true
		}
	}
	return "", nil, false
}

// textVersionKey is what a version's text version is kept under: apart
// from the readings of files (textKey), by its version and revision.
func textVersionKey(versionID string, revision int) string {
	return textVersionPrefix(versionID) + strconv.Itoa(revision)
}

// textVersionPrefix begins the keys of a version's text versions.
func textVersionPrefix(versionID string) string { return "text\x00" + versionID + "\x00" }

// headingSections are where a text version's pages and slides begin: at
// its headings, "## 第 N 頁" and "## 投影片 N", as the transcriber writes
// them (and staff keep them), which its parts are cut on.
func headingSections(text string) []doctext.Section {
	var out []doctext.Section
	for off := 0; off < len(text); {
		line, _, _ := strings.Cut(text[off:], "\n")
		if kind, n, ok := pageHeading(strings.TrimRight(line, "\r ")); ok && (len(out) == 0 || off > out[len(out)-1].Offset) {
			out = append(out, doctext.Section{Kind: kind, N: n, Offset: off})
		}
		off += len(line) + 1
	}
	return out
}

// pageHeading reads a page's or a slide's heading: its kind and number.
func pageHeading(line string) (string, int, bool) {
	kind, rest := doctext.SectionPage, ""
	switch {
	case strings.HasPrefix(line, "## 第 ") && strings.HasSuffix(line, " 頁"):
		rest = strings.TrimSuffix(strings.TrimPrefix(line, "## 第 "), " 頁")
	case strings.HasPrefix(line, "## 投影片 "):
		kind, rest = doctext.SectionSlide, strings.TrimPrefix(line, "## 投影片 ")
	default:
		return "", 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil || n < 1 {
		return "", 0, false
	}
	return kind, n, true
}

// DropText forgets what is kept of the version's text versions: the worker
// calls it as Core's events say the version's text changed (core.
// TextEvents), though a changed text is kept under its revision apart.
func (c *TextCache) DropText(versionID string) {
	if c == nil || versionID == "" {
		return
	}
	prefix := textVersionPrefix(versionID)
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.byKey {
		if strings.HasPrefix(key, prefix) {
			c.size -= e.Value.(*cachedReading).cost
			c.lru.Remove(e)
			delete(c.byKey, key)
		}
	}
}
