-- 000113_create_payouts.up.sql (02 §21.4; 03 §6.1). A payout (hakediş
-- ödemesi) to a payee: finance admin A creates it from payable lines
-- (payout_items, 000115) → 'pending_approval'; finance admin B ≠ A approves
-- → 'approved'; the EFT is made at the bank by hand; any finance admin marks
-- it 'paid' with the receipt and the bank reference (same transaction:
-- ledger payout_paid + order lines marked paid out + outbox). 'failed'
-- releases the lines. The IBAN is snapshotted at creation from the payee's
-- approved account. Never deleted (financial_records).
-- 02 fixes and additions (H-20):
--   * 06 H-37: `(status = 'paid') = (paid_at … AND paid_by … AND
--     bank_reference … AND receipt_file_id …)` let an unpaid payout carry
--     part of the payment; paid_at, paid_by and the receipt are each tied to
--     'paid', the bank reference to the EFT states (paid, failed);
--     approved_by/at are set together, never while pending and always in
--     approved / paid / failed; cancelled_at and cancel_reason (04
--     adminCancelPayout) belong to 'cancelled', failure_reason to 'failed'.
--   * 06 H-38: (payee_id, org_id) → payees and (bank_account_id, payee_id) →
--     payee_bank_accounts: the account and the seller org are the payee's.
--     payouts_id_payee_uq is the target of payout_items and order_lines.
--   * payouts_bank_account_guard (02 "status='approved' at creation"): a
--     payout is created only for an approved account, with its exact IBAN
--     and holder as snapshot, and approved only while that account is still
--     approved (a revoked or superseded account blocks the approval; 04
--     adminRevokeBankAccount). Raised as check_violation, CONSTRAINT
--     payouts_bank_account_chk.
--   * koro_initial_status(): a payout starts 'pending_approval';
--     payouts_frozen keeps the payee, the account, the snapshots, the amount
--     and the creator; payouts_approval_frozen the approver once set;
--     payouts_paid_frozen makes 'paid' final (status and receipt).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE payouts (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  payee_id          uuid NOT NULL,
  org_id            uuid NOT NULL,                                      -- seller org (= payees.org_id, composite FK)
  bank_account_id   uuid NOT NULL,                                      -- approved at creation (payouts_bank_account_guard)
  iban_snapshot     text NOT NULL CONSTRAINT payouts_iban_snapshot_chk CHECK (public.koro_iban_tr_valid(iban_snapshot)),
  holder_snapshot   text NOT NULL CONSTRAINT payouts_holder_snapshot_chk CHECK (char_length(holder_snapshot) BETWEEN 2 AND 300),
  amount            bigint NOT NULL CONSTRAINT payouts_amount_chk CHECK (amount > 0),
  currency          char(3) NOT NULL DEFAULT 'TRY' CONSTRAINT payouts_currency_chk CHECK (currency = 'TRY'),
  status            text NOT NULL DEFAULT 'pending_approval'
                      CONSTRAINT payouts_status_chk CHECK (status IN ('pending_approval','approved','paid','failed','cancelled')),
  created_by        uuid NOT NULL CONSTRAINT payouts_created_by_fkey REFERENCES users (id),
  approved_by       uuid CONSTRAINT payouts_approved_by_fkey REFERENCES users (id),
  approved_at       timestamptz,
  paid_by           uuid CONSTRAINT payouts_paid_by_fkey REFERENCES users (id),
  paid_at           timestamptz,
  bank_reference    text CONSTRAINT payouts_bank_reference_chk CHECK (char_length(bank_reference) <= 100),
  receipt_file_id   uuid CONSTRAINT payouts_receipt_file_id_fkey REFERENCES files (id),   -- the bank receipt (dekont)
  failure_reason    text CONSTRAINT payouts_failure_reason_chk CHECK (char_length(failure_reason) <= 500),
  cancelled_at      timestamptz,
  cancel_reason     text CONSTRAINT payouts_cancel_reason_chk CHECK (char_length(cancel_reason) <= 500),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payouts_payee_id_org_id_fkey FOREIGN KEY (payee_id, org_id) REFERENCES payees (id, org_id),
  CONSTRAINT payouts_bank_account_id_payee_id_fkey FOREIGN KEY (bank_account_id, payee_id) REFERENCES payee_bank_accounts (id, payee_id),
  CONSTRAINT payouts_id_payee_uq UNIQUE (id, payee_id),
  -- four eyes (03 §6.1)
  CONSTRAINT payouts_four_eyes_chk CHECK (approved_by IS NULL OR approved_by <> created_by),
  -- one CHECK per state (06 H-37)
  CONSTRAINT payouts_approved_chk CHECK ((approved_by IS NULL) = (approved_at IS NULL)
                                     AND (status <> 'pending_approval' OR approved_by IS NULL)
                                     AND (status NOT IN ('approved','paid','failed') OR approved_by IS NOT NULL)),
  CONSTRAINT payouts_paid_chk CHECK ((status = 'paid') = (paid_at IS NOT NULL)
                                 AND (status = 'paid') = (paid_by IS NOT NULL)
                                 AND (status = 'paid') = (receipt_file_id IS NOT NULL)
                                 AND (status <> 'paid' OR bank_reference IS NOT NULL)
                                 AND (bank_reference IS NULL OR status IN ('paid','failed'))),
  CONSTRAINT payouts_cancelled_chk CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL)
                                      AND (status = 'cancelled') = (cancel_reason IS NOT NULL)),
  CONSTRAINT payouts_failed_chk CHECK ((status = 'failed') = (failure_reason IS NOT NULL))
);
CREATE INDEX payouts_payee_idx ON payouts (payee_id, created_at DESC);
CREATE INDEX payouts_org_idx   ON payouts (org_id, created_at DESC);
CREATE INDEX payouts_open_idx  ON payouts (created_at) WHERE status IN ('pending_approval','approved');

