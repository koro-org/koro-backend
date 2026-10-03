-- 000003_create_outbox.down.sql
-- DATA LOSS: unpublished domain events.
BEGIN;
DROP TABLE IF EXISTS outbox;
COMMIT;
