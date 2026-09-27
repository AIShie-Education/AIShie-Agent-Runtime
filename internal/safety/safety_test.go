package safety

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
)

// data32 is 32 characters of the data alphabet; data31 one fewer.
const (
	data32 = "c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0"
	data31 = "c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV"
)

type bodyCase struct {
	name          string
	in, want      string
	links, images int
}

func runBodyCases(t *testing.T, cases []bodyCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rep := Body(tc.in, 0)
			if got != tc.want {
				t.Fatalf("Body(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			if rep.LinksRemoved != tc.links || rep.ImagesRemoved != tc.images {
				t.Fatalf("Body(%q): links %d images %d; want %d and %d", tc.in, rep.LinksRemoved, rep.ImagesRemoved, tc.links, tc.images)
			}
			if rep.Truncated || rep.Empty != (tc.want == "") {
				t.Fatalf("report %+v", rep)
			}
			// What Body returns is already safe: it comes back unchanged.
			if again, rep2 := Body(got, 0); again != got || rep2.LinksRemoved+rep2.ImagesRemoved != 0 {
				t.Fatalf("not idempotent: %q became %q (%+v)", got, again, rep2)
			}
		})
	}
}

func TestInlineLinks(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "plain text", in: "Loops repeat a block of code.", want: "Loops repeat a block of code."},
		{name: "a link that carries nothing", in: "See [json](https://docs.python.org/3/library/json.html).", want: "See [json](https://docs.python.org/3/library/json.html)."},
		{name: "query", in: "[click](https://evil.example/?q=secret)", want: "click", links: 1},
		{name: "empty query", in: "[click](https://evil.example/?)", want: "click", links: 1},
		{name: "fragment", in: "[dumps](https://docs.python.org/3/library/json.html#json.dumps)", want: "dumps", links: 1},
		{name: "user information", in: "[x](https://grades@evil.example/)", want: "x", links: 1},
		{name: "javascript", in: "[x](javascript:alert(1))", want: "x", links: 1},
		{name: "upper-case scheme", in: "[x](JavaScript:alert(1))", want: "x", links: 1},
		{name: "file", in: "[x](file:///etc/passwd)", want: "x", links: 1},
		{name: "other scheme", in: "[x](ftp://files.example/a)", want: "x", links: 1},
		{name: "percent-encoding", in: "[x](https://evil.example/a%20b)", want: "x", links: 1},
		{name: "32 data characters in a segment", in: "[x](https://evil.example/" + data32 + ")", want: "x", links: 1},
		{name: "31 data characters", in: "[x](https://docs.example/" + data31 + ")", want: "[x](https://docs.example/" + data31 + ")"},
		{name: "data in a host label", in: "[x](https://" + data32 + ".evil.example/)", want: "x", links: 1},
		{name: "data split by dots stays", in: "[x](https://docs.example/" + data31 + "." + data31 + ")", want: "[x](https://docs.example/" + data31 + "." + data31 + ")"},
		{name: "relative link", in: "[notes](/courses/cs101/notes)", want: "[notes](/courses/cs101/notes)"},
		{name: "relative link with a query", in: "[notes](notes?x=1)", want: "notes", links: 1},
		{name: "protocol-relative", in: "[x](//evil.example/?q)", want: "x", links: 1},
		{name: "empty destination", in: "[x]()", want: "[x]()"},
		{name: "mailto", in: "[mail](mailto:ta@example.edu)", want: "[mail](mailto:ta@example.edu)"},
		{name: "mailto with a query", in: "[mail](mailto:ta@example.edu?subject=grades)", want: "mail", links: 1},
		{name: "mailto spelling data", in: "[mail](mailto:" + data32 + "@evil.example)", want: "mail", links: 1},
		{name: "title", in: `[x](https://evil.example/?q "a title")`, want: "x", links: 1},
		{name: "title kept on a clean link", in: `[x](https://docs.python.org/ 'Python docs')`, want: `[x](https://docs.python.org/ 'Python docs')`},
		{name: "parenthesised title", in: "[x](https://evil.example/#f (t))", want: "x", links: 1},
		{name: "angle-bracket destination", in: "[x](<https://evil.example/a b?q>)", want: "x", links: 1},
		{name: "angle-bracket destination, clean", in: "[x](<https://docs.example/a b>)", want: "[x](<https://docs.example/a b>)"},
		{name: "parentheses in the destination", in: "[Foo](https://en.wikipedia.org/wiki/Foo_(bar))", want: "[Foo](https://en.wikipedia.org/wiki/Foo_(bar))"},
		{name: "nested brackets", in: "[a [b] c](https://evil.example/?q)", want: "a [b] c", links: 1},
		{name: "code span with a bracket in the text", in: "[a `]` b](https://evil.example/?q)", want: "a `]` b", links: 1},
		{name: "escaped brackets are no link", in: `\[not a link\](https://evil.example/?q)`, want: `\[not a link\]([link removed])`, links: 1},
		{name: "escaped query is still a query", in: `[x](https://evil.example/\?q)`, want: "x", links: 1},
		{name: "entity for ?", in: "[x](https://evil.example/&#63;q=1)", want: "x", links: 1},
		{name: "entity for :", in: "[x](javascript&colon;alert(1))", want: "x", links: 1},
		{name: "numeric entity for :", in: "[x](javascript&#x3A;alert(1))", want: "x", links: 1},
		{name: "empty link text", in: "a [](https://evil.example/?q) b", want: "a  b", links: 1},
		{name: "only an empty link", in: "[](https://evil.example/?q)", want: "", links: 1},
		{name: "multiple links on a line", in: "[a](https://x.example/?1) and [b](https://docs.python.org/) and [c](https://y.example/#f)", want: "a and [b](https://docs.python.org/) and c", links: 2},
		{name: "link over two lines", in: "[multi\nline](https://evil.example/?q)", want: "multi\nline", links: 1},
		{name: "destination on the next line", in: "[x](\nhttps://evil.example/?q\n)", want: "x", links: 1},
		{name: "space in a destination is no link", in: "[x](https://evil.example/?q data)", want: "[x]([link removed] data)", links: 1},
		{name: "a URL in the link text", in: "[https://evil.example/?q=1](javascript:x)", want: "[link removed]", links: 2},
		{name: "a URL in a clean link's text", in: "[see https://evil.example/?q=1](https://docs.python.org/)", want: "[see [link removed]](https://docs.python.org/)", links: 1},
		{name: "a link inside a link", in: "[a [b](https://evil.example/?q) c](https://docs.python.org/)", want: "[a b c](https://docs.python.org/)", links: 1},
		{name: "emphasis inside", in: "[**bold** _it_](https://evil.example/?q)", want: "**bold** _it_", links: 1},
		{name: "CJK text", in: "请看[文档](https://evil.example/?q=1)。", want: "请看文档。", links: 1},
		{name: "emoji text", in: "Done 🎉 [see 👩‍💻](https://evil.example/#x)!", want: "Done 🎉 see 👩‍💻!", links: 1},
		{name: "IRI path stays", in: "[東京](https://ja.wikipedia.org/wiki/東京)", want: "[東京](https://ja.wikipedia.org/wiki/東京)"},
		{name: "unbalanced bracket", in: "a [ b https://evil.example/?q ] c", want: "a [ b [link removed] ] c", links: 1},
	})
}

