package doctext

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/unicode/norm"
)

// Fonts (ISO 32000-1 §9.5 to §9.10): what text a font's codes stand for,
// and how wide each is. Text comes first from the font's ToUnicode CMap;
// then, for a composite font, from its encoding when that is a predefined
// Unicode or national CMap; for a simple font, from its encoding (a named
// base encoding, and the glyph names of its Differences). A code nothing
// maps is U+FFFD, which the PDF's readability then counts against it.

// glyph is one code of a string as shown: its text, its width in text
// space for a font size of 1, whether word spacing applies to it (the
// single-byte code 32), and whether nothing mapped it to text.
type glyph struct {
	text     string
	w        float64
	space    bool
	unmapped bool
}

type cidWidth struct {
	lo, hi int
	w      float64
	ws     []float64
}

type pdfFont struct {
	composite bool
	vertical  bool
	toU       *cmap
	encCMap   *cmap
	pre       *predefined
	simple    [256]string
	dingbats  bool
	first     int
	widths    []float64
	dw        float64
	cidW      []cidWidth
	missing   float64
	// fm0 turns a width in glyph space into text space: a thousandth, or
	// a Type 3 font's own matrix.
	fm0 float64
}

// font is the font v names (a reference, or a dictionary), read once.
func (d *pdfDoc) font(v any) *pdfFont {
	if r, ok := v.(pdfRef); ok {
		if f, ok := d.fonts[r]; ok {
			return f
		}
		f := d.newFont(d.dict(r))
		d.fonts[r] = f
		return f
	}
	return d.newFont(d.dict(v))
}

// cmapOf reads a CMap stream.
func (d *pdfDoc) cmapOf(v any) *cmap {
	s, ok := d.resolve(v).(*pdfStream)
	if !ok {
		return nil
	}
	data, err := d.decode(s)
	if err != nil {
		d.failLimit(err)
		return nil
	}
	return parseCMap(data, d.bud)
}

func (d *pdfDoc) newFont(fd pdfDict) *pdfFont {
	f := &pdfFont{fm0: 0.001, dw: 1000}
	if fd == nil {
		return f
	}
	f.toU = d.cmapOf(fd["ToUnicode"])
	if asName(fd["Subtype"]) == "Type0" {
		f.composite = true
		var desc pdfDict
		if kids := d.array(fd["DescendantFonts"]); len(kids) > 0 {
			desc = d.dict(kids[0])
		}
		switch enc := d.resolve(fd["Encoding"]).(type) {
		case pdfName:
			if p, ok := predefinedCMap(string(enc)); ok {
				f.pre, f.vertical = &p, p.vertical
			}
		case *pdfStream:
			f.encCMap = d.cmapOf(enc)
			if f.encCMap != nil {
				f.vertical = f.encCMap.vertical
				if p, ok := predefinedCMap(f.encCMap.use); ok {
					f.pre = &p
					f.vertical = f.vertical || p.vertical
				}
			}
		}
		if f.pre == nil && (f.encCMap == nil || len(f.encCMap.spaces) == 0) {
			f.pre = &predefined{split: twoBytes}
		}
		if w, ok := asFloat(d.resolve(desc["DW"])); ok {
			f.dw = w
		}
		f.cidW = d.cidWidths(d.array(desc["W"]))
		return f
	}
	if asName(fd["Subtype"]) == "Type3" {
		if fm := d.array(fd["FontMatrix"]); len(fm) == 6 {
			if a, ok := asFloat(d.resolve(fm[0])); ok && a != 0 && a > -1 && a < 1 {
				f.fm0 = a
			}
		}
	}
	f.first, _ = asInt(d.resolve(fd["FirstChar"]))
	for _, w := range d.array(fd["Widths"]) {
		x, _ := asFloat(d.resolve(w))
		f.widths = append(f.widths, x)
		if len(f.widths) == 256 {
			break
		}
	}
	desc := d.dict(fd["FontDescriptor"])
	f.missing, _ = asFloat(d.resolve(desc["MissingWidth"]))
	flags, _ := asInt(d.resolve(desc["Flags"]))
	base := asName(fd["BaseFont"])
	if i := strings.IndexByte(base, '+'); i == 6 {
		base = base[i+1:]
	}
	f.simpleEncoding(d, fd, asName(fd["Subtype"]), base, flags&4 != 0)
	return f
}

