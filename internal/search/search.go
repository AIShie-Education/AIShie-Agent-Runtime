// Package search is how the runtime finds passages in a course's
// documents (docs/design.md §4, Search): the terms a text is indexed and
// queried by, the passages a file's text is cut into, and how a passage is
// scored against a query and shown.
//
// Terms are the runtime's own, the same in every store and every locale.
// PostgreSQL's full-text search keeps a run of Chinese as one word, and
// pg_trgm finds no word in it under a C ctype and little under a UTF-8
// one, so neither finds 排序 in 合併排序的複雜度. Here a text is folded
// (NFKC, lower case), and cut into words of the alphabetic scripts and,
// in the scripts written without spaces (Han, kana, Hangul), into every
// two characters side by side and every character alone: the bigrams
// that Lucene's CJK analyser indexes, which find a word of two characters
// or more wherever it is written, and the characters, which find a word of
// one. A query of several characters is matched by its bigrams.
package search

import (
	"math"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
)

// MaxQueryRunes bounds a query: a phrase or a few words, not a passage.
const MaxQueryRunes = 200

// MaxQueryTerms bounds the terms a query is matched by.
const MaxQueryTerms = 32

// MaxTermBytes bounds one term: a longer word is cut, so that a run of
// letters with no space (a URL, a hash) is not a term of kilobytes.
const MaxTermBytes = 64

// fold is r as terms are made of it: its compatibility form (a full-width
// letter or digit is the ASCII one, a ligature its letters), in lower case.
func fold(r rune) string {
	if r < utf8.RuneSelf {
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		return string(r)
	}
	return strings.ToLower(norm.NFKC.String(string(r)))
}

// class is what a character is to the terms.
type class int

const (
	// other separates words.
	other class = iota
	// word is a letter, a digit or a mark of a script written with spaces.
	word
	// ideo is a character of a script written without them: each is a
	// term, and so is each pair side by side.
	ideo
)

func classOf(r rune) class {
	switch {
	case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Bopomofo),
		// Written in those runs, though Unicode counts them common to
		// several scripts: the long vowel mark of katakana, and the marks
		// that repeat a character or close a word.
		r == 'ー', r == '々', r == '〆':
		return ideo
	case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r):
		return word
	}
	return other
}

// Terms are the terms of text, in order, repeats kept: its words, folded
// and their plurals taken off (stem), and of its runs of Han, kana and
// Hangul each character and each pair of characters side by side.
func Terms(text string) []string {
	var out []string
	var w strings.Builder
	var prev string // the ideograph before, folded, "" after anything else
	flush := func() {
		if w.Len() > 0 {
			out = append(out, stem(cutTerm(w.String())))
			w.Reset()
		}
	}
	for _, r := range text {
		for _, f := range fold(r) {
			switch classOf(f) {
			case ideo:
				flush()
				c := string(f)
				if prev != "" {
					out = append(out, prev+c)
				}
				out = append(out, c)
				prev = c
			case word:
				prev = ""
				w.WriteRune(f)
			default:
				prev = ""
				flush()
			}
		}
	}
	flush()
	return out
}

// cutTerm cuts a word past MaxTermBytes, on a rune boundary.
func cutTerm(s string) string {
	if len(s) <= MaxTermBytes {
		return s
	}
	n := MaxTermBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// stem takes the plural off an English word, so that "merges" finds
// "merge" and "queries" "query": the endings alone, never a guess at any
// other form, and only of words of ASCII letters longer than three.
func stem(w string) string {
	if len(w) <= 3 || strings.IndexFunc(w, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0 {
		return w
	}
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses"), strings.HasSuffix(w, "xes"), strings.HasSuffix(w, "zes"),
		strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "shes"):
		return w[:len(w)-2]
	case strings.HasSuffix(w, "ss"), strings.HasSuffix(w, "us"), strings.HasSuffix(w, "is"):
		return w
	case strings.HasSuffix(w, "s"):
		return w[:len(w)-1]
	}
	return w
}

// stopwords are English words too common to find anything by: a query
// drops them when it has other terms.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true, "doe": true, "does": true,
	"do": true, "for": true, "from": true, "how": true, "in": true, "is": true, "it": true, "of": true, "on": true, "or": true,
	"that": true, "the": true, "thi": true, "this": true, "to": true, "wa": true, "was": true, "what": true, "when": true,
	"where": true, "which": true, "who": true, "why": true, "with": true,
}

