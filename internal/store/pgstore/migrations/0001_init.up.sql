-- AIShie Agent Runtime, store migration 0001 (up)
-- The runtime's own state (docs/design.md §8): leases, answers written
-- ahead, cursors, memory, the seats it has seen, the ledger, and each
-- agent's state. It is never Core's database (Core's docs/agent-runtime.md
-- §8.3).
--
-- Ids are text: Core's are UUIDs, the runtime's own (agt_…, ten_…) are not,
-- and nothing here joins on them. Every table but lease is keyed on
-- agent_id, and every query filters on it (§5.4). Money is bigint
-- pico-dollars (1e-12 USD). No column holds the text of what people wrote:
-- note.text is written by the runtime, and attempt.args are the exact bytes
-- of the runtime's own write, kept so that a resend under the same key sends
-- the same bytes (§2.2); both go when a seat's memory is purged.

BEGIN;

SET LOCAL lock_timeout = '10s';

-- A lease is held by one holder until it expires, on the database's clock,
-- so that workers on several machines agree on when.
CREATE TABLE lease (
    name       text        PRIMARY KEY,
    holder     text        NOT NULL,
    expires_at timestamptz NOT NULL
);

-- An answer, or a close, written ahead: stored before it is sent, and
-- finished with what Core said.
CREATE TABLE attempt (
    agent_id          text        NOT NULL,
    key               text        NOT NULL,
    -- seq orders attempts written at one instant as they were written.
    seq               bigint      GENERATED ALWAYS AS IDENTITY,
    member_id         text        NOT NULL,
    course_id         text        NOT NULL,
    conversation_id   text        NOT NULL,
    message_id        text        NOT NULL, -- '' for a close
    attempt_no        integer     NOT NULL, -- 0 for a close
    tool              text        NOT NULL,
    args              bytea       NOT NULL, -- bytea, not jsonb: jsonb would not keep the bytes
    kind              text        NOT NULL,
    state             text        NOT NULL,
    action_id         text,
    posted_message_id text,
    error_code        text,
    reason            text,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    PRIMARY KEY (agent_id, key),
    CONSTRAINT attempt_state_known CHECK (state IN
        ('sending', 'executed', 'proposed', 'failed', 'denied', 'error', 'rejected', 'cancelled'))
);
CREATE INDEX attempt_message_idx ON attempt (agent_id, conversation_id, message_id);
CREATE INDEX attempt_action_idx ON attempt (agent_id, action_id) WHERE action_id IS NOT NULL;
CREATE INDEX attempt_member_state_idx ON attempt (agent_id, member_id, state);

-- Where the runtime has read up to, per seat: event_list's next_seq,
-- action_list_mine's after.
CREATE TABLE cursor (
    agent_id   text        NOT NULL,
    member_id  text        NOT NULL,
    kind       text        NOT NULL,
    value      text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, member_id, kind)
);

-- Memory, keyed on the seat, then the conversation (§2.5).
CREATE TABLE note (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id        text        NOT NULL,
    member_id       text        NOT NULL,
    conversation_id text        NOT NULL,
    kind            text        NOT NULL,
    text            text        NOT NULL,
    message_id      text,
    created_at      timestamptz NOT NULL
);
CREATE INDEX note_conversation_idx ON note (agent_id, member_id, conversation_id, created_at);

-- The seats the runtime has seen, so that a seat's memory is purged a while
-- after it leaves me_memberships.
CREATE TABLE seat (
    agent_id  text        NOT NULL,
    member_id text        NOT NULL,
    course_id text        NOT NULL,
    seen_at   timestamptz NOT NULL,
    gone_at   timestamptz,
    PRIMARY KEY (agent_id, member_id)
);
CREATE INDEX seat_gone_idx ON seat (gone_at) WHERE gone_at IS NOT NULL;

-- The ledger (§5.3): ids and numbers, never text. Quotas sum it by agent,
-- tenant, and asker (course, opener) since the start of a day.
CREATE TABLE llm_call (
    agent_id           text        NOT NULL,
    id                 text        NOT NULL,
    at                 timestamptz NOT NULL,
    tenant_id          text        NOT NULL,
    course_id          text        NOT NULL,
    member_id          text        NOT NULL,
    conversation_id    text        NOT NULL,
    message_id         text        NOT NULL,
    opener_member_id   text        NOT NULL,
    adapter            text        NOT NULL,
    provider           text        NOT NULL,
    model              text        NOT NULL,
    stop               text        NOT NULL,
    raw_stop           text        NOT NULL,
    input_tokens       bigint      NOT NULL,
    cache_read_tokens  bigint      NOT NULL,
    cache_write_tokens bigint      NOT NULL,
    output_tokens      bigint      NOT NULL,
    reasoning_tokens   bigint      NOT NULL,
    estimated          boolean     NOT NULL,
    raw_usage          jsonb,
    price_version      text        NOT NULL,
    cost_pusd          bigint      NOT NULL,
    key_source         text        NOT NULL,
    latency_ms         bigint      NOT NULL,
    PRIMARY KEY (agent_id, id)
);
CREATE INDEX llm_call_agent_idx ON llm_call (agent_id, at);
CREATE INDEX llm_call_tenant_idx ON llm_call (tenant_id, at);
CREATE INDEX llm_call_asker_idx ON llm_call (course_id, opener_member_id, at);
CREATE INDEX llm_call_at_idx ON llm_call (at);

CREATE TABLE answer (
    agent_id         text        NOT NULL,
    id               text        NOT NULL,
    -- seq orders answers recorded at one instant as they were recorded.
    seq              bigint      GENERATED ALWAYS AS IDENTITY,
    at               timestamptz NOT NULL,
    tenant_id        text        NOT NULL,
    course_id        text        NOT NULL,
    member_id        text        NOT NULL,
    conversation_id  text        NOT NULL,
    message_id       text        NOT NULL,
    opener_member_id text        NOT NULL,
    key              text        NOT NULL,
    outcome          text        NOT NULL,
    billable         boolean     NOT NULL,
    turns            integer     NOT NULL,
    tool_calls       integer     NOT NULL,
    input_tokens     bigint      NOT NULL,
    output_tokens    bigint      NOT NULL,
    cost_pusd        bigint      NOT NULL,
    key_source       text        NOT NULL,
    prompt_hash      text        NOT NULL,
    latency_ms       bigint      NOT NULL,
    PRIMARY KEY (agent_id, id)
);
CREATE INDEX answer_agent_idx ON answer (agent_id, at);
CREATE INDEX answer_tenant_idx ON answer (tenant_id, at);
CREATE INDEX answer_asker_idx ON answer (course_id, opener_member_id, at);
CREATE INDEX answer_at_idx ON answer (at);

-- What the owner's page shows about each agent.
CREATE TABLE agent_state (
    agent_id   text        PRIMARY KEY,
    state      text        NOT NULL,
    detail     text        NOT NULL,
    worker     text        NOT NULL,
    updated_at timestamptz NOT NULL
);

COMMIT;
