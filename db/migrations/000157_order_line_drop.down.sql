-- 000157_order_line_drop.down.sql (BE-134). Back to frozen order amounts:
-- an order's totals are the sums of all its lines again. Refused while a
-- line is dropped: its order's amounts no longer include it, and without
-- dropped_at it would read as a cancelled line whose money waits for a
-- refund.
BEGIN;
SET LOCAL lock_timeout = '5s';
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM order_lines WHERE dropped_at IS NOT NULL) THEN
    RAISE EXCEPTION '000157 down: order lines were dropped from their orders; their amounts cannot be frozen with them again';
  END IF;
END $$;
DROP TRIGGER orders_totals_match_lines ON orders;
DROP TRIGGER orders_totals_match_lines ON order_lines;
CREATE OR REPLACE FUNCTION public.orders_check_lines_total() RETURNS trigger
  LANGUAGE plpgsql AS $$
DECLARE
  order_ref uuid := (to_jsonb(NEW) ->> TG_ARGV[0])::uuid;
  o         record;
BEGIN
  SELECT ord.subtotal_amount, ord.vat_amount, ord.vat_withheld_amount, ord.buyer_fee_amount, ord.payable_amount,
         count(l.id) AS line_count,
         coalesce(sum(l.net_amount), 0) AS net, coalesce(sum(l.vat_amount), 0) AS vat,
         coalesce(sum(l.vat_withheld_amount), 0) AS withheld, coalesce(sum(l.buyer_fee_amount), 0) AS fee,
         coalesce(sum(l.payable_amount), 0) AS payable
    INTO o
    FROM orders ord LEFT JOIN order_lines l ON l.order_id = ord.id
   WHERE ord.id = order_ref
   GROUP BY ord.id;
  IF NOT FOUND THEN
    RETURN NULL;
  END IF;
  IF o.line_count = 0 OR o.subtotal_amount <> o.net OR o.vat_amount <> o.vat OR o.vat_withheld_amount <> o.withheld
     OR o.buyer_fee_amount <> o.fee OR o.payable_amount <> o.payable THEN
    RAISE EXCEPTION 'order % does not match its % line(s): subtotal %/%, vat %/%, withheld %/%, buyer fee %/%, payable %/%',
      order_ref, o.line_count, o.subtotal_amount, o.net, o.vat_amount, o.vat, o.vat_withheld_amount, o.withheld,
      o.buyer_fee_amount, o.fee, o.payable_amount, o.payable
      USING ERRCODE = 'check_violation', CONSTRAINT = 'orders_totals_match_lines';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER orders_totals_match_lines AFTER INSERT ON orders
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.orders_check_lines_total('id');
CREATE CONSTRAINT TRIGGER orders_totals_match_lines AFTER INSERT ON order_lines
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.orders_check_lines_total('order_id');
DROP TRIGGER orders_amounts_guard ON orders;
DROP FUNCTION public.orders_amounts_guard();
DROP TRIGGER orders_frozen ON orders;
CREATE TRIGGER orders_frozen BEFORE UPDATE ON orders
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'order_no', 'org_id', 'org_kind', 'placed_by_user_id',
    'buyer_party_id', 'buyer_snapshot', 'model_code', 'collection_method', 'currency', 'subtotal_amount', 'vat_amount',
    'vat_withheld_amount', 'buyer_fee_amount', 'total_amount', 'payable_amount', 'koro_collect_amount',
    'direct_to_payee_amount', 'placed_at');
DROP TRIGGER order_lines_drop_guard ON order_lines;
DROP FUNCTION public.order_lines_drop_guard();
DROP TRIGGER order_lines_dropped_frozen ON order_lines;
ALTER TABLE order_lines DROP CONSTRAINT order_lines_dropped_chk;
ALTER TABLE order_lines DROP COLUMN dropped_at;
COMMIT;
