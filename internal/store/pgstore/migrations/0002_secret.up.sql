-- AIShie Agent Runtime, store migration 0002 (up)
-- The secret store (docs/design.md §11): the agents' Core tokens and their
-- owners' model keys, sealed by the vault (internal/vault). Each row is one
-- secret's ciphertext under a data key (DEK) of its own, AES-256-GCM, and
-- that key wrapped by the key-encryption key kek_id names. Both are bound to
-- the row's id, tenant and kind, so that neither can be moved to another
-- row or tenant. No column holds plaintext: hint is all that may be shown.

BEGIN;

SET LOCAL lock_timeout = '10s';

CREATE TABLE secret (
    id          text        PRIMARY KEY,
    tenant_id   text        NOT NULL,
    kind        text        NOT NULL,
    kek_id      text        NOT NULL,
    wrapped_dek bytea       NOT NULL,
    nonce       bytea       NOT NULL,
    ciphertext  bytea       NOT NULL,
    hint        text        NOT NULL,
    created_by  text        NOT NULL,
    created_at  timestamptz NOT NULL,
    CONSTRAINT secret_id_shape CHECK (id ~ '^sec_[A-Za-z0-9_-]{1,60}$'),
    CONSTRAINT secret_kind_known CHECK (kind IN ('core_token', 'model_key'))
);
CREATE INDEX secret_tenant_idx ON secret (tenant_id);
-- keys check and keys rewrap find the secrets each key wraps.
CREATE INDEX secret_kek_idx ON secret (kek_id);

COMMIT;
