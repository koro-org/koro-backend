-- 000122_model_c_direct_confirmation.down
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TRIGGER orders_direct_confirmation_guard ON orders;
DROP FUNCTION koro_orders_direct_confirmation_guard();
ALTER TABLE orders DROP COLUMN direct_confirmed_at;
COMMIT;
