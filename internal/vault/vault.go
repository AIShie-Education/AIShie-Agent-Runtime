// Package vault seals the secrets the runtime keeps in its own store: the
// Core tokens of hosted agents and their owners' model keys (docs/design.md
// §11). It is envelope encryption:
//
//   - each secret has a data key of its own (DEK), 256 random bits, and is
//     encrypted under it with AES-256-GCM;
//   - the additional data of that encryption is "aishie/secret/v1", the
//     secret's id, its tenant and its kind, each length-prefixed, so that a
//     ciphertext moved to another row, tenant or kind does not open;
//   - the DEK is wrapped by a key-encryption key (KEK), bound to the same
//     additional data, and the row keeps the KEK's id beside it.
//
// The KEK is chosen by KMS_KEY_ID. local:<dir>/<name> is a 32-byte key,
// base64, in the file <dir>/<name>; every other file in <dir> is kept too,
// so that secrets wrapped by an older key still open while `aishie-runtime
// keys rewrap` moves them to the newer one. awskms: and vault: are refused
// until they are built; the KEK interface is what they will implement.
//
// Plaintext exists only in the caller's hands: Seal takes it and Open gives
// it. No error of this package holds a secret, a key or plaintext.
package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// KEK wraps and unwraps data keys: the key-encryption key KMS_KEY_ID names.
// An implementation holds its key where the runtime cannot read it back if
// it can (a KMS), and never logs or returns it.
type KEK interface {
	// ID names the key Wrap wraps with, as a secret's kek_id records it.
	ID() string
	// Wrap wraps dek, bound to aad, under the current key, and returns
	// that key's id with the wrapped bytes.
	Wrap(ctx context.Context, dek, aad []byte) (kekID string, wrapped []byte, err error)
	// Unwrap unwraps a data key that the key kekID wrapped, bound to aad.
	// A key it does not hold, or bytes or additional data that are not
	// those wrapped, is an error.
	Unwrap(ctx context.Context, kekID string, wrapped, aad []byte) ([]byte, error)
}

// Vault seals and opens secrets with a KEK.
type Vault struct {
	kek  KEK
	rand io.Reader
}

// New is a vault on kek.
func New(kek KEK) *Vault { return &Vault{kek: kek, rand: rand.Reader} }

// KEKID is the id of the key the vault wraps new data keys with.
func (v *Vault) KEKID() string { return v.kek.ID() }

// dekSize is a data key's size: AES-256.
const dekSize = 32

// label begins every secret's additional data, and names its version.
const label = "aishie/secret/v1"

// AAD is the additional data a secret is bound to: label, id, tenant and
// kind, each as a 4-byte big-endian length and its bytes, so that no two
// different rows give the same bytes.
func AAD(id, tenant, kind string) []byte {
	var b []byte
	for _, f := range []string{label, id, tenant, kind} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f))) // #nosec G115 -- ids are short; a length past 4 GiB cannot be built here.
		b = append(b, f...)
	}
	return b
}

// NewSecretID is a new secret's id: sec_ and a random UUID.
func NewSecretID() string { return "sec_" + uuid.NewString() }

// Seal encrypts plaintext as the secret s names (its ID, TenantID and Kind
// must be set; CreatedBy is kept as given), and returns the row to store:
// the ciphertext, the wrapped DEK, the KEK's id and the hint.
func (v *Vault) Seal(ctx context.Context, s store.Secret, plaintext string) (store.Secret, error) {
	if !store.IsSecretID(s.ID) || s.TenantID == "" || (s.Kind != store.SecretCoreToken && s.Kind != store.SecretModelKey) {
		return store.Secret{}, errors.New("vault: a secret to seal needs an id (sec_…), a tenant and a kind")
	}
	if plaintext == "" {
		return store.Secret{}, fmt.Errorf("vault: secret %s: nothing to seal", s.ID)
	}
	aad := AAD(s.ID, s.TenantID, s.Kind)
	dek := make([]byte, dekSize)
	defer clear(dek)
	if _, err := io.ReadFull(v.rand, dek); err != nil {
		return store.Secret{}, fmt.Errorf("vault: secret %s: a data key: %w", s.ID, err)
	}
	nonce, ciphertext, err := seal(dek, []byte(plaintext), aad, v.rand)
	if err != nil {
		return store.Secret{}, fmt.Errorf("vault: secret %s: %w", s.ID, err)
	}
	kekID, wrapped, err := v.kek.Wrap(ctx, dek, aad)
	if err != nil {
		return store.Secret{}, fmt.Errorf("vault: secret %s: wrap its data key: %w", s.ID, err)
	}
	s.KEKID, s.WrappedDEK, s.Nonce, s.Ciphertext = kekID, wrapped, nonce, ciphertext
	s.Hint = Hint(s.Kind, plaintext)
	return s, nil
}

