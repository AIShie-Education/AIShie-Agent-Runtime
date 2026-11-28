-- AIShie Agent Runtime, store migration 0017 (down)
-- Drops only the additive AR chat schema from 0017_chat.up.sql.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE chat_approval_action;
DROP TABLE chat_approval;
DROP TABLE chat_generation;
DROP TABLE chat_idempotency;
DROP TABLE chat_attachment;
DROP TABLE chat_draft;
DROP TABLE chat_message;
DROP TABLE chat_conversation;

COMMIT;
