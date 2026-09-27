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

// The expectations below are what AIShiteru-Frontend's renderer
// (markdown-it 15, html off, linkify on, the TeX plugin) makes of each
// input: each was checked by rendering it, and Body's output, with the
// frontend's own src/utils/markdown.ts.

func TestInlineLinks(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "plain text", in: "Loops repeat a block of code.", want: "Loops repeat a block of code."},
		{name: "a link that carries nothing", in: "See [json](https://docs.python.org/3/library/json.html).", want: "See [json](https://docs.python.org/3/library/json.html)."},
		{name: "query", in: "[click](https://evil.example/?q=secret)", want: "click", links: 1},
		{name: "empty query", in: "[click](https://evil.example/?)", want: "click", links: 1},
		{name: "fragment", in: "[dumps](https://docs.python.org/3/library/json.html#json.dumps)", want: "dumps", links: 1},
		{name: "user information", in: "[x](https://grades@evil.example/)", want: "x", links: 1},
		{name: "user and password", in: "[x](https://u:p@evil.example/)", want: "x", links: 1},
		{name: "javascript is no link, and is stripped", in: "[x](javascript:alert(1))", want: "x", links: 1},
		{name: "upper-case scheme", in: "[x](JavaScript:alert(1))", want: "x", links: 1},
		{name: "file", in: "[x](file:///etc/passwd)", want: "x", links: 1},
		{name: "other scheme", in: "[x](ftp://files.example/a)", want: "x", links: 1},
		{name: "a scheme of letters is a scheme", in: "[x](foo:bar)", want: "x", links: 1},
		{name: "percent-encoding", in: "[x](https://evil.example/a%20b)", want: "x", links: 1},
		{name: "a character the renderer encodes", in: "[x](https://evil.example/a|b)", want: "x", links: 1},
		{name: "brackets are encoded", in: "[x](https://evil.example/a[b])", want: "x", links: 1},
		{name: "a path that is not ASCII is encoded", in: "[東京](https://ja.wikipedia.org/wiki/東京)", want: "東京", links: 1},
		{name: "a host that is not ASCII is punycode", in: "[x](https://日本.jp/docs)", want: "[x](https://日本.jp/docs)"},
		{name: "a short host label in punycode", in: "[x](https://" + strings.Repeat("秘", 12) + ".evil.example/)", want: "[x](https://" + strings.Repeat("秘", 12) + ".evil.example/)"},
		{name: "a long host label in punycode", in: "[x](https://学生の成績はＢマイナスで番号は四二.evil.example/)", want: "x", links: 1},
		{name: "32 data characters in a segment", in: "[x](https://evil.example/" + data32 + ")", want: "x", links: 1},
		{name: "31 data characters", in: "[x](https://docs.example/" + data31 + ")", want: "[x](https://docs.example/" + data31 + ")"},
		{name: "data in a host label", in: "[x](https://" + data32 + ".evil.example/)", want: "x", links: 1},
		{name: "data split by dots stays", in: "[x](https://docs.example/" + data31 + "." + data31 + ")", want: "[x](https://docs.example/" + data31 + "." + data31 + ")"},
		{name: "a URL as long as data", in: "[x](https://docs.example/" + strings.Repeat("a/", 1100) + ")", want: "x", links: 1},
		{name: "relative link", in: "[notes](/courses/cs101/notes)", want: "[notes](/courses/cs101/notes)"},
		{name: "relative link with a query", in: "[notes](notes?x=1)", want: "notes", links: 1},
		{name: "protocol-relative", in: "[x](//evil.example/?q)", want: "x", links: 1},
		{name: "http without slashes", in: "[x](https:evil.example/?q)", want: "x", links: 1},
		{name: "empty destination", in: "[x]()", want: "[x]()"},
		{name: "empty angle destination", in: "[x](<>)", want: "[x](<>)"},
		{name: "mailto", in: "[mail](mailto:ta@example.edu)", want: "[mail](mailto:ta@example.edu)"},
		{name: "mailto with a query", in: "[mail](mailto:ta@example.edu?subject=grades)", want: "mail", links: 1},
		{name: "mailto spelling data", in: "[mail](mailto:" + data32 + "@evil.example)", want: "mail", links: 1},
		{name: "title", in: `[x](https://evil.example/?q "a title")`, want: "x", links: 1},
		{name: "title kept on a clean link", in: `[x](https://docs.python.org/ 'Python docs')`, want: `[x](https://docs.python.org/ 'Python docs')`},
		{name: "a URL in a title is not followed", in: `[x](https://docs.python.org/ "https://evil.example/?q")`, want: `[x](https://docs.python.org/ "https://evil.example/?q")`},
		{name: "parenthesised title", in: "[x](https://evil.example/#f (t))", want: "x", links: 1},
		{name: "angle-bracket destination", in: "[x](<https://evil.example/a b?q>)", want: "x", links: 1},
		{name: "a space in an angle-bracket destination is encoded", in: "[x](<https://docs.example/a b>)", want: "x", links: 1},
		{name: "parentheses in the destination", in: "[Foo](https://en.wikipedia.org/wiki/Foo_(bar))", want: "[Foo](https://en.wikipedia.org/wiki/Foo_(bar))"},
		{name: "nested brackets", in: "[a [b] c](https://evil.example/?q)", want: "a [b] c", links: 1},
		{name: "code span with a bracket in the text", in: "[a `]` b](https://evil.example/?q)", want: "a `]` b", links: 1},
		{name: "escaped brackets are no link, but the URL is", in: `\[not a link\](https://evil.example/?q)`, want: `\[not a link\]([link removed])`, links: 1},
		{name: "escaped query is still a query", in: `[x](https://evil.example/\?q)`, want: "x", links: 1},
		{name: "entity for ?", in: "[x](https://evil.example/&#63;q=1)", want: "x", links: 1},
		{name: "entity for :", in: "[x](javascript&colon;alert(1))", want: "x", links: 1},
		{name: "numeric entity for :", in: "[x](javascript&#x3A;alert(1))", want: "x", links: 1},
		{name: "an entity that is not one stays as written", in: "[x](https://docs.example/a&ampx;b)", want: "[x](https://docs.example/a&ampx;b)"},
		{name: "empty link text", in: "a [](https://evil.example/?q) b", want: "a  b", links: 1},
		{name: "only an empty link", in: "[](https://evil.example/?q)", want: "", links: 1},
		{name: "multiple links on a line", in: "[a](https://x.example/?1) and [b](https://docs.python.org/) and [c](https://y.example/#f)", want: "a and [b](https://docs.python.org/) and c", links: 2},
		{name: "link over two lines", in: "[multi\nline](https://evil.example/?q)", want: "multi\nline", links: 1},
		{name: "destination on the next line", in: "[x](\nhttps://evil.example/?q\n)", want: "x", links: 1},
		{name: "a space ends a destination: no link, but the URL is linkified", in: "[x](https://evil.example/?q data)", want: "[x]([link removed] data)", links: 1},
		{name: "a URL in the link text of a javascript: link", in: "[https://evil.example/?q=1](javascript:x)", want: "[link removed]", links: 2},
		{name: "a URL in a clean link's text is no link", in: "[see https://evil.example/?q=1](https://docs.python.org/)", want: "[see https://evil.example/?q=1](https://docs.python.org/)"},
		{name: "a link inside a link: the inner one is the link", in: "[a [b](https://evil.example/?q) c](https://docs.python.org/)", want: "[a b c](https://docs.python.org/)", links: 1},
		{name: "a clean link inside a link: the outer text's URL is linkified", in: "[a [b](https://docs.python.org/) c](https://evil.example/?q)", want: "[a [b](https://docs.python.org/) c]([link removed])", links: 1},
		{name: "emphasis inside", in: "[**bold** _it_](https://evil.example/?q)", want: "**bold** _it_", links: 1},
		{name: "CJK text", in: "请看[文档](https://evil.example/?q=1)。", want: "请看文档。", links: 1},
		{name: "emoji text", in: "Done 🎉 [see 👩‍💻](https://evil.example/#x)!", want: "Done 🎉 see 👩‍💻!", links: 1},
		{name: "unbalanced bracket", in: "a [ b https://evil.example/?q ] c", want: "a [ b [link removed] ] c", links: 1},
		{name: "a clean link keeps its destination whole", in: "[x](https://docs.example/a$b) [y](https://evil.example/?q) $", want: "[x](https://docs.example/a$b) y $", links: 1},
		{name: "a backtick in a destination is encoded", in: "[x](https://docs.example/a`b)", want: "x", links: 1},
		{name: "in a heading", in: "# See [x](https://evil.example/?q)", want: "# See x", links: 1},
		{name: "in a block quote over two lines", in: "> [a\n> b](https://evil.example/?q)", want: "> a\n> b", links: 1},
		{name: "a title over lines in a block quote", in: "> [a](https://evil.example/?q \"t\n> u\") after", want: "> a\n>  after", links: 1},
	})
}

