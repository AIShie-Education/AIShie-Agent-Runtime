package doctext

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/ascii85"
	"errors"
	"io"
	"slices"
)

// A stream's filters (ISO 32000-1 §7.4), undone in order: Flate and LZW
// (with their predictors), ASCIIHex, ASCII85 and RunLength. Image filters
// (DCT, JPX, CCITT, JBIG2) are an image's, never text's, and are not
// undone. Every filter's output is spent from the budget, and none may
// make more than one stream is allowed.

// errImageData is a stream whose filter is an image's.
var errImageData = errors.New("doctext: an image's data")

// decode is s's data with its encryption and filters undone.
func (d *pdfDoc) decode(s *pdfStream) ([]byte, error) {
	if err := d.bud.alive(); err != nil {
		return nil, err
	}
	filters := d.names(s.dict["Filter"])
	parms := d.resolve(s.dict["DecodeParms"])
	data := s.raw
	if d.crypt != nil && asName(s.dict["Type"]) != "XRef" && !slices.Contains(filters, "Crypt") {
		data = d.crypt.stream(data, s.num, s.gen, asName(s.dict["Type"]) == "Metadata")
	}
	if len(filters) > 8 {
		return nil, malformedf("a stream of it has too many filters")
	}
	for i, f := range filters {
		var p pdfDict
		switch x := parms.(type) {
		case pdfDict:
			if i == 0 {
				p = x
			}
		case pdfArray:
			if i < len(x) {
				p = d.dict(x[i])
			}
		}
		var err error
		left := d.bud.left()
		switch f {
		case "FlateDecode", "Fl":
			data, err = inflate(data, left)
			if err == nil {
				data, err = predict(data, p, left)
			}
		case "LZWDecode", "LZW":
			early := true
			if n, ok := asInt(p["EarlyChange"]); ok && n == 0 {
				early = false
			}
			data, err = unLZW(data, early, left)
			if err == nil {
				data, err = predict(data, p, left)
			}
		case "ASCIIHexDecode", "AHx":
			data = unASCIIHex(data)
		case "ASCII85Decode", "A85":
			data, err = unASCII85(data)
		case "RunLengthDecode", "RL":
			data, err = unRunLength(data, left)
		case "Crypt":
		case "DCTDecode", "DCT", "JPXDecode", "CCITTFaxDecode", "CCF", "JBIG2Decode":
			return nil, errImageData
		default:
			return nil, malformedf("a stream of it has an unknown filter")
		}
		if err != nil {
			return nil, err
		}
		if err := d.bud.inflate(int64(len(data))); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// names is a name, or an array of names, as a list.
func (d *pdfDoc) names(v any) []string {
	switch x := d.resolve(v).(type) {
	case pdfName:
		return []string{string(x)}
	case pdfArray:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, asName(d.resolve(e)))
		}
		return out
	}
	return nil
}

// tooLarge is a stream that decodes to more than the runtime reads.
func tooLarge() error { return limitf("a stream of it decodes to more than the runtime reads") }

// readLimited reads r to its end, at most limit bytes: more is ErrLimit.
// A stream cut short, as damaged files' are, keeps what it gave.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(out)) > limit {
		return nil, tooLarge()
	}
	if err != nil && len(out) == 0 && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, malformedf("a stream of it cannot be decompressed")
	}
	return out, nil
}

// inflate undoes Flate: zlib, or raw deflate where the zlib header is
// missing.
func inflate(data []byte, limit int64) ([]byte, error) {
	if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		defer closeQuietly(zr)
		return readLimited(zr, limit)
	}
	fr := flate.NewReader(bytes.NewReader(data))
	defer closeQuietly(fr)
	return readLimited(fr, limit)
}

