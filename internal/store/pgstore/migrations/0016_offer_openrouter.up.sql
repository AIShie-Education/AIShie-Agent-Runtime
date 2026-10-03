-- AIShie Agent Runtime, store migration 0016 (up)
-- A site's offer of OpenRouter's model may hold OpenRouter's upstream routing
-- (its `provider` object, canonical: docs/design.md §3), sent with every call
-- made on it. Null for none, and for every offer of another provider.
-- Additive: a release before this one never reads it, and calls OpenRouter
-- without it (docs/deploying.md says to upgrade every worker first).

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE school_offer ADD COLUMN openrouter jsonb;
ALTER TABLE school_offer ADD CONSTRAINT school_offer_openrouter_object
    CHECK (openrouter IS NULL OR jsonb_typeof(openrouter) = 'object');
ALTER TABLE school_offer ADD CONSTRAINT school_offer_openrouter_provider
    CHECK (openrouter IS NULL OR provider = 'openrouter');

COMMIT;
