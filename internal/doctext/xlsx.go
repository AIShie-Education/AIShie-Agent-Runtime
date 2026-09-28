package doctext

import (
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A workbook's text: each sheet in the workbook's order, under "## Sheet N:
// name", its rows as CSV, the values as the file stores them (a formula's
// cached value, a number as written, a date as its serial number), shared
// and inline strings resolved; empty rows and trailing empty cells left
// out. A sheet is cut to its first MaxRows rows and MaxCols columns, and
// its heading says what was cut. A chart sheet is [chart].

// wbSheet is a sheet the workbook lists.
type wbSheet struct {
	name, rid string
	hidden    bool
}

func readXLSX(data []byte, b *budget) (*Result, error) {
	p, err := openOPC(data, b)
	if err != nil {
		return nil, err
	}
	main := p.mainPart("xl/workbook.xml")
	if !p.has(main) {
		return nil, malformedf("it holds no workbook")
	}
	rs, err := p.rels(main)
	if err != nil {
		return nil, err
	}
	sheets, err := workbookSheets(p, main)
	if err != nil {
		return nil, err
	}
	var shared []string
	for _, rl := range rs {
		if rl.is("sharedStrings") && rl.target != "" {
			if shared, err = sharedStrings(p, rl.target); err != nil {
				return nil, err
			}
			break
		}
	}
	res := &Result{Format: XLSX, Of: len(sheets)}
	out := newTextOut(b.lim.MaxText)
	byID := relMap(rs)
	for i, sh := range sheets {
		if i == b.lim.MaxParts {
			res.Notes = append(res.Notes, fmt.Sprintf("only its first %d sheets of %d are given", i, len(sheets)))
			break
		}
		if out.cut {
			break
		}
		rl := byID[sh.rid]
		head := fmt.Sprintf("## Sheet %d: %s", i+1, oneLine(sh.name))
		if sh.hidden {
			head += " (hidden)"
		}
		res.Parts++
		switch {
		case rl.is("chartsheet"):
			res.Charts++
			out.section(SectionSheet, i+1, head)
			out.line("[chart]")
		case rl.is("worksheet") && rl.target != "":
			rows, cut, err := readSheet(p, rl.target, shared)
			switch {
			case err != nil && cutShort(err) && res.Parts > 1:
				res.Parts--
				res.Notes = append(res.Notes, restNote(err))
				out.finish(res)
				return res, nil
			case errors.Is(err, ErrMalformed):
				out.section(SectionSheet, i+1, head)
				out.line("[this sheet could not be read]")
				continue
			case err != nil:
				return nil, err
			}
			if cut != "" {
				head += " (" + cut + ")"
				res.Notes = append(res.Notes, fmt.Sprintf("sheet %q is cut: %s", oneLine(sh.name), cut))
			}
			out.section(SectionSheet, i+1, head)
			for _, r := range rows {
				out.line(r)
			}
		default:
			out.section(SectionSheet, i+1, head)
			out.line("[not read: a sheet of a kind the runtime does not read]")
		}
	}
	out.finish(res)
	return res, nil
}

// workbookSheets are the sheets workbook.xml lists, in its order.
func workbookSheets(p *opc, main string) ([]wbSheet, error) {
	rc, err := p.open(main)
	if rc == nil || err != nil {
		return nil, err
	}
	defer closeQuietly(rc)
	x := newXReader(rc, p.b)
	var out []wbSheet
	for {
		tok, err := x.token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sheet" {
			state := attr(se, "state")
			out = append(out, wbSheet{name: attr(se, "name"), rid: relAttr(se, "id"), hidden: state == "hidden" || state == "veryHidden"})
		}
	}
}

// sharedStrings are the workbook's shared strings, in order: each one's
// text, its phonetic runs left out.
func sharedStrings(p *opc, part string) ([]string, error) {
	rc, err := p.open(part)
	if rc == nil || err != nil {
		return nil, err
	}
	defer closeQuietly(rc)
	x := newXReader(rc, p.b)
	var out []string
	for {
		tok, err := x.token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "si" {
			t, err := x.textOf()
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		}
	}
}

