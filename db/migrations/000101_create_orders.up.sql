-- 000101_create_orders.up.sql (02 §19.1; ADR-0014). An order of a buyer
-- company: one payment model per order (06 H-6: amounts in kuruş, computed
-- per line by platform/money and frozen at order time). order_no is also the
-- bank-transfer reference. MVP flow (model A): the company pays the Koro
-- account, finance matches the transfer (payments), Koro keeps its
-- commission and pays the rest to the payee (ledger payouts).
-- collection_account_id gets its FK in 000104. Never deleted
-- (financial_records): app_rw has no DELETE.
-- 02 fixes and additions (H-20):
--   * 06 H-38: (buyer_party_id, org_id) → legal_parties: the billed party
--     belongs to the buying company; orders_id_org_uq is the target of the
--     (order_id, org_id) FKs of order_lines and payments.
--   * the frozen values (H-6, ADR-0014) cannot be changed after placement:
--     orders_frozen (koro_freeze_columns) covers the parties, the model and
--     every amount except the running paid / refunded amounts.
--   * amount consistency: vat_withheld_amount <= vat_amount; a paid order has
--     paid exactly koro_collect_amount; 'refunded' / 'partially_refunded'
--     match refunded_amount; the model and the collection method agree
--     (model C ⇔ direct_to_payee, as in commerce_policies).
--   * per-state CHECKs (06 H-37): paid_at belongs to the paid states
--     (disputed included: only a paid order is disputed); completed has
--     completed_at; an order expires after it is placed.
--   * orders_buyer_snapshot_chk: the snapshot is a JSON object.
--   The totals equal the sums of the lines: orders_totals_match_lines
--   (deferred constraint trigger, 000102).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE orders (
  id                      uuid PRIMARY KEY DEFAULT uuidv7(),
  order_no                text NOT NULL CONSTRAINT orders_order_no_uq UNIQUE
                            CONSTRAINT orders_order_no_chk CHECK (order_no ~ '^KR[0-9A-HJKMNP-TV-Z]{8}$'),   -- = bank-transfer reference
  org_id                  uuid NOT NULL,                                -- buyer company
  org_kind                text NOT NULL DEFAULT 'company' CONSTRAINT orders_org_kind_chk CHECK (org_kind = 'company'),
  placed_by_user_id       uuid NOT NULL CONSTRAINT orders_placed_by_user_id_fkey REFERENCES users (id),
  buyer_party_id          uuid NOT NULL,
  buyer_snapshot          jsonb NOT NULL,          -- {title,tax_office,tax_no,address,email} at order time
  model_code              text NOT NULL CONSTRAINT orders_model_code_chk CHECK (model_code IN ('A','A2','B','C')),
  collection_method       text NOT NULL CONSTRAINT orders_collection_method_chk CHECK (collection_method IN ('bank_transfer','iyzico','direct_to_payee')),
  collection_account_id   uuid,                    -- FK collection_accounts (000104)
  status                  text NOT NULL DEFAULT 'awaiting_payment' CONSTRAINT orders_status_chk CHECK (status IN ('awaiting_payment','paid',
                            'in_fulfilment','completed','cancelled','expired','partially_refunded','refunded','disputed')),
  currency                char(3) NOT NULL DEFAULT 'TRY' CONSTRAINT orders_currency_chk CHECK (currency = 'TRY'),
  subtotal_amount         bigint NOT NULL CONSTRAINT orders_subtotal_amount_chk CHECK (subtotal_amount >= 0),         -- Σ net
  vat_amount              bigint NOT NULL CONSTRAINT orders_vat_amount_chk CHECK (vat_amount >= 0),                   -- Σ VAT
  vat_withheld_amount     bigint NOT NULL DEFAULT 0 CONSTRAINT orders_vat_withheld_amount_chk CHECK (vat_withheld_amount >= 0),
  buyer_fee_amount        bigint NOT NULL DEFAULT 0 CONSTRAINT orders_buyer_fee_amount_chk CHECK (buyer_fee_amount >= 0),   -- payer=buyer commission + its VAT
  total_amount            bigint NOT NULL CONSTRAINT orders_total_amount_chk CHECK (total_amount >= 0),
  payable_amount          bigint NOT NULL CONSTRAINT orders_payable_amount_chk CHECK (payable_amount >= 0),             -- what the buyer pays in total
  koro_collect_amount     bigint NOT NULL CONSTRAINT orders_koro_collect_amount_chk CHECK (koro_collect_amount >= 0),   -- arrives at the Koro account
  direct_to_payee_amount  bigint NOT NULL DEFAULT 0 CONSTRAINT orders_direct_to_payee_amount_chk CHECK (direct_to_payee_amount >= 0),   -- model C only
  paid_amount             bigint NOT NULL DEFAULT 0 CONSTRAINT orders_paid_amount_chk CHECK (paid_amount >= 0),         -- captured at Koro
  refunded_amount         bigint NOT NULL DEFAULT 0 CONSTRAINT orders_refunded_amount_chk CHECK (refunded_amount >= 0),
  expires_at              timestamptz NOT NULL,     -- placed_at + 7 days (configuration)
  placed_at               timestamptz NOT NULL DEFAULT now(),
  paid_at                 timestamptz,
  completed_at            timestamptz,
  cancelled_at            timestamptz,
  cancel_reason           text CONSTRAINT orders_cancel_reason_chk CHECK (char_length(cancel_reason) <= 500),
  created_at              timestamptz NOT NULL DEFAULT now(),
  updated_at              timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT orders_org_id_org_kind_fkey FOREIGN KEY (org_id, org_kind) REFERENCES organizations (id, kind),
  CONSTRAINT orders_buyer_party_id_org_id_fkey FOREIGN KEY (buyer_party_id, org_id) REFERENCES legal_parties (id, org_id),
  CONSTRAINT orders_id_org_uq UNIQUE (id, org_id),
  CONSTRAINT orders_buyer_snapshot_chk CHECK (jsonb_typeof(buyer_snapshot) = 'object'),
  CONSTRAINT orders_model_method_chk CHECK ((model_code = 'C') = (collection_method = 'direct_to_payee')),
  -- amounts (kuruş)
  CONSTRAINT orders_total_chk CHECK (total_amount = subtotal_amount + vat_amount + buyer_fee_amount),
  CONSTRAINT orders_payable_chk CHECK (payable_amount = total_amount - vat_withheld_amount),
  CONSTRAINT orders_withheld_chk CHECK (vat_withheld_amount <= vat_amount),
  CONSTRAINT orders_collect_split_chk CHECK (koro_collect_amount + direct_to_payee_amount = payable_amount),
  CONSTRAINT orders_direct_model_chk CHECK (model_code = 'C' OR direct_to_payee_amount = 0),
  CONSTRAINT orders_paid_limit_chk CHECK (paid_amount <= koro_collect_amount),
  CONSTRAINT orders_refunded_limit_chk CHECK (refunded_amount <= paid_amount),
  -- states (06 H-37)
  CONSTRAINT orders_paid_chk CHECK ((status IN ('paid','in_fulfilment','completed','partially_refunded','refunded','disputed'))
                                    = (paid_at IS NOT NULL)),
  CONSTRAINT orders_paid_full_chk CHECK (status NOT IN ('paid','in_fulfilment','completed','partially_refunded','refunded','disputed')
                                         OR paid_amount = koro_collect_amount),
  CONSTRAINT orders_refunded_chk CHECK (status <> 'refunded' OR (refunded_amount > 0 AND refunded_amount = paid_amount)),
  CONSTRAINT orders_partially_refunded_chk CHECK (status <> 'partially_refunded' OR (refunded_amount > 0 AND refunded_amount < paid_amount)),
  CONSTRAINT orders_completed_chk CHECK (status <> 'completed' OR completed_at IS NOT NULL),
  CONSTRAINT orders_cancelled_chk CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL)),
  CONSTRAINT orders_expires_chk CHECK (expires_at > placed_at)
);
CREATE INDEX orders_org_idx     ON orders (org_id, placed_at DESC);
CREATE INDEX orders_pending_idx ON orders (expires_at) WHERE status = 'awaiting_payment';
CREATE TRIGGER orders_touch_updated_at BEFORE UPDATE ON orders
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER orders_frozen BEFORE UPDATE ON orders
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'order_no', 'org_id', 'org_kind', 'placed_by_user_id',
    'buyer_party_id', 'buyer_snapshot', 'model_code', 'collection_method', 'currency', 'subtotal_amount', 'vat_amount',
    'vat_withheld_amount', 'buyer_fee_amount', 'total_amount', 'payable_amount', 'koro_collect_amount',
    'direct_to_payee_amount', 'placed_at');
GRANT SELECT, INSERT, UPDATE ON orders TO app_rw;
COMMIT;