// simpleEncoding fills a simple font's code-to-text table: its base
// encoding (the one named, or the font's own: Standard for a Type 1 font,
// WinAnsi for a TrueType one, Symbol's for Symbol), then its Differences.
func (f *pdfFont) simpleEncoding(d *pdfDoc, fd pdfDict, subtype, base string, symbolic bool) {
	lower := strings.ToLower(base)
	if strings.Contains(lower, "dingbat") || strings.Contains(lower, "wingding") || strings.Contains(lower, "webding") {
		// Their glyphs are pictures: bullets, ticks, arrows. Each is a
		// bullet to the model.
		f.dingbats = true
		return
	}
	var table *[256]string
	switch {
	case strings.HasPrefix(lower, "symbol"):
		table = &symbolEncoding
	case subtype == "TrueType" && !symbolic:
		table = &winAnsiEncoding
	case symbolic:
		table = &asciiEncoding
	default:
		table = &standardEncoding
	}
	var diffs pdfArray
	switch enc := d.resolve(fd["Encoding"]).(type) {
	case pdfName:
		if t := namedEncoding(string(enc)); t != nil {
			table = t
		}
	case pdfDict:
		if t := namedEncoding(asName(d.resolve(enc["BaseEncoding"]))); t != nil {
			table = t
		}
		diffs = d.array(enc["Differences"])
	}
	f.simple = *table
	code := -1
	for _, e := range diffs {
		switch x := d.resolve(e).(type) {
		case int:
			code = x
		case pdfName:
			if code >= 0 && code < 256 {
				f.simple[code] = glyphText(string(x))
			}
			code++
		}
	}
}

func namedEncoding(name string) *[256]string {
	switch name {
	case "WinAnsiEncoding":
		return &winAnsiEncoding
	case "MacRomanEncoding":
		return &macRomanEncoding
	case "StandardEncoding", "MacExpertEncoding":
		return &standardEncoding
	case "PDFDocEncoding":
		return &pdfDocEncoding
	}
	return nil
}

// cidWidths reads a CIDFont's W array: c [w1 w2 …] and c1 c2 w entries.
func (d *pdfDoc) cidWidths(w pdfArray) []cidWidth {
	var out []cidWidth
	for i := 0; i < len(w) && len(out) < 1<<16; {
		c, ok := asInt(d.resolve(w[i]))
		if !ok || i+1 >= len(w) {
			break
		}
		if arr, isArr := d.resolve(w[i+1]).(pdfArray); isArr {
			cw := cidWidth{lo: c, hi: c + len(arr) - 1}
			for _, x := range arr {
				v, _ := asFloat(d.resolve(x))
				cw.ws = append(cw.ws, v)
			}
			out = append(out, cw)
			i += 2
			continue
		}
		hi, ok1 := asInt(d.resolve(w[i+1]))
		if i+2 >= len(w) || !ok1 {
			break
		}
		v, _ := asFloat(d.resolve(w[i+2]))
		out = append(out, cidWidth{lo: c, hi: hi, w: v})
		i += 3
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].lo < out[j].lo })
	return out
}

// width is the width of code (a CID for a composite font) in glyph space.
func (f *pdfFont) width(code int) float64 {
	if f.composite {
		i := sort.Search(len(f.cidW), func(i int) bool { return f.cidW[i].lo > code }) - 1
		if i >= 0 && code <= f.cidW[i].hi {
			if cw := f.cidW[i]; cw.ws != nil {
				return cw.ws[code-cw.lo]
			}
			return f.cidW[i].w
		}
		return f.dw
	}
	if i := code - f.first; i >= 0 && i < len(f.widths) && f.widths[i] > 0 {
		return f.widths[i]
	}
	if f.missing > 0 {
		return f.missing
	}
	// A standard font given no widths: half an em, as a guess good
	// enough to tell words apart.
	return 500
}

