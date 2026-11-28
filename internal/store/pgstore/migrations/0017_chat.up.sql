-- AIShie Agent Runtime, store migration 0017 (up)
-- AR-owned browser chat. Existing migrations 0001--0016 are immutable. No
-- Core conversation id, Core token, provider key or provider error text is
-- stored here. All timestamps use the database clock unless supplied by a
-- store contract test.

BEGIN;

SET LOCAL lock_timeout = '10s';

CREATE TABLE chat_conversation (
    id text PRIMARY KEY,
    course_id text NOT NULL,
    agent_id text NOT NULL,
    runtime_agent_id text NOT NULL DEFAULT '',
    opener_actor_id text NOT NULL,
    title text NOT NULL,
    state text NOT NULL CHECK (state IN ('queued', 'answering', 'answered', 'reply_pending_approval', 'failed', 'closed')),
    revision bigint NOT NULL CHECK (revision > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    last_activity_at timestamptz NOT NULL,
    closed_at timestamptz,
    close_reason text NOT NULL DEFAULT ''
);
CREATE INDEX chat_conversation_owner ON chat_conversation
    (course_id, opener_actor_id, last_activity_at DESC, id DESC);

CREATE TABLE chat_message (
    id text PRIMARY KEY,
    conversation_id text NOT NULL REFERENCES chat_conversation(id) ON DELETE CASCADE,
    seq bigint NOT NULL CHECK (seq > 0),
    role text NOT NULL CHECK (role IN ('user', 'assistant')),
    status text NOT NULL CHECK (status IN ('posted', 'pending', 'retracted', 'failed')),
    body text NOT NULL,
    sources jsonb,
    sources_stated boolean,
    usage jsonb,
    finish_reason text NOT NULL DEFAULT '',
    error_kind text NOT NULL DEFAULT '',
    error_retryable boolean,
    created_at timestamptz NOT NULL,
    retracted_at timestamptz,
    retracted_reason text NOT NULL DEFAULT '',
    UNIQUE (conversation_id, seq),
    CHECK (sources IS NULL OR jsonb_typeof(sources) = 'array'),
    CHECK (usage IS NULL OR jsonb_typeof(usage) = 'object'),
    CHECK (status <> 'retracted' OR body = '')
);
CREATE INDEX chat_message_page ON chat_message (conversation_id, seq DESC);

CREATE TABLE chat_draft (
    conversation_id text PRIMARY KEY REFERENCES chat_conversation(id) ON DELETE CASCADE,
    version bigint NOT NULL CHECK (version > 0),
    text text NOT NULL,
    steps jsonb NOT NULL CHECK (jsonb_typeof(steps) = 'array'),
    updated_at timestamptz NOT NULL
);

CREATE TABLE chat_attachment (
    id text PRIMARY KEY,
    course_id text NOT NULL,
    owner_actor_id text NOT NULL,
    message_id text REFERENCES chat_message(id) ON DELETE CASCADE,
    position integer NOT NULL DEFAULT -1,
    filename text NOT NULL,
    media_type text NOT NULL,
    size bigint NOT NULL CHECK (size >= 0),
    bytes bytea NOT NULL,
    idempotency_key text,
    request_fingerprint text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    CHECK (media_type LIKE 'image/%' OR media_type LIKE 'text/%'),
    CHECK (NOT media_type LIKE 'image/%' OR size <= 5242880),
    CHECK (NOT media_type LIKE 'text/%' OR size <= 32768)
);
CREATE INDEX chat_attachment_owner ON chat_attachment (course_id, owner_actor_id, id);
CREATE UNIQUE INDEX chat_attachment_idempotency ON chat_attachment (course_id, owner_actor_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX chat_attachment_message ON chat_attachment (message_id, position);

CREATE TABLE chat_idempotency (
    course_id text NOT NULL,
    actor_id text NOT NULL,
    key text NOT NULL,
    operation text NOT NULL,
    fingerprint text NOT NULL,
    conversation_id text NOT NULL REFERENCES chat_conversation(id) ON DELETE CASCADE,
    message_id text NOT NULL,
    submission_status text,
    review_state text,
    approval_id text,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (course_id, actor_id, key)
);

CREATE TABLE chat_generation (
    id text PRIMARY KEY,
    conversation_id text NOT NULL REFERENCES chat_conversation(id) ON DELETE CASCADE,
    user_message_id text NOT NULL REFERENCES chat_message(id) ON DELETE CASCADE,
    assistant_message_id text NOT NULL UNIQUE,
    state text NOT NULL CHECK (state IN ('queued', 'running', 'proposed', 'done', 'failed', 'cancelled')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    holder text NOT NULL DEFAULT '',
    fence bigint NOT NULL DEFAULT 0 CHECK (fence >= 0),
    answer_level text NOT NULL DEFAULT 'autonomous' CHECK (answer_level IN ('autonomous', 'pending_review', 'confirm_required')),
    revision_reason text NOT NULL DEFAULT '',
    revision_body text NOT NULL DEFAULT '',
    revision_of_generation_id text NOT NULL DEFAULT '',
    lease_expires_at timestamptz NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    result jsonb,
    error_kind text NOT NULL DEFAULT '',
    error_retryable boolean,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK (result IS NULL OR jsonb_typeof(result) = 'object')
);
CREATE INDEX chat_generation_recovery ON chat_generation (state, created_at, id);
CREATE INDEX chat_generation_conversation ON chat_generation (conversation_id, created_at);

CREATE TABLE chat_approval (
    id text PRIMARY KEY,
    course_id text NOT NULL,
    conversation_id text NOT NULL REFERENCES chat_conversation(id) ON DELETE CASCADE,
    subject text NOT NULL CHECK (subject IN ('question', 'answer')),
    message_id text NOT NULL REFERENCES chat_message(id) ON DELETE CASCADE,
    generation_id text NOT NULL REFERENCES chat_generation(id) ON DELETE CASCADE,
    mode text NOT NULL CHECK (mode IN ('decision', 'review')),
    state text NOT NULL CHECK (state IN ('pending', 'approved', 'rejected', 'changes_requested', 'reviewed', 'cancelled')),
    reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    decided_at timestamptz
);
CREATE INDEX chat_approval_course ON chat_approval (course_id, state, created_at, id);
CREATE INDEX chat_approval_conversation ON chat_approval (conversation_id, created_at);

CREATE TABLE chat_approval_action (
    course_id text NOT NULL,
    actor_id text NOT NULL,
    key text NOT NULL,
    approval_id text NOT NULL REFERENCES chat_approval(id) ON DELETE CASCADE,
    operation text NOT NULL CHECK (operation IN ('decide', 'review')),
    fingerprint text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (course_id, actor_id, key)
);

COMMIT;
