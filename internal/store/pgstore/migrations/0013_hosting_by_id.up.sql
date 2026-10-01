-- AIShie Agent Runtime, store migration 0013 (up)
-- Hosting by an agent's id (AIShie-Core #52; docs/design.md §11.2): the
-- runtime is issued each agent's one token by Core's agent_runtime service,
-- and takes no token from anyone.
--   * A hosted agent's row holds a token only once Core has issued it: its
--     owner hosts it by its id before choosing its model, and a token is
--     issued only then; pausing it revokes the token and drops it from the
--     row. token_secret_id is NULL while the row holds none (token_hint '').
--   * token_issued says whether the token was issued to the runtime by
--     Core (true), or pasted by an owner before (false, the default, as
--     every row before this migration's is): the worker that next starts
--     the agent is issued one in its place, which revokes the pasted one.
--     token_credential_id is Core's id of an issued token.
--   * agent_token holds the tokens Core issued the runtime for the agents
--     of the operator's configuration (YAML), sealed (0002), one an agent,
--     by its id in the configuration: shared by every worker, and issued by
--     the one that holds the agent's lease.
-- Additive: the release before reads every row that holds a token as it
-- did (an issued token is the agent's API token, which it runs the agent
-- with). It cannot read a row that holds none, which only this release
-- writes: an agent hosted by its id before its model is chosen, or paused;
-- going back past this release restores the deploy's backup
-- (docs/deploying.md).

BEGIN;

SET LOCAL lock_timeout = '10s';

ALTER TABLE hosted_agent ALTER COLUMN token_secret_id DROP NOT NULL;
-- Constant defaults: the rows there are not written again.
ALTER TABLE hosted_agent ADD COLUMN token_issued boolean NOT NULL DEFAULT false;
ALTER TABLE hosted_agent ADD COLUMN token_credential_id text NOT NULL DEFAULT '';
ALTER TABLE hosted_agent ADD CONSTRAINT hosted_agent_token_issued
    CHECK (token_secret_id IS NOT NULL OR (NOT token_issued AND token_credential_id = '' AND token_hint = ''));
-- Its token and its key are two secrets, when it holds both: a row with
-- neither holds two NULLs, which are not distinct.
ALTER TABLE hosted_agent DROP CONSTRAINT hosted_agent_two_secrets;
ALTER TABLE hosted_agent ADD CONSTRAINT hosted_agent_two_secrets
    CHECK (key_secret_id IS NULL OR key_secret_id IS DISTINCT FROM token_secret_id);

-- The operator's agents' tokens, as Core issued them to the runtime.
CREATE TABLE agent_token (
    agent_id      text        PRIMARY KEY,
    core_actor_id text        NOT NULL,
    secret_id     text        NOT NULL UNIQUE REFERENCES secret (id),
    credential_id text        NOT NULL,
    hint          text        NOT NULL,
    issued_at     timestamptz NOT NULL,
    issued_by     text        NOT NULL
);

COMMIT;