// decode is raw, a string shown in the font, as its glyphs.
func (f *pdfFont) decode(raw []byte) []glyph {
	out := make([]glyph, 0, len(raw))
	for i := 0; i < len(raw); {
		n := 1
		if f.composite {
			switch {
			case f.encCMap != nil && len(f.encCMap.spaces) > 0:
				n = f.encCMap.split(raw[i:])
			case f.pre != nil:
				n = f.pre.split(raw[i:])
			}
			n = min(max(n, 1), 4, len(raw)-i)
		}
		codeBytes := raw[i : i+n]
		code := codeOf(pdfString(codeBytes))
		text, ok := "", false
		if f.toU != nil {
			text, ok = f.toU.lookup(code, n)
		}
		if !ok && f.composite && f.pre != nil && f.pre.decode != nil {
			text, ok = f.pre.decode(codeBytes)
		}
		if !ok && !f.composite {
			switch {
			case f.dingbats:
				text, ok = "•", true
			case f.simple[code] != "":
				text, ok = f.simple[code], true
			}
		}
		g := glyph{text: text, space: n == 1 && code == 32}
		if !ok {
			g.text, g.unmapped = "�", true
		}
		cid := int(code)
		if f.composite && f.pre != nil && f.pre.decode != nil {
			// A national CMap's codes are not CIDs: their widths are
			// the default.
			cid = -1
		}
		g.w = f.width(cid) * f.fm0
		out = append(out, g)
		i += n
	}
	return out
}

// Encodings of simple fonts (Annex D), code to text.
var (
	standardEncoding, winAnsiEncoding, macRomanEncoding, pdfDocEncoding, symbolEncoding, asciiEncoding [256]string
)

