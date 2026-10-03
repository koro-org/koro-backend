-- 000108_create_refunds.down.sql
-- DATA LOSS: every refund with its four-eyes trail.
BEGIN;
DROP TABLE IF EXISTS refunds;
COMMIT;
