-- 000105_create_incoming_bank_transfers.up.sql (02 §20.2; 06 H-18). A
-- transfer that arrived at a Koro collection account (entered by hand, CSV
-- import or bank API). The system suggests an order from extracted_order_no
-- and the amount; a finance admin matches it (payments). An unmatched
-- transfer may be ignored or sent back to its sender. Never deleted
-- (financial_records): app_rw has no DELETE.
-- 06 H-18 (T-60): sending money back is a money exit, so it takes four eyes
-- like refunds: one finance admin requests the return (target IBAN and
-- reason) → 'return_requested'; another approves → 'return_approved'; after
-- the EFT the receipt and bank reference are recorded → 'returned'. A
-- rejected request clears the return columns and the transfer is
-- 'unmatched' again (the audit log keeps the request).
-- 02 fixes and additions (H-20):
--   * 06 H-37: `(status = 'matched') = (matched_order_id … AND matched_by …
--     AND matched_at …)` let an unmatched transfer carry part of a match;
--     every match column, the ignore reason and every return column is now
--     tied to its state on its own.
--   * incoming_bank_transfers_id_order_uq (06 H-38): the target of payments
--     (incoming_transfer_id, order_id), so a payment belongs to the order its
--     transfer was matched to.
--   * incoming_bank_transfers_frozen: the bank data (account, value date,
--     amount, reference) never changes; incoming_bank_transfers_return_frozen:
--     the requester and the target IBAN cannot change once the return is
--     approved; incoming_bank_transfers_returned_frozen makes 'returned'
--     final.
--   * koro_initial_status(): a transfer is recorded 'unmatched' (a match
--     or a return is always a later, separate step).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE TABLE incoming_bank_transfers (
  id                      uuid PRIMARY KEY DEFAULT uuidv7(),
  collection_account_id   uuid NOT NULL CONSTRAINT incoming_bank_transfers_collection_account_id_fkey REFERENCES collection_accounts (id),
  value_date              date NOT NULL,
  amount                  bigint NOT NULL CONSTRAINT incoming_bank_transfers_amount_chk CHECK (amount > 0),
  currency                char(3) NOT NULL DEFAULT 'TRY' CONSTRAINT incoming_bank_transfers_currency_chk CHECK (currency = 'TRY'),
  sender_name             text CONSTRAINT incoming_bank_transfers_sender_name_chk CHECK (char_length(sender_name) <= 300),
  sender_iban             text CONSTRAINT incoming_bank_transfers_sender_iban_chk CHECK (sender_iban IS NULL OR public.koro_iban_tr_valid(sender_iban)),
  description             text NOT NULL CONSTRAINT incoming_bank_transfers_description_chk CHECK (char_length(description) <= 500),
  bank_reference          text NOT NULL CONSTRAINT incoming_bank_transfers_bank_reference_chk CHECK (char_length(bank_reference) <= 100),   -- receipt / transaction no
  extracted_order_no      text CONSTRAINT incoming_bank_transfers_extracted_order_no_chk CHECK (extracted_order_no ~ '^KR[0-9A-HJKMNP-TV-Z]{8}$'),
  status                  text NOT NULL DEFAULT 'unmatched' CONSTRAINT incoming_bank_transfers_status_chk CHECK (status IN ('unmatched','matched','ignored',
                            'return_requested','return_approved','returned')),
  matched_order_id        uuid CONSTRAINT incoming_bank_transfers_matched_order_id_fkey REFERENCES orders (id),
  matched_by              uuid CONSTRAINT incoming_bank_transfers_matched_by_fkey REFERENCES users (id),
  matched_at              timestamptz,
  ignore_reason           text CONSTRAINT incoming_bank_transfers_ignore_reason_chk CHECK (char_length(ignore_reason) <= 500),
  receipt_file_id         uuid CONSTRAINT incoming_bank_transfers_receipt_file_id_fkey REFERENCES files (id),
  entry_method            text NOT NULL CONSTRAINT incoming_bank_transfers_entry_method_chk CHECK (entry_method IN ('manual','csv_import','bank_api')),
  entered_by              uuid NOT NULL CONSTRAINT incoming_bank_transfers_entered_by_fkey REFERENCES users (id),
  -- 06 H-18: return to the sender (four eyes, as refunds)
  return_requested_by     uuid CONSTRAINT incoming_bank_transfers_return_requested_by_fkey REFERENCES users (id),
  return_requested_at     timestamptz,
  return_reason           text CONSTRAINT incoming_bank_transfers_return_reason_chk CHECK (char_length(return_reason) <= 500),
  return_target_iban      text CONSTRAINT incoming_bank_transfers_return_target_iban_chk CHECK (return_target_iban IS NULL OR public.koro_iban_tr_valid(return_target_iban)),
  return_approved_by      uuid CONSTRAINT incoming_bank_transfers_return_approved_by_fkey REFERENCES users (id),
  return_approved_at      timestamptz,
  returned_at             timestamptz,
  return_bank_reference   text CONSTRAINT incoming_bank_transfers_return_bank_reference_chk CHECK (char_length(return_bank_reference) <= 100),
  return_receipt_file_id  uuid CONSTRAINT incoming_bank_transfers_return_receipt_file_id_fkey REFERENCES files (id),
  created_at              timestamptz NOT NULL DEFAULT now(),
  updated_at              timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT incoming_bank_transfers_account_reference_uq UNIQUE (collection_account_id, bank_reference),
  CONSTRAINT incoming_bank_transfers_id_order_uq UNIQUE (id, matched_order_id),
  -- one CHECK per state (06 H-37)
  CONSTRAINT incoming_bank_transfers_matched_chk CHECK ((status = 'matched') = (matched_order_id IS NOT NULL)
                                                    AND (status = 'matched') = (matched_by IS NOT NULL)
                                                    AND (status = 'matched') = (matched_at IS NOT NULL)),
  CONSTRAINT incoming_bank_transfers_ignored_chk CHECK ((status = 'ignored') = (ignore_reason IS NOT NULL)),
  CONSTRAINT incoming_bank_transfers_return_request_chk CHECK (
             (status IN ('return_requested','return_approved','returned')) = (return_requested_by IS NOT NULL)
         AND (status IN ('return_requested','return_approved','returned')) = (return_requested_at IS NOT NULL)
         AND (status IN ('return_requested','return_approved','returned')) = (return_reason IS NOT NULL)
         AND (status IN ('return_requested','return_approved','returned')) = (return_target_iban IS NOT NULL)),
  CONSTRAINT incoming_bank_transfers_return_approval_chk CHECK (
             (status IN ('return_approved','returned')) = (return_approved_by IS NOT NULL)
         AND (status IN ('return_approved','returned')) = (return_approved_at IS NOT NULL)),
  CONSTRAINT incoming_bank_transfers_returned_chk CHECK (
             (status = 'returned') = (returned_at IS NOT NULL)
         AND (status = 'returned') = (return_bank_reference IS NOT NULL)
         AND (status = 'returned') = (return_receipt_file_id IS NOT NULL)),
  -- four eyes (06 H-18, 03 §6.1)
  CONSTRAINT incoming_bank_transfers_return_four_eyes_chk CHECK (return_approved_by IS NULL OR return_approved_by <> return_requested_by)
);
CREATE INDEX incoming_bank_transfers_unmatched_idx ON incoming_bank_transfers (value_date) WHERE status = 'unmatched';
CREATE INDEX incoming_bank_transfers_ref_idx       ON incoming_bank_transfers (extracted_order_no) WHERE extracted_order_no IS NOT NULL;
CREATE INDEX incoming_bank_transfers_return_idx    ON incoming_bank_transfers (return_requested_at)
  WHERE status IN ('return_requested','return_approved');
