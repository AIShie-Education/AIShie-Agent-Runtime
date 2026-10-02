-- AIShie Agent Runtime, store migration 0015 (down)
-- Reverts 0015_changes_requested.up.sql: an attempt sent back for changes
-- becomes one rejected, with what was asked as its reason, and so does its
-- note in the conversation's memory. A release before this one reads a
-- rejection's reason only through memory, and reads no note of a kind it
-- does not know: as a rejected note, what was asked still reaches the next
-- attempt's prompt.

BEGIN;

SET LOCAL lock_timeout = '10s';

UPDATE attempt SET state = 'rejected' WHERE state = 'changes_requested';
UPDATE note SET kind = 'rejected' WHERE kind = 'changes_requested';
ALTER TABLE attempt DROP CONSTRAINT IF EXISTS attempt_state_known;
ALTER TABLE attempt ADD CONSTRAINT attempt_state_known CHECK (state IN
    ('sending', 'executed', 'proposed', 'failed', 'denied', 'error', 'rejected', 'cancelled'));

COMMIT;
