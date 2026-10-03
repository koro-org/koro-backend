-- 000115_create_payout_items.down.sql
-- DATA LOSS: the lines of every payout.
BEGIN;
ALTER TABLE order_lines DROP CONSTRAINT IF EXISTS order_lines_payout_id_id_fkey;
DROP TRIGGER IF EXISTS payout_items_sum ON payouts;
DROP TABLE IF EXISTS payout_items;
DROP FUNCTION IF EXISTS public.payouts_check_items();
DROP FUNCTION IF EXISTS public.payout_items_check();
COMMIT;
