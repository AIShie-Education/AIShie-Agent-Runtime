package doctext

import (
	"encoding/xml"
	"strings"
)

// What the three Office readers share: walking an element's children,
// gathering the text of its runs, and writing tables as Markdown.

// children reads the children of the element whose start was the last
// token, calling fn with each child's start; fn must read the child through
// its end (x.skip does). It returns once the element has ended.
func (x *xreader) children(fn func(se xml.StartElement) error) error {
	for {
		tok, err := x.token()
		if err != nil {
			return eofMalformed(err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := fn(t); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}

// collect adds to sb the text of the element whose start was the last
// token: the character data of its t elements (a:t, w:t, m:t of a formula),
// a line break for br and cr, a tab for tab. Of an mc:AlternateContent it
// reads the first choice alone (the others say the same for older
// readers), and it passes over phonetic runs (rPh), deleted text and field
// codes.
func (x *xreader) collect(sb *strings.Builder) error {
	inText := 0
	for depth := 1; depth > 0; {
		tok, err := x.token()
		if err != nil {
			return eofMalformed(err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Fallback", "rPh", "delText", "instrText", "instrTextDel":
				if err := x.skip(); err != nil {
					return err
				}
				continue
			case "t":
				inText = depth + 1
			case "br", "cr":
				sb.WriteByte('\n')
			case "tab":
				sb.WriteByte('\t')
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < inText {
				inText = 0
			}
		case xml.CharData:
			if inText > 0 {
				sb.Write(t)
			}
		}
	}
	return nil
}

// textOf is the text of the element whose start was the last token
// (collect).
func (x *xreader) textOf() (string, error) {
	var sb strings.Builder
	err := x.collect(&sb)
	return sb.String(), err
}

// firstChoice reads an mc:AlternateContent whose start was the last token:
// fn is given the children of its first mc:Choice (or of its mc:Fallback
// when it has no choice), and the rest is passed over.
func (x *xreader) firstChoice(fn func(se xml.StartElement) error) error {
	done := false
	return x.children(func(se xml.StartElement) error {
		if done || se.Name.Local != "Choice" && se.Name.Local != "Fallback" {
			return x.skip()
		}
		done = true
		return x.children(fn)
	})
}

// markdownTable writes rows as a Markdown table: the first row as its
// header, every row as wide as the widest, a cell's lines joined with
// <br> and its pipes escaped.
func markdownTable(rows [][]string) []string {
	width := 0
	for _, r := range rows {
		width = max(width, len(r))
	}
	if width == 0 {
		return nil
	}
	out := make([]string, 0, len(rows)+1)
	for i, r := range rows {
		var sb strings.Builder
		sb.WriteString("|")
		for c := range width {
			cell := ""
			if c < len(r) {
				cell = tableCell(r[c])
			}
			sb.WriteString(" " + cell + " |")
		}
		out = append(out, sb.String())
		if i == 0 {
			out = append(out, "|"+strings.Repeat(" --- |", width))
		}
	}
	return out
}

func tableCell(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.ReplaceAll(s, "\n", "<br>")
}

// oneLine is s with its line breaks and tabs made spaces, trimmed.
func oneLine(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(s))
}

// emptyRow reports whether every cell of row is empty.
func emptyRow(row []string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
