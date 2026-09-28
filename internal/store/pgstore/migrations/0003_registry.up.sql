-- AIShie Agent Runtime, store migration 0003 (up)
-- The registry of hosted agents (docs/design.md §11.2): agents people
-- connect rather than an operator writing YAML, their settings per course,
-- and the people who use the runtime's API. The registry turns each hosted
-- agent into the same agent document a YAML file holds, and every worker
-- reloads when the registry's revision moves on: every statement that
-- writes a hosted agent or course moves it, by trigger, and tells every
-- listener (NOTIFY aishie_registry), a write made by hand included.

BEGIN;

SET LOCAL lock_timeout = '10s';

-- The people who have used the API, as Core's assertion named them.
CREATE TABLE person (
    core_actor_id text        PRIMARY KEY,
    display_name  text        NOT NULL,
    platform_role text        NOT NULL, -- root, admin, or ''
    last_seen_at  timestamptz NOT NULL
);

-- One row per hosted agent. Its token, and its owner's own model key when
-- one is stored, are sealed secrets of its tenant (0002), which go with it.
-- settings is the rest of the agent document, never a reference.
CREATE TABLE hosted_agent (
    id              text        PRIMARY KEY,
    core_actor_id   text        NOT NULL UNIQUE,
    owner_actor_id  text        NOT NULL, -- '' when Core names no owner
    owner_verified  boolean     NOT NULL,
    tenant_id       text        NOT NULL,
    display_name    text        NOT NULL,
    token_secret_id text        NOT NULL UNIQUE REFERENCES secret (id),
    token_hint      text        NOT NULL,
    key_secret_id   text        UNIQUE REFERENCES secret (id),
    key_hint        text        NOT NULL,
    paused          boolean     NOT NULL,
    settings        jsonb       NOT NULL,
    version         integer     NOT NULL,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    CONSTRAINT hosted_agent_id_shape CHECK (id ~ '^agt_[A-Za-z0-9_-]{1,60}$'),
    CONSTRAINT hosted_agent_settings_object CHECK (jsonb_typeof(settings) = 'object'),
    CONSTRAINT hosted_agent_two_secrets CHECK (key_secret_id IS DISTINCT FROM token_secret_id)
);
CREATE INDEX hosted_agent_owner_idx ON hosted_agent (owner_actor_id);

-- A hosted agent's settings for one course: courses[course_id].
CREATE TABLE hosted_course (
    agent_id   text        NOT NULL REFERENCES hosted_agent (id) ON DELETE CASCADE,
    course_id  text        NOT NULL,
    settings   jsonb       NOT NULL,
    updated_by text        NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (agent_id, course_id),
    CONSTRAINT hosted_course_settings_object CHECK (jsonb_typeof(settings) = 'object')
);

-- The registry's revision: one row.
CREATE TABLE registry_rev (
    one boolean PRIMARY KEY DEFAULT true,
    rev bigint  NOT NULL,
    CONSTRAINT registry_rev_one_row CHECK (one)
);
INSERT INTO registry_rev (rev) VALUES (0);

CREATE FUNCTION registry_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE registry_rev SET rev = rev + 1;
    -- Delivered when the transaction commits; one for all its statements.
    PERFORM pg_notify('aishie_registry', '');
    RETURN NULL;
END
$$;

CREATE TRIGGER hosted_agent_changed AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON hosted_agent
    FOR EACH STATEMENT EXECUTE FUNCTION registry_changed();
CREATE TRIGGER hosted_course_changed AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON hosted_course
    FOR EACH STATEMENT EXECUTE FUNCTION registry_changed();

COMMIT;
