-- AIShie Agent Runtime, store migration 0013 (down)
-- Reverts 0013_hosting_by_id.up.sql: a hosted agent's row holds a token
-- again, always. Rows that hold none (hosted by their id before a model
-- was chosen, or paused) are deleted, with their courses (by cascade):
-- their owners host them again. The operator's agents' tokens are
-- forgotten, their sealed secrets destroyed with them; Core still holds
-- them live until the runtime is issued others, or they are revoked there.

BEGIN;

SET LOCAL lock_timeout = '10s';

WITH gone AS (DELETE FROM agent_token RETURNING secret_id)
DELETE FROM secret WHERE id IN (SELECT secret_id FROM gone);
DROP TABLE IF EXISTS agent_token;
-- Its key's secret goes with the row, as DeleteHostedAgent destroys it
-- (a key is one row's alone: key_secret_id is unique).
WITH gone AS (DELETE FROM hosted_agent WHERE token_secret_id IS NULL RETURNING key_secret_id)
DELETE FROM secret WHERE id IN (SELECT key_secret_id FROM gone WHERE key_secret_id IS NOT NULL);
ALTER TABLE hosted_agent DROP CONSTRAINT IF EXISTS hosted_agent_token_issued;
ALTER TABLE hosted_agent DROP CONSTRAINT IF EXISTS hosted_agent_two_secrets;
ALTER TABLE hosted_agent ADD CONSTRAINT hosted_agent_two_secrets CHECK (key_secret_id IS DISTINCT FROM token_secret_id);
ALTER TABLE hosted_agent DROP COLUMN IF EXISTS token_credential_id;
ALTER TABLE hosted_agent DROP COLUMN IF EXISTS token_issued;
ALTER TABLE hosted_agent ALTER COLUMN token_secret_id SET NOT NULL;

COMMIT;