func TestImages(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "a clean image stays", in: "![diagram](https://docs.python.org/3/_images/a.png)", want: "![diagram](https://docs.python.org/3/_images/a.png)"},
		{name: "query", in: "![chart](https://evil.example/p.png?d=1)", want: "chart", images: 1},
		{name: "data URI", in: "![chart](data:image/png;base64,iVBORw0KGgo=)", want: "chart", images: 1},
		{name: "no alt", in: "![](https://evil.example/p.png?d=1)", want: "[image]", images: 1},
		{name: "blank alt", in: "a ![  ](https://evil.example/#x) b", want: "a [image] b", images: 1},
		{name: "image inside a clean link", in: "[![alt](https://evil.example/i.png?x)](https://docs.python.org/)", want: "[alt](https://docs.python.org/)", images: 1},
		{name: "image inside a stripped link", in: "[![alt](https://evil.example/i.png?x)](https://evil.example/?y)", want: "alt", links: 1, images: 1},
		{name: "link inside alt", in: "![a [b](https://evil.example/?q) c](https://docs.python.org/i.png)", want: "![a b c](https://docs.python.org/i.png)", links: 1},
		{name: "escaped bang is a link", in: `\![x](https://evil.example/?q)`, want: `\!x`, links: 1},
	})
}

func TestReferences(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "full reference", in: "[x][ref]\n\n[ref]: https://evil.example/?q \"t\"", want: "x", links: 2},
		{name: "collapsed and shortcut", in: "[ref] and [Ref][]\n\n[ref]: https://evil.example/?q", want: "ref and Ref", links: 3},
		{name: "clean definition stays", in: "[x][ref]\n\n[ref]: https://docs.python.org/", want: "[x][ref]\n\n[ref]: https://docs.python.org/"},
		{name: "destination on the next line", in: "use [ref]\n\n[ref]:\n  https://evil.example/?q", want: "use ref", links: 2},
		{name: "title on the next line", in: "use [ref]\n\n[ref]: https://evil.example/?q\n  \"a title\"\n\nafter", want: "use ref\n\n\nafter", links: 2},
		{name: "title over two lines", in: "[ref]: https://evil.example/#f 'one\ntwo'\nnext", want: "next", links: 1},
		{name: "angle-bracket destination", in: "[ref]: <https://evil.example/a b?q>\n\n[ref]", want: "ref", links: 2},
		{name: "in a block quote", in: "> [ref]: https://evil.example/?q\n\n[ref]", want: "ref", links: 2},
		{name: "in a list item", in: "- [ref]: javascript:alert(1)\n- item [ref]", want: "- item ref", links: 2},
		{name: "a use before its definition", in: "![pic][p]\n\n[p]: https://evil.example/p.png?x", want: "pic", links: 1, images: 1},
		{name: "image use without alt", in: "![][p]\n\n[p]: https://evil.example/p.png?x", want: "[image]", links: 1, images: 1},
		{name: "one removed, one kept: the renderer finds the kept one", in: "[a]\n\n[a]: https://evil.example/?q\n[a]: https://docs.python.org/", want: "[a]\n\n[a]: https://docs.python.org/", links: 1},
		{name: "not a definition, but its URL still goes", in: "[ref]: https://evil.example/?q trailing words", want: "", links: 1},
		{name: "undefined reference stays", in: "[x][nothing] and [y]", want: "[x][nothing] and [y]"},
		{name: "a footnote-like line with a clean first word", in: "[1]: Python docs, https://evil.example/?q", want: "[1]: Python docs, [link removed]", links: 1},
		{name: "definition inside a paragraph", in: "text\n[ref]: https://evil.example/?q\nmore [ref]", want: "text\nmore ref", links: 2},
	})
}