// QueryTerms are the distinct terms a query is matched by, in the order
// they come: of a run of two characters or more of Han, kana or Hangul its
// pairs, of one character the character; its words, but for stopwords
// and single letters where it has other terms; at most MaxQueryTerms.
func QueryTerms(q string) []string {
	var all, kept []string
	seen := map[string]bool{}
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			all = append(all, t)
		}
	}
	terms := Terms(q)
	for i, t := range terms {
		if !isIdeoTerm(t) || utf8.RuneCountInString(t) == 2 {
			add(t)
			continue
		}
		// A character alone: a term only when it stands alone in the
		// query, with no pair of it before or after.
		pairBefore := i > 0 && isIdeoTerm(terms[i-1]) && utf8.RuneCountInString(terms[i-1]) == 2
		pairAfter := i+1 < len(terms) && isIdeoTerm(terms[i+1]) && utf8.RuneCountInString(terms[i+1]) == 2
		if !pairBefore && !pairAfter {
			add(t)
		}
	}
	for _, t := range all {
		if !stopwords[t] && (utf8.RuneCountInString(t) > 1 || isIdeoTerm(t) || isDigits(t)) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		kept = all
	}
	if len(kept) > MaxQueryTerms {
		kept = kept[:MaxQueryTerms]
	}
	return kept
}

func isIdeoTerm(t string) bool {
	r, _ := utf8.DecodeRuneInString(t)
	return classOf(r) == ideo
}

func isDigits(t string) bool {
	return t != "" && strings.IndexFunc(t, func(r rune) bool { return !unicode.IsDigit(r) }) < 0
}

// Distinct are terms once each, sorted bytewise: what a passage is indexed
// by.
func Distinct(terms []string) []string {
	out := slices.Clone(terms)
	slices.Sort(out)
	return slices.Compact(out)
}

// Chunk is one passage of a text: text[Start:End], on the slide, page or
// sheet Kind N (doctext's sections; "" and 0 before the first, or in a
// text with none).
type Chunk struct {
	Start, End int
	Kind       string
	N          int
}

// DefaultChunkBytes is about how long a passage is: a slide's text, or a
// few paragraphs of a page, enough to say what it is about and short
// enough to point at.
const DefaultChunkBytes = 1500

// Chunks cut text into passages, in order, side by side: one for each
// slide, page or sheet (sections), and one of the text before the first;
// one longer than size bytes cut where a paragraph ends, else a line, else
// between two characters. Together they are text, nothing given twice,
// but for passages of nothing but spaces, which are left out.
func Chunks(text string, sections []doctext.Section, size int) []Chunk {
	size = max(size, 64)
	type unit struct {
		start, end int
		kind       string
		n          int
	}
	var units []unit
	start, kind, n := 0, "", 0
	for _, s := range sections {
		if s.Offset <= start || s.Offset > len(text) {
			if s.Offset == start {
				kind, n = s.Kind, s.N
			}
			continue
		}
		units = append(units, unit{start, s.Offset, kind, n})
		start, kind, n = s.Offset, s.Kind, s.N
	}
	units = append(units, unit{start, len(text), kind, n})
	var out []Chunk
	for _, u := range units {
		for s := u.start; s < u.end; {
			e := cutAt(text, s, u.end, size)
			if strings.TrimSpace(text[s:e]) != "" {
				out = append(out, Chunk{Start: s, End: e, Kind: u.kind, N: u.n})
			}
			s = e
		}
	}
	return out
}

// cutAt is where the passage of text that begins at start, within end,
// ends: at end when it fits size, else after the last empty line, line or
// character that fits, of the first of these kinds that ends past half of
// size.
func cutAt(text string, start, end, size int) int {
	if end-start <= size {
		return end
	}
	limit := start + size
	for limit > start && !utf8.RuneStart(text[limit]) {
		limit--
	}
	window := text[start:limit]
	for _, sep := range []string{"\n\n", "\n"} {
		if i := strings.LastIndex(window, sep); i >= 0 && i+len(sep) >= size/2 {
			return start + i + len(sep)
		}
	}
	if limit == start {
		_, w := utf8.DecodeRuneInString(text[start:])
		return start + w
	}
	return limit
}

