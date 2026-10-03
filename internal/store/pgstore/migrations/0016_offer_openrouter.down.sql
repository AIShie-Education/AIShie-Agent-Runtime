-- AIShie Agent Runtime, store migration 0016 (down)
-- Reverts 0016_offer_openrouter.up.sql: the site's offers lose their
-- upstream routing, and are called as a release before it calls them,
-- without one. A rollback of the runtime never runs this: it leaves the
-- schema as it is (docs/deploying.md), and `aishie-runtime migrate down`
-- takes every migration down, not this one alone.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TRIGGER IF EXISTS school_offer_left_openrouter ON school_offer;
DROP FUNCTION IF EXISTS school_offer_left_openrouter();
ALTER TABLE school_offer DROP COLUMN openrouter;

COMMIT;
