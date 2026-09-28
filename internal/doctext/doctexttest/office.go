// Package doctexttest makes small course documents for the tests: Office
// Open XML files (.pptx, .docx, .xlsx) and PDFs, written here byte by byte
// so that every test says what its file holds.
package doctexttest

import (
	"archive/zip"
	"bytes"
	"fmt"
	"html"
	"sort"
	"strings"
)

// Media types of the files made.
const (
	PPTXType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	DOCXType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	XLSXType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	PDFType  = "application/pdf"
)

const (
	nsRel = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsPkg = "http://schemas.openxmlformats.org/package/2006/relationships"
	nsA   = "http://schemas.openxmlformats.org/drawingml/2006/main"
	nsP   = "http://schemas.openxmlformats.org/presentationml/2006/main"
	nsW   = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	nsS   = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsC   = "http://schemas.openxmlformats.org/drawingml/2006/chart"
	relT  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"
)

// Zip writes parts into a zip archive, in the order given.
func Zip(parts ...[2]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		w, err := zw.Create(p[0])
		if err != nil {
			panic(err)
		}
		if _, err := w.Write([]byte(p[1])); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func esc(s string) string { return html.EscapeString(s) }

// relsXML is a relationships part.
func relsXML(rels ...[3]string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	sb.WriteString(`<Relationships xmlns="` + nsPkg + `">`)
	for _, r := range rels {
		mode := ""
		if strings.Contains(r[2], "://") {
			mode = ` TargetMode="External"`
		}
		fmt.Fprintf(&sb, `<Relationship Id="%s" Type="%s" Target="%s"%s/>`, r[0], r[1], esc(r[2]), mode)
	}
	sb.WriteString(`</Relationships>`)
	return sb.String()
}

func contentTypes(main, mainType string, more map[string]string) string {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	sb.WriteString(`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	sb.WriteString(`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`)
	sb.WriteString(`<Default Extension="xml" ContentType="application/xml"/>`)
	sb.WriteString(`<Default Extension="png" ContentType="image/png"/>`)
	fmt.Fprintf(&sb, `<Override PartName="/%s" ContentType="%s"/>`, main, mainType)
	keys := make([]string, 0, len(more))
	for k := range more {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, `<Override PartName="/%s" ContentType="%s"/>`, k, more[k])
	}
	sb.WriteString(`</Types>`)
	return sb.String()
}

// png is the smallest PNG: one transparent pixel.
var png = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
	"\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82"

// Bullet is a paragraph of a slide's body, at a level (0 the top).
type Bullet struct {
	Text  string
	Level int
}

// Slide is one slide of a deck.
type Slide struct {
	Title string
	// Body is the body placeholder's paragraphs, which the slide bullets.
	Body []Bullet
	// Text is a text box's paragraphs, not bulleted.
	Text []string
	// Table is a table's rows, the first its header.
	Table [][]string
	// Images is how many pictures it shows.
	Images int
	// Chart is a chart's title; "" for no chart.
	Chart string
	// Notes are the speaker notes.
	Notes  string
	Hidden bool
	// SlideNumber and Footer add those placeholders, which are not
	// content.
	SlideNumber, Footer string
}

// Deck is a presentation.
type Deck struct {
	Slides []Slide
	// NamedBackwards names the slide parts in the reverse of the
	// presentation's order (slide1.xml is the last slide), so that only
	// the presentation's list gives the order.
	NamedBackwards bool
	// External adds a relationship of the first slide to a web page and
	// one to a file outside the package, which a reader must never follow.
	External bool
}

// PPTX is a presentation of slides.
func PPTX(slides ...Slide) []byte { return PPTXDeck(Deck{Slides: slides}) }

// PPTXDeck is a presentation.
func PPTXDeck(d Deck) []byte {
	parts := [][2]string{}
	types := map[string]string{}
	var presRels [][3]string
	var ids strings.Builder
	for i, s := range d.Slides {
		n := i + 1
		if d.NamedBackwards {
			n = len(d.Slides) - i
		}
		name := fmt.Sprintf("slides/slide%d.xml", n)
		rid := fmt.Sprintf("rId%d", i+10)
		presRels = append(presRels, [3]string{rid, relT + "slide", name})
		fmt.Fprintf(&ids, `<p:sldId id="%d" r:id="%s"/>`, 256+i, rid)
		types["ppt/"+name] = "application/vnd.openxmlformats-officedocument.presentationml.slide+xml"
		var rels [][3]string
		var shapes strings.Builder
		id := 2
		if s.Title != "" {
			shapes.WriteString(sp(id, `<p:ph type="title"/>`, []string{s.Title}, nil))
			id++
		}
		if len(s.Body) > 0 {
			var texts []string
			var lvls []int
			for _, b := range s.Body {
				texts, lvls = append(texts, b.Text), append(lvls, b.Level)
			}
			shapes.WriteString(sp(id, `<p:ph idx="1"/>`, texts, lvls))
			id++
		}
		if len(s.Text) > 0 {
			shapes.WriteString(sp(id, "", s.Text, nil))
			id++
		}
		if s.Table != nil {
			shapes.WriteString(tableFrame(id, s.Table))
			id++
		}
		for k := range s.Images {
			rid := fmt.Sprintf("rIdImg%d", k)
			rels = append(rels, [3]string{rid, relT + "image", "../media/image1.png"})
			fmt.Fprintf(&shapes, `<p:pic><p:nvPicPr><p:cNvPr id="%d" name="Picture %d" descr="a diagram"/><p:cNvPicPr/><p:nvPr/></p:nvPicPr>`+
				`<p:blipFill><a:blip r:embed="%s"/></p:blipFill><p:spPr/></p:pic>`, id, id, rid)
			id++
		}
		if s.Chart != "" {
			chart := fmt.Sprintf("charts/chart%d.xml", n)
			rels = append(rels, [3]string{"rIdChart", relT + "chart", "../" + chart})
			types["ppt/"+chart] = "application/vnd.openxmlformats-officedocument.drawingml.chart+xml"
			parts = append(parts, [2]string{"ppt/" + chart, chartXML(s.Chart)})
			fmt.Fprintf(&shapes, `<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="%d" name="Chart"/><p:cNvGraphicFramePr/><p:nvPr/></p:nvGraphicFramePr>`+
				`<p:xfrm/><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/chart">`+
				`<c:chart xmlns:c="%s" r:id="rIdChart"/></a:graphicData></a:graphic></p:graphicFrame>`, id, nsC)
			id++
		}
		if s.SlideNumber != "" {
			shapes.WriteString(sp(id, `<p:ph type="sldNum" idx="12"/>`, []string{s.SlideNumber}, nil))
			id++
		}
		if s.Footer != "" {
			shapes.WriteString(sp(id, `<p:ph type="ftr" idx="11"/>`, []string{s.Footer}, nil))
		}
		if s.Notes != "" {
			notes := fmt.Sprintf("notesSlides/notesSlide%d.xml", n)
			rels = append(rels, [3]string{"rIdNotes", relT + "notesSlide", "../" + notes})
			types["ppt/"+notes] = "application/vnd.openxmlformats-officedocument.presentationml.notesSlide+xml"
			nshapes := sp(2, `<p:ph type="sldImg"/>`, nil, nil) + sp(3, `<p:ph type="body" idx="1"/>`, strings.Split(s.Notes, "\n"), nil) +
				sp(4, `<p:ph type="sldNum" idx="5"/>`, []string{fmt.Sprint(i + 1)}, nil)
			parts = append(parts, [2]string{"ppt/" + notes, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
				`<p:notes xmlns:a="` + nsA + `" xmlns:r="` + nsRel + `" xmlns:p="` + nsP + `"><p:cSld><p:spTree>` +
				`<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/>` + nshapes +
				`</p:spTree></p:cSld></p:notes>`})
		}
		if d.External && i == 0 {
			rels = append(rels, [3]string{"rIdWeb", relT + "hyperlink", "https://example.invalid/slides"},
				[3]string{"rIdFile", relT + "image", "file:///etc/passwd"})
		}
		show := ""
		if s.Hidden {
			show = ` show="0"`
		}
		parts = append(parts, [2]string{"ppt/" + name, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
			`<p:sld xmlns:a="` + nsA + `" xmlns:r="` + nsRel + `" xmlns:p="` + nsP + `"` + show + `><p:cSld><p:spTree>` +
			`<p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/>` + shapes.String() +
			`</p:spTree></p:cSld></p:sld>`})
		if len(rels) > 0 {
			parts = append(parts, [2]string{fmt.Sprintf("ppt/slides/_rels/slide%d.xml.rels", n), relsXML(rels...)})
		}
	}
	pres := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
		`<p:presentation xmlns:a="` + nsA + `" xmlns:r="` + nsRel + `" xmlns:p="` + nsP + `"><p:sldIdLst>` + ids.String() +
		`</p:sldIdLst><p:sldSz cx="12192000" cy="6858000"/></p:presentation>`
	head := [][2]string{
		{"[Content_Types].xml", contentTypes("ppt/presentation.xml", "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml", types)},
		{"_rels/.rels", relsXML([3]string{"rId1", relT + "officeDocument", "ppt/presentation.xml"})},
		{"ppt/presentation.xml", pres},
		{"ppt/_rels/presentation.xml.rels", relsXML(presRels...)},
		{"ppt/media/image1.png", png},
	}
	return Zip(append(head, parts...)...)
}

