-- AIShie Agent Runtime, store migration 0012 (down)
-- Reverts 0012_transcription_file.up.sql: the jobs no longer say which file
-- of a version each was.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE transcription_job DROP COLUMN IF EXISTS position;
ALTER TABLE transcription_job DROP COLUMN IF EXISTS file_id;

COMMIT;
