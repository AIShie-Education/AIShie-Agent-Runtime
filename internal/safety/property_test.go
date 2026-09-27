package safety

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// fragments build bodies with no code in them, so that nothing of SECRET,
// which appears only in URLs, may be left once Body is done, whatever the
// fragments make together. Code, which Body leaves alone, needs backticks,
// a fence or four spaces at the start of a line, and none can form here.
// Neither can a backslash escape, which may leave a URL as harmless text
// (\[t](javascript:x) is no link); the table tests cover escapes.
var fragments = []string{
	"[t](https://e.example/?SECRET)", "[t](https://e.example/#SECRET)", "[t](javascript:SECRET)",
	"[t](<https://e.example/a b?SECRET>)", "[t](//e.example/?SECRET \"title\")", "[t](https://SECRET@e.example/)",
	"![a](https://e.example/SECRETSECRETSECRETSECRETSECRETSE)", "![](data:SECRET)", "[r]", "[r][]", "[t][r]",
	"[r]: https://e.example/?SECRET", "[r]:", "https://e.example/?SECRET", "\"https://e.example/#SECRET\"",
	"<https://e.example/?SECRET>", "<a href='https://e.example/?SECRET'>", "</a>", "<img src=\"https://e.example/?SECRET\" alt=x>",
	"www.e.example/?SECRET", "//e.example/?SECRET", "e.example/?SECRET", "https://e.example/&#63;SECRET",
	"[t](https://e.example/\\?SECRET)", "[t](https&#58;//e.example/?SECRET)",
	"[", "]", "(", ")", "!", "*", "_", "|", "\"", "'", ">", "text", "学习", "👩‍💻", "&amp;",
	"\n", "\n\n", "\n> ", "\n- ", "\n1. ", "\n# ", "\n| ", "[docs](https://docs.python.org/)", "https://docs.python.org/3/",
}

func TestNothingOfASecretSurvives(t *testing.T) {
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
		if strings.Contains(out, "SECRET") {
			t.Fatalf("a secret survived\n in: %q\nout: %q", in, out)
		}
		if limit > 0 && utf8.RuneCountInString(out) > limit {
			t.Fatalf("over the limit: %q", out)
		}
		if again, _ := Body(out, limit); again != out {
			t.Fatalf("not idempotent\n in: %q\nout: %q\nagain: %q", in, out, again)
		}
	}
}

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
