-- AIShie Agent Runtime, store migration 0004 (down)
-- Reverts 0004_seat_snapshot.up.sql: the seats' snapshots go; their ids
-- and times stay.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP INDEX IF EXISTS llm_call_agent_course_idx;
DROP INDEX IF EXISTS answer_agent_course_idx;

ALTER TABLE seat
    DROP COLUMN IF EXISTS perms,
    DROP COLUMN IF EXISTS principal_member_id,
    DROP COLUMN IF EXISTS answers_course,
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS section,
    DROP COLUMN IF EXISTS course_title,
    DROP COLUMN IF EXISTS course_code;

COMMIT;
