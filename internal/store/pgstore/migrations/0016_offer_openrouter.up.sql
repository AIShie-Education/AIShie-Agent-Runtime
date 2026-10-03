-- AIShie Agent Runtime, store migration 0016 (up)
-- A site's offer of OpenRouter's model may hold OpenRouter's upstream routing
-- (its `provider` object, canonical: docs/design.md §3), sent with every call
-- made on it. Null for none, and for every offer of another provider.
-- Additive: a release before this one never reads it, and calls OpenRouter
-- without it (docs/deploying.md says to upgrade every worker first).
--
-- A release before this one also writes an offer without it: its update
-- sets the provider and leaves the routing as it is. An offer it moves off
-- OpenRouter loses its routing, as this release's own move does, rather
-- than break school_offer_openrouter_provider, so that it runs beside this
-- schema, and after a rollback, as it did before.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE school_offer ADD COLUMN openrouter jsonb;
ALTER TABLE school_offer ADD CONSTRAINT school_offer_openrouter_object
    CHECK (openrouter IS NULL OR jsonb_typeof(openrouter) = 'object');
ALTER TABLE school_offer ADD CONSTRAINT school_offer_openrouter_provider
    CHECK (openrouter IS NULL OR provider = 'openrouter');

-- Fired only by an update that moves an offer off OpenRouter and leaves its
-- routing as it was: one that sets a routing anew is still refused.
CREATE FUNCTION school_offer_left_openrouter() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.openrouter := NULL;
    RETURN NEW;
END
$$;

CREATE TRIGGER school_offer_left_openrouter BEFORE UPDATE OF provider ON school_offer
    FOR EACH ROW
    WHEN (OLD.provider = 'openrouter' AND NEW.provider <> 'openrouter' AND NEW.openrouter IS NOT DISTINCT FROM OLD.openrouter)
    EXECUTE FUNCTION school_offer_left_openrouter();

COMMIT;