func TestImages(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "a clean image stays", in: "![diagram](https://docs.python.org/3/_images/a.png)", want: "![diagram](https://docs.python.org/3/_images/a.png)"},
		{name: "query", in: "![chart](https://evil.example/p.png?d=1)", want: "chart", images: 1},
		{name: "a same-origin image with a query", in: "![chart](/api/p.png?d=1)", want: "chart", images: 1},
		{name: "data URI", in: "![chart](data:image/png;base64,iVBORw0KGgo=)", want: "chart", images: 1},
		{name: "data URI the renderer refuses", in: "![chart](data:text/html;base64,PGI+)", want: "chart", images: 1},
		{name: "no alt", in: "![](https://evil.example/p.png?d=1)", want: "[image]", images: 1},
		{name: "blank alt", in: "a ![  ](https://evil.example/#x) b", want: "a [image] b", images: 1},
		{name: "image inside a clean link", in: "[![alt](https://evil.example/i.png?x)](https://docs.python.org/)", want: "[alt](https://docs.python.org/)", images: 1},
		{name: "image inside a stripped link", in: "[![alt](https://evil.example/i.png?x)](https://evil.example/?y)", want: "alt", links: 1, images: 1},
		{name: "a link inside alt text is no link", in: "![a [b](https://evil.example/?q) c](https://docs.python.org/i.png)", want: "![a [b](https://evil.example/?q) c](https://docs.python.org/i.png)"},
		{name: "a link inside the alt of a stripped image is stripped next", in: "![a [b](https://evil.example/?q) c](https://evil.example/i.png?x)", want: "a b c", links: 1, images: 1},
		{name: "escaped bang is a link", in: `\![x](https://evil.example/?q)`, want: `\!x`, links: 1},
		{name: "reference image", in: "![pic][p]\n\n[p]: https://evil.example/p.png?x", want: "pic", links: 1, images: 1},
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
		{name: "in a list item, the marker stays", in: "- [ref]: https://evil.example/?q\n- item [ref]", want: "- \n- item ref", links: 2},
		{name: "a javascript: definition is none, and no link", in: "- [ref]: javascript:alert(1)\n- item [ref]", want: "- [ref]: javascript:alert(1)\n- item [ref]"},
		{name: "a use before its definition", in: "![pic][p]\n\n[p]: https://evil.example/p.png?x", want: "pic", links: 1, images: 1},
		{name: "image use without alt", in: "![][p]\n\n[p]: https://evil.example/p.png?x", want: "[image]", links: 1, images: 1},
		{name: "the first definition is the one used", in: "[a]\n\n[a]: https://evil.example/?q\n[a]: https://docs.python.org/", want: "a\n\n[a]: https://docs.python.org/", links: 2},
		{name: "labels match without case or spacing", in: "[Foo  Bar]\n\n[foo bar]: https://evil.example/?q", want: "Foo  Bar", links: 2},
		{name: "not a definition: trailing words make it text, and its URL is linkified", in: "[ref]: https://evil.example/?q trailing words", want: "[ref]: [link removed] trailing words", links: 1},
		{name: "not a definition: it cannot interrupt a paragraph", in: "text\n[ref]: https://evil.example/?q\nmore [ref]", want: "text\n[ref]: [link removed]\nmore [ref]", links: 1},
		{name: "not a definition: a relative URL in text is no link", in: "text\n[ref]: /r?secret", want: "text\n[ref]: /r?secret"},
		{name: "undefined reference stays", in: "[x][nothing] and [y]", want: "[x][nothing] and [y]"},
		{name: "a footnote-like line with a clean first word", in: "[1]: Python docs, https://evil.example/?q", want: "[1]: Python docs, [link removed]", links: 1},
	})
}

