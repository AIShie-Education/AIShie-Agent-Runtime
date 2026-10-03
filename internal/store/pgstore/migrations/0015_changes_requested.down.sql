-- AIShie Agent Runtime, store migration 0015 (down)
-- Reverts 0015_changes_requested.up.sql: an attempt sent back for changes
-- becomes one rejected, with what was asked as its reason, and so does its
-- note in the conversation's memory, both of kinds a release before this
-- one knows. A note whose request was never read (its text empty, as
-- action_list_mine did not give it) is left as it is, of a kind that
-- release has no sentence for and passes over: made a rejection's, it
-- would be read as one given no reason, and the next attempt told that
-- nothing was asked. A rollback of the runtime never
-- runs this: it leaves the schema as it is (docs/deploying.md), and
-- `aishie-runtime migrate down` takes every migration down, not this one
-- alone.

BEGIN;

SET LOCAL lock_timeout = '10s';

UPDATE attempt SET state = 'rejected' WHERE state = 'changes_requested';
UPDATE note SET kind = 'rejected' WHERE kind = 'changes_requested' AND text <> '';
ALTER TABLE attempt DROP CONSTRAINT IF EXISTS attempt_state_known;
ALTER TABLE attempt ADD CONSTRAINT attempt_state_known CHECK (state IN
    ('sending', 'executed', 'proposed', 'failed', 'denied', 'error', 'rejected', 'cancelled'));

COMMIT;