func TestBareURLs(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "clean bare URL", in: "See https://docs.python.org/3/library/json.html for more.", want: "See https://docs.python.org/3/library/json.html for more."},
		{name: "bare URL with a query", in: "See https://evil.example/?q=1 now", want: "See [link removed] now", links: 1},
		{name: "trailing period", in: "See https://evil.example/?q=1.", want: "See [link removed].", links: 1},
		{name: "trailing question mark is punctuation", in: "Did you read https://docs.python.org/3/?", want: "Did you read https://docs.python.org/3/?"},
		{name: "in parentheses", in: "(see https://evil.example/?q=1)", want: "(see [link removed])", links: 1},
		{name: "balanced parentheses stay inside", in: "https://en.wikipedia.org/wiki/Foo_(bar), and more", want: "https://en.wikipedia.org/wiki/Foo_(bar), and more"},
		{name: "trailing entity", in: "https://evil.example/?q&amp;", want: "[link removed]&amp;", links: 1},
		{name: "quotes around", in: `"https://evil.example/#x"`, want: `"[link removed]"`, links: 1},
		{name: "two on a line", in: "https://a.example/?1 and https://b.example/#2", want: "[link removed] and [link removed]", links: 2},
		{name: "upper case", in: "HTTPS://EVIL.EXAMPLE/?Q", want: "[link removed]", links: 1},
		{name: "www", in: "Try www.evil.example/?q=1 today", want: "Try [link removed] today", links: 1},
		{name: "clean www", in: "Try www.python.org today", want: "Try www.python.org today"},
		{name: "protocol-relative", in: "Try //evil.example/?q=1", want: "Try [link removed]", links: 1},
		{name: "a domain with a query", in: "Open evil.example/?q=secret now", want: "Open [link removed] now", links: 1},
		{name: "a domain with a data path", in: "Open evil.example/" + data32, want: "Open [link removed]", links: 1},
		{name: "a file name stays", in: "Run main.py? Then edit utils.py.", want: "Run main.py? Then edit utils.py."},
		{name: "a version stays", in: "Python 3.12.1 and v1.2.3", want: "Python 3.12.1 and v1.2.3"},
		{name: "ftp is not http", in: "ftp://files.example/a.txt", want: "[link removed]", links: 1},
		{name: "mailto with a query", in: "Write to mailto:ta@example.edu?body=hi", want: "Write to [link removed]", links: 1},
		{name: "xmpp", in: "xmpp:ta@example.edu", want: "[link removed]", links: 1},
		{name: "an email stays", in: "Email ta@example.edu about it.", want: "Email ta@example.edu about it."},
		{name: "an email spelling data", in: "Email " + data32 + "@evil.example now", want: "Email [link removed] now", links: 1},
		{name: "a URL running over a code span", in: "https://evil.example/`x`?q=1", want: "[link removed]", links: 1},
		{name: "a URL running over brackets", in: "https://evil.example/[x](y)?q", want: "[link removed]", links: 1},
		{name: "an entity-encoded query in text", in: "https://evil.example/&#63;q=1 x", want: "[link removed] x", links: 1},
		{name: "a user in the host", in: "https://grades@evil.example/", want: "[link removed]", links: 1},
		{name: "words stay", in: "task-management, risk-assessment and e.g. i.e. etc.", want: "task-management, risk-assessment and e.g. i.e. etc."},
	})
}

