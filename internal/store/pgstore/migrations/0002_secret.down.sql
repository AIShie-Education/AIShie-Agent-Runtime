-- AIShie Agent Runtime, store migration 0002 (down)
-- Reverts 0002_secret.up.sql: every sealed secret goes with it.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS secret;

COMMIT;