func init() {
	for c := 0x20; c < 0x7F; c++ {
		s := string(rune(c))
		standardEncoding[c], asciiEncoding[c], pdfDocEncoding[c] = s, s, s
	}
	standardEncoding['\''], standardEncoding['`'] = "’", "‘"
	for code, name := range map[int]string{
		0xA1: "exclamdown", 0xA2: "cent", 0xA3: "sterling", 0xA4: "fraction", 0xA5: "yen", 0xA6: "florin", 0xA7: "section",
		0xA8: "currency", 0xA9: "quotesingle", 0xAA: "quotedblleft", 0xAB: "guillemotleft", 0xAC: "guilsinglleft",
		0xAD: "guilsinglright", 0xAE: "fi", 0xAF: "fl", 0xB1: "endash", 0xB2: "dagger", 0xB3: "daggerdbl",
		0xB4: "periodcentered", 0xB6: "paragraph", 0xB7: "bullet", 0xB8: "quotesinglbase", 0xB9: "quotedblbase",
		0xBA: "quotedblright", 0xBB: "guillemotright", 0xBC: "ellipsis", 0xBD: "perthousand", 0xBF: "questiondown",
		0xC1: "grave", 0xC2: "acute", 0xC3: "circumflex", 0xC4: "tilde", 0xC5: "macron", 0xC6: "breve", 0xC7: "dotaccent",
		0xC8: "dieresis", 0xCA: "ring", 0xCB: "cedilla", 0xCD: "hungarumlaut", 0xCE: "ogonek", 0xCF: "caron", 0xD0: "emdash",
		0xE1: "AE", 0xE3: "ordfeminine", 0xE8: "Lslash", 0xE9: "Oslash", 0xEA: "OE", 0xEB: "ordmasculine", 0xF1: "ae",
		0xF5: "dotlessi", 0xF8: "lslash", 0xF9: "oslash", 0xFA: "oe", 0xFB: "germandbls",
	} {
		standardEncoding[code] = glyphText(name)
	}
	for c := range 256 {
		if c >= 0x20 && c != 0x7F {
			if r := charmap.Windows1252.DecodeByte(byte(c)); r != utf8.RuneError && (r >= 0xA0 || r < 0x80) {
				winAnsiEncoding[c] = string(r)
			}
		}
		if c >= 0x20 && c != 0x7F {
			if r := charmap.Macintosh.DecodeByte(byte(c)); r != utf8.RuneError {
				macRomanEncoding[c] = string(r)
			}
		}
		if c >= 0xA1 {
			pdfDocEncoding[c] = string(rune(c))
		}
	}
	for code, r := range map[int]rune{
		0x18: '˘', 0x19: 'ˇ', 0x1A: 'ˆ', 0x1B: '˙', 0x1C: '˝', 0x1D: '˛', 0x1E: '˚', 0x1F: '˜',
		0x80: '•', 0x81: '†', 0x82: '‡', 0x83: '…', 0x84: '—', 0x85: '–', 0x86: 'ƒ', 0x87: '⁄', 0x88: '‹', 0x89: '›',
		0x8A: '−', 0x8B: '‰', 0x8C: '„', 0x8D: '“', 0x8E: '”', 0x8F: '‘', 0x90: '’', 0x91: '‚', 0x92: '™', 0x93: 'ﬁ',
		0x94: 'ﬂ', 0x95: 'Ł', 0x96: 'Œ', 0x97: 'Š', 0x98: 'Ÿ', 0x99: 'Ž', 0x9A: 'ı', 0x9B: 'ł', 0x9C: 'œ', 0x9D: 'š',
		0x9E: 'ž', 0xA0: '€',
	} {
		pdfDocEncoding[code] = string(r)
	}
	symbolEncoding = asciiEncoding
	greek := "ΑΒΧΔΕΦΓΗΙϑΚΛΜΝΟΠΘΡΣΤΥςΩΞΨΖ"
	for i, r := range []rune(greek) {
		symbolEncoding['A'+i] = string(r)
	}
	for i, r := range []rune("αβχδεφγηιϕκλμνοπθρστυϖωξψζ") {
		symbolEncoding['a'+i] = string(r)
	}
	for code, r := range map[int]rune{
		0x22: '∀', 0x24: '∃', 0x27: '∋', 0x2A: '∗', 0x2D: '−', 0x40: '≅', 0x5C: '∴', 0x5E: '⊥', 0x60: '‾', 0x7E: '∼',
		0xA2: '′', 0xA3: '≤', 0xA5: '∞', 0xAB: '↔', 0xAC: '←', 0xAD: '↑', 0xAE: '→', 0xAF: '↓', 0xB0: '°', 0xB1: '±',
		0xB2: '″', 0xB3: '≥', 0xB4: '×', 0xB5: '∝', 0xB6: '∂', 0xB7: '•', 0xB8: '÷', 0xB9: '≠', 0xBA: '≡', 0xBB: '≈',
		0xBC: '…', 0xC4: '⊗', 0xC5: '⊕', 0xC6: '∅', 0xC7: '∩', 0xC8: '∪', 0xC9: '⊃', 0xCA: '⊇', 0xCB: '⊄', 0xCC: '⊂',
		0xCD: '⊆', 0xCE: '∈', 0xCF: '∉', 0xD0: '∠', 0xD1: '∇', 0xD5: '∏', 0xD6: '√', 0xD7: '⋅', 0xD8: '¬', 0xD9: '∧',
		0xDA: '∨', 0xDB: '⇔', 0xDC: '⇐', 0xDD: '⇑', 0xDE: '⇒', 0xDF: '⇓', 0xE5: '∑', 0xF2: '∫',
	} {
		symbolEncoding[code] = string(r)
	}
}