func TestBareURLs(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "clean bare URL", in: "See https://docs.python.org/3/library/json.html for more.", want: "See https://docs.python.org/3/library/json.html for more."},
		{name: "bare URL with a query", in: "See https://evil.example/?q=1 now", want: "See [link removed] now", links: 1},
		{name: "trailing period", in: "See https://evil.example/?q=1.", want: "See [link removed].", links: 1},
		{name: "trailing question mark is punctuation", in: "Did you read https://docs.python.org/3/?", want: "Did you read https://docs.python.org/3/?"},
		{name: "trailing ideographic full stop", in: "看 https://docs.python.org/3/。", want: "看 https://docs.python.org/3/。"},
		{name: "letters after a URL are part of it, and encoded", in: "看https://docs.python.org/3/の文書", want: "看[link removed]", links: 1},
		{name: "in parentheses", in: "(see https://evil.example/?q=1)", want: "(see [link removed])", links: 1},
		{name: "balanced parentheses stay inside", in: "https://en.wikipedia.org/wiki/Foo_(bar), and more", want: "https://en.wikipedia.org/wiki/Foo_(bar), and more"},
		{name: "a trailing entity's ; is left out", in: "https://evil.example/?q&amp;", want: "[link removed];", links: 1},
		{name: "quotes around", in: `"https://evil.example/#x"`, want: `"[link removed]"`, links: 1},
		{name: "two on a line", in: "https://a.example/?1 and https://b.example/#2", want: "[link removed] and [link removed]", links: 2},
		{name: "upper case", in: "HTTPS://EVIL.EXAMPLE/?Q", want: "[link removed]", links: 1},
		{name: "after a letter that is no scheme character", in: "看https://evil.example/?q", want: "看[link removed]", links: 1},
		{name: "after an underscore", in: "_https://evil.example/?q", want: "_[link removed]", links: 1},
		{name: "after a dot", in: "a.https://evil.example/?q", want: "a.[link removed]", links: 1},
		{name: "in emphasis", in: "*https://evil.example/?q*", want: "*[link removed]*", links: 1},
		{name: "a URL running over a code span", in: "https://evil.example/`x`?q=1", want: "[link removed]", links: 1},
		{name: "a URL running over brackets", in: "https://evil.example/[x](y)?q", want: "[link removed]", links: 1},
		{name: "a URL over a bracket pair after an open bracket", in: "[https://evil.example/[x]?q=secret", want: "[[link removed]", links: 1},
		{name: "a URL ends where linkify ends it, and the rest is read after it", in: "https://docs.example/x) [y](https://evil.example/?q) `", want: "https://docs.example/x) y `", links: 1},
		{name: "a backtick is part of a linkified URL, and encoded", in: "https://docs.example/`x [y](https://evil.example/?q) `", want: "[link removed] y `", links: 2},
		{name: "an entity-encoded query in text", in: "https://evil.example/&#63;q=1 x", want: "[link removed] x", links: 1},
		{name: "a user is no part of a linkified URL", in: "https://grades@evil.example/", want: "https://grades@evil.example/"},
		{name: "a port", in: "http://localhost:8080/docs", want: "http://localhost:8080/docs"},
		{name: "a bad port makes no link", in: "https://evil.example:99999/?q", want: "https://evil.example:99999/?q"},
		{name: "ftp is a link, and not http", in: "ftp://files.example/a.txt", want: "[link removed]", links: 1},
		{name: "protocol-relative after a space", in: "Try //evil.example/?q=1", want: "Try [link removed]", links: 1},
		{name: "protocol-relative after punctuation", in: "a.//evil.example/?q=1", want: "a.[link removed]", links: 1},
		{name: "protocol-relative after an underscore is none", in: "_//evil.example/?q=1_", want: "_//evil.example/?q=1_"},
		{name: "protocol-relative after a letter is none", in: "a//evil.example/?q=1", want: "a//evil.example/?q=1"},
		{name: "protocol-relative with a host that is not ASCII", in: "(//日本.jp/?q", want: "([link removed]", links: 1},
		{name: "no bare domains: www", in: "Try www.evil.example/?q=1 today", want: "Try www.evil.example/?q=1 today"},
		{name: "no bare domains", in: "Open evil.example/?q=secret now", want: "Open evil.example/?q=secret now"},
		{name: "a file name stays", in: "Run main.py? Then edit utils.py.", want: "Run main.py? Then edit utils.py."},
		{name: "a version stays", in: "Python 3.12.1 and v1.2.3", want: "Python 3.12.1 and v1.2.3"},
		{name: "words stay", in: "task-management, risk-assessment and e.g. i.e. etc.", want: "task-management, risk-assessment and e.g. i.e. etc."},
		{name: "xmpp is no link", in: "xmpp:ta@example.edu", want: "xmpp:ta@example.edu"},
		{name: "a URL in a heading", in: "## https://evil.example/?q", want: "## [link removed]", links: 1},
		{name: "a URL in a table cell", in: "| a |\n|---|\n| https://evil.example/?q |", want: "| a |\n|---|\n| [link removed] |", links: 1},
	})
}

