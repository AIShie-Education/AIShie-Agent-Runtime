package safety

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// fragments build bodies of links, near-links and the Markdown around them.
// Links carry the word SECRET; so does text that is no link.
var fragments = []string{
	"[t](https://e.example/?SECRET)", "[t](https://e.example/#SECRET)", "[t](javascript:SECRET)",
	"[t](<https://e.example/a b?SECRET>)", "[t](//e.example/?SECRET \"title\")", "[t](https://SECRET@e.example/)",
	"![a](https://e.example/SECRETSECRETSECRETSECRETSECRETSE)", "![](data:SECRET)", "[r]", "[r][]", "[t][r]",
	"[r]: https://e.example/?SECRET", "[r]:", "https://e.example/?SECRET", "\"https://e.example/#SECRET\"",
	"<https://e.example/?SECRET>", "<a href='https://e.example/?SECRET'>", "</a>", "<img src=\"https://e.example/?SECRET\" alt=x>",
	"www.e.example/?SECRET", "//e.example/?SECRET", "e.example/?SECRET", "https://e.example/&#63;SECRET",
	"[t](https://e.example/\\?SECRET)", "[t](https&#58;//e.example/?SECRET)", "x?SECRET@e.example", "SECRET@日本.jp",
	"[", "]", "(", ")", "!", "*", "_", "|", "\"", "'", ">", "text", "学习", "👩‍💻", "&amp;", "$", "$$", "`", "```", "~~",
	"\n", "\n\n", "\n> ", "\n- ", "\n1. ", "\n# ", "\n| ", "\n|---|\n", "\n    ", "\n```\n",
	"[docs](https://docs.python.org/)", "https://docs.python.org/3/",
}

// linksIn lists every URL the renderer would make a link or image of in s,
// as Body reads s, and more (see linkifyCandidates).
func linksIn(s string) []string {
	doc := parseBlocks(s)
	var urls []string
	for _, d := range doc.defs {
		urls = append(urls, d.dest)
	}
	for _, t := range doc.inlines {
		res := parseInline(t.s, doc.refs)
		for _, f := range res.found {
			urls = append(urls, f.url)
		}
		if !linkifyTest(t.s) {
			continue
		}
		for _, p := range joinPieces(res.pieces) {
			if !p.inLink {
				linkifyCandidates(t.s[p.start:p.end], func(_, _ int, url string) { urls = append(urls, url) })
			}
		}
	}
	return urls
}

// What Body returns holds no link that carries data, whatever the
// fragments make together, and is its own result. (That Body reads
// Markdown as the frontend's renderer does is held by the recorded cases
// of TestRenderer.)
func TestNothingCarryingSurvives(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for n := range 5000 {
		var b strings.Builder
		for range 1 + rng.IntN(24) {
			b.WriteString(fragments[rng.IntN(len(fragments))])
			if rng.IntN(3) == 0 {
				b.WriteByte(' ')
			}
		}
		in := b.String()
		limit := 0
		if n%4 == 0 {
			limit = 20 + rng.IntN(200)
		}
		out, _ := Body(in, limit)
		for _, u := range linksIn(out) {
			if carries(u) && !deadScheme(u) {
				t.Fatalf("a link carrying data survived: %q\n in: %q\nout: %q", u, in, out)
			}
		}
		if limit > 0 && utf8.RuneCountInString(out) > limit {
			t.Fatalf("over the limit: %q", out)
		}
		if again, _ := Body(out, limit); again != out {
			t.Fatalf("not idempotent\n in: %q\nout: %q\nagain: %q", in, out, again)
		}
	}
}

// deadScheme reports a URL the renderer refuses to link to, which Body
// strips all the same where it can find one, but which is no link.
func deadScheme(u string) bool { return !validateLink(u) }

func FuzzBody(f *testing.F) {
	for _, s := range fragments {
		f.Add(s, 0)
	}
	f.Add("```\n[x](https://e.example/?q)\n```\n[y](https://e.example/?q)", 50)
	f.Fuzz(func(t *testing.T, in string, limit int) {
		limit %= CoreMaxChars + 1
		out, rep := Body(in, limit)
		max := limit
		if max <= 0 {
			max = CoreMaxChars
		}
		if utf8.RuneCountInString(out) > max {
			t.Fatalf("over the limit %d: %q", max, out)
		}
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8: %q", out)
		}
		if rep.Empty != (out == "") {
			t.Fatalf("Empty %v for %q", rep.Empty, out)
		}
		if again, _ := Body(out, limit); again != out {
			t.Fatalf("not idempotent: %q then %q", out, again)
		}
		for _, u := range linksIn(out) {
			if carries(u) && !deadScheme(u) {
				t.Fatalf("a link carrying data survived: %q in %q", u, out)
			}
		}
	})
}

// A long, link-heavy answer, and a body built to be slow: Body reads at
// most maxInputChars of it.
func BenchmarkBody(b *testing.B) {
	answer := strings.Repeat("Loops repeat a block. See [the docs](https://docs.python.org/3/tutorial/controlflow.html) "+
		"and <https://evil.example/?q=1>, or `for x in xs:` in code.\n\n", 150)
	hostile := strings.Repeat("[](a", 32000)
	for _, in := range []struct {
		name, text string
	}{{"answer", answer}, {"hostile", hostile}} {
		b.Run(in.name, func(b *testing.B) {
			for b.Loop() {
				Body(in.text, 19000)
			}
		})
	}
}
