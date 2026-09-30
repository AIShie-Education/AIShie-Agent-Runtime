-- AIShie Agent Runtime, store migration 0011 (up)
-- The transcriber (docs/design.md §12), which the site's administrators
-- turn on: the service credential Core issued for it, sealed as a secret
-- (0002) of the tenant 'site', which goes with it, with what the
-- transcriber last found of it; its record of each version of a document
-- it claimed, kept 90 days; and the kind of each model call the ledger
-- holds, an answer's or the transcriber's, whose cost counts against the
-- school's key alone. A statement that gives or forgets the credential
-- moves the registry's revision on and tells every listener, by 0003's
-- trigger function, so that every worker's transcriber takes it; a note of
-- what Core made of it does not.
-- Additive: a release before this one never reads them, and its ledger
-- rows are answers' (kind's default).

BEGIN;

SET LOCAL lock_timeout = '10s';

-- A constant default: the rows there are not written again.
ALTER TABLE llm_call ADD COLUMN kind text NOT NULL DEFAULT 'model_calls';

CREATE TABLE transcription_credential (
    one           boolean     PRIMARY KEY DEFAULT true,
    secret_id     text        NOT NULL UNIQUE REFERENCES secret (id),
    hint          text        NOT NULL,
    credential_id text        NOT NULL,
    tested        boolean     NOT NULL,
    set_by        text        NOT NULL,
    set_at        timestamptz NOT NULL,
    last_ok_at    timestamptz,
    rejected_at   timestamptz,
    last_error    text        NOT NULL,
    CONSTRAINT transcription_credential_one_row CHECK (one)
);

CREATE TRIGGER transcription_credential_changed AFTER INSERT OR DELETE OR TRUNCATE ON transcription_credential
    FOR EACH STATEMENT EXECUTE FUNCTION registry_changed();
CREATE TRIGGER transcription_credential_replaced AFTER UPDATE OF secret_id ON transcription_credential
    FOR EACH STATEMENT EXECUTE FUNCTION registry_changed();

-- pages is null until the file's pages are counted, cost_pusd while a call
-- had no price, finished_at while the job works.
CREATE TABLE transcription_job (
    id            text        PRIMARY KEY,
    seq           bigint      GENERATED ALWAYS AS IDENTITY UNIQUE,
    version_id    text        NOT NULL,
    document_id   text        NOT NULL,
    course_id     text        NOT NULL,
    lease_id      text        NOT NULL,
    status        text        NOT NULL,
    reason        text        NOT NULL,
    backfill      boolean     NOT NULL,
    attempt       integer     NOT NULL,
    content_type  text        NOT NULL,
    byte_size     bigint      NOT NULL,
    pages         integer,
    pages_sent    integer     NOT NULL,
    offer         text        NOT NULL,
    model         text        NOT NULL,
    model_calls   integer     NOT NULL,
    cost_pusd     bigint,
    input_tokens  bigint      NOT NULL,
    output_tokens bigint      NOT NULL,
    worker        text        NOT NULL,
    started_at    timestamptz NOT NULL,
    heartbeat_at  timestamptz NOT NULL,
    finished_at   timestamptz,
    CONSTRAINT transcription_job_id_shape CHECK (id ~ '^trj_'),
    CONSTRAINT transcription_job_status_known CHECK (status IN ('working', 'done', 'failed', 'skipped', 'dropped'))
);
CREATE INDEX transcription_job_started_idx ON transcription_job (started_at);
CREATE INDEX transcription_job_status_idx ON transcription_job (status, seq);

COMMIT;
