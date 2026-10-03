-- 000107_create_payment_splits.down.sql
-- DATA LOSS: the per-line split of every payment.
BEGIN;
DROP TABLE IF EXISTS payment_splits;
COMMIT;
