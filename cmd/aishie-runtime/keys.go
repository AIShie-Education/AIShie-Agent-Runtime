package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/config"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/redact"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store/pgstore"
	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/vault"
)

// keysPage is how many secrets keys reads at once.
const keysPage = 500

// cmdKeys is `aishie-runtime keys check|rewrap`: the secrets sealed in the
// store at DATABASE_URL, against the keyring KMS_KEY_ID names. check opens
// every secret and forgets what it holds; rewrap wraps every data key an
// older key wraps under KMS_KEY_ID's own, which is how a key is retired
// (docs/deploying.md, Rotating the key). Neither prints a secret: only ids,
// kinds, tenants and the keys' ids.
func cmdKeys(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 || (args[0] != "check" && args[0] != "rewrap") {
		return usageError(stderr, "keys takes check or rewrap")
	}
	env, err := config.FromEnv(getenv)
	if err != nil {
		return failure(stderr, "the environment:\n%s", problemsText(err))
	}
	switch {
	case env.DatabaseURL == "":
		return failure(stderr, "DATABASE_URL is not set: the sealed secrets are kept in the runtime's database")
	case env.KMSKeyID == "":
		return failure(stderr, "KMS_KEY_ID is not set: it names the keyring the secrets are sealed with")
	}
	v, err := vault.Open(env.KMSKeyID)
	if err != nil {
		return failure(stderr, "the keyring: %v", err)
	}
	st, err := pgstore.Open(ctx, env.DatabaseURL)
	if err != nil {
		return failure(stderr, "the store: %v", err)
	}
	defer func() { _ = st.Close() }()
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(stdout, format+"\n", a...) }
	if args[0] == "check" {
		return keysCheck(ctx, p, st, v, stderr)
	}
	return keysRewrap(ctx, p, st, v, stderr)
}

// eachSecret calls fn with every secret in the store, a page at a time, in
// the order of their ids.
func eachSecret(ctx context.Context, st store.Secrets, fn func(*store.Secret)) error {
	after := ""
	for {
		page, err := st.ListSecrets(ctx, after, keysPage)
		if err != nil {
			return err
		}
		for i := range page {
			fn(&page[i])
		}
		if len(page) < keysPage {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

// describeSecret names a secret by what may be shown of it.
func describeSecret(s *store.Secret) string {
	return fmt.Sprintf("secret %s (%s of tenant %s, wrapped by %s)", s.ID, s.Kind, s.TenantID, s.KEKID)
}

// keysCheck opens every secret, and says how many each key wraps and which
// do not open.
func keysCheck(ctx context.Context, p func(string, ...any), st store.Secrets, v *vault.Vault, stderr io.Writer) int {
	byKey := map[string]int{}
	total, failed := 0, 0
	err := eachSecret(ctx, st, func(s *store.Secret) {
		total++
		if err := v.Check(ctx, s); err != nil {
			failed++
			p("%s: FAILED: %s", describeSecret(s), redact.String(err.Error()))
			return
		}
		byKey[s.KEKID]++
	})
	if err != nil {
		return failure(stderr, "reading the secrets: %v", err)
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	older := 0
	for _, k := range keys {
		mark := ""
		if k == v.KEKID() {
			mark = " (the current key)"
		} else {
			older += byKey[k]
		}
		p("%s wraps %d secrets%s", k, byKey[k], mark)
	}
	if older > 0 {
		p("%d secrets are wrapped by an older key: run aishie-runtime keys rewrap before that key is removed", older)
	}
	if failed > 0 {
		return failure(stderr, "%d of %d secrets do not open with the keyring", failed, total)
	}
	p("every secret opens: %d", total)
	return exitOK
}

// keysRewrap wraps every data key an older key wraps under the current key.
// A secret another process rewrapped or deleted meanwhile is left to it.
func keysRewrap(ctx context.Context, p func(string, ...any), st store.Secrets, v *vault.Vault, stderr io.Writer) int {
	var rewrapped, current, gone, failed int
	err := eachSecret(ctx, st, func(s *store.Secret) {
		if s.KEKID == v.KEKID() {
			current++
			return
		}
		kekID, wrapped, err := v.Rewrap(ctx, s)
		if err == nil {
			err = st.RewrapSecret(ctx, s.ID, s.KEKID, kekID, wrapped)
		}
		switch {
		case err == nil:
			rewrapped++
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrConflict):
			gone++
		default:
			failed++
			p("%s: FAILED: %s", describeSecret(s), redact.String(err.Error()))
		}
	})
	if err != nil {
		return failure(stderr, "reading the secrets: %v", err)
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("rewrapped %d secrets under %s", rewrapped, v.KEKID()))
	parts = append(parts, fmt.Sprintf("%d were already", current))
	if gone > 0 {
		parts = append(parts, fmt.Sprintf("%d were deleted or rewrapped by another meanwhile", gone))
	}
	p("%s", strings.Join(parts, "; "))
	if failed > 0 {
		return failure(stderr, "%d secrets could not be rewrapped; they are as they were", failed)
	}
	return exitOK
}