// predict undoes a Flate or LZW predictor (§7.4.4.4): TIFF 2 for 8-bit
// samples, and PNG's (10 to 15), each row saying its own.
func predict(data []byte, p pdfDict, limit int64) ([]byte, error) {
	pred, _ := asInt(p["Predictor"])
	if pred <= 1 {
		return data, nil
	}
	colors, bpc, columns := 1, 8, 1
	if n, ok := asInt(p["Colors"]); ok {
		colors = n
	}
	if n, ok := asInt(p["BitsPerComponent"]); ok {
		bpc = n
	}
	if n, ok := asInt(p["Columns"]); ok {
		columns = n
	}
	if colors < 1 || colors > 32 || columns < 1 || columns > 1<<20 || (bpc != 1 && bpc != 2 && bpc != 4 && bpc != 8 && bpc != 16) {
		return nil, malformedf("a stream of it has a predictor it cannot use")
	}
	bpp := max(1, colors*bpc/8)
	rowLen := (colors*bpc*columns + 7) / 8
	if pred == 2 {
		if bpc != 8 {
			return data, nil
		}
		out := bytes.Clone(data)
		for r := 0; r+rowLen <= len(out); r += rowLen {
			row := out[r : r+rowLen]
			for i := bpp; i < len(row); i++ {
				row[i] += row[i-bpp]
			}
		}
		return out, nil
	}
	if pred < 10 {
		return data, nil
	}
	rows := len(data) / (rowLen + 1)
	if int64(rows*rowLen) > limit {
		return nil, tooLarge()
	}
	out := make([]byte, 0, rows*rowLen)
	prev := make([]byte, rowLen)
	for r := range rows {
		in := data[r*(rowLen+1) : (r+1)*(rowLen+1)]
		kind, src := in[0], in[1:]
		row := make([]byte, rowLen)
		for i := range rowLen {
			var left, upLeft byte
			if i >= bpp {
				left, upLeft = row[i-bpp], prev[i-bpp]
			}
			up := prev[i]
			switch kind {
			case 1:
				row[i] = src[i] + left
			case 2:
				row[i] = src[i] + up
			case 3:
				row[i] = src[i] + byte((int(left)+int(up))/2) //nolint:gosec // the mean of two bytes is a byte.
			case 4:
				row[i] = src[i] + paeth(left, up, upLeft)
			default:
				row[i] = src[i]
			}
		}
		out = append(out, row...)
		prev = row
	}
	return out, nil
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	switch {
	case pa <= pb && pa <= pc:
		return a
	case pb <= pc:
		return b
	}
	return c
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// unLZW undoes PDF's LZW: codes of 9 to 12 bits, first bit first, 256
// clearing the table and 257 ending the data; early, the code width grows
// one code sooner, as PDF's default has it.
func unLZW(data []byte, early bool, limit int64) ([]byte, error) {
	var out []byte
	table := make([][]byte, 258, 4096)
	reset := func() {
		table = table[:258]
		for i := range 256 {
			table[i] = []byte{byte(i)}
		}
	}
	reset()
	width := 9
	var prev []byte
	var bitBuf uint32
	bits := 0
	pos := 0
	for {
		for bits < width && pos < len(data) {
			bitBuf = bitBuf<<8 | uint32(data[pos])
			pos++
			bits += 8
		}
		if bits < width {
			return out, nil
		}
		code := int(bitBuf>>(bits-width)) & (1<<width - 1)
		bits -= width
		switch code {
		case 256:
			reset()
			width, prev = 9, nil
			continue
		case 257:
			return out, nil
		}
		var entry []byte
		switch {
		case code < len(table):
			entry = table[code]
		case code == len(table) && prev != nil:
			entry = append(bytes.Clone(prev), prev[0])
		default:
			return out, nil
		}
		if int64(len(out)+len(entry)) > limit {
			return nil, tooLarge()
		}
		out = append(out, entry...)
		if prev != nil && len(table) < 4096 {
			table = append(table, append(bytes.Clone(prev), entry[0]))
		}
		prev = entry
		next := len(table)
		if early {
			next++
		}
		switch {
		case next >= 2048:
			width = 12
		case next >= 1024:
			width = 11
		case next >= 512:
			width = 10
		}
	}
}

// unASCIIHex undoes ASCIIHex: pairs of hex digits, white space passed
// over, > ending it.
func unASCIIHex(data []byte) []byte {
	l := lexer{b: append([]byte(nil), data...)}
	return []byte(l.hex())
}

// unASCII85 undoes ASCII85, from an optional <~ to ~>.
func unASCII85(data []byte) ([]byte, error) {
	data = bytes.TrimPrefix(bytes.TrimLeft(data, " \t\r\n\f\x00"), []byte("<~"))
	if i := bytes.Index(data, []byte("~>")); i >= 0 {
		data = data[:i]
	}
	out := make([]byte, 4*(len(data)/5+2)+4*bytes.Count(data, []byte("z")))
	n, _, err := ascii85.Decode(out, data, true)
	if err != nil {
		return nil, malformedf("a stream of it is not ASCII85")
	}
	return out[:n], nil
}

// unRunLength undoes RunLength: a length byte n, then n+1 bytes as they
// are (n < 128) or one byte 257-n times (n > 128); 128 ends it.
func unRunLength(data []byte, limit int64) ([]byte, error) {
	var out []byte
	for i := 0; i < len(data); {
		n := int(data[i])
		i++
		switch {
		case n < 128:
			end := min(i+n+1, len(data))
			out = append(out, data[i:end]...)
			i = end
		case n > 128:
			if i >= len(data) {
				return out, nil
			}
			out = append(out, bytes.Repeat(data[i:i+1], 257-n)...)
			i++
		default:
			return out, nil
		}
		if int64(len(out)) > limit {
			return nil, tooLarge()
		}
	}
	return out, nil
}
