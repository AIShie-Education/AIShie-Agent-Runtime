package safety

import (
	"strings"
	"unicode/utf8"
)

// hostSeparators are what punycode.js's toASCII reads as dots.
func isHostSeparator(r rune) bool { return r == '.' || r == '。' || r == '．' || r == '｡' }

// punycodeHost is host as markdown-it's normalizeLink sends it to the
// browser: each label with a character that is not ASCII in punycode
// (RFC 3492), prefixed "xn--"; the others as they are.
func punycodeHost(host string) string {
	labels := strings.FieldsFunc(host, isHostSeparator)
	for i, l := range labels {
		if !isASCII(l) {
			labels[i] = "xn--" + punycode(l)
		}
	}
	return strings.Join(labels, ".")
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// punycode is RFC 3492's encoding of s, as punycode.js makes it.
func punycode(s string) string {
	const (
		base, tMin, tMax, skew, damp = 36, 1, 26, 38, 700
		initialBias, initialN        = 72, 128
	)
	input := []rune(s)
	var out []byte
	for _, r := range input {
		if r < 0x80 {
			out = append(out, byte(r))
		}
	}
	basic := len(out)
	handled := basic
	if basic > 0 {
		out = append(out, '-')
	}
	const digits = "abcdefghijklmnopqrstuvwxyz0123456789"
	digit := func(d int) byte { return digits[d] }
	adapt := func(delta, numPoints int, first bool) int {
		if first {
			delta /= damp
		} else {
			delta /= 2
		}
		delta += delta / numPoints
		k := 0
		for delta > ((base-tMin)*tMax)/2 {
			delta /= base - tMin
			k += base
		}
		return k + (base-tMin+1)*delta/(delta+skew)
	}
	n, delta, bias := initialN, 0, initialBias
	for handled < len(input) {
		m := int(^uint(0) >> 1)
		for _, r := range input {
			if int(r) >= n && int(r) < m {
				m = int(r)
			}
		}
		delta += (m - n) * (handled + 1)
		n = m
		for _, r := range input {
			if int(r) < n {
				delta++
			}
			if int(r) != n {
				continue
			}
			q := delta
			for k := base; ; k += base {
				t := k - bias
				if t < tMin {
					t = tMin
				} else if t > tMax {
					t = tMax
				}
				if q < t {
					break
				}
				out = append(out, digit(t+(q-t)%(base-t)))
				q = (q - t) / (base - t)
			}
			out = append(out, digit(q))
			bias = adapt(delta, handled+1, handled == basic)
			delta = 0
			handled++
		}
		delta++
		n++
	}
	return string(out)
}
