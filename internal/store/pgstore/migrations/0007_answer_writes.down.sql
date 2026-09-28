-- AIShie Agent Runtime, store migration 0007 (down)
-- Reverts 0007_answer_writes.up.sql: the answers' counts of writes go.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE answer
    DROP COLUMN IF EXISTS writes_failed,
    DROP COLUMN IF EXISTS writes_denied,
    DROP COLUMN IF EXISTS writes_proposed,
    DROP COLUMN IF EXISTS writes_executed,
    DROP COLUMN IF EXISTS writes;

COMMIT;
