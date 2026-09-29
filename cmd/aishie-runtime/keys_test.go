package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// writeKey writes a 32-byte key, base64, as the file name in dir.
func writeKey(t *testing.T, dir, name string) {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestKeys: keys check opens every secret and says which key wraps it;
// keys rewrap moves them to the current key, after which the old one can
// go; a secret that does not open fails the check, by its id. Nothing of a
// secret is printed.
func TestKeys(t *testing.T) {
	dbURL := scratchDatabase(t)
	if err := pgstore.Migrate(dbURL, pgstore.Up); err != nil {
		t.Fatal(err)
	}
	st, err := pgstore.Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dir := t.TempDir()
	writeKey(t, dir, "v1")
	v1, err := vault.Open("local:" + filepath.Join(dir, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	plaintexts := map[string]string{}
	for i := range 3 {
		id := fmt.Sprintf("sec_%d", i)
		plaintexts[id] = fmt.Sprintf("sk-proj-secret-%d-0123456789abcdefghij", i)
		s, err := v1.Seal(context.Background(), store.Secret{ID: id, TenantID: "ten_owner", Kind: store.SecretModelKey}, plaintexts[id])
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutSecret(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	keys := func(key string, args ...string) (int, string) {
		t.Helper()
		code, out, errs := runCmd(t, env("DATABASE_URL", dbURL, "KMS_KEY_ID", "local:"+filepath.Join(dir, key)), append([]string{"keys"}, args...)...)
		for _, p := range plaintexts {
			if strings.Contains(out+errs, p) || strings.Contains(out+errs, p[len(p)-12:]) {
				t.Fatalf("keys %v printed a secret:\n%s%s", args, out, errs)
			}
		}
		return code, out + errs
	}

	if code, out := keys("v1", "check"); code != exitOK || !strings.Contains(out, "local:v1 wraps 3 secrets (the current key)") ||
		!strings.Contains(out, "every secret opens: 3") {
		t.Fatalf("keys check: %d\n%s", code, out)
	}

	writeKey(t, dir, "v2")
	if code, out := keys("v2", "check"); code != exitOK || !strings.Contains(out, "local:v1 wraps 3 secrets\n") ||
		!strings.Contains(out, "3 secrets are wrapped by an older key: run aishie-runtime keys rewrap") {
		t.Fatalf("keys check with v2 current: %d\n%s", code, out)
	}
	if code, out := keys("v2", "rewrap"); code != exitOK || !strings.Contains(out, "rewrapped 3 secrets under local:v2; 0 were already") {
		t.Fatalf("keys rewrap: %d\n%s", code, out)
	}
	if code, out := keys("v2", "rewrap"); code != exitOK || !strings.Contains(out, "rewrapped 0 secrets under local:v2; 3 were already") {
		t.Fatalf("keys rewrap again: %d\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(dir, "v1")); err != nil {
		t.Fatal(err)
	}
	if code, out := keys("v2", "check"); code != exitOK || !strings.Contains(out, "local:v2 wraps 3 secrets (the current key)") {
		t.Fatalf("keys check after v1 went: %d\n%s", code, out)
	}
	v2, err := vault.Open("local:" + filepath.Join(dir, "v2"))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range plaintexts {
		s, err := st.Secret(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := v2.Open(context.Background(), s); err != nil || got != want {
			t.Fatalf("%s after the rewrap: %v", id, err)
		}
	}

	// A secret moved to another tenant does not open, and fails the check
	// by its id; a rewrap leaves it as it is.
	execSQL(t, dbURL, `UPDATE secret SET tenant_id = 'ten_other' WHERE id = 'sec_1'`)
	writeKey(t, dir, "v3")
	if code, out := keys("v3", "rewrap"); code != exitFailure || !strings.Contains(out, "secret sec_1 (model_key of tenant ten_other, wrapped by local:v2): FAILED") ||
		!strings.Contains(out, "rewrapped 2 secrets under local:v3") {
		t.Fatalf("keys rewrap with a secret that does not open: %d\n%s", code, out)
	}
	if code, out := keys("v3", "check"); code != exitFailure || !strings.Contains(out, "sec_1") || !strings.Contains(out, "1 of 3 secrets do not open") {
		t.Fatalf("keys check with a secret that does not open: %d\n%s", code, out)
	}
	if code, out := keys("nope", "check"); code != exitFailure || !strings.Contains(out, "no key file") {
		t.Fatalf("keys check with a key that is not there: %d\n%s", code, out)
	}
	if code, _, errs := runCmd(t, env("DATABASE_URL", dbURL), "keys", "check"); code != exitFailure || !strings.Contains(errs, "KMS_KEY_ID is not set") {
		t.Fatalf("keys check without KMS_KEY_ID: %d %s", code, errs)
	}
}

// execSQL runs sql on the database at u.
func execSQL(t *testing.T, u, sql string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(t.Context(), sql); err != nil {
		t.Fatal(err)
	}
}

// TestCheckLiveOpensSealedSecrets: check --live resolves a sealed://
// reference as run does, from the store at DATABASE_URL with the keyring
// KMS_KEY_ID names; without them it says what it lacks. check without
// --live reads the keyring too, so that a deploy stops before a runtime
// that could not open its secrets starts.
func TestCheckLiveOpensSealedSecrets(t *testing.T) {
	w := newLiveWorld(t)
	dbURL := scratchDatabase(t)
	if err := pgstore.Migrate(dbURL, pgstore.Up); err != nil {
		t.Fatal(err)
	}
	st, err := pgstore.Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dir := t.TempDir()
	writeKey(t, dir, "v1")
	v, err := vault.Open("local:" + filepath.Join(dir, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(w.secrets, "agents", "own", "token"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := v.Seal(context.Background(), store.Secret{ID: "sec_own", TenantID: "ten_yuki", Kind: store.SecretCoreToken}, strings.TrimSpace(string(token)))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSecret(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	yaml, err := os.ReadFile(filepath.Join(w.config, "own.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sealed := strings.Replace(string(yaml), `"secret://agents/own/token"`, `"sealed://sec_own"`, 1)
	if err := os.WriteFile(filepath.Join(w.config, "own.yaml"), []byte(sealed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(w.config, "tutor.yaml")); err != nil {
		t.Fatal(err)
	}
	kms := "local:" + filepath.Join(dir, "v1")

	code, out, errs := runCmd(t, env("CONFIG", w.config, "DATABASE_URL", dbURL, "KMS_KEY_ID", kms), "check", "--live")
	if code != exitOK || !strings.Contains(out, "sealed secrets: new ones are sealed by local:v1") ||
		!strings.Contains(out, `agent own: connected as "Yuki's helper"`) || strings.Contains(out+errs, "ais_") {
		t.Fatalf("check --live with a sealed token: %d\n%s%s", code, out, errs)
	}
	code, out, errs = runCmd(t, env("CONFIG", w.config), "check", "--live")
	if code != exitFailure || !strings.Contains(out, "sealed://sec_own: this runtime opens no sealed secret: set KMS_KEY_ID") {
		t.Fatalf("check --live without the vault: %d\n%s%s", code, out, errs)
	}
	code, out, errs = runCmd(t, env("CONFIG", w.config, "KMS_KEY_ID", "local:"+filepath.Join(dir, "v9")), "check")
	if code != exitFailure || !strings.Contains(errs, "the sealed secrets' keyring") {
		t.Fatalf("check with a keyring that cannot be read: %d\n%s%s", code, out, errs)
	}
}