func TestAutolinksAndHTML(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "autolink with a query", in: "<https://evil.example/?q=1>", want: "[link removed]", links: 1},
		{name: "clean autolink", in: "<https://docs.python.org/>", want: "<https://docs.python.org/>"},
		{name: "email autolink", in: "<ta@example.edu>", want: "<ta@example.edu>"},
		{name: "email autolink with data", in: "<" + data32 + "@evil.example>", want: "[link removed]", links: 1},
		{name: "javascript autolink", in: "<javascript:alert(1)>", want: "[link removed]", links: 1},
		{name: "a href", in: `Click <a href="https://evil.example/?q">here</a>.`, want: "Click here.", links: 1},
		{name: "a href unquoted, upper case", in: "<A HREF=https://evil.example/#x>here</A>", want: "here", links: 1},
		{name: "a clean a href", in: `<a href="https://docs.python.org/">docs</a>`, want: `<a href="https://docs.python.org/">docs</a>`},
		{name: "a href with an entity", in: `<a href="javascript&#58;alert(1)">x</a>`, want: "x", links: 1},
		{name: "a href with a tab in the scheme", in: "<a href=\"java\tscript:alert(1)\">x</a>", want: "x", links: 1},
		{name: "a over two lines", in: "<a\nhref='https://evil.example/?q'>x</a>", want: "x", links: 1},
		{name: "img with alt", in: `<img src="https://evil.example/p.png?d=1" alt="chart">`, want: "chart", images: 1},
		{name: "img without alt", in: `<img src='https://evil.example/p.png?d=1'/>`, want: "[image]", images: 1},
		{name: "img srcset", in: `<img src="a.png" srcset="a.png 1x, https://evil.example/b.png?x 2x" alt="a">`, want: "a", images: 1},
		{name: "a clean img", in: `<img src="https://docs.python.org/a.png" alt="a">`, want: `<img src="https://docs.python.org/a.png" alt="a">`},
		{name: "style url", in: `<span style="background:url('https://evil.example/?q')">x</span>`, want: "x</span>", links: 1},
		{name: "other tags", in: `<iframe src="https://evil.example/?q"></iframe>`, want: "</iframe>", links: 1},
		{name: "a comment is left alone", in: "<!-- https://evil.example/?q -->", want: "<!-- https://evil.example/?q -->"},
		{name: "a comment over lines is read as text", in: "<!--\nhttps://evil.example/?q\n-->", want: "<!--\n[link removed]\n-->", links: 1},
		{name: "a tag holding a pipe is read as text", in: "| <span title='|'> [x](https://evil.example/?q) |'> |", want: "| <span title='|'> x |'> |", links: 1},
		{name: "html block with a fence inside", in: "<div>\n```\n<a href=\"https://evil.example/?q\">x</a>\n```\n</div>", want: "<div>\n```\nx\n```\n</div>", links: 1},
		{name: "pre block over a blank line", in: "<pre>\n\n```\n<a href=\"https://evil.example/?q\">x</a>\n```\n</pre>", want: "<pre>\n\n```\nx\n```\n</pre>", links: 1},
	})
}

