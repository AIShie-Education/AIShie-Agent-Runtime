-- AIShie Agent Runtime, store migration 0010 (up)
-- The money the site's administrators manage through the API
-- (docs/design.md §11.5), beside what runtime.yaml and the price file set:
-- the site's price table, rows priced in pUSD a token (exact), whose version
-- is when they last changed (site_price_rev, moved on by every statement
-- that writes them, a second past the time before at least, so that a cost
-- the ledger records names the rows it was priced by, as a new file's
-- version does); and the tenants' daily quotas on the school's key, each
-- in place of runtime.yaml's runtime.tenants. Every statement that writes
-- them moves the registry's revision on and tells every listener, by 0003's
-- trigger function, so that every worker puts the change in force.
-- Additive: a release before this one never reads them.

BEGIN;

SET LOCAL lock_timeout = '10s';

CREATE TABLE site_price (
    id               text        PRIMARY KEY,
    provider         text        NOT NULL,
    model            text        NOT NULL,
    from_day         date        NOT NULL,
    input_pusd       bigint      NOT NULL,
    cache_read_pusd  bigint      NOT NULL,
    cache_write_pusd bigint      NOT NULL,
    output_pusd      bigint      NOT NULL,
    version          integer     NOT NULL,
    created_by       text        NOT NULL,
    created_at       timestamptz NOT NULL,
    updated_by       text        NOT NULL,
    updated_at       timestamptz NOT NULL,
    CONSTRAINT site_price_id_shape CHECK (id ~ '^[A-Za-z0-9._-]{1,64}$'),
    CONSTRAINT site_price_provider_shape CHECK (provider ~ '^[a-z0-9_]+$'),
    CONSTRAINT site_price_model_given CHECK (btrim(model) <> ''),
    CONSTRAINT site_price_not_negative CHECK (input_pusd >= 0 AND cache_read_pusd >= 0 AND cache_write_pusd >= 0 AND output_pusd >= 0),
    CONSTRAINT site_price_one_per_day UNIQUE (provider, model, from_day)
);

-- When the site's prices last changed: one row, 'epoch' before they ever
-- have.
CREATE TABLE site_price_rev (
    one        boolean     PRIMARY KEY DEFAULT true,
    changed_at timestamptz NOT NULL,
    CONSTRAINT site_price_rev_one_row CHECK (one)
);
INSERT INTO site_price_rev (changed_at) VALUES ('epoch');

CREATE FUNCTION site_price_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE site_price_rev
       SET changed_at = GREATEST(date_trunc('second', clock_timestamp()), date_trunc('second', changed_at) + interval '1 second');
    RETURN NULL;
END
$$;

CREATE TRIGGER site_price_versioned AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON site_price
    FOR EACH STATEMENT EXECUTE FUNCTION site_price_changed();
CREATE TRIGGER site_price_changed AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON site_price
    FOR EACH STATEMENT EXECUTE FUNCTION registry_changed();

CREATE TABLE site_tenant_quota (
    tenant_id  text        PRIMARY KEY,
    answers    integer,
    usd_pusd   bigint,
    updated_by text        NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT site_tenant_quota_tenant_shape CHECK (tenant_id ~ '^[A-Za-z0-9_-]{1,64}$'),
    CONSTRAINT site_tenant_quota_positive CHECK ((answers IS NULL OR answers >= 1) AND (usd_pusd IS NULL OR usd_pusd >= 1))
);

CREATE TRIGGER site_tenant_quota_changed AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON site_tenant_quota
    FOR EACH STATEMENT EXECUTE FUNCTION registry_changed();

COMMIT;
