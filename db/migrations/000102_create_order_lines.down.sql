-- 000102_create_order_lines.down.sql
-- DATA LOSS: every order line (frozen prices, commissions, payees).
BEGIN;
DROP TRIGGER IF EXISTS orders_totals_match_lines ON orders;
DROP TABLE IF EXISTS order_lines;
DROP FUNCTION IF EXISTS public.orders_check_lines_total();
COMMIT;
