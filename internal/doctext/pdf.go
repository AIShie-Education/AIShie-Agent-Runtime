package doctext

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// A PDF's text: each page in order under "## Page N", its text as its
// content draws it (pdfcontent.go); a page with none says so. Whether the
// text reads as text is judged over the whole file (Result.Unreadable): a
// scanned file has none, and one whose fonts map their glyphs to nothing,
// or to one character over and over (a broken ToUnicode CMap), has text
// that is not the document's.

func readPDF(data []byte, b *budget) (*Result, error) {
	d, err := openPDF(data, b)
	if err != nil {
		return nil, err
	}
	pages, total := d.pages(b.lim.MaxParts)
	if d.err != nil {
		return nil, d.err
	}
	res := &Result{Format: PDF, Of: total}
	out := newTextOut(b.lim.MaxText)
	q := newQuality()
	empty, stopped := 0, false
	for i, pg := range pages {
		if out.cut {
			break
		}
		pt := d.runPage(pg, b.lim.MaxText)
		if err := d.err; err != nil {
			if !cutShort(err) || res.Parts == 0 && strings.TrimSpace(pt.sb.String()) == "" {
				return nil, err
			}
			// The pages read, and what this one gave before the end.
			res.Notes = append(res.Notes, restNote(err))
			if text := cleanText(pt.sb.String()); text != "" {
				res.Parts++
				q.add(text)
				out.section(SectionPage, i+1, fmt.Sprintf("## Page %d", i+1))
				out.line(text)
			}
			stopped = true
			break
		}
		res.Parts++
		res.Images += pt.images
		text := cleanText(pt.sb.String())
		q.add(text)
		out.section(SectionPage, i+1, fmt.Sprintf("## Page %d", i+1))
		if text == "" {
			empty++
			out.line("[no text on this page]")
			continue
		}
		out.line(text)
	}
	if res.Parts < total && !out.cut && !stopped {
		res.Notes = append(res.Notes, fmt.Sprintf("only its first %d pages of %d are given", res.Parts, total))
	}
	res.Unreadable = q.judge()
	if res.Unreadable == "" && empty > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d of its %d pages have no text: they may be scanned, or pictures of text", empty, res.Parts))
	}
	out.finish(res)
	return res, nil
}

// PDFPages is how many pages data, a PDF, has: its structure read within
// lim and ctx, and none of its content. A file that needs a password to
// open is ErrEncrypted.
func PDFPages(ctx context.Context, data []byte, lim Limits) (n int, err error) {
	if recoverPanics {
		defer func() {
			if r := recover(); r != nil {
				n, err = 0, fmt.Errorf("%w: it is malformed", ErrMalformed)
			}
		}()
	}
	d, err := openPDF(data, newBudget(ctx, lim))
	if err != nil {
		return 0, err
	}
	_, total := d.pages(0)
	if d.err != nil {
		return 0, d.err
	}
	return total, nil
}

// cleanText is a page's text with control characters but line breaks and
// tabs taken out, each line's trailing spaces trimmed, and no more than one
// empty line in a row.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == '\r' {
			return '\n'
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// quality measures whether a PDF's text reads as text.
type quality struct {
	pages, withText int
	// visible are the characters that are not white space; bad those
	// that stand for nothing (U+FFFD, private use, controls); letters the
	// letters and digits; inRuns those in runs of five or more of the
	// same character.
	visible, bad, letters, inRuns int
	counts                        map[rune]int
	// scripts count the letters of each script, but Latin's and those
	// written with ideographs, over the first letters of the file.
	scripts map[string]int
	sampled int
}

func newQuality() *quality {
	return &quality{counts: map[rune]int{}, scripts: map[string]int{}}
}

// maxSampled is how many letters the scripts are counted over.
const maxSampled = 20000

