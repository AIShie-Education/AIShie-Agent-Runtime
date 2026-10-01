-- AIShie Agent Runtime, store migration 0014 (up)
-- The search of a course's materials (docs/design.md §4, Search): the text
-- a model reading a version's file is given (Core's text version where it
-- is done, the runtime's own reading of the file otherwise, the version's
-- own text), cut into passages, each with the terms the runtime's search
-- finds it by (package search). Kept per course and version, by the file's
-- key in its version (its id, or 'body'), at the revision of the text it
-- was read at; shared by every agent and seat in the course, since which
-- seat may read a version is never kept here: every search asks Core, as
-- the asking seat, which it may read, and searches those alone. A version
-- purged in Core is dropped as the runtime learns of it, and a file no
-- search has needed for 30 days is dropped by housekeeping.
--
-- The terms are the runtime's own, words and, of Chinese, Japanese and
-- Korean, characters and pairs of them, kept as a text[] under a GIN index:
-- built into every PostgreSQL, the same under every locale, where
-- full-text search keeps a run of Chinese as one word and pg_trgm finds no
-- word in it under a C ctype. No extension is needed.
-- Additive: a release before this one never reads it.

BEGIN;

SET LOCAL lock_timeout = '10s';

CREATE TABLE search_file (
    version_id  text        NOT NULL,
    file_key    text        NOT NULL,
    course_id   text        NOT NULL,
    document_id text        NOT NULL,
    revision    text        NOT NULL,
    source      text        NOT NULL CHECK (source IN ('staff', 'ai', 'runtime', 'body')),
    name        text        NOT NULL DEFAULT '',
    position    integer     NOT NULL DEFAULT 0 CHECK (position >= 0),
    -- passages and length are its passages' count and their terms in all,
    -- which a search's scores are reckoned against.
    passages    integer     NOT NULL DEFAULT 0 CHECK (passages >= 0),
    length      bigint      NOT NULL DEFAULT 0 CHECK (length >= 0),
    indexed_at  timestamptz NOT NULL DEFAULT now(),
    used_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (version_id, file_key)
);

CREATE INDEX search_file_by_course ON search_file (course_id);
CREATE INDEX search_file_by_document ON search_file (document_id);
CREATE INDEX search_file_by_use ON search_file (used_at);

CREATE TABLE search_passage (
    version_id   text    NOT NULL,
    file_key     text    NOT NULL,
    seq          integer NOT NULL CHECK (seq >= 0),
    section_kind text    NOT NULL DEFAULT '',
    section_n    integer NOT NULL DEFAULT 0 CHECK (section_n >= 0),
    start_offset integer NOT NULL CHECK (start_offset >= 0),
    part         integer NOT NULL CHECK (part >= 1),
    text         text    NOT NULL,
    terms        text[]  NOT NULL,
    length       integer NOT NULL CHECK (length >= 0),
    PRIMARY KEY (version_id, file_key, seq),
    FOREIGN KEY (version_id, file_key) REFERENCES search_file (version_id, file_key) ON DELETE CASCADE
);

CREATE INDEX search_passage_terms ON search_passage USING gin (terms);

COMMIT;