func TestEmails(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "an email stays", in: "Email ta@example.edu about it.", want: "Email ta@example.edu about it."},
		{name: "an email spelling data", in: "Email " + data32 + "@evil.example now", want: "Email [link removed] now", links: 1},
		{name: "a query in the name", in: "Email x?body=secret@evil.example now", want: "Email [link removed] now", links: 1},
		{name: "percent in the name", in: "a%62@evil.example", want: "[link removed]", links: 1},
		{name: "a name after punctuation is none", in: "me,x?body=secret@evil.example", want: "me,x?body=secret@evil.example"},
		{name: "a name after a quote", in: `"x?y@evil.example"`, want: `"[link removed]"`, links: 1},
		{name: "a name in emphasis", in: "_x?y@evil.example_", want: "_[link removed]_", links: 1},
		{name: "a domain that is not ASCII is punycode", in: "Mail x@日本.jp", want: "Mail x@日本.jp"},
		{name: "mailto: with a query leaves the query out", in: "Write to mailto:ta@example.edu?body=hi", want: "Write to mailto:ta@example.edu?body=hi"},
		{name: "mailto: spelling data", in: "mailto:" + data32 + "@evil.example", want: "[link removed]", links: 1},
		{name: "an email autolink", in: "<ta@example.edu>", want: "<ta@example.edu>"},
		{name: "an email autolink with data", in: "<" + data32 + "@evil.example>", want: "[link removed]", links: 1},
	})
}

