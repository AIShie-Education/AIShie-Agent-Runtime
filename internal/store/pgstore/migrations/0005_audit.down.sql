-- AIShie Agent Runtime, store migration 0005 (down)
-- Reverts 0005_audit.up.sql: the audit goes, with every row in it.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS audit;

COMMIT;
