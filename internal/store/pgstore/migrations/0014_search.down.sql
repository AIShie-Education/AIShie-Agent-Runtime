-- AIShie Agent Runtime, store migration 0014 (down)
-- Reverts 0014_search.up.sql: the search's passages go, and a release that
-- has the search reads the files again as searches need them.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS search_passage;
DROP TABLE IF EXISTS search_file;

COMMIT;