func TestAutolinksAndHTML(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "autolink with a query", in: "<https://evil.example/?q=1>", want: "[link removed]", links: 1},
		{name: "clean autolink", in: "<https://docs.python.org/>", want: "<https://docs.python.org/>"},
		{name: "an autolink's entities are not undone", in: "<https://evil.example/&#63;q>", want: "[link removed]", links: 1},
		{name: "javascript autolink is no link, and is stripped", in: "<javascript:alert(1)>", want: "[link removed]", links: 1},
		{name: "an autolink of another scheme", in: "<irc://evil.example/x>", want: "[link removed]", links: 1},
		// With html off, HTML is shown as text: what is in it is read as text.
		{name: "a href", in: `Click <a href="https://evil.example/?q">here</a>.`, want: "Click here.", links: 1},
		{name: "a href unquoted, upper case", in: "<A HREF=https://evil.example/#x>here</A>", want: "here", links: 1},
		{name: "a href with an entity", in: `<a href="javascript&#58;alert(1)">x</a>`, want: "x", links: 1},
		{name: "a href with a tab in the scheme", in: "<a href=\"java\tscript:alert(1)\">x</a>", want: "x", links: 1},
		{name: "a over two lines", in: "<a\nhref='https://evil.example/?q'>x</a>", want: "x", links: 1},
		{name: "a relative href", in: `<a href="/r?secret">x</a>`, want: "x", links: 1},
		{name: "a clean a href is text", in: `<a href="https://docs.python.org/">docs</a>`, want: `<a href="https://docs.python.org/">docs</a>`},
		{name: "img with alt", in: `<img src="https://evil.example/p.png?d=1" alt="chart">`, want: "chart", images: 1},
		{name: "img without alt", in: `<img src='https://evil.example/p.png?d=1'/>`, want: "[image]", images: 1},
		{name: "img srcset", in: `<img src="a.png" srcset="a.png 1x, https://evil.example/b.png?x 2x" alt="a">`, want: "a", images: 1},
		{name: "a clean img is text", in: `<img src="https://docs.python.org/a.png" alt="a">`, want: `<img src="https://docs.python.org/a.png" alt="a">`},
		{name: "style url", in: `<span style="background:url('https://evil.example/?q')">x</span>`, want: "x</span>", links: 1},
		{name: "other tags", in: `<iframe src="https://evil.example/?q"></iframe>`, want: "</iframe>", links: 1},
		{name: "a URL in a title attribute is linkified", in: `<span title="https://evil.example/?q=secret">x</span>`, want: `<span title="[link removed]">x</span>`, links: 1},
		{name: "a comment is text", in: "<!-- https://evil.example/?q -->", want: "<!-- [link removed] -->", links: 1},
		{name: "a comment over lines is text", in: "<!--\nhttps://evil.example/?q\n-->", want: "<!--\n[link removed]\n-->", links: 1},
		{name: "a processing instruction is text", in: "<? https://evil.example/?q ?>", want: "<? [link removed] ?>", links: 1},
		{name: "CDATA is text", in: "<![CDATA[ https://evil.example/?q ]]>", want: "<![CDATA[ [link removed] ]]>", links: 1},
		{name: "a tag in code is left alone", in: "`<a href=\"https://evil.example/?q\">`", want: "`<a href=\"https://evil.example/?q\">`"},
		// There are no HTML blocks: a fence after a tag is a fence.
		{name: "a fence after a div is code", in: "<div>\n```\n<a href=\"https://evil.example/?q\">x</a>\n```\n</div>", want: "<div>\n```\n<a href=\"https://evil.example/?q\">x</a>\n```\n</div>"},
		{name: "a fence closed by a div's end is code up to its own close", in: "<div>\n```\n</div>\n\n[x](https://evil.example/?q)\n```\n[y](https://evil.example/?q)", want: "<div>\n```\n</div>\n\n[x](https://evil.example/?q)\n```\ny", links: 1},
	})
}

