-- AIShie Agent Runtime, store migration 0001 (down)
-- Reverts 0001_init.up.sql: every table, and all the runtime's state with
-- it, the ledger included.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS agent_state;
DROP TABLE IF EXISTS answer;
DROP TABLE IF EXISTS llm_call;
DROP TABLE IF EXISTS seat;
DROP TABLE IF EXISTS note;
DROP TABLE IF EXISTS cursor;
DROP TABLE IF EXISTS attempt;
DROP TABLE IF EXISTS lease;

COMMIT;