// sp is a shape with a text body of paragraphs, a placeholder when ph is
// given; levels are the paragraphs' levels.
func sp(id int, ph string, paras []string, levels []int) string {
	var body strings.Builder
	for i, p := range paras {
		ppr := ""
		if i < len(levels) && levels[i] > 0 {
			ppr = fmt.Sprintf(`<a:pPr lvl="%d"/>`, levels[i])
		}
		body.WriteString(`<a:p>` + ppr)
		for j, line := range strings.Split(p, "\n") {
			if j > 0 {
				body.WriteString(`<a:br/>`)
			}
			body.WriteString(`<a:r><a:rPr lang="en-US"/><a:t>` + esc(line) + `</a:t></a:r>`)
		}
		body.WriteString(`<a:endParaRPr lang="en-US"/></a:p>`)
	}
	return fmt.Sprintf(`<p:sp><p:nvSpPr><p:cNvPr id="%d" name="Shape %d"/><p:cNvSpPr/><p:nvPr>%s</p:nvPr></p:nvSpPr><p:spPr/>`+
		`<p:txBody><a:bodyPr/><a:lstStyle/>%s</p:txBody></p:sp>`, id, id, ph, body.String())
}

func tableFrame(id int, rows [][]string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="%d" name="Table"/><p:cNvGraphicFramePr/><p:nvPr/></p:nvGraphicFramePr>`+
		`<p:xfrm/><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/table"><a:tbl><a:tblGrid/>`, id)
	for _, r := range rows {
		sb.WriteString(`<a:tr h="370840">`)
		for _, c := range r {
			sb.WriteString(`<a:tc><a:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>` + esc(c) + `</a:t></a:r></a:p></a:txBody><a:tcPr/></a:tc>`)
		}
		sb.WriteString(`</a:tr>`)
	}
	sb.WriteString(`</a:tbl></a:graphicData></a:graphic></p:graphicFrame>`)
	return sb.String()
}

