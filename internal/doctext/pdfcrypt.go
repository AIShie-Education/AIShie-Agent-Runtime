package doctext

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" //nolint:gosec // PDF's standard security handler is defined with MD5 (ISO 32000-1 §7.6.3); it is read, not relied on.
	"crypto/rc4" //nolint:gosec // and with RC4 for files before AES; the runtime only opens what anyone may open.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
)

// PDF's standard security handler (ISO 32000-1 §7.6.3, ISO 32000-2 §7.6.4):
// a file encrypted so that anyone may open it (an empty user password,
// the owner's password guarding only printing, copying and changes) is
// read as any other, as every reader reads it; RC4 and AES of 128 and 256
// bits, revisions 2 to 6. A file that needs a password to open, or that is
// encrypted by another handler (certificates), is ErrEncrypted: the
// runtime never guesses a password.

type cryptMethod int

const (
	cryptNone cryptMethod = iota
	cryptRC4
	cryptAESV2
	cryptAESV3
)

// pdfCrypt is a file's key, and how its strings and streams are encrypted.
type pdfCrypt struct {
	key      []byte
	str, stm cryptMethod
	// skip is the number of the object that holds the encryption
	// dictionary, which is not itself encrypted.
	skip int
	// metadata: the document's XMP metadata stream is encrypted too.
	metadata bool
}

// padding is the password padding of §7.6.3.3.
var padding = []byte{
	0x28, 0xBF, 0x4E, 0x5E, 0x4E, 0x75, 0x8A, 0x41, 0x64, 0x00, 0x4E, 0x56, 0xFF, 0xFA, 0x01, 0x08,
	0x2E, 0x2E, 0x00, 0xB6, 0xD0, 0x68, 0x3E, 0x80, 0x2F, 0x0C, 0xA9, 0xFE, 0x64, 0x53, 0x69, 0x7A,
}

func errPassword() error { return fmt.Errorf("%w: it needs a password to open", ErrEncrypted) }

// newCrypt reads the encryption dictionary, and opens the file with the
// empty password, as the user's or the owner's.
func newCrypt(enc pdfDict, id0 []byte) (*pdfCrypt, error) {
	if asName(enc["Filter"]) != "Standard" {
		return nil, fmt.Errorf("%w: it is encrypted for its readers' certificates", ErrEncrypted)
	}
	v, _ := asInt(enc["V"])
	r, _ := asInt(enc["R"])
	o, _ := enc["O"].(pdfString)
	u, _ := enc["U"].(pdfString)
	pv, _ := asInt(enc["P"])
	c := &pdfCrypt{metadata: true, skip: -1}
	if b, ok := enc["EncryptMetadata"].(bool); ok {
		c.metadata = b
	}
	length := 40
	if n, ok := asInt(enc["Length"]); ok {
		length = n
	}
	switch v {
	case 1:
		c.str, c.stm, length = cryptRC4, cryptRC4, 40
	case 2:
		c.str, c.stm = cryptRC4, cryptRC4
	case 4, 5:
		cf, _ := enc["CF"].(pdfDict)
		method := func(name pdfName) (cryptMethod, int) {
			if name == "" || name == "Identity" {
				return cryptNone, 0
			}
			f, _ := cf[name].(pdfDict)
			n, _ := asInt(f["Length"])
			switch asName(f["CFM"]) {
			case "V2":
				return cryptRC4, n
			case "AESV2":
				return cryptAESV2, 16
			case "AESV3":
				return cryptAESV3, 32
			}
			return cryptNone, 0
		}
		var n1, n2 int
		c.str, n1 = method(pdfName(asName(enc["StrF"])))
		c.stm, n2 = method(pdfName(asName(enc["StmF"])))
		length = 128
		if n := max(n1, n2); n > 0 && n <= 32 {
			length = n * 8
		} else if n > 32 {
			length = n
		}
	default:
		return nil, fmt.Errorf("%w: its encryption is of a kind the runtime does not read", ErrEncrypted)
	}
	switch {
	case r >= 5:
		key, ok := aes256Key(r, []byte(o), []byte(u), stringOf(enc["OE"]), stringOf(enc["UE"]))
		if !ok {
			return nil, errPassword()
		}
		c.key = key
		return c, nil
	case r >= 2:
		n := length / 8
		if r == 2 {
			n = 5
		}
		if n < 5 || n > 16 || len(o) < 32 || len(u) < 16 {
			return nil, malformedf("its encryption dictionary is damaged")
		}
		pbytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(pbytes, uint32(int32(pv))) //nolint:gosec // P is a 32-bit field, however the file writes it.
		if key := rc4Key(padding, []byte(o[:32]), pbytes, id0, r, n, c.metadata); checkUser(key, []byte(u), id0, r) {
			c.key = key
			return c, nil
		}
		// The owner's empty password opens it too, through the user's.
		user := ownerToUser([]byte(o[:32]), r, n)
		if key := rc4Key(user, []byte(o[:32]), pbytes, id0, r, n, c.metadata); checkUser(key, []byte(u), id0, r) {
			c.key = key
			return c, nil
		}
		return nil, errPassword()
	}
	return nil, fmt.Errorf("%w: its encryption is of a kind the runtime does not read", ErrEncrypted)
}

