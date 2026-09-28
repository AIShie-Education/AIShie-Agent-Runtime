-- AIShie Agent Runtime, store migration 0004 (up)
-- The seat as me_memberships last showed it (docs/design.md §8): the
-- worker writes these at each read, so that the API can show an agent's
-- seats without its token. Additive: a release before this one writes
-- seats as it did, and leaves these at their defaults.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE seat
    ADD COLUMN course_code         text    NOT NULL DEFAULT '',
    ADD COLUMN course_title        text    NOT NULL DEFAULT '',
    ADD COLUMN section             text    NOT NULL DEFAULT '',
    ADD COLUMN status              text    NOT NULL DEFAULT '',
    ADD COLUMN answers_course      boolean NOT NULL DEFAULT false,
    ADD COLUMN principal_member_id text,
    ADD COLUMN perms               jsonb   NOT NULL DEFAULT '{}';

-- Reports read one course's answers and calls of an agent over a span.
CREATE INDEX answer_agent_course_idx ON answer (agent_id, course_id, at);
CREATE INDEX llm_call_agent_course_idx ON llm_call (agent_id, course_id, at);

COMMIT;
