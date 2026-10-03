-- 000004_create_webhook_inbox.down.sql
-- DATA LOSS: received webhooks (deduplication history).
BEGIN;
DROP TABLE IF EXISTS webhook_inbox;
COMMIT;
