package vault

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Schemes KMS_KEY_ID may name.
const (
	SchemeLocal  = "local:"
	SchemeAWSKMS = "awskms:"
	SchemeVault  = "vault:"
)

// OpenKEK is the KEK KMS_KEY_ID names: local:<dir>/<name>, the key in the
// file <dir>/<name>, with every key in <dir> kept for unwrapping. awskms:
// (AWS KMS, whose encryption context would be the additional data's
// fields) and vault: (HashiCorp Vault's transit engine) are refused: they
// are not built yet.
func OpenKEK(kmsKeyID string) (KEK, error) {
	switch {
	case strings.HasPrefix(kmsKeyID, SchemeLocal):
		return OpenLocal(strings.TrimPrefix(kmsKeyID, SchemeLocal))
	case strings.HasPrefix(kmsKeyID, SchemeAWSKMS):
		return nil, errors.New("KMS_KEY_ID: awskms: is not built yet; use local:<dir>/<name>")
	case strings.HasPrefix(kmsKeyID, SchemeVault):
		return nil, errors.New("KMS_KEY_ID: vault: is not built yet; use local:<dir>/<name>")
	}
	// The value is not repeated: it may be the key itself, pasted where
	// its name belongs.
	return nil, errors.New("KMS_KEY_ID: not local:<dir>/<name>, such as local:/secrets/kek/v1 (awskms: and vault: are not built yet)")
}

// KeyringDir is the directory of the keyring a local: KMS_KEY_ID names,
// which no secret:// or file:// reference may read; "" for any other.
func KeyringDir(kmsKeyID string) string {
	path, ok := strings.CutPrefix(kmsKeyID, SchemeLocal)
	if !ok || path == "" {
		return ""
	}
	return filepath.Dir(filepath.Clean(path))
}

// Open is the vault KMS_KEY_ID names (OpenKEK).
func Open(kmsKeyID string) (*Vault, error) {
	kek, err := OpenKEK(kmsKeyID)
	if err != nil {
		return nil, err
	}
	return New(kek), nil
}

// Local is a KEK kept in files: a keyring of 32-byte keys, one per file of
// a directory, each base64. One of them, the current one, wraps; any of
// them unwraps what it wrapped. A key's id is local:<its file's name>, so
// that it stays the same wherever the directory is mounted.
type Local struct {
	current string
	keys    map[string][]byte
	rand    io.Reader
}

// keyNameRe is what a key file may be called.
var keyNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// maxKeyFile bounds a key file: 32 bytes are 44 characters in base64.
const maxKeyFile = 1 << 10

// OpenLocal reads the keyring of path's directory, path's file being the
// current key. Files whose names begin with '.' are passed over, as are
// directories (a mounted Kubernetes secret's ..data among them); every
// other file must be a key, or the keyring is refused, naming the file.
// Errors never repeat path, which KMS_KEY_ID gave: a key pasted where its
// file's name belongs would be repeated with it.
func OpenLocal(path string) (*Local, error) {
	if path == "" {
		return nil, errors.New("KMS_KEY_ID: local: names no file; give local:<dir>/<name>, such as local:/secrets/kek/v1")
	}
	dir, name := filepath.Split(filepath.Clean(path))
	if dir == "" {
		dir = "."
	}
	if !keyNameRe.MatchString(name) {
		return nil, errors.New("KMS_KEY_ID: the key's file name is not letters, digits, '.', '_' and '-'")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("KMS_KEY_ID: its directory cannot be read: %w", pathless(err))
	}
	l := &Local{current: SchemeLocal + name, keys: map[string][]byte{}, rand: rand.Reader}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, n)) // follows a link, as a mounted secret's files are
		if err != nil {
			return nil, fmt.Errorf("KMS_KEY_ID: the key file %q in its directory: %w", n, pathless(err))
		}
		if info.IsDir() {
			continue
		}
		if !keyNameRe.MatchString(n) {
			return nil, fmt.Errorf("KMS_KEY_ID: its directory holds %q, whose name is not a key's (letters, digits, '.', '_' and '-'): the directory is the keyring's alone", n)
		}
		key, err := readKey(filepath.Join(dir, n), info)
		if err != nil {
			return nil, fmt.Errorf("KMS_KEY_ID: the key file %q in its directory %w", n, err)
		}
		l.keys[SchemeLocal+n] = key
	}
	if _, ok := l.keys[l.current]; !ok {
		return nil, errors.New("KMS_KEY_ID: its directory has no key file of the name it gives")
	}
	return l, nil
}

// pathless is err without the path an *fs.PathError names.
func pathless(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// readKey reads one key file: 32 bytes, base64 (standard or URL, padded or
// not), with the whitespace around them ignored. What is wrong with it is
// said without a byte of it.
func readKey(path string, info os.FileInfo) ([]byte, error) {
	if !info.Mode().IsRegular() {
		return nil, errors.New("is not a regular file")
	}
	if info.Size() > maxKeyFile {
		return nil, errors.New("is too large to be a key: a key is 32 bytes, base64")
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the operator names the keyring (KMS_KEY_ID).
	if err != nil {
		return nil, fmt.Errorf("cannot be read: %w", pathless(err))
	}
	defer clear(b)
	text := strings.TrimSpace(string(b))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(text); err == nil {
			if len(key) != dekSize {
				clear(key)
				return nil, fmt.Errorf("is %d bytes, not %d: make one with openssl rand -base64 32", len(key), dekSize)
			}
			return key, nil
		}
	}
	return nil, errors.New("is not base64: make one with openssl rand -base64 32")
}

// ID is the current key's id, local:<name>.
func (l *Local) ID() string { return l.current }

// IDs are the ids of every key in the keyring, sorted.
func (l *Local) IDs() []string {
	ids := make([]string, 0, len(l.keys))
	for id := range l.keys {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Wrap wraps dek under the current key: AES-256-GCM, a random nonce before
// the sealed bytes, bound to aad.
func (l *Local) Wrap(_ context.Context, dek, aad []byte) (string, []byte, error) {
	nonce, sealed, err := seal(l.keys[l.current], dek, aad, l.rand)
	if err != nil {
		return "", nil, err
	}
	return l.current, append(nonce, sealed...), nil
}

// Unwrap unwraps what Wrap wrapped, with the key kekID of the keyring.
func (l *Local) Unwrap(_ context.Context, kekID string, wrapped, aad []byte) ([]byte, error) {
	key, ok := l.keys[kekID]
	if !ok {
		return nil, fmt.Errorf("the keyring holds no key %s (KMS_KEY_ID's directory: add its file back, or rewrap before removing it)", kekID)
	}
	const nonceSize = 12
	if len(wrapped) < nonceSize {
		return nil, errNotOpened
	}
	return unseal(key, wrapped[:nonceSize], wrapped[nonceSize:], aad)
}
