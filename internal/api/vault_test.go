package api

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

func testVault(t *testing.T) *vault.Vault {
	t.Helper()
	dir := t.TempDir()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "v1"), []byte(base64.StdEncoding.EncodeToString(k)), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open("local:" + filepath.Join(dir, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