func chartXML(title string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
		`<c:chartSpace xmlns:c="` + nsC + `" xmlns:a="` + nsA + `"><c:chart><c:title><c:tx><c:rich><a:bodyPr/><a:p><a:r><a:t>` +
		esc(title) + `</a:t></a:r></a:p></c:rich></c:tx><c:overlay val="0"/></c:title><c:plotArea><c:barChart>` +
		`<c:ser><c:tx><c:v>Series 1</c:v></c:tx></c:ser></c:barChart><c:valAx><c:title><c:tx><c:rich><a:p><a:r><a:t>Axis</a:t></a:r></a:p></c:rich></c:tx></c:title></c:valAx></c:plotArea></c:chart></c:chartSpace>`
}

// Block is one block of a Word document: a paragraph (a heading when
// Heading is 1 to 9, a list item when List is set), or a table.
type Block struct {
	Text    string
	Heading int
	// List is "bullet" or "number" for a list item, at Level.
	List  string
	Level int
	// Table is a table's rows.
	Table [][]string
	// Footnote is a footnote the paragraph refers to at its end.
	Footnote string
	// Image puts a picture after the text.
	Image bool
	// TextBox is a text box's text, anchored in the paragraph.
	TextBox string
	// Deleted is text deleted with tracked changes, which is not the
	// document's.
	Deleted string
}

