-- 000114_add_fk_order_lines_payout.down.sql
BEGIN;
ALTER TABLE order_lines DROP CONSTRAINT IF EXISTS order_lines_payout_id_payee_id_fkey;
COMMIT;
