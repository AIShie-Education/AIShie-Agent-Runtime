-- AIShie Agent Runtime, store migration 0006 (down)
-- Reverts 0006_hosted_api.up.sql: the states' reasons and versions, the
-- seats' course status, and the provider of an owner's key go.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE hosted_agent
    DROP COLUMN IF EXISTS key_provider;

ALTER TABLE seat
    DROP COLUMN IF EXISTS course_status;

ALTER TABLE agent_state
    DROP COLUMN IF EXISTS config_version,
    DROP COLUMN IF EXISTS reason;

COMMIT;