-- The account of a payout is approved when the payout is created (with the
-- exact IBAN and holder) and at the moment of approval (approved_by is
-- written, whatever the new status).
CREATE FUNCTION public.payouts_check_bank_account() RETURNS trigger
  LANGUAGE plpgsql AS $$
DECLARE
  acc record;
BEGIN
  IF TG_OP = 'UPDATE' AND NOT (OLD.approved_by IS NULL AND NEW.approved_by IS NOT NULL) THEN
    RETURN NEW;
  END IF;
  SELECT a.status, a.iban, a.holder_name INTO acc
    FROM payee_bank_accounts a WHERE a.id = NEW.bank_account_id AND a.payee_id = NEW.payee_id;
  IF NOT FOUND OR acc.status <> 'approved' OR acc.iban <> NEW.iban_snapshot OR acc.holder_name <> NEW.holder_snapshot THEN
    RAISE EXCEPTION 'payout %: bank account % is not the approved account of the payee (or the snapshot differs)', NEW.id, NEW.bank_account_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'payouts_bank_account_chk';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payouts_initial_status BEFORE INSERT ON payouts
  FOR EACH ROW EXECUTE FUNCTION public.koro_initial_status('pending_approval');
CREATE TRIGGER payouts_bank_account_guard BEFORE INSERT OR UPDATE ON payouts
  FOR EACH ROW EXECUTE FUNCTION public.payouts_check_bank_account();
CREATE TRIGGER payouts_touch_updated_at BEFORE UPDATE ON payouts
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER payouts_frozen BEFORE UPDATE ON payouts
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'payee_id', 'org_id', 'bank_account_id', 'iban_snapshot',
    'holder_snapshot', 'amount', 'currency', 'created_by');
CREATE TRIGGER payouts_approval_frozen BEFORE UPDATE ON payouts
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('approved_by', 'approved_by', 'approved_at');
CREATE TRIGGER payouts_paid_frozen BEFORE UPDATE ON payouts
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('paid_at', 'status', 'paid_at', 'paid_by', 'bank_reference', 'receipt_file_id');
GRANT SELECT, INSERT, UPDATE ON payouts TO app_rw;
COMMIT;
