-- 000025_create_notification_deliveries.down.sql
-- DATA LOSS: the delivery log (deduplication history of notifications).
BEGIN;
DROP TABLE IF EXISTS notification_deliveries;
COMMIT;
