-- 000157_order_line_drop.up.sql (BE-134; Q-28c, user decision 2026-10-26;
-- 02 §19.1, §19.2 doc patch). When an offering of an unpaid cart is
-- cancelled, only its line drops out of the order: the order keeps its
-- reference code and its amounts become the sums of its other lines
-- (computed in Go per line, as at placement). Until now every amount of an
-- order was frozen and an unpaid order with a cancelled offering was
-- cancelled whole.
--   * order_lines.dropped_at: the line left its unpaid order. A dropped
--     line is cancelled, never refunded nor paid out (order_lines_dropped_chk)
--     and stays so (order_lines_dropped_frozen): it carries no money.
--   * order_lines_drop_guard: a line drops only while its order awaits
--     payment and took no money — nothing paid (no transfer matched, not
--     even in part) and, for model C, no direct payment confirmed; a line is
--     never inserted dropped. Raised as check_violation, CONSTRAINT
--     order_lines_drop_unpaid (platform/db.Translate maps it).
--   * orders_frozen keeps the parties, the model, the currency and the
--     reference code frozen for good; the amounts move to
--     orders_amounts_guard: they change only on such an unpaid order, only
--     to the sums of its live (not dropped) lines — the lines are dropped
--     first in the same transaction — with Koro's part the model's
--     (direct_to_payee: the buyer fee; otherwise everything). Anything else
--     stays a restrict_violation (23001), as before.
--   * orders_totals_match_lines sums the live lines only and also runs on a
--     change of the order amounts or of a line's dropped_at (deferred, at
--     commit): an order keeps at least one live line (dropping the last one
--     cancels the order instead) and its totals match them.
BEGIN;
SET LOCAL lock_timeout = '5s';

ALTER TABLE order_lines ADD COLUMN dropped_at timestamptz;
ALTER TABLE order_lines ADD CONSTRAINT order_lines_dropped_chk
  CHECK (dropped_at IS NULL OR (fulfillment_status = 'cancelled' AND refunded_amount = 0 AND payout_id IS NULL));
CREATE TRIGGER order_lines_dropped_frozen BEFORE UPDATE ON order_lines
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('dropped_at', 'dropped_at', 'fulfillment_status', 'refunded_amount');

CREATE FUNCTION public.order_lines_drop_guard() RETURNS trigger
  LANGUAGE plpgsql AS $$
DECLARE
  o record;
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.dropped_at IS NOT NULL THEN
      RAISE EXCEPTION 'order line % cannot be inserted dropped', NEW.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'order_lines_drop_unpaid';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.dropped_at IS NULL OR OLD.dropped_at IS NOT NULL THEN
    RETURN NEW; -- not a drop (order_lines_dropped_frozen keeps a drop)
  END IF;
  SELECT status, paid_amount, direct_confirmed_at INTO o FROM orders WHERE id = NEW.order_id;
  IF NOT FOUND OR o.status <> 'awaiting_payment' OR o.paid_amount <> 0 OR o.direct_confirmed_at IS NOT NULL THEN
    RAISE EXCEPTION 'order line % cannot be dropped: its order % is %, % paid, direct payment confirmed at %',
      NEW.id, NEW.order_id, o.status, o.paid_amount, o.direct_confirmed_at
      USING ERRCODE = 'check_violation', CONSTRAINT = 'order_lines_drop_unpaid';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER order_lines_drop_guard BEFORE INSERT OR UPDATE OF dropped_at ON order_lines
  FOR EACH ROW EXECUTE FUNCTION public.order_lines_drop_guard();

DROP TRIGGER orders_frozen ON orders;
CREATE TRIGGER orders_frozen BEFORE UPDATE ON orders
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'order_no', 'org_id', 'org_kind', 'placed_by_user_id',
    'buyer_party_id', 'buyer_snapshot', 'model_code', 'collection_method', 'currency', 'placed_at');

CREATE FUNCTION public.orders_amounts_guard() RETURNS trigger
  LANGUAGE plpgsql AS $$
DECLARE
  l       record;
  collect bigint;
