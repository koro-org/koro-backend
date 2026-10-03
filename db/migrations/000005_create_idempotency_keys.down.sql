-- 000005_create_idempotency_keys.down.sql
-- DATA LOSS: stored idempotent responses (at most 24 hours old).
BEGIN;
DROP TABLE IF EXISTS idempotency_keys;
COMMIT;
