-- AIShie Agent Runtime, store migration 0007 (up)
-- The writes a model made through its seat's perms (docs/design.md §4), in
-- the answer's ledger row: how many it sent to Core, and how many of those
-- Core executed, proposed, denied and failed. Counts only, never what a
-- write said.
-- Additive: a release before this one writes answers as it did, and leaves
-- these columns at 0.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE answer
    ADD COLUMN writes          integer NOT NULL DEFAULT 0,
    ADD COLUMN writes_executed integer NOT NULL DEFAULT 0,
    ADD COLUMN writes_proposed integer NOT NULL DEFAULT 0,
    ADD COLUMN writes_denied   integer NOT NULL DEFAULT 0,
    ADD COLUMN writes_failed   integer NOT NULL DEFAULT 0;

COMMIT;