func stringOf(v any) []byte {
	s, _ := v.(pdfString)
	return []byte(s)
}

// rc4Key is Algorithm 2: the file's key from a padded password.
func rc4Key(pw, o, p, id0 []byte, r, n int, metadata bool) []byte {
	h := md5.New() //nolint:gosec // §7.6.3.3 Algorithm 2.
	h.Write(pw[:32])
	h.Write(o)
	h.Write(p)
	h.Write(id0)
	if r >= 4 && !metadata {
		h.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	}
	key := h.Sum(nil)
	if r >= 3 {
		for range 50 {
			s := md5.Sum(key[:n]) //nolint:gosec // §7.6.3.3 Algorithm 2 step f.
			key = s[:]
		}
	}
	return key[:n]
}

// checkUser is Algorithms 4 and 5: whether key opens the file.
func checkUser(key, u, id0 []byte, r int) bool {
	if r == 2 {
		got := rc4Crypt(key, padding)
		return len(u) >= 32 && bytes.Equal(got, u[:32])
	}
	h := md5.New() //nolint:gosec // §7.6.3.4 Algorithm 5.
	h.Write(padding)
	h.Write(id0)
	x := rc4Crypt(key, h.Sum(nil))
	for i := 1; i <= 19; i++ {
		x = rc4Crypt(xorKey(key, byte(i)), x)
	}
	return bytes.Equal(x, u[:16])
}

// ownerToUser is Algorithm 7 with the empty owner password: the padded
// user password the owner's key hides in O.
func ownerToUser(o []byte, r, n int) []byte {
	s := md5.Sum(padding) //nolint:gosec // §7.6.3.4 Algorithm 3.
	key := s[:]
	if r >= 3 {
		for range 50 {
			s = md5.Sum(key[:n]) //nolint:gosec // Algorithm 3 step c.
			key = s[:]
		}
	}
	key = key[:n]
	if r == 2 {
		return rc4Crypt(key, o)
	}
	x := bytes.Clone(o)
	for i := 19; i >= 0; i-- {
		x = rc4Crypt(xorKey(key, byte(i)), x)
	}
	return x
}

func xorKey(key []byte, b byte) []byte {
	out := make([]byte, len(key))
	for i, k := range key {
		out[i] = k ^ b
	}
	return out
}

func rc4Crypt(key, data []byte) []byte {
	c, err := rc4.NewCipher(key) //nolint:gosec // PDF's RC4 encryption, read only.
	if err != nil {
		return nil
	}
	out := make([]byte, len(data))
	c.XORKeyStream(out, data)
	return out
}

// aes256Key opens a file of revision 5 or 6 with the empty password, as
// the user's or the owner's: the file's key, and whether it opened.
func aes256Key(r int, o, u, oe, ue []byte) ([]byte, bool) {
	if len(u) < 48 || len(ue) < 32 {
		return nil, false
	}
	if bytes.Equal(hash6(r, nil, u[32:40], nil), u[:32]) {
		return aesUnwrap(hash6(r, nil, u[40:48], nil), ue[:32])
	}
	if len(o) >= 48 && len(oe) >= 32 && bytes.Equal(hash6(r, nil, o[32:40], u[:48]), o[:32]) {
		return aesUnwrap(hash6(r, nil, o[40:48], u[:48]), oe[:32])
	}
	return nil, false
}

