-- AIShie Agent Runtime, store migration 0009 (down)
-- Reverts 0009_site.up.sql: the site's settings go, and the environment and
-- runtime.yaml decide alone again; the offers of the school's plan the site
-- made go, and a hosted agent on one is then on an offer the school no
-- longer has. Their sealed keys stay in the secret table (0002), as 0003's
-- hosted agents' secrets do.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS school_offer;
DROP TABLE IF EXISTS site_setting;

COMMIT;