CREATE INDEX incoming_bank_transfers_order_idx     ON incoming_bank_transfers (matched_order_id) WHERE matched_order_id IS NOT NULL;
CREATE TRIGGER incoming_bank_transfers_touch_updated_at BEFORE UPDATE ON incoming_bank_transfers
  FOR EACH ROW EXECUTE FUNCTION public.koro_touch_updated_at();
CREATE TRIGGER incoming_bank_transfers_initial_status BEFORE INSERT ON incoming_bank_transfers
  FOR EACH ROW EXECUTE FUNCTION public.koro_initial_status('unmatched');
CREATE TRIGGER incoming_bank_transfers_frozen BEFORE UPDATE ON incoming_bank_transfers
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('id', 'collection_account_id', 'value_date', 'amount', 'currency',
    'bank_reference', 'entry_method', 'entered_by');
CREATE TRIGGER incoming_bank_transfers_return_frozen BEFORE UPDATE ON incoming_bank_transfers
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('return_approved_by', 'return_requested_by', 'return_requested_at',
    'return_reason', 'return_target_iban', 'return_approved_by', 'return_approved_at');
CREATE TRIGGER incoming_bank_transfers_returned_frozen BEFORE UPDATE ON incoming_bank_transfers
  FOR EACH ROW EXECUTE FUNCTION public.koro_freeze_columns('returned_at', 'status', 'returned_at', 'return_bank_reference', 'return_receipt_file_id');
GRANT SELECT, INSERT, UPDATE ON incoming_bank_transfers TO app_rw;
COMMIT;
