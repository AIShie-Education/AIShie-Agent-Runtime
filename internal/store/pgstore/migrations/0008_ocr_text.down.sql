-- AIShie Agent Runtime, store migration 0008 (down)
-- Reverts 0008_ocr_text.up.sql: what OCR recognized goes, and is recognized
-- again when next asked for.

BEGIN;

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS ocr_text;

COMMIT;