// aesUnwrap decrypts a 32-byte key with AES-256 in CBC mode and a zero IV.
func aesUnwrap(kek, wrapped []byte) ([]byte, bool) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, false
	}
	out := make([]byte, len(wrapped))
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(out, wrapped)
	return out, true
}

// hash6 is the hash of ISO 32000-2 Algorithm 2.B (revision 6), or SHA-256
// alone (revision 5).
func hash6(r int, pw, salt, udata []byte) []byte {
	k := sha256.Sum256(append(append(append([]byte(nil), pw...), salt...), udata...))
	key := k[:]
	if r == 5 {
		return key
	}
	// At least 64 rounds, then until the last byte of E is no more than the
	// rounds done less 32: never more than 288.
	for i := 0; i < 512; i++ {
		seq := append(append(append([]byte(nil), pw...), key...), udata...)
		k1 := bytes.Repeat(seq, 64)
		block, err := aes.NewCipher(key[:16])
		if err != nil {
			return nil
		}
		e := make([]byte, len(k1))
		cipher.NewCBCEncrypter(block, key[16:32]).CryptBlocks(e, k1)
		sum := 0
		for _, b := range e[:16] {
			sum += int(b)
		}
		switch sum % 3 {
		case 0:
			s := sha256.Sum256(e)
			key = s[:]
		case 1:
			s := sha512.Sum384(e)
			key = s[:]
		default:
			s := sha512.Sum512(e)
			key = s[:]
		}
		if i >= 63 && int(e[len(e)-1]) <= i+1-32 {
			break
		}
	}
	return key[:32]
}

// objectKey is the key of one object's strings and streams (Algorithm 1).
func (c *pdfCrypt) objectKey(m cryptMethod, num, gen int) []byte {
	if m == cryptAESV3 {
		return c.key
	}
	h := md5.New() //nolint:gosec // §7.6.2 Algorithm 1.
	h.Write(c.key)
	//nolint:gosec // Algorithm 1 takes the low three bytes of the number and two of the generation.
	h.Write([]byte{byte(num), byte(num >> 8), byte(num >> 16), byte(gen), byte(gen >> 8)})
	if m == cryptAESV2 {
		h.Write([]byte("sAlT"))
	}
	return h.Sum(nil)[:min(len(c.key)+5, 16)]
}

func (c *pdfCrypt) decrypt(m cryptMethod, data []byte, num, gen int) []byte {
	switch m {
	case cryptRC4:
		return rc4Crypt(c.objectKey(m, num, gen), data)
	case cryptAESV2, cryptAESV3:
		if len(data) < 2*aes.BlockSize || len(data)%aes.BlockSize != 0 {
			return nil
		}
		block, err := aes.NewCipher(c.objectKey(m, num, gen))
		if err != nil {
			return nil
		}
		out := make([]byte, len(data)-aes.BlockSize)
		cipher.NewCBCDecrypter(block, data[:aes.BlockSize]).CryptBlocks(out, data[aes.BlockSize:])
		if n := int(out[len(out)-1]); n >= 1 && n <= aes.BlockSize && n <= len(out) {
			out = out[:len(out)-n]
		}
		return out
	}
	return data
}

// stream decrypts a stream's bytes.
func (c *pdfCrypt) stream(data []byte, num, gen int, metadata bool) []byte {
	if metadata && !c.metadata {
		return data
	}
	return c.decrypt(c.stm, data, num, gen)
}

// strings decrypts the strings of object num, in place where they are in
// arrays and dictionaries (a stream's dictionary among them).
func (c *pdfCrypt) strings(v any, num, gen int) any {
	switch x := v.(type) {
	case pdfString:
		return pdfString(c.decrypt(c.str, []byte(x), num, gen))
	case pdfArray:
		for i, e := range x {
			x[i] = c.strings(e, num, gen)
		}
	case pdfDict:
		for k, e := range x {
			x[k] = c.strings(e, num, gen)
		}
	case *pdfStream:
		c.strings(x.dict, num, gen)
	}
	return v
}