// otherScripts are the scripts a glyph mapped to the wrong code lands in
// when its font's codes are taken for Unicode's: a real text mixes few of
// them.
var otherScripts = []struct {
	name  string
	table *unicode.RangeTable
}{
	{"Cyrillic", unicode.Cyrillic}, {"Armenian", unicode.Armenian}, {"Hebrew", unicode.Hebrew},
	{"Arabic", unicode.Arabic}, {"Syriac", unicode.Syriac}, {"Thaana", unicode.Thaana}, {"Nko", unicode.Nko},
	{"Devanagari", unicode.Devanagari}, {"Bengali", unicode.Bengali}, {"Gurmukhi", unicode.Gurmukhi},
	{"Gujarati", unicode.Gujarati}, {"Oriya", unicode.Oriya}, {"Tamil", unicode.Tamil}, {"Telugu", unicode.Telugu},
	{"Kannada", unicode.Kannada}, {"Malayalam", unicode.Malayalam}, {"Sinhala", unicode.Sinhala},
	{"Thai", unicode.Thai}, {"Lao", unicode.Lao}, {"Tibetan", unicode.Tibetan}, {"Myanmar", unicode.Myanmar},
	{"Georgian", unicode.Georgian}, {"Ethiopic", unicode.Ethiopic}, {"Cherokee", unicode.Cherokee},
	{"Canadian_Aboriginal", unicode.Canadian_Aboriginal}, {"Ogham", unicode.Ogham}, {"Runic", unicode.Runic},
	{"Khmer", unicode.Khmer}, {"Mongolian", unicode.Mongolian}, {"Yi", unicode.Yi}, {"Coptic", unicode.Coptic},
	{"Glagolitic", unicode.Glagolitic}, {"Tifinagh", unicode.Tifinagh},
}

// add counts a page's text.
func (q *quality) add(text string) {
	q.pages++
	if strings.TrimSpace(text) != "" {
		q.withText++
	}
	var prev rune
	run := 0
	flush := func() {
		if run >= 5 {
			q.inRuns += run
		}
	}
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		q.visible++
		if r == prev {
			run++
		} else {
			flush()
			prev, run = r, 1
		}
		if len(q.counts) < 1<<16 || q.counts[r] > 0 {
			q.counts[r]++
		}
		switch {
		case r == '�' || unicode.Is(unicode.Co, r) || unicode.IsControl(r):
			q.bad++
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			q.letters++
			if q.sampled < maxSampled && r >= 0x80 && !unicode.In(r, unicode.Latin, unicode.Han, unicode.Hiragana,
				unicode.Katakana, unicode.Hangul, unicode.Bopomofo, unicode.Greek, unicode.Common) {
				q.sampled++
				for _, s := range otherScripts {
					if unicode.Is(s.table, r) {
						q.scripts[s.name]++
						break
					}
				}
			}
		}
	}
	flush()
}

// judge says why the text does not read as text, or "" when it does.
func (q *quality) judge() string {
	if q.visible == 0 || q.withText*10 < q.pages && q.letters < 200 {
		return UnreadableNoText
	}
	top := 0
	for _, n := range q.counts {
		top = max(top, n)
	}
	switch v := float64(q.visible); {
	case float64(q.bad) > 0.2*v:
		// Glyphs the fonts map to nothing, or to private use.
		return UnreadableUnmapped
	case q.visible >= 40 && float64(top) > 0.4*v, q.visible >= 10 && float64(top) > 0.6*v, q.visible >= 5 && float64(q.inRuns) > 0.5*v:
		// One character over and over: a map that gives every glyph the
		// same text.
		return UnreadableUnmapped
	case q.visible >= 40 && float64(q.letters) < 0.2*v:
		// Punctuation and symbols where the words should be.
		return UnreadableUnmapped
	}
	if q.letters >= 40 {
		mixed := 0
		for _, n := range q.scripts {
			if float64(n) >= 0.05*float64(min(q.letters, maxSampled)) {
				mixed++
			}
		}
		if mixed >= 2 {
			// Letters of several unrelated scripts at once: glyph numbers
			// taken for characters.
			return UnreadableUnmapped
		}
	}
	return ""
}
