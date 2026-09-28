-- AIShie Agent Runtime, store migration 0008 (up)
-- What the runtime's OCR recognized of a course document's file that has no
-- text of its own to read (docs/design.md §4, Files): a scanned PDF, one
-- whose fonts map to nothing, an image. Kept by the file's sha256, which the
-- runtime computes from the bytes it fetched, so that a file is recognized
-- once for every worker and every copy of it; a failure is kept too, for a
-- while, so that a file OCR cannot read is not tried at every question.
-- Additive: a release before this one never reads it.

BEGIN;

SET LOCAL lock_timeout = '10s';

CREATE TABLE ocr_text (
    sum         text PRIMARY KEY CHECK (sum ~ '^sha256:[0-9a-f]{64}$'),
    status      text NOT NULL CHECK (status IN ('done', 'failed')),
    kind        text NOT NULL CHECK (kind IN ('pdf', 'image')),
    text        text NOT NULL DEFAULT '',
    pages       integer NOT NULL DEFAULT 0 CHECK (pages >= 0),
    pages_of    integer NOT NULL DEFAULT 0 CHECK (pages_of >= 0),
    sections    jsonb NOT NULL DEFAULT '[]',
    notes       jsonb NOT NULL DEFAULT '[]',
    reason      text NOT NULL DEFAULT '',
    engine      text NOT NULL DEFAULT '',
    duration_ms bigint NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ocr_text_by_age ON ocr_text (status, created_at);

COMMIT;
