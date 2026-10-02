-- AIShie Agent Runtime, store migration 0015 (down)
-- Reverts 0015_changes_requested.up.sql: an attempt sent back for changes
-- becomes one rejected, with what was asked as its reason, which is how a
-- release before this one reads it.

BEGIN;

SET LOCAL lock_timeout = '10s';

UPDATE attempt SET state = 'rejected' WHERE state = 'changes_requested';
ALTER TABLE attempt DROP CONSTRAINT IF EXISTS attempt_state_known;
ALTER TABLE attempt ADD CONSTRAINT attempt_state_known CHECK (state IN
    ('sending', 'executed', 'proposed', 'failed', 'denied', 'error', 'rejected', 'cancelled'));

COMMIT;
