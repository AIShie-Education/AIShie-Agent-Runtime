-- AIShie Agent Runtime, store migration 0011 (down)
-- Reverts 0011_transcription.up.sql: the transcriber's credential and its
-- record of jobs go, and the ledger no longer says which calls were the
-- transcriber's (their costs stay, on the school's key). The credential's
-- sealed token stays in the secret table (0002), as the offers' keys do.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS transcription_job;
DROP TABLE IF EXISTS transcription_credential;
ALTER TABLE llm_call DROP COLUMN IF EXISTS kind;

COMMIT;