// readSheet reads a worksheet's rows as CSV lines, at most MaxRows of them
// and MaxCols columns, and says what it cut ("" when nothing).
func readSheet(p *opc, part string, shared []string) ([]string, string, error) {
	rc, err := p.open(part)
	if rc == nil || err != nil {
		return nil, "", err
	}
	defer closeQuietly(rc)
	lim := p.b.lim
	x := newXReader(rc, p.b)
	var lines []string
	rows, lastRow, widest, kept := 0, 0, 0, 0
	for {
		tok, err := x.token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			if ee, ok := tok.(xml.EndElement); ok && ee.Name.Local == "sheetData" {
				break
			}
			continue
		}
		if se.Name.Local != "row" {
			continue
		}
		n, err := strconv.Atoi(attr(se, "r"))
		if err != nil || n <= lastRow {
			n = lastRow + 1
		}
		lastRow = n
		if kept >= lim.MaxRows {
			// Past the rows given: counted, not read.
			rows++
			if err := x.skip(); err != nil {
				return nil, "", err
			}
			continue
		}
		row, width, err := readRow(x, shared, lim.MaxCols)
		if err != nil {
			return nil, "", err
		}
		widest = max(widest, width)
		if emptyRow(row) {
			continue
		}
		rows++
		kept++
		lines = append(lines, csvLine(row))
	}
	var cut []string
	if rows > kept {
		cut = append(cut, fmt.Sprintf("its first %d rows of %d are given", kept, rows))
	}
	if widest > lim.MaxCols {
		cut = append(cut, fmt.Sprintf("its first %d columns of %d are given", lim.MaxCols, widest))
	}
	return lines, strings.Join(cut, "; "), nil
}

// readRow reads a row's cells (c) into their columns, at most maxCols of
// them, and says how wide the row is.
func readRow(x *xreader, shared []string, maxCols int) ([]string, int, error) {
	var row []string
	next, width := 0, 0
	err := x.children(func(se xml.StartElement) error {
		if se.Name.Local != "c" {
			return x.skip()
		}
		col := next
		if c, ok := column(attr(se, "r")); ok && c >= next {
			col = c
		}
		next = col + 1
		width = max(width, col+1)
		typ := attr(se, "t")
		var value string
		err := x.children(func(se xml.StartElement) error {
			switch se.Name.Local {
			case "v":
				v, err := x.textOfAll()
				value = v
				return err
			case "is":
				t, err := x.textOf()
				if typ == "inlineStr" {
					value = t
				}
				return err
			}
			return x.skip()
		})
		if err != nil {
			return err
		}
		if col >= maxCols {
			return nil
		}
		switch typ {
		case "s":
			if i, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && i >= 0 && i < len(shared) {
				value = shared[i]
			} else {
				value = ""
			}
		case "b":
			switch strings.TrimSpace(value) {
			case "1":
				value = "TRUE"
			case "0":
				value = "FALSE"
			}
		}
		for len(row) <= col {
			row = append(row, "")
		}
		row[col] = value
		return nil
	})
	return row, width, err
}

// column is the column of a cell reference (B3 is 1), and whether it is
// one: at most XFD, Excel's last.
func column(ref string) (int, bool) {
	n, letters := 0, 0
	for _, c := range ref {
		switch {
		case c >= 'A' && c <= 'Z':
			n = n*26 + int(c-'A') + 1
		case c >= 'a' && c <= 'z':
			n = n*26 + int(c-'a') + 1
		default:
			if letters == 0 {
				return 0, false
			}
			return n - 1, n <= 16384
		}
		letters++
		if letters > 3 {
			return 0, false
		}
	}
	return n - 1, letters > 0 && n <= 16384
}

// csvLine is row as one CSV line, its trailing empty cells left out.
func csvLine(row []string) string {
	for len(row) > 0 && row[len(row)-1] == "" {
		row = row[:len(row)-1]
	}
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.Write(row)
	w.Flush()
	return strings.TrimRight(sb.String(), "\r\n")
}