func TestCodeIsLeftAlone(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "code span", in: "Use `requests.get('https://api.example/?q=1')` here.", want: "Use `requests.get('https://api.example/?q=1')` here."},
		{name: "code span with link syntax", in: "Write `[x](https://evil.example/?q)` for a link.", want: "Write `[x](https://evil.example/?q)` for a link."},
		{name: "double backticks", in: "``a ` [x](https://evil.example/?q) ``", want: "``a ` [x](https://evil.example/?q) ``"},
		{name: "a code span over two lines", in: "`a\n[x](https://evil.example/?q)`", want: "`a\n[x](https://evil.example/?q)`"},
		{name: "a code span cut by a heading is none", in: "`a\n# [x](https://evil.example/?q) `", want: "`a\n# x `", links: 1},
		{name: "fenced code", in: "Try:\n\n```python\nurl = 'https://api.example/?q=1'\n```\n\nDone.", want: "Try:\n\n```python\nurl = 'https://api.example/?q=1'\n```\n\nDone."},
		{name: "tilde fence", in: "~~~\n<img src=\"https://evil.example/?q\">\n~~~", want: "~~~\n<img src=\"https://evil.example/?q\">\n~~~"},
		{name: "a longer closing fence", in: "````\n```\n[x](https://evil.example/?q)\n```\n````\n[y](https://evil.example/?q)", want: "````\n```\n[x](https://evil.example/?q)\n```\n````\ny", links: 1},
		{name: "an unclosed fence is code to the end", in: "```\n[x](https://evil.example/?q)", want: "```\n[x](https://evil.example/?q)"},
		{name: "a fence interrupts a paragraph", in: "text\n```\n[x](https://evil.example/?q)\n```", want: "text\n```\n[x](https://evil.example/?q)\n```"},
		{name: "indented code", in: "Example:\n\n    requests.get('https://api.example/?q=1')\n\nEnd.", want: "Example:\n\n    requests.get('https://api.example/?q=1')\n\nEnd."},
		{name: "indented code at the start", in: "    curl https://api.example/?q=1", want: "    curl https://api.example/?q=1"},
		{name: "tab-indented code", in: "Run:\n\n\tcurl https://api.example/?q=1", want: "Run:\n\n\tcurl https://api.example/?q=1"},
		{name: "fence in a list item", in: "- step\n\n  ```\n  curl https://api.example/?q=1\n  ```", want: "- step\n\n  ```\n  curl https://api.example/?q=1\n  ```"},
		{name: "fence in an ordered list item", in: "1. Run:\n   ```bash\n   curl \"https://api.example/v1/items?page=2\"\n   ```\n2. Done.", want: "1. Run:\n   ```bash\n   curl \"https://api.example/v1/items?page=2\"\n   ```\n2. Done."},
		{name: "fence in a block quote", in: "> ```\n> [x](https://evil.example/?q)\n> ```", want: "> ```\n> [x](https://evil.example/?q)\n> ```"},
		{name: "indented code in a list item", in: "1. step\n\n       see https://api.example/?q", want: "1. step\n\n       see https://api.example/?q"},
		{name: "an indented paragraph in a list item is text", in: "- item\n\n    [x](https://evil.example/?q)", want: "- item\n\n    x", links: 1},
		{name: "a line that leaves the list item ends its fence", in: "- item\n  ```\n[x](https://evil.example/?q)\n  ```", want: "- item\n  ```\nx\n  ```", links: 1},
		{name: "indented lines inside a paragraph are text", in: "text\n    [x](https://evil.example/?q)", want: "text\n    x", links: 1},
		{name: "a code span in a table cell", in: "| a |\n|---|\n| `https://api.example/?q` |", want: "| a |\n|---|\n| `https://api.example/?q` |"},
		{name: "a table's pipe splits a code span", in: "| a | b |\n|---|---|\n| `a | [x](https://evil.example/?q) b` |", want: "| a | b |\n|---|---|\n| `a | x b` |", links: 1},
		{name: "an escaped pipe does not", in: "| a |\n|---|\n| `a \\| [x](https://evil.example/?q)` |", want: "| a |\n|---|\n| `a \\| [x](https://evil.example/?q)` |"},
		{name: "a pipe without a table is text", in: "| `a | [x](https://evil.example/?q) b` |", want: "| `a | [x](https://evil.example/?q) b` |"},
		{name: "backslash before a backtick", in: "\\`[x](https://evil.example/?q)`", want: "\\`x`", links: 1},
		{name: "list ended by a paragraph, then code", in: "- item\n\ntext\n\n    https://api.example/?q=1", want: "- item\n\ntext\n\n    https://api.example/?q=1"},
	})
}

