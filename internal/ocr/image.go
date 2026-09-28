package ocr

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif"  // image.DecodeConfig reads GIF's size
	_ "image/jpeg" // and JPEG's
	_ "image/png"  // and PNG's
)

// checkImage refuses an image OCR must not be given: one whose size cannot
// be read from its header, or past MaxPixels (decoded, a picture of a
// hundred thousand pixels a side is gigabytes, whatever its file's size).
// tesseract is held to its memory besides; this refuses such a file
// before any program is run on it.
func (e *Engine) checkImage(data []byte) error {
	w, h, err := imageSize(data)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if w <= 0 || h <= 0 {
		return fmt.Errorf("%w: it has no pixels", ErrMalformed)
	}
	if int64(w)*int64(h) > e.cfg.MaxPixels || w > 50_000 || h > 50_000 {
		return fmt.Errorf("%w: it is %d×%d pixels", ErrTooLarge, w, h)
	}
	return nil
}

// imageSize reads an image's size from its header: PNG, JPEG and GIF with
// the standard library's decoders, WebP from its RIFF chunks.
func imageSize(data []byte) (int, int, error) {
	if isWebP(data) {
		return webpSize(data)
	}
	c, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("its size cannot be read: %w", err)
	}
	return c.Width, c.Height, nil
}

func isWebP(data []byte) bool {
	return len(data) >= 16 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

// webpSize reads a WebP's canvas size from its first chunk: VP8X's
// canvas, VP8L's image header, or VP8's key frame header.
func webpSize(data []byte) (int, int, error) {
	if len(data) < 30 {
		return 0, 0, fmt.Errorf("the WebP is too short")
	}
	le24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
	switch string(data[12:16]) {
	case "VP8X":
		return le24(data[24:27]) + 1, le24(data[27:30]) + 1, nil
	case "VP8L":
		if data[20] != 0x2f {
			return 0, 0, fmt.Errorf("the WebP's lossless header is not one")
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, nil
	case "VP8 ":
		if data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, fmt.Errorf("the WebP's key frame is not one")
		}
		return int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff), nil
	}
	return 0, 0, fmt.Errorf("the WebP holds no image chunk first")
}

// imageExt is the extension of an image by what it holds, which is how
// leptonica, under tesseract, tells what to read it as.
func imageExt(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG")):
		return ".png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8}):
		return ".jpg"
	case bytes.HasPrefix(data, []byte("GIF8")):
		return ".gif"
	case isWebP(data):
		return ".webp"
	}
	return ".img"
}
