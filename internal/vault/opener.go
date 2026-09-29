package vault

import (
	"context"
	"errors"
	"fmt"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// SecretReader reads sealed secrets: store.Secrets.
type SecretReader interface {
	Secret(ctx context.Context, id string) (*store.Secret, error)
}

// Opener opens the secrets a store keeps, by id, for sealed:// references
// (secrets.Opener). It is the only way from a stored secret to its
// plaintext, and the worker calls it only as an agent starts.
type Opener struct {
	Vault *Vault
	Store SecretReader
}

// OpenSecret reads the secret id from the store and opens it.
func (o Opener) OpenSecret(ctx context.Context, id string) (string, error) {
	s, err := o.Store.Secret(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", fmt.Errorf("the secret %s is not in the store: it was deleted, or never stored", id)
	case err != nil:
		return "", err
	}
	return o.Vault.Open(ctx, s)
}
