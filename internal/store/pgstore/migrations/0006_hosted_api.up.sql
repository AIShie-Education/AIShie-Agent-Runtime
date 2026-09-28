-- AIShie Agent Runtime, store migration 0006 (up)
-- What the API reads of a hosted agent (the M2 API contract, §7.2):
--   * an agent's state says why (reason, one of the contract's closed list)
--     and the version of the agent's row the worker put in force
--     (config_version), so that the API tells a change the worker has not
--     applied yet (starting) from one it has;
--   * a seat keeps its course's status (active, archived, …), so that the
--     API says whether the agent answers in it;
--   * a hosted agent keeps which provider its owner's own key is for
--     (key_provider), so that a stored key is never sent to another
--     provider's host.
-- Additive: a release before this one writes these rows as it did, and
-- leaves these columns at their defaults.

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE agent_state
    ADD COLUMN reason         text    NOT NULL DEFAULT '',
    ADD COLUMN config_version integer NOT NULL DEFAULT 0;

ALTER TABLE seat
    ADD COLUMN course_status text NOT NULL DEFAULT '';

ALTER TABLE hosted_agent
    ADD COLUMN key_provider text NOT NULL DEFAULT '';

COMMIT;
