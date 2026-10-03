-- 000106_create_payments.up.sql (02 §20.3). A payment captured for an order:
-- in the MVP a matched bank transfer (provider 'bank_transfer', confirmed by
-- a finance admin); 'direct_confirmed' is the admin's confirmation of a
-- model C payment made straight to the payee (it never reaches the Koro
-- account; the ledger records it for information only); iyzico is phase 2.
-- Never deleted (financial_records): app_rw has no DELETE.
-- 02 fixes and additions (H-20):
--   * 06 H-38: (order_id, org_id) → orders (the buyer is the order's) and
--     (incoming_transfer_id, order_id) → incoming_bank_transfers (id,
--     matched_order_id): a bank-transfer payment belongs to the order its
--     transfer is matched to, and the match cannot be undone while the
--     payment exists. payments_id_order_uq is the target of payment_splits
--     and refunds.
--   * 06 H-37 (full per state): a transfer id exactly for bank transfers;
--     captured_at exactly in the captured states (chargeback included);
--     captured_by only once captured and required for the manual providers;
--     'refunded' / 'partially_refunded' match refunded_amount; a failed
--     payment has a failure code.
--   * psp_fee_amount <= amount.
--   * payments_frozen: the order, the provider, the transfer and the amount
--     never change.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE payments (
  id                    uuid PRIMARY KEY DEFAULT uuidv7(),
  order_id              uuid NOT NULL,
  org_id                uuid NOT NULL,                                     -- buyer (= orders.org_id, composite FK)
  provider              text NOT NULL CONSTRAINT payments_provider_chk CHECK (provider IN ('bank_transfer','iyzico','direct_confirmed','manual')),
  incoming_transfer_id  uuid CONSTRAINT payments_incoming_transfer_id_uq UNIQUE,
  provider_payment_id   text,
  conversation_id       text CONSTRAINT payments_conversation_id_uq UNIQUE,   -- iyzico (phase 2) = payments.id
  amount                bigint NOT NULL CONSTRAINT payments_amount_chk CHECK (amount > 0),
  currency              char(3) NOT NULL DEFAULT 'TRY' CONSTRAINT payments_currency_chk CHECK (currency = 'TRY'),
  status                text NOT NULL CONSTRAINT payments_status_chk CHECK (status IN ('initiated','captured','failed','partially_refunded','refunded','chargeback')),
  method                text,
  installments          smallint CONSTRAINT payments_installments_chk CHECK (installments BETWEEN 1 AND 12),
  psp_fee_amount        bigint NOT NULL DEFAULT 0 CONSTRAINT payments_psp_fee_amount_chk CHECK (psp_fee_amount >= 0),
  refunded_amount       bigint NOT NULL DEFAULT 0 CONSTRAINT payments_refunded_amount_chk CHECK (refunded_amount >= 0),
  captured_at           timestamptz,
  captured_by           uuid CONSTRAINT payments_captured_by_fkey REFERENCES users (id),   -- the finance admin who confirmed it by hand
  failure_code          text,
  evidence_file_id      uuid CONSTRAINT payments_evidence_file_id_fkey REFERENCES files (id),
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payments_order_id_org_id_fkey FOREIGN KEY (order_id, org_id) REFERENCES orders (id, org_id),
  CONSTRAINT payments_incoming_transfer_id_order_id_fkey FOREIGN KEY (incoming_transfer_id, order_id)
    REFERENCES incoming_bank_transfers (id, matched_order_id),
  CONSTRAINT payments_id_order_uq UNIQUE (id, order_id),
  CONSTRAINT payments_transfer_chk CHECK ((provider = 'bank_transfer') = (incoming_transfer_id IS NOT NULL)),
  CONSTRAINT payments_captured_chk CHECK ((status IN ('captured','partially_refunded','refunded','chargeback')) = (captured_at IS NOT NULL)),
  CONSTRAINT payments_captured_by_chk CHECK (captured_by IS NULL OR captured_at IS NOT NULL),
  CONSTRAINT payments_manual_capture_chk CHECK (provider NOT IN ('bank_transfer','direct_confirmed','manual')
                                               OR captured_at IS NULL OR captured_by IS NOT NULL),
  CONSTRAINT payments_refunded_limit_chk CHECK (refunded_amount <= amount),
  CONSTRAINT payments_refunded_chk CHECK (status <> 'refunded' OR refunded_amount = amount),
  CONSTRAINT payments_partially_refunded_chk CHECK (status <> 'partially_refunded' OR (refunded_amount > 0 AND refunded_amount < amount)),
  CONSTRAINT payments_failed_chk CHECK (status <> 'failed' OR failure_code IS NOT NULL),
  CONSTRAINT payments_psp_fee_chk CHECK (psp_fee_amount <= amount)
);
CREATE UNIQUE INDEX payments_provider_uq ON payments (provider, provider_payment_id) WHERE provider_payment_id IS NOT NULL;
CREATE INDEX payments_order_idx ON payments (order_id);
CREATE TRIGGER payments_touch_updated_at BEFORE UPDATE ON payments
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER payments_frozen BEFORE UPDATE ON payments
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'order_id', 'org_id', 'provider', 'incoming_transfer_id', 'amount', 'currency');
GRANT SELECT, INSERT, UPDATE ON payments TO app_rw;
COMMIT;
