-- 000107_create_payment_splits.up.sql (02 §20.4). How a payment divides per
-- order line between the seller (payee) and the platform (commission, its
-- VAT and the buyer fee). In the manual model A flow one row per line is
-- written when the order is fully paid (status 'released'); with iyzico
-- (phase 2) held → approved. Never deleted (financial_records).
-- 02 additions (H-20):
--   * 06 H-38: order_id (new column) with (payment_id, order_id) → payments
--     and (order_line_id, order_id) → order_lines: the split's line belongs
--     to the order of its payment.
--   * a split moves some money (seller + platform > 0); approved_at belongs
--     to the approved / released / refunded states and 'approved' has it.
--   * payment_splits_frozen: the payment, the line and the amounts never
--     change.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE payment_splits (
  id                     uuid PRIMARY KEY DEFAULT uuidv7(),
  payment_id             uuid NOT NULL,
  order_id               uuid NOT NULL,                   -- = payments.order_id = order_lines.order_id
  order_line_id          uuid NOT NULL,
  provider_item_id       text,                            -- iyzico paymentTransactionId (phase 2)
  seller_amount          bigint NOT NULL CONSTRAINT payment_splits_seller_amount_chk CHECK (seller_amount >= 0),
  platform_amount        bigint NOT NULL CONSTRAINT payment_splits_platform_amount_chk CHECK (platform_amount >= 0),   -- commission + its VAT + buyer fee
  psp_commission_amount  bigint NOT NULL DEFAULT 0 CONSTRAINT payment_splits_psp_commission_amount_chk CHECK (psp_commission_amount >= 0),
  status                 text NOT NULL CONSTRAINT payment_splits_status_chk CHECK (status IN ('held','approved','disapproved','released','refunded')),
  approved_at            timestamptz,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payment_splits_payment_id_order_id_fkey FOREIGN KEY (payment_id, order_id) REFERENCES payments (id, order_id),
  CONSTRAINT payment_splits_order_line_id_order_id_fkey FOREIGN KEY (order_line_id, order_id) REFERENCES order_lines (id, order_id),
  CONSTRAINT payment_splits_payment_line_uq UNIQUE (payment_id, order_line_id),
  CONSTRAINT payment_splits_amount_chk CHECK (seller_amount + platform_amount > 0),
  CONSTRAINT payment_splits_approved_chk CHECK ((status <> 'approved' OR approved_at IS NOT NULL)
                                             AND (approved_at IS NULL OR status IN ('approved','released','refunded')))
);
CREATE INDEX payment_splits_line_idx ON payment_splits (order_line_id);
CREATE TRIGGER payment_splits_touch_updated_at BEFORE UPDATE ON payment_splits
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER payment_splits_frozen BEFORE UPDATE ON payment_splits
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'payment_id', 'order_id', 'order_line_id', 'seller_amount', 'platform_amount');
GRANT SELECT, INSERT, UPDATE ON payment_splits TO app_rw;
COMMIT;