// glyphNames are the text of the glyph names that are not a letter with
// an accent (glyphText composes those) nor uniXXXX or uXXXX: the Adobe
// Glyph List's names of the Latin encodings, ligatures, Greek, and the
// mathematical signs TeX's fonts name.
var glyphNames = map[string]string{
	"space": " ", "nbspace": " ", "exclam": "!", "quotedbl": "\"", "numbersign": "#", "dollar": "$", "percent": "%",
	"ampersand": "&", "quotesingle": "'", "quoteright": "’", "parenleft": "(", "parenright": ")", "asterisk": "*",
	"plus": "+", "comma": ",", "hyphen": "-", "sfthyphen": "\u00ad", "period": ".", "slash": "/", "zero": "0", "one": "1",
	"two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9", "colon": ":",
	"semicolon": ";", "less": "<", "equal": "=", "greater": ">", "question": "?", "at": "@", "bracketleft": "[",
	"backslash": "\\", "bracketright": "]", "asciicircum": "^", "underscore": "_", "grave": "`", "quoteleft": "‘",
	"braceleft": "{", "bar": "|", "braceright": "}", "asciitilde": "~",
	"exclamdown": "¡", "cent": "¢", "sterling": "£", "currency": "¤", "yen": "¥", "brokenbar": "¦", "section": "§",
	"dieresis": "¨", "copyright": "©", "ordfeminine": "ª", "guillemotleft": "«", "logicalnot": "¬", "registered": "®",
	"macron": "¯", "degree": "°", "plusminus": "±", "twosuperior": "²", "threesuperior": "³", "acute": "´", "mu": "μ",
	"paragraph": "¶", "periodcentered": "·", "cedilla": "¸", "onesuperior": "¹", "ordmasculine": "º",
	"guillemotright": "»", "onequarter": "¼", "onehalf": "½", "threequarters": "¾", "questiondown": "¿", "AE": "Æ",
	"Eth": "Ð", "multiply": "×", "Oslash": "Ø", "Thorn": "Þ", "germandbls": "ß", "ae": "æ", "eth": "ð", "divide": "÷",
	"oslash": "ø", "thorn": "þ", "Euro": "€", "quotesinglbase": "‚", "florin": "ƒ", "quotedblbase": "„",
	"ellipsis": "…", "dagger": "†", "daggerdbl": "‡", "circumflex": "ˆ", "perthousand": "‰", "guilsinglleft": "‹",
	"OE": "Œ", "quotedblleft": "“", "quotedblright": "”", "bullet": "•", "endash": "–", "emdash": "—", "tilde": "˜",
	"trademark": "™", "guilsinglright": "›", "oe": "œ", "fi": "ﬁ", "fl": "ﬂ", "ff": "ﬀ", "ffi": "ﬃ", "ffl": "ﬄ",
	"dotlessi": "ı", "dotlessj": "ȷ", "Lslash": "Ł", "lslash": "ł", "Dcroat": "Đ", "dcroat": "đ", "fraction": "⁄",
	"minus": "−", "breve": "˘", "dotaccent": "˙", "ring": "˚", "hungarumlaut": "˝", "ogonek": "˛", "caron": "ˇ",
	"Alpha": "Α", "Beta": "Β", "Gamma": "Γ", "Delta": "Δ", "Epsilon": "Ε", "Zeta": "Ζ", "Eta": "Η", "Theta": "Θ",
	"Iota": "Ι", "Kappa": "Κ", "Lambda": "Λ", "Mu": "Μ", "Nu": "Ν", "Xi": "Ξ", "Omicron": "Ο", "Pi": "Π", "Rho": "Ρ",
	"Sigma": "Σ", "Tau": "Τ", "Upsilon": "Υ", "Phi": "Φ", "Chi": "Χ", "Psi": "Ψ", "Omega": "Ω", "alpha": "α",
	"beta": "β", "gamma": "γ", "delta": "δ", "epsilon": "ε", "zeta": "ζ", "eta": "η", "theta": "θ", "iota": "ι",
	"kappa": "κ", "lambda": "λ", "nu": "ν", "xi": "ξ", "omicron": "ο", "pi": "π", "rho": "ρ", "sigma": "σ",
	"sigma1": "ς", "tau": "τ", "upsilon": "υ", "phi": "φ", "chi": "χ", "psi": "ψ", "omega": "ω", "theta1": "ϑ",
	"phi1": "ϕ", "omega1": "ϖ", "epsilon1": "ϵ", "lessequal": "≤", "greaterequal": "≥", "notequal": "≠",
	"infinity": "∞", "summation": "∑", "product": "∏", "integral": "∫", "partialdiff": "∂", "radical": "√",
	"approxequal": "≈", "equivalence": "≡", "similar": "∼", "congruent": "≅", "proportional": "∝", "element": "∈",
	"notelement": "∉", "intersection": "∩", "union": "∪", "logicaland": "∧", "logicalor": "∨", "universal": "∀",
	"existential": "∃", "emptyset": "∅", "therefore": "∴", "propersubset": "⊂", "propersuperset": "⊃",
	"reflexsubset": "⊆", "reflexsuperset": "⊇", "circleplus": "⊕", "circlemultiply": "⊗", "perpendicular": "⊥",
	"angle": "∠", "gradient": "∇", "dotmath": "⋅", "prime": "′", "minute": "′", "second": "″", "arrowleft": "←",
	"arrowup": "↑", "arrowright": "→", "arrowdown": "↓", "arrowboth": "↔", "arrowdblleft": "⇐", "arrowdblright": "⇒",
	"arrowdblboth": "⇔", "lozenge": "◊", "openbullet": "◦", "filledbox": "■", "H18533": "●", "circle": "○",
	"checkmark": "✓", "angleleft": "〈", "angleright": "〉", "Omegagreek": "Ω", "Deltagreek": "Δ", "mugreek": "μ",
	"Ohm": "Ω", "increment": "∆", "estimated": "℮", "numero": "№", "afii61352": "№", "copyrightsans": "©",
	"registersans": "®", "trademarksans": "™",
}

