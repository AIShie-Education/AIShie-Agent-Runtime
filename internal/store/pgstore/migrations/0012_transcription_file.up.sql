-- AIShie Agent Runtime, store migration 0012 (up)
-- A version of a document holds several files since AIShie-Core #49, and
-- the transcriber claims each on its own (docs/design.md §12): its record
-- of a job says which file of the version it was, by Core's id and its
-- place among the version's files. A job of a Core before #49, which
-- claimed versions of one file, names none: '' and 0.
-- Additive: a release before this one never reads them, and the rows it
-- writes take the defaults.

BEGIN;

SET LOCAL lock_timeout = '10s';

-- Constant defaults: the rows there are not written again.
ALTER TABLE transcription_job ADD COLUMN file_id text NOT NULL DEFAULT '';
ALTER TABLE transcription_job ADD COLUMN position integer NOT NULL DEFAULT 0;

COMMIT;