func TestMathIsLeftAlone(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "inline", in: "The area is $x^2$ and $\\frac{a}{b}$.", want: "The area is $x^2$ and $\\frac{a}{b}$."},
		{name: "a link in TeX is none", in: "$[x](https://evil.example/?q)$", want: "$[x](https://evil.example/?q)$"},
		{name: "a URL in TeX is none", in: "$\\text{https://evil.example/?q}$", want: "$\\text{https://evil.example/?q}$"},
		{name: "a dollar before a space opens nothing", in: "$ [x](https://evil.example/?q)$", want: "$ x$", links: 1},
		{name: "prices are text", in: "It costs $5 and [x](https://evil.example/?q) $10.", want: "It costs $5 and x $10.", links: 1},
		{name: "display on its own lines", in: "$$\n\\href{https://evil.example/?q}{x}\n$$", want: "$$\n\\href{https://evil.example/?q}{x}\n$$"},
		{name: "display on one line", in: "$$ [x](https://evil.example/?q) $$", want: "$$ [x](https://evil.example/?q) $$"},
		{name: "an unclosed display is text", in: "$$\n[x](https://evil.example/?q)\n\nafter", want: "$$\nx\n\nafter", links: 1},
	})
}

// Answers a tutor writes every day come back exactly as written.
func TestOrdinaryAnswersStay(t *testing.T) {
	for _, in := range []string{
		"## Loops\n\nA `for` loop repeats a block:\n\n```python\nfor i in range(3):\n    print(i)  # see https://docs.python.org/3/tutorial/controlflow.html?highlight=for\n```\n\nSee [the tutorial](https://docs.python.org/3/tutorial/controlflow.html).",
		"1. Open the notebook.\n2. Run:\n\n   ```bash\n   curl -s \"https://api.example.edu/v1/grades?student=me&term=2026\"\n   ```\n\n3. Compare with $\\sum_{i=1}^{n} x_i$.",
		"> Note: the formula is $$E = mc^2$$ and the unit is kg·m²/s².\n\n| Symbol | Meaning |\n|---|---|\n| `E` | energy |\n| $m$ | mass |",
		"微分の定義は $f'(x) = \\lim_{h \\to 0} \\frac{f(x+h) - f(x)}{h}$ です。詳しくは https://docs.python.org/3/ を見てください。",
		"Email your TA at ta@example.edu or see <https://lms.example.edu/courses/cs101>.\n\n- **Due:** Friday\n- *Weight:* 10%",
		"Here is JSON:\n\n    {\"url\": \"https://api.example/items?page=2#top\"}\n\nand a path: `/usr/local/bin`.",
		"Use `git log --oneline` and read https://git-scm.com/docs/git-log (the docs), then *try it*.",
	} {
		if got, rep := Body(in, 0); got != in || rep != (Report{}) {
			t.Errorf("Body changed an ordinary answer\n in: %q\nout: %q (%+v)", in, got, rep)
		}
	}
}

func TestLineEndingsAndTrim(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{name: "CRLF", in: "line one\r\n[x](https://evil.example/?q)\r\n", want: "line one\nx", links: 1},
		{name: "CR", in: "a\rb", want: "a\nb"},
		{name: "NUL", in: "a\x00b", want: "a\uFFFDb"},
		{name: "leading blank lines and trailing space", in: "\n\n  \nHello\n\n  \t\n", want: "Hello"},
		{name: "white space only", in: " \n\t\n ", want: ""},
		{name: "white space of any kind only", in: "\u3000\n\u00a0 ", want: ""},
		{name: "a leading line of U+3000 is text to the renderer", in: "\u3000\nHello", want: "\u3000\nHello"},
		{name: "invalid UTF-8", in: "ok \xff done", want: "ok \uFFFD done"},
		{name: "ideographic space trimmed at the end", in: "答え\u3000", want: "答え"},
		// U+3000 keeps the second line from being a table's delimiter row;
		// trimmed, it makes a table whose '|' cuts the code span open.
		{name: "what trimming exposes is stripped", in: "|`a|[x](https://evil.example/?q)`|\n|-|-|\u3000", want: "|`a|x`|\n|-|-|", links: 1},
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
