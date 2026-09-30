-- AIShie Agent Runtime, store migration 0009 (up)
-- What the site's administrators change through the API (docs/design.md
-- §11.5), within the ceiling the operator's environment and runtime.yaml
-- set: the site's settings, by name (OCR on or off and its languages, the
-- school plan's quotas), each a JSON object; and the offers of the school's
-- plan they make, each with the school's key sealed as a secret (0002) of
-- the tenant 'school', which goes with it. Every statement that writes
-- either moves the registry's revision on and tells every listener, by
-- 0003's trigger function, so that every worker puts the change in force
-- as it does a hosted agent's, a write made by hand included.
-- Additive: a release before this one never reads them.

BEGIN;

SET LOCAL lock_timeout = '10s';

CREATE TABLE site_setting (
    name       text        PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_by text        NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT site_setting_name_shape CHECK (name ~ '^[a-z][a-z0-9_]{0,63}$'),
    CONSTRAINT site_setting_value_object CHECK (jsonb_typeof(value) = 'object')
);

-- base_url is the endpoint the API made from a provider's offer, '' for
-- the adapter's own; max_output_tokens 0 is the runtime's default, and
-- reasoning_effort '' none.
CREATE TABLE school_offer (
    id                text        PRIMARY KEY,
    label             text        NOT NULL,
    adapter           text        NOT NULL,
    provider          text        NOT NULL,
    model             text        NOT NULL,
    base_url          text        NOT NULL,
    region            text        NOT NULL,
    max_output_tokens integer     NOT NULL,
    reasoning_effort  text        NOT NULL,
    enabled           boolean     NOT NULL,
    key_secret_id     text        NOT NULL UNIQUE REFERENCES secret (id),
    key_hint          text        NOT NULL,
    key_tested        boolean     NOT NULL,
    version           integer     NOT NULL,
    created_by        text        NOT NULL,
    created_at        timestamptz NOT NULL,
    updated_by        text        NOT NULL,
    updated_at        timestamptz NOT NULL,
    CONSTRAINT school_offer_id_shape CHECK (id ~ '^[A-Za-z0-9_-]{1,64}$'),
    CONSTRAINT school_offer_output_bound CHECK (max_output_tokens >= 0)
);

CREATE TRIGGER site_setting_changed AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON site_setting
    FOR EACH STATEMENT EXECUTE FUNCTION registry_changed();
CREATE TRIGGER school_offer_changed AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON school_offer
    FOR EACH STATEMENT EXECUTE FUNCTION registry_changed();

COMMIT;
