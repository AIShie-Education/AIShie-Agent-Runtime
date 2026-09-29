package worker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/llm/scripted"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/secrets"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/memstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// testVault is a vault on a keyring of one key, in a directory of t's own.
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

// sealInto seals plaintext as the secret id of tenant and kind, and keeps
// it in st under tenant as: the tenant it was sealed for, unless a test
// moves it.
func sealInto(t *testing.T, v *vault.Vault, st store.Store, id, tenant, as, kind, plaintext string) {
	t.Helper()
	s, err := v.Seal(context.Background(), store.Secret{ID: id, TenantID: tenant, Kind: kind}, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	s.TenantID = as
	if err := st.PutSecret(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// TestAgentStartsFromSealedSecrets: an agent whose Core token and model key
// are sealed in the store (sealed://) starts from them, answers, and logs
// neither. One whose sealed token was moved to another tenant does not
// open: that agent fails, naming the reference and never what it holds,
// and the other runs on.
func TestAgentStartsFromSealedSecrets(t *testing.T) {
	w := newWorld(t)
	own := w.ownAgent("yuki-helper", 0)
	tu := w.tutor("cs101-tutor")
	v := testVault(t)
	st := memstore.New()
	sealInto(t, v, st, "sec_token", "ten_yuki", "ten_yuki", store.SecretCoreToken, own.actor.Token)
	sealInto(t, v, st, "sec_key", "ten_yuki", "ten_yuki", store.SecretModelKey, modelKey)
	sealInto(t, v, st, "sec_moved", "ten_sato", "ten_other", store.SecretCoreToken, tu.actor.Token)
	w.env.Clear() // nothing but the sealed secrets holds a token or the key

	model := scripted.New(scripted.Reply("Opened from the store."))
	sealed := func(token string) map[string]any {
		return map[string]any{"core": map[string]any{"token_ref": token}, "model": map[string]any{"key_ref": "sealed://sec_key"}}
	}
	cfg := w.config(nil,
		w.agentDoc("yuki-helper", "m1", sealed("sealed://sec_token"), nil),
		w.agentDoc("cs101-tutor", "m1", sealed("sealed://sec_moved"), nil))
	var mu sync.Mutex
	var keys []string
	wk := w.start(cfg, models{"m1": model}, workerOpts{store: st, edit: func(o *Options) {
		o.Secrets = secrets.Resolver{Sealed: vault.Opener{Vault: v, Store: st}}
		next := o.NewAdapter
		o.NewAdapter = func(c llm.Config) (llm.Adapter, error) {
			mu.Lock()
			keys = append(keys, c.APIKey)
			mu.Unlock()
			return next(c)
		}
	}})
	wk.waitState("yuki-helper", store.AgentRunning)
	conv, _ := w.ask(0, own, "Does it open?")
	if got := w.waitAnswers(conv, 1); got[0].Body != "Opened from the store." {
		t.Errorf("the answer: %q", got[0].Body)
	}
	mu.Lock()
	if len(keys) == 0 || keys[0] != modelKey {
		t.Errorf("the adapter was given %d keys, the first not the sealed one", len(keys))
	}
	mu.Unlock()

	failed := wk.waitState("cs101-tutor", store.AgentError)
	if !strings.Contains(failed.Detail, "sealed://sec_moved") || !strings.Contains(failed.Detail, "does not open") {
		t.Errorf("the detail of the agent whose token does not open: %q", failed.Detail)
	}
	if n := len(w.calls(tu.actor.ID, "")); n != 0 {
		t.Errorf("the agent whose token does not open made %d calls to Core", n)
	}
	wk.stop()
	logs := w.logs.String()
	for _, s := range []string{own.actor.Token, tu.actor.Token, modelKey, own.actor.Token[len(own.actor.Token)-16:], "ais_"} {
		if strings.Contains(logs, s) || strings.Contains(failed.Detail, s) {
			t.Errorf("a log line or the detail holds a secret (%.8s…)", s)
		}
	}
}