func TestCodeIsLeftAlone(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "code span", in: "Use `requests.get('https://api.example/?q=1')` here.", want: "Use `requests.get('https://api.example/?q=1')` here."},
		{name: "code span with link syntax", in: "Write `[x](https://evil.example/?q)` for a link.", want: "Write `[x](https://evil.example/?q)` for a link."},
		{name: "double backticks", in: "``a ` [x](https://evil.example/?q) ``", want: "``a ` [x](https://evil.example/?q) ``"},
		{name: "fenced code", in: "Try:\n\n```python\nurl = 'https://api.example/?q=1'\n```\n\nDone.", want: "Try:\n\n```python\nurl = 'https://api.example/?q=1'\n```\n\nDone."},
		{name: "tilde fence", in: "~~~\n<img src=\"https://evil.example/?q\">\n~~~", want: "~~~\n<img src=\"https://evil.example/?q\">\n~~~"},
		{name: "a longer closing fence", in: "````\n```\n[x](https://evil.example/?q)\n```\n````\n[y](https://evil.example/?q)", want: "````\n```\n[x](https://evil.example/?q)\n```\n````\ny", links: 1},
		{name: "an unclosed fence is code to the end", in: "```\n[x](https://evil.example/?q)", want: "```\n[x](https://evil.example/?q)"},
		{name: "indented code", in: "Example:\n\n    requests.get('https://api.example/?q=1')\n\nEnd.", want: "Example:\n\n    requests.get('https://api.example/?q=1')\n\nEnd."},
		{name: "indented code at the start", in: "    curl https://api.example/?q=1", want: "    curl https://api.example/?q=1"},
		{name: "tab-indented code", in: "Run:\n\n\tcurl https://api.example/?q=1", want: "Run:\n\n\tcurl https://api.example/?q=1"},
		{name: "fence in a list item", in: "- step\n\n  ```\n  curl https://api.example/?q=1\n  ```", want: "- step\n\n  ```\n  curl https://api.example/?q=1\n  ```"},
		// What only looks like code to a careless reader is stripped.
		{name: "indented paragraph in a list is not code", in: "- item\n\n    [x](https://evil.example/?q)", want: "- item\n\n    x", links: 1},
		{name: "ordered list continuation is not code", in: "1. step\n\n    see https://evil.example/?q", want: "1. step\n\n    see [link removed]", links: 1},
		{name: "a line that leaves the list item ends its fence", in: "- item\n  ```\n[x](https://evil.example/?q)\n  ```", want: "- item\n  ```\nx\n  ```", links: 1},
		{name: "indented lines inside a paragraph are not code", in: "text\n    [x](https://evil.example/?q)", want: "text\n    x", links: 1},
		{name: "a code span with a pipe is not code in a table", in: "| `a | [x](https://evil.example/?q) b` |", want: "| `a | x b` |", links: 1},
		{name: "a code span over two lines is not taken", in: "`a\n# [x](https://evil.example/?q) `", want: "`a\n# x `", links: 1},
		{name: "a fence in a block quote is not taken", in: "> ```\n> [x](https://evil.example/?q)\n> ```", want: "> ```\n> x\n> ```", links: 1},
		{name: "backslash before a backtick", in: "\\`[x](https://evil.example/?q)`", want: "\\`x`", links: 1},
		{name: "list ended by a paragraph, then code", in: "- item\n\ntext\n\n    https://api.example/?q=1", want: "- item\n\ntext\n\n    https://api.example/?q=1"},
	})
}

func TestLineEndingsAndTrim(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "CRLF", in: "line one\r\n[x](https://evil.example/?q)\r\n", want: "line one\nx", links: 1},
		{name: "CR", in: "a\rb", want: "a\nb"},
		{name: "leading blank lines and trailing space", in: "\n\n  \nHello\n\n  \t\n", want: "Hello"},
		{name: "white space only", in: " \n\t\n ", want: ""},
		{name: "invalid UTF-8", in: "ok \xff done", want: "ok \uFFFD done"},
		{name: "ideographic space trimmed at the end", in: "答え\u3000", want: "答え"},
	})
}

