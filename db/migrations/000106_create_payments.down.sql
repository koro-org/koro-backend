-- 000106_create_payments.down.sql
-- DATA LOSS: every captured payment.
BEGIN;
DROP TABLE IF EXISTS payments;
COMMIT;