// Doc is a Word document.
type Doc struct {
	Blocks         []Block
	Header, Footer string
	// Sections repeats the header and footer in that many sections.
	Sections int
}

// DOCX is a Word document.
func DOCX(d Doc) []byte {
	var body strings.Builder
	var notes strings.Builder
	noteID := 0
	for _, b := range d.Blocks {
		if b.Table != nil {
			body.WriteString(`<w:tbl><w:tblPr/><w:tblGrid/>`)
			for _, r := range b.Table {
				body.WriteString(`<w:tr>`)
				for _, c := range r {
					body.WriteString(`<w:tc><w:tcPr/><w:p><w:r><w:t xml:space="preserve">` + esc(c) + `</w:t></w:r></w:p></w:tc>`)
				}
				body.WriteString(`</w:tr>`)
			}
			body.WriteString(`</w:tbl>`)
			continue
		}
		body.WriteString(`<w:p><w:pPr>`)
		switch {
		case b.Heading > 0:
			fmt.Fprintf(&body, `<w:pStyle w:val="Heading%d"/>`, b.Heading)
		case b.List == "bullet":
			fmt.Fprintf(&body, `<w:numPr><w:ilvl w:val="%d"/><w:numId w:val="1"/></w:numPr>`, b.Level)
		case b.List == "number":
			fmt.Fprintf(&body, `<w:numPr><w:ilvl w:val="%d"/><w:numId w:val="2"/></w:numPr>`, b.Level)
		}
		body.WriteString(`</w:pPr><w:r><w:t xml:space="preserve">` + esc(b.Text) + `</w:t></w:r>`)
		if b.Deleted != "" {
			body.WriteString(`<w:del w:id="90" w:author="x"><w:r><w:delText>` + esc(b.Deleted) + `</w:delText></w:r></w:del>`)
		}
		if b.Footnote != "" {
			noteID++
			fmt.Fprintf(&body, `<w:r><w:rPr><w:rStyle w:val="FootnoteReference"/></w:rPr><w:footnoteReference w:id="%d"/></w:r>`, noteID)
			fmt.Fprintf(&notes, `<w:footnote w:id="%d"><w:p><w:r><w:footnoteRef/></w:r><w:r><w:t xml:space="preserve"> %s</w:t></w:r></w:p></w:footnote>`, noteID, esc(b.Footnote))
		}
		if b.Image {
			body.WriteString(`<w:r><w:drawing><wp:inline xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"><a:graphic xmlns:a="` + nsA +
				`"><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture">` +
				`<pic:blipFill><a:blip r:embed="rIdImg"/></pic:blipFill></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r>`)
		}
		if b.TextBox != "" {
			body.WriteString(`<w:r><mc:AlternateContent xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"><mc:Choice Requires="wps"><w:drawing><wp:anchor xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing">` +
				`<a:graphic xmlns:a="` + nsA + `"><a:graphicData uri="http://schemas.microsoft.com/office/word/2010/wordprocessingShape"><wps:wsp xmlns:wps="http://schemas.microsoft.com/office/word/2010/wordprocessingShape"><wps:txbx><w:txbxContent>` +
				`<w:p><w:r><w:t>` + esc(b.TextBox) + `</w:t></w:r></w:p></w:txbxContent></wps:txbx></wps:wsp></a:graphicData></a:graphic></wp:anchor></w:drawing></mc:Choice>` +
				`<mc:Fallback><w:pict><v:shape xmlns:v="urn:schemas-microsoft-com:vml"><v:textbox><w:txbxContent><w:p><w:r><w:t>` + esc(b.TextBox) +
				`</w:t></w:r></w:p></w:txbxContent></v:textbox></v:shape></w:pict></mc:Fallback></mc:AlternateContent></w:r>`)
		}
		body.WriteString(`</w:p>`)
	}
	sections := max(d.Sections, 1)
	rels := [][3]string{
		{"rIdStyles", relT + "styles", "styles.xml"},
		{"rIdNumbering", relT + "numbering", "numbering.xml"},
		{"rIdNotes", relT + "footnotes", "footnotes.xml"},
		{"rIdImg", relT + "image", "media/image1.png"},
		{"rIdLink", relT + "hyperlink", "https://example.invalid/"},
	}
	parts := [][2]string{}
	for s := range sections {
		if d.Header != "" {
			id := fmt.Sprintf("rIdHeader%d", s)
			rels = append(rels, [3]string{id, relT + "header", fmt.Sprintf("header%d.xml", s+1)})
			parts = append(parts, [2]string{fmt.Sprintf("word/header%d.xml", s+1), `<w:hdr xmlns:w="` + nsW + `"><w:p><w:r><w:t>` + esc(d.Header) + `</w:t></w:r></w:p></w:hdr>`})
			body.WriteString(`<w:p><w:pPr><w:sectPr><w:headerReference w:type="default" r:id="` + id + `"/></w:sectPr></w:pPr></w:p>`)
		}
		if d.Footer != "" {
			id := fmt.Sprintf("rIdFooter%d", s)
			rels = append(rels, [3]string{id, relT + "footer", fmt.Sprintf("footer%d.xml", s+1)})
			parts = append(parts, [2]string{fmt.Sprintf("word/footer%d.xml", s+1), `<w:ftr xmlns:w="` + nsW + `"><w:p><w:r><w:t>` + esc(d.Footer) + `</w:t></w:r></w:p></w:ftr>`})
		}
	}
	doc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
		`<w:document xmlns:w="` + nsW + `" xmlns:r="` + nsRel + `"><w:body>` + body.String() + `<w:sectPr/></w:body></w:document>`
	styles := `<w:styles xmlns:w="` + nsW + `">`
	for h := 1; h <= 6; h++ {
		styles += fmt.Sprintf(`<w:style w:type="paragraph" w:styleId="Heading%d"><w:name w:val="heading %d"/><w:basedOn w:val="Normal"/><w:pPr><w:outlineLvl w:val="%d"/></w:pPr></w:style>`, h, h, h-1)
	}
	styles += `<w:style w:type="paragraph" w:styleId="Normal"><w:name w:val="Normal"/></w:style></w:styles>`
	numbering := `<w:numbering xmlns:w="` + nsW + `">` +
		`<w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0"><w:numFmt w:val="bullet"/></w:lvl><w:lvl w:ilvl="1"><w:numFmt w:val="bullet"/></w:lvl></w:abstractNum>` +
		`<w:abstractNum w:abstractNumId="1"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl><w:lvl w:ilvl="1"><w:numFmt w:val="lowerLetter"/></w:lvl></w:abstractNum>` +
		`<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num><w:num w:numId="2"><w:abstractNumId w:val="1"/></w:num></w:numbering>`
	footnotes := `<w:footnotes xmlns:w="` + nsW + `"><w:footnote w:type="separator" w:id="-1"><w:p><w:r><w:separator/></w:r></w:p></w:footnote>` +
		`<w:footnote w:type="continuationSeparator" w:id="0"><w:p><w:r><w:continuationSeparator/></w:r></w:p></w:footnote>` + notes.String() + `</w:footnotes>`
	head := [][2]string{
		{"[Content_Types].xml", contentTypes("word/document.xml", "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml", nil)},
		{"_rels/.rels", relsXML([3]string{"rId1", relT + "officeDocument", "word/document.xml"})},
		{"word/document.xml", doc},
		{"word/_rels/document.xml.rels", relsXML(rels...)},
		{"word/styles.xml", styles},
		{"word/numbering.xml", numbering},
		{"word/footnotes.xml", footnotes},
		{"word/media/image1.png", png},
	}
	return Zip(append(head, parts...)...)
}