// accents are the names of accents a glyph name may end in, with the
// combining mark each adds to its letter.
var accents = []struct{ name, mark string }{
	{"hungarumlaut", "̋"}, {"circumflex", "̂"}, {"commaaccent", "̦"}, {"dotaccent", "̇"},
	{"dieresis", "̈"}, {"cedilla", "̧"}, {"macron", "̄"}, {"ogonek", "̨"}, {"acute", "́"},
	{"grave", "̀"}, {"tilde", "̃"}, {"breve", "̆"}, {"caron", "̌"}, {"ring", "̊"},
}

// glyphText is the text of a glyph name, "" when it is none known: the
// Adobe Glyph List's rules, for the names fonts use.
func glyphText(name string) string {
	if i := strings.IndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	if name == "" || name == ".notdef" {
		return ""
	}
	if strings.Contains(name, "_") {
		var sb strings.Builder
		for _, part := range strings.Split(name, "_") {
			t := glyphText(part)
			if t == "" {
				return ""
			}
			sb.WriteString(t)
		}
		return sb.String()
	}
	if t, ok := glyphNames[name]; ok {
		return t
	}
	if len(name) == 1 && (name[0] >= 'a' && name[0] <= 'z' || name[0] >= 'A' && name[0] <= 'Z') {
		return name
	}
	if hex, ok := strings.CutPrefix(name, "uni"); ok && len(hex) >= 4 && len(hex)%4 == 0 {
		var sb strings.Builder
		for i := 0; i < len(hex); i += 4 {
			n, err := strconv.ParseUint(hex[i:i+4], 16, 16)
			if err != nil || n >= 0xD800 && n <= 0xDFFF {
				return ""
			}
			sb.WriteRune(rune(n))
		}
		return sb.String()
	}
	if hex, ok := strings.CutPrefix(name, "u"); ok && len(hex) >= 4 && len(hex) <= 6 {
		if n, err := strconv.ParseUint(hex, 16, 32); err == nil && n <= utf8.MaxRune && utf8.ValidRune(rune(n)) {
			return string(rune(n))
		}
	}
	for _, a := range accents {
		if base, ok := strings.CutSuffix(name, a.name); ok && len(base) == 1 {
			if t := glyphText(base); t != "" {
				s := norm.NFC.String(t + a.mark)
				if a.name == "commaaccent" && utf8.RuneCountInString(s) > 1 {
					// Gcommaaccent and its kin are the letters with a
					// cedilla, as the Adobe Glyph List has them.
					s = norm.NFC.String(t + "\u0327")
				}
				return s
			}
		}
	}
	return ""
}
