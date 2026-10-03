-- 000101_create_orders.down.sql
-- DATA LOSS: every order.
BEGIN;
DROP TABLE IF EXISTS orders;
COMMIT;