// Open decrypts s. It fails, saying so and never why in a way that holds a
// secret, when s's data key is wrapped by a key the keyring lacks, or when
// its bytes, its id, its tenant or its kind are not those it was sealed
// with.
func (v *Vault) Open(ctx context.Context, s *store.Secret) (string, error) {
	b, err := v.open(ctx, s)
	if err != nil {
		return "", err
	}
	defer clear(b)
	return string(b), nil
}

// Check opens s and forgets what it holds: `aishie-runtime keys check`.
func (v *Vault) Check(ctx context.Context, s *store.Secret) error {
	b, err := v.open(ctx, s)
	clear(b)
	return err
}

func (v *Vault) open(ctx context.Context, s *store.Secret) ([]byte, error) {
	aad := AAD(s.ID, s.TenantID, s.Kind)
	dek, err := v.kek.Unwrap(ctx, s.KEKID, s.WrappedDEK, aad)
	if err != nil {
		return nil, fmt.Errorf("vault: secret %s: unwrap its data key: %w", s.ID, err)
	}
	defer clear(dek)
	b, err := unseal(dek, s.Nonce, s.Ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("vault: secret %s: %w", s.ID, err)
	}
	return b, nil
}

// Rewrap unwraps s's data key with the keyring and wraps it again with the
// current KEK, bound to the same additional data. The ciphertext is not
// touched. It returns the new key's id and wrapped bytes, for
// store.Secrets.RewrapSecret.
func (v *Vault) Rewrap(ctx context.Context, s *store.Secret) (string, []byte, error) {
	aad := AAD(s.ID, s.TenantID, s.Kind)
	dek, err := v.kek.Unwrap(ctx, s.KEKID, s.WrappedDEK, aad)
	if err != nil {
		return "", nil, fmt.Errorf("vault: secret %s: unwrap its data key: %w", s.ID, err)
	}
	defer clear(dek)
	// A rewrap must not leave a secret that no longer opens: the data key
	// is proved against the ciphertext before it is wrapped again.
	b, err := unseal(dek, s.Nonce, s.Ciphertext, aad)
	clear(b)
	if err != nil {
		return "", nil, fmt.Errorf("vault: secret %s: %w", s.ID, err)
	}
	kekID, wrapped, err := v.kek.Wrap(ctx, dek, aad)
	if err != nil {
		return "", nil, fmt.Errorf("vault: secret %s: wrap its data key: %w", s.ID, err)
	}
	return kekID, wrapped, nil
}

// errNotOpened is every failure of AES-GCM to open: the key is not the one,
// or the bytes or the additional data changed. It says nothing more, since
// there is nothing more that can be told apart.
var errNotOpened = errors.New("it does not open: its key, its bytes or its binding (id, tenant, kind) are not those it was sealed with")

// seal encrypts plaintext under key with AES-256-GCM and a random nonce.
func seal(key, plaintext, aad []byte, r io.Reader) (nonce, ciphertext []byte, err error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(r, nonce); err != nil {
		return nil, nil, fmt.Errorf("a nonce: %w", err)
	}
	return nonce, aead.Seal(nil, nonce, plaintext, aad), nil
}

// unseal decrypts what seal encrypted.
func unseal(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errNotOpened
	}
	b, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errNotOpened
	}
	return b, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	if len(key) != dekSize {
		return nil, fmt.Errorf("a key of %d bytes, not %d", len(key), dekSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
