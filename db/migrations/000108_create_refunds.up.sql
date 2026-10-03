-- 000108_create_refunds.up.sql (02 §20.5; 03 §6.1). A refund to the buyer —
-- a money exit, so four eyes: finance admin A requests (amount, reason,
-- target IBAN) → 'requested'; finance admin B ≠ A approves → 'approved'; the
-- EFT is made at the bank ('processing') and recorded with its receipt and
-- reference → 'completed' (the ledger posts refund_completed in the same
-- transaction). An overpayment refund (reason 'overpayment') is a refund of
-- the payment, not of the order (orders.refunded_amount excludes it). Never
-- deleted (financial_records).
-- 02 fixes and additions (H-20):
--   * 06 H-37: approved_by/at are set exactly in the approved states
--     (approved, processing, completed, failed); new rejected_by /
--     rejected_at / rejection_reason and failure_reason columns record the
--     rejection and the failed EFT, each tied to its state; completed_at and
--     the receipt belong to 'completed', the bank reference to the EFT states.
--   * the target IBAN is part of the request for a bank EFT (04 createRefund)
--     and only for it, so the approver approves the destination as well;
--     refunds_frozen keeps the payment, the line, the amount, the method and
--     the target IBAN, refunds_approval_frozen the approver once set,
--     refunds_completed_frozen makes 'completed' final.
--   * 06 H-38: (payment_id, order_id) → payments and (order_line_id,
--     order_id) → order_lines.
--   * koro_initial_status(): a refund starts 'requested', never approved.
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE refunds (
  id                uuid PRIMARY KEY DEFAULT uuidv7(),
  payment_id        uuid NOT NULL,
  order_id          uuid NOT NULL,
  order_line_id     uuid,
  amount            bigint NOT NULL CONSTRAINT refunds_amount_chk CHECK (amount > 0),
  reason            text NOT NULL CONSTRAINT refunds_reason_chk CHECK (reason IN ('event_cancelled','offering_cancelled','not_fulfilled',
                      'duplicate_payment','overpayment','goodwill','other')),
  reason_note       text CONSTRAINT refunds_reason_note_chk CHECK (char_length(reason_note) <= 1000),
  status            text NOT NULL DEFAULT 'requested' CONSTRAINT refunds_status_chk CHECK (status IN ('requested','approved','processing','completed','failed','rejected')),
  requested_by      uuid NOT NULL CONSTRAINT refunds_requested_by_fkey REFERENCES users (id),
  approved_by       uuid CONSTRAINT refunds_approved_by_fkey REFERENCES users (id),
  approved_at       timestamptz,
  rejected_by       uuid CONSTRAINT refunds_rejected_by_fkey REFERENCES users (id),
  rejected_at       timestamptz,
  rejection_reason  text CONSTRAINT refunds_rejection_reason_chk CHECK (char_length(rejection_reason) <= 1000),
  method            text NOT NULL CONSTRAINT refunds_method_chk CHECK (method IN ('bank_eft','iyzico')),
  target_iban       text CONSTRAINT refunds_target_iban_chk CHECK (target_iban IS NULL OR public.koro_iban_tr_valid(target_iban)),
  bank_reference    text CONSTRAINT refunds_bank_reference_chk CHECK (char_length(bank_reference) <= 100),
  receipt_file_id   uuid CONSTRAINT refunds_receipt_file_id_fkey REFERENCES files (id),
  failure_reason    text CONSTRAINT refunds_failure_reason_chk CHECK (char_length(failure_reason) <= 500),
  completed_at      timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT refunds_payment_id_order_id_fkey FOREIGN KEY (payment_id, order_id) REFERENCES payments (id, order_id),
  CONSTRAINT refunds_order_line_id_order_id_fkey FOREIGN KEY (order_line_id, order_id) REFERENCES order_lines (id, order_id),
  -- four eyes (03 §6.1: money exit)
  CONSTRAINT refunds_four_eyes_chk CHECK (approved_by IS NULL OR approved_by <> requested_by),
  -- one CHECK per state (06 H-37)
  CONSTRAINT refunds_approved_chk CHECK ((status IN ('approved','processing','completed','failed')) = (approved_by IS NOT NULL)
                                     AND (status IN ('approved','processing','completed','failed')) = (approved_at IS NOT NULL)),
  CONSTRAINT refunds_rejected_chk CHECK ((status = 'rejected') = (rejected_by IS NOT NULL)
                                     AND (status = 'rejected') = (rejected_at IS NOT NULL)
                                     AND (rejection_reason IS NULL OR status = 'rejected')),
  CONSTRAINT refunds_completed_chk CHECK ((status = 'completed') = (completed_at IS NOT NULL)
                                      AND (status = 'completed') = (receipt_file_id IS NOT NULL)
                                      AND (status <> 'completed' OR bank_reference IS NOT NULL)
                                      AND (bank_reference IS NULL OR status IN ('processing','completed','failed'))),
  CONSTRAINT refunds_failed_chk CHECK ((status = 'failed') = (failure_reason IS NOT NULL)),
  CONSTRAINT refunds_target_chk CHECK ((method = 'bank_eft') = (target_iban IS NOT NULL))
);
CREATE INDEX refunds_open_idx  ON refunds (created_at) WHERE status IN ('requested','approved','processing');
CREATE INDEX refunds_order_idx ON refunds (order_id);
CREATE TRIGGER refunds_touch_updated_at BEFORE UPDATE ON refunds
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER refunds_initial_status BEFORE INSERT ON refunds
  FOR EACH ROW EXECUTE FUNCTION public.koro_initial_status('requested');
CREATE TRIGGER refunds_frozen BEFORE UPDATE ON refunds
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'payment_id', 'order_id', 'order_line_id', 'amount', 'reason',
    'method', 'target_iban', 'requested_by');
CREATE TRIGGER refunds_approval_frozen BEFORE UPDATE ON refunds
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('approved_by', 'approved_by', 'approved_at');
CREATE TRIGGER refunds_completed_frozen BEFORE UPDATE ON refunds
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('completed_at', 'status', 'completed_at', 'bank_reference', 'receipt_file_id');
GRANT SELECT, INSERT, UPDATE ON refunds TO app_rw;
COMMIT;
