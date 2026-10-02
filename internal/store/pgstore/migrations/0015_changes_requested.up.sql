-- AIShie Agent Runtime, store migration 0015 (up)
-- An attempt at an answer whose proposal a person sent back for changes
-- (AIShie-Core #68) is settled as changes_requested, with what they asked
-- to change as its reason: the next attempt at the message names its
-- action as the proposal it revises.
-- Additive: a release before this one writes no such attempt, and reads
-- one as an attempt that posted nothing, answering the message again
-- under the next number, without naming what it revises.

BEGIN;

SET LOCAL lock_timeout = '10s';

-- NOT VALID: every row already passes it, as the states it takes are more
-- than before, and validating would read the whole table under a lock
-- that holds every answer up. New rows are held to it all the same.
ALTER TABLE attempt DROP CONSTRAINT attempt_state_known;
ALTER TABLE attempt ADD CONSTRAINT attempt_state_known CHECK (state IN
    ('sending', 'executed', 'proposed', 'failed', 'denied', 'error', 'rejected', 'cancelled', 'changes_requested')) NOT VALID;

COMMIT;