// Scorer scores passages against a query by Okapi BM25 over the passages
// searched: DF is how many of them hold each term, N how many there are,
// and AvgLen their mean length in terms.
type Scorer struct {
	DF     map[string]int
	N      int
	AvgLen float64
}

// BM25's constants, as Lucene and Elasticsearch set them.
const (
	k1 = 1.2
	b  = 0.75
)

// Score is how well a passage of text matches the query's terms (q, as
// QueryTerms gives them) and phrase (the query as it was written): BM25,
// times how many of the terms it holds of all (a passage with every word
// of the question beats one with a rare one alone), and half again where
// it holds the phrase itself as it was written, spaces aside. 0 for a
// passage that holds none of them.
func (s Scorer) Score(q []string, phrase, text string) float64 {
	if len(q) == 0 {
		return 0
	}
	terms := Terms(text)
	tf := map[string]int{}
	want := map[string]bool{}
	for _, t := range q {
		want[t] = true
	}
	for _, t := range terms {
		if want[t] {
			tf[t]++
		}
	}
	if len(tf) == 0 {
		return 0
	}
	avg := s.AvgLen
	if avg <= 0 {
		avg = float64(len(terms))
	}
	n := float64(max(s.N, 1))
	var score float64
	for _, t := range q {
		f := float64(tf[t])
		if f == 0 {
			continue
		}
		df := float64(s.DF[t])
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		score += idf * f * (k1 + 1) / (f + k1*(1-b+b*float64(len(terms))/max(avg, 1)))
	}
	score *= 0.5 + 0.5*float64(len(tf))/float64(len(q))
	if p := squeeze(phrase); p != "" && strings.Contains(squeeze(text), p) {
		score *= 1.5
	}
	return score
}

// squeeze is s folded, with only its letters, digits and ideographs: the
// phrase of a query as it is looked for in a passage, whatever spaces and
// punctuation either has.
func squeeze(s string) string {
	var out strings.Builder
	for _, r := range s {
		for _, f := range fold(r) {
			if classOf(f) != other {
				out.WriteRune(f)
			}
		}
	}
	return out.String()
}

// Excerpt is a short piece of text, at most size runes and an ellipsis at
// each end it is cut at, around where most of the query's terms (q) are
// found together, its spaces and lines run together as one space: what a
// result shows of a passage. Without a term in it, it is the passage's
// beginning.
func Excerpt(text string, q []string, size int) string {
	size = max(size, 16)
	runes := []rune(text)
	if len(runes) <= size {
		return collapse(text)
	}
	// Where each term of q begins, in runes.
	want := map[string]bool{}
	for _, t := range q {
		want[t] = true
	}
	var hits []int
	folded := make([]string, len(runes))
	for i, r := range runes {
		folded[i] = fold(r)
	}
	for i := range runes {
		for _, t := range q {
			if matchAt(folded, i, t) {
				hits = append(hits, i)
				break
			}
		}
	}
	start := 0
	if len(hits) > 0 {
		// The window of size runes that begins a little before a hit and
		// holds the most hits; the first such.
		best, bestN := hits[0], 0
		for i, h := range hits {
			j := sort.SearchInts(hits, h+size*3/4)
			if j-i > bestN {
				best, bestN = h, j-i
			}
		}
		start = max(best-size/4, 0)
	}
	end := start + size
	if end > len(runes) {
		end = len(runes)
		start = max(end-size, 0)
	}
	out := collapse(string(runes[start:end]))
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

// matchAt reports whether the term t begins at rune i of the folded text:
// a word at a word's start, an ideograph or a pair of them anywhere.
func matchAt(folded []string, i int, t string) bool {
	if !strings.HasPrefix(t, folded[i]) {
		return false
	}
	var b strings.Builder
	for j := i; j < len(folded) && b.Len() < len(t); j++ {
		b.WriteString(folded[j])
	}
	if !strings.HasPrefix(b.String(), t) {
		return false
	}
	if isIdeoTerm(t) {
		return true
	}
	if i > 0 {
		r, _ := utf8.DecodeRuneInString(folded[i-1])
		if classOf(r) == word {
			return false
		}
	}
	return true
}

// collapse runs a text's spaces and lines together as one space, and
// trims it.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
