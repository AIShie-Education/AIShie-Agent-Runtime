-- AIShie Agent Runtime, store migration 0005 (up)
-- The audit of the runtime's API (docs/design.md §11.4): one row for each
-- thing a person did through it, or was refused, kept about 13 months
-- (the product owner's D11). Ids, hints, providers, models and results:
-- never a secret, a name, or text anyone wrote.

BEGIN;

SET LOCAL lock_timeout = '10s';

CREATE TABLE audit (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at          timestamptz NOT NULL,
    actor_id    text        NOT NULL,
    session_id  text        NOT NULL,
    ip          text        NOT NULL,
    action      text        NOT NULL,
    target_type text        NOT NULL,
    target_id   text        NOT NULL,
    outcome     text        NOT NULL, -- ok, or a refusal's reason
    detail      jsonb       NOT NULL,
    CONSTRAINT audit_detail_object CHECK (jsonb_typeof(detail) = 'object')
);
CREATE INDEX audit_at_idx ON audit (at);
CREATE INDEX audit_actor_idx ON audit (actor_id, at);
CREATE INDEX audit_target_idx ON audit (target_type, target_id, at);

COMMIT;
