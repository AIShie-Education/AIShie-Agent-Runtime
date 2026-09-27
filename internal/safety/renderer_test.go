package safety

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// testdata/renderer.json holds generated bodies of links, near-links and the
// Markdown around them (code, TeX, tables, lists, block quotes), each with
// Body's output, as checked when they were recorded against
// AIShiteru-Frontend's own renderer (src/utils/markdown.ts: markdown-it
// 15.0.2, linkify-it 6.1.0, KaTeX, DOMPurify): how many links and images
// carrying data the renderer made of the input and of the output (none),
// and whether the output's code and TeX read as the input's. A change to
// Body that changes an output must be checked against the renderer again.
type rendererCase struct {
	In       string `json:"in"`
	Limit    int    `json:"limit"`
	Out      string `json:"out"`
	Renderer struct {
		CarryingIn  int  `json:"carrying_in"`
		CarryingOut int  `json:"carrying_out"`
		CodeKept    bool `json:"code_kept"`
	} `json:"renderer"`
}

func TestRenderer(t *testing.T) {
	b, err := os.ReadFile("testdata/renderer.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []rendererCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	stripped := 0
	for i, c := range cases {
		if c.Renderer.CarryingOut != 0 {
			t.Fatalf("case %d was recorded with a link carrying data in its output", i)
		}
		got, rep := Body(c.In, c.Limit)
		if got != c.Out {
			t.Errorf("case %d: Body(%q, %d)\n got %q\nwant %q", i, c.In, c.Limit, got, c.Out)
			continue
		}
		if c.Renderer.CarryingIn > 0 {
			stripped++
			if rep.LinksRemoved+rep.ImagesRemoved == 0 {
				t.Errorf("case %d: the renderer made %d links carrying data, and Body reports none removed", i, c.Renderer.CarryingIn)
			}
		}
	}
	if stripped < len(cases)/3 {
		t.Fatalf("only %d of %d cases had links to strip", stripped, len(cases))
	}
}

func TestCarries(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://docs.python.org/3/library/json.html", false},
		{"http://localhost:8080/x", false},
		{"/courses/1", false},
		{"", false},
		{"mailto:ta@example.edu", false},
		{"https://en.wikipedia.org/wiki/Foo_(bar)", false},
		{"https://日本.jp/", false}, // punycode: xn--wgv71a.jp
		{"https://[::1]/docs", false},
		{"https://e.example/?", true},
		{"https://e.example/#top", true},
		{"https://e.example/a%20b", true},
		{"https://e.example/a b", true},
		{"https://e.example/東京", true},
		{"https://e.example/a|b", true},
		{"https://e.example/a`b", true},
		{"https://e.example/a\\b", true},
		{"https://u@e.example/", true},
		{"https://u:p@e.example/", true},
		{"//u@e.example/", true},
		{"///u@e.example/", true},
		{"http:u@e.example/", true},
		{"HTTPS://日本.jp/", true}, // not written http: or https:, so percent-encoded
		{"https://e.example👩‍💻$$/", true},
		{"mailto:x@e.example👩‍💻$$", true},
		{"javascript:alert(1)", true},
		{"data:image/png;base64,iVBOR", true},
		{"ftp://e.example/", true},
		{"foo:bar", true},
		{"-:8080", false}, // no scheme to a browser: a relative path
		{"https://e.example/" + data32, true},
		{"https://e.example/" + data31, false},
		{"https://" + data32 + ".e.example/", true},
		{"//" + data31 + "=", true}, // mdurl ends the host at '='; a browser does not
		{"https://学生の成績はＢマイナスで番号は四二.e.example/", true},
		{"mailto:" + data32 + "@e.example", true},
		{"mailto:" + data31 + "/path", true},
		{"mailto:x@e.example?subject=hi", true},
		{"https://e.example/" + strings.Repeat("a/", 1100), true},
		{"https://" + strings.Repeat("a.", 130) + "com/", true},
	} {
		if got := carries(tc.url); got != tc.want {
			t.Errorf("carries(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestPunycode(t *testing.T) {
	// Vectors from punycode.js, which markdown-it uses.
	for in, want := range map[string]string{
		"日本":           "wgv71a",
		"東京":           "1lqs71d",
		"münchen":      "mnchen-3ya",
		"秘秘秘秘秘秘秘秘秘秘秘秘": "tmzaaaaaaaaaaa",
		"学生の成績はＢマイナスで番号は四二": "n9jndb7xoe0e0gm94yillvwhquoy7ut54dn6a755gx517b",
		"👩‍💻": "1ug1855pcha",
	} {
		if got := punycode(in); got != want {
			t.Errorf("punycode(%q) = %q, want %q", in, got, want)
		}
	}
	if got := punycodeHost("日本。jp"); got != "xn--wgv71a.jp" {
		t.Errorf("punycodeHost = %q", got)
	}
}

// linkify-it's matching, case by case: what markdown-it's inline rule
// takes as a link at the start of the text.
func TestLinkifyMatchAtStart(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://e.example/a.b, and", "https://e.example/a.b"},
		{"https://e.example/a?b=1?", "https://e.example/a?b=1"},
		{"https://e.example/(a)b) c", "https://e.example/(a)b"},
		{"https://e.example/[a]b] c", "https://e.example/[a]b"},
		{"https://e.example/\"q\"x", "https://e.example/\"q\"x"},
		{"https://e.example/\"q x", "https://e.example/"},
		{"https://e.example/it's", "https://e.example/it's"},
		{"https://e.example/a...b", "https://e.example/a...b"},
		{"https://e.example/a..", "https://e.example/a"},
		{"https://e.example/a!!b", "https://e.example/a!!b"},
		{"https://e.example/a--", "https://e.example/a--"},
		{"https://e.example/a;", "https://e.example/a"},
		{"https://e.example/a<b", "https://e.example/a"},
		{"https://e.example/a。b", "https://e.example/a"},
		{"https://e.example/a日本", "https://e.example/a日本"},
		{"https://e.example:8080/x", "https://e.example:8080/x"},
		{"https://e.example:65535/", "https://e.example:65535/"},
		{"https://e.example:65536/", ""},
		{"https://e.example_x/", ""},
		{"https://e-.example/", ""},
		{"https://grades@e.example/", "https://grades"},
		{"https://localhost/x", "https://localhost/x"},
		{"https://[::1]:8080/x", "https://[::1]:8080/x"},
		{"https://日本.jp/x", "https://日本.jp/x"},
		{"http:e.example", ""},
		{"mailto:x.y+z@e.example?q", "mailto:x.y+z@e.example"},
		{"mailto:x..y@e.example", ""},
		{"ftp://e.example/f", "ftp://e.example/f"},
	} {
		n := linkifyMatchAtStart(tc.in)
		if got := tc.in[:n]; got != tc.want {
			t.Errorf("linkifyMatchAtStart(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
