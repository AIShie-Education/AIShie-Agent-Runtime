package search

import (
	"cmp"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/doctext"
)

// TestTerms: words of the alphabetic scripts, folded and their plurals
// taken off; of Han, kana and Hangul every character and every pair side
// by side; full-width letters and digits as ASCII; punctuation between.
func TestTerms(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"Merge sort splits the LIST in two", []string{"merge", "sort", "split", "the", "list", "in", "two"}},
		{"合併排序", []string{"合", "合併", "併", "併排", "排", "排序", "序"}},
		{"第三週：O(n log n)", []string{"第", "第三", "三", "三週", "週", "o", "n", "log", "n"}},
		{"ＡＢＣ１２３ queries classes", []string{"abc123", "query", "class"}},
		{"Python的list", []string{"python", "的", "list"}},
		{"ソート算法", []string{"ソ", "ソー", "ー", "ート", "ト", "ト算", "算", "算法", "法"}},
		{"", nil},
	} {
		if got := Terms(c.text); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Terms(%q) = %q, want %q", c.text, got, c.want)
		}
	}
	long := strings.Repeat("x", 100)
	if got := Terms(long); len(got) != 1 || len(got[0]) != MaxTermBytes {
		t.Errorf("a long word: %q", got)
	}
}

// TestQueryTerms: a phrase of Chinese is matched by its pairs, a
// character alone by itself; English stopwords and single letters go
// where anything else is left, and stay where nothing is.
func TestQueryTerms(t *testing.T) {
	for _, c := range []struct {
		q    string
		want []string
	}{
		{"排序的複雜度", []string{"排序", "序的", "的複", "複雜", "雜度"}},
		{"序", []string{"序"}},
		{"What is the complexity of merge sort?", []string{"complexity", "merge", "sort"}},
		{"the", []string{"the"}},
		{"O(n log n) 排序", []string{"log", "排序"}},
		{"week 3", []string{"week", "3"}},
		{"sort sort SORT", []string{"sort"}},
	} {
		if got := QueryTerms(c.q); !reflect.DeepEqual(got, c.want) {
			t.Errorf("QueryTerms(%q) = %q, want %q", c.q, got, c.want)
		}
	}
	if n := len(QueryTerms(strings.Repeat("字", 100))); n != 1 {
		t.Errorf("one character over and over: %d terms", n)
	}
	var many []string
	for i := range 50 {
		many = append(many, "word"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if n := len(QueryTerms(strings.Join(many, " "))); n != MaxQueryTerms {
		t.Errorf("fifty words: %d terms", n)
	}
}

// TestChunks: a passage a slide or a page, the text before the first one
// apart; a long one cut at a paragraph's end past half the size; the
// passages side by side, together the text.
func TestChunks(t *testing.T) {
	text := "Intro\n\n## Slide 1: Sorting\nMerge sort\n\n## Slide 2: 排序\n" + strings.Repeat("合併排序。\n", 40) + "\n" + strings.Repeat("快速排序。\n", 40)
	s1 := strings.Index(text, "## Slide 1")
	s2 := strings.Index(text, "## Slide 2")
	sections := []doctext.Section{{Kind: doctext.SectionSlide, N: 1, Offset: s1}, {Kind: doctext.SectionSlide, N: 2, Offset: s2}}
	got := Chunks(text, sections, 400)
	if got[0] != (Chunk{Start: 0, End: s1}) || got[1] != (Chunk{Start: s1, End: s2, Kind: "slide", N: 1}) {
		t.Fatalf("the first passages: %+v", got[:2])
	}
	at := 0
	for i, c := range got {
		if c.Start != at || c.End <= c.Start || c.End-c.Start > 400 {
			t.Errorf("passage %d: %+v (after %d)", i, c, at)
		}
		if i >= 2 && (c.Kind != "slide" || c.N != 2) {
			t.Errorf("passage %d is not on slide 2: %+v", i, c)
		}
		at = c.End
	}
	if at != len(text) {
		t.Errorf("the passages end at %d of %d", at, len(text))
	}
	for _, c := range got[2:] {
		if s := text[c.Start:c.End]; c.End != len(text) && !strings.HasSuffix(s, "\n") {
			t.Errorf("a passage cut inside a line: %q", s[len(s)-10:])
		}
	}

	if got := Chunks("   \n\n", nil, 100); len(got) != 0 {
		t.Errorf("spaces alone: %+v", got)
	}
	// No line to cut at: between two characters, never inside one.
	han := strings.Repeat("字", 100)
	for _, c := range Chunks(han, nil, 100) {
		if (c.End-c.Start)%3 != 0 {
			t.Errorf("cut inside a character: %+v", c)
		}
	}
}

// TestScore: BM25 over the passages searched, a rare term above a common
// one, every term above some, the phrase as written above its words
// apart; a Traditional Chinese question finds the passage that answers
// it, and an English one its own.
func TestScore(t *testing.T) {
	passages := []string{
		"## 投影片 2\n排序的複雜度\n合併排序：O(n log n)，最壞情況也是 O(n log n)",
		"## 投影片 3\n快速排序平均 O(n log n)，最壞情況 O(n²)",
		"Merge sort splits the list in two, then merges the halves.",
		"Quicksort picks a pivot; the list is sorted around it.",
		"The syllabus lists the weeks of the course and the exam dates.",
	}
	rank := func(q string) []int {
		qt := QueryTerms(q)
		df := map[string]int{}
		total := 0
		for _, p := range passages {
			terms := Terms(p)
			total += len(terms)
			for _, term := range Distinct(terms) {
				for _, w := range qt {
					if term == w {
						df[w]++
					}
				}
			}
		}
		sc := Scorer{DF: df, N: len(passages), AvgLen: float64(total) / float64(len(passages))}
		scores := map[int]float64{}
		var out []int
		for i, p := range passages {
			if s := sc.Score(qt, q, p); s > 0 {
				scores[i] = s
				out = append(out, i)
			}
		}
		slices.SortStableFunc(out, func(a, b int) int { return cmp.Compare(scores[b], scores[a]) })
		return out
	}
	if got := rank("合併排序的複雜度"); len(got) == 0 || got[0] != 0 {
		t.Errorf("合併排序的複雜度 ranks %v", got)
	}
	if got := rank("最壞情況"); len(got) != 2 {
		t.Errorf("最壞情況 is in two passages: %v", got)
	}
	if got := rank("How does merge sort split the list?"); len(got) == 0 || got[0] != 2 {
		t.Errorf("merge sort ranks %v", got)
	}
	if got := rank("exam dates"); len(got) != 1 || got[0] != 4 {
		t.Errorf("exam dates ranks %v", got)
	}
	if got := rank("photosynthesis"); len(got) != 0 {
		t.Errorf("a word in no passage: %v", got)
	}
	sc := Scorer{DF: map[string]int{"merge": 1, "sort": 1}, N: 10, AvgLen: 10}
	if a, b := sc.Score([]string{"merge", "sort"}, "merge sort", "merge sort and more"), sc.Score([]string{"merge", "sort"}, "merge sort", "sort and merge more"); a <= b {
		t.Errorf("the phrase as written %v, its words apart %v", a, b)
	}
}

// TestExcerpt: a short piece around where the terms are, with an
// ellipsis where it is cut, its lines run together; a passage that fits
// whole.
func TestExcerpt(t *testing.T) {
	text := strings.Repeat("前言。", 60) + "\n合併排序的複雜度是 O(n log n)。\n" + strings.Repeat("結語。", 60)
	got := Excerpt(text, QueryTerms("合併排序"), 40)
	if !strings.Contains(got, "合併排序的複雜度") || !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") ||
		len([]rune(got)) > 42 || strings.Contains(got, "\n") {
		t.Errorf("Excerpt: %q", got)
	}
	if got := Excerpt("Merge sort\n\nsplits   the list", QueryTerms("list"), 100); got != "Merge sort splits the list" {
		t.Errorf("a passage that fits: %q", got)
	}
	long := "Sorting. " + strings.Repeat("filler words here. ", 30) + "Merge sort splits the list in two." + strings.Repeat(" more filler.", 30)
	if got := Excerpt(long, QueryTerms("merge sort"), 60); !strings.Contains(got, "Merge sort splits") {
		t.Errorf("English: %q", got)
	}
	if got := Excerpt(long, QueryTerms("absent"), 20); !strings.HasPrefix(got, "Sorting.") {
		t.Errorf("no term in it: %q", got)
	}
}
