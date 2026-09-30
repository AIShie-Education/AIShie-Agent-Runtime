package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// Secrets shaped as the real ones are. None of them is live.
const (
	coreToken = "ais_k7v2m4qhx3ab_9Jx2abcDEFghiJKLmnoPQRstuVWXyz0123456789_-abcd"
	modelKey  = "sk-proj-Ab3dEf6hIj9kLm2nOp5qRs8tUv1wXy4z3f9a"
)

// keyring writes 32-byte keys, base64, as files named names in a directory
// of its own, and returns the directory.
func keyring(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		addKey(t, dir, n)
	}
	return dir
}

func addKey(t *testing.T, dir, name string) {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustVault(t *testing.T, kmsKeyID string) *Vault {
	t.Helper()
	v, err := Open(kmsKeyID)
	if err != nil {
		t.Fatalf("Open(%s): %v", kmsKeyID, err)
	}
	return v
}

func mustSeal(t *testing.T, v *Vault, id, tenant, kind, plaintext string) store.Secret {
	t.Helper()
	s, err := v.Seal(context.Background(), store.Secret{ID: id, TenantID: tenant, Kind: kind, CreatedBy: "person-1"}, plaintext)
	if err != nil {
		t.Fatalf("Seal(%s): %v", id, err)
	}
	return s
}

// noSecret fails when err's text holds any of the secrets, whole or their
// secret halves.
func noSecret(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range []string{coreToken, modelKey} {
		for _, part := range []string{s, s[len(s)/2:]} {
			if strings.Contains(err.Error(), part) {
				t.Fatalf("%s: the error holds a secret: %v", what, err)
			}
		}
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	dir := keyring(t, "v1")
	v := mustVault(t, "local:"+filepath.Join(dir, "v1"))
	ctx := context.Background()
	for _, c := range []struct{ kind, plaintext, hint string }{
		{store.SecretCoreToken, coreToken, "ais_k7v2m4qhx3ab…"},
		{store.SecretModelKey, modelKey, "sk-proj-…3f9a"},
	} {
		s := mustSeal(t, v, "sec_"+c.kind, "ten_owner", c.kind, c.plaintext)
		if s.KEKID != "local:v1" || s.Hint != c.hint || s.CreatedBy != "person-1" || s.TenantID != "ten_owner" || s.Kind != c.kind {
			t.Errorf("sealed %+v", s)
		}
		if bytes.Contains(s.Ciphertext, []byte(c.plaintext)) || bytes.Contains(s.WrappedDEK, []byte(c.plaintext)) {
			t.Error("the ciphertext holds the plaintext")
		}
		if err := store.CheckSecret(s); err != nil {
			t.Errorf("a store would refuse the sealed secret: %v", err)
		}
		got, err := v.Open(ctx, &s)
		if err != nil || got != c.plaintext {
			t.Fatalf("Open = %q, %v", got, err)
		}
		if err := v.Check(ctx, &s); err != nil {
			t.Fatalf("Check: %v", err)
		}
	}
	// Each seal has a data key and a nonce of its own.
	a := mustSeal(t, v, "sec_1", "ten_owner", store.SecretCoreToken, coreToken)
	b := mustSeal(t, v, "sec_1", "ten_owner", store.SecretCoreToken, coreToken)
	if bytes.Equal(a.Ciphertext, b.Ciphertext) || bytes.Equal(a.WrappedDEK, b.WrappedDEK) || bytes.Equal(a.Nonce, b.Nonce) {
		t.Error("two seals of one secret share bytes")
	}
}

func TestSealRefusesWhatIsNotASecret(t *testing.T) {
	v := mustVault(t, "local:"+filepath.Join(keyring(t, "v1"), "v1"))
	for name, s := range map[string]store.Secret{
		"no id":         {TenantID: "t", Kind: store.SecretCoreToken},
		"a bad id":      {ID: "key_1", TenantID: "t", Kind: store.SecretCoreToken},
		"no tenant":     {ID: "sec_1", Kind: store.SecretCoreToken},
		"an odd kind":   {ID: "sec_1", TenantID: "t", Kind: "password"},
		"an empty kind": {ID: "sec_1", TenantID: "t"},
	} {
		_, err := v.Seal(context.Background(), s, coreToken)
		if err == nil {
			t.Errorf("%s: sealed", name)
		}
		noSecret(t, name, err)
	}
	if _, err := v.Seal(context.Background(), store.Secret{ID: "sec_1", TenantID: "t", Kind: store.SecretModelKey}, ""); err == nil {
		t.Error("an empty secret was sealed")
	}
}

// A sealed secret opens only as it was sealed: any byte of it changed, or
// moved to another id, tenant or kind, and it does not, and says so
// without a byte of it.
func TestTamperingFails(t *testing.T) {
	v := mustVault(t, "local:"+filepath.Join(keyring(t, "v1", "v2"), "v1"))
	ctx := context.Background()
	orig := mustSeal(t, v, "sec_a", "ten_owner", store.SecretCoreToken, coreToken)
	flip := func(b []byte, i int) []byte {
		b = slices.Clone(b)
		b[i%len(b)] ^= 0x01
		return b
	}
	for name, mutate := range map[string]func(*store.Secret){
		"another id":                 func(s *store.Secret) { s.ID = "sec_b" },
		"another tenant":             func(s *store.Secret) { s.TenantID = "ten_other" },
		"another kind":               func(s *store.Secret) { s.Kind = store.SecretModelKey },
		"fields run together":        func(s *store.Secret) { s.ID, s.TenantID = "sec_aten", "_owner" },
		"a ciphertext byte":          func(s *store.Secret) { s.Ciphertext = flip(s.Ciphertext, 3) },
		"the ciphertext's tag":       func(s *store.Secret) { s.Ciphertext = flip(s.Ciphertext, len(s.Ciphertext)-1) },
		"the ciphertext cut short":   func(s *store.Secret) { s.Ciphertext = s.Ciphertext[:len(s.Ciphertext)-1] },
		"the nonce":                  func(s *store.Secret) { s.Nonce = flip(s.Nonce, 0) },
		"a short nonce":              func(s *store.Secret) { s.Nonce = s.Nonce[:4] },
		"a wrapped key byte":         func(s *store.Secret) { s.WrappedDEK = flip(s.WrappedDEK, 20) },
		"the wrapped key's nonce":    func(s *store.Secret) { s.WrappedDEK = flip(s.WrappedDEK, 0) },
		"a wrapped key cut short":    func(s *store.Secret) { s.WrappedDEK = s.WrappedDEK[:8] },
		"another key of the keyring": func(s *store.Secret) { s.KEKID = "local:v2" },
		"a key the keyring lacks":    func(s *store.Secret) { s.KEKID = "local:v9" },
		"another secret's wrapped key": func(s *store.Secret) {
			other := mustSeal(t, v, "sec_a", "ten_owner", store.SecretCoreToken, modelKey)
			s.WrappedDEK = other.WrappedDEK
		},
	} {
		s := orig
		mutate(&s)
		got, err := v.Open(ctx, &s)
		if err == nil {
			t.Errorf("%s: opened as %q", name, got)
			continue
		}
		noSecret(t, name, err)
		if err := v.Check(ctx, &s); err == nil {
			t.Errorf("%s: Check passed", name)
		}
		if _, _, err := v.Rewrap(ctx, &s); err == nil {
			t.Errorf("%s: rewrapped", name)
		}
	}
	if got, err := v.Open(ctx, &orig); err != nil || got != coreToken {
		t.Fatalf("the original no longer opens: %v", err)
	}
}

// Rotation, as docs/deploying.md has it: add v2, point KMS_KEY_ID at it,
// rewrap, then retire v1.
func TestRotation(t *testing.T) {
	ctx := context.Background()
	dir := keyring(t, "v1")
	v1 := mustVault(t, "local:"+filepath.Join(dir, "v1"))
	s := mustSeal(t, v1, "sec_1", "ten_owner", store.SecretModelKey, modelKey)

	addKey(t, dir, "v2")
	v2 := mustVault(t, "local:"+filepath.Join(dir, "v2"))
	if v2.KEKID() != "local:v2" {
		t.Fatalf("KEKID = %s", v2.KEKID())
	}
	// v1's secrets still open with v2 current: the keyring keeps v1.
	if got, err := v2.Open(ctx, &s); err != nil || got != modelKey {
		t.Fatalf("an old secret with the new key current: %q, %v", got, err)
	}
	// New secrets are v2's.
	if n := mustSeal(t, v2, "sec_2", "ten_owner", store.SecretCoreToken, coreToken); n.KEKID != "local:v2" {
		t.Fatalf("a new secret is wrapped by %s", n.KEKID)
	}
	kekID, wrapped, err := v2.Rewrap(ctx, &s)
	if err != nil {
		t.Fatal(err)
	}
	if kekID != "local:v2" || bytes.Equal(wrapped, s.WrappedDEK) {
		t.Fatalf("rewrapped to %s", kekID)
	}
	rewrapped := s
	rewrapped.KEKID, rewrapped.WrappedDEK = kekID, wrapped

	// v1 retired: what was rewrapped opens, what was not does not.
	if err := os.Remove(filepath.Join(dir, "v1")); err != nil {
		t.Fatal(err)
	}
	only2 := mustVault(t, "local:"+filepath.Join(dir, "v2"))
	if got, err := only2.Open(ctx, &rewrapped); err != nil || got != modelKey {
		t.Fatalf("the rewrapped secret after v1 retired: %q, %v", got, err)
	}
	_, err = only2.Open(ctx, &s)
	if err == nil || !strings.Contains(err.Error(), "no key local:v1") {
		t.Fatalf("a secret v1 still wraps, after v1 retired: %v", err)
	}
	// The ciphertext was never touched.
	if !bytes.Equal(rewrapped.Ciphertext, s.Ciphertext) || !bytes.Equal(rewrapped.Nonce, s.Nonce) {
		t.Error("a rewrap changed the ciphertext")
	}
}

func TestAADIsLengthPrefixed(t *testing.T) {
	seen := map[string]string{}
	for _, f := range [][3]string{
		{"sec_ab", "c", "core_token"}, {"sec_a", "bc", "core_token"}, {"sec_a", "b", "ccore_token"},
		{"sec_a", "", "bcore_token"}, {"", "sec_ab", "core_token"},
	} {
		k := string(AAD(f[0], f[1], f[2]))
		if prev, ok := seen[k]; ok {
			t.Errorf("AAD%v = AAD%s", f, prev)
		}
		seen[k] = strings.Join(f[:], ",")
	}
	if !bytes.HasPrefix(AAD("sec_a", "t", "k"), append([]byte{0, 0, 0, 16}, "aishie/secret/v1"...)) {
		t.Errorf("AAD does not begin with the label: %q", AAD("sec_a", "t", "k"))
	}
}

func TestOpenKEK(t *testing.T) {
	dir := keyring(t, "v1", "v2")
	// A key with whitespace around it, and in URL-safe base64 unpadded.
	if err := os.WriteFile(filepath.Join(dir, "v3"), []byte("  "+base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Passed over: hidden files and directories, as a mounted Kubernetes
	// secret has.
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "..data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "old"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A link, as a mounted secret's files are.
	if err := os.Symlink(filepath.Join(dir, "v2"), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	kek, err := OpenKEK("local:" + filepath.Join(dir, "v3"))
	if err != nil {
		t.Fatal(err)
	}
	l, ok := kek.(*Local)
	if !ok {
		t.Fatalf("OpenKEK gave a %T", kek)
	}
	if l.ID() != "local:v3" || !slices.Equal(l.IDs(), []string{"local:current", "local:v1", "local:v2", "local:v3"}) {
		t.Fatalf("ID %s, IDs %v", l.ID(), l.IDs())
	}
	// A relative path is the directory's, from where the process runs.
	t.Chdir(dir)
	if _, err := OpenKEK("local:v1"); err != nil {
		t.Errorf("a relative path: %v", err)
	}
}

// A keyring that cannot be used is refused, saying why, and never with a
// byte of a key or of what KMS_KEY_ID says: a key pasted there would be
// repeated.
func TestOpenKEKRefuses(t *testing.T) {
	pasted := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))
	good := keyring(t, "v1")
	bad := func(content string) string {
		d := keyring(t, "v1")
		if err := os.WriteFile(filepath.Join(d, "v2"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return d
	}
	stray := keyring(t, "v1")
	if err := os.WriteFile(filepath.Join(stray, "README md"), []byte("keys"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := keyring(t, "v1")
	if err := os.WriteFile(filepath.Join(big, "v2"), bytes.Repeat([]byte("A"), 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ id, want string }{
		{"", "not local:"},
		{pasted, "not local:"},
		{"awskms:arn:aws:kms:us-east-1:111122223333:key/abc", "awskms: is not built yet"},
		{"vault:transit/aishie", "vault: is not built yet"},
		{"local:", "names no file"},
		{"local:" + pasted, "file name is not"},
		{"local:" + filepath.Join(t.TempDir(), "missing", "v1"), "its directory cannot be read"},
		{"local:" + filepath.Join(good, "v2"), "no key file of the name it gives"},
		{"local:" + filepath.Join(bad("not base64 !"), "v1"), `"v2" in its directory is not base64`},
		{"local:" + filepath.Join(bad(base64.StdEncoding.EncodeToString([]byte("short"))), "v1"), "is 5 bytes, not 32"},
		{"local:" + filepath.Join(stray, "v1"), `holds "README md"`},
		{"local:" + filepath.Join(big, "v1"), "too large"},
	} {
		_, err := OpenKEK(c.id)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("OpenKEK(%q) = %v, want an error saying %q", c.id, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), pasted) || strings.Contains(err.Error(), pasted[4:20]) {
			t.Errorf("OpenKEK(%q): the error repeats what KMS_KEY_ID says: %v", c.id, err)
		}
	}
}

func TestHint(t *testing.T) {
	for _, c := range []struct{ kind, secret, want string }{
		{store.SecretCoreToken, coreToken, "ais_k7v2m4qhx3ab…"},
		{store.SecretCoreToken, " " + coreToken + "\n", "ais_k7v2m4qhx3ab…"},
		{store.SecretCoreToken, "ais_short", "…"},
		{store.SecretCoreToken, "ais_NOTAPREFIX!_secretsecretsecret", "…"},
		{store.SecretCoreToken, "sk-proj-not-a-token-at-all-0123456789", "…"},
		// Twelve characters of the prefix's alphabet are a prefix only
		// where Core's _ follows them: here they are a secret's.
		{store.SecretCoreToken, "ais_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnop", "…"},
		{store.SecretCoreToken, "ais_k7v2m4qhx3ab", "…"},
		// A service credential's hint is its public prefix too.
		{store.SecretCoreToken, "aissvc_ixgrh7nbgpyd_Qm9vYmFyYmF6cXV4cXV1eHF1dXhxdXV4cXV1eHF1dXg", "aissvc_ixgrh7nbgpyd…"},
		{store.SecretCoreToken, "aissvc_ixgrh7nbgpyd", "…"},
		{store.SecretModelKey, "sk-0123456789abcdef0123456789ab3f9a", "sk-…3f9a"},
		{store.SecretModelKey, modelKey, "sk-proj-…3f9a"},
		{store.SecretModelKey, "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx-9z0Q", "sk-ant-…9z0Q"},
		{store.SecretModelKey, "AIzaSyA1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q", "AIza…5p6Q"},
		{store.SecretModelKey, "0123456789abcdef0123456789abcdef", "…cdef"},
		{store.SecretModelKey, "sk-short-key", "sk-…"},
		{store.SecretModelKey, "tiny", "…"},
		{store.SecretModelKey, "sk-proj-0123456789ab", "sk-proj-…"},
	} {
		got := Hint(c.kind, c.secret)
		if got != c.want {
			t.Errorf("Hint(%s, %q) = %q, want %q", c.kind, c.secret, got, c.want)
		}
		// Never more than a prefix and four characters of it.
		if tail := strings.TrimPrefix(got, strings.SplitN(got, Ellipsis, 2)[0]+Ellipsis); len([]rune(tail)) > 4 {
			t.Errorf("Hint(%s, %q) = %q shows more than four characters", c.kind, c.secret, got)
		}
	}
}

func TestKeyringDir(t *testing.T) {
	for id, want := range map[string]string{
		"local:/secrets/kek/v1":    "/secrets/kek",
		"local:/secrets/kek/../v1": "/secrets",
		"local:v1":                 ".",
		"local:":                   "",
		"awskms:arn":               "",
		"":                         "",
	} {
		if got := KeyringDir(id); got != want {
			t.Errorf("KeyringDir(%q) = %q, want %q", id, got, want)
		}
	}
}