// Formula is a cell with a formula, and the value Excel cached for it.
type Formula struct {
	F      string
	Cached float64
}

// Sheet is one sheet of a workbook: its rows of cells, each a string
// (kept in the shared strings), an int or a float64, a bool, a Formula, or
// nil for an empty cell.
type Sheet struct {
	Name   string
	Rows   [][]any
	Hidden bool
	// Inline keeps the strings in the cells (inlineStr) instead.
	Inline bool
}

// XLSX is a workbook.
func XLSX(sheets ...Sheet) []byte {
	var shared []string
	index := map[string]int{}
	str := func(s string) int {
		if i, ok := index[s]; ok {
			return i
		}
		index[s] = len(shared)
		shared = append(shared, s)
		return len(shared) - 1
	}
	var wb strings.Builder
	rels := [][3]string{{"rIdStrings", relT + "sharedStrings", "sharedStrings.xml"}}
	parts := [][2]string{}
	for i, sh := range sheets {
		state := ""
		if sh.Hidden {
			state = ` state="hidden"`
		}
		fmt.Fprintf(&wb, `<sheet name="%s" sheetId="%d" r:id="rIdSheet%d"%s/>`, esc(sh.Name), i+1, i, state)
		rels = append(rels, [3]string{fmt.Sprintf("rIdSheet%d", i), relT + "worksheet", fmt.Sprintf("worksheets/sheet%d.xml", i+1)})
		var data strings.Builder
		for r, row := range sh.Rows {
			fmt.Fprintf(&data, `<row r="%d">`, r+1)
			for c, v := range row {
				ref := fmt.Sprintf("%s%d", colName(c), r+1)
				switch x := v.(type) {
				case nil:
				case string:
					if sh.Inline {
						fmt.Fprintf(&data, `<c r="%s" t="inlineStr"><is><t>%s</t></is></c>`, ref, esc(x))
					} else {
						fmt.Fprintf(&data, `<c r="%s" t="s"><v>%d</v></c>`, ref, str(x))
					}
				case bool:
					b := 0
					if x {
						b = 1
					}
					fmt.Fprintf(&data, `<c r="%s" t="b"><v>%d</v></c>`, ref, b)
				case Formula:
					fmt.Fprintf(&data, `<c r="%s"><f>%s</f><v>%v</v></c>`, ref, esc(x.F), x.Cached)
				default:
					fmt.Fprintf(&data, `<c r="%s"><v>%v</v></c>`, ref, x)
				}
			}
			data.WriteString(`</row>`)
		}
		parts = append(parts, [2]string{fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
			`<worksheet xmlns="` + nsS + `" xmlns:r="` + nsRel + `"><sheetData>` + data.String() + `</sheetData><mergeCells count="0"/></worksheet>`})
	}
	var sst strings.Builder
	fmt.Fprintf(&sst, `<sst xmlns="`+nsS+`" count="%d" uniqueCount="%d">`, len(shared), len(shared))
	for _, s := range shared {
		sst.WriteString(`<si><t xml:space="preserve">` + esc(s) + `</t></si>`)
	}
	sst.WriteString(`</sst>`)
	head := [][2]string{
		{"[Content_Types].xml", contentTypes("xl/workbook.xml", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml", nil)},
		{"_rels/.rels", relsXML([3]string{"rId1", relT + "officeDocument", "xl/workbook.xml"})},
		{"xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" + `<workbook xmlns="` + nsS + `" xmlns:r="` + nsRel + `"><sheets>` + wb.String() + `</sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", relsXML(rels...)},
		{"xl/sharedStrings.xml", sst.String()},
	}
	return Zip(append(head, parts...)...)
}

// colName is a column's letters: 0 is A, 26 is AA.
func colName(c int) string {
	s := ""
	for c++; c > 0; c = (c - 1) / 26 {
		s = string(rune('A'+(c-1)%26)) + s
	}
	return s
}