func TestTruncation(t *testing.T) {
	long := func(unit string, n int) string { return strings.Repeat(unit, n) }
	for _, tc := range []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "fits", in: "short", max: 100, want: "short"},
		{name: "exactly the limit", in: long("a", 100), max: 100, want: long("a", 100)},
		{name: "paragraph break", in: long("para one. ", 8) + "\n\n" + long("two ", 30), max: 100, want: strings.TrimSpace(long("para one. ", 8)) + "\n\n…"},
		{name: "sentence end", in: "The first sentence is here and fairly long. The second goes on and on without stopping at all", max: 60, want: "The first sentence is here and fairly long.…"},
		{name: "sentence end with a closing quote", in: `Then the teacher said "stop, please." And a very long clause follows without an end`, max: 60, want: `Then the teacher said "stop, please."…`},
		{name: "a sentence end too early is passed over", in: "Short. Then a long run of words that goes on well past the halfway mark of it", max: 60, want: "Short. Then a long run of words that goes on well past the…"},
		{name: "space", in: "word " + long("x", 10) + " " + long("y", 30) + " " + long("z", 80), max: 60, want: "word " + long("x", 10) + " " + long("y", 30) + "…"},
		{name: "no break keeps at least half", in: "a " + long("b", 200), max: 100, want: "a " + long("b", 97) + "…"},
		{name: "CJK sentence", in: long("学", 30) + "。" + long("习", 100), max: 60, want: long("学", 30) + "。…"},
		{name: "CJK without breaks", in: long("漢字", 100), max: 51, want: long("漢字", 25) + "…"},
		{name: "emoji sequence not split", in: long("a", 97) + "👩‍💻👩‍💻", max: 100, want: long("a", 97) + "…"},
		{name: "flag not split", in: long("a", 98) + "🇯🇵🇯🇵", max: 101, want: long("a", 98) + "🇯🇵…"},
		{name: "combining mark not split", in: long("a", 98) + "e\u0301e\u0301", max: 100, want: long("a", 98) + "…"},
		{name: "a cut that opens a code span strips what was code", in: "`https://evil.example/?q=1&" + long("x", 60) + "` end", max: 30, want: "`[link removed]…"},
		{name: "a cut inside a code span with spaces keeps it closed", in: "`https://evil.example/?q=1` " + long("word ", 30), max: 30, want: "`https://evil.example/?q=1`…"},
		{name: "a limit above Core's is Core's", in: long("a", 20001), max: 50000, want: long("a", 19999) + "…"},
		{name: "no limit is Core's", in: long("a", 20001), max: 0, want: long("a", 19999) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, rep := Body(tc.in, tc.max)
			if got != tc.want {
				t.Fatalf("Body(%d)\n got %q\nwant %q", tc.max, got, tc.want)
			}
			limit := tc.max
			if limit <= 0 || limit > CoreMaxChars {
				limit = CoreMaxChars
			}
			if n := len([]rune(got)); n > limit {
				t.Fatalf("%d characters, over %d", n, limit)
			}
			if rep.Truncated != strings.HasSuffix(got, Ellipsis) {
				t.Fatalf("Truncated %v for %q", rep.Truncated, got)
			}
		})
	}
}

func TestTruncationNeverExceeds(t *testing.T) {
	// Every limit from 1 up, over text mixing everything.
	text := "Intro [a](https://docs.python.org/) 学习。 `https://api.example/?q=1` 👩‍💻 🇯🇵\n\n" +
		"```\ncode https://x.example/?q\n```\n\n- item https://y.example/#f\n\n" + strings.Repeat("more words. ", 10)
	for limit := 1; limit < 260; limit++ {
		got, rep := Body(text, limit)
		if n := len([]rune(got)); n > limit {
			t.Fatalf("limit %d: %d characters: %q", limit, n, got)
		}
		if !rep.Truncated && got != text && !strings.Contains(text, "?q") {
			t.Fatalf("limit %d: changed without a cut", limit)
		}
		if again, _ := Body(got, limit); again != got {
			t.Fatalf("limit %d: not idempotent: %q then %q", limit, got, again)
		}
	}
}

// update rewrites the golden files: go test ./internal/safety -update.
var update = flag.Bool("update", false, "rewrite the golden files")

func TestGoldenAnswer(t *testing.T) {
	in, err := os.ReadFile("testdata/answer.md")
	if err != nil {
		t.Fatal(err)
	}
	got, rep := Body(string(in), 19000)
	got += fmt.Sprintf("\n\n<!-- %+v -->\n", rep)
	golden := "testdata/answer.golden.md"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("Body differs from %s (go test -update to rewrite it):\n%s", golden, got)
	}
}