BEGIN
  IF (NEW.subtotal_amount, NEW.vat_amount, NEW.vat_withheld_amount, NEW.buyer_fee_amount, NEW.total_amount,
      NEW.payable_amount, NEW.koro_collect_amount, NEW.direct_to_payee_amount)
     IS NOT DISTINCT FROM
     (OLD.subtotal_amount, OLD.vat_amount, OLD.vat_withheld_amount, OLD.buyer_fee_amount, OLD.total_amount,
      OLD.payable_amount, OLD.koro_collect_amount, OLD.direct_to_payee_amount) THEN
    RETURN NEW;
  END IF;
  SELECT count(*) AS n, coalesce(sum(net_amount), 0) AS net, coalesce(sum(vat_amount), 0) AS vat,
         coalesce(sum(vat_withheld_amount), 0) AS withheld, coalesce(sum(buyer_fee_amount), 0) AS fee,
         coalesce(sum(payable_amount), 0) AS payable
    INTO l
    FROM order_lines WHERE order_id = OLD.id AND dropped_at IS NULL;
  collect := CASE WHEN NEW.collection_method = 'direct_to_payee' THEN l.fee ELSE l.payable END; -- Koro's part (02 §18.3)
  IF OLD.status <> 'awaiting_payment' OR NEW.status <> 'awaiting_payment' OR OLD.paid_amount <> 0 OR NEW.paid_amount <> 0
     OR OLD.direct_confirmed_at IS NOT NULL OR NEW.direct_confirmed_at IS NOT NULL OR l.n = 0
     OR NEW.subtotal_amount <> l.net OR NEW.vat_amount <> l.vat OR NEW.vat_withheld_amount <> l.withheld
     OR NEW.buyer_fee_amount <> l.fee OR NEW.payable_amount <> l.payable
     OR NEW.koro_collect_amount <> collect THEN
    RAISE EXCEPTION 'UPDATE of orders.% amounts is not allowed: they follow only the lines dropped from an unpaid order', OLD.id
      USING ERRCODE = 'restrict_violation', CONSTRAINT = 'orders_amounts_frozen';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER orders_amounts_guard BEFORE UPDATE ON orders
  FOR EACH ROW EXECUTE FUNCTION public.orders_amounts_guard();

-- The order totals are the sums of its live lines (checked at commit, so the
-- order and its lines are written in any order within one transaction).
-- TG_ARGV[0] names the column of the trigger row that holds the order id.
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
    FROM orders ord LEFT JOIN order_lines l ON l.order_id = ord.id AND l.dropped_at IS NULL
   WHERE ord.id = order_ref
   GROUP BY ord.id;
  IF NOT FOUND THEN
    RETURN NULL;
  END IF;
  IF o.line_count = 0 OR o.subtotal_amount <> o.net OR o.vat_amount <> o.vat OR o.vat_withheld_amount <> o.withheld
     OR o.buyer_fee_amount <> o.fee OR o.payable_amount <> o.payable THEN
    RAISE EXCEPTION 'order % does not match its % live line(s): subtotal %/%, vat %/%, withheld %/%, buyer fee %/%, payable %/%',
      order_ref, o.line_count, o.subtotal_amount, o.net, o.vat_amount, o.vat, o.vat_withheld_amount, o.withheld,
      o.buyer_fee_amount, o.fee, o.payable_amount, o.payable
      USING ERRCODE = 'check_violation', CONSTRAINT = 'orders_totals_match_lines';
  END IF;
  RETURN NULL;
END $$;
DROP TRIGGER orders_totals_match_lines ON orders;
DROP TRIGGER orders_totals_match_lines ON order_lines;
CREATE CONSTRAINT TRIGGER orders_totals_match_lines
  AFTER INSERT OR UPDATE OF subtotal_amount, vat_amount, vat_withheld_amount, buyer_fee_amount, payable_amount ON orders
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.orders_check_lines_total('id');
CREATE CONSTRAINT TRIGGER orders_totals_match_lines AFTER INSERT OR UPDATE OF dropped_at ON order_lines
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.orders_check_lines_total('order_id');
COMMIT;
