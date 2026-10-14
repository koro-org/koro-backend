-- 000102_create_order_lines.up.sql (02 §19.2; 06 H-6). An order line: the
-- offering, the frozen payee and the frozen commerce policy parameters with
-- every amount computed by platform/money (per line, half away from zero,
-- 06 H-6). Example (model A, 10 % from the seller, no withholding): net
-- 1,000,000, VAT 200,000, gross 1,200,000, commission 100,000 + VAT 20,000
-- ⇒ seller_net 1,080,000, payable 1,200,000. A platform-native line is
-- 'not_applicable' for fulfilment. payout_id gets its FKs in 000114/000115.
-- Never deleted (financial_records): app_rw has no DELETE.
-- 02 fixes and additions (H-20):
--   * 06 H-37: `(fulfillment_status = 'fulfilled') = (fulfilled_at IS NOT
--     NULL AND fulfilled_via IS NOT NULL)` let a line outside 'fulfilled'
--     carry one of the two; each is tied to the state. A disputed line has a
--     reason.
--   * 06 H-38: (order_id, org_id) → orders, (offering_id, seller_org_id) →
--     offerings, (payee_id, seller_org_id) → payees: the buyer is the
--     order's, the payee belongs to the seller org that sold the offering.
--     order_lines_id_order_uq and order_lines_id_payee_uq are the targets of
--     payment_splits, refunds, ledger_entries and payout_items.
--   * money: vat_withheld_amount <= vat_amount, a valid withholding ratio; a
--     line is paid out only to its payee and only once fulfilled.
--   * frozen values (H-6, ADR-0014): order_lines_frozen keeps the payee, the
--     policy copy and every amount; order_lines_paid_out_frozen keeps
--     payout_id / paid_out_at once set (no second payout of a line).
--   * 06 H-55 (BE-040): (event_id, seller_org_id) → events (id, org_id)
--     (events_id_org_uq): a line is bound only to an event of its seller org
--     (the offering's org); MATCH SIMPLE leaves a line without an event free.
--   * the policy windows are frozen too (02 §18.3 "commerce_policy_id + tüm
--     parametreler"; BE-040): dispute_window_days starts the buyer's dispute
--     window when the seller marks the line, payout_hold_days delays its
--     payout after fulfilment (T-13: 7 / 0 days by default).
--   * order_lines_offering_idx: the lines of an offering (the per-buyer
--     limit of catalog.Reserve, the offering.cancelled consumer; BE-040).
--   * orders_totals_match_lines: deferred constraint trigger on orders and
--     order_lines; at commit an order has at least one line and its subtotal,
--     VAT, withholding, buyer fee and payable amounts equal the sums of its
--     lines (raised as check_violation, CONSTRAINT orders_totals_match_lines,
--     so platform/db.Translate maps it).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE order_lines (
  id                       uuid PRIMARY KEY DEFAULT uuidv7(),
  order_id                 uuid NOT NULL,
  org_id                   uuid NOT NULL,                    -- buyer (= orders.org_id, composite FK)
  seller_org_id            uuid NOT NULL,                    -- = offerings.org_id (composite FK)
  payee_id                 uuid,                             -- frozen; NULL for a platform seller
  offering_id              uuid NOT NULL,
  service_id               uuid NOT NULL CONSTRAINT order_lines_service_id_fkey REFERENCES services (id),
  event_id                 uuid,                             -- an event of the seller org (composite FK, 06 H-55)
  commerce_policy_id       uuid NOT NULL CONSTRAINT order_lines_commerce_policy_id_fkey REFERENCES commerce_policies (id),
  title_snapshot           text NOT NULL,
  deliverables_snapshot    jsonb NOT NULL DEFAULT '[]',
  quantity                 integer NOT NULL CONSTRAINT order_lines_quantity_chk CHECK (quantity > 0),
  unit_price_amount        bigint NOT NULL CONSTRAINT order_lines_unit_price_amount_chk CHECK (unit_price_amount > 0),
  net_amount               bigint NOT NULL CONSTRAINT order_lines_net_amount_chk CHECK (net_amount >= 0),
  vat_rate_bp              integer NOT NULL CONSTRAINT order_lines_vat_rate_bp_chk CHECK (vat_rate_bp BETWEEN 0 AND 10000),
  vat_amount               bigint NOT NULL CONSTRAINT order_lines_vat_amount_chk CHECK (vat_amount >= 0),
  vat_withholding_num      smallint,
  vat_withholding_den      smallint,
  vat_withheld_amount      bigint NOT NULL DEFAULT 0 CONSTRAINT order_lines_vat_withheld_amount_chk CHECK (vat_withheld_amount >= 0),
  gross_amount             bigint NOT NULL CONSTRAINT order_lines_gross_amount_chk CHECK (gross_amount >= 0),
  commission_rate_bp       integer NOT NULL CONSTRAINT order_lines_commission_rate_bp_chk CHECK (commission_rate_bp BETWEEN 0 AND 10000),
  commission_basis         text NOT NULL CONSTRAINT order_lines_commission_basis_chk CHECK (commission_basis IN ('net_excl_vat','gross_incl_vat')),
  commission_payer         text NOT NULL CONSTRAINT order_lines_commission_payer_chk CHECK (commission_payer IN ('seller','buyer')),
  commission_amount        bigint NOT NULL CONSTRAINT order_lines_commission_amount_chk CHECK (commission_amount >= 0),
  commission_vat_rate_bp   integer NOT NULL CONSTRAINT order_lines_commission_vat_rate_bp_chk CHECK (commission_vat_rate_bp BETWEEN 0 AND 10000),
  commission_vat_amount    bigint NOT NULL CONSTRAINT order_lines_commission_vat_amount_chk CHECK (commission_vat_amount >= 0),
  dispute_window_days      smallint NOT NULL DEFAULT 7 CONSTRAINT order_lines_dispute_window_days_chk CHECK (dispute_window_days BETWEEN 0 AND 60),
  payout_hold_days         smallint NOT NULL DEFAULT 0 CONSTRAINT order_lines_payout_hold_days_chk CHECK (payout_hold_days BETWEEN 0 AND 90),
  buyer_fee_amount         bigint NOT NULL CONSTRAINT order_lines_buyer_fee_amount_chk CHECK (buyer_fee_amount >= 0),
  seller_net_amount        bigint NOT NULL CONSTRAINT order_lines_seller_net_amount_chk CHECK (seller_net_amount >= 0),   -- the payee's earning
  payable_amount           bigint NOT NULL CONSTRAINT order_lines_payable_amount_chk CHECK (payable_amount >= 0),
  fulfillment_status       text NOT NULL CONSTRAINT order_lines_fulfillment_status_chk CHECK (fulfillment_status IN ('not_applicable','pending','marked',
                             'fulfilled','disputed','cancelled')),
  fulfillment_marked_at    timestamptz,
  fulfillment_marked_by    uuid CONSTRAINT order_lines_fulfillment_marked_by_fkey REFERENCES users (id),
  dispute_window_ends_at   timestamptz,
  fulfilled_at             timestamptz,
  fulfilled_via            text CONSTRAINT order_lines_fulfilled_via_chk CHECK (fulfilled_via IN ('buyer_confirmed','window_elapsed','admin','auto_platform')),
  dispute_reason           text CONSTRAINT order_lines_dispute_reason_chk CHECK (char_length(dispute_reason) <= 1000),
  refunded_amount          bigint NOT NULL DEFAULT 0 CONSTRAINT order_lines_refunded_amount_chk CHECK (refunded_amount >= 0),
  payout_id                uuid,                 -- FK payouts (000114) and payout_items (000115)
  paid_out_at              timestamptz,
  created_at               timestamptz NOT NULL DEFAULT now(),
  updated_at               timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT order_lines_order_id_org_id_fkey FOREIGN KEY (order_id, org_id) REFERENCES orders (id, org_id),
  CONSTRAINT order_lines_offering_id_seller_org_id_fkey FOREIGN KEY (offering_id, seller_org_id) REFERENCES offerings (id, org_id),
  CONSTRAINT order_lines_payee_id_seller_org_id_fkey FOREIGN KEY (payee_id, seller_org_id) REFERENCES payees (id, org_id),
  CONSTRAINT order_lines_event_id_seller_org_id_fkey FOREIGN KEY (event_id, seller_org_id) REFERENCES events (id, org_id),
  CONSTRAINT order_lines_id_order_uq UNIQUE (id, order_id),
  CONSTRAINT order_lines_id_payee_uq UNIQUE (id, payee_id),
  -- amounts (kuruş; 02 §19.2 rules, computed by platform/money)
  CONSTRAINT order_lines_net_chk CHECK (net_amount = quantity * unit_price_amount),
  CONSTRAINT order_lines_gross_chk CHECK (gross_amount = net_amount + vat_amount),
  CONSTRAINT order_lines_withholding_pair_chk CHECK ((vat_withholding_num IS NULL) = (vat_withholding_den IS NULL)),
  CONSTRAINT order_lines_withholding_ratio_chk CHECK (vat_withholding_num IS NULL OR (vat_withholding_num > 0 AND vat_withholding_num <= vat_withholding_den)),
  CONSTRAINT order_lines_withheld_chk CHECK ((vat_withholding_num IS NOT NULL OR vat_withheld_amount = 0) AND vat_withheld_amount <= vat_amount),
  CONSTRAINT order_lines_buyer_fee_chk CHECK (buyer_fee_amount = CASE WHEN commission_payer = 'buyer' THEN commission_amount + commission_vat_amount ELSE 0 END),
  CONSTRAINT order_lines_seller_net_chk CHECK (seller_net_amount = gross_amount - vat_withheld_amount
         - CASE WHEN commission_payer = 'seller' THEN commission_amount + commission_vat_amount ELSE 0 END),
  CONSTRAINT order_lines_payable_chk CHECK (payable_amount = gross_amount - vat_withheld_amount + buyer_fee_amount),
  CONSTRAINT order_lines_refunded_limit_chk CHECK (refunded_amount <= payable_amount),
  -- fulfilment (06 H-37)
  CONSTRAINT order_lines_fulfilled_chk CHECK ((fulfillment_status = 'fulfilled') = (fulfilled_at IS NOT NULL)
                                          AND (fulfillment_status = 'fulfilled') = (fulfilled_via IS NOT NULL)),
  CONSTRAINT order_lines_marked_chk CHECK (fulfillment_status <> 'marked' OR (fulfillment_marked_at IS NOT NULL AND dispute_window_ends_at IS NOT NULL)),
  CONSTRAINT order_lines_disputed_chk CHECK (fulfillment_status <> 'disputed' OR dispute_reason IS NOT NULL),
  -- payout
  CONSTRAINT order_lines_paid_out_chk CHECK ((payout_id IS NULL) = (paid_out_at IS NULL)),
  CONSTRAINT order_lines_payout_line_chk CHECK (payout_id IS NULL OR (payee_id IS NOT NULL AND fulfillment_status = 'fulfilled'))
);
CREATE INDEX order_lines_order_idx   ON order_lines (order_id);
CREATE INDEX order_lines_seller_idx  ON order_lines (seller_org_id, created_at DESC);
CREATE INDEX order_lines_payable_idx ON order_lines (payee_id, fulfilled_at)
  WHERE fulfillment_status = 'fulfilled' AND payout_id IS NULL;
CREATE INDEX order_lines_window_idx  ON order_lines (dispute_window_ends_at) WHERE fulfillment_status = 'marked';
CREATE INDEX order_lines_offering_idx ON order_lines (offering_id);
CREATE TRIGGER order_lines_touch_updated_at BEFORE UPDATE ON order_lines
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER order_lines_frozen BEFORE UPDATE ON order_lines
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'order_id', 'org_id', 'seller_org_id', 'payee_id', 'offering_id',
    'service_id', 'event_id', 'commerce_policy_id', 'title_snapshot', 'deliverables_snapshot', 'quantity', 'unit_price_amount',
    'net_amount', 'vat_rate_bp', 'vat_amount', 'vat_withholding_num', 'vat_withholding_den', 'vat_withheld_amount',
    'gross_amount', 'commission_rate_bp', 'commission_basis', 'commission_payer', 'commission_amount',
    'commission_vat_rate_bp', 'commission_vat_amount', 'dispute_window_days', 'payout_hold_days', 'buyer_fee_amount',
    'seller_net_amount', 'payable_amount');
CREATE TRIGGER order_lines_paid_out_frozen BEFORE UPDATE ON order_lines
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('payout_id', 'payout_id', 'paid_out_at');

-- The order totals are the sums of its lines (checked at commit, so the
-- order and its lines are written in any order within one transaction).
-- TG_ARGV[0] names the column of the trigger row that holds the order id.
CREATE FUNCTION public.orders_check_lines_total() RETURNS trigger
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
GRANT SELECT, INSERT, UPDATE ON order_lines TO app_rw;
COMMIT;
