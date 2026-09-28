-- AIShie Agent Runtime, store migration 0003 (down)
-- Reverts 0003_registry.up.sql: every hosted agent and its settings go with
-- it. Their sealed secrets stay in the secret table (0002).

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS hosted_course;
DROP TABLE IF EXISTS hosted_agent;
DROP FUNCTION IF EXISTS registry_changed();
DROP TABLE IF EXISTS registry_rev;
DROP TABLE IF EXISTS person;

COMMIT;
